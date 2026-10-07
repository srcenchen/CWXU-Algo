package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cwxu-algo/api/user/v1/sponsor"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"
	"cwxu-algo/app/user/internal/external/payment"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
)

const (
	sponsorMinCents = 100     // 最少 ¥1
	sponsorMaxCents = 1000000 // 最多 ¥10000
	sponsorMsgMax   = 60      // 留言最长字数
	expenseNoteMax  = 200     // 说明最长字数
)

// SponsorService 打赏赞助：公开展示 + 支付FM下单 + 站管开支管理
type SponsorService struct {
	sponsor.UnimplementedSponsorServer
	data *data.Data
	dal  *dal.SponsorDal
}

// NewSponsorService 创建打赏服务
func NewSponsorService(d *data.Data, sponsorDal *dal.SponsorDal) *SponsorService {
	return &SponsorService{data: d, dal: sponsorDal}
}

// sponsorNotifyURL 打赏专用异步回调地址（与会员支付区分路由）
func sponsorNotifyURL() string {
	return strings.Replace(notifyURL(), "/payment/notify", "/payment/sponsor-notify", 1)
}

// sponsorDonateAmount 赞助金额是否在允许区间（¥1–¥10000）
func sponsorDonateAmount(amountCents int64) bool {
	return amountCents >= sponsorMinCents && amountCents <= sponsorMaxCents
}

// paymentGateway 从站点配置构建支付FM网关（回调指向打赏专用路由）
func (s *SponsorService) paymentGateway(ctx context.Context) (*payment.PayFmGateway, error) {
	rt := sitesettings.Load(ctx, s.data.RDB, s.data.DB)
	apiBase, merchantNo, secret, payType, configured := rt.PayFmConf()
	if !configured {
		return nil, fmt.Errorf("支付未配置（请在站点设置填写支付FM接口地址/商户号/接入密钥）")
	}
	return payment.NewPayFmGateway(apiBase, merchantNo, secret, payType, sponsorNotifyURL())
}

// GetSettings 公开：打赏页设置（入口开关 + 说明 Markdown）
func (s *SponsorService) GetSettings(ctx context.Context, _ *sponsor.GetSettingsReq) (*sponsor.GetSettingsRes, error) {
	st, err := s.dal.GetSettings(ctx)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &sponsor.GetSettingsRes{
		Code:                     0,
		Message:                  "success",
		MembershipSponsorEnabled: st.MembershipSponsorEnabled,
		DonationEnabled:          st.DonationEnabled,
		IntroMarkdown:            st.IntroMarkdown,
	}, nil
}

// UpdateSettings 站管：更新打赏页设置
func (s *SponsorService) UpdateSettings(ctx context.Context, req *sponsor.UpdateSettingsReq) (*sponsor.UpdateSettingsRes, error) {
	if !auth.VerifySiteAdmin(ctx) {
		return &sponsor.UpdateSettingsRes{Code: 1, Message: "需要站点管理员权限"}, nil
	}
	intro := strings.TrimSpace(req.GetIntroMarkdown())
	if len([]rune(intro)) > 2000 {
		return &sponsor.UpdateSettingsRes{Code: 1, Message: "说明过长（最多 2000 字）"}, nil
	}
	if err := s.dal.UpdateSettings(ctx, req.GetMembershipSponsorEnabled(), req.GetDonationEnabled(), intro); err != nil {
		log.Errorf("Sponsor UpdateSettings: %v", err)
		return &sponsor.UpdateSettingsRes{Code: 1, Message: "保存失败，请稍后再试"}, nil
	}
	return &sponsor.UpdateSettingsRes{Code: 0, Message: "已保存"}, nil
}

// Overview 公开：资金概览
func (s *SponsorService) Overview(ctx context.Context, _ *sponsor.OverviewReq) (*sponsor.OverviewRes, error) {
	totalIncome, totalExpense, count, monthIncome, monthExpense, err := s.dal.Overview(ctx)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &sponsor.OverviewRes{
		Code:              0,
		Message:           "success",
		BalanceCents:      totalIncome - totalExpense,
		TotalIncomeCents:  totalIncome,
		TotalExpenseCents: totalExpense,
		DonationCount:     count,
		MonthIncomeCents:  monthIncome,
		MonthExpenseCents: monthExpense,
		Loss:              monthExpense > monthIncome,
	}, nil
}

// ListDonations 公开：赞助名单（时间倒序；keyword 模糊昵称/留言）
func (s *SponsorService) ListDonations(ctx context.Context, req *sponsor.ListDonationsReq) (*sponsor.ListDonationsRes, error) {
	list, total, err := s.dal.ListDonations(ctx, req.GetPage(), req.GetPageSize(), req.GetKeyword())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &sponsor.ListDonationsRes{
		Code:    0,
		Message: "success",
		List:    make([]*sponsor.Donation, 0, len(list)),
		Total:   total,
	}
	for _, d := range list {
		var createdAt int64
		if d.PaidAt != nil {
			createdAt = d.PaidAt.Unix()
		}
		res.List = append(res.List, &sponsor.Donation{
			Id:          int64(d.ID),
			Nickname:    d.Nickname,
			AmountCents: d.AmountCents,
			Message:     d.Message,
			CreatedAt:   createdAt,
		})
	}
	return res, nil
}

// ListExpenses 公开：开支明细（时间倒序）
func (s *SponsorService) ListExpenses(ctx context.Context, _ *sponsor.ListExpensesReq) (*sponsor.ListExpensesRes, error) {
	list, err := s.dal.ListExpenses(ctx)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &sponsor.ListExpensesRes{Code: 0, Message: "success", List: make([]*sponsor.Expense, 0, len(list))}
	for _, e := range list {
		res.List = append(res.List, &sponsor.Expense{
			Id:          int64(e.ID),
			AmountCents: e.AmountCents,
			Note:        e.Note,
			Kind:        e.Kind,
			SpentAt:     e.SpentAt.Unix(),
		})
	}
	return res, nil
}

// Monthly 公开：近 N 个月收支
func (s *SponsorService) Monthly(ctx context.Context, req *sponsor.MonthlyReq) (*sponsor.MonthlyRes, error) {
	rows, err := s.dal.MonthlyRows(ctx, int(req.GetCount()))
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &sponsor.MonthlyRes{Code: 0, Message: "success", List: make([]*sponsor.MonthlyRow, 0, len(rows))}
	for _, r := range rows {
		res.List = append(res.List, &sponsor.MonthlyRow{
			Month:        r.Month,
			IncomeCents:  r.IncomeCents,
			ExpenseCents: r.ExpenseCents,
			NetCents:     r.NetCents,
		})
	}
	return res, nil
}

// Donate 登录：打赏下单（支付FM）
func (s *SponsorService) Donate(ctx context.Context, req *sponsor.DonateReq) (*sponsor.DonateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &sponsor.DonateRes{Code: 1, Message: "请先登录"}, nil
	}
	amount := req.GetAmountCents()
	if !sponsorDonateAmount(amount) {
		return &sponsor.DonateRes{Code: 1, Message: "赞助金额需在 ¥1–¥10000"}, nil
	}
	msg := strings.TrimSpace(req.GetMessage())
	if len([]rune(msg)) > sponsorMsgMax {
		return &sponsor.DonateRes{Code: 1, Message: fmt.Sprintf("留言最多 %d 字", sponsorMsgMax)}, nil
	}
	g, err := s.paymentGateway(ctx)
	if err != nil {
		return &sponsor.DonateRes{Code: 1, Message: err.Error()}, nil
	}
	nickname := strings.TrimSpace(pd.Name)
	if nickname == "" {
		nickname = strings.TrimSpace(pd.Username)
	}
	if nickname == "" {
		nickname = "匿名"
	}
	orderNo := fmt.Sprintf("D%d", time.Now().UnixNano())
	order, err := s.dal.CreateOrder(ctx, orderNo, pd.UserID, nickname, amount, msg)
	if err != nil {
		log.Errorf("Sponsor Donate 建单 user=%d amount=%d: %v", pd.UserID, amount, err)
		return &sponsor.DonateRes{Code: 1, Message: "下单失败，请稍后再试"}, nil
	}
	payURL, err := g.CreateOrder(ctx, orderNo, amount, "GoAlgo 赞助支持")
	if err != nil {
		if _, cerr := s.dal.MarkOrderClosed(ctx, order.ID); cerr != nil {
			log.Warnf("Sponsor Donate 关单失败 order=%s: %v", orderNo, cerr)
		}
		log.Warnf("Sponsor Donate 支付FM下单失败 user=%d: %v", pd.UserID, err)
		return &sponsor.DonateRes{Code: 1, Message: "下单失败：" + err.Error()}, nil
	}
	return &sponsor.DonateRes{
		Code:        0,
		Message:     "success",
		OrderNo:     orderNo,
		PayUrl:      payURL,
		AmountCents: amount,
		ExpireAt:    time.Now().Add(15 * time.Minute).Unix(),
	}, nil
}

// DonationStatus 登录：查打赏订单状态（本人或站管）
func (s *SponsorService) DonationStatus(ctx context.Context, req *sponsor.DonationStatusReq) (*sponsor.DonationStatusRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &sponsor.DonationStatusRes{Code: 1, Message: "请先登录"}, nil
	}
	orderNo := strings.TrimSpace(req.GetOrderNo())
	if orderNo == "" {
		return &sponsor.DonationStatusRes{Code: 1, Message: "缺少订单号"}, nil
	}
	o, err := s.dal.GetOrderByNo(ctx, orderNo)
	if err != nil {
		return &sponsor.DonationStatusRes{Code: 1, Message: "订单不存在"}, nil
	}
	if o.UserID != pd.UserID && !auth.VerifySiteAdmin(ctx) {
		return &sponsor.DonationStatusRes{Code: 1, Message: "无权查看该订单"}, nil
	}
	var paidAt int64
	if o.PaidAt != nil {
		paidAt = o.PaidAt.Unix()
	}
	return &sponsor.DonationStatusRes{
		Code:    0,
		Message: "success",
		OrderNo: o.OrderNo,
		Status:  o.Status,
		PaidAt:  paidAt,
	}, nil
}

// RecordExpense 站管：记录开支
func (s *SponsorService) RecordExpense(ctx context.Context, req *sponsor.RecordExpenseReq) (*sponsor.RecordExpenseRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.VerifySiteAdmin(ctx) {
		return &sponsor.RecordExpenseRes{Code: 1, Message: "需要站点管理员权限"}, nil
	}
	amount := req.GetAmountCents()
	if amount <= 0 {
		return &sponsor.RecordExpenseRes{Code: 1, Message: "金额需大于 0"}, nil
	}
	note := strings.TrimSpace(req.GetNote())
	if note == "" {
		return &sponsor.RecordExpenseRes{Code: 1, Message: "请填写开支说明"}, nil
	}
	if len([]rune(note)) > expenseNoteMax {
		return &sponsor.RecordExpenseRes{Code: 1, Message: "说明过长"}, nil
	}
	spentAt := time.Now()
	if req.GetSpentAt() > 0 {
		spentAt = time.Unix(req.GetSpentAt(), 0)
	}
	var by uint
	if pd != nil {
		by = pd.UserID
	}
	if err := s.dal.RecordExpense(ctx, "expense", amount, note, spentAt, by); err != nil {
		log.Errorf("Sponsor RecordExpense: %v", err)
		return &sponsor.RecordExpenseRes{Code: 1, Message: "保存失败，请稍后再试"}, nil
	}
	return &sponsor.RecordExpenseRes{Code: 0, Message: "已记录开支"}, nil
}

// AdjustBalance 站管：扣减可用金额
func (s *SponsorService) AdjustBalance(ctx context.Context, req *sponsor.AdjustBalanceReq) (*sponsor.AdjustBalanceRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.VerifySiteAdmin(ctx) {
		return &sponsor.AdjustBalanceRes{Code: 1, Message: "需要站点管理员权限"}, nil
	}
	amount := req.GetAmountCents()
	if amount <= 0 {
		return &sponsor.AdjustBalanceRes{Code: 1, Message: "金额需大于 0"}, nil
	}
	note := strings.TrimSpace(req.GetNote())
	if note == "" {
		return &sponsor.AdjustBalanceRes{Code: 1, Message: "请填写扣减说明"}, nil
	}
	if len([]rune(note)) > expenseNoteMax {
		return &sponsor.AdjustBalanceRes{Code: 1, Message: "说明过长"}, nil
	}
	var by uint
	if pd != nil {
		by = pd.UserID
	}
	if err := s.dal.RecordExpense(ctx, "adjust", amount, note, time.Now(), by); err != nil {
		log.Errorf("Sponsor AdjustBalance: %v", err)
		return &sponsor.AdjustBalanceRes{Code: 1, Message: "保存失败，请稍后再试"}, nil
	}
	return &sponsor.AdjustBalanceRes{Code: 0, Message: "已扣减可用金额"}, nil
}

// SponsorNotifyHTTP 打赏支付异步回调（原生 HTTP handler）
func (s *SponsorService) SponsorNotifyHTTP(w http.ResponseWriter, r *http.Request) {
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
		log.Warnf("SponsorNotifyHTTP parse form: %v", err)
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
		log.Warnf("SponsorNotifyHTTP gateway: %v", err)
		writeAck(false)
		return
	}
	ntf, err := g.ParseNotification(values)
	if err != nil {
		log.Warnf("SponsorNotifyHTTP 验签失败: %v", err)
		writeAck(false)
		return
	}
	paidCents, perr := payment.ParseYuanStringToCents(ntf.Amount)
	if perr != nil {
		log.Warnf("SponsorNotifyHTTP 金额解析失败 order=%s amount=%s: %v", ntf.OrderNo, ntf.Amount, perr)
		writeAck(false)
		return
	}
	order, err := s.dal.GetOrderByNo(r.Context(), ntf.OrderNo)
	if err != nil {
		log.Warnf("SponsorNotifyHTTP 订单不存在 order=%s: %v", ntf.OrderNo, err)
		writeAck(false)
		return
	}
	if paidCents != order.AmountCents {
		log.Errorf("SponsorNotifyHTTP 金额不符 order=%s expect=%d got=%s", order.OrderNo, order.AmountCents, ntf.Amount)
		writeAck(false)
		return
	}
	paidAt := ntf.PayTime
	if paidAt.IsZero() {
		paidAt = time.Now()
	}
	if _, _, err := s.dal.ClaimPaidOrder(r.Context(), ntf.OrderNo, ntf.PlatformOrderNo, paidAt); err != nil {
		log.Errorf("SponsorNotifyHTTP 入账失败 order=%s: %v", ntf.OrderNo, err)
		writeAck(false)
		return
	}
	writeAck(true)
}
