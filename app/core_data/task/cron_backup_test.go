package task

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

type fakeScheduledBackup struct{ runs int }

func (b *fakeScheduledBackup) RunScheduled() { b.runs++ }

func TestRegisterBackupScheduleAtTwoShanghaiWithoutCatchup(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	c := cron.New(cron.WithLocation(loc))
	backup := &fakeScheduledBackup{}
	if err := registerBackupSchedule(c, backup); err != nil {
		t.Fatal(err)
	}
	if backup.runs != 0 {
		t.Fatalf("registration ran backup %d times", backup.runs)
	}
	entries := c.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	from := time.Date(2026, 8, 16, 1, 59, 0, 0, loc)
	want := time.Date(2026, 8, 16, 2, 0, 0, 0, loc)
	if got := entries[0].Schedule.Next(from); !got.Equal(want) {
		t.Fatalf("next run = %v, want %v", got, want)
	}
}
