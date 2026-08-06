package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/resmon"

	"github.com/go-kratos/kratos/v2/log"
)

// 运维告警：系统长时间处于异常状态时给「运维告警邮件接收人」发邮件。
//
// 覆盖两类异常（站点设置里配置收件人；未配置则不发送）：
//  1. OJ 同步大面积出错：多个 OJ 同时在最近 15 分钟内失败且尚未恢复，持续 ≥30 分钟；
//  2. 系统资源长期占用过高：最近 30 分钟平均 CPU ≥90% 或内存 ≥92%，持续 ≥30 分钟
//     （取长窗均值，正常同步的短时尖峰不计）。
//
// 去重：条件首次满足记录起点，超过阈值时长才告警；发信后置 sent（2 小时窗口），
// 条件恢复即清空起点，下一次新异常会重新告警。Redis 锁保证多实例只跑一次。

const (
	alertOjSinceKey  = "ops:alert:oj:since"
	alertOjSentKey   = "ops:alert:oj:sent"
	alertResSinceKey = "ops:alert:res:since"
	alertResSentKey  = "ops:alert:res:sent"
	alertSentTTL     = 2 * time.Hour
	alertSustainMin  = 30 * time.Minute
	// alertOJAbnormalWindow 判定 OJ 处于「同步异常」的最近失败时间窗口
	alertOJAbnormalWindow = 15 * time.Minute
	// alertOJMinAbnormal 至少多少个 OJ 同时异常才算「大面积」
	alertOJMinAbnormal = 2
	// alertCPUThreshold / alertMemThreshold 资源长期占用阈值（百分比）
	alertCPUThreshold = 90.0
	alertMemThreshold = 92.0
	// alertResourceWindow 资源均值窗口（排除短时同步高峰）
	alertResourceWindow = 30 * time.Minute
)

// opsAlertOJPlatforms 参与 OJ 同步异常统计的平台（与站管监控 ojCaps 对齐）
var opsAlertOJPlatforms = []string{
	"NowCoder", "AtCoder", "CodeForces", "LuoGu", "QOJ", "LeetCode", "LOJ", "UOJ", "POJ",
}

// runOpsAlertTick 每 5 分钟执行一次异常检测与告警
func (t *CronTask) runOpsAlertTick() {
	if t.rdb == nil {
		return
	}
	if !t.tryCronLock("ops_alert", 4*time.Minute) {
		return
	}
	ctx := context.Background()

	// —— 1. OJ 同步大面积出错 ——
	abnormal := 0
	var names []string
	for _, p := range opsAlertOJPlatforms {
		lastFail, _ := t.rdb.Get(ctx, OjLastFailKey(p)).Int64()
		lastOK, _ := t.rdb.Get(ctx, OjLastOKKey(p)).Int64()
		if lastFail > 0 && time.Since(time.Unix(lastFail, 0)) <= alertOJAbnormalWindow && lastFail >= lastOK {
			abnormal++
			names = append(names, p)
		}
	}
	if abnormal >= alertOJMinAbnormal {
		_ = t.rdb.SetNX(ctx, alertOjSinceKey, time.Now().Unix(), 0)
		if t.alertReady(ctx, alertOjSinceKey, alertOjSentKey) {
			t.sendOpsAlertMail(ctx, "OJ 同步大面积出错", buildOJAlertHTML(names))
		}
	} else {
		_ = t.rdb.Del(ctx, alertOjSinceKey, alertOjSentKey).Err()
	}

	// —— 2. 系统资源长期占用过高 ——
	if t.checkResourceAlert(ctx) {
		_ = t.rdb.SetNX(ctx, alertResSinceKey, time.Now().Unix(), 0)
		if t.alertReady(ctx, alertResSinceKey, alertResSentKey) {
			snap := resmon.SnapshotNow()
			t.sendOpsAlertMail(ctx, "系统资源长期占用过高",
				buildResourceAlertHTML(snap.CPUUsedPercent, snap.MemUsedPercent))
		}
	} else {
		_ = t.rdb.Del(ctx, alertResSinceKey, alertResSentKey).Err()
	}
}

// checkResourceAlert 最近 alertResourceWindow 的平均 CPU/内存是否长期超标
func (t *CronTask) checkResourceAlert(ctx context.Context) bool {
	samples := resmon.Series(ctx, t.rdb, 0)
	if len(samples) == 0 {
		return false
	}
	cutoff := time.Now().Add(-alertResourceWindow).Unix()
	var cpuSum, memSum float64
	var cnt int
	for _, s := range samples {
		if s.At >= cutoff {
			cpuSum += s.CPU
			memSum += s.Mem
			cnt++
		}
	}
	if cnt == 0 {
		return false
	}
	cpuAvg := cpuSum / float64(cnt)
	memAvg := memSum / float64(cnt)
	if cpuAvg >= alertCPUThreshold || memAvg >= alertMemThreshold {
		log.Warnf("OpsAlert: resource high cpu=%.1f%% mem=%.1f%% avg(%d samples in %v)", cpuAvg, memAvg, cnt, alertResourceWindow)
		return true
	}
	return false
}

// alertReady 条件已持续超过 alertSustainMin 且本窗口未发过信
func (t *CronTask) alertReady(ctx context.Context, sinceKey, sentKey string) bool {
	// 已发过且窗口未过：不再重复轰炸
	sent, err := t.rdb.Exists(ctx, sentKey).Result()
	if err == nil && sent > 0 {
		return false
	}
	since, err := t.rdb.Get(ctx, sinceKey).Int64()
	if err != nil {
		return false
	}
	if time.Since(time.Unix(since, 0)) < alertSustainMin {
		return false
	}
	_ = t.rdb.Set(ctx, sentKey, time.Now().Unix(), alertSentTTL).Err()
	return true
}

// sendOpsAlertMail 给配置的运维告警收件人发邮件（从 Redis 共享配置读取 SMTP + 收件人）
func (t *CronTask) sendOpsAlertMail(ctx context.Context, title, inner string) {
	rt, err := sitesettings.LoadFromRedis(ctx, t.rdb)
	if err != nil || rt == nil {
		log.Warnf("OpsAlert: 读取站点共享配置失败，跳过告警邮件: %v", err)
		return
	}
	brand := rt.SiteTitle
	if strings.TrimSpace(brand) == "" {
		brand = "GoAlgo"
	}
	subject := fmt.Sprintf("【%s】%s", brand, title)
	body := mail.Wrap(mail.LayoutOpts{
		Brand:     brand,
		Title:     title,
		Preheader: "系统长时间异常，请及时处理",
	}, inner)
	notify.EmailOpsRecipientsRuntime(rt, subject, body)
}

func buildOJAlertHTML(platforms []string) string {
	items := make([]string, 0, len(platforms))
	for _, p := range platforms {
		items = append(items, "<li>"+mail.Escape(p)+"</li>")
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	return "<p style=\"margin:0 0 12px;\">多个 OJ 的提交同步持续异常（15 分钟内失败且未恢复）。</p>" +
		"<p style=\"margin:0 0 8px;\">异常 OJ：</p><ul>" + strings.Join(items, "") + "</ul>" +
		"<p style=\"margin:12px 0 0;font-size:12px;color:#737373;\">检测时间：" + mail.Escape(now) + "</p>"
}

func buildResourceAlertHTML(cpu, mem float64) string {
	now := time.Now().Format("2006-01-02 15:04:05")
	return "<p style=\"margin:0 0 12px;\">系统资源长时间占用过高（最近 30 分钟平均，正常同步短时高峰不计）。</p>" +
		fmt.Sprintf("<p style=\"margin:0 0 8px;\">CPU 均值 %.1f%%；内存均值 %.1f%%。</p>", cpu, mem) +
		"<p style=\"margin:12px 0 0;font-size:12px;color:#737373;\">检测时间：" + mail.Escape(now) + "</p>"
}
