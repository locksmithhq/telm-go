package telm

// runtime.go — automatic process and Go runtime metrics collection.
//
// Called internally by Init(). No implementation required by the caller —
// metrics are registered as OTel observable callbacks and collected on each
// reader cycle.
//
// Emitted metrics:
//
//	process.cpu.usage         gauge   %      CPU usage in the current interval (via /proc/self/stat)
//	process.memory.bytes      gauge   By     heap allocated (runtime.MemStats.HeapAlloc)
//	runtime.goroutines        gauge   -      active goroutines (runtime.NumGoroutine)
//	runtime.gc.pause_ms       gauge   ms     last GC pause duration (runtime.MemStats.PauseNs)
//	process.io.read_bytes     gauge   By     bytes read in the last interval (via /proc/self/io rchar)
//	process.io.write_bytes    gauge   By     bytes written in the last interval (via /proc/self/io wchar)

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// cpuTracker tracks previous CPU ticks to calculate usage percentage.
type cpuTracker struct {
	mu        sync.Mutex
	prevUtime uint64
	prevStime uint64
	prevWall  time.Time
}

func (t *cpuTracker) percent() float64 {
	utime, stime, err := procStat()
	if err != nil {
		return 0
	}
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.prevWall.IsZero() {
		t.prevUtime = utime
		t.prevStime = stime
		t.prevWall = now
		return 0
	}

	deltaWall := now.Sub(t.prevWall).Seconds()
	if deltaWall <= 0 {
		return 0
	}

	deltaTicks := float64((utime + stime) - (t.prevUtime + t.prevStime))
	t.prevUtime = utime
	t.prevStime = stime
	t.prevWall = now

	// clk_tck = 100 Hz on Linux; * 100 converts to percentage
	return math.Min(100.0, math.Round((deltaTicks/100.0/deltaWall*100.0)*10)/10)
}

// ioTracker tracks absolute I/O bytes to compute per-interval deltas.
type ioTracker struct {
	mu        sync.Mutex
	prevRchar uint64
	prevWchar uint64
	init      bool
}

func (t *ioTracker) delta() (readDelta, writeDelta float64) {
	rchar, wchar := procIO()

	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.init {
		t.prevRchar = rchar
		t.prevWchar = wchar
		t.init = true
		return 0, 0
	}

	r := float64(rchar) - float64(t.prevRchar)
	w := float64(wchar) - float64(t.prevWchar)
	if r < 0 {
		r = 0
	}
	if w < 0 {
		w = 0
	}
	t.prevRchar = rchar
	t.prevWchar = wchar
	return r, w
}

// registerRuntimeMetrics registers observable callbacks on the provided meter.
// Callbacks are invoked automatically on each SDK collection cycle.
func registerRuntimeMetrics(meter metric.Meter) {
	cpu := &cpuTracker{}
	io := &ioTracker{}

	cpuGauge, _ := meter.Float64ObservableGauge(
		"process.cpu.usage",
		metric.WithDescription("CPU usage percentage"),
		metric.WithUnit("%"),
	)
	memGauge, _ := meter.Int64ObservableGauge(
		"process.memory.bytes",
		metric.WithDescription("Heap memory allocated in bytes"),
		metric.WithUnit("By"),
	)
	goroutinesGauge, _ := meter.Int64ObservableGauge(
		"runtime.goroutines",
		metric.WithDescription("Number of active goroutines"),
	)
	gcPauseGauge, _ := meter.Float64ObservableGauge(
		"runtime.gc.pause_ms",
		metric.WithDescription("Last GC pause duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	ioReadGauge, _ := meter.Float64ObservableGauge(
		"process.io.read_bytes",
		metric.WithDescription("Bytes read in the last collection interval"),
		metric.WithUnit("By"),
	)
	ioWriteGauge, _ := meter.Float64ObservableGauge(
		"process.io.write_bytes",
		metric.WithDescription("Bytes written in the last collection interval"),
		metric.WithUnit("By"),
	)

	_, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveFloat64(cpuGauge, cpu.percent())

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		o.ObserveInt64(memGauge, int64(ms.HeapAlloc))
		o.ObserveInt64(goroutinesGauge, int64(runtime.NumGoroutine()))
		if ms.NumGC > 0 {
			pauseNs := ms.PauseNs[(ms.NumGC+255)%256]
			o.ObserveFloat64(gcPauseGauge, math.Round(float64(pauseNs)/1e6*100)/100)
		}

		r, w := io.delta()
		o.ObserveFloat64(ioReadGauge, r)
		o.ObserveFloat64(ioWriteGauge, w)

		return nil
	}, cpuGauge, memGauge, goroutinesGauge, gcPauseGauge, ioReadGauge, ioWriteGauge)
}

// procStat reads utime and stime from /proc/self/stat (in jiffies).
func procStat() (utime, stime uint64, err error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, 0, err
	}
	s := string(data)
	// The comm field may contain spaces; skip everything up to the last ')'.
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, 0, fmt.Errorf("telm: invalid /proc/self/stat")
	}
	fields := strings.Fields(s[idx+2:])
	// After state: ppid(1) pgrp(2) session(3) tty(4) tpgid(5) flags(6) minflt(7)
	// cminflt(8) majflt(9) cmajflt(10) utime(11) stime(12)
	if len(fields) < 13 {
		return 0, 0, fmt.Errorf("telm: insufficient fields in /proc/self/stat")
	}
	utime, _ = strconv.ParseUint(fields[11], 10, 64)
	stime, _ = strconv.ParseUint(fields[12], 10, 64)
	return utime, stime, nil
}

// procIO reads rchar and wchar from /proc/self/io (total I/O bytes for the process).
func procIO() (rchar, wchar uint64) {
	f, err := os.Open("/proc/self/io")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		val, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		switch parts[0] {
		case "rchar":
			rchar = val
		case "wchar":
			wchar = val
		}
	}
	return rchar, wchar
}
