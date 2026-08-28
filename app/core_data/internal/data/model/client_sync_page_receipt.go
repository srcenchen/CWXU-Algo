package model

import "time"

// ClientSyncPageReceipt is the durable idempotency boundary between the SQL
// submit transaction and the Redis session checkpoint. A retry can rebuild the
// exact page counters after SQL committed even when Redis or the HTTP response
// failed.
type ClientSyncPageReceipt struct {
	SessionID        string `gorm:"primaryKey;size:64"`
	Restart          int32  `gorm:"primaryKey"`
	Page             int32  `gorm:"primaryKey"`
	Digest           string `gorm:"size:64;not null"`
	UserID           int64  `gorm:"not null;index"`
	Platform         string `gorm:"size:32;not null"`
	Generation       int64  `gorm:"not null"`
	PageInserted     int64  `gorm:"not null"`
	Inserted         int64  `gorm:"not null"`
	ProcessedPages   int32  `gorm:"not null"`
	NextPage         int32  `gorm:"not null"`
	FirstSubmitID    string `gorm:"size:64"`
	RemoteCount      int32  `gorm:"not null"`
	PerPage          int32  `gorm:"not null"`
	CompletionReason string `gorm:"size:16"`
	NextAvailableAt  int64  `gorm:"not null"`
	HasPending       bool   `gorm:"not null;default:false"`
	EffectsAppliedAt *time.Time
	CreatedAt        time.Time
	ExpiresAt        time.Time `gorm:"not null;index"`
}

// ClientSyncPostProcessJob is the durable dirty-session boundary. A dirty
// session becomes runnable on normal completion, explicit termination, or its
// last persisted expiry. Lease fields make the fixed-size worker safe across
// multiple core_data replicas.
type ClientSyncPostProcessJob struct {
	SessionID    string     `gorm:"primaryKey;size:64"`
	UserID       int64      `gorm:"not null;index"`
	Platform     string     `gorm:"size:32;not null"`
	Dirty        bool       `gorm:"not null;default:false;index"`
	ReceiptCount int32      `gorm:"not null;default:0"`
	ReadyAt      time.Time  `gorm:"not null;index"`
	LeaseUntil   *time.Time `gorm:"index"`
	Attempts     int        `gorm:"not null;default:0"`
	LastError    string     `gorm:"size:512;not null;default:''"`
	CompletedAt  *time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
