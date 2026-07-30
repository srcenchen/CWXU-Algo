package model

import "time"

// ObsidianPluginMeta 当前发布的 GoAlgo Blog 插件版本（单行 id=1）。
// 版本号由发布脚本写入；客户端只读本表拿 version / downloadBase，再去云存储下具体文件。
type ObsidianPluginMeta struct {
	ID uint `gorm:"primaryKey"`
	// Version semver，如 0.1.2
	Version string `gorm:"size:32;not null;default:''"`
	// MinAppVersion 最低 Obsidian 版本
	MinAppVersion string `gorm:"size:32;not null;default:'1.4.0'"`
	// Notes 更新说明（可空）
	Notes string `gorm:"type:text"`
	// ReleasedAt unix 秒
	ReleasedAt int64 `gorm:"not null;default:0"`
	// DownloadBase 云存储目录 URL（无尾斜杠），如 https://zhiyuansofts.cn/obsidian/goalgo-blog/0.1.2
	DownloadBase string `gorm:"size:512;not null;default:''"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
}

func (ObsidianPluginMeta) TableName() string { return "obsidian_plugin_meta" }
