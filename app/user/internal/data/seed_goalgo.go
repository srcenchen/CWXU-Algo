package data

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

func randomInviteCode() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// seedGoAlgoFramework 公共域、全员入域、站点标题、旧 role 迁移。
// 管理员必须通过显式运维流程创建，启动时绝不创建弱口令账号或自动提权。
func seedGoAlgoFramework(db *gorm.DB) {
	// 1. 公共域
	var public model.Org
	err := db.Where("slug = ?", model.PublicOrgSlug).First(&public).Error
	if err == gorm.ErrRecordNotFound {
		public = model.Org{
			Name:              model.PublicOrgName,
			Slug:              model.PublicOrgSlug,
			Plan:              "free",
			SeatLimit:         50,
			Status:            model.OrgStatusActive,
			IsSystem:          true,
			JoinMode:          model.OrgJoinAuto,
			InviteCode:        "PUBLIC-" + randomInviteCode(),
			EnableAIEmail:     true,
			EnableSpider:      true,
			SpiderIntervalMin: 60,
			AIEmailSchedule:   "30 7 * * *",
		}
		if e := db.Create(&public).Error; e != nil {
			log.Errorf("seed public org: %v", e)
			return
		}
		log.Infof("seeded system org 公共域 id=%d", public.ID)
	} else if err != nil {
		log.Errorf("query public org: %v", err)
		return
	}

	// 历史 seat_limit=0（旧语义未限制）→ 默认 50
	_ = db.Model(&model.Org{}).Where("seat_limit <= 0 OR seat_limit IS NULL").Update("seat_limit", 50).Error

	// 各组织确保有「默认分组」；旧名「未分组」改名
	_ = db.Model(&model.Group{}).Where("name = ?", "未分组").Updates(map[string]interface{}{
		"name":     model.DefaultGroupName,
		"describe": model.DefaultGroupDesc,
	}).Error

	// 每组织默认分组 + 全员 org_members 挂默认组（见 migrateOrgMembersDefaultGroups）
	migrateOrgMembersDefaultGroups(db)

	// 2. 旧 group 挂到公共域
	_ = db.Model(&model.Group{}).Where("org_id = 0 OR org_id IS NULL").Update("org_id", public.ID).Error

	// 3. 用户：is_site_admin 从 role_id=1；role 2/3 降为 0；全员入公共域
	_ = db.Model(&model.User{}).Where("role_id = ?", 1).Update("is_site_admin", true).Error
	_ = db.Model(&model.User{}).Where("role_id IN ?", []int{2, 3}).Update("role_id", 0).Error

	// 清理指向已删除用户的孤儿 membership / 申请（历史硬删用户未清关联时会出现）
	if res := db.Exec(`
		DELETE FROM org_members m
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = m.user_id)
	`); res.Error != nil {
		log.Errorf("cleanup orphan org_members: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Infof("cleaned orphan org_members: %d", res.RowsAffected)
	}
	if res := db.Exec(`
		DELETE FROM org_join_requests r
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = r.user_id)
	`); res.Error != nil {
		log.Errorf("cleanup orphan org_join_requests: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Infof("cleaned orphan org_join_requests: %d", res.RowsAffected)
	}

	// 公共域默认分组（全员入域时写入 org_members.group_id）
	var pubDefForJoin model.Group
	if db.Where("org_id = ? AND name IN ?", public.ID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").First(&pubDefForJoin).Error != nil {
		defName := model.DefaultGroupName
		pubDefForJoin = model.Group{Name: &defName, Describe: model.DefaultGroupDesc, OrgID: public.ID}
		_ = db.Create(&pubDefForJoin).Error
	}
	pubDefGID := pubDefForJoin.ID

	var users []model.User
	if e := db.Find(&users).Error; e != nil {
		log.Errorf("list users for org migrate: %v", e)
		return
	}
	now := time.Now()
	for _, u := range users {
		var n int64
		db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", public.ID, u.ID).Count(&n)
		if n == 0 {
			display := strings.TrimSpace(u.Name)
			if display == "" {
				display = u.Username
			}
			var gptr *uint
			if pubDefGID > 0 {
				g := pubDefGID
				gptr = &g
			}
			m := model.OrgMember{
				OrgID:          public.ID,
				UserID:         u.ID,
				Role:           model.OrgRoleMember,
				GroupID:        gptr,
				OrgDisplayName: display,
				JoinedAt:       now,
			}
			if e := db.Create(&m).Error; e != nil {
				log.Errorf("ensure public membership user_id=%d: %v", u.ID, e)
			}
		}
		if u.CurrentOrgID == 0 {
			_ = db.Model(&u).Update("current_org_id", public.ID).Error
		}
	}

	// 全员入公共域后再次兜底：补 org_members 默认分组（含刚创建的 membership）
	migrateOrgMembersDefaultGroups(db)

	// 4. 站点标题默认 GoAlgo
	var sc model.SiteConfig
	if e := db.First(&sc, 1).Error; e == gorm.ErrRecordNotFound {
		_ = db.Create(&model.SiteConfig{ID: 1, SiteTitle: "GoAlgo"}).Error
	} else if e == nil && (sc.SiteTitle == "" || sc.SiteTitle == "Algo-CWUX") {
		_ = db.Model(&model.SiteConfig{}).Where("id = ?", 1).Update("site_title", "GoAlgo").Error
	}

	// 5. 无有效 group_id 的用户 → 挂到其 current_org 的默认分组，否则公共域默认分组
	var pubDef model.Group
	if db.Where("org_id = ? AND name IN ?", public.ID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").First(&pubDef).Error != nil {
		defName := model.DefaultGroupName
		pubDef = model.Group{Name: &defName, Describe: model.DefaultGroupDesc, OrgID: public.ID}
		_ = db.Create(&pubDef).Error
	} else if pubDef.Name != nil && *pubDef.Name == "未分组" {
		n := model.DefaultGroupName
		_ = db.Model(&pubDef).Updates(map[string]interface{}{"name": n, "describe": model.DefaultGroupDesc}).Error
	}

	// 按用户 current_org 修正无效 group_id
	type ug struct {
		ID           uint
		GroupID      int64
		CurrentOrgID uint
	}
	var bad []ug
	_ = db.Raw(`
		SELECT u.id, COALESCE(u.group_id, 0) AS group_id, COALESCE(u.current_org_id, 0) AS current_org_id
		FROM users u
		WHERE (
			u.group_id IS NULL OR u.group_id = 0
			OR u.group_id NOT IN (SELECT id FROM groups)
		)
	`).Scan(&bad).Error
	for _, u := range bad {
		orgID := u.CurrentOrgID
		if orgID == 0 {
			orgID = public.ID
		}
		var defG model.Group
		if db.Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
			Order("id ASC").First(&defG).Error != nil {
			defName := model.DefaultGroupName
			defG = model.Group{Name: &defName, Describe: model.DefaultGroupDesc, OrgID: orgID}
			if db.Create(&defG).Error != nil {
				defG.ID = pubDef.ID
			}
		}
		if defG.ID > 0 {
			_ = db.Model(&model.User{}).Where("id = ?", u.ID).Update("group_id", defG.ID).Error
		}
	}

	// 删除历史虚拟「未分组」若存在 id=0 脏数据（一般无此行）
	_ = db.Where("name = ? AND id = 0", "未分组").Delete(&model.Group{}).Error

	// 注意：个人日报/周报偏好（email_enabled / email_weekly_enabled）禁止在 seed 里全表清零。
	// 曾有「一次性迁移默认关」误写成每次启动都 UPDATE，导致用户打开开关后部署/重启又被关。
	// 新用户默认关由列 default:false 保证即可。

	// 7. 姓名语义迁移：users.name → 全局昵称(=username)；旧真实姓名 → 唯一非公共域的组织内名称
	// 幂等：仅当 org_display_name 为空且 name≠username 时拷贝；之后 name 统一为 username
	migrateNameToOrgDisplay(db, public.ID)

	// 8. 公共域称呼 ≡ 全局昵称：空 org_display_name 回填 users.name
	syncPublicOrgDisplayFromName(db, public.ID)

	log.Infof("GoAlgo framework seed done public_org_id=%d users=%d fixed_groups=%d", public.ID, len(users), len(bad))
}

// syncPublicOrgDisplayFromName 将公共域空称呼回填为 users.name（与全局昵称合并）
func syncPublicOrgDisplayFromName(db *gorm.DB, publicOrgID uint) {
	res := db.Exec(`
		UPDATE org_members m
		SET org_display_name = u.name
		FROM users u
		WHERE m.user_id = u.id
		  AND m.org_id = ?
		  AND (m.org_display_name IS NULL OR TRIM(m.org_display_name) = '')
		  AND COALESCE(TRIM(u.name), '') <> ''
	`, publicOrgID)
	n := int64(0)
	if res != nil {
		n = res.RowsAffected
	}
	log.Infof("sync public org_display from name: updated=%d", n)
}

// migrateNameToOrgDisplay 一次性迁移：
// - 用户有且仅有 1 个非公共域 membership 且 org_display_name 为空 → 写入旧 users.name
// - 全部 users.name 改为 username（全局昵称）
func migrateNameToOrgDisplay(db *gorm.DB, publicOrgID uint) {
	type row struct {
		UserID uint
		Name   string
		Uname  string
		Cnt    int64
	}
	var rows []row
	// 每个用户非公共域 membership 数量
	_ = db.Raw(`
		SELECT u.id AS user_id, COALESCE(u.name,'') AS name, COALESCE(u.username,'') AS uname,
			(SELECT COUNT(*) FROM org_members m
			 JOIN orgs o ON o.id = m.org_id
			 WHERE m.user_id = u.id
			   AND o.id <> ? AND COALESCE(o.is_system,false) = false) AS cnt
		FROM users u
	`, publicOrgID).Scan(&rows).Error

	copied := 0
	for _, r := range rows {
		if r.Cnt != 1 {
			continue
		}
		oldName := strings.TrimSpace(r.Name)
		if oldName == "" || oldName == r.Uname {
			continue
		}
		// 找到那一条非公共域 membership
		var m model.OrgMember
		err := db.Table("org_members AS m").
			Joins("JOIN orgs o ON o.id = m.org_id").
			Where("m.user_id = ? AND o.id <> ? AND COALESCE(o.is_system,false) = false",
				r.UserID, publicOrgID).
			Select("m.*").
			First(&m).Error
		if err != nil {
			continue
		}
		if strings.TrimSpace(m.OrgDisplayName) != "" {
			continue
		}
		if db.Model(&model.OrgMember{}).Where("id = ?", m.ID).
			Update("org_display_name", oldName).Error == nil {
			copied++
		}
	}

	// 全局昵称 = 用户名（覆盖旧真实姓名）
	res := db.Exec(`UPDATE users SET name = username WHERE name IS DISTINCT FROM username`)
	nChanged := int64(0)
	if res != nil {
		nChanged = res.RowsAffected
	}
	log.Infof("migrate name→org_display: copied=%d name_to_username=%d", copied, nChanged)
}

// EnsurePublicOrgID 供业务使用
func EnsurePublicOrgID(db *gorm.DB) (uint, error) {
	var o model.Org
	if err := db.Where("slug = ?", model.PublicOrgSlug).First(&o).Error; err != nil {
		return 0, fmt.Errorf("public org missing: %w", err)
	}
	return o.ID, nil
}

// migrateOrgMembersDefaultGroups 幂等迁移：
// 1. 每个组织确保有「默认分组」
// 2. 该组织内 org_members.group_id 为空 / 0 / 指向不存在或不属于本组织的分组 → 挂到默认分组
// 3. 兼容：users.group_id 仍为空时，按 current_org 默认分组回填（旧字段）
// 已在自定义分组的成员不受影响。
func migrateOrgMembersDefaultGroups(db *gorm.DB) {
	var allOrgs []model.Org
	if e := db.Find(&allOrgs).Error; e != nil {
		log.Errorf("migrateOrgMembersDefaultGroups list orgs: %v", e)
		return
	}

	fixedMembers := int64(0)
	for _, o := range allOrgs {
		defID := ensureOrgDefaultGroup(db, o.ID)
		if defID == 0 {
			continue
		}

		// 空 / 0
		res := db.Exec(`
			UPDATE org_members
			SET group_id = ?
			WHERE org_id = ?
			  AND (group_id IS NULL OR group_id = 0)
		`, defID, o.ID)
		if res.Error != nil {
			log.Errorf("migrate org_members null group org_id=%d: %v", o.ID, res.Error)
		} else {
			fixedMembers += res.RowsAffected
		}

		// 指向不存在、或不属于本组织的分组（跨域脏数据）
		res = db.Exec(`
			UPDATE org_members m
			SET group_id = ?
			WHERE m.org_id = ?
			  AND m.group_id IS NOT NULL
			  AND m.group_id > 0
			  AND NOT EXISTS (
				SELECT 1 FROM groups g
				WHERE g.id = m.group_id AND g.org_id = m.org_id
			  )
		`, defID, o.ID)
		if res.Error != nil {
			log.Errorf("migrate org_members invalid group org_id=%d: %v", o.ID, res.Error)
		} else {
			fixedMembers += res.RowsAffected
		}

		// 公共域：旧 users.group_id 仍为空的全员挂公共默认组（兼容读路径）
		if o.IsSystem || o.Slug == model.PublicOrgSlug {
			_ = db.Model(&model.User{}).
				Where("group_id = 0 OR group_id IS NULL").
				Update("group_id", defID).Error
		}
	}

	log.Infof("migrateOrgMembersDefaultGroups: orgs=%d fixed_org_members=%d", len(allOrgs), fixedMembers)
}

// ensureOrgDefaultGroup 确保组织有默认分组，返回 id；失败返回 0
func ensureOrgDefaultGroup(db *gorm.DB, orgID uint) uint {
	if orgID == 0 {
		return 0
	}
	var defG model.Group
	err := db.Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").First(&defG).Error
	if err == nil {
		if defG.Name != nil && *defG.Name == "未分组" {
			n := model.DefaultGroupName
			_ = db.Model(&defG).Updates(map[string]interface{}{
				"name": n, "describe": model.DefaultGroupDesc,
			}).Error
		}
		return defG.ID
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Errorf("ensureOrgDefaultGroup org_id=%d: %v", orgID, err)
		return 0
	}
	defName := model.DefaultGroupName
	defG = model.Group{
		Name:     &defName,
		Describe: model.DefaultGroupDesc,
		OrgID:    orgID,
	}
	if e := db.Create(&defG).Error; e != nil {
		log.Errorf("create default group org_id=%d: %v", orgID, e)
		return 0
	}
	return defG.ID
}
