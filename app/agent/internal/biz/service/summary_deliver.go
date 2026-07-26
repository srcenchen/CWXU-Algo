package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/mail"

	"github.com/go-kratos/kratos/v2/log"
)

const recentSummaryTTL = 4 * time.Hour

type recentSummaryPayload struct {
	Msg        []string `json:"msg"`
	UpdateTime int64    `json:"updateTime"`
}

func (uc *SummaryUseCase) sendHTMLEmail(to, subject, body string) error {
	body = stripCodeFence(body)
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("邮件正文为空")
	}
	if to == "" {
		return fmt.Errorf("收件人为空")
	}
	rt := uc.runtime(context.Background())
	sender := mail.NewSender(rt.SMTPConf())
	if !sender.Configured() {
		return fmt.Errorf("SMTP 未配置（请在站点设置中填写）")
	}
	return sender.Send(to, subject, body)
}

func (uc *SummaryUseCase) saveRecentSummary(ctx context.Context, userId int64, raw string) error {
	raw = stripCodeFence(raw)
	var payload recentSummaryPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fmt.Errorf("解析近期总结 JSON 失败: %w; raw=%s", err, truncateRunes(raw, 200))
	}
	if len(payload.Msg) < 5 || len(payload.Msg) > 10 {
		return fmt.Errorf("msg 条数应在 5-10，实际 %d", len(payload.Msg))
	}
	for i, m := range payload.Msg {
		if utf8.RuneCountInString(m) > 40 {
			// 截断过长条目，避免前端撑破
			payload.Msg[i] = string([]rune(m)[:40])
		}
	}
	if payload.UpdateTime == 0 {
		payload.UpdateTime = time.Now().Unix()
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("agent:summary:%d:recent", userId)
	if err := uc.redis.Set(ctx, key, string(b), recentSummaryTTL).Err(); err != nil {
		return fmt.Errorf("写入 Redis 失败: %w", err)
	}
	log.Infof("近期总结已写入 %s ttl=%s", key, recentSummaryTTL)
	return nil
}

func (uc *SummaryUseCase) tryAcquireLock(ctx context.Context, key string, ttl time.Duration) bool {
	ok, err := uc.redis.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		log.Warnf("获取锁失败 %s: %v", key, err)
		return true // 锁失败不阻塞主流程
	}
	return ok
}

// releaseLock 显式释放锁（失败路径用：避免瞬时失败在 TTL 内一直撞锁被静默吞掉）
func (uc *SummaryUseCase) releaseLock(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := uc.redis.Del(ctx, key).Err(); err != nil {
		log.Warnf("释放锁失败 %s: %v", key, err)
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
