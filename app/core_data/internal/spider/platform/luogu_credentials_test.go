package platform

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cwxu-algo/app/core_data/internal/spider"
)

func TestFetchSubmitLogEmptyUsernameSkipsLogin(t *testing.T) {
	lg := &NewLuoGu{}
	lg.SetCredentials("configured-user", "configured-password")

	_, _, err := lg.FetchSubmitLogComplete(context.Background(), 91, "  ", false)
	if err == nil || !strings.Contains(err.Error(), "username 为空") {
		t.Fatalf("empty username error = %v, want username validation", err)
	}
	if !errors.Is(err, spider.ErrEmptyPlatformUsername) {
		t.Fatalf("empty username error = %v, want binding error sentinel", err)
	}
}

func TestCachedClientBecomesUnusableWhenCredentialsAreCleared(t *testing.T) {
	client := &http.Client{}
	lg := &NewLuoGu{client: client, username: "configured-user", password: "configured-password"}
	if !lg.cachedClientStillUsable(client) {
		t.Fatal("current client with credentials should be usable")
	}

	lg.SetCredentials("", "")
	if lg.cachedClientStillUsable(client) {
		t.Fatal("cleared credentials must invalidate an in-flight cached client")
	}
}

func TestOlderCredentialSnapshotCannotRestoreClearedCredentials(t *testing.T) {
	lg := &NewLuoGu{}
	lg.SetCredentialsVersioned("", "", 2)
	lg.SetCredentialsVersioned("stale-user", "stale-password", 1)

	if lg.HasLoginCredentials() {
		t.Fatal("older runtime snapshot restored cleared credentials")
	}

	lg.SetCredentialsVersioned("current-user", "current-password", 3)
	lg.SetCredentialsVersioned("", "", 0)
	if lg.HasLoginCredentials() {
		t.Fatal("unreadable runtime must fail safe by clearing credentials")
	}
}
