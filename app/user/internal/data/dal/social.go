package dal

import (
	"cwxu-algo/app/common/utils/sqllike"
	"context"
	"fmt"
	"strings"

	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SocialDal struct {
	db *gorm.DB
}

func NewSocialDal(data *data.Data) *SocialDal {
	return &SocialDal{db: data.DB}
}

// Follow 关注；不可关注自己
func (d *SocialDal) Follow(ctx context.Context, followerID, followeeID uint) error {
	if followerID == 0 || followeeID == 0 || followerID == followeeID {
		return fmt.Errorf("无效的关注目标")
	}
	var n int64
	if err := d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", followeeID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("用户不存在")
	}
	f := model.UserFollow{FollowerID: followerID, FolloweeID: followeeID}
	return d.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error
}

// Unfollow 取消关注
func (d *SocialDal) Unfollow(ctx context.Context, followerID, followeeID uint) error {
	return d.db.WithContext(ctx).
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Delete(&model.UserFollow{}).Error
}

// IsFollowing 是否已关注
func (d *SocialDal) IsFollowing(ctx context.Context, followerID, followeeID uint) bool {
	if followerID == 0 || followeeID == 0 {
		return false
	}
	var n int64
	_ = d.db.WithContext(ctx).Model(&model.UserFollow{}).
		Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Count(&n).Error
	return n > 0
}

// Counts 关注数 / 粉丝数
func (d *SocialDal) Counts(ctx context.Context, userID uint) (following, followers int64, err error) {
	if userID == 0 {
		return 0, 0, nil
	}
	err = d.db.WithContext(ctx).Model(&model.UserFollow{}).
		Where("follower_id = ?", userID).Count(&following).Error
	if err != nil {
		return
	}
	err = d.db.WithContext(ctx).Model(&model.UserFollow{}).
		Where("followee_id = ?", userID).Count(&followers).Error
	return
}

// FollowingIDs 关注的 userId 列表（不分页，供过滤）
func (d *SocialDal) FollowingIDs(ctx context.Context, userID uint) ([]int64, error) {
	if userID == 0 {
		return []int64{}, nil
	}
	var ids []int64
	err := d.db.WithContext(ctx).Model(&model.UserFollow{}).
		Where("follower_id = ?", userID).
		Order("id DESC").
		Pluck("followee_id", &ids).Error
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

// SocialUser 列表项（Name 为解析后的主展示名）
// 注意：InCurrentOrg / SharedOrgs 仅运行时填充，禁止被 GORM Scan 当成列（会 500）。
type SocialUser struct {
	UserID   uint   `gorm:"column:user_id"`
	Username string `gorm:"column:username"`
	Name     string `gorm:"column:name"`
	Avatar   string `gorm:"column:avatar"`
	// 全站特殊身份（公共域 badge 用；列表 SQL 需选出）
	IsSiteAdmin bool `gorm:"column:is_site_admin"`
	// SiteRoles 持有的自定义站点角色名（公共域 badge 用；EnrichDisplay 批量回填）
	SiteRoles []string `gorm:"-"`
	// InCurrentOrg 目标是否属于观众当前组织
	InCurrentOrg bool `gorm:"-"`
	// SharedOrgs 双方共属、且非当前域的组织称呼（含公共域；切换组织后仍返回）
	// 观众不可见的组织绝不出现
	SharedOrgs []SharedOrgAlias `gorm:"-"`
}

// socialUserRow 纯 DB 扫描行（无切片/派生字段，避免 Scan 崩溃）
type socialUserRow struct {
	UserID      uint   `gorm:"column:user_id"`
	Username    string `gorm:"column:username"`
	Name        string `gorm:"column:name"`
	Avatar      string `gorm:"column:avatar"`
	IsSiteAdmin bool   `gorm:"column:is_site_admin"`
}

func rowsToSocialUsers(rows []socialUserRow) []SocialUser {
	out := make([]SocialUser, 0, len(rows))
	for _, r := range rows {
		out = append(out, SocialUser{
			UserID:      r.UserID,
			Username:    r.Username,
			Name:        r.Name,
			Avatar:      r.Avatar,
			IsSiteAdmin: r.IsSiteAdmin,
		})
	}
	return out
}

// SharedOrgAlias 双方共属的其他组织内称呼（隐私边界：观众必须同属该组织）
type SharedOrgAlias struct {
	OrgID       uint   `json:"orgId"`
	OrgName     string `json:"orgName"`
	DisplayName string `json:"displayName"`
}

// ListFollowing 关注列表
func (d *SocialDal) ListFollowing(ctx context.Context, userID uint, page, pageSize int) ([]SocialUser, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	var total int64
	q := d.db.WithContext(ctx).Model(&model.UserFollow{}).Where("follower_id = ?", userID)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []socialUserRow
	err := d.db.WithContext(ctx).Table("user_follows f").
		Select("u.id AS user_id, u.username, u.name, u.avatar, u.is_site_admin").
		Joins("JOIN users u ON u.id = f.followee_id").
		Where("f.follower_id = ?", userID).
		Order("f.id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	if err != nil {
		return []SocialUser{}, 0, err
	}
	return rowsToSocialUsers(rows), total, nil
}

// ListFollowers 粉丝列表
func (d *SocialDal) ListFollowers(ctx context.Context, userID uint, page, pageSize int) ([]SocialUser, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	var total int64
	q := d.db.WithContext(ctx).Model(&model.UserFollow{}).Where("followee_id = ?", userID)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []socialUserRow
	err := d.db.WithContext(ctx).Table("user_follows f").
		Select("u.id AS user_id, u.username, u.name, u.avatar, u.is_site_admin").
		Joins("JOIN users u ON u.id = f.follower_id").
		Where("f.followee_id = ?", userID).
		Order("f.id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	if err != nil {
		return []SocialUser{}, 0, err
	}
	return rowsToSocialUsers(rows), total, nil
}

// SearchUsers 搜索用户（用户名 / 昵称）。
// 公共域可见性：未配置隐私 或 allow_public_profile=true 的用户才出现在搜索结果中。
// （私人域成员互搜由调用方另做组织过滤时再放宽；当前全站搜索遵循公共域规则。）
func (d *SocialDal) SearchUsers(ctx context.Context, keyword string, page, pageSize int) ([]SocialUser, int64, error) {
	kw := strings.TrimSpace(keyword)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	// 空关键词 / 仅通配符不扫全表，避免枚举
	like := sqllike.Pattern(kw)
	if like == "" {
		return []SocialUser{}, 0, nil
	}
	q := d.db.WithContext(ctx).Model(&model.User{}).
		Where("(privacy_configured = false OR allow_public_profile = true)").
		Where("username ILIKE ? OR name ILIKE ?", like, like)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []socialUserRow
	err := q.Select("id AS user_id, username, name, avatar, is_site_admin").
		Order("id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	if err != nil {
		return []SocialUser{}, 0, err
	}
	return rowsToSocialUsers(rows), total, nil
}

// GetPrivacy 读取隐私字段
func (d *SocialDal) GetPrivacy(ctx context.Context, userID uint) (configured, allowProfile, allowFeed bool, err error) {
	var u model.User
	err = d.db.WithContext(ctx).Select(
		"privacy_configured", "allow_public_profile", "allow_public_feed",
	).First(&u, userID).Error
	if err != nil {
		return false, true, true, err
	}
	// GORM default:true 对已有行可能为 false 零值；未配置时按产品默认 true
	if !u.PrivacyConfigured {
		return false, true, true, nil
	}
	return true, u.AllowPublicProfile, u.AllowPublicFeed, nil
}

// UpdatePrivacy 保存隐私并标记已配置
func (d *SocialDal) UpdatePrivacy(ctx context.Context, userID uint, allowProfile, allowFeed bool) error {
	return d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"privacy_configured":   true,
		"allow_public_profile": allowProfile,
		"allow_public_feed":    allowFeed,
	}).Error
}

// FilterPublicFeedUserIDs 保留允许出现在公共域动态的用户（未配置=默认允许）
func (d *SocialDal) FilterPublicFeedUserIDs(ctx context.Context, userIDs []int64) ([]int64, error) {
	if len(userIDs) == 0 {
		return []int64{}, nil
	}
	type row struct {
		ID                uint
		PrivacyConfigured bool
		AllowPublicFeed   bool
	}
	var rows []row
	err := d.db.WithContext(ctx).Model(&model.User{}).
		Select("id, privacy_configured, allow_public_feed").
		Where("id IN ?", userIDs).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		if !r.PrivacyConfigured || r.AllowPublicFeed {
			out = append(out, int64(r.ID))
		}
	}
	return out, nil
}

// CanViewPublicProfile 公共域下是否允许查看资料（未配置=默认允许）
func (d *SocialDal) CanViewPublicProfile(ctx context.Context, userID uint) (bool, error) {
	configured, allowProfile, _, err := d.GetPrivacy(ctx, userID)
	if err != nil {
		return false, err
	}
	if !configured {
		return true, nil
	}
	return allowProfile, nil
}

// GetByUsername 精确用户名
func (d *SocialDal) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var u model.User
	err := d.db.WithContext(ctx).Where("username = ?", username).First(&u).Error
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// IsPublicOrg 当前 org 是否公共域
func (d *SocialDal) IsPublicOrg(ctx context.Context, orgID uint) bool {
	if orgID == 0 {
		return true // 无组织上下文视作公共域
	}
	var o model.Org
	if err := d.db.WithContext(ctx).Select("id, slug, is_system").First(&o, orgID).Error; err != nil {
		return true
	}
	return o.IsSystem || o.Slug == model.PublicOrgSlug
}

// PublicOrgID 公共域 id（失败返回 0）
func (d *SocialDal) PublicOrgID(ctx context.Context) uint {
	var o model.Org
	if err := d.db.WithContext(ctx).Select("id").Where("slug = ?", model.PublicOrgSlug).First(&o).Error; err != nil {
		return 0
	}
	return o.ID
}

// EnrichDisplay 按观众当前组织解析主展示名，并附带共属组织徽章。
// 规则：
//  1. 目标在当前域 → name = 当前域 org_display_name（空则 username）
//  2. 目标不在当前域 → name = 公共域称呼（users.name ≡ 公共域 org_display_name；空则 username）
//  3. sharedOrgs = 双方共属、且**非当前域**的组织（含公共域；切换到校队后仍会标公共域与其他共属校队）
//     观众不可见的域绝不返回
func (d *SocialDal) EnrichDisplay(ctx context.Context, viewerID, viewerOrgID uint, users []SocialUser) []SocialUser {
	if len(users) == 0 {
		return users
	}
	uids := make([]uint, 0, len(users))
	for _, u := range users {
		if u.UserID > 0 {
			uids = append(uids, u.UserID)
		}
	}
	if len(uids) == 0 {
		return users
	}

	publicID := d.PublicOrgID(ctx)
	if viewerOrgID == 0 && publicID > 0 {
		viewerOrgID = publicID
	}

	// 当前组织内称呼（含是否成员：map 有 key 即成员）
	currentNames := map[uint]string{}
	if viewerOrgID > 0 {
		currentNames = d.orgDisplayNameMap(ctx, viewerOrgID, uids)
	}
	// 公共域称呼：优先 org_display_name，否则 users.name（调用方已填 Name）
	publicNames := map[uint]string{}
	if publicID > 0 {
		publicNames = d.orgDisplayNameMap(ctx, publicID, uids)
	}

	// userId → 解析后的公共域主称呼（徽章回填用）
	resolvedPublic := make(map[uint]string, len(users))

	for i := range users {
		u := &users[i]
		publicName := strings.TrimSpace(publicNames[u.UserID])
		if publicName == "" {
			publicName = strings.TrimSpace(u.Name)
		}
		if publicName == "" {
			publicName = u.Username
		}
		resolvedPublic[u.UserID] = publicName

		if dname, inOrg := currentNames[u.UserID]; inOrg {
			u.InCurrentOrg = true
			if strings.TrimSpace(dname) != "" {
				u.Name = strings.TrimSpace(dname)
			} else if u.Username != "" {
				u.Name = u.Username
			} else {
				u.Name = publicName
			}
		} else {
			u.InCurrentOrg = false
			u.Name = publicName
		}
		if u.Name == "" {
			u.Name = u.Username
		}
	}

	// 任意当前域：共属其他组织徽章（排除当前域；含公共域）
	if viewerID > 0 {
		aliases := d.sharedOrgAliases(ctx, viewerID, uids, viewerOrgID)
		for i := range users {
			list := aliases[users[i].UserID]
			if len(list) == 0 {
				users[i].SharedOrgs = []SharedOrgAlias{}
				continue
			}
			out := make([]SharedOrgAlias, 0, len(list))
			for _, a := range list {
				dn := strings.TrimSpace(a.DisplayName)
				// 公共域空称呼回填解析后的昵称
				if dn == "" && publicID > 0 && a.OrgID == publicID {
					dn = resolvedPublic[users[i].UserID]
				}
				if dn == "" {
					continue
				}
				a.DisplayName = dn
				out = append(out, a)
			}
			users[i].SharedOrgs = out
		}
	} else {
		for i := range users {
			users[i].SharedOrgs = []SharedOrgAlias{}
		}
	}

	// 自定义站点角色名（内置角色不进 badge：站管另有专属 badge）
	siteRoles := d.siteRoleNameMap(ctx, uids)
	for i := range users {
		names := siteRoles[users[i].UserID]
		if names == nil {
			names = []string{}
		}
		users[i].SiteRoles = names
	}
	return users
}

// siteRoleNameMap userId → 持有的自定义站点角色名（按角色 id 升序，保证展示稳定）
func (d *SocialDal) siteRoleNameMap(ctx context.Context, userIDs []uint) map[uint][]string {
	out := make(map[uint][]string)
	if len(userIDs) == 0 {
		return out
	}
	type row struct {
		UserID uint   `gorm:"column:user_id"`
		Name   string `gorm:"column:name"`
	}
	var rows []row
	err := d.db.WithContext(ctx).Table("user_roles AS ur").
		Select("ur.user_id AS user_id, r.name AS name").
		Joins("JOIN roles r ON r.id = ur.role_id AND r.scope = 'site' AND r.is_system = false").
		Where("ur.user_id IN ? AND ur.org_id = 0", userIDs).
		Order("ur.user_id ASC, r.id ASC").
		Scan(&rows).Error
	if err != nil {
		return out
	}
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if r.UserID == 0 || name == "" {
			continue
		}
		out[r.UserID] = append(out[r.UserID], name)
	}
	return out
}

func (d *SocialDal) orgDisplayNameMap(ctx context.Context, orgID uint, userIDs []uint) map[uint]string {
	out := make(map[uint]string)
	if orgID == 0 || len(userIDs) == 0 {
		return out
	}
	type row struct {
		UserID         uint
		OrgDisplayName string
	}
	var rows []row
	_ = d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Select("user_id, org_display_name").
		Where("org_id = ? AND user_id IN ?", orgID, userIDs).
		Find(&rows).Error
	for _, r := range rows {
		// 有 membership 即写入 key（空字符串表示在组织但未设称呼）
		out[r.UserID] = strings.TrimSpace(r.OrgDisplayName)
	}
	return out
}

// sharedOrgAliases 观众与目标共属的组织称呼（排除当前观看域 excludeOrgID）。
// 含公共域与各校队；隐私：仅双方都加入的组织。
func (d *SocialDal) sharedOrgAliases(ctx context.Context, viewerID uint, targetIDs []uint, excludeOrgID uint) map[uint][]SharedOrgAlias {
	out := make(map[uint][]SharedOrgAlias)
	if viewerID == 0 || len(targetIDs) == 0 {
		return out
	}
	type row struct {
		UserID         uint
		OrgID          uint
		OrgName        string
		OrgDisplayName string
		Slug           string
	}
	var rows []row
	q := d.db.WithContext(ctx).Table("org_members AS m_view").
		Select("m_tgt.user_id AS user_id, o.id AS org_id, o.name AS org_name, m_tgt.org_display_name AS org_display_name, o.slug AS slug").
		Joins("JOIN org_members AS m_tgt ON m_tgt.org_id = m_view.org_id AND m_tgt.user_id IN ?", targetIDs).
		Joins("JOIN orgs AS o ON o.id = m_view.org_id").
		Where("m_view.user_id = ?", viewerID)
	if excludeOrgID > 0 {
		q = q.Where("o.id <> ?", excludeOrgID)
	}
	err := q.Order("o.id ASC").Scan(&rows).Error
	if err != nil {
		return out
	}
	// 公共域优先，便于在校队视图下先看到「公共域 · 昵称」
	appendRow := func(r row) {
		out[r.UserID] = append(out[r.UserID], SharedOrgAlias{
			OrgID:       r.OrgID,
			OrgName:     strings.TrimSpace(r.OrgName),
			DisplayName: strings.TrimSpace(r.OrgDisplayName),
		})
	}
	for _, r := range rows {
		if r.Slug == model.PublicOrgSlug {
			appendRow(r)
		}
	}
	for _, r := range rows {
		if r.Slug != model.PublicOrgSlug {
			appendRow(r)
		}
	}
	return out
}

// SearchUsersInContext 搜索：用户名 / 全局昵称 / 当前组织内称呼（模糊）；再按观众上下文解析展示名
func (d *SocialDal) SearchUsersInContext(ctx context.Context, keyword string, page, pageSize int, viewerID, viewerOrgID uint) ([]SocialUser, int64, error) {
	kw := strings.TrimSpace(keyword)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	like := sqllike.Pattern(kw)
	if like == "" {
		return []SocialUser{}, 0, nil
	}
	publicID := d.PublicOrgID(ctx)
	isPublicView := viewerOrgID == 0 || d.IsPublicOrg(ctx, viewerOrgID)

	// 基础：公共域可见性
	base := d.db.WithContext(ctx).Table("users AS u").
		Where("(u.privacy_configured = false OR u.allow_public_profile = true)")

	// 匹配：username / name / 当前组织 org_display_name（私人域时）/ 公共域 org_display_name
	if !isPublicView && viewerOrgID > 0 {
		base = base.Where(`
			u.username ILIKE ? OR u.name ILIKE ?
			OR EXISTS (
				SELECT 1 FROM org_members m
				WHERE m.user_id = u.id AND m.org_id = ? AND m.org_display_name ILIKE ?
			)
			OR EXISTS (
				SELECT 1 FROM org_members m
				WHERE m.user_id = u.id AND m.org_id = ? AND m.org_display_name ILIKE ?
			)
		`, like, like, viewerOrgID, like, publicID, like)
	} else {
		// 公共域：username / name / 公共域称呼 / 观众共属组织内称呼（仅登录后可搜共属队内名）
		if viewerID > 0 {
			base = base.Where(`
				u.username ILIKE ? OR u.name ILIKE ?
				OR EXISTS (
					SELECT 1 FROM org_members m
					WHERE m.user_id = u.id AND m.org_id = ? AND m.org_display_name ILIKE ?
				)
				OR EXISTS (
					SELECT 1 FROM org_members m_view
					JOIN org_members m_tgt ON m_tgt.org_id = m_view.org_id AND m_tgt.user_id = u.id
					JOIN orgs o ON o.id = m_view.org_id
					WHERE m_view.user_id = ?
					  AND COALESCE(o.is_system, false) = false
					  AND o.slug <> ?
					  AND m_tgt.org_display_name ILIKE ?
				)
			`, like, like, publicID, like, viewerID, model.PublicOrgSlug, like)
		} else {
			base = base.Where(`
				u.username ILIKE ? OR u.name ILIKE ?
				OR EXISTS (
					SELECT 1 FROM org_members m
					WHERE m.user_id = u.id AND m.org_id = ? AND m.org_display_name ILIKE ?
				)
			`, like, like, publicID, like)
		}
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []socialUserRow
	err := base.Select("u.id AS user_id, u.username, u.name, u.avatar, u.is_site_admin").
		Order("u.id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&rows).Error
	if err != nil {
		return []SocialUser{}, 0, err
	}
	return d.EnrichDisplay(ctx, viewerID, viewerOrgID, rowsToSocialUsers(rows)), total, nil
}
