package loadgate

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/load"
)

// 全局 CPU 负载门控：后台批任务（爬虫 / 画像重建 / 定时入队）在系统过载时自动放缓，
// 把 CPU 让给在线访问。以 load1/核数 是否超过阈值判定过载。
//
// 阈值可用环境变量 CWXU_CPU_GATE_THRESHOLD 覆盖（0 < t < 1，默认 0.7）。
// 例如 2 核机器：load1 超过 1.4 即视为过载，后台任务开始退避。
type Guard struct {
	ncpu      float64
	threshold float64
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

// New 按环境变量构建门控。
func New() *Guard {
	thr := 0.7
	if v := os.Getenv("CWXU_CPU_GATE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			thr = f
		}
	}
	return &Guard{ncpu: float64(runtime.NumCPU()), threshold: thr}
}

// Load 当前 1 分钟系统负载（loadavg）；读取失败返回 0（视为不 overloaded）。
func (g *Guard) Load() float64 {
	if g == nil {
		return 0
	}
	avg, err := load.Avg()
	if err != nil {
		return 0
	}
	return avg.Load1
}

// LoadRatio 负载 / 核数 比值；>1 表示核已打满。
func (g *Guard) LoadRatio() float64 {
	if g == nil || g.ncpu <= 0 {
		return 0
	}
	return g.Load() / g.ncpu
}

// Threshold 当前过载阈值。
func (g *Guard) Threshold() float64 {
	if g == nil {
		return 0.7
	}
	return g.threshold
}

// Overloaded 当前系统负载超过阈值（后台任务应退避）。
func (g *Guard) Overloaded() bool {
	if g == nil {
		return false
	}
	return g.LoadRatio() > g.threshold
}

// Wait 若过载则轮询等待负载回落到阈值下；最多等 max（<=0 时用默认 30s）。
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
