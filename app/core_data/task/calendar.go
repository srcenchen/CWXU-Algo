package task

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"cwxu-algo/api/user/v1/profile"
	mailpkg "cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	calspider "cwxu-algo/app/core_data/internal/spider/calendar"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/log"
)

const maxAdvanceMinutes = 4320 // 与 model 白名单最大值一致

func (t *CronTask) calDal() *dal.ContestCalendarDal {
	return dal.NewContestCalendarDalDB(t.db)
}

func (t *CronTask) runCalendarCrawl() {
	if loadgateSkipTick("calendar_crawl") {
		return
	}
	if !t.tryCronLock("calendar_crawl", 30*time.Minute) {
		return
	}
	items, errs := calspider.FetchAll()
	for _, e := range errs {
		log.Warnf("CronTask calendar crawl source error: %v", e)
	}
	if len(items) == 0 {
		log.Warnf("CronTask calendar crawl: empty result (errs=%d)", len(errs))
		return
	}
	n, err := t.calDal().UpsertItems(items)
	if err != nil {
		log.Errorf("CronTask calendar upsert: %v", err)
		return
	}
	if nerr := t.calDal().NormalizeLegacyPlatformNames(); nerr != nil {
		log.Warnf("CronTask calendar normalize platform: %v", nerr)
	}
	keepBefore := time.Now().Add(-7 * 24 * time.Hour).Unix()
	deleted, _ := t.calDal().CleanupEnded(keepBefore)
	_, _ = t.calDal().CleanupNotifyLogs(time.Now().Add(-30 * 24 * time.Hour))
	log.Infof("CronTask calendar crawl: upserted=%d deleted_ended=%d item_in=%d errs=%d",
		n, deleted, len(items), len(errs))
}

func (t *CronTask) runCalendarNotify() {
	if loadgateSkipTick("calendar_notify") {
		return
	}
	if !t.tryCronLock("calendar_notify", 4*time.Minute) {
		return
	}
	now := time.Now()
	nowUnix := now.Unix()
	maxSec := int64(maxAdvanceMinutes * 60)

	contests, err := t.calDal().ListUpcomingInWindow(nowUnix, maxSec)
	if err != nil {
		log.Errorf("CronTask calendar notify list contests: %v", err)
		return
	}
	if len(contests) == 0 {
		return
	}
	subs, err := t.calDal().ListEnabledSubs()
	if err != nil {
		log.Errorf("CronTask calendar notify list subs: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}

	// platform / contest（仅 enabled=true）；contest enabled=false 为静默，覆盖平台
	byPlatform := make(map[string][]model.ContestCalendarSub)
	byContestOn := make(map[uint][]model.ContestCalendarSub)
	muted := make(map[string]struct{}) // "userID:calendarID"
	muteRows, muteErr := t.calDal().ListMutedContestSubs()
	if muteErr != nil {
		log.Warnf("CronTask calendar notify list mutes: %v", muteErr)
	} else {
		for _, m := range muteRows {
			muted[fmt.Sprintf("%d:%d", m.UserID, m.CalendarID)] = struct{}{}
		}
	}
	for _, s := range subs {
		if s.Scope == model.CalScopePlatform {
			byPlatform[s.Platform] = append(byPlatform[s.Platform], s)
		} else if s.Scope == model.CalScopeContest && s.CalendarID > 0 {
			byContestOn[s.CalendarID] = append(byContestOn[s.CalendarID], s)
		}
	}

	// site_configs 在 user 库；只读 Redis，勿传 core_data DB
	rt := sitesettings.Load(context.Background(), t.rdb, nil)
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		log.Warnf("CronTask calendar notify: SMTP empty (Redis miss or not published by user service), skip send")
		return
	}

	emailCache := make(map[int64]string)
	sent := 0
	skipped := 0
	const batchLimit = 200
	for _, c := range contests {
		// 先 contest 再 platform；同用户同 advance 只发一封（时间重复去重）
		matched := make([]model.ContestCalendarSub, 0, 8)
		matched = append(matched, byContestOn[c.ID]...)
		matched = append(matched, byPlatform[c.Platform]...)
		// 去重：同一用户同一 advance 只处理一次；contest 优先（先入 matched）
		seen := map[string]struct{}{}
		for _, sub := range matched {
			// contest 静默覆盖平台：本场该用户不提醒
			if _, ok := muted[fmt.Sprintf("%d:%d", sub.UserID, c.ID)]; ok {
				skipped++
				continue
			}
			key := fmt.Sprintf("%d:%d", sub.UserID, sub.AdvanceMinutes)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			notifyAt := c.StartTime - int64(sub.AdvanceMinutes)*60
			if nowUnix < notifyAt {
				continue
			}
			if nowUnix >= c.StartTime {
				continue
			}
			// 先原子占坑再发信，避免「SMTP 已投递但日志未写」导致下轮 cron 重发；
			// 多实例并发时也只有一个能 claim 成功。
			claimed, err := t.calDal().TryClaimNotifyLog(sub.UserID, c.ID, sub.AdvanceMinutes)
			if err != nil {
				log.Warnf("CronTask calendar claim user=%d contest=%d: %v", sub.UserID, c.ID, err)
				skipped++
				continue
			}
			if !claimed {
				skipped++
				continue
			}
			to, ok := emailCache[sub.UserID]
			if !ok {
				to = t.lookupEmail(sub.UserID)
				emailCache[sub.UserID] = to
			}
			if strings.TrimSpace(to) == "" {
				// 无邮箱：释放占坑，绑定后仍可收到提醒
				_ = t.calDal().DeleteNotifyLog(sub.UserID, c.ID, sub.AdvanceMinutes)
				skipped++
				continue
			}
			subject, body := buildCalendarMail(rt.SiteTitle, &c, sub.AdvanceMinutes)
			if err := sender.Send(to, subject, body); err != nil {
				log.Warnf("CronTask calendar mail user=%d contest=%d: %v", sub.UserID, c.ID, err)
				// SMTP 明确失败才释放，允许下次重试；成功则保留日志防重发
				_ = t.calDal().DeleteNotifyLog(sub.UserID, c.ID, sub.AdvanceMinutes)
				skipped++
				continue
			}
			sent++
			if sent >= batchLimit {
				log.Infof("CronTask calendar notify: hit batch limit %d sent=%d skipped=%d", batchLimit, sent, skipped)
				return
			}
		}
	}
	if sent > 0 || skipped > 0 {
		log.Infof("CronTask calendar notify: sent=%d skipped=%d contests=%d", sent, skipped, len(contests))
	}
}

func (t *CronTask) lookupEmail(userID int64) string {
	if t.reg == nil || userID <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := userrpc.ProfileClient(&t.reg.Reg)
	if err != nil {
		log.Warnf("CronTask calendar email dial: %v", err)
		return ""
	}
	res, err := cli.GetContactEmail(ctx, &profile.GetContactEmailReq{UserId: userID})
	if err != nil || res == nil {
		return ""
	}
	return strings.TrimSpace(res.GetEmail())
}

func buildCalendarMail(siteTitle string, c *model.ContestCalendar, advanceMin int) (subject, body string) {
	if siteTitle == "" {
		siteTitle = "GoAlgo"
	}
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Unix(c.StartTime, 0).In(loc).Format("2006-01-02 15:04")
	end := time.Unix(c.EndTime, 0).In(loc).Format("2006-01-02 15:04")
	advLabel := formatAdvance(advanceMin)
	name := html.EscapeString(c.Name)
	plat := html.EscapeString(c.PlatformName)
	if plat == "" {
		plat = html.EscapeString(c.Platform)
	}
	subject = fmt.Sprintf("[%s] 比赛提醒：%s 将于 %s 开始", siteTitle, c.Name, start)
	var inner strings.Builder
	inner.WriteString(mailpkg.P("你好，"))
	inner.WriteString(fmt.Sprintf(
		`<p style="margin:0 0 14px;font-size:14px;line-height:1.6;color:%s;">你订阅的比赛即将开始（提前 <strong>%s</strong> 提醒）：</p>`,
		mailpkg.ColorForeground, html.EscapeString(advLabel),
	))
	inner.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:14px;">`)
	inner.WriteString(mailpkg.RowKV("平台", plat))
	inner.WriteString(mailpkg.RowKV("比赛", "<strong>"+name+"</strong>"))
	inner.WriteString(mailpkg.RowKV("开始", start+"（北京时间）"))
	inner.WriteString(mailpkg.RowKV("结束", end+"（北京时间）"))
	inner.WriteString(`</table>`)
	inner.WriteString(`<p style="margin:18px 0 8px;">`)
	if c.URL != "" {
		inner.WriteString(mailpkg.BtnPrimary(c.URL, "前往比赛页面"))
	}
	inner.WriteString(`</p>`)
	inner.WriteString(fmt.Sprintf(
		`<p style="margin:16px 0 0;color:%s;font-size:12px;">管理订阅：登录 %s → 比赛 → 比赛日历。若不再需要提醒，可在页面中取消订阅。</p>`,
		mailpkg.ColorMutedFg, html.EscapeString(siteTitle),
	))
	body = mailpkg.Wrap(mailpkg.LayoutOpts{
		Brand:     siteTitle,
		Title:     "比赛提醒",
		Preheader: fmt.Sprintf("%s 将于 %s 开始", c.Name, start),
	}, inner.String())
	return subject, body
}

func formatAdvance(m int) string {
	if m < 60 {
		return fmt.Sprintf("%d 分钟", m)
	}
	if m%1440 == 0 {
		return fmt.Sprintf("%d 天", m/1440)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d 小时", m/60)
	}
	return fmt.Sprintf("%d 分钟", m)
}
