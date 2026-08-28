package platform

import (
	"os"
	"testing"
)

func TestLuoGuFullFetchPlanDoesNotTruncateAtTwoHundredPages(t *testing.T) {
	pages, complete := luoguFullFetchPlan(4001, 20)
	if pages != 201 || !complete {
		t.Fatalf("pages=%d complete=%v", pages, complete)
	}
	pages, complete = luoguFullFetchPlan(4000, 20)
	if pages != 200 || !complete {
		t.Fatalf("pages=%d complete=%v", pages, complete)
	}
}

func TestValidateLuoGuRecordsRejectsDuplicateAndMissingIDs(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		records []Record
	}{
		{name: "duplicate pages", total: 3, records: []Record{{ID: 101}, {ID: 102}, {ID: 101}}},
		{name: "missing row", total: 3, records: []Record{{ID: 101}, {ID: 102}}},
		{name: "non-positive id", total: 2, records: []Record{{ID: 101}, {ID: 0}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateLuoGuRecords(tt.total, tt.records); err == nil {
				t.Fatal("incomplete unique submit IDs must fail")
			}
		})
	}
}

func TestValidateLuoGuRecordPageRejectsInconsistentMetadataAndEmptyMiddlePage(t *testing.T) {
	first := Injection{}
	first.CurrentData.Records.Count = 3
	first.CurrentData.Records.PerPage = 2
	first.CurrentData.Records.Result = []Record{{ID: 101}, {ID: 102}}

	tests := []struct {
		name string
		page int
		inj  Injection
	}{
		{name: "count changed", page: 2, inj: func() Injection {
			v := first
			v.CurrentData.Records.Count = 4
			v.CurrentData.Records.Result = []Record{{ID: 103}}
			return v
		}()},
		{name: "perPage changed", page: 2, inj: func() Injection {
			v := first
			v.CurrentData.Records.PerPage = 1
			v.CurrentData.Records.Result = []Record{{ID: 103}}
			return v
		}()},
		{name: "empty middle page", page: 2, inj: func() Injection {
			v := first
			v.CurrentData.Records.Result = nil
			return v
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateLuoGuRecordPage(first.CurrentData.Records.Count, first.CurrentData.Records.PerPage, tt.page, &tt.inj); err == nil {
				t.Fatal("inconsistent page must fail")
			}
		})
	}
}

// TestParseLuoGuHTML 锁定当前洛谷 record 页 _feInjection 解析契约（2026-08-19 实时抓取）。
func TestParseLuoGuHTML(t *testing.T) {
	raw, err := os.ReadFile("testdata/luogu_record_page.html")
	if err != nil {
		t.Fatal(err)
	}
	lg := &NewLuoGu{}
	inj, err := lg.parseLuoGuHTML(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if inj.Code != 200 {
		t.Fatalf("code=%d, want 200", inj.Code)
	}
	if inj.CurrentData.Records.PerPage <= 0 {
		t.Fatalf("perPage=%d, want >0", inj.CurrentData.Records.PerPage)
	}
	if len(inj.CurrentData.Records.Result) == 0 {
		t.Fatal("result empty, want >0 records")
	}
	first := inj.CurrentData.Records.Result[0]
	if first.ID == 0 {
		t.Fatal("first record id is 0")
	}
	if first.Problem.Pid == "" {
		t.Fatal("first record problem pid empty")
	}
}

// TestParseLuoGuHTMLLoginPageRejected 登录页/未登录跳转页必须报错，不能静默当作空数据。
func TestParseLuoGuHTMLLoginPageRejected(t *testing.T) {
	lg := &NewLuoGu{}
	if _, err := lg.parseLuoGuHTML("<html><title>登录 - 洛谷</title></html>"); err == nil {
		t.Fatal("login page must fail to parse, got nil error")
	}
}
