package sitesettings

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 服务健康状态：统一通过 Redis 缓存（site:status:{service}）跨服务共享。
// OJ 状态同时落库 site_configs（供站点设置页展示）；agent/ai/smtp 以 Redis 为准。

const (
	ServiceLuoGu   = "oj_luogu"
	ServiceQOJ     = "oj_qoj"
	ServiceVJudge  = "oj_vjudge"
	ServiceAgent   = "agent"
	ServiceAiAnaly = "ai_analyze"
	ServiceSmtp    = "smtp"

	StatusOK        = "ok"
	StatusFail      = "fail"
	StatusUnchecked = "unchecked"
)

type ServiceStatus struct {
	Status string `json:"status"`
	At     int64  `json:"at"` // unix 秒
	ErrMsg string `json:"errMsg"`
}

func statusRedisKey(service string) string {
	return "site:status:" + strings.TrimSpace(service)
}

// SetServiceStatus 写入某服务最近一次调用结果（ok/fail）+ 时间戳。
func SetServiceStatus(ctx context.Context, rdb *redis.Client, service, status, errMsg string) {
	if rdb == nil || service == "" {
		return
	}
	now := time.Now().Unix()
	if status != StatusOK && status != StatusFail {
		status = StatusFail
	}
	if errMsg != "" && len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	b, _ := json.Marshal(ServiceStatus{Status: status, At: now, ErrMsg: errMsg})
	_ = rdb.Set(ctx, statusRedisKey(service), b, 72*time.Hour).Err()
}

// GetServiceStatus 读取某服务最近调用状态。
func GetServiceStatus(ctx context.Context, rdb *redis.Client, service string) ServiceStatus {
	if rdb == nil {
		return ServiceStatus{Status: StatusUnchecked}
	}
	b, err := rdb.Get(ctx, statusRedisKey(service)).Bytes()
	if err != nil {
		return ServiceStatus{Status: StatusUnchecked}
	}
	var st ServiceStatus
	if json.Unmarshal(b, &st) != nil {
		return ServiceStatus{Status: StatusUnchecked}
	}
	if st.Status == "" {
		st.Status = StatusUnchecked
	}
	return st
}

// GetAllServiceStatus 返回所有关注服务的最近状态。
func GetAllServiceStatus(ctx context.Context, rdb *redis.Client) map[string]ServiceStatus {
	out := map[string]ServiceStatus{}
	for _, svc := range []string{ServiceLuoGu, ServiceQOJ, ServiceVJudge, ServiceAgent, ServiceAiAnaly, ServiceSmtp} {
		out[svc] = GetServiceStatus(ctx, rdb, svc)
	}
	return out
}

// AllServiceNames 健康端点用到的服务清单（顺序稳定）。
func AllServiceNames() []string {
	return []string{ServiceLuoGu, ServiceQOJ, ServiceVJudge, ServiceAgent, ServiceAiAnaly, ServiceSmtp}
}
