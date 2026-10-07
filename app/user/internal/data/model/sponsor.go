package model

import "time"

// DefaultSponsorIntro 打赏页默认说明（站管可后台改）
const DefaultSponsorIntro = "如果希望这个项目能继续做下去，欢迎赞助支持。所有费用都会用于服务器相关业务的必要开支。"

// SponsorOrder 打赏订单（order_no 幂等；paid 后计入赞助收入）
type SponsorOrder struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	// OrderNo 订单号（幂等锚点）
	OrderNo string `gorm:"size:64;not null;uniqueIndex;comment:订单号"`
	// UserID 打赏用户
	UserID uint `gorm:"not null;index;comment:打赏用户"`
	// Nickname 下单时昵称快照（展示用，避免列表 join）
	Nickname string `gorm:"size:64;not null;comment:昵称快照"`
	// AmountCents 金额（分）
	AmountCents int64 `gorm:"not null;comment:金额(分)"`
	// Message 留言（可空）
	Message string `gorm:"size:200;default:'';comment:留言"`
	// Status pending|paid|closed
	Status string `gorm:"size:16;not null;default:'pending';index;comment:状态 pending|paid|closed"`
	// PlatformOrderNo 支付FM平台订单号
	PlatformOrderNo string `gorm:"size:64;default:'';comment:支付FM平台订单号"`
	// PaidAt 支付成功时间
	PaidAt *time.Time
}

func (SponsorOrder) TableName() string { return "sponsor_orders" }

// SponsorExpense 打赏开支（日常开支 / 余额调整；note 必填）
type SponsorExpense struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time

	// AmountCents 开支金额（分）
	AmountCents int64 `gorm:"not null;comment:金额(分)"`
	// Note 说明（必填，展示在打赏页）
	Note string `gorm:"size:200;not null;comment:说明"`
	// Kind expense=日常开支；adjust=余额调整
	Kind string `gorm:"size:16;not null;default:'expense';comment:类型 expense|adjust"`
	// SpentAt 发生时间
	SpentAt time.Time `gorm:"not null;index;comment:发生时间"`
	// CreatedBy 操作人（站管用户ID）
	CreatedBy uint `gorm:"not null;default:0;comment:操作人"`
}

func (SponsorExpense) TableName() string { return "sponsor_expenses" }

// SponsorSetting 打赏页设置（单行 id=1）
type SponsorSetting struct {
	ID        uint `gorm:"primaryKey"`
	UpdatedAt time.Time

	// MembershipSponsorEnabled 会员赞助入口开关
	MembershipSponsorEnabled bool `gorm:"not null;default:true;comment:会员赞助入口开关"`
	// DonationEnabled 打赏赞助入口开关
	DonationEnabled bool `gorm:"not null;default:true;comment:打赏赞助入口开关"`
	// IntroMarkdown 打赏页说明（Markdown，站管可改）
	IntroMarkdown string `gorm:"type:text;not null;default:'';comment:打赏页说明(Markdown)"`
}

func (SponsorSetting) TableName() string { return "sponsor_settings" }
