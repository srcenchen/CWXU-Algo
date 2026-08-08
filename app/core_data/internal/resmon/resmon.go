package resmon

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/loadgate"

	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

// 资源监控：后台每 25s 采样全机 CPU/内存/磁盘/负载，
// 内存缓存当前快照（供健康接口读，替代每次访问实时 gopsutil），
// 并把采样追加到 Redis 24h 时序（供运维页图表）。
//
// 采样周期与 loadgate 一致（25s），避免频繁读 /proc。

const (
	SampleInterval = 25 * time.Second
	// seriesMaxLen 24h @25s ≈ 3456 点，留点余量
	seriesMaxLen = 4000
	seriesKey    = "ops:res:series"
	snapshotKey  = "ops:res:snapshot"
	// 默认返回给前端的点数（降采样后），约 5 分钟粒度覆盖 24h
	defaultPoints = 288
	maxPoints     = 1440
)

// Sample 单个时序采样点
type Sample struct {
	At  int64   `json:"at"`  // unix 秒
	CPU float64 `json:"cpu"` // 0-100
	Mem float64 `json:"mem"` // 0-100
}

// Snapshot 当前资源快照（缓存，供健康接口）
type Snapshot struct {
	CPUUsedPercent  float64
	MemUsedPercent  float64
	MemUsed         int64
	MemTotal        int64
	DiskUsedPercent float64
	DiskUsed        int64
	DiskTotal       int64
	Load1           float64
	Load5           float64
	Load15          float64
	LoadRelPercent  float64 // load1/核数*100
	CollectedAt     int64
}

var (
	startOnce sync.Once
	mu        sync.RWMutex
	snap      Snapshot
	rdb       *redis.Client
	ncpu      float64
	// dataDiskPath 运维磁盘统计目录（站点配置 data_disk_path；空=默认 /data）
	dataDiskPath atomic.Value
)

// Start 启动后台采样（幂等；在 core_data 启动健康服务时调用）。
func Start(client *redis.Client) {
	startOnce.Do(func() {
		rdb = client
		if n := runtime.NumCPU(); n > 0 {
			ncpu = float64(n)
		}
		if ncpu <= 0 {
			ncpu = 1
		}
		go loop()
	})
}

// refreshDataDiskPath 每轮采样前刷新数据盘路径（读站点共享配置，25s 一次成本可忽略）
func refreshDataDiskPath(client *redis.Client) {
	path := ""
	if client != nil {
		if rt, err := sitesettings.LoadFromRedis(context.Background(), client); err == nil && rt != nil {
			path = strings.TrimSpace(rt.DataDiskPath)
		}
	}
	dataDiskPath.Store(path)
}

func loop() {
	// 首轮丢基线
	_, _ = cpu.Percent(0, false)
	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		refreshDataDiskPath(rdb)
		s := collect()
		mu.Lock()
		snap = s
		mu.Unlock()
		if rdb != nil {
			pushSample(rdb, s)
		}
	}
}

func collect() Snapshot {
	now := time.Now().Unix()
	var s Snapshot
	s.CollectedAt = now
	// CPU 复用 loadgate 已采样的全机 busy%（避免双采样）
	s.CPUUsedPercent = loadgate.Global().CPUBusy()
	// 内存
	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemUsedPercent = vm.UsedPercent
		s.MemUsed, s.MemTotal = int64(vm.Used), int64(vm.Total)
	}
	// 磁盘（数据盘 /data 优先，未挂载回退根分区）
	if du, err := dataDiskUsage(); err == nil {
		s.DiskUsedPercent = du.UsedPercent
		s.DiskUsed, s.DiskTotal = int64(du.Used), int64(du.Total)
	}
	// 负载
	if la, err := load.Avg(); err == nil {
		s.Load1, s.Load5, s.Load15 = la.Load1, la.Load5, la.Load15
		if ncpu > 0 {
			s.LoadRelPercent = la.Load1 / ncpu * 100
		}
	}
	return s
}

func pushSample(r *redis.Client, s Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entry := fmt.Sprintf("%d,%.1f,%.1f", s.CollectedAt, s.CPUUsedPercent, s.MemUsedPercent)
	pipe := r.Pipeline()
	pipe.LPush(ctx, seriesKey, entry)
	pipe.LTrim(ctx, seriesKey, 0, seriesMaxLen-1)
	if b, e := json.Marshal(s); e == nil {
		pipe.Set(ctx, snapshotKey, b, 0)
	}
	_, _ = pipe.Exec(ctx)
}

// SnapshotNow 最近一次采样快照（采样未就绪时为空值）。
func SnapshotNow() Snapshot {
	mu.RLock()
	defer mu.RUnlock()
	return snap
}

// ParseEntry 解析 "unix,cpu,mem" 条目
func ParseEntry(raw string) (Sample, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) < 3 {
		return Sample{}, false
	}
	at, err1 := strconv.ParseInt(parts[0], 10, 64)
	c, err2 := strconv.ParseFloat(parts[1], 64)
	m, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return Sample{}, false
	}
	return Sample{At: at, CPU: c, Mem: m}, true
}

// Series 从 Redis 读最近采样并降采样到 <=points 个点（组内均值，时间取组中点）。
// 无 Redis 或读取失败返回空。
func Series(ctx context.Context, r *redis.Client, points int) []Sample {
	if r == nil {
		return nil
	}
	if points <= 0 {
		points = defaultPoints
	}
	if points > maxPoints {
		points = maxPoints
	}
	raw, err := r.LRange(ctx, seriesKey, 0, -1).Result()
	if err != nil || len(raw) == 0 {
		return nil
	}
	raw = reverse(raw) // LPUSH 后为倒序，反转为时间升序
	samples := make([]Sample, 0, len(raw))
	for _, e := range raw {
		if s, ok := ParseEntry(e); ok {
			samples = append(samples, s)
		}
	}
	if len(samples) <= points {
		return samples
	}
	return downsample(samples, points)
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// downsample 把 samples 均匀缩到 <=points 个点（组内均值，时间取组中点）
func downsample(samples []Sample, points int) []Sample {
	if len(samples) <= points {
		return samples
	}
	bucket := len(samples) / points
	if bucket < 1 {
		bucket = 1
	}
	out := make([]Sample, 0, points)
	for i := 0; i < len(samples); i += bucket {
		j := i + bucket
		if j > len(samples) {
			j = len(samples)
		}
		chunk := samples[i:j]
		var c, m, atSum float64
		for _, s := range chunk {
			c += s.CPU
			m += s.Mem
			atSum += float64(s.At)
		}
		n := float64(len(chunk))
		out = append(out, Sample{At: int64(atSum / n), CPU: c / n, Mem: m / n})
	}
	if len(out) > points {
		out = out[:points]
	}
	return out
}

// dataDiskUsage 站点配置的数据盘路径优先（空=默认 /data），未挂载回退根分区（运维监控磁盘口径）
func dataDiskUsage() (*disk.UsageStat, error) {
	path, _ := dataDiskPath.Load().(string)
	if strings.TrimSpace(path) == "" {
		path = "/data"
	}
	if du, err := disk.Usage(path); err == nil && du.Total > 0 {
		return du, nil
	}
	return disk.Usage("/")
}

// DiskRootUsage 供健康接口兜底/校验用（读取失败返回 0）
func DiskRootUsage() (used, total int64, pct float64) {
	if du, err := dataDiskUsage(); err == nil {
		return int64(du.Used), int64(du.Total), du.UsedPercent
	}
	return 0, 0, 0
}

// LoadAvg 兜底读当前负载
func LoadAvg() (l1, l5, l15 float64) {
	if la, err := load.Avg(); err == nil {
		return la.Load1, la.Load5, la.Load15
	}
	return 0, 0, 0
}
