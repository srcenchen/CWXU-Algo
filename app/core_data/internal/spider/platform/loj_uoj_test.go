package platform

import (
	"context"
	"strings"
	"testing"

	"cwxu-algo/app/core_data/internal/spider"
)

func TestLOJ_FetchSubmitLog_IncrementalLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live loj")
	}
	logs, err := NewLOJ{}.FetchSubmitLog(context.Background(), 1, "supy", false)
	if err != nil {
		t.Fatalf("loj incremental: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected some submissions for supy")
	}
	for _, l := range logs {
		if l.Platform != spider.LOJ {
			t.Fatalf("platform=%s", l.Platform)
		}
		if !strings.HasPrefix(l.SubmitID, "loj-") {
			t.Fatalf("submit_id=%s want loj- prefix", l.SubmitID)
		}
		if l.Status == "" {
			t.Fatal("empty status")
		}
	}
	t.Logf("loj incremental n=%d first=%+v", len(logs), logs[0])
}

func TestLOJ_FetchRating_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live loj")
	}
	r, has, err := NewLOJ{}.FetchRating("supy")
	if err != nil {
		t.Fatalf("loj rating: %v", err)
	}
	if !has || r <= 0 {
		t.Fatalf("rating has=%v r=%d", has, r)
	}
	t.Logf("loj rating=%d", r)
}

func TestLOJ_MapStatus(t *testing.T) {
	if mapLOJStatus("Accepted") != "AC" {
		t.Fatal("Accepted")
	}
	if mapLOJStatus("WrongAnswer") != "WA" {
		t.Fatal("WA")
	}
	if mapLOJStatus("Pending") != "Judging" {
		t.Fatal("Pending")
	}
}

func TestUOJ_FetchSubmitLog_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live uoj")
	}
	logs, err := NewUOJ{}.FetchSubmitLog(context.Background(), 42, "lgvc", true)
	if err != nil {
		t.Fatalf("uoj ac list: %v", err)
	}
	if len(logs) < 10 {
		t.Fatalf("expected many ACs for lgvc, got %d", len(logs))
	}
	for _, l := range logs {
		if l.Platform != spider.UOJ {
			t.Fatalf("platform=%s", l.Platform)
		}
		if !strings.HasPrefix(l.SubmitID, "uoj-ac-42-") {
			t.Fatalf("submit_id=%s", l.SubmitID)
		}
		if l.Status != "AC" {
			t.Fatalf("status=%s", l.Status)
		}
		if l.ExternalID == "" {
			t.Fatal("empty external_id")
		}
	}
	t.Logf("uoj ac n=%d sample=%+v", len(logs), logs[0])
}

func TestUOJ_FetchRating_Live(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live uoj")
	}
	r, has, err := NewUOJ{}.FetchRating("lgvc")
	if err != nil {
		t.Fatalf("uoj rating: %v", err)
	}
	if !has || r < 1000 {
		t.Fatalf("rating has=%v r=%d", has, r)
	}
	t.Logf("uoj rating=%d", r)
}

func TestUOJ_ParseACList(t *testing.T) {
	html := `
<div class="list-group-item">
  <h4 class="list-group-item-heading">AC 过的题目：共 2 道题 </h4>
  <ul class="list-group-item-text nav nav-pills uoj-ac-problems-list">
    <li><a href="https://uoj.ac/problem/1">#1. A + B Problem</a></li>
    <li><a href="/problem/7">#7. 【NOI2014】购票</a></li>
  </ul>
</div>`
	got := parseUOJACProblems(html)
	if len(got) != 2 {
		t.Fatalf("n=%d", len(got))
	}
	if got[0].ID != "1" || !strings.Contains(got[0].Title, "A + B") {
		t.Fatalf("%+v", got[0])
	}
	if got[1].ID != "7" {
		t.Fatalf("%+v", got[1])
	}
}

func TestUOJ_ParseRating(t *testing.T) {
	html := `
<div class="list-group-item">
  <h4 class="list-group-item-heading">Rating</h4>
  <p class="list-group-item-text"><strong style="color:red">2341</strong></p>
</div>`
	r, ok := parseUOJRating(html)
	if !ok || r != 2341 {
		t.Fatalf("r=%d ok=%v", r, ok)
	}
}
