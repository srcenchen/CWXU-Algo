package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"cwxu-algo/api/user/v1/subscription"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"
	"cwxu-algo/app/user/internal/data/model"
	"cwxu-algo/app/user/internal/external/payment"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
)

// paymentNotifyURL 支付FM异步回调地址（不存库；环境变量 PAYMENT_NOTIFY_URL 可覆盖）
const paymentNotifyURL = "https://algo.zhiyuansofts.cn/v1/payment/notify"

func notifyURL() string {
	if v := strings.TrimSpace(os.Getenv("PAYMENT_NOTIFY_URL")); v != "" {
		return v
	}
	return paymentNotifyURL
}

// SubscriptionService C 端订阅：套餐 / 订单 / 订阅状态 / 站管管理 / AI 配额
type SubscriptionService struct {
	subscription.UnimplementedSubscriptionServer
	data       *data.Data
	subDal     *dal.SubscriptionDal
	profileDal *dal.ProfileDal
}

// NewSubscriptionService 创建订阅服务
func NewSubscriptionService(d *data.Data, subDal *dal.SubscriptionDal, profileDal *dal.ProfileDal) *SubscriptionService {
	return &SubscriptionService{data: d, subDal: subDal, profileDal: profileDal}
}

// paymentGateway 从站点配置构建支付FM网关；未配置返回错误（「支付未配置」）
func (s *SubscriptionService) paymentGateway(ctx context.Context) (*payment.PayFmGateway, error) {
	rt := sitesettings.Load(ctx, s.data.RDB, s.data.DB)
	apiBase, merchantNo, secret, payType, configured := rt.PayFmConf()
	if !configured {
		return nil, fmt.Errorf("支付未配置（请在站点设置填写支付FM接口地址/商户号/接入密钥）")
	}
	g, err := payment.NewPayFmGateway(apiBase, merchantNo, secret, payType, notifyURL())
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ListPlans 公开：套餐列表（前端对比表）
func (s *SubscriptionService) ListPlans(ctx context.Context, _ *subscription.ListPlansReq) (*subscription.ListPlansRes, error) {
	plans, err := s.subDal.ListPlans(ctx, false)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &subscription.ListPlansRes{Code: 0, Message: "success", Plans: make([]*subscription.Plan, 0, len(plans))}
	for _, p := range plans {
		res.Plans = append(res.Plans, planPB(p))
	}
	return res, nil
}

func planPB(p model.SubscriptionPlan) *subscription.Plan {
	return &subscription.Plan{
		Plan:               p.Plan,
		PriceCents:         p.PriceCents,
		ManualRefreshDaily: int32(p.ManualRefreshDaily),
		SyncIntervalMin:    int32(p.SyncIntervalMin),
		AiAnalyzeMonth:     int32(p.AiAnalyzeMonth),
		EnableFetchProblem: p.EnableFetchProblem,
		EnableAiAnalyze:    p.EnableAiAnalyze,
		EnableAiDaily:      p.EnableAiDaily,
		EnableRegularDaily: p.EnableRegularDaily,
		Days:               int32(p.Days),
		Enabled:            p.Enabled,
	}
}

// CreateOrder 登录：创建订单（调支付FM下单，返回支付链接）
func (s *SubscriptionService) CreateOrder(ctx context.Context, req *subscription.CreateOrderReq) (*subscription.CreateOrderRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &subscription.CreateOrderRes{Code: 1, Message: "请先登录"}, nil
	}
	plan := strings.TrimSpace(req.GetPlan())
	if plan != "plus" && plan != "pro" {
		return &subscription.CreateOrderRes{Code: 1, Message: "请选择有效套餐（Plus / Pro）"}, nil
	}
	p, err := s.subDal.PlanByTier(ctx, plan)
	if err != nil {
		return &subscription.CreateOrderRes{Code: 1, Message: "套餐不存在"}, nil
	}
	if !p.Enabled || p.PriceCents <= 0 {
		return &subscription.CreateOrderRes{Code: 1, Message: "该套餐未上架，暂不可购买"}, nil
	}
	g, err := s.paymentGateway(ctx)
	if err != nil {
		return &subscription.CreateOrderRes{Code: 1, Message: err.Error()}, nil
	}
	orderNo := fmt.Sprintf("S%d", time.Now().UnixNano())
	order, err := s.subDal.CreateOrder(ctx, orderNo, pd.UserID, plan, p.PriceCents)
	if err != nil {
		log.Errorf("CreateOrder 建单 user=%d plan=%s: %v", pd.UserID, plan, err)
		return &subscription.CreateOrderRes{Code: 1, Message: "下单失败，请稍后再试"}, nil
	}
	payURL, err := g.CreateOrder(ctx, orderNo, p.PriceCents, fmt.Sprintf("GoAlgo %s 会员（%d 天）", tierName(plan), p.Days))
	if err != nil {
		// 下单失败：关单并返回错误（支付FM侧可能未配置完整）
		if _, cerr := s.subDal.MarkOrderClosed(ctx, order.ID); cerr != nil {
			log.Warnf("CreateOrder 关单失败 order=%s: %v", orderNo, cerr)
		}
		log.Warnf("CreateOrder 支付FM下单失败 user=%d plan=%s: %v", pd.UserID, plan, err)
		return &subscription.CreateOrderRes{Code: 1, Message: "下单失败：" + err.Error()}, nil
	}
	expireAt := time.Now().Add(15 * time.Minute).Unix()
	return &subscription.CreateOrderRes{
		Code:        0,
		Message:     "success",
		OrderNo:     orderNo,
		PayUrl:      payURL,
		AmountCents: p.PriceCents,
		ExpireAt:    expireAt,
	}, nil
}

// GetOrder 登录：查订单状态（前端回流轮询）
func (s *SubscriptionService) GetOrder(ctx context.Context, req *subscription.GetOrderReq) (*subscription.GetOrderRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &subscription.GetOrderRes{Code: 1, Message: "请先登录"}, nil
	}
	orderNo := strings.TrimSpace(req.GetOrderNo())
	if orderNo == "" {
		return &subscription.GetOrderRes{Code: 1, Message: "缺少订单号"}, nil
	}
	o, err := s.subDal.GetOrderByNo(ctx, orderNo)
	if err != nil {
		return &subscription.GetOrderRes{Code: 1, Message: "订单不存在"}, nil
	}
	// 本人或站管可见
	if o.UserID != pd.UserID && !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return &subscription.GetOrderRes{Code: 1, Message: "无权查看该订单"}, nil
	}
	var paidAt int64
	if o.PaidAt != nil {
		paidAt = o.PaidAt.Unix()
	}
	return &subscription.GetOrderRes{
		Code:    0,
		Message: "success",
		OrderNo: o.OrderNo,
		Status:  o.Status,
		PaidAt:  paidAt,
	}, nil
}

// MySubscription 登录：我的订阅状态（tier 空=未订阅；过期按未订阅）
func (s *SubscriptionService) MySubscription(ctx context.Context, _ *subscription.MySubscriptionReq) (*subscription.MySubscriptionRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &subscription.MySubscriptionRes{Code: 1, Message: "请先登录"}, nil
	}
	tier, expireAt, source := s.subDal.UserSubscription(ctx, int64(pd.UserID))
	res := &subscription.MySubscriptionRes{Code: 0, Message: "success", Tier: tier, Source: source}
	if tier != "" && expireAt != nil {
		res.ExpireAt = expireAt.Unix()
		daysLeft := int32(time.Until(*expireAt).Hours() / 24)
		if daysLeft < 0 {
			daysLeft = 0
		}
		res.DaysLeft = daysLeft
	}
	return res, nil
}

// NotifyHTTP 支付FM异步回调（原生 HTTP handler：GET query / POST form，不走 proto JSON）。
// 流程：验签（md5(state+商户号+订单号+金额+密钥)，state=1）→ 订单存在 → 金额相等 →
// 行锁置 paid（幂等）→ 赢家履约 → 回 success。
func (s *SubscriptionService) NotifyHTTP(w http.ResponseWriter, r *http.Request) {
	writeAck := func(success bool) {
		if success {
			_, _ = w.Write([]byte("success"))
			return
		}
		_, _ = w.Write([]byte("failure"))
	}
	if r == nil {
		writeAck(false)
		return
	}
	if err := r.ParseForm(); err != nil {
		log.Warnf("NotifyHTTP parse form: %v", err)
		writeAck(false)
		return
	}
	values := make(map[string]string, len(r.Form))
	for k, v := range r.Form {
		if len(v) > 0 {
			values[k] = v[0]
		}
	}
	g, err := s.paymentGateway(r.Context())
	if err != nil {
		log.Warnf("NotifyHTTP gateway: %v", err)
		writeAck(false)
		return
	}
	ntf, err := g.ParseNotification(values)
	if err != nil {
		log.Warnf("NotifyHTTP 验签失败: %v", err)
		writeAck(false)
		return
	}
	// 金额相等校验（订单金额 vs 回调金额；分）——必须在置 paid 之前，防篡改
	paidCents, perr := payment.ParseYuanStringToCents(ntf.Amount)
	if perr != nil {
		log.Warnf("NotifyHTTP 金额解析失败 order=%s amount=%s: %v", ntf.OrderNo, ntf.Amount, perr)
		writeAck(false)
		return
	}
	order, err := s.subDal.GetOrderByNo(r.Context(), ntf.OrderNo)
	if err != nil {
		log.Warnf("NotifyHTTP 订单不存在 order=%s: %v", ntf.OrderNo, err)
		writeAck(false)
		return
	}
	if paidCents != order.AmountCents {
		log.Errorf("NotifyHTTP 金额不符 order=%s expect=%d got=%s", order.OrderNo, order.AmountCents, ntf.Amount)
		writeAck(false)
		return
	}
	// 行锁置 paid（幂等）：claimed=false 表示已履约过的重复回调
	order, claimed, err := s.subDal.ClaimPaidOrder(r.Context(), ntf.OrderNo, ntf.PlatformOrderNo, ntf.PayTime)
	if err != nil {
		log.Warnf("NotifyHTTP 订单处理失败 order=%s: %v", ntf.OrderNo, err)
		writeAck(false)
		return
	}
	if !claimed {
		// 重复回调：已履约，直接成功
		writeAck(true)
		return
	}
	plan, perr := s.subDal.PlanByTier(r.Context(), order.Plan)
	if perr != nil || plan == nil || plan.Days < 1 {
		log.Errorf("NotifyHTTP 套餐缺失 order=%s plan=%s: %v", order.OrderNo, order.Plan, perr)
		writeAck(false)
		return
	}
	if err := s.subDal.FulfillPayFm(r.Context(), int64(order.UserID), order.Plan, plan.Days); err != nil {
		log.Errorf("NotifyHTTP 履约失败 order=%s user=%d: %v", order.OrderNo, order.UserID, err)
		writeAck(false)
		return
	}
	log.Infof("NotifyHTTP 履约成功 order=%s user=%d plan=%s amount=%d", order.OrderNo, order.UserID, order.Plan, order.AmountCents)
	writeAck(true)
}

// GetAiAnalyzeQuota 服务间：单用户 AI 分析月配额。
// 语义：组织开通 AI 分析（任一非公共域 active 组织 enable_ai_summary=true）→ 无限
// （组织成员优先消耗组织配额，不扣个人配额）；否则 Pro 订阅 active → 套餐 AiAnalyzeMonth；
// 否则 0（不能触发 AI 分析）。
func (s *SubscriptionService) GetAiAnalyzeQuota(ctx context.Context, req *subscription.GetAiAnalyzeQuotaReq) (*subscription.GetAiAnalyzeQuotaRes, error) {
	uid := req.GetUserId()
	if uid <= 0 {
		return &subscription.GetAiAnalyzeQuotaRes{QuotaPerMonth: 0}, nil
	}
	if s.orgAiAnalyzeUnlimited(ctx, uid) {
		return &subscription.GetAiAnalyzeQuotaRes{Unlimited: true}, nil
	}
	if tier, active := s.profileDal.SubscriptionTier(ctx, uid); active && tier == "pro" {
		if plan, err := s.profileDal.PlanByTier(ctx, tier); err == nil && plan != nil && plan.EnableAiAnalyze {
			return &subscription.GetAiAnalyzeQuotaRes{QuotaPerMonth: int32(plan.AiAnalyzeMonth)}, nil
		}
	}
	return &subscription.GetAiAnalyzeQuotaRes{QuotaPerMonth: 0}, nil
}

// MyAiStatus 登录：当前用户 AI 能力落地状态（会员页标记「实际是否有权限」用）。
// AI 分析来源独立标记组织开通；落地语义与 GetAiAnalyzeQuota 一致（组织开通=无限）。
func (s *SubscriptionService) MyAiStatus(ctx context.Context, _ *subscription.MyAiStatusReq) (*subscription.MyAiStatusRes, error) {
	uid := int64(auth.GetCurrentUserId(ctx))
	if uid <= 0 {
		return &subscription.MyAiStatusRes{Code: 1, Message: "请先登录"}, nil
	}
	res := &subscription.MyAiStatusRes{Code: 0, Message: "success"}

	// AI 分析：Pro active → 套餐值；组织开通单独标记（与 GetAiAnalyzeQuota 落地语义一致）
	proActive := false
	proQuota := 0
	proAiDaily := false
	if tier, active := s.profileDal.SubscriptionTier(ctx, uid); active && tier == "pro" {
		proActive = true
		if plan, err := s.profileDal.PlanByTier(ctx, tier); err == nil && plan != nil {
			if plan.EnableAiAnalyze {
				proQuota = plan.AiAnalyzeMonth
			}
			proAiDaily = plan.EnableAiDaily
		}
	}
	source, unlimited, quota := resolveAiAnalyzeStatus(proActive, proQuota, s.orgAiAnalyzeUnlimited(ctx, uid))
	res.AiAnalyzeSource = source
	res.AiAnalyzeUnlimited = unlimited
	res.AiAnalyzeQuota = int32(quota)

	// AI 日报：组织授权独立标记；生效 = Pro + 套餐开启 + 个人开关（与定时任务分流一致）
	res.AiDailyOrgAllowed = s.profileDal.UserHasOrgDailyEmailGrant(ctx, uid)
	res.AiDailyEnabled = proActive && proAiDaily && s.profileDal.AIDailyEnabled(ctx, uid)
	return res, nil
}

// resolveAiAnalyzeStatus AI 分析落地状态（纯函数，便于测试）：
// 组织开通 → 无限（unlimited=true；同时 Pro 订阅标 pro_org，否则 org）；
// 无组织开通 → Pro 订阅标 pro 并返回套餐配额，否则 none/0。
func resolveAiAnalyzeStatus(proActive bool, proQuota int, orgUnlimited bool) (source string, unlimited bool, quota int) {
	switch {
	case proActive && orgUnlimited:
		return "pro_org", true, proQuota
	case proActive:
		return "pro", false, proQuota
	case orgUnlimited:
		return "org", true, 0
	default:
		return "none", false, 0
	}
}

// orgAiAnalyzeUnlimited 用户所属非公共域 active 组织是否开通 AI 分析：
// 任一组织 enable_ai_summary=true → 组织成员 AI 分析无限（组织优先，个人配额不参与）。
func (s *SubscriptionService) orgAiAnalyzeUnlimited(ctx context.Context, userID int64) bool {
	var n int64
	err := s.data.DB.WithContext(ctx).
		Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("m.user_id = ? AND o.slug <> ? AND COALESCE(o.is_system, false) = false AND o.status = ? AND o.enable_ai_summary = ?",
			userID, model.PublicOrgSlug, model.OrgStatusActive, true).
		Count(&n).Error
	return err == nil && n > 0
}

// GrantSubscription 站管：人工赋予/更新订阅（重复调用叠加）
func (s *SubscriptionService) GrantSubscription(ctx context.Context, req *subscription.GrantSubscriptionReq) (*subscription.GrantSubscriptionRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "需要用户同步运维权限"}, nil
	}
	tier := strings.TrimSpace(req.GetTier())
	if tier != "plus" && tier != "pro" {
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "档位须为 plus 或 pro"}, nil
	}
	days := int(req.GetDays())
	if days < 1 || days > 365 {
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "天数须在 1–365"}, nil
	}
	uid := req.GetUserId()
	if uid <= 0 {
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "用户ID无效"}, nil
	}
	if _, err := s.profileDal.GetById(ctx, uid); err != nil {
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "用户不存在"}, nil
	}
	if err := s.subDal.Grant(ctx, uid, tier, days); err != nil {
		log.Errorf("GrantSubscription user=%d: %v", uid, err)
		return &subscription.GrantSubscriptionRes{Code: 1, Message: "赋予失败，请稍后再试"}, nil
	}
	return &subscription.GrantSubscriptionRes{Code: 0, Message: fmt.Sprintf("已赋予 %s 会员 %d 天", tierName(tier), days)}, nil
}

// RevokeSubscription 站管：取消订阅（立即回落免费；保留 AI 日报偏好）
func (s *SubscriptionService) RevokeSubscription(ctx context.Context, req *subscription.RevokeSubscriptionReq) (*subscription.RevokeSubscriptionRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return &subscription.RevokeSubscriptionRes{Code: 1, Message: "需要用户同步运维权限"}, nil
	}
	uid := req.GetUserId()
	if uid <= 0 {
		return &subscription.RevokeSubscriptionRes{Code: 1, Message: "用户ID无效"}, nil
	}
	if err := s.subDal.Revoke(ctx, uid); err != nil {
		log.Errorf("RevokeSubscription user=%d: %v", uid, err)
		return &subscription.RevokeSubscriptionRes{Code: 1, Message: "取消失败，请稍后再试"}, nil
	}
	return &subscription.RevokeSubscriptionRes{Code: 0, Message: "已取消订阅"}, nil
}

// ListSubscriptions 站管：订阅用户列表（keyword 模糊 username/name）
func (s *SubscriptionService) ListSubscriptions(ctx context.Context, req *subscription.ListSubscriptionsReq) (*subscription.ListSubscriptionsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return &subscription.ListSubscriptionsRes{Code: 1, Message: "需要用户同步运维权限"}, nil
	}
	users, total, err := s.subDal.ListSubscriptions(ctx, req.GetPage(), req.GetPageSize(), req.GetKeyword())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &subscription.ListSubscriptionsRes{Code: 0, Message: "success", List: make([]*subscription.SubUser, 0, len(users)), Total: total}
	for _, u := range users {
		var expireAt int64
		if u.SubExpireAt != nil {
			expireAt = u.SubExpireAt.Unix()
		}
		res.List = append(res.List, &subscription.SubUser{
			UserId:   int64(u.ID),
			Username: u.Username,
			Name:     u.Name,
			Tier:     u.SubTier,
			ExpireAt: expireAt,
			Source:   u.SubSource,
		})
	}
	return res, nil
}

// UpdatePlans 站管：更新套餐配额模板（校验后逐档 upsert + 失效缓存）
func (s *SubscriptionService) UpdatePlans(ctx context.Context, req *subscription.UpdatePlansReq) (*subscription.UpdatePlansRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return &subscription.UpdatePlansRes{Code: 1, Message: "需要用户同步运维权限"}, nil
	}
	if len(req.GetPlans()) == 0 {
		return &subscription.UpdatePlansRes{Code: 1, Message: "请至少提交一个套餐"}, nil
	}
	for _, p := range req.GetPlans() {
		plan := strings.TrimSpace(p.GetPlan())
		if plan == "" {
			return &subscription.UpdatePlansRes{Code: 1, Message: "套餐档位不能为空"}, nil
		}
		if plan == "free" && p.GetPriceCents() != 0 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "免费档价格必须为 0"}, nil
		}
		if p.GetPriceCents() < 0 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "价格不能为负"}, nil
		}
		if p.GetManualRefreshDaily() < 0 || p.GetManualRefreshDaily() > 100 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "手动刷新次数须在 0–100"}, nil
		}
		if p.GetSyncIntervalMin() < 5 || p.GetSyncIntervalMin() > 10080 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "同步间隔须在 5–10080 分钟"}, nil
		}
		if p.GetAiAnalyzeMonth() < 0 || p.GetAiAnalyzeMonth() > 10000 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "AI 分析次数须在 0–10000"}, nil
		}
		if p.GetDays() < 1 || p.GetDays() > 365 {
			return &subscription.UpdatePlansRes{Code: 1, Message: "购买天数须在 1–365"}, nil
		}
		// 部分档位可不提交：仅更新提交的档位，其余不动
	}
	for _, p := range req.GetPlans() {
		np := model.SubscriptionPlan{
			Plan:               strings.TrimSpace(p.GetPlan()),
			PriceCents:         p.GetPriceCents(),
			ManualRefreshDaily: int(p.GetManualRefreshDaily()),
			SyncIntervalMin:    int(p.GetSyncIntervalMin()),
			AiAnalyzeMonth:     int(p.GetAiAnalyzeMonth()),
			EnableFetchProblem: p.GetEnableFetchProblem(),
			EnableAiAnalyze:    p.GetEnableAiAnalyze(),
			EnableAiDaily:      p.GetEnableAiDaily(),
			EnableRegularDaily: p.GetEnableRegularDaily(),
			Days:               int(p.GetDays()),
			Enabled:            p.GetEnabled(),
		}
		if err := s.subDal.UpsertPlan(ctx, np); err != nil {
			log.Errorf("UpdatePlans upsert %s: %v", np.Plan, err)
			return &subscription.UpdatePlansRes{Code: 1, Message: "保存失败，请稍后再试"}, nil
		}
		s.profileDal.InvalidatePlanCache(ctx, np.Plan)
	}
	return &subscription.UpdatePlansRes{Code: 0, Message: "套餐配置已保存"}, nil
}

func tierName(tier string) string {
	switch tier {
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	default:
		return tier
	}
}
