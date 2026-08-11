package model

import "time"

// 支付订单状态
const (
	OrderStatusPending = "pending"
	OrderStatusPaid    = "paid"
	OrderStatusClosed  = "closed"
)

// SubscriptionPlan 个人订阅套餐模板（站管可改；plan 唯一：free|plus|pro）
type SubscriptionPlan struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	Plan string `gorm:"size:16;uniqueIndex;not null;comment:套餐档 free|plus|pro"`
	// PriceCents 价格（分）；free=0
	PriceCents int64 `gorm:"default:0;comment:价格(分) free=0"`
	// ManualRefreshDaily 每日手动刷新做题记录次数
	ManualRefreshDaily int `gorm:"default:2;comment:每日手动刷新次数"`
	// SyncIntervalMin 自动同步间隔（分钟）
	SyncIntervalMin int `gorm:"default:180;comment:自动同步间隔(分钟)"`
	// AiAnalyzeMonth AI 分析题目次数/月（0=无）
	AiAnalyzeMonth int `gorm:"default:0;comment:AI分析次数/月(0=无)"`
	// EnableFetchProblem 爬题面
	EnableFetchProblem bool `gorm:"default:false;comment:爬题面"`
	// EnableAiAnalyze AI 分析题目
	EnableAiAnalyze bool `gorm:"default:false;comment:AI分析题目"`
	// EnableAiDaily AI 日报（Pro 专属，默认关）
	EnableAiDaily bool `gorm:"default:false;comment:AI日报"`
	// EnableRegularDaily 常规日报（无 AI）
	EnableRegularDaily bool `gorm:"default:true;comment:常规日报(无AI)"`
	// Days 购买时长（天）
	Days int `gorm:"default:30;comment:购买时长(天)"`
	// Enabled 上架
	Enabled bool `gorm:"default:true;comment:上架"`
}

func (SubscriptionPlan) TableName() string { return "subscription_plans" }

// PaymentOrder 支付订单（GuadArt OrderLedger 移植；order_no 为幂等锚点）
type PaymentOrder struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	// OrderNo 订单号（幂等锚点）
	OrderNo string `gorm:"size:64;not null;uniqueIndex;comment:订单号"`
	// UserID 下单用户
	UserID uint `gorm:"not null;index;comment:下单用户"`
	// Plan 购买的套餐档
	Plan string `gorm:"size:16;not null;comment:套餐档"`
	// Months 购买月数（1–12）；履约天数 = 套餐 days × months
	Months int `gorm:"default:1;comment:购买月数"`
	// AmountCents 应付金额（分）
	AmountCents int64 `gorm:"not null;comment:金额(分)"`
	// Status pending|paid|closed
	Status string `gorm:"size:16;not null;default:'pending';index;comment:状态 pending|paid|closed"`
	// PlatformOrderNo 支付FM平台订单号
	PlatformOrderNo string `gorm:"size:64;default:'';comment:支付FM平台订单号"`
	// PaidAt 支付成功时间
	PaidAt *time.Time
}

func (PaymentOrder) TableName() string { return "payment_orders" }
