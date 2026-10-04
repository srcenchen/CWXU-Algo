package service

import (
	"context"
	"strings"
	"testing"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/task"
)

func TestSyncSubjectKeepsLuoguKeysAndIsolatesQOJ(t *testing.T) {
	if syncSubject("LuoGu", "2245873") != "2245873" || syncSubject("", "2245873") != "2245873" {
		t.Fatal("Luogu subject changed")
	}
	if syncSubject("QOJ", "alice_01") != "QOJ:alice_01" {
		t.Fatal("QOJ subject was not isolated")
	}
}

func TestQOJBrowserSyncDoesNotRequireExistingBinding(t *testing.T) {
	svc, db, rdb, clock, importer := newLuoguSyncServiceTest(t)
	svc.luoguTokenValidator = &fakeLuoguValidator{identity: luoguPluginIdentity{
		AuthorizationID: 21, UserID: 7, LuoguUID: "alice_01", ClientKind: "userscript",
		ClientVersion: "0.2.0", Platform: "QOJ",
	}}
	if err := rdb.Set(context.Background(), task.GenerationKey(7, "QOJ"), 3, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	started, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "qoj-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "0.2.0", RequestId: strings.Repeat("q", 43), Platform: "QOJ", OjUid: "alice_01",
	})
	if err != nil || started.Platform != "QOJ" || started.OjUid != "alice_01" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	var binding model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, spiderregistry.QOJ).First(&binding).Error; err != nil || binding.Username != "alice_01" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if active, _ := rdb.Get(context.Background(), luoguSyncActiveKey(7, "QOJ:alice_01")).Result(); active != started.SessionId {
		t.Fatalf("qoj active=%q", active)
	}
	if n, _ := rdb.Exists(context.Background(), luoguSyncActiveKey(7, "LuoGu:alice_01"), luoguSyncActiveKey(7, "alice_01")).Result(); n != 0 {
		t.Fatal("QOJ session used a Luogu active key")
	}

	record := &spiderpb.LuoguSyncRecord{
		SubmitId: "101", SubmitTime: clock.Now().Unix(), Status: 0, Language: 1,
		Verdict: "Accepted", LanguageName: "C++", SubmitTimeText: "2026-08-24 12:00:00",
		Problem: &spiderpb.LuoguSyncProblem{Pid: "19004", Title: "#19004. Local Maxima"},
	}
	uploaded, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "alice_01", Page: 1, RemoteCount: 1, PerPage: 1, Platform: "QOJ", Records: []*spiderpb.LuoguSyncRecord{record},
	})
	if err != nil || !uploaded.Done || uploaded.CompletionReason != "remote_end" || uploaded.Platform != "QOJ" {
		t.Fatalf("upload=%+v err=%v", uploaded, err)
	}
	if importer.calls != 1 {
		t.Fatalf("imports=%d", importer.calls)
	}

	svc.luoguTokenValidator = &fakeLuoguValidator{identity: luoguPluginIdentity{
		AuthorizationID: 22, UserID: 7, LuoguUID: "bob", ClientKind: "userscript", Platform: "QOJ",
	}}
	_, mismatch := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "other-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "0.2.0", RequestId: strings.Repeat("b", 43), Platform: "QOJ",
	})
	if luoguReason(mismatch) != "QOJ_ACCOUNT_MISMATCH" {
		t.Fatalf("mismatch=%v", mismatch)
	}
}

func TestLuoguBrowserSyncStillUsesLegacyActiveKey(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	active, err := rdb.Get(context.Background(), luoguSyncActiveKey(7, "2245873")).Result()
	if err != nil || active != started.SessionId {
		t.Fatalf("active=%q err=%v", active, err)
	}
	if n, _ := rdb.Exists(context.Background(), luoguSyncActiveKey(7, "LuoGu:2245873")).Result(); n != 0 {
		t.Fatal("Luogu session moved to a platform-prefixed key")
	}
}

func TestQOJPageRejectsLuoguNumericStatus(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	svc.luoguTokenValidator = &fakeLuoguValidator{identity: luoguPluginIdentity{
		AuthorizationID: 23, UserID: 9, LuoguUID: "carol", ClientKind: "userscript", Platform: "QOJ",
	}}
	if err := svc.rdb.Set(context.Background(), task.GenerationKey(9, "QOJ"), 1, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	started, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "carol-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "0.2.0", RequestId: strings.Repeat("c", 43), Platform: "QOJ",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "carol", Page: 1, RemoteCount: 1, PerPage: 1, Platform: "QOJ",
		Records: []*spiderpb.LuoguSyncRecord{{
			SubmitId: "8", SubmitTime: clock.Now().Unix(), Status: 12, Language: 1,
			Problem: &spiderpb.LuoguSyncProblem{Pid: "1", Title: "#1. A"},
		}},
	})
	if luoguReason(err) != "QOJ_LAYOUT_CHANGED" {
		t.Fatalf("err=%v", err)
	}
}
