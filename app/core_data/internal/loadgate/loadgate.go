package loadgate

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/load"
)

// 全局 CPU 负载门控：后台批任务（爬虫 / 画像重建 / 定时入队）在系统 CPU 过载时自动放缓，
// 把 CPU 让给在线访问。
//
// 判定信号：后台采样线程每 25s 用 gopsutil 测一次全机 CPU 占用百分比（跨进程，含 postgres
// 容器与 docker-proxy）。busy% 超过阈值（默认 70）即视为过载。
// 阈值可用环境变量 CWXU_CPU_GATE_THRESHOLD 覆盖（0 < t < 1，默认 0.7）。
//
// 说明：不用 load1 作主信号——该主机 loadavg 受僵尸进程/容器记账影响长期虚高
// （实测 CPU 96% 空闲时 load1 仍 >1.4），会误伤后台同步。
type Guard struct {
	ncpu      float64
	threshold float64
	mu        sync.RWMutex
	cpuBusy   float64 // 0-100，后台采样缓存
}

var (
	globalOnce sync.Once
	global     *Guard
)

// Global 返回进程级共享门控（懒初始化，避免改 wire 依赖注入）。
func Global() *Guard {
	globalOnce.Do(func() { global = New() })
	return global
}

// New 按环境变量构建门控，并启动后台 CPU 采样。
func New() *Guard {
	thr := 0.7
	if v := os.Getenv("CWXU_CPU_GATE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			thr = f
		}
	}
	g := &Guard{ncpu: float64(runtime.NumCPU()), threshold: thr}
	go g.sampleLoop()
	return g
}

func (g *Guard) sampleLoop() {
	// 首轮丢一次（gopsutil 首次 cpu.Percent(0) 无基线返回 0）
	_, _ = cpu.Percent(0, false)
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		// interval=0：返回自上次采样以来的总 CPU 占用（跨所有核）
		pct, err := cpu.Percent(0, false)
		if err != nil {
			continue
		}
		if len(pct) > 0 {
			g.mu.Lock()
			g.cpuBusy = pct[0]
			g.mu.Unlock()
		}
	}
}

// sampleInterval 采样周期：与资源监控一致，25s 一次，避免频繁读 /proc
const sampleInterval = 25 * time.Second

// CPUBusy 最近一次全机 CPU 占用百分比（0-100；采样未就绪时为 0）。
func (g *Guard) CPUBusy() float64 {
	if g == nil {
		return 0
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cpuBusy
}

// Load 当前 1 分钟系统负载（loadavg）；仅供日志展示。
func (g *Guard) Load() float64 {
	avg, err := load.Avg()
	if err != nil {
		return 0
	}
	return avg.Load1
}

// LoadRatio 负载 / 核数 比值；仅供日志展示。
func (g *Guard) LoadRatio() float64 {
	if g == nil || g.ncpu <= 0 {
		return 0
	}
	return g.Load() / g.ncpu
}

// Threshold 当前过载阈值（busy% 门限，0-1）。
func (g *Guard) Threshold() float64 {
	if g == nil {
		return 0.7
	}
	return g.threshold
}

// Overloaded 当前 CPU 占用超过阈值（后台任务应退避）。
func (g *Guard) Overloaded() bool {
	if g == nil {
		return false
	}
	return g.CPUBusy() >= g.threshold*100
}

// Wait 若过载则轮询等待 CPU 回落到阈值下；最多等 max（<=0 时用默认 30s）。
// 返回最终是否已有余量。ctx 取消则立即返回 false。
func (g *Guard) Wait(ctx context.Context, max time.Duration) bool {
	if g == nil {
		return true
	}
	if !g.Overloaded() {
		return true
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	deadline := time.Now().Add(max)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if !g.Overloaded() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if ctx != nil {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return false
			}
		} else {
			<-ticker.C
		}
	}
}
