package platform

import (
	"context"
	"strings"
	"testing"
)

func TestIsNowCoderWAFBody(t *testing.T) {
	waf := []byte(`<!doctypehtml><meta charset="UTF-8"><meta name="aliyun_waf_aa"content="ff926c7f">`)
	if !isNowCoderWAFBody(waf) {
		t.Fatal("expected WAF detect")
	}
	ok := []byte(`<div class="my-state-item"><div class="state-num">727</div><span>Rating</span></div>`)
	if isNowCoderWAFBody(ok) {
		t.Fatal("false positive WAF")
	}
}

func TestFetchRating_NowCoder_NotWAFSilent(t *testing.T) {
	// 与 rating_test 互补：强调「无 err 时必须 hasRating」，防止回归静默 0
	r, has, err := NewNowCoder{}.FetchRating("978880410")
	if err != nil {
		if strings.Contains(err.Error(), "WAF") {
			t.Fatalf("still blocked by WAF: %v", err)
		}
		t.Skipf("network: %v", err)
	}
	if !has {
		t.Fatalf("hasRating=false without error — would zero out stored rating (r=%d)", r)
	}
}

func TestGetSubLogResp_PracticeNotEmpty(t *testing.T) {
	doc, err := getSubLogRespCtx(context.Background(),
		"https://ac.nowcoder.com/acm/contest/profile/978880410/practice-coding?pageSize=50&page=1")
	if err != nil {
		if strings.Contains(err.Error(), "WAF") {
			t.Fatalf("practice-coding WAF: %v", err)
		}
		t.Skipf("network: %v", err)
	}
	subs := parsePracticeCodingTable(doc)
	if len(subs) == 0 {
		t.Fatal("practice-coding returned 0 rows (likely WAF/empty parse)")
	}
	t.Logf("practice rows=%d first=%+v", len(subs), subs[0])
}
