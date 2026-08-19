package platform

import (
	"os"
	"testing"
)

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