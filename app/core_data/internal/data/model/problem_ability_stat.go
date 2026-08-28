package model

import "time"

// AbilityModelState names the fully published global ability-stat snapshot.
// ID is always 1; keeping the pointer separate makes a snapshot switch atomic.
type AbilityModelState struct {
	ID            uint   `gorm:"primaryKey"`
	ActiveVersion uint64 `gorm:"not null"`
	// BuiltAt is the publication time of ActiveVersion. ActiveVersion itself is
	// the ready/status marker: a version becomes readable only after its rows
	// have been written in the same transaction.
	BuiltAt   time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (AbilityModelState) TableName() string { return "ability_model_state" }

// ProblemAbilityStat is a problem's posterior AC snapshot. ModelVersion and
// ProblemID form the key so readers can only select one complete version.
type ProblemAbilityStat struct {
	ModelVersion    uint64    `gorm:"primaryKey"`
	ProblemID       uint      `gorm:"primaryKey"`
	Platform        string    `gorm:"size:32;not null"`
	Difficulty      string    `gorm:"size:32;not null"`
	AttemptCount    float64   `gorm:"not null"`
	ACUserCount     float64   `gorm:"not null"`
	GroupPriorRate  float64   `gorm:"not null"`
	PosteriorACRate float64   `gorm:"not null"`
	Hardness        float64   `gorm:"not null"`
	BuiltAt         time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

func (ProblemAbilityStat) TableName() string { return "problem_ability_stats" }
