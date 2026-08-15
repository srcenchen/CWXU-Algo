package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/mail"

	"github.com/go-kratos/kratos/v2/log"
)

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
