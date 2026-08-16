package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"cwxu-algo/app/user/internal/data/model"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Org{}, &model.OrgMember{}, &model.Group{}, &model.Role{}, &model.UserRole{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Seed public org + default group + system roles (mirrors seedGoAlgoFramework + seedRbac).
	now := timeNow()
	db.Create(&model.Org{
		ID: 1, Slug: model.PublicOrgSlug, Name: model.PublicOrgName,
		Status: model.OrgStatusActive, IsSystem: true, JoinMode: model.OrgJoinAuto,
		CreatedAt: now, UpdatedAt: now,
	})
	name := model.DefaultGroupName
	db.Create(&model.Group{ID: 1, Name: &name, Describe: model.DefaultGroupDesc, OrgID: 1, CreatedAt: now, UpdatedAt: now})
	db.Create(&model.Role{ID: 1, Code: "site_admin", Name: "站点管理员", Scope: "site", IsSystem: true, CreatedAt: now, UpdatedAt: now})
	db.Create(&model.Role{ID: 2, Code: "member", Name: "成员", Scope: "org", IsSystem: true, CreatedAt: now, UpdatedAt: now})
	return db
}

func TestHashPasswordMatchesLoginContract(t *testing.T) {
	hashed, err := HashPassword("secret-password")
	if err != nil {
		t.Fatal(err)
	}
	// Login sends SHA256(plain) as hex; stored must be bcrypt of the lowercase hex.
	digest := sha256.Sum256([]byte("secret-password"))
	clientHash := strings.ToLower(hex.EncodeToString(digest[:]))
	if err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(clientHash)); err != nil {
		t.Fatalf("stored hash must verify against client SHA256 hex: %v", err)
	}
}

func TestCreateFirstAdminSuccess(t *testing.T) {
	db := openTestDB(t)
	cfg := &AdminConfig{Username: "admin", Email: "admin@example.com", Name: "系统管理员", Password: "password123"}
	id, err := CreateFirstAdmin(db, cfg, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("user id must be non-zero")
	}
	var user model.User
	if err := db.First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	if !user.IsSiteAdmin || user.RoleID != 1 || user.CurrentOrgID != 1 || user.GroupId != 1 {
		t.Fatalf("admin flags wrong: %+v", user)
	}
	if user.Email != "admin@example.com" {
		t.Fatalf("email must be lowercased: %s", user.Email)
	}
	var member model.OrgMember
	if err := db.Where("user_id = ?", id).First(&member).Error; err != nil {
		t.Fatal(err)
	}
	if member.Role != model.OrgRoleMember || member.GroupID == nil || *member.GroupID != 1 {
		t.Fatalf("member row wrong: %+v", member)
	}
	var roles []model.UserRole
	if err := db.Where("user_id = ?", id).Order("role_id").Find(&roles).Error; err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 user_roles, got %d", len(roles))
	}
	if !(roles[0].RoleID == 1 && roles[0].OrgID == 0) || !(roles[1].RoleID == 2 && roles[1].OrgID == 1) {
		t.Fatalf("role assignments wrong: %+v", roles)
	}
}

func TestCreateFirstAdminRefusesSecondAdmin(t *testing.T) {
	db := openTestDB(t)
	cfg := &AdminConfig{Username: "admin", Email: "a@example.com", Name: "管理员", Password: "password123"}
	if _, err := CreateFirstAdmin(db, cfg, nil); err != nil {
		t.Fatalf("first create: %v", err)
	}
	cfg2 := &AdminConfig{Username: "admin2", Email: "b@example.com", Name: "管理员二", Password: "password123"}
	if _, err := CreateFirstAdmin(db, cfg2, nil); err != nil {
		if err != errExists {
			t.Fatalf("expected errExists, got %v", err)
		}
	} else {
		t.Fatal("second admin must be refused")
	}
}

func TestCreateFirstAdminRefusesWhenPublicOrgMissing(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&model.User{}, &model.Org{}, &model.OrgMember{}, &model.Group{}, &model.Role{}, &model.UserRole{})
	cfg := &AdminConfig{Username: "admin", Email: "a@example.com", Name: "管理员", Password: "password123"}
	if _, err := CreateFirstAdmin(db, cfg, nil); err == nil {
		t.Fatal("expected error when public org missing")
	}
}

func TestLoadAdminConfigValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.env")
	if err := os.WriteFile(path, []byte("ADMIN_USERNAME=admin\nADMIN_EMAIL=A@Example.COM\nADMIN_PASSWORD=password123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAdminConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Username != "admin" || cfg.Email != "a@example.com" || cfg.Name != "admin" {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
}

func TestLoadAdminConfigRejectsWideOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.env")
	if err := os.WriteFile(path, []byte("ADMIN_PASSWORD=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdminConfig(path); err == nil {
		t.Fatal("expected error for non-0600 config")
	}
}

func TestLoadAdminConfigRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link.env")
	if err := os.WriteFile(target, []byte("ADMIN_PASSWORD=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAdminConfig(link); err == nil {
		t.Fatal("expected error for symlink config")
	}
}
