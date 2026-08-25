package model

import "time"

// SpiderRepairState records a completed versioned data repair for one binding.
type SpiderRepairState struct {
	UserID      int64     `gorm:"primaryKey;comment:用户ID"`
	Platform    string    `gorm:"primaryKey;size:32;comment:OJ平台"`
	RepairKey   string    `gorm:"primaryKey;size:64;comment:修复标识"`
	Version     int       `gorm:"not null;comment:已完成修复版本"`
	CompletedAt time.Time `gorm:"not null;comment:完成时间"`
}

func (SpiderRepairState) TableName() string { return "spider_repair_states" }
