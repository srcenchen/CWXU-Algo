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

// 运维告警：系统处于异常状态时给「运维告警邮件接收人」发邮件。
//
// 覆盖两类异常（站点设置里配置收件人；未配置则不发送）：
//  1. OJ 同步异常：任一 OJ 最近失败且未恢复（15 分钟窗口内）即发异常邮件；
//     该 OJ 恢复后再发一封恢复邮件。异常期间不重复轰炸（每平台一个已通知标记，恢复才清除）；
//  2. 系统资源长期占用过高：最近 30 分钟平均 CPU ≥90% 或内存 ≥92%，持续 ≥30 分钟
//     （取长窗均值，正常同步的短时尖峰不计）。
//
// 去重：OJ 告警按平台标记（异常发信置位、恢复发信清位）；资源告警持续 ≥30 分钟才发、
// 发信后 2 小时窗口。Redis 锁保证多实例只跑一次。

const (
	// alertOjNotifiedPrefix 单平台「已发异常邮件」标记前缀；恢复后删除
	alertOjNotifiedPrefix = "ops:alert:oj:notified:"
	alertResSinceKey      = "ops:alert:res:since"
	alertResSentKey       = "ops:alert:res:sent"
	alertSentTTL          = 2 * time.Hour
	alertSustainMin       = 30 * time.Minute
	// alertOJAbnormalWindow 判定 OJ 处于「同步异常」的最近失败时间窗口
	alertOJAbnormalWindow = 15 * time.Minute
	// alertCPUThreshold / alertMemThreshold 资源长期占用阈值（百分比）
	alertCPUThreshold = 90.0
	alertMemThreshold = 92.0
	// alertResourceWindow 资源均值窗口（排除短时同步高峰）
	alertResourceWindow = 30 * time.Minute
)

// alertOjNotifiedKey 该 OJ 是否已发过异常告警邮件（存在即已发，等待恢复）
func alertOjNotifiedKey(platform string) string {
	return alertOjNotifiedPrefix + platform
}

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

	// —— 1. OJ 同步异常（单平台）：任一 OJ 异常即报，恢复即报 ——
	for _, p := range opsAlertOJPlatforms {
		lastFail, _ := t.rdb.Get(ctx, OjLastFailKey(p)).Int64()
		lastOK, _ := t.rdb.Get(ctx, OjLastOKKey(p)).Int64()
		abnormal := lastFail > 0 && time.Since(time.Unix(lastFail, 0)) <= alertOJAbnormalWindow && lastFail >= lastOK
		notifiedKey := alertOjNotifiedKey(p)
		notified, _ := t.rdb.Exists(ctx, notifiedKey).Result()

		switch {
		case abnormal && notified == 0:
			// 新异常：立即告警（不等待持续时长）
			t.sendOpsAlertMail(ctx, "OJ 同步异常："+p, buildOJAlertHTML(p, false), false)
			_ = t.rdb.Set(ctx, notifiedKey, time.Now().Unix(), 0).Err()
		case !abnormal && notified > 0:
			// 已恢复：发恢复邮件并清除标记
			t.sendOpsAlertMail(ctx, "OJ 同步恢复："+p, buildOJAlertHTML(p, true), true)
			_ = t.rdb.Del(ctx, notifiedKey).Err()
		}
	}

	// —— 2. 系统资源长期占用过高 ——
	if t.checkResourceAlert(ctx) {
		_ = t.rdb.SetNX(ctx, alertResSinceKey, time.Now().Unix(), 0)
		if t.alertReady(ctx, alertResSinceKey, alertResSentKey) {
			snap := resmon.SnapshotNow()
			t.sendOpsAlertMail(ctx, "系统资源长期占用过高",
				buildResourceAlertHTML(snap.CPUUsedPercent, snap.MemUsedPercent), false)
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
// recovered 为 true 时（OJ 恢复邮件）preheader 用「已恢复正常」，避免收件箱预览仍显示异常告警。
func (t *CronTask) sendOpsAlertMail(ctx context.Context, title, inner string, recovered bool) {
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
	pre := "系统长时间异常，请及时处理"
	if recovered {
		pre = "服务已恢复正常"
	}
	body := mail.Wrap(mail.LayoutOpts{
		Brand:     brand,
		Title:     title,
		Preheader: pre,
	}, inner)
	notify.EmailOpsRecipientsRuntime(rt, subject, body)
}

// buildOJAlertHTML 生成单平台同步异常 / 恢复邮件正文
func buildOJAlertHTML(platform string, recovered bool) string {
	now := time.Now().Format("2006-01-02 15:04:05")
	name := spiderDisplayName(platform)
	if recovered {
		return "<p style=\"margin:0 0 12px;\">OJ 提交同步已恢复：<b>" + mail.Escape(name) + "</b>。</p>" +
			"<p style=\"margin:12px 0 0;font-size:12px;color:#737373;\">检测时间：" + mail.Escape(now) + "</p>"
	}
	return "<p style=\"margin:0 0 12px;\">OJ 提交同步异常：<b>" + mail.Escape(name) + "</b>（最近 15 分钟内失败且未恢复）。</p>" +
		"<p style=\"margin:12px 0 0;font-size:12px;color:#737373;\">检测时间：" + mail.Escape(now) + "</p>"
}

func buildResourceAlertHTML(cpu, mem float64) string {
	now := time.Now().Format("2006-01-02 15:04:05")
	return "<p style=\"margin:0 0 12px;\">系统资源长时间占用过高（最近 30 分钟平均，正常同步短时高峰不计）。</p>" +
		fmt.Sprintf("<p style=\"margin:0 0 8px;\">CPU 均值 %.1f%%；内存均值 %.1f%%。</p>", cpu, mem) +
		"<p style=\"margin:12px 0 0;font-size:12px;color:#737373;\">检测时间：" + mail.Escape(now) + "</p>"
}
