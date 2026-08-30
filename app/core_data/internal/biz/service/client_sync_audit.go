package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const clientSyncAuditRetention = 7 * 24 * time.Hour

type ClientSyncAuditStart struct {
	SessionID                                  string
	AuthorizationID                            uint64
	UserID                                     int64
	Username                                   string
	Platform, OJUID, ClientKind, ClientVersion string
	StartedAt                                  time.Time
}

type ClientSyncAuditProgress struct {
	SessionID                                 string
	ProcessedPages, RemoteCount, RestartCount int32
	Inserted                                  int64
	UpdatedAt                                 time.Time
}

func (uc *SpiderUseCase) StartClientSyncAudit(ctx context.Context, start ClientSyncAuditStart) error {
	if uc == nil || uc.data == nil || uc.data.DB == nil || strings.TrimSpace(start.SessionID) == "" {
		return nil
	}
	row := model.ClientSyncAudit{SessionID: start.SessionID, AuthorizationID: start.AuthorizationID, UserID: start.UserID, Username: start.Username, Platform: start.Platform, OJUID: start.OJUID, ClientKind: start.ClientKind, ClientVersion: start.ClientVersion, Status: "running", StartedAt: start.StartedAt.UTC(), UpdatedAt: start.StartedAt.UTC(), RemoteCount: -1}
	return uc.data.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

func updateClientSyncAudit(ctx context.Context, db *gorm.DB, progress ClientSyncAuditProgress) error {
	if db == nil || strings.TrimSpace(progress.SessionID) == "" {
		return nil
	}
	result := db.WithContext(ctx).Model(&model.ClientSyncAudit{}).
		Where("session_id = ? AND terminal_at IS NULL", progress.SessionID).
		Updates(map[string]interface{}{
			"processed_pages": gorm.Expr("CASE WHEN processed_pages > ? THEN processed_pages ELSE ? END", progress.ProcessedPages, progress.ProcessedPages),
			"remote_count":    gorm.Expr("CASE WHEN remote_count > ? THEN remote_count ELSE ? END", progress.RemoteCount, progress.RemoteCount),
			"inserted":        gorm.Expr("CASE WHEN inserted > ? THEN inserted ELSE ? END", progress.Inserted, progress.Inserted),
			"restart_count":   gorm.Expr("CASE WHEN restart_count > ? THEN restart_count ELSE ? END", progress.RestartCount, progress.RestartCount),
			"updated_at":      gorm.Expr("CASE WHEN updated_at > ? THEN updated_at ELSE ? END", progress.UpdatedAt.UTC(), progress.UpdatedAt.UTC()),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("client sync audit progress not updated: session=%s", progress.SessionID)
	}
	return nil
}

func (uc *SpiderUseCase) UpdateClientSyncAudit(ctx context.Context, progress ClientSyncAuditProgress) error {
	if uc == nil || uc.data == nil {
		return nil
	}
	return updateClientSyncAudit(ctx, uc.data.DB, progress)
}

func terminateClientSyncAudit(ctx context.Context, db *gorm.DB, sessionID, status, reason, errorCode, errorMessage string, at time.Time) error {
	if db == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if len(errorMessage) > 512 {
		errorMessage = errorMessage[:512]
	}
	at = at.UTC()
	result := db.WithContext(ctx).Model(&model.ClientSyncAudit{}).Where("session_id = ? AND terminal_at IS NULL", sessionID).
		Updates(map[string]interface{}{"status": status, "completion_reason": reason, "error_code": errorCode, "error_message": errorMessage, "terminal_at": at, "retention_until": at.Add(clientSyncAuditRetention), "updated_at": at})
	if result.Error != nil {
		return result.Error
	}
	// A zero-row terminal update is the expected idempotent replay case.
	return nil
}

func (uc *SpiderUseCase) TerminateClientSyncAudit(ctx context.Context, sessionID, status, reason, errorCode, errorMessage string, at time.Time) error {
	if uc == nil || uc.data == nil {
		return nil
	}
	return terminateClientSyncAudit(ctx, uc.data.DB, sessionID, status, reason, errorCode, errorMessage, at)
}
