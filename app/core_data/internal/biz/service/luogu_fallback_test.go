package service

import (
	"testing"

	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type credentialRecorder struct {
	username string
	password string
}

func (r *credentialRecorder) SetCredentials(username, password string) {
	r.username = username
	r.password = password
}

func TestShouldUseLuoGuPublicFallbackRequiresSubmitSyncEnabled(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	if !shouldUseLuoGuPublicFallback(rdb, spider.LuoGu, false) {
		t.Fatal("missing credentials with submit sync enabled should use public fallback")
	}
	if err := task.SetPlatformPaused(rdb, spider.LuoGu, true); err != nil {
		t.Fatal(err)
	}
	if shouldUseLuoGuPublicFallback(rdb, spider.LuoGu, false) {
		t.Fatal("paused submit sync must not use public fallback")
	}
	if shouldUseLuoGuPublicFallback(rdb, spider.LuoGu, true) {
		t.Fatal("configured credentials must not use public fallback")
	}
	if shouldUseLuoGuPublicFallback(rdb, spider.QOJ, false) {
		t.Fatal("public fallback is LuoGu-only")
	}
	if shouldUseLuoGuPublicFallback(nil, spider.LuoGu, false) {
		t.Fatal("unknown submit sync state must not use public fallback")
	}
	mr.Close()
	if shouldUseLuoGuPublicFallback(rdb, spider.LuoGu, false) {
		t.Fatal("unreadable submit sync state must not use public fallback")
	}
}

func TestApplyOjCredentialsClearsStaleCredentialsWithoutRuntime(t *testing.T) {
	luogu := &credentialRecorder{username: "stale-user", password: "stale-password"}
	qoj := &credentialRecorder{username: "stale-user", password: "stale-password"}

	applyOjCredentials(nil, luogu, qoj)
	if luogu.username != "" || luogu.password != "" {
		t.Fatalf("LuoGu stale credentials retained: %+v", luogu)
	}
	if qoj.username != "" || qoj.password != "" {
		t.Fatalf("QOJ stale credentials retained: %+v", qoj)
	}

	applyOjCredentials(&sitesettings.Runtime{
		OjLuoguUsername: "luogu-user", OjLuoguPassword: "luogu-password",
		OjQojUsername: "qoj-user", OjQojPassword: "qoj-password",
	}, luogu, qoj)
	if luogu.username != "luogu-user" || luogu.password != "luogu-password" {
		t.Fatalf("LuoGu credentials = %+v", luogu)
	}
	if qoj.username != "qoj-user" || qoj.password != "qoj-password" {
		t.Fatalf("QOJ credentials = %+v", qoj)
	}
}
