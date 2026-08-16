package service

import (
	"context"
	"errors"
	"os"
	"strings"

	backuppb "cwxu-algo/api/core/v1/backup"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/backupcoord"

	kerrors "github.com/go-kratos/kratos/v2/errors"
)

type backupManager interface {
	Trigger(context.Context, backupcoord.Trigger) error
	Status() backupcoord.Status
}

type BackupService struct {
	backuppb.UnimplementedBackupServer
	manager    backupManager
	authorized func(context.Context) bool
}

func NewBackupService(manager *backupcoord.Coordinator) *BackupService {
	return newBackupService(manager, func(ctx context.Context) bool {
		return auth.HasPerm(ctx, rbac.PermSiteBackup)
	})
}

func newBackupService(manager backupManager, authorized func(context.Context) bool) *BackupService {
	return &BackupService{manager: manager, authorized: authorized}
}

func backupPayloadHasPermission(payload *auth.JwtPayload) bool {
	return auth.PayloadHasPerm(payload, rbac.PermSiteBackup)
}

func (s *BackupService) Run(ctx context.Context, _ *backuppb.RunBackupRequest) (*backuppb.RunBackupReply, error) {
	if !s.authorized(ctx) {
		return nil, kerrors.Forbidden("BACKUP_PERMISSION_DENIED", "site backup permission required")
	}
	if err := s.manager.Trigger(ctx, backupcoord.TriggerManual); err != nil {
		switch {
		case errors.Is(err, backupcoord.ErrDisabled):
			return nil, kerrors.ServiceUnavailable("BACKUP_DISABLED", "backup is disabled")
		case errors.Is(err, backupcoord.ErrAlreadyRunning):
			return nil, kerrors.Conflict("BACKUP_ALREADY_RUNNING", "backup is already running")
		case errors.Is(err, backupcoord.ErrStopping):
			return nil, kerrors.ServiceUnavailable("BACKUP_STOPPING", "backup service is stopping")
		default:
			return nil, kerrors.InternalServer("BACKUP_TRIGGER_FAILED", err.Error())
		}
	}
	return &backuppb.RunBackupReply{Accepted: true, Status: backupStatusProto(s.manager.Status())}, nil
}

func (s *BackupService) Status(ctx context.Context, _ *backuppb.GetBackupStatusRequest) (*backuppb.GetBackupStatusReply, error) {
	if !s.authorized(ctx) {
		return nil, kerrors.Forbidden("BACKUP_PERMISSION_DENIED", "site backup permission required")
	}
	return &backuppb.GetBackupStatusReply{Status: backupStatusProto(s.manager.Status())}, nil
}

// DownloadKey 返回备份加密密钥（32 原始字节）。
// 路径取自 CWXU_BACKUP_ENCRYPTION_KEY_FILE，与备份任务一致；需 site.backup.manage 权限。
func (s *BackupService) DownloadKey(ctx context.Context, _ *backuppb.DownloadBackupKeyRequest) (*backuppb.DownloadBackupKeyReply, error) {
	if !s.authorized(ctx) {
		return nil, kerrors.Forbidden("BACKUP_PERMISSION_DENIED", "site backup permission required")
	}
	path := strings.TrimSpace(os.Getenv("CWXU_BACKUP_ENCRYPTION_KEY_FILE"))
	if path == "" {
		return nil, kerrors.ServiceUnavailable("BACKUP_KEY_UNCONFIGURED", "backup encryption key is not configured")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, kerrors.InternalServer("BACKUP_KEY_READ_FAILED", "read backup encryption key: "+err.Error())
	}
	if len(key) != 32 {
		return nil, kerrors.InternalServer("BACKUP_KEY_INVALID", "backup encryption key must be exactly 32 bytes")
	}
	return &backuppb.DownloadBackupKeyReply{Key: key}, nil
}

func backupStatusProto(status backupcoord.Status) *backuppb.BackupStatus {
	result := &backuppb.BackupStatus{
		Enabled: status.Enabled, Status: string(status.State), Trigger: string(status.Trigger),
		Stage: status.Stage, Message: status.Message, Error: status.Error,
		ArchiveKey: status.ArchiveKey, ArchiveSize: status.ArchiveSize,
		Sha256: status.SHA256, DatabaseCount: int32(status.DatabaseCount),
	}
	if !status.StartedAt.IsZero() {
		result.StartedAt = status.StartedAt.Unix()
	}
	if !status.FinishedAt.IsZero() {
		result.FinishedAt = status.FinishedAt.Unix()
	}
	return result
}
