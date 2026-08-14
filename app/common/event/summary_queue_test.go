package event

import "testing"

func TestSummaryQueueForTypeSeparatesMailFromInteractiveWork(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"PersonalLastDay", SummaryMailQueue},
		{"PersonalDailyRule", SummaryMailQueue},
		{"WeeklyStaff", SummaryMailQueue},
		{"WeeklyReportForCoach", SummaryMailQueue},
		{"PersonalRecent", SummaryQueue},
		{"TrainingReport", SummaryQueue},
		{"", SummaryQueue},
	}
	for _, tc := range cases {
		if got := SummaryQueueForType(tc.typ); got != tc.want {
			t.Fatalf("SummaryQueueForType(%q) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}
