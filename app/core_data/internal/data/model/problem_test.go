package model

import "testing"

func TestSolutionsMetaScanAcceptsLegacyObject(t *testing.T) {
	var got SolutionsMeta
	if err := got.Scan([]byte(`{"name":"滑动窗口","time_complexity":"O(m)"}`)); err != nil {
		t.Fatalf("scan legacy object: %v", err)
	}
	if len(got) != 1 || got[0].Name != "滑动窗口" || got[0].TimeComplexity != "O(m)" {
		t.Fatalf("scan legacy object = %#v", got)
	}
}
