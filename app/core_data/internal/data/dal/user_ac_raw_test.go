package dal

import "testing"

// 纯公式断言：与 ComputeLifetimeACRaw 注释一致（无 DB）
func TestLifetimeACRawFormula(t *testing.T) {
	// nonLC=100 次（含多次）, lcOfficial=50, lcEvents=10, lcDistinct=8 → extras=2
	// lcPart = 50+2 = 52; raw = 100+52 = 152; unique=140 → raw 152
	nonLC, lcOfficial, lcEvents, lcDistinct := int64(100), int64(50), int64(10), int64(8)
	extras := lcEvents - lcDistinct
	if extras < 0 {
		extras = 0
	}
	lcPart := lcOfficial + extras
	if lcEvents > lcPart {
		lcPart = lcEvents
	}
	raw := nonLC + lcPart
	unique := int64(140)
	if raw < unique {
		raw = unique
	}
	if raw != 152 {
		t.Fatalf("raw=%d want 152", raw)
	}

	// 仅力扣官方、无多次：raw == unique
	nonLC, lcOfficial, lcEvents, lcDistinct = 0, 372, 1, 1
	extras = lcEvents - lcDistinct
	if extras < 0 {
		extras = 0
	}
	lcPart = lcOfficial + extras
	raw = nonLC + lcPart
	unique = 372
	if raw < unique {
		raw = unique
	}
	if raw != 372 {
		t.Fatalf("LC-only raw=%d want 372", raw)
	}

	// 无官方键，只有明细 5 次
	nonLC, lcOfficial, lcEvents = 20, 0, 5
	lcPart = lcEvents
	raw = nonLC + lcPart
	if raw != 25 {
		t.Fatalf("no-official raw=%d want 25", raw)
	}
}
