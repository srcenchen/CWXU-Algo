package dal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/sqllike"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SponsorDal 打赏赞助：设置 / 订单 / 开支
type SponsorDal struct {
	db *gorm.DB
}

// NewSponsorDal 创建打赏 dal
func NewSponsorDal(data *data.Data) *SponsorDal {
	return &SponsorDal{db: data.DB}
}

// MonthlyAgg 月度聚合（打赏页/管理页月度收支用）
type MonthlyAgg struct {
	Month        string
	IncomeCents  int64
	ExpenseCents int64
	NetCents     int64
}

// GetSettings 读取打赏页设置（不存在则建默认行）
func (d *SponsorDal) GetSettings(ctx context.Context) (*model.SponsorSetting, error) {
	var s model.SponsorSetting
	err := d.db.WithContext(ctx).First(&s, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s = model.SponsorSetting{
			ID:                       1,
			MembershipSponsorEnabled: true,
			DonationEnabled:          true,
			IntroMarkdown:            model.DefaultSponsorIntro,
		}
		if e := d.db.WithContext(ctx).Create(&s).Error; e != nil {
			return nil, e
		}
		return &s, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// UpdateSettings 覆盖写入打赏页设置（确保行存在）
func (d *SponsorDal) UpdateSettings(ctx context.Context, membership, donation bool, intro string) error {
	if _, err := d.GetSettings(ctx); err != nil {
		return err
	}
	return d.db.WithContext(ctx).Model(&model.SponsorSetting{}).Where("id = 1").
		Updates(map[string]interface{}{
			"membership_sponsor_enabled": membership,
			"donation_enabled":           donation,
			"intro_markdown":             intro,
			"updated_at":                 time.Now(),
		}).Error
}

// CreateOrder 创建待支付打赏订单（order_no 唯一）；giftTier 非空表示支付后回赠会员
func (d *SponsorDal) CreateOrder(ctx context.Context, orderNo string, userID uint, nickname string, amountCents int64, message, giftTier string) (*model.SponsorOrder, error) {
	o := model.SponsorOrder{
		OrderNo:     orderNo,
		UserID:      userID,
		Nickname:    nickname,
		AmountCents: amountCents,
		Message:     message,
		Status:      model.OrderStatusPending,
		GiftTier:    giftTier,
	}
	if err := d.db.WithContext(ctx).Create(&o).Error; err != nil {
		var dup model.SponsorOrder
		if e2 := d.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&dup).Error; e2 == nil {
			return &dup, nil
		}
		return nil, err
	}
	return &o, nil
}

// GetOrderByNo 按订单号查打赏订单
func (d *SponsorDal) GetOrderByNo(ctx context.Context, orderNo string) (*model.SponsorOrder, error) {
	var o model.SponsorOrder
	err := d.db.WithContext(ctx).Where("order_no = ?", strings.TrimSpace(orderNo)).First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("订单不存在")
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// MarkOrderClosed 关闭订单（仅 pending 可关；幂等）
func (d *SponsorDal) MarkOrderClosed(ctx context.Context, id uint) (bool, error) {
	res := d.db.WithContext(ctx).Model(&model.SponsorOrder{}).
		Where("id = ? AND status = ?", id, model.OrderStatusPending).
		Update("status", model.OrderStatusClosed)
	return res.RowsAffected > 0, res.Error
}

// ClaimPaidOrder 支付回调入账：订单置 paid（行锁，幂等）；有回赠档位时同事务发放会员。
// claimed=true 表示本次调用赢得入账权；false 表示已 paid（重复回调）。
func (d *SponsorDal) ClaimPaidOrder(ctx context.Context, orderNo, platformOrderNo string, paidAt time.Time) (*model.SponsorOrder, bool, error) {
	var o model.SponsorOrder
	claimed := false
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", orderNo).First(&o).Error; err != nil {
			return err
		}
		if o.Status == model.OrderStatusPaid {
			return nil
		}
		o.Status = model.OrderStatusPaid
		o.PlatformOrderNo = platformOrderNo
		o.PaidAt = &paidAt
		if err := tx.Save(&o).Error; err != nil {
			return err
		}
		// 回赠会员：与入账同一事务，避免「已付款未发放」；来源记 payfm（本单为在线支付）
		if o.GiftTier != "" {
			if err := grantGiftInTx(tx, o.UserID, o.GiftTier, model.SponsorGiftDays, "payfm", time.Now()); err != nil {
				return err
			}
		}
		claimed = true
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("订单不存在")
		}
		return nil, false, err
	}
	return &o, claimed, nil
}

// grantGiftInTx 在既有事务内发放赞助回赠会员（与订阅支付履约同语义：晋升排队档 + 档位叠加）。
func grantGiftInTx(tx *gorm.DB, userID uint, tier string, days int, source string, now time.Time) error {
	if days < 1 {
		return fmt.Errorf("回赠天数非法: %d", days)
	}
	var u model.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).First(&u).Error; err != nil {
		return err
	}
	promoteInPlace(&u, now)
	applyPurchase(&u, tier, days, source, now)
	return tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"sub_tier":           u.SubTier,
		"sub_expire_at":      u.SubExpireAt,
		"sub_source":         u.SubSource,
		"sub_pending_tier":   u.SubPendingTier,
		"sub_pending_days":   u.SubPendingDays,
		"sub_pending_source": u.SubPendingSource,
		"sub_reminded":       u.SubReminded,
	}).Error
}

// CloseStalePendingOrders 关单：pending 超过 olderThan 置 closed（定时任务调用）
func (d *SponsorDal) CloseStalePendingOrders(ctx context.Context, olderThan time.Duration) (int64, error) {
	res := d.db.WithContext(ctx).Model(&model.SponsorOrder{}).
		Where("status = ? AND created_at < ?", model.OrderStatusPending, time.Now().Add(-olderThan)).
		Update("status", model.OrderStatusClosed)
	return res.RowsAffected, res.Error
}

// ListDonations 已支付打赏名单（时间倒序；keyword 模糊昵称/留言，服务端过滤与 total 一致）
func (d *SponsorDal) ListDonations(ctx context.Context, page, pageSize int64, keyword string) ([]model.SponsorOrder, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}
	q := d.db.WithContext(ctx).Model(&model.SponsorOrder{}).Where("status = ?", model.OrderStatusPaid)
	if kw := sqllike.Pattern(keyword); kw != "" {
		q = q.Where("(nickname ILIKE ? OR message ILIKE ?)", kw, kw)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.SponsorOrder
	err := q.Order("paid_at DESC NULLS LAST, id DESC").
		Offset(int((page - 1) * pageSize)).Limit(int(pageSize)).
		Find(&list).Error
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListExpenses 全部开支（发生时间倒序）
func (d *SponsorDal) ListExpenses(ctx context.Context) ([]model.SponsorExpense, error) {
	var list []model.SponsorExpense
	err := d.db.WithContext(ctx).Order("spent_at DESC, id DESC").Find(&list).Error
	return list, err
}

// RecordExpense 记录一笔开支/余额调整
func (d *SponsorDal) RecordExpense(ctx context.Context, kind string, amountCents int64, note string, spentAt time.Time, createdBy uint) error {
	return d.db.WithContext(ctx).Create(&model.SponsorExpense{
		AmountCents: amountCents,
		Note:        note,
		Kind:        kind,
		SpentAt:     spentAt,
		CreatedBy:   createdBy,
	}).Error
}

// Overview 资金概览：累计赞助/开支、人数、本月收支
func (d *SponsorDal) Overview(ctx context.Context) (totalIncome, totalExpense, donationCount, monthIncome, monthExpense int64, err error) {
	db := d.db.WithContext(ctx)
	if err = db.Model(&model.SponsorOrder{}).Where("status = ?", model.OrderStatusPaid).
		Select("COALESCE(SUM(amount_cents),0)").Scan(&totalIncome).Error; err != nil {
		return
	}
	if err = db.Model(&model.SponsorOrder{}).Where("status = ?", model.OrderStatusPaid).
		Count(&donationCount).Error; err != nil {
		return
	}
	if err = db.Model(&model.SponsorExpense{}).
		Select("COALESCE(SUM(amount_cents),0)").Scan(&totalExpense).Error; err != nil {
		return
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if err = db.Model(&model.SponsorOrder{}).
		Where("status = ? AND paid_at >= ?", model.OrderStatusPaid, start).
		Select("COALESCE(SUM(amount_cents),0)").Scan(&monthIncome).Error; err != nil {
		return
	}
	if err = db.Model(&model.SponsorExpense{}).Where("spent_at >= ?", start).
		Select("COALESCE(SUM(amount_cents),0)").Scan(&monthExpense).Error; err != nil {
		return
	}
	return
}

// MonthlyRows 近 count 个月收支（倒序，含当月）
func (d *SponsorDal) MonthlyRows(ctx context.Context, count int) ([]MonthlyAgg, error) {
	if count < 1 {
		count = 6
	}
	if count > 24 {
		count = 24
	}
	type agg struct {
		Month string
		Total int64
	}
	income := map[string]int64{}
	expense := map[string]int64{}
	var ia []agg
	if err := d.db.WithContext(ctx).Model(&model.SponsorOrder{}).
		Where("status = ? AND paid_at IS NOT NULL", model.OrderStatusPaid).
		Select("to_char(paid_at, 'YYYY-MM') AS month, COALESCE(SUM(amount_cents),0) AS total").
		Group("month").Scan(&ia).Error; err != nil {
		return nil, err
	}
	for _, r := range ia {
		income[r.Month] = r.Total
	}
	var ea []agg
	if err := d.db.WithContext(ctx).Model(&model.SponsorExpense{}).
		Select("to_char(spent_at, 'YYYY-MM') AS month, COALESCE(SUM(amount_cents),0) AS total").
		Group("month").Scan(&ea).Error; err != nil {
		return nil, err
	}
	for _, r := range ea {
		expense[r.Month] = r.Total
	}
	now := time.Now()
	cursor := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	out := make([]MonthlyAgg, 0, count)
	for i := 0; i < count; i++ {
		key := cursor.Format("2006-01")
		out = append(out, MonthlyAgg{
			Month:        key,
			IncomeCents:  income[key],
			ExpenseCents: expense[key],
			NetCents:     income[key] - expense[key],
		})
		cursor = cursor.AddDate(0, -1, 0)
	}
	return out, nil
}
