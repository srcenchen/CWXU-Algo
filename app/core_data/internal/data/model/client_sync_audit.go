package model

import "time"

// ClientSyncAudit is a bounded-lifetime session summary. It deliberately does
// not contain credentials, cookies, page payloads, HTML, or stack traces.
type ClientSyncAudit struct {
	SessionID        string     `gorm:"primaryKey;size:64"`
	AuthorizationID  uint64     `gorm:"not null;index"`
	UserID           int64      `gorm:"not null;index"`
	Username         string     `gorm:"size:64;not null;default:''"`
	Platform         string     `gorm:"size:32;not null;index"`
	OJUID            string     `gorm:"size:64;not null;index"`
	ClientKind       string     `gorm:"size:32;not null"`
	ClientVersion    string     `gorm:"size:64;not null"`
	Status           string     `gorm:"size:16;not null;index"`
	CompletionReason string     `gorm:"size:32;not null;default:''"`
	StartedAt        time.Time  `gorm:"not null;index"`
	UpdatedAt        time.Time  `gorm:"not null"`
	TerminalAt       *time.Time `gorm:"index:idx_client_sync_audits_retention_terminal,priority:2"`
	RetentionUntil   time.Time  `gorm:"index:idx_client_sync_audits_retention_terminal,priority:1"`
	ProcessedPages   int32      `gorm:"not null;default:0"`
	RemoteCount      int32      `gorm:"not null;default:-1"`
	Inserted         int64      `gorm:"not null;default:0"`
	RestartCount     int32      `gorm:"not null;default:0"`
	ErrorCode        string     `gorm:"size:64;not null;default:''"`
	ErrorMessage     string     `gorm:"size:512;not null;default:''"`
}

func (ClientSyncAudit) TableName() string { return "client_sync_audits" }
