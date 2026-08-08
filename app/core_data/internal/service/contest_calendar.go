package service

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/contest_calendar"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/event"
	mailpkg "cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/mailqueue"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	calspider "cwxu-algo/app/core_data/internal/spider/calendar"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

type ContestCalendarService struct {
	contest_calendar.UnimplementedContestCalendarServer
	dal  *dal.ContestCalendarDal
	reg  *discovery.Register
	data *data.Data
	mq   *event.RabbitMQ
}

func NewContestCalendarService(calDal *dal.ContestCalendarDal, reg *discovery.Register, d *data.Data, mq *event.RabbitMQ) *ContestCalendarService {
	return &ContestCalendarService{dal: calDal, reg: reg, data: d, mq: mq}
}

func (s *ContestCalendarService) toItem(m *model.ContestCalendar, subscribed bool) *contest_calendar.CalendarItem {
	return &contest_calendar.CalendarItem{
		Id:           uint32(m.ID),
		Platform:     m.Platform,
		PlatformName: m.PlatformName,
		ExternalId:   m.ExternalID,
		Name:         m.Name,
		Url:          m.URL,
		StartTime:    m.StartTime,
		EndTime:      m.EndTime,
		Source:       m.Source,
		IconUrl:      m.IconURL,
		Subscribed:   subscribed,
	}
}

func (s *ContestCalendarService) ListCalendar(ctx context.Context, req *contest_calendar.ListCalendarReq) (*contest_calendar.ListCalendarRes, error) {
	status := strings.TrimSpace(req.GetStatus())
	if status == "" {
		status = "upcoming"
	}
	list, total, err := s.dal.List(dal.CalendarListQuery{
		Platform: req.GetPlatform(),
		Keyword:  req.GetKeyword(),
		Status:   status,
		TimeFrom: req.GetTimeFrom(),
		TimeTo:   req.GetTimeTo(),
		Limit:    int(req.GetLimit()),
		Offset:   int(req.GetOffset()),
	})
	if err != nil {
		log.Errorf("ListCalendar: %v", err)
		return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
	}

	// 登录用户：标记是否已订阅（平台或单场）。
	// contest enabled=false 视为本场静默，覆盖平台订阅。
	subPlatforms := map[string]bool{}
	subContestsOn := map[uint]bool{}
	subContestsMuted := map[uint]bool{}
	if user := auth.GetCurrentUser(ctx); user != nil {
		if subs, err := s.dal.ListSubsByUser(int64(user.UserID)); err == nil {
			for _, sub := range subs {
				if sub.Scope == model.CalScopePlatform {
					if sub.Enabled {
						subPlatforms[sub.Platform] = true
					}
				} else if sub.Scope == model.CalScopeContest {
					if sub.Enabled {
						subContestsOn[sub.CalendarID] = true
					} else {
						subContestsMuted[sub.CalendarID] = true
					}
				}
			}
		}
	}

	items := make([]*contest_calendar.CalendarItem, 0, len(list))
	for i := range list {
		m := &list[i]
		subbed := false
		if subContestsMuted[m.ID] {
			subbed = false
		} else if subContestsOn[m.ID] {
			subbed = true
		} else if subPlatforms[m.Platform] {
			subbed = true
		}
		items = append(items, s.toItem(m, subbed))
	}
	return &contest_calendar.ListCalendarRes{
		Code:    0,
		Message: "OK",
		Data:    items,
		Total:   total,
	}, nil
}

func (s *ContestCalendarService) ListPlatforms(ctx context.Context, req *contest_calendar.ListPlatformsReq) (*contest_calendar.ListPlatformsRes, error) {
	_ = ctx
	_ = req
	// 固定统计即将开始的场次（前端平台 chips 用）
	stats, err := s.dal.ListPlatforms(true)
	if err != nil {
		log.Errorf("ListPlatforms: %v", err)
		return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
	}
	items := make([]*contest_calendar.PlatformItem, 0, len(stats))
	for _, st := range stats {
		items = append(items, &contest_calendar.PlatformItem{
			Platform:     st.Platform,
			PlatformName: st.PlatformName,
			IconUrl:      st.IconURL,
			Count:        st.Count,
		})
	}
	return &contest_calendar.ListPlatformsRes{Code: 0, Message: "OK", Data: items}, nil
}

func (s *ContestCalendarService) subToProto(sub *model.ContestCalendarSub) *contest_calendar.SubItem {
	item := &contest_calendar.SubItem{
		Id:             uint32(sub.ID),
		Scope:          sub.Scope,
		Platform:       sub.Platform,
		CalendarId:     uint32(sub.CalendarID),
		AdvanceMinutes: int32(sub.AdvanceMinutes),
		Enabled:        sub.Enabled,
	}
	if sub.Scope == model.CalScopeContest && sub.CalendarID > 0 {
		if cal, err := s.dal.GetByID(sub.CalendarID); err == nil && cal != nil {
			item.ContestName = cal.Name
			item.ContestUrl = cal.URL
			item.StartTime = cal.StartTime
		}
	}
	return item
}

func (s *ContestCalendarService) GetMySubs(ctx context.Context, _ *contest_calendar.GetMySubsReq) (*contest_calendar.GetMySubsRes, error) {
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return &contest_calendar.GetMySubsRes{Code: 1, Message: "未登录"}, nil
	}
	subs, err := s.dal.ListSubsByUser(int64(user.UserID))
	if err != nil {
		return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
	}
	items := make([]*contest_calendar.SubItem, 0, len(subs))
	for i := range subs {
		items = append(items, s.subToProto(&subs[i]))
	}
	return &contest_calendar.GetMySubsRes{Code: 0, Message: "OK", Data: items}, nil
}

func (s *ContestCalendarService) UpsertSub(ctx context.Context, req *contest_calendar.UpsertSubReq) (*contest_calendar.UpsertSubRes, error) {
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return &contest_calendar.UpsertSubRes{Code: 1, Message: "未登录"}, nil
	}
	scope := strings.ToLower(strings.TrimSpace(req.GetScope()))
	if scope != model.CalScopePlatform && scope != model.CalScopeContest {
		return &contest_calendar.UpsertSubRes{Code: 2, Message: "scope 必须是 platform 或 contest"}, nil
	}
	adv := int(req.GetAdvanceMinutes())
	if adv <= 0 {
		adv = 180 // 默认提前 3 小时
	}
	if !model.ValidCalendarAdvance(adv) {
		return &contest_calendar.UpsertSubRes{Code: 3, Message: "提前时间不在允许范围内"}, nil
	}

	// 校验邮箱（服务间 GetContactEmail，不做隐私剥离）
	email, err := s.lookupUserEmail(ctx, int64(user.UserID))
	if err != nil {
		log.Warnf("lookup email: %v", err)
	}
	if strings.TrimSpace(email) == "" {
		return &contest_calendar.UpsertSubRes{Code: 4, Message: "请先在个人资料中绑定邮箱后再订阅"}, nil
	}

	sub := &model.ContestCalendarSub{
		UserID:         int64(user.UserID),
		Scope:          scope,
		AdvanceMinutes: adv,
		Enabled:        req.GetEnabled(),
	}
	// proto3 bool 默认 false：若客户端想 enabled=true 会传 true；
	// 新建默认应开启。约定：未传 enabled 时当 true 无法区分。
	// 前端总是显式传 enabled。若 false 则为关闭订阅。
	// 首次创建时如果 enabled=false 也允许（等于预留关闭）。

	var cal *model.ContestCalendar
	if scope == model.CalScopePlatform {
		plat := calspider.NormalizePlatform(req.GetPlatform())
		if plat == "" {
			return &contest_calendar.UpsertSubRes{Code: 5, Message: "platform 不能为空"}, nil
		}
		sub.Platform = plat
		sub.CalendarID = 0
	} else {
		cid := uint(req.GetCalendarId())
		if cid == 0 {
			return &contest_calendar.UpsertSubRes{Code: 6, Message: "calendarId 不能为空"}, nil
		}
		var getErr error
		cal, getErr = s.dal.GetByID(cid)
		if getErr != nil {
			if getErr == gorm.ErrRecordNotFound {
				return &contest_calendar.UpsertSubRes{Code: 7, Message: "比赛不存在"}, nil
			}
			return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
		}
		if cal.StartTime <= time.Now().Unix() {
			return &contest_calendar.UpsertSubRes{Code: 8, Message: "比赛已开始或已结束，无法订阅"}, nil
		}
		sub.CalendarID = cid
		sub.Platform = cal.Platform
	}

	created, prevEnabled, prevAdvance, err := s.dal.UpsertSub(sub)
	if err != nil {
		log.Errorf("UpsertSub: %v", err)
		return nil, errors.InternalServer("保存失败", "服务暂时不可用")
	}
	// 重新加载以拿 ID
	subs, _ := s.dal.ListSubsByUser(int64(user.UserID))
	var saved *model.ContestCalendarSub
	for i := range subs {
		if subs[i].Scope == sub.Scope && subs[i].Platform == sub.Platform && subs[i].CalendarID == sub.CalendarID {
			saved = &subs[i]
			break
		}
	}
	if saved == nil {
		saved = sub
	}

	// 订阅成功确认信（与开赛提醒独立）：
	// enabled=true 保存成功后**每次**异步发信（含已订阅再点、取消后再订）；不限流。
	// enabled=false（关闭）不发「订阅成功」信。
	_ = created
	_ = prevEnabled
	_ = prevAdvance
	if req.GetEnabled() {
		go s.sendSubscribeConfirmMail(email, saved, cal, true, false)
	}

	return &contest_calendar.UpsertSubRes{
		Code:    0,
		Message: "OK",
		Data:    s.subToProto(saved),
	}, nil
}

func (s *ContestCalendarService) DeleteSub(ctx context.Context, req *contest_calendar.DeleteSubReq) (*contest_calendar.DeleteSubRes, error) {
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return &contest_calendar.DeleteSubRes{Code: 1, Message: "未登录"}, nil
	}
	scope := strings.ToLower(strings.TrimSpace(req.GetScope()))
	if scope != model.CalScopePlatform && scope != model.CalScopeContest {
		return &contest_calendar.DeleteSubRes{Code: 2, Message: "scope 无效"}, nil
	}
	plat := calspider.NormalizePlatform(req.GetPlatform())
	if err := s.dal.DeleteSub(int64(user.UserID), scope, plat, uint(req.GetCalendarId())); err != nil {
		return nil, errors.InternalServer("删除失败", "服务暂时不可用")
	}
	return &contest_calendar.DeleteSubRes{Code: 0, Message: "OK"}, nil
}

func (s *ContestCalendarService) lookupUserEmail(ctx context.Context, userID int64) (string, error) {
	if s.reg == nil {
		return "", nil
	}
	cli, err := userrpc.ProfileClient(&s.reg.Reg)
	if err != nil {
		return "", err
	}
	res, err := cli.GetContactEmail(ctx, &profile.GetContactEmailReq{UserId: userID})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	return strings.TrimSpace(res.GetEmail()), nil
}

// sendSubscribeConfirmMail 异步发送订阅成功确认信。
// rateLimited 参数保留兼容调用方；产品要求每次订阅都发，调用处传 false。
func (s *ContestCalendarService) sendSubscribeConfirmMail(
	to string,
	sub *model.ContestCalendarSub,
	cal *model.ContestCalendar,
	doSend bool,
	_ bool, // rateLimited: 已禁用限流
) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("UpsertSub confirm mail panic: %v", r)
		}
	}()
	if !doSend || s.data == nil || strings.TrimSpace(to) == "" || sub == nil {
		return
	}

	// site_configs 在 user 库；core_data 只读 Redis（user 启动/定时/管理端 Publish）。
	// 切勿传 s.data.DB，否则 miss 时会误读 core 库并可能污染 Redis。
	rt := sitesettings.Load(context.Background(), s.data.RDB, nil)
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		log.Warnf("UpsertSub confirm mail: SMTP empty (Redis miss or not published by user service), skip to=%s user=%d", to, sub.UserID)
		return
	}
	siteTitle := rt.SiteTitle
	if siteTitle == "" {
		siteTitle = "GoAlgo"
	}
	advLabel := formatAdvanceMinutes(sub.AdvanceMinutes)
	plat := html.EscapeString(sub.Platform)
	scopeLabel := "平台订阅"
	detail := fmt.Sprintf("平台 <strong>%s</strong> 的全部即将开始比赛", plat)
	if sub.Scope == model.CalScopeContest {
		scopeLabel = "单场订阅"
		name := sub.Platform
		if cal != nil {
			if cal.PlatformName != "" {
				plat = html.EscapeString(cal.PlatformName)
			}
			name = cal.Name
		}
		detail = fmt.Sprintf("比赛 <strong>%s</strong>（%s）", html.EscapeString(name), plat)
	}
	subject := fmt.Sprintf("[%s] 比赛提醒订阅成功", siteTitle)
	inner := fmt.Sprintf(`
<p style="margin:0 0 12px;">你好，</p>
<p style="margin:0 0 14px;">你已成功订阅比赛邮件提醒（%s）。</p>
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:14px;">
<tr><td style="padding:6px 12px 6px 0;color:#737373;width:72px;">类型</td><td style="padding:6px 0;">%s</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">内容</td><td style="padding:6px 0;">%s</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">提前量</td><td style="padding:6px 0;">开赛前 <strong>%s</strong></td></tr>
</table>
<p style="margin:16px 0 0;color:#737373;font-size:12px;">管理订阅：登录 %s → 比赛 → 比赛日历。若不再需要提醒，可在页面中取消订阅。</p>
`, html.EscapeString(scopeLabel), html.EscapeString(scopeLabel), detail, html.EscapeString(advLabel), html.EscapeString(siteTitle))
	body := mailpkg.Wrap(mailpkg.LayoutOpts{
		Brand:     siteTitle,
		Title:     "比赛提醒订阅成功",
		Preheader: "你已成功订阅比赛邮件提醒",
	}, inner)
	// 异步入队：SMTP 发送由 mail consumer 消费，不再阻塞订阅接口
	if !mailqueue.Enqueue(s.mq, to, subject, body) {
		log.Warnf("UpsertSub confirm mail enqueue FAIL to=%s user=%d", to, sub.UserID)
		return
	}
	log.Infof("UpsertSub confirm mail enqueued to=%s user=%d scope=%s", to, sub.UserID, sub.Scope)
}

func formatAdvanceMinutes(m int) string {
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
