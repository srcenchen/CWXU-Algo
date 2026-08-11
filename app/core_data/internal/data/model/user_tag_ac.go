package model

// UserTagAC 用户在某算法标签上的去重 AC 题数（画像雷达写时预聚合）
type UserTagAC struct {
	UserID int64  `gorm:"primaryKey;comment:用户ID"`
	Tag    string `gorm:"primaryKey;size:64;not null;comment:算法标签"`
	Count  int64  `gorm:"not null;default:0;comment:去重 AC 题数"`
	// Weight 难度加权题量：简单=1 / 中等=3 / 困难=8 / 未知=2 求和。
	// 能力雷达 score 基于它做饱和锚定，避免纯题量失真。
	Weight float64 `gorm:"not null;default:0;comment:难度加权题量"`
}

func (UserTagAC) TableName() string { return "user_tag_ac" }

// DifficultyWeightSQL 难度 → 能力分权重的 SQL 表达式（简单=1 / 中等=3 / 困难=8 / 未知=2）。
// 供 user_tag_ac 预聚合重建/回填与 Go 端 DifficultyWeight 对齐使用。
const DifficultyWeightSQL = `CASE
	WHEN btrim(lower(p.difficulty)) IN ('简单','easy','入门') THEN 1
	WHEN btrim(lower(p.difficulty)) IN ('中等','medium','中级') THEN 3
	WHEN btrim(lower(p.difficulty)) IN ('困难','hard','高级') THEN 8
	ELSE 2
END`
