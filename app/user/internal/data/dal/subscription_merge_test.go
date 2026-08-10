package dal

import (
	"testing"
)

// 每日手动刷新配额合并：override 0 / 5 / nil × 订阅(plan=15) / 免费
func TestMergeRefreshQuota(t *testing.T) {
	five := 5
	zero := 0
	plan15 := 15

	cases := []struct {
		name       string
		override   *int
		planQuota  int
		subscribed bool
		wantQuota  int
		wantOver   bool
	}{
		{"订阅+覆盖0=禁止", &zero, plan15, true, 0, true},
		{"订阅+覆盖5=取最大15", &five, plan15, true, 15, true},
		{"订阅+无覆盖=订阅档15", nil, plan15, true, 15, false},
		{"免费+无覆盖=默认2", nil, 0, false, 2, false},
		{"免费+覆盖5=覆盖生效", &five, 0, false, 5, true},
		{"免费+覆盖0=禁止", &zero, 0, false, 0, true},
		{"订阅+覆盖0且plan0=禁止", &zero, 0, true, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ov := mergeRefreshQuota(c.override, c.planQuota, c.subscribed)
			if q != c.wantQuota || ov != c.wantOver {
				t.Fatalf("mergeRefreshQuota(%v,%d,%v) = (%d,%v), want (%d,%v)",
					c.override, c.planQuota, c.subscribed, q, ov, c.wantQuota, c.wantOver)
			}
		})
	}
}

// 爬取间隔 min 合并：公共域 180 + 订阅 60 → 60；组织 30 + 订阅 60 → 30；无组织无订阅 → 180
func TestMergeSpiderInterval(t *testing.T) {
	cases := []struct {
		name       string
		orgMin     int
		override   int
		subMin     int
		subscribed bool
		want       int
	}{
		{"公共域180+订阅60 → 60", 180, 0, 60, true, 60},
		{"组织30+订阅60 → 30", 30, 0, 60, true, 30},
		{"无组织无订阅 → 180", 0, 0, 0, false, 180},
		{"订阅60无组织 → 60", 0, 0, 60, true, 60},
		{"覆盖120+组织60 → 60（min）", 60, 120, 0, false, 60},
		{"仅覆盖120 → 120", 0, 120, 0, false, 120},
		{"脏数据0全部 → 180", 0, 0, 0, false, 180},
		{"超上限夹到10080", 20000, 0, 0, false, 10080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeSpiderInterval(c.orgMin, c.override, c.subMin, c.subscribed)
			if got != c.want {
				t.Fatalf("mergeSpiderInterval(%d,%d,%d,%v) = %d, want %d",
					c.orgMin, c.override, c.subMin, c.subscribed, got, c.want)
			}
		})
	}
}
