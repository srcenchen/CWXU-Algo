package service

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/health"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/opsmetrics"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data"

	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"gorm.io/gorm"
)

// HealthService 运维监控：服务状态 + 中间件 + 服务器资源 + API 延迟 + 容量估算
type HealthService struct {
	health.UnimplementedHealthServer
	db    *gorm.DB
	udb   *gorm.DB // optional: algo_user
	rdb   *redis.Client
	server *conf.Server
}

func NewHealthService(d *data.Data, c *conf.Server) *HealthService {
	return &HealthService{db: d.DB, udb: d.UserDB, rdb: d.RDB, server: c}
}

func (s *HealthService) GetHealth(ctx context.Context, _ *health.GetHealthReq) (*health.GetHealthRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteConfigRead) {
		return &health.GetHealthRes{Code: 1, Message: "需要查看站点配置权限"}, nil
	}
	services := s.collectServices(ctx)
	middleware := s.collectMiddleware(ctx)
	resources := s.collectResources(ctx)
	api := s.collectAPI(ctx)
	capacity := s.collectCapacity(ctx, resources, api)

	return &health.GetHealthRes{
		Code:        0,
		Message:     "success",
		Services:    services,
		Middleware:  middleware,
		Resources:   resources,
		Api:         api,
		Capacity:    capacity,
		CollectedAt: time.Now().Unix(),
	}, nil
}

func (s *HealthService) collectServices(ctx context.Context) []*health.HealthServiceItem {
	names := []string{
		sitesettings.ServiceAgent, sitesettings.ServiceAiAnaly,
		sitesettings.ServiceSmtp, sitesettings.ServiceLuoGu, sitesettings.ServiceQOJ,
	}
	out := make([]*health.HealthServiceItem, 0, len(names))
	for _, n := range names {
		st := sitesettings.GetServiceStatus(ctx, s.rdb, n)
		out = append(out, &health.HealthServiceItem{
			Name: n, Status: st.Status, At: st.At, ErrMsg: st.ErrMsg,
		})
	}
	return out
}

func probePing(ctx context.Context, fn func(context.Context) error) (int64, string) {
	start := time.Now()
	if err := fn(ctx); err != nil {
		return time.Since(start).Milliseconds(), err.Error()
	}
	return time.Since(start).Milliseconds(), ""
}

func (s *HealthService) collectMiddleware(ctx context.Context) []*health.HealthMiddlewareItem {
	out := make([]*health.HealthMiddlewareItem, 0, 4)

	// 数据库（core）
	if s.db != nil {
		sqlDB, err := s.db.DB()
		status, lat, msg := "fail", int64(0), ""
		if err == nil {
			lat, msg = probePing(ctx, func(c context.Context) error {
				dctx, cancel := context.WithTimeout(c, 3*time.Second)
				defer cancel()
				return sqlDB.PingContext(dctx)
			})
			if msg == "" {
				status = "ok"
			}
		} else {
			msg = err.Error()
		}
		out = append(out, &health.HealthMiddlewareItem{Name: "database", Status: status, LatencyMs: lat, ErrMsg: msg})
	} else {
		out = append(out, &health.HealthMiddlewareItem{Name: "database", Status: "fail", ErrMsg: "未配置数据库"})
	}

	// Redis
	if s.rdb != nil {
		lat, msg := probePing(ctx, func(c context.Context) error {
			dctx, cancel := context.WithTimeout(c, 3*time.Second)
			defer cancel()
			return s.rdb.Ping(dctx).Err()
		})
		status := "fail"
		if msg == "" {
			status = "ok"
		}
		out = append(out, &health.HealthMiddlewareItem{Name: "redis", Status: status, LatencyMs: lat, ErrMsg: msg})
	} else {
		out = append(out, &health.HealthMiddlewareItem{Name: "redis", Status: "fail", ErrMsg: "未配置 Redis"})
	}

	// 注册中心 Consul
	if s.server != nil && s.server.RegDsn != "" {
		addr := strings.TrimSpace(s.server.RegDsn)
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		lat := time.Since(start).Milliseconds()
		if err != nil {
			out = append(out, &health.HealthMiddlewareItem{Name: "registry", Status: "fail", LatencyMs: lat, ErrMsg: err.Error()})
		} else {
			_ = conn.Close()
			out = append(out, &health.HealthMiddlewareItem{Name: "registry", Status: "ok", LatencyMs: lat})
		}
	} else {
		out = append(out, &health.HealthMiddlewareItem{Name: "registry", Status: "fail", ErrMsg: "未配置注册中心"})
	}

	// 消息队列 RabbitMQ
	if s.server != nil && s.server.AmqpDsn != "" {
		dsn := s.server.AmqpDsn
		start := time.Now()
		conn, err := net.DialTimeout("tcp", amqpHost(dsn), 3*time.Second)
		lat := time.Since(start).Milliseconds()
		if err != nil {
			out = append(out, &health.HealthMiddlewareItem{Name: "mq", Status: "fail", LatencyMs: lat, ErrMsg: err.Error()})
		} else {
			_ = conn.Close()
			out = append(out, &health.HealthMiddlewareItem{Name: "mq", Status: "ok", LatencyMs: lat})
		}
	} else {
		out = append(out, &health.HealthMiddlewareItem{Name: "mq", Status: "fail", ErrMsg: "未配置消息队列"})
	}

	return out
}

func (s *HealthService) collectResources(ctx context.Context) []*health.HealthResourceItem {
	out := make([]*health.HealthResourceItem, 0, 4)

	// CPU
	cpuPct := 0.0
	if per, err := cpu.PercentWithContext(ctx, 500*time.Millisecond, false); err == nil && len(per) > 0 {
		cpuPct = per[0]
	}
	out = append(out, &health.HealthResourceItem{
		Name: "cpu", Status: levelStatus(cpuPct, 70, 90),
		UsedPercent: cpuPct,
	})

	// 内存
	memTotal, memUsed := int64(0), int64(0)
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		memTotal, memUsed = int64(vm.Total), int64(vm.Used)
		memPct := vm.UsedPercent
		out = append(out, &health.HealthResourceItem{
			Name: "memory", Status: levelStatus(memPct, 75, 92),
			UsedPercent: memPct, Used: memUsed, Total: memTotal,
		})
	}

	// 磁盘（根分区）
	if du, err := disk.UsageWithContext(ctx, "/"); err == nil {
		out = append(out, &health.HealthResourceItem{
			Name: "disk", Status: levelStatus(du.UsedPercent, 75, 92),
			UsedPercent: du.UsedPercent, Used: int64(du.Used), Total: int64(du.Total),
		})
	}

	// 负载
	if la, err := load.AvgWithContext(ctx); err == nil {
		cores, _ := cpu.Counts(true)
		if cores < 1 {
			cores = 1
		}
		// 相对负载 = load1 / 核数；>1 说明排队
		rel := la.Load1 / float64(cores) * 100
		out = append(out, &health.HealthResourceItem{
			Name: "load", Status: levelStatus(rel, 70, 95),
			UsedPercent: rel,
			Detail:      fmt.Sprintf("load1=%.2f load5=%.2f load15=%.2f (核数 %d)", la.Load1, la.Load5, la.Load15, cores),
		})
	}

	return out
}

func (s *HealthService) collectAPI(ctx context.Context) *health.HealthApiItem {
	snap := opsmetrics.ReadSnapshot(ctx, s.rdb)
	return &health.HealthApiItem{
		RequestsToday:       snap.APIRequestsToday,
		ConcurrentNow:       snap.APIInflight,
		PeakConcurrentToday: snap.APIPeakToday,
		LatencyAvgMs:        snap.APILatencyAvg,
		LatencyP50Ms:        snap.APILatencyP50,
		LatencyP95Ms:        snap.APILatencyP95,
		LatencyP99Ms:        snap.APILatencyP99,
		SpiderEnqueuedToday: snap.SpiderEnqueued,
		SpiderOkToday:       snap.SpiderOK,
		SpiderFailToday:     snap.SpiderFail,
	}
}

// collectCapacity 估算峰值/健康用户数 + 当前负载等级。
func (s *HealthService) collectCapacity(ctx context.Context, resources []*health.HealthResourceItem, api *health.HealthApiItem) *health.HealthCapacityItem {
	// 用户数
	var registered, mau, todayUV, todayPV int64
	if s.udb != nil {
		_ = s.udb.WithContext(ctx).Table("users").Count(&registered).Error
	}
	snap := opsmetrics.ReadSnapshot(ctx, s.rdb)
	mau = snap.MAU
	if day := currentDayVisit(ctx, s.udb); day != nil {
		todayPV = day.PV
		todayUV = day.UV
	}

	// 存储：磁盘总量 + 已用；再做「用户均摊」估算
	var storageUsed, storageTotal int64
	for _, r := range resources {
		if r.Name == "disk" {
			storageUsed, storageTotal = r.Used, r.Total
		}
	}

	// 资源负载度：取 CPU / 内存 / 磁盘 中最高的使用率（load 是相对值，不计入）
	peakLoad := 0.0
	for _, r := range resources {
		if r.Name == "load" {
			continue
		}
		if r.UsedPercent > peakLoad {
			peakLoad = r.UsedPercent
		}
	}

	// 峰值用户估算模型：
	// - 并发基准：假设每并发用户约占 4MB 内存（HTTP 会话 + 查询）
	// - 内存预算：总内存的 70% 可用于业务
	// - 磁盘预算：总量 80% 可用
	// 综合资源约束，给出 peak（安全上限）与 healthy（推荐长期负载 60%）
	var memTotal int64
	for _, r := range resources {
		if r.Name == "memory" {
			memTotal = r.Total
		}
	}
	concurrentByMem := int64(0)
	if memTotal > 0 {
		budget := float64(memTotal) * 0.7 / (4 * 1024 * 1024)
		concurrentByMem = int64(budget)
	}
	// 磁盘：按「每活跃用户约 10MB 训练数据」估算（提交/统计/热力）
	usersByDisk := int64(0)
	if storageTotal > 0 {
		budget := float64(storageTotal) * 0.8 / (10 * 1024 * 1024)
		usersByDisk = int64(budget)
	}
	// 用户并发率：MAU 中同一时刻约 5% 在线（可配置的乐观假设）
	if mau > 0 && snap.APIInflight > 0 {
		ratio := float64(snap.APIInflight) / float64(mau)
		if ratio > 0 && ratio < 0.5 {
			onlineRatio := ratio
			_ = onlineRatio
		}
	}
	peakUsers := concurrentByMem
	if usersByDisk > 0 && usersByDisk < peakUsers {
		peakUsers = usersByDisk
	}
	if peakUsers < 0 {
		peakUsers = 0
	}
	healthyUsers := int64(float64(peakUsers) * 0.6)

	// 负载等级
	loadLevel := "low"
	note := ""
	switch {
	case peakLoad >= 92:
		loadLevel = "critical"
		note = "资源占用超过 92%，建议扩容或限流"
	case peakLoad >= 80:
		loadLevel = "high"
		note = "资源占用较高，关注峰值时段"
	case peakLoad >= 60:
		loadLevel = "normal"
		note = "资源使用正常"
	default:
		loadLevel = "low"
		note = "资源充裕"
	}
	// 并发已到峰值附近
	if peakUsers > 0 && snap.APIInflight >= peakUsers*8/10 {
		loadLevel = "high"
		if peakLoad >= 80 {
			loadLevel = "critical"
		}
		note = "当前并发接近容量上限"
	}

	return &health.HealthCapacityItem{
		RegisteredUsers: registered,
		Mau:             mau,
		TodayUv:         todayUV,
		TodayPv:         todayPV,
		StorageUsed:     storageUsed,
		StorageTotal:    storageTotal,
		PeakUsers:       peakUsers,
		HealthyUsers:    healthyUsers,
		LoadLevel:       loadLevel,
		LoadNote:        note,
	}
}

// levelStatus 按使用率给状态
func levelStatus(pct, warn, crit float64) string {
	if pct >= crit {
		return "critical"
	}
	if pct >= warn {
		return "warn"
	}
	return "ok"
}

// amqpHost 从 amqp://user:pass@host:port/vhost 提取 host:port
func amqpHost(dsn string) string {
	s := dsn
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	if slash := strings.Index(s, "/"); slash >= 0 {
		s = s[:slash]
	}
	if s == "" {
		return ""
	}
	if !strings.Contains(s, ":") {
		return s + ":5672"
	}
	return s
}

// DayVisitStat 轻量读取今日 UV/PV（algo_user.site_visit_dailys）
type dayVisitStat struct {
	PV int64
	UV int64
}

func currentDayVisit(ctx context.Context, db *gorm.DB) *dayVisitStat {
	if db == nil {
		return nil
	}
	day := time.Now().Format("2006-01-02")
	var out dayVisitStat
	if err := db.WithContext(ctx).Table("site_visit_dailys").
		Select("pv, uv").Where("date = ?", day).Scan(&out).Error; err != nil {
		return nil
	}
	return &out
}
