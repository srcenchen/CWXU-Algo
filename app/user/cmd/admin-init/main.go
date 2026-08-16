package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/user/internal/data/model"
)

var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)
	errExists  = errors.New("已存在站点管理员，拒绝创建")
)

// AdminConfig 首个管理员输入（仅来自受保护的配置文件）。
type AdminConfig struct {
	Username string
	Email    string
	Name     string
	Password string
}

// LoadAdminConfig 解析受保护的 KEY=VALUE 配置文件。
// 要求：普通文件、非符号链接、权限 0600。
func LoadAdminConfig(path string) (*AdminConfig, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("读取管理员配置文件：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("管理员配置文件不能是符号链接")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("管理员配置文件必须是普通文件")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("管理员配置文件权限必须为 0600，当前为 %o", info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &AdminConfig{}
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("无效配置行：%q", line)
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	cfg.Username = values["ADMIN_USERNAME"]
	cfg.Email = values["ADMIN_EMAIL"]
	cfg.Name = values["ADMIN_NAME"]
	cfg.Password = values["ADMIN_PASSWORD"]
	if cfg.Name == "" {
		cfg.Name = cfg.Username
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *AdminConfig) validate() error {
	if !usernameRe.MatchString(c.Username) {
		return errors.New("用户名必须为 3-64 位字母/数字/下划线/短横线")
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" || len(email) > 320 || !strings.Contains(email, "@") {
		return errors.New("邮箱格式不正确")
	}
	c.Email = email
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = c.Username
	}
	if utf8.RuneCountInString(c.Name) > 64 {
		return errors.New("展示名不能超过 64 个字符")
	}
	if len(c.Password) < 8 {
		return errors.New("密码长度至少 8 位")
	}
	return nil
}

// HashPassword 生成 bcrypt(小写SHA256十六进制)，与登录校验保持一致。
func HashPassword(plain string) (string, error) {
	digest := sha256.Sum256([]byte(plain))
	hexDigest := strings.ToLower(hex.EncodeToString(digest[:]))
	hashed, err := bcrypt.GenerateFromPassword([]byte(hexDigest), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt 密码哈希：%w", err)
	}
	return string(hashed), nil
}

// LockFunc 在事务内获取 PostgreSQL 应用级锁；测试可传 nil 跳过。
type LockFunc func(tx *gorm.DB) error

func PGAdvisoryLock(tx *gorm.DB) error {
	const key = 0x476F616C676F
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", key).Error
}

// CreateFirstAdmin 在单个事务内创建首个站点管理员。
func CreateFirstAdmin(db *gorm.DB, cfg *AdminConfig, lock LockFunc) (uint, error) {
	hashed, err := HashPassword(cfg.Password)
	if err != nil {
		return 0, err
	}
	var createdID uint
	err = db.Transaction(func(tx *gorm.DB) error {
		if lock != nil {
			if err := lock(tx); err != nil {
				return fmt.Errorf("获取应用级锁：%w", err)
			}
		}
		if err := ensureNoAdmin(tx); err != nil {
			return err
		}
		publicOrg, err := loadPublicOrg(tx)
		if err != nil {
			return err
		}
		defaultGroupID, err := ensureDefaultGroup(tx, publicOrg.ID)
		if err != nil {
			return err
		}
		siteAdminRole, err := loadSystemRole(tx, rbac.RoleSiteAdmin, "site")
		if err != nil {
			return err
		}
		memberRole, err := loadSystemRole(tx, rbac.RoleMember, "org")
		if err != nil {
			return err
		}
		if err := ensureUsernameEmailFree(tx, cfg.Username, cfg.Email); err != nil {
			return err
		}
		user := model.User{
			Username:           cfg.Username,
			Password:           hashed,
			Name:               cfg.Name,
			Email:              cfg.Email,
			GroupId:            int64(defaultGroupID),
			RoleID:             int(permissionRoleAdmin),
			IsSiteAdmin:        true,
			CurrentOrgID:       publicOrg.ID,
			AllowPublicProfile: true,
			AllowPublicFeed:    true,
		}
		if err := tx.Create(&user).Error; err != nil {
			return fmt.Errorf("创建用户：%w", err)
		}
		groupID := defaultGroupID
		member := model.OrgMember{
			OrgID:          publicOrg.ID,
			UserID:         user.ID,
			Role:           model.OrgRoleMember,
			GroupID:        &groupID,
			OrgDisplayName: cfg.Name,
			JoinedAt:       timeNow(),
		}
		if err := tx.Create(&member).Error; err != nil {
			return fmt.Errorf("创建公共域成员：%w", err)
		}
		siteRole := model.UserRole{UserID: user.ID, RoleID: siteAdminRole.ID, OrgID: 0}
		if err := tx.Create(&siteRole).Error; err != nil {
			return fmt.Errorf("写入站点管理员角色：%w", err)
		}
		memberRoleRow := model.UserRole{UserID: user.ID, RoleID: memberRole.ID, OrgID: publicOrg.ID}
		if err := tx.Create(&memberRoleRow).Error; err != nil {
			return fmt.Errorf("写入公共域成员角色：%w", err)
		}
		createdID = user.ID
		return nil
	})
	if err != nil {
		return 0, err
	}
	return createdID, nil
}

const permissionRoleAdmin = 1

func ensureNoAdmin(tx *gorm.DB) error {
	var count int64
	if err := tx.Model(&model.User{}).
		Where("is_site_admin = ? OR role_id = ?", true, permissionRoleAdmin).
		Count(&count).Error; err != nil {
		return fmt.Errorf("检查已有管理员：%w", err)
	}
	if count > 0 {
		return errExists
	}
	var siteRole model.Role
	if err := tx.Where("code = ? AND scope = ?", rbac.RoleSiteAdmin, "site").
		First(&siteRole).Error; err != nil {
		return fmt.Errorf("站点管理员角色未初始化：%w", err)
	}
	var roleCount int64
	if err := tx.Model(&model.UserRole{}).
		Where("role_id = ? AND org_id = ?", siteRole.ID, 0).
		Count(&roleCount).Error; err != nil {
		return fmt.Errorf("检查已有管理员角色：%w", err)
	}
	if roleCount > 0 {
		return errExists
	}
	return nil
}

func loadPublicOrg(tx *gorm.DB) (*model.Org, error) {
	var org model.Org
	if err := tx.Where("slug = ? AND is_system = ?", model.PublicOrgSlug, true).
		First(&org).Error; err != nil {
		return nil, fmt.Errorf("公共域未初始化，请先启动 user 服务一次：%w", err)
	}
	if org.Status != model.OrgStatusActive {
		return nil, fmt.Errorf("公共域状态异常：%s", org.Status)
	}
	if org.ID == 0 {
		return nil, errors.New("公共域 ID 无效")
	}
	return &org, nil
}

func ensureDefaultGroup(tx *gorm.DB, orgID uint) (uint, error) {
	var group model.Group
	err := tx.Where("org_id = ?", orgID).
		Where("name IN ?", []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").First(&group).Error
	if err == nil {
		return group.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("查询默认分组：%w", err)
	}
	name := model.DefaultGroupName
	group = model.Group{Name: &name, Describe: model.DefaultGroupDesc, OrgID: orgID}
	if err := tx.Create(&group).Error; err != nil {
		return 0, fmt.Errorf("创建默认分组：%w", err)
	}
	return group.ID, nil
}

func loadSystemRole(tx *gorm.DB, code, scope string) (*model.Role, error) {
	var role model.Role
	if err := tx.Where("code = ? AND scope = ? AND is_system = ?", code, scope, true).
		First(&role).Error; err != nil {
		return nil, fmt.Errorf("系统角色 %s 未初始化：%w", code, err)
	}
	if role.ID == 0 {
		return nil, fmt.Errorf("系统角色 %s ID 无效", code)
	}
	return &role, nil
}

func ensureUsernameEmailFree(tx *gorm.DB, username, email string) error {
	var count int64
	if err := tx.Model(&model.User{}).
		Where("username = ? OR email = ?", username, email).
		Count(&count).Error; err != nil {
		return fmt.Errorf("检查用户名/邮箱：%w", err)
	}
	if count > 0 {
		return fmt.Errorf("用户名或邮箱已被占用")
	}
	return nil
}

func run() int {
	var adminConfigPath, dsn string
	flags := flag.NewFlagSet("admin-init", flag.ContinueOnError)
	flags.StringVar(&adminConfigPath, "admin-config", "", "管理员配置文件路径（必须 root 所有、0600、非符号链接）")
	flags.StringVar(&dsn, "db-dsn", os.Getenv("USER_DATABASE_DSN"), "PostgreSQL DSN（默认 USER_DATABASE_DSN）")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if adminConfigPath == "" {
		fmt.Fprintln(os.Stderr, "admin-init: 必须提供 --admin-config")
		return 2
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "admin-init: 缺少 USER_DATABASE_DSN")
		return 2
	}
	cfg, err := LoadAdminConfig(adminConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin-init: %v\n", err)
		return 1
	}
	db, err := openDB(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin-init: %v\n", err)
		return 1
	}
	id, err := CreateFirstAdmin(db, cfg, PGAdvisoryLock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin-init: %v\n", err)
		return 1
	}
	if err := os.Remove(adminConfigPath); err != nil {
		fmt.Fprintf(os.Stderr, "admin-init: 管理员已创建(用户ID=%d)，但删除配置文件失败：%v\n", id, err)
		return 1
	}
	fmt.Printf("admin-init: 首个管理员创建成功 username=%s userID=%d\n", cfg.Username, id)
	return 0
}

func main() {
	os.Exit(run())
}
