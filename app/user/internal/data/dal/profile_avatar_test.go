package dal

import (
	"context"
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newProfileAvatarTestDal(t *testing.T) (*ProfileDal, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewProfileDalRaw(db, rdb), db
}

func TestUpdateAvatarEmailPreservesAvatarWhenAvatarIsOmitted(t *testing.T) {
	d, db := newProfileAvatarTestDal(t)
	user := model.User{Username: "avatar-user", Email: "avatar@example.com", Password: "hash", Avatar: "/avatar/38/original.jpg"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.UpdateAvatarEmail(context.Background(), model.User{ID: user.ID, Email: user.Email}, false, false); err != nil {
		t.Fatal(err)
	}

	var got model.User
	if err := db.First(&got, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Avatar != user.Avatar {
		t.Fatalf("avatar was cleared: got %q, want %q", got.Avatar, user.Avatar)
	}
}

func TestUpdateAvatarEmailWritesProvidedAvatar(t *testing.T) {
	d, db := newProfileAvatarTestDal(t)
	user := model.User{Username: "avatar-change", Email: "avatar-change@example.com", Password: "hash", Avatar: "/avatar/39/old.jpg"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	const next = "/avatar/39/new.jpg"
	if err := d.UpdateAvatarEmail(context.Background(), model.User{ID: user.ID, Email: user.Email, Avatar: next}, false, true); err != nil {
		t.Fatal(err)
	}

	var got model.User
	if err := db.First(&got, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Avatar != next {
		t.Fatalf("avatar not updated: got %q, want %q", got.Avatar, next)
	}
}

func TestUpdateAvatarEmailClearsAvatarWhenExplicitlyRequested(t *testing.T) {
	d, db := newProfileAvatarTestDal(t)
	user := model.User{Username: "avatar-clear", Email: "avatar-clear@example.com", Password: "hash", Avatar: "/avatar/40/old.jpg"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.UpdateAvatarEmail(context.Background(), model.User{ID: user.ID, Email: user.Email}, false, true); err != nil {
		t.Fatal(err)
	}

	var got model.User
	if err := db.First(&got, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Avatar != "" {
		t.Fatalf("avatar not cleared: got %q", got.Avatar)
	}
}

func TestUpdateAvatarEmailPreservesAvatarWhenEmptyIsNotExplicit(t *testing.T) {
	d, db := newProfileAvatarTestDal(t)
	user := model.User{Username: "avatar-implicit", Email: "avatar-implicit@example.com", Password: "hash", Avatar: "/avatar/41/old.jpg"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.UpdateAvatarEmail(context.Background(), model.User{ID: user.ID, Email: user.Email}, false, false); err != nil {
		t.Fatal(err)
	}

	var got model.User
	if err := db.First(&got, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Avatar != user.Avatar {
		t.Fatalf("avatar was implicitly cleared: got %q, want %q", got.Avatar, user.Avatar)
	}
}
