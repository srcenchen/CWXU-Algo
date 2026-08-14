package service

import (
	"errors"
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestValidateAvatarReferenceRequiresExistingOwnedObject(t *testing.T) {
	const own = "/avatar/120/hash.jpg"
	checks := 0
	exists := func(key string) (bool, error) {
		checks++
		return key == own, nil
	}
	if err := validateAvatarReference(120, own, exists); err != nil {
		t.Fatalf("own existing avatar rejected: %v", err)
	}
	if err := validateAvatarReference(120, "/avatar/121/hash.jpg", exists); err == nil {
		t.Fatal("another user's avatar must be rejected")
	}
	if err := validateAvatarReference(120, "/avatar/120/missing.jpg", exists); err == nil {
		t.Fatal("missing avatar must be rejected")
	}
	if err := validateAvatarReference(120, "https://external.example/avatar.jpg", exists); err == nil {
		t.Fatal("external avatar must be rejected")
	}
	if checks != 2 {
		t.Fatalf("storage existence checks = %d, want 2", checks)
	}

	storageErr := errors.New("storage unavailable")
	if err := validateAvatarReference(120, own, func(string) (bool, error) { return false, storageErr }); !errors.Is(err, storageErr) {
		t.Fatalf("storage error = %v, want %v", err, storageErr)
	}
}

func TestResolveAvatarChangePreservesOmittedAndEquivalentReferences(t *testing.T) {
	oldManaged := "/avatar/120/hash.jpg"
	checks := 0
	exists := func(string) (bool, error) {
		checks++
		return true, nil
	}

	for _, tc := range []struct {
		name      string
		old       string
		requested string
	}{
		{name: "omitted", old: oldManaged},
		{name: "same managed object", old: oldManaged, requested: "https://cdn.example/avatar/120/hash.jpg"},
		{name: "same legacy external url", old: "https://q1.qlogo.cn/avatar.jpg", requested: "https://q1.qlogo.cn/avatar.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, err := resolveAvatarChange(120, tc.old, tc.requested, false, exists)
			if err != nil || changed || got != tc.old {
				t.Fatalf("got=%q changed=%v err=%v, want old reference unchanged", got, changed, err)
			}
		})
	}
	if checks != 0 {
		t.Fatalf("equivalent references performed %d storage checks", checks)
	}
}

func TestResolveAvatarChangeValidatesNewReferenceAndClearsExplicitly(t *testing.T) {
	const old = "/avatar/120/old.jpg"
	got, changed, err := resolveAvatarChange(120, old, "/avatar/120/new.jpg", false, func(key string) (bool, error) {
		return key == "/avatar/120/new.jpg", nil
	})
	if err != nil || !changed || got != "/avatar/120/new.jpg" {
		t.Fatalf("new reference: got=%q changed=%v err=%v", got, changed, err)
	}

	got, changed, err = resolveAvatarChange(120, old, "", true, nil)
	if err != nil || !changed || got != "" {
		t.Fatalf("clear: got=%q changed=%v err=%v", got, changed, err)
	}
}

func TestStaleAvatarObjectKeyOnlyReturnsOwnedDifferentObject(t *testing.T) {
	oldURL := "https://old.example/avatar/120/old.jpg"
	if got := staleAvatarObjectKey(120, oldURL, "/avatar/120/new.jpg"); got != "/avatar/120/old.jpg" {
		t.Fatalf("stale key = %q", got)
	}
	if got := staleAvatarObjectKey(120, oldURL, "/avatar/120/old.jpg"); got != "" {
		t.Fatalf("same object must not be deleted: %q", got)
	}
	if got := staleAvatarObjectKey(120, "/avatar/121/shared.jpg", "/avatar/120/new.jpg"); got != "" {
		t.Fatalf("another user's object must not be deleted: %q", got)
	}
}

func TestAvatarObjectReferencedDetectsCanonicalAndAbsoluteReferences(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	users := []model.User{
		{Username: "canonical", Email: "canonical@example.com", Password: "hash", Avatar: "/avatar/120/shared.jpg"},
		{Username: "absolute", Email: "absolute@example.com", Password: "hash", Avatar: "https://old.example/avatar/121/shared.jpg"},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"/avatar/120/shared.jpg", "/avatar/121/shared.jpg"} {
		referenced, err := avatarObjectReferenced(db, key)
		if err != nil {
			t.Fatal(err)
		}
		if !referenced {
			t.Fatalf("reference not found for %s", key)
		}
	}
	referenced, err := avatarObjectReferenced(db, "/avatar/122/missing.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if referenced {
		t.Fatal("unreferenced key reported as referenced")
	}
}

func TestLocalAvatarRelPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/api/user/static/avatar/27/20260730_ab.jpg", "avatar/27/20260730_ab.jpg"},
		{"/v1/user/static/avatar/27/20260730_ab.jpg", "avatar/27/20260730_ab.jpg"},
		{"https://algo.zhiyuansofts.cn/api/user/static/avatar/27/x.jpg", "avatar/27/x.jpg"},
		{"", ""},
		{"/api/user/static/site/27/x.jpg", ""},
		{"/avatar/27/a1b2.jpg", ""},
		{"/api/user/static/avatar/../etc/passwd", ""},
	}
	for _, c := range cases {
		if got := localAvatarRelPath(c.in); got != c.want {
			t.Errorf("localAvatarRelPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAvatarForStoreService(t *testing.T) {
	if got := normalizeAvatarForStore("https://zhiyuansofts.cn/avatar/27/a1b2.jpg"); got != "/avatar/27/a1b2.jpg" {
		t.Errorf("normalize full url: %s", got)
	}
	if got := normalizeAvatarForStore("/api/user/static/avatar/27/x.jpg"); got != "/api/user/static/avatar/27/x.jpg" {
		t.Errorf("local path must pass through: %s", got)
	}
	if got := normalizeAvatarForStore("https://example.com/a.png"); got != "https://example.com/a.png" {
		t.Errorf("external url must pass through: %s", got)
	}
}

func TestExpandAvatarBase(t *testing.T) {
	base := "https://cdn.example.com"
	if got := expandAvatarBase(base, "/avatar/27/a1b2.jpg"); got != "https://cdn.example.com/avatar/27/a1b2.jpg" {
		t.Errorf("expand: %s", got)
	}
	if got := expandAvatarBase(base, "/api/user/static/avatar/27/x.jpg"); got != "/api/user/static/avatar/27/x.jpg" {
		t.Errorf("local path pass-through: %s", got)
	}
	if got := expandAvatarBase("", "/avatar/27/a1b2.jpg"); got != "/avatar/27/a1b2.jpg" {
		t.Errorf("empty base: %s", got)
	}
}
