package model

import "time"

// UserTagAC 用户在某算法标签上的去重 AC 题数（画像雷达写时预聚合）
type UserTagAC struct {
	UserID int64  `gorm:"primaryKey;comment:用户ID"`
	Tag    string `gorm:"primaryKey;size:64;not null;comment:算法标签"`
	Count  int64  `gorm:"not null;default:0;comment:标签下去重规范题数"`
	// Weight is the unrounded quality sum Σx. It must retain enough precision
	// for TagAbilityScore rather than retaining the retired difficulty weight.
	Weight       float64 `gorm:"type:numeric(18,12);not null;default:0;comment:标签掌握质量和"`
	ScoreVersion uint    `gorm:"not null;default:0;index:idx_user_tag_ac_active,priority:2;comment:个人评分语义版本"`
	ModelVersion uint64  `gorm:"not null;default:0;index:idx_user_tag_ac_active,priority:3;comment:题目后验模型版本"`
}

func (UserTagAC) TableName() string { return "user_tag_ac" }

// UserTagACSnapshot is the atomic publication header for one user's tag
// aggregate. The header makes a legitimately empty radar distinguishable from
// a snapshot that has never been built, and binds every row to the evidence
// revisions used to produce it.
type UserTagACSnapshot struct {
	UserID                  int64     `gorm:"primaryKey;comment:用户ID"`
	ScoreVersion            uint      `gorm:"not null;comment:个人评分语义版本"`
	ModelVersion            uint64    `gorm:"not null;index;comment:题目后验模型版本"`
	EvidenceDatasetRevision uint64    `gorm:"not null;comment:全局证据版本"`
	EvidenceUserRevision    uint64    `gorm:"not null;comment:用户证据版本"`
	RowCount                int64     `gorm:"not null;default:0;comment:原子快照标签行数"`
	PublishedAt             time.Time `gorm:"not null;comment:快照发布时间"`
}

func (UserTagACSnapshot) TableName() string { return "user_tag_ac_snapshots" }

// DifficultyWeightSQL 难度 → 能力分权重的 SQL 表达式（简单=1 / 中等=3 / 困难=8 / 未知=2）。
// 供 user_tag_ac 预聚合重建/回填与 Go 端 DifficultyWeight 对齐使用。
const DifficultyWeightSQL = `CASE
	WHEN btrim(lower(p.difficulty)) IN ('简单','easy','入门') THEN 1
	WHEN btrim(lower(p.difficulty)) IN ('中等','medium','中级') THEN 3
	WHEN btrim(lower(p.difficulty)) IN ('困难','hard','高级') THEN 8
	ELSE 2
END`
