package main

// System monitor — cross-platform (Windows + Linux) via gopsutil.
// Exposes CPU, memory, disk and network throughput for the /system endpoint,
// which the panel dashboard polls to draw its meters.

import (
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

// SystemStats is the JSON shape returned by /system.
type SystemStats struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemPercent  float64 `json:"mem_percent"`
	MemUsedMB   uint64  `json:"mem_used_mb"`
	MemTotalMB  uint64  `json:"mem_total_mb"`
	DiskPercent float64 `json:"disk_percent"`
	DiskUsedGB  uint64  `json:"disk_used_gb"`
	DiskTotalGB uint64  `json:"disk_total_gb"`
	NetRxMbps   float64 `json:"net_rx_mbps"`
	NetTxMbps   float64 `json:"net_tx_mbps"`
	Uptime      int64   `json:"uptime_sec"`
	// GPU comes from nvidia-smi (gpu.go). Present=false on a machine without
	// an NVIDIA card, in which case the panel simply omits the tiles.
	GPU GPUStats `json:"gpu"`
}

type sysMonitor struct {
	mu        sync.Mutex
	last      SystemStats
	diskPath  string
	lastNet   net.IOCountersStat
	lastNetAt time.Time
	haveNet   bool
	started   time.Time
}

var sysmon *sysMonitor

// startSysMonitor kicks off a background sampler. diskPath is the volume to
// report (e.g. "C:\\" on Windows, "/" on Linux); empty picks a sane default.
func startSysMonitor(diskPath string) {
	if diskPath == "" {
		diskPath = defaultDiskPath()
	}
	sysmon = &sysMonitor{diskPath: diskPath, started: time.Now()}
	go sysmon.loop()
}

func (s *sysMonitor) loop() {
	// Prime the CPU counter (first call needs an interval baseline).
	_, _ = cpu.Percent(0, false)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.sample()
	}
}

func (s *sysMonitor) sample() {
	var st SystemStats
	st.Uptime = int64(time.Since(s.started).Seconds())

	// CPU (percentage over the last interval).
	if pcts, err := cpu.Percent(0, false); err == nil && len(pcts) > 0 {
		st.CPUPercent = round1(pcts[0])
	}

	// Memory.
	if vm, err := mem.VirtualMemory(); err == nil {
		st.MemPercent = round1(vm.UsedPercent)
		st.MemUsedMB = vm.Used / 1024 / 1024
		st.MemTotalMB = vm.Total / 1024 / 1024
	}

	// Disk. Read the path under the lock so /reload can change it live.
	s.mu.Lock()
	diskPath := s.diskPath
	s.mu.Unlock()
	if du, err := disk.Usage(diskPath); err == nil {
		st.DiskPercent = round1(du.UsedPercent)
		st.DiskUsedGB = du.Used / 1024 / 1024 / 1024
		st.DiskTotalGB = du.Total / 1024 / 1024 / 1024
	}

	// Network throughput (delta since last sample, all interfaces summed).
	if counters, err := net.IOCounters(false); err == nil && len(counters) > 0 {
		c := counters[0]
		now := time.Now()
		if s.haveNet {
			dt := now.Sub(s.lastNetAt).Seconds()
			if dt > 0 {
				rxBytes := float64(c.BytesRecv - s.lastNet.BytesRecv)
				txBytes := float64(c.BytesSent - s.lastNet.BytesSent)
				st.NetRxMbps = round1(rxBytes * 8 / 1e6 / dt)
				st.NetTxMbps = round1(txBytes * 8 / 1e6 / dt)
			}
		}
		s.lastNet = c
		s.lastNetAt = now
		s.haveNet = true
	}

	// GPU is sampled on its own schedule; just fold in the latest reading.
	st.GPU = gpumon.snapshot()

	s.mu.Lock()
	s.last = st
	s.mu.Unlock()
}

func (s *sysMonitor) snapshot() SystemStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// setDiskPath changes the reported volume live (used by /reload). Empty picks
// the platform default.
func (s *sysMonitor) setDiskPath(p string) {
	if p == "" {
		p = defaultDiskPath()
	}
	s.mu.Lock()
	s.diskPath = p
	s.mu.Unlock()
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
