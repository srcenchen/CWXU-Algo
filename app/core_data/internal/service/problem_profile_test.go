package service

import (
	"fmt"
	"testing"

	"cwxu-algo/api/core/v1/problem"
)

func TestTopUserProfileRadarKeepsAllTagStatsSeparate(t *testing.T) {
	tagStats := make([]*problem.TagScore, 25)
	for i := range tagStats {
		tagStats[i] = &problem.TagScore{Tag: fmt.Sprintf("tag-%02d", i)}
	}

	radar := topUserProfileRadar(tagStats)

	if len(radar) != 8 || len(tagStats) != 25 {
		t.Fatalf("radar=%d tagStats=%d, want 8 and 25", len(radar), len(tagStats))
	}
	for i := range radar {
		if radar[i] != tagStats[i] {
			t.Fatalf("radar[%d] does not preserve the sorted tag prefix", i)
		}
	}
	if cap(radar) != len(radar) {
		t.Fatalf("radar capacity=%d, want isolated prefix capacity %d", cap(radar), len(radar))
	}
}
