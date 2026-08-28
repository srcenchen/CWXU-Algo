package model

import "time"

// PluginAuthorization stores only the digest of a browser-sync device token.
// The plaintext token is returned once and must never be persisted.
type PluginAuthorization struct {
	ID            uint      `gorm:"primaryKey"`
	UserID        uint      `gorm:"not null;index:idx_plugin_authorizations_user_provider"`
	Provider      string    `gorm:"size:32;not null;index:idx_plugin_authorizations_user_provider"`
	ClientKind    string    `gorm:"size:32;not null"`
	ClientVersion string    `gorm:"size:64;not null"`
	LuoguUID      string    `gorm:"size:32;not null;index"`
	TokenHash     string    `gorm:"size:72;not null;uniqueIndex"`
	RiskVersion   string    `gorm:"size:32;not null"`
	AcceptedAt    time.Time `gorm:"not null"`
	ExpiresAt     time.Time `gorm:"not null;index"`
	LastUsedAt    *time.Time
	RevokedAt     *time.Time `gorm:"index"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (PluginAuthorization) TableName() string { return "plugin_authorizations" }
