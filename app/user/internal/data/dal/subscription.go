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

// SubscriptionDal C 端订阅：套餐 / 订单 / 订阅状态
type SubscriptionDal struct {
	db *gorm.DB
}

// NewSubscriptionDal 创建订阅 dal
func NewSubscriptionDal(data *data.Data) *SubscriptionDal {
	return &SubscriptionDal{db: data.DB}
}

// ListPlans 套餐列表（按 enabled 过滤后的全部档位，站管可见全量）
func (d *SubscriptionDal) ListPlans(ctx context.Context, onlyEnabled bool) ([]model.SubscriptionPlan, error) {
	var plans []model.SubscriptionPlan
	q := d.db.WithContext(ctx).Order("id ASC")
	if onlyEnabled {
		q = q.Where("enabled = ?", true)
	}
	err := q.Find(&plans).Error
	return plans, err
}

// PlanByTier 按档位查套餐（未启用也返回；调用方自行判断 enabled）
func (d *SubscriptionDal) PlanByTier(ctx context.Context, tier string) (*model.SubscriptionPlan, error) {
	var p model.SubscriptionPlan
	err := d.db.WithContext(ctx).Where("plan = ?", strings.TrimSpace(tier)).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("套餐档不存在: %s", tier)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertPlan 逐档 upsert 套餐模板（plan 唯一）。
// 用 map 创建：带 default 标签的字段传零值（0/false）时，GORM 对 struct 会做「零值→默认值」
// 替换并从 INSERT 剔除（如 manual_refresh_daily=0 会变回 2、enabled=false 无法下架），
// map 路径不做该处理，可保证零值真实写入。
func (d *SubscriptionDal) UpsertPlan(ctx context.Context, p model.SubscriptionPlan) error {
	cols := []string{
		"plan", "price_cents", "manual_refresh_daily", "sync_interval_min",
		"ai_analyze_month", "enable_fetch_problem", "enable_ai_analyze",
		"enable_ai_daily", "enable_regular_daily", "days", "enabled",
	}
	now := time.Now()
	values := map[string]interface{}{
		"plan":                 p.Plan,
		"price_cents":          p.PriceCents,
		"manual_refresh_daily": p.ManualRefreshDaily,
		"sync_interval_min":    p.SyncIntervalMin,
		"ai_analyze_month":     p.AiAnalyzeMonth,
		"enable_fetch_problem": p.EnableFetchProblem,
		"enable_ai_analyze":    p.EnableAiAnalyze,
		"enable_ai_daily":      p.EnableAiDaily,
		"enable_regular_daily": p.EnableRegularDaily,
		"days":                 p.Days,
		"enabled":              p.Enabled,
		"created_at":           now,
		"updated_at":           now,
	}
	return d.db.WithContext(ctx).Model(&model.SubscriptionPlan{}).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "plan"}},
			DoUpdates: append(clause.AssignmentColumns(cols),
				clause.Assignment{Column: clause.Column{Name: "updated_at"}, Value: now}),
		}).Create(values).Error
}

// CreateOrder 创建待支付订单（order_no 唯一；重复调用返回已存在订单或错误）
func (d *SubscriptionDal) CreateOrder(ctx context.Context, orderNo string, userID uint, plan string, amountCents int64) (*model.PaymentOrder, error) {
	o := model.PaymentOrder{
		OrderNo:     orderNo,
		UserID:      userID,
		Plan:        plan,
		AmountCents: amountCents,
		Status:      model.OrderStatusPending,
	}
	err := d.db.WithContext(ctx).Create(&o).Error
	if err != nil {
		// 幂等：并发重复下单时返回已存在订单
		var dup model.PaymentOrder
		if e2 := d.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&dup).Error; e2 == nil {
			return &dup, nil
		}
		return nil, err
	}
	return &o, nil
}

// GetOrderByNo 按订单号查订单（order_no 唯一）
func (d *SubscriptionDal) GetOrderByNo(ctx context.Context, orderNo string) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
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
func (d *SubscriptionDal) MarkOrderClosed(ctx context.Context, id uint) (bool, error) {
	res := d.db.WithContext(ctx).Model(&model.PaymentOrder{}).
		Where("id = ? AND status = ?", id, model.OrderStatusPending).
		Update("status", model.OrderStatusClosed)
	return res.RowsAffected > 0, res.Error
}

// ClaimPaidOrder 回调履约行锁：仅 pending/closed 可置 paid（FOR UPDATE）。
// 返回 (order, claimed, err)：claimed=true 表示本次调用赢得履约权（从非 paid 置为 paid），
// false 表示订单已 paid（重复回调）或不存在，调用方不应重复履约。
func (d *SubscriptionDal) ClaimPaidOrder(ctx context.Context, orderNo, platformOrderNo string, paidAt time.Time) (*model.PaymentOrder, bool, error) {
	var o model.PaymentOrder
	claimed := false
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", orderNo).
			First(&o).Error; err != nil {
			return err
		}
		if o.Status == model.OrderStatusPaid {
			return nil // 已履约
		}
		o.Status = model.OrderStatusPaid
		o.PlatformOrderNo = platformOrderNo
		o.PaidAt = &paidAt
		if err := tx.Save(&o).Error; err != nil {
			return err
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

// UserSubscription 用户当前订阅状态（未订阅/过期返回空）
func (d *SubscriptionDal) UserSubscription(ctx context.Context, userID int64) (tier string, expireAt *time.Time, source string) {
	var u model.User
	if err := d.db.WithContext(ctx).
		Select("sub_tier, sub_expire_at, sub_source").
		Where("id = ?", userID).
		First(&u).Error; err != nil {
		return "", nil, ""
	}
	tier = strings.TrimSpace(u.SubTier)
	if tier == "" {
		return "", nil, ""
	}
	if u.SubExpireAt != nil && !u.SubExpireAt.After(time.Now()) {
		return "", nil, ""
	}
	return tier, u.SubExpireAt, u.SubSource
}

// Grant 人工赋予/更新订阅：expire = max(now, 当前 expireAt) + days（重复调用叠加）
// 事务内完成，source 固定 manager。
func (d *SubscriptionDal) Grant(ctx context.Context, userID int64, tier string, days int) error {
	if days < 1 || days > 365 {
		return fmt.Errorf("天数须在 1–365")
	}
	now := time.Now()
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", userID).
			First(&u).Error; err != nil {
			return err
		}
		base := now
		if u.SubExpireAt != nil && u.SubExpireAt.After(now) {
			base = *u.SubExpireAt
		}
		expire := base.AddDate(0, 0, days)
		return tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
			"sub_tier":      tier,
			"sub_expire_at": expire,
			"sub_source":    "manager",
		}).Error
	})
}

// Revoke 取消订阅：清空 tier/expire/source（保留 ai_daily_enabled 用户偏好）
func (d *SubscriptionDal) Revoke(ctx context.Context, userID int64) error {
	return d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"sub_tier":      "",
		"sub_expire_at": nil,
		"sub_source":    "",
	}).Error
}

// FulfillPayFm 支付FM支付履约：users.sub_tier / sub_expire_at = max(now, 当前) + plan.Days / sub_source='payfm'。
// 在订单已 paid 的前提下调用（同事务或紧随其后）。
func (d *SubscriptionDal) FulfillPayFm(ctx context.Context, userID int64, tier string, days int) error {
	if days < 1 {
		return fmt.Errorf("套餐天数非法: %d", days)
	}
	now := time.Now()
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", userID).
			First(&u).Error; err != nil {
			return err
		}
		base := now
		if u.SubExpireAt != nil && u.SubExpireAt.After(now) {
			base = *u.SubExpireAt
		}
		expire := base.AddDate(0, 0, days)
		return tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
			"sub_tier":      tier,
			"sub_expire_at": expire,
			"sub_source":    "payfm",
		}).Error
	})
}

// ListSubscriptions 订阅用户列表（含已过期；keyword 模糊 username/name，服务端过滤与 total 一致）
func (d *SubscriptionDal) ListSubscriptions(ctx context.Context, page, pageSize int64, keyword string) ([]model.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	q := d.db.WithContext(ctx).Model(&model.User{}).
		Where("sub_tier <> ''")
	if kw := sqllike.Pattern(keyword); kw != "" {
		q = q.Where("(username ILIKE ? OR name ILIKE ?)", kw, kw)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var users []model.User
	err := q.Select("id, username, name, sub_tier, sub_expire_at, sub_source").
		Order("sub_expire_at DESC NULLS LAST, id DESC").
		Offset(int((page - 1) * pageSize)).Limit(int(pageSize)).
		Find(&users).Error
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// CloseStalePendingOrders 关单：pending 超过 5 分钟置 closed（定时任务调用）。
// 返回关闭数量。
func (d *SubscriptionDal) CloseStalePendingOrders(ctx context.Context, olderThan time.Duration) (int64, error) {
	res := d.db.WithContext(ctx).Model(&model.PaymentOrder{}).
		Where("status = ? AND created_at < ?", model.OrderStatusPending, time.Now().Add(-olderThan)).
		Update("status", model.OrderStatusClosed)
	return res.RowsAffected, res.Error
}
