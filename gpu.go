package main

// GPU telemetry via nvidia-smi. Cross-platform (the tool ships with the
// driver on Windows and Linux alike) and dependency-free.
//
// The encoder figure is the interesting one here: NVENC, not the shader
// cores, is what limits how many streams this machine can push at once.

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GPUStats struct {
	Present     bool    `json:"present"`
	Name        string  `json:"name"`
	Util        float64 `json:"util_percent"`     // graphics/compute load
	EncoderUtil float64 `json:"encoder_percent"`  // NVENC load
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	TempC       float64 `json:"temp_c"`
}

type gpuMonitor struct {
	mu   sync.Mutex
	last GPUStats
	off  bool // nvidia-smi missing: stop trying, report Present=false
}

var gpumon = &gpuMonitor{}

const gpuQuery = "name,utilization.gpu,utilization.encoder,memory.used,memory.total,temperature.gpu"

// startGPUMonitor samples nvidia-smi in the background. Safe to call on a
// machine without an NVIDIA card: the first failure disables it quietly.
func startGPUMonitor() {
	go func() {
		gpumon.sample()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if gpumon.disabled() {
				return
			}
			gpumon.sample()
		}
	}()
}

func (g *gpuMonitor) disabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.off
}

func (g *gpuMonitor) sample() {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu="+gpuQuery,
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		g.mu.Lock()
		g.off = true
		g.last = GPUStats{}
		g.mu.Unlock()
		return
	}

	// First GPU only -- this box has one, and a second would need its own
	// reporting shape anyway.
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	f := strings.Split(line, ",")
	if len(f) < 6 {
		return
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}

	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0 // "[N/A]" on cards that don't report a field
		}
		return v
	}

	st := GPUStats{
		Present:     true,
		Name:        f[0],
		Util:        num(f[1]),
		EncoderUtil: num(f[2]),
		MemUsedMB:   uint64(num(f[3])),
		MemTotalMB:  uint64(num(f[4])),
		TempC:       num(f[5]),
	}

	g.mu.Lock()
	g.last = st
	g.mu.Unlock()
}

func (g *gpuMonitor) snapshot() GPUStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}
