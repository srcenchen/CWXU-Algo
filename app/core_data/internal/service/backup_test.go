package service

import (
	"context"
	"strings"
	"testing"

	backuppb "cwxu-algo/api/core/v1/backup"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/backupcoord"

	kerrors "github.com/go-kratos/kratos/v2/errors"
)

type fakeBackupManager struct {
	err      error
	trigger  backupcoord.Trigger
	triggers int
	status   backupcoord.Status
}

func (m *fakeBackupManager) Trigger(_ context.Context, trigger backupcoord.Trigger) error {
	m.trigger = trigger
	m.triggers++
	return m.err
}
func (m *fakeBackupManager) Status() backupcoord.Status { return m.status }

func TestBackupPermissionRequiresPermSiteBackup(t *testing.T) {
	if backupPayloadHasPermission(&auth.JwtPayload{Pm: rbac.Encode([]string{rbac.PermSiteConfigRead})}) {
		t.Fatal("unrelated site permission must not authorize backup")
	}
	if !backupPayloadHasPermission(&auth.JwtPayload{Pm: rbac.Encode([]string{rbac.PermSiteBackup})}) {
		t.Fatal("site backup permission must authorize backup")
	}
	if backupPayloadHasPermission(nil) {
		t.Fatal("nil payload must be denied")
	}
}

func TestRunBackupTriggersManualAsynchronously(t *testing.T) {
	manager := &fakeBackupManager{status: backupcoord.Status{Enabled: true, State: backupcoord.StateRunning}}
	svc := newBackupService(manager, func(context.Context) bool { return true })
	reply, err := svc.Run(context.Background(), &backuppb.RunBackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Accepted || manager.triggers != 1 || manager.trigger != backupcoord.TriggerManual {
		t.Fatalf("reply=%+v triggers=%d trigger=%q", reply, manager.triggers, manager.trigger)
	}
}

func TestBackupServiceRejectsUnauthorizedAndUnavailableRuns(t *testing.T) {
	tests := []struct {
		name       string
		authorized bool
		managerErr error
		wantReason string
	}{
		{name: "permission", authorized: false, wantReason: "BACKUP_PERMISSION_DENIED"},
		{name: "disabled", authorized: true, managerErr: backupcoord.ErrDisabled, wantReason: "BACKUP_DISABLED"},
		{name: "duplicate", authorized: true, managerErr: backupcoord.ErrAlreadyRunning, wantReason: "BACKUP_ALREADY_RUNNING"},
		{name: "stopping", authorized: true, managerErr: backupcoord.ErrStopping, wantReason: "BACKUP_STOPPING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &fakeBackupManager{err: tt.managerErr}
			svc := newBackupService(manager, func(context.Context) bool { return tt.authorized })
			_, err := svc.Run(context.Background(), &backuppb.RunBackupRequest{})
			if err == nil || err.Error() == "" {
				t.Fatalf("Run error = %v", err)
			}
			if reason := kratosReason(err); reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func kratosReason(err error) string {
	if err == nil {
		return ""
	}
	return kerrors.FromError(err).Reason
}

func TestBackupStatusRequiresPermission(t *testing.T) {
	manager := &fakeBackupManager{}
	svc := newBackupService(manager, func(context.Context) bool { return false })
	_, err := svc.Status(context.Background(), &backuppb.GetBackupStatusRequest{})
	if reason := kratosReason(err); reason != "BACKUP_PERMISSION_DENIED" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestRunBackupReturnsStaticConfigurationProblem(t *testing.T) {
	manager := &fakeBackupManager{
		err: backupcoord.ErrDisabled,
		status: backupcoord.Status{Error: "backup disabled: missing CWXU_BACKUP_PG_DSN"},
	}
	svc := newBackupService(manager, func(context.Context) bool { return true })
	_, err := svc.Run(context.Background(), &backuppb.RunBackupRequest{})
	if err == nil || !strings.Contains(err.Error(), "CWXU_BACKUP_PG_DSN") {
		t.Fatalf("Run error = %v, want static configuration problem", err)
	}
}
