package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
)

// subscriptionReminderWindowDays 到期前提醒窗口（天）
const subscriptionReminderWindowDays = 3

// subscriptionReminderEvery 到期提醒扫描间隔（6 小时，保证 3 天/1 天窗口不漏）
const subscriptionReminderEvery = 6 * time.Hour

// subscriptionReminderLockKey 提醒扫描分布式锁（多 user 实例只跑一次，避免重复发信）
const subscriptionReminderLockKey = "sub:reminder:sweep:lock"

// tryReminderLock 原子抢锁；TTL 略小于扫描间隔，保证下轮可抢。
func tryReminderLock(rdb *redis.Client) bool {
	if rdb == nil {
		return true
	}
	ttl := subscriptionReminderEvery - time.Hour
	ok, err := rdb.SetNX(context.Background(), subscriptionReminderLockKey, "1", ttl).Result()
	if err != nil {
		log.Warnf("subscription reminder sweep lock: %v", err)
		return false
	}
	return ok
}

// runtime 站点运行时配置（SMTP / 站点标题）
func (s *SubscriptionService) runtime(ctx context.Context) *sitesettings.Runtime {
	if s == nil || s.data == nil {
		return nil
	}
	return sitesettings.Load(ctx, s.data.RDB, s.data.DB)
}

func (s *SubscriptionService) mailSender(ctx context.Context) *mail.Sender {
	rt := s.runtime(ctx)
	if rt == nil {
		return nil
	}
	return rt.MailSender()
}

func (s *SubscriptionService) siteTitle(ctx context.Context) string {
	rt := s.runtime(ctx)
	if rt == nil {
		return "GoAlgo"
	}
	if t := strings.TrimSpace(rt.SiteTitle); t != "" {
		return t
	}
	return "GoAlgo"
}

// sendThankYouMail 支付成功感谢信（异步调用方负责 goroutine）。
// 无邮箱 / SMTP 未配置 / 发送失败均静默跳过（低频弱任务，可接受丢失）。
func (s *SubscriptionService) sendThankYouMail(ctx context.Context, userID uint, tierLabel string, months int) {
	if s == nil || s.data == nil || s.data.DB == nil {
		return
	}
	email := notify.LookupUserEmail(s.data.DB, userID)
	if email == "" {
		return
	}
	sender := s.mailSender(ctx)
	if sender == nil || !sender.Configured() {
		return
	}
	var info userSubMailInfo
	_ = s.data.DB.WithContext(ctx).Raw("SELECT name, sub_expire_at FROM users WHERE id = ?", userID).Scan(&info).Error
	subject, body := thankYouMailContent(s.siteTitle(ctx), tierLabel, months, info.Name, info.SubExpire)
	if err := sender.Send(email, subject, body); err != nil {
		log.Warnf("sendThankYouMail user=%d: %v", userID, err)
	}
}

type userSubMailInfo struct {
	Name      string     `gorm:"column:name"`
	SubExpire *time.Time `gorm:"column:sub_expire_at"`
}

// thankYouMailContent 感谢信正文（纯函数，便于单测）。
func thankYouMailContent(brand, tierLabel string, months int, name string, expireAt *time.Time) (subject, body string) {
	if brand == "" {
		brand = "GoAlgo"
	}
	if name == "" {
		name = "朋友"
	}
	expireStr := ""
	if expireAt != nil {
		expireStr = expireAt.Format("2006-01-02")
	}
	subject = fmt.Sprintf("【%s】感谢你的支持！%s 会员已开通", brand, tierLabel)
	inner := mail.P(fmt.Sprintf("%s，感谢你对 %s 的支持！", name, brand))
	if months > 0 {
		inner += mail.P(fmt.Sprintf("你已成功开通 %s 会员（%d 个月）", tierLabel, months))
	} else {
		inner += mail.P(fmt.Sprintf("你已成功开通 %s 会员", tierLabel))
	}
	if expireStr != "" {
		inner += mail.P(fmt.Sprintf("有效期至 %s，会员专属能力现已生效。", expireStr))
	}
	inner += mail.P("你的支持会用于维持服务器与 AI 服务的日常运营，让我们能继续把算法学习体验做得更好。")
	inner += `<div style="margin:16px 0;">` + mail.BtnPrimary(mail.SiteHomeURL, "开始使用") + `</div>`
	body = mail.Wrap(mail.LayoutOpts{Brand: brand, Title: "感谢你的支持", Preheader: subject}, inner)
	return subject, body
}

// expiryReminderContent 到期提醒邮件正文（纯函数，便于单测）。window=3 或 1（天）。
func expiryReminderContent(brand string, u dal.ExpiringUser, window int) (subject, body string) {
	if brand == "" {
		brand = "GoAlgo"
	}
	tierLabel := tierName(u.Tier)
	days := "3 天"
	title := "会员即将到期"
	if window == 1 {
		days = "1 天"
		title = "会员明天到期"
	}
	expireStr := u.ExpireAt.Format("2006-01-02")
	subject = fmt.Sprintf("【%s】你的 %s 会员将在 %s 后到期", brand, tierLabel, days)
	greeting := "你好"
	if strings.TrimSpace(u.Name) != "" {
		greeting += "，" + strings.TrimSpace(u.Name)
	}
	inner := mail.P(greeting+"！") +
		mail.P(fmt.Sprintf("你的 %s 会员将在 %s（%s）到期，届时会自动回落到免费版，部分会员专属能力会暂时无法使用。", tierLabel, days, expireStr)) +
		mail.P("如果还想继续使用会员功能，记得提前续费，避免出现空档。") +
		`<div style="margin:16px 0;">` + mail.BtnPrimary(mail.SiteHomeURL+"/profile", "去续费") + `</div>`
	body = mail.Wrap(mail.LayoutOpts{Brand: brand, Title: title, Preheader: subject}, inner)
	return subject, body
}

// startSubscriptionReminderSweep 后台到期提醒扫描：
//   - 先晋升到期排队档（Pro 到期自动续 Plus）
//   - 对「3 天内到期且未提醒」的用户发提醒邮件（3 天/1 天各一次）
//
// 返回 stop 函数，进程退出时调用。
func startSubscriptionReminderSweep(d *data.Data) func() {
	if d == nil || d.DB == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	go func() {
		ctx := context.Background()
		subDal := dal.NewSubscriptionDal(d)
		sweep := func() {
			if !tryReminderLock(d.RDB) {
				return
			}
			if n, err := subDal.PromoteDue(ctx); err != nil {
				log.Warnf("subscription reminder sweep promote: %v", err)
			} else if n > 0 {
				log.Infof("subscription reminder sweep promoted %d users", n)
			}
			users, err := subDal.ListExpiringSoon(ctx, subscriptionReminderWindowDays)
			if err != nil {
				log.Warnf("subscription reminder sweep list: %v", err)
				return
			}
			sent := 0
			for _, u := range users {
				daysLeft := time.Until(u.ExpireAt).Hours() / 24
				var window int
				if daysLeft <= 1 && u.Reminded != 1 {
					window = 1
				} else if daysLeft <= float64(subscriptionReminderWindowDays) && u.Reminded < 3 {
					window = 3
				}
				if window == 0 {
					continue
				}
				if sendExpiryReminderMail(d, u, window, ctx) {
					if err := subDal.MarkReminded(ctx, u.UserID, window); err != nil {
						log.Warnf("subscription reminder sweep mark user=%d: %v", u.UserID, err)
					}
					sent++
				}
			}
			if sent > 0 {
				log.Infof("subscription reminder sweep sent %d reminder emails", sent)
			}
		}
		// 启动先跑一次（覆盖进程重启期间到期的用户），再按周期轮询
		sweep()
		ticker := time.NewTicker(subscriptionReminderEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
	return func() { close(stopCh) }
}

// sendExpiryReminderMail 发送到期提醒邮件；返回是否已成功投递。
func sendExpiryReminderMail(d *data.Data, u dal.ExpiringUser, window int, ctx context.Context) bool {
	if d == nil || d.DB == nil || strings.TrimSpace(u.Email) == "" {
		return false
	}
	rt := sitesettings.Load(ctx, d.RDB, d.DB)
	if rt == nil {
		return false
	}
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		return false
	}
	brand := strings.TrimSpace(rt.SiteTitle)
	if brand == "" {
		brand = "GoAlgo"
	}
	subject, body := expiryReminderContent(brand, u, window)
	if err := sender.Send(u.Email, subject, body); err != nil {
		log.Warnf("sendExpiryReminderMail user=%d window=%d: %v", u.UserID, window, err)
		return false
	}
	return true
}
