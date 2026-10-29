package gui

import (
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// coreUsage splits one core's busy time since the last sample into percents,
// like htop's bars: user (with nice), system (with interrupts and steal) and
// waiting for I/O.
type coreUsage struct {
	User   float64 `json:"user"`
	System float64 `json:"system"`
	IOWait float64 `json:"iowait"`
}

type memoryUsage struct {
	Total uint64 `json:"total"`
	// Used is what programs hold: total minus what is available without swapping.
	Used      uint64 `json:"used"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swapTotal"`
	SwapUsed  uint64 `json:"swapUsed"`
}

type resources struct {
	Cores  []coreUsage `json:"cores"`
	Memory memoryUsage `json:"memory"`
	// Load is the 1, 5 and 15 minute load average; missing on Windows.
	Load []float64 `json:"load,omitempty"`
}

// cpuSampler remembers the last CPU times, so each request reports the usage
// since the one before, as htop does between refreshes.
type cpuSampler struct {
	mu   sync.Mutex
	last []cpu.TimesStat
	at   time.Time
}

func (sampler *cpuSampler) sample() []coreUsage {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	now, err := cpu.Times(true)
	if err != nil || len(now) == 0 {
		return make([]coreUsage, runtime.NumCPU())
	}
	previous := sampler.last
	// A first request, or one long after the last, measures a short interval of its own.
	if len(previous) != len(now) || time.Since(sampler.at) > 10*time.Second {
		previous = now
		time.Sleep(200 * time.Millisecond)
		if now, err = cpu.Times(true); err != nil || len(now) != len(previous) {
			return make([]coreUsage, len(previous))
		}
	}
	sampler.last, sampler.at = now, time.Now()
	cores := make([]coreUsage, len(now))
	for index, times := range now {
		before := previous[index]
		total := times.Total() - before.Total()
		if total <= 0 {
			continue
		}
		percent := func(value float64) float64 { return max(0, min(100, 100*value/total)) }
		cores[index] = coreUsage{
			User:   percent(times.User + times.Nice - before.User - before.Nice),
			System: percent(times.System + times.Irq + times.Softirq + times.Steal - before.System - before.Irq - before.Softirq - before.Steal),
			IOWait: percent(times.Iowait - before.Iowait),
		}
	}
	return cores
}

func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	result := resources{Cores: s.cpu.sample()}
	if memory, err := mem.VirtualMemoryWithContext(r.Context()); err == nil {
		result.Memory = memoryUsage{Total: memory.Total, Used: memory.Total - memory.Available, Cached: memory.Cached + memory.Buffers}
		if result.Memory.Used+result.Memory.Cached > memory.Total {
			result.Memory.Cached = memory.Total - result.Memory.Used
		}
	}
	if swap, err := mem.SwapMemoryWithContext(r.Context()); err == nil {
		result.Memory.SwapTotal, result.Memory.SwapUsed = swap.Total, swap.Used
	}
	if average, err := load.AvgWithContext(r.Context()); err == nil && runtime.GOOS != "windows" {
		result.Load = []float64{average.Load1, average.Load5, average.Load15}
	}
	writeJSON(w, http.StatusOK, result)
}
