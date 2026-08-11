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
func (d *SubscriptionDal) CreateOrder(ctx context.Context, orderNo string, userID uint, plan string, months int, amountCents int64) (*model.PaymentOrder, error) {
	if months < 1 {
		months = 1
	}
	o := model.PaymentOrder{
		OrderNo:     orderNo,
		UserID:      userID,
		Plan:        plan,
		Months:      months,
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

// subActive 当前档是否有效（nil 到期=长期视为有效）。
func subActive(u *model.User, now time.Time) bool {
	return u.SubTier != "" && (u.SubExpireAt == nil || u.SubExpireAt.After(now))
}

// ceilDays 向上取整剩余天数（暂停 Plus 计时用，保证用户不吃亏）。
func ceilDays(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	days := int(d / (24 * time.Hour))
	if d%(24*time.Hour) != 0 {
		days++
	}
	return days
}

// promoteInPlace 懒晋升：当前档已过期且有排队档 → 晋升（清空 pending，到期提醒重置）。
// 返回是否发生了晋升。
func promoteInPlace(u *model.User, now time.Time) bool {
	if u.SubTier == "" || u.SubPendingTier == "" {
		return false
	}
	if u.SubExpireAt != nil && u.SubExpireAt.After(now) {
		return false
	}
	days := u.SubPendingDays
	if days < 1 {
		days = 1
	}
	expire := now.AddDate(0, 0, days)
	u.SubTier = u.SubPendingTier
	u.SubExpireAt = &expire
	u.SubSource = u.SubPendingSource
	u.SubPendingTier = ""
	u.SubPendingDays = 0
	u.SubPendingSource = ""
	u.SubReminded = 0
	return true
}

// applyPurchase 购买/赋予档位后的档位叠加（纯函数，便于单测）：
//   - 无有效订阅/已过期 → 直接开通，清空排队档
//   - 有效同档续费 → expire = max(now, 当前到期) + days
//   - Pro 有效买 Plus → Plus 全额时长排队（pending_days += days），Pro 到期后生效
//   - Plus 有效买 Pro（升级）→ Pro 立即生效；Plus 剩余天数暂停，Pro 到期后自动续上
//
// 任何变化后重置到期提醒标记（sub_reminded=0），让新到期日重新走提醒。
func applyPurchase(u *model.User, tier string, days int, source string, now time.Time) {
	if days < 1 {
		return
	}
	prevSource := u.SubSource
	active := subActive(u, now)
	base := now
	if u.SubExpireAt != nil && u.SubExpireAt.After(now) {
		base = *u.SubExpireAt
	}
	switch {
	case !active:
		expire := now.AddDate(0, 0, days)
		u.SubTier = tier
		u.SubExpireAt = &expire
		u.SubSource = source
		u.SubPendingTier = ""
		u.SubPendingDays = 0
		u.SubPendingSource = ""
	case u.SubTier == tier:
		expire := base.AddDate(0, 0, days)
		u.SubExpireAt = &expire
	case u.SubTier == "pro" && tier == "plus":
		u.SubPendingTier = "plus"
		u.SubPendingDays += days
		if u.SubPendingSource == "" {
			u.SubPendingSource = source
		}
	case u.SubTier == "plus" && tier == "pro":
		remaining := 0
		if u.SubExpireAt != nil && u.SubExpireAt.After(now) {
			remaining = ceilDays(u.SubExpireAt.Sub(now))
		}
		expire := now.AddDate(0, 0, days)
		u.SubTier = "pro"
		u.SubExpireAt = &expire
		u.SubSource = source
		if u.SubPendingTier == "plus" {
			u.SubPendingDays += remaining
		} else {
			u.SubPendingTier = "plus"
			u.SubPendingDays = remaining
			u.SubPendingSource = prevSource
		}
	default:
		// 防御：理论不可达的组合（pending 只可能是 plus），直接按同档/新开处理
		expire := base.AddDate(0, 0, days)
		u.SubExpireAt = &expire
		u.SubTier = tier
		u.SubSource = source
	}
	u.SubReminded = 0
}

// ClaimAndFulfillPaidOrder 支付回调履约：订单置 paid + 用户订阅叠加在同一事务内（FOR UPDATE）。
// 返回 (order, claimed, err)：claimed=true 表示本次调用赢得履约权（从非 paid 置为 paid 并同步履约），
// false 表示订单已 paid（重复回调，已履约过），调用方不应再履约。
// 关键：履约与置 paid 原子，任一步失败整事务回滚，回调重试会从 pending 完整重做，
// 避免「订单已 paid 但用户权益未发放」的资金损失（旧实现 claim 与 fulfill 分两事务，失败即永久丢单）。
func (d *SubscriptionDal) ClaimAndFulfillPaidOrder(ctx context.Context, orderNo, platformOrderNo string, paidAt time.Time, userID int64, tier string, days int) (*model.PaymentOrder, bool, error) {
	if days < 1 {
		return nil, false, fmt.Errorf("套餐天数非法: %d", days)
	}
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
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", userID).
			First(&u).Error; err != nil {
			return err
		}
		now := time.Now()
		// 过期且有排队档先晋升，再按叠加规则履约
		promoteInPlace(&u, now)
		applyPurchase(&u, tier, days, "payfm", now)
		if err := tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
			"sub_tier":           u.SubTier,
			"sub_expire_at":      u.SubExpireAt,
			"sub_source":         u.SubSource,
			"sub_pending_tier":   u.SubPendingTier,
			"sub_pending_days":   u.SubPendingDays,
			"sub_pending_source": u.SubPendingSource,
			"sub_reminded":       u.SubReminded,
		}).Error; err != nil {
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

// UserSubscription 用户当前订阅状态（未订阅/过期返回空；读取时懒晋升排队档）。
// 返回 (tier, expireAt, source, pendingTier, pendingDays)。
func (d *SubscriptionDal) UserSubscription(ctx context.Context, userID int64) (tier string, expireAt *time.Time, source string, pendingTier string, pendingDays int) {
	var u model.User
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", userID).
			First(&u).Error; err != nil {
			return err
		}
		now := time.Now()
		if promoteInPlace(&u, now) {
			// 晋升产生了状态变化：写回（pending 清空、reminded 重置）
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
		return nil
	})
	if err != nil {
		return "", nil, "", "", 0
	}
	tier = strings.TrimSpace(u.SubTier)
	if tier == "" {
		return "", nil, "", "", 0
	}
	if u.SubExpireAt != nil && !u.SubExpireAt.After(time.Now()) {
		// 无排队档时已过期按未订阅
		return "", nil, "", "", 0
	}
	pendingTier = strings.TrimSpace(u.SubPendingTier)
	pendingDays = u.SubPendingDays
	if pendingDays < 0 {
		pendingDays = 0
	}
	return tier, u.SubExpireAt, u.SubSource, pendingTier, pendingDays
}

// Grant 人工赋予/更新订阅（走与支付相同的档位叠加语义；事务内完成，source 固定 manager）。
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
		promoteInPlace(&u, now)
		applyPurchase(&u, tier, days, "manager", now)
		return tx.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
			"sub_tier":           u.SubTier,
			"sub_expire_at":      u.SubExpireAt,
			"sub_source":         u.SubSource,
			"sub_pending_tier":   u.SubPendingTier,
			"sub_pending_days":   u.SubPendingDays,
			"sub_pending_source": u.SubPendingSource,
			"sub_reminded":       u.SubReminded,
		}).Error
	})
}

// Revoke 取消订阅：清空当前档 + 排队档（保留 ai_daily_enabled 用户偏好）。
func (d *SubscriptionDal) Revoke(ctx context.Context, userID int64) error {
	return d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"sub_tier":           "",
		"sub_expire_at":      nil,
		"sub_source":         "",
		"sub_pending_tier":   "",
		"sub_pending_days":   0,
		"sub_pending_source": "",
		"sub_reminded":       0,
	}).Error
}

// PromoteDue 批量晋升：当前档已过期且有排队档的用户（每日后台 sweep 兜底）。
// 晋升后排队档生效，到期提醒重置，新到期日重新走提醒。
func (d *SubscriptionDal) PromoteDue(ctx context.Context) (int64, error) {
	res := d.db.WithContext(ctx).Model(&model.User{}).
		Where("sub_tier <> '' AND sub_expire_at IS NOT NULL AND sub_expire_at <= ? AND sub_pending_tier <> ''",
			time.Now()).
		Updates(map[string]interface{}{
			"sub_tier":           gorm.Expr("sub_pending_tier"),
			"sub_expire_at":      gorm.Expr("now() + make_interval(days => sub_pending_days)"),
			"sub_source":         gorm.Expr("sub_pending_source"),
			"sub_pending_tier":   "",
			"sub_pending_days":   0,
			"sub_pending_source": "",
			"sub_reminded":       0,
		})
	return res.RowsAffected, res.Error
}

// ExpiringUser 即将到期用户（提醒邮件用）
type ExpiringUser struct {
	UserID   uint
	Name     string
	Email    string
	Tier     string
	ExpireAt time.Time
	Reminded int
}

// ListExpiringSoon 即将到期（windowDays 内）且尚未发满提醒的用户。
// sub_reminded<3：仍可发 3 天提醒；sub_reminded!=1 且剩余<=1 天：发 1 天提醒（外层判定）。
func (d *SubscriptionDal) ListExpiringSoon(ctx context.Context, windowDays int) ([]ExpiringUser, error) {
	if windowDays < 1 {
		windowDays = 1
	}
	var rows []ExpiringUser
	err := d.db.WithContext(ctx).Table("users").
		Select("id AS user_id, name, email, sub_tier AS tier, sub_expire_at AS expire_at, sub_reminded AS reminded").
		Where("sub_tier <> '' AND sub_expire_at IS NOT NULL AND sub_expire_at > ? AND sub_expire_at <= ? AND sub_reminded < 3",
			time.Now(), time.Now().AddDate(0, 0, windowDays)).
		Find(&rows).Error
	return rows, err
}

// MarkReminded 记录已发提醒窗口（3=已发3天提醒，1=已发1天提醒）。
func (d *SubscriptionDal) MarkReminded(ctx context.Context, userID uint, window int) error {
	return d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).
		Update("sub_reminded", window).Error
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
	err := q.Select("id, username, name, avatar, sub_tier, sub_expire_at, sub_source, sub_pending_tier, sub_pending_days").
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
