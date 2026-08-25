package model

import "time"

// LuoGuPublicSnapshot stores public-profile totals without fabricating submit history.
type LuoGuPublicSnapshot struct {
	UserID                  int64     `gorm:"primaryKey;comment:用户ID"`
	Platform                string    `gorm:"primaryKey;size:32;comment:OJ平台"`
	RemoteUID               int64     `gorm:"not null;comment:洛谷UID"`
	TotalSolved             int64     `gorm:"not null;default:0;comment:公开累计过题数"`
	TotalSubmit             int64     `gorm:"not null;default:0;comment:公开累计提交数"`
	TodaySolved             int64     `gorm:"not null;default:0;comment:当天临时过题数"`
	TodaySubmit             int64     `gorm:"not null;default:0;comment:当天临时提交数"`
	RealTodaySolvedBaseline int64     `gorm:"not null;default:0;comment:激活时个人今日去重过题基线"`
	RealTodayACBaseline     int64     `gorm:"not null;default:0;comment:激活时组织今日AC条数基线"`
	RealTodaySubmitBaseline int64     `gorm:"not null;default:0;comment:激活时今日提交基线"`
	Active                  bool      `gorm:"not null;default:false;index;comment:是否覆盖统计"`
	RecoveryRequired        bool      `gorm:"not null;default:false;index;comment:是否需完整恢复"`
	ObservedAt              time.Time `gorm:"not null;comment:公开累计观测时间"`
}

func (LuoGuPublicSnapshot) TableName() string { return "luogu_public_snapshots" }
