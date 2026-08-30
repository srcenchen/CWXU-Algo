package dal

import (
	"context"
	"testing"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDeleteUserDeletesPluginAuthorizationsInSameLifecycle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.PluginAuthorization{}, &model.UserFollow{}, &model.OrgMember{}, &model.OrgJoinRequest{}, &model.Paste{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "delete-me", Email: "delete@example.test", Password: "x"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	authz := model.PluginAuthorization{UserID: user.ID, Provider: "luogu", ClientKind: "userscript", ClientVersion: "1.0.0", LuoguUID: "123", TokenHash: "sha256:delete", RiskVersion: "v1", AcceptedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&authz).Error; err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := NewProfileDalRaw(db, rdb).Delete(context.Background(), int64(user.ID)); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.PluginAuthorization{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("authorization count after delete = %d", count)
	}
}
