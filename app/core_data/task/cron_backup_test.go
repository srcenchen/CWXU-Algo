package task

import (
	"testing"
	"time"
)

type fakeScheduledBackup struct {
	runs int
	at   time.Time
}

func (b *fakeScheduledBackup) RunScheduled(at time.Time) { b.runs++; b.at = at }

func TestBackupScheduleChecksEveryMinuteWithCurrentTime(t *testing.T) {
	b := &fakeScheduledBackup{}
	now := time.Date(2026, 8, 16, 3, 15, 0, 0, time.FixedZone("CST", 8*3600))
	backupTick(b, now)
	if b.runs != 1 || !b.at.Equal(now) {
		t.Fatalf("tick = (%d, %v)", b.runs, b.at)
	}
}
