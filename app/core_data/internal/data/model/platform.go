package model

import "time"

type Platform struct {
	Id       int64  `gorm:"primaryKey"`
	UserID   int64  `gorm:"uniqueIndex:idx_platform_user;comment:用户ID"`
	Platform string `gorm:"size:32;uniqueIndex:idx_platform_user;comment:平台"`
	Username string `gorm:"size:128;not null;comment:平台用户名"`
	// Rating 各 OJ 当前 rating（整数；力扣等浮点四舍五入）
	Rating int `gorm:"comment:OJ Rating"`
	// HasRating 是否成功抓到有效 rating（false=未参赛/平台无 rating/抓取失败）
	HasRating bool `gorm:"default:false;comment:是否有有效 Rating"`
	// Browser-local sync checkpoint. It advances only after a stable scan has
	// reached the previous checkpoint or the remote end.
	ClientSyncHeadSubmitID string     `gorm:"size:64;comment:浏览器同步完整扫描锚点"`
	ClientSyncCompletedAt  *time.Time `gorm:"comment:浏览器同步最近完整完成时间"`
}
