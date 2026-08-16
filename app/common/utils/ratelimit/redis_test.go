package ratelimit

import (
	"strings"
	"testing"
)

func TestSpiderSetKeyUsesOJAccountIdentity(t *testing.T) {
	key := SpiderSetKey("CodeForces", "tourist")

	if key != SpiderSetKey("CodeForces", "tourist") {
		t.Fatal("same OJ account should share a rate limit key")
	}
	if key == SpiderSetKey("CodeForces", "Petr") {
		t.Fatal("different usernames should use different rate limit keys")
	}
	if key == SpiderSetKey("AtCoder", "tourist") {
		t.Fatal("same username on different platforms should use different rate limit keys")
	}
	if strings.Contains(key, "tourist") {
		t.Fatal("rate limit key should not expose the OJ username")
	}
}
