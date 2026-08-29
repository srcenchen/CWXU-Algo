package model

import "time"

// AbilityModelState names the fully published global ability-stat snapshot.
// ID is always 1; keeping the pointer separate makes a snapshot switch atomic.
type AbilityModelState struct {
	ID                         uint   `gorm:"primaryKey"`
	ActiveVersion              uint64 `gorm:"not null"`
	LastScheduledRefreshPeriod string `gorm:"size:32;not null;default:''"`
	// BuiltAt is the publication time of ActiveVersion. ActiveVersion itself is
	// the ready/status marker: a version becomes readable only after its rows
	// have been written in the same transaction.
	BuiltAt   time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (AbilityModelState) TableName() string { return "ability_model_state" }

// AbilityProfileScheduleRun marks a fully enqueued daily profile batch. The
// marker is written only after every candidate publish attempt succeeds.
type AbilityProfileScheduleRun struct {
	Period       string    `gorm:"primaryKey;size:32"`
	ModelVersion uint64    `gorm:"not null"`
	CompletedAt  time.Time `gorm:"not null"`
}

func (AbilityProfileScheduleRun) TableName() string { return "ability_profile_schedule_runs" }

// AbilityMaintenancePending is the durable retry source for fact changes whose
// derived ability/profile maintenance has not completed yet.
type AbilityMaintenancePending struct {
	Scope       string `gorm:"primaryKey;size:128"`
	OperationID string `gorm:"size:64;not null"`
	Revision    uint64 `gorm:"not null;default:1"`
	Phase       string `gorm:"size:16;not null;default:'intent'"`
	LeaseOwner  string `gorm:"size:64;not null;default:''"`
	// RelayLease guards the short MQ publication critical section after facts
	// have finalized. It is separate from the longer invalidation owner lease.
	RelayLeaseOwner    string    `gorm:"size:64;not null;default:''"`
	RelayLeaseUntil    time.Time `gorm:"index"`
	ProblemID          uint      `gorm:"not null;default:0"`
	UserID             int64     `gorm:"not null;default:0"`
	Platform           string    `gorm:"size:64;not null;default:''"`
	Payload            string    `gorm:"type:text;not null;default:''"`
	Operation          string    `gorm:"size:32;not null"`
	TagsChanged        bool      `gorm:"not null;default:false"`
	DifficultyChanged  bool      `gorm:"not null;default:false"`
	TargetModelVersion uint64    `gorm:"not null;default:0"`
	CreatedAt          time.Time `gorm:"not null"`
	UpdatedAt          time.Time `gorm:"not null"`
}

func (AbilityMaintenancePending) TableName() string { return "ability_maintenance_pending" }

// AbilityMaintenanceTarget is both durable work progress and the force-profile
// outbox. Publishing is at-least-once and keyed by (intent_id,user_id).
type AbilityMaintenanceTarget struct {
	IntentID        string    `gorm:"primaryKey;size:64"`
	UserID          int64     `gorm:"primaryKey"`
	Revision        uint64    `gorm:"not null;default:1"`
	State           string    `gorm:"size:24;not null;default:'pending'"`
	MessagePayload  string    `gorm:"type:text;not null;default:''"`
	PublishAttempts int       `gorm:"not null;default:0"`
	LastError       string    `gorm:"type:text;not null;default:''"`
	NextRetryAt     time.Time `gorm:"index"`
	CreatedAt       time.Time `gorm:"not null"`
	UpdatedAt       time.Time `gorm:"not null"`
}

func (AbilityMaintenanceTarget) TableName() string { return "ability_maintenance_targets" }

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
