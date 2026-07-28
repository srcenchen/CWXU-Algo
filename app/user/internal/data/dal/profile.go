package dal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	data2 "cwxu-algo/app/common/data"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/user/internal/biz/dormancy"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type ProfileDal struct {
	db  *gorm.DB
	rdb *redis.Client
}

func (d *ProfileDal) DB() *gorm.DB { return d.db }

func NewProfileDal(data *data.Data) *ProfileDal {
	return &ProfileDal{db: data.DB, rdb: data.RDB}
}

// GetById 根据Id获取用户详细信息
func (d *ProfileDal) GetById(ctx context.Context, userId int64) (*model.User, error) {
	cacheKey := fmt.Sprintf("user:%d:profile", userId)
	profile, _, err := data2.GetCacheDal[model.User](ctx, d.rdb, cacheKey, func(data *model.User) error {
		err := d.db.Where("id = ?", userId).First(data).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("没有找到相关用户信息")
		} else if err != nil {
			return fmt.Errorf("未知错误 %s", err.Error())
		}
		return nil
	})
	return profile, err
}

// GetByName 按姓名或用户名模糊查询（ILIKE，忽略大小写）
func (d *ProfileDal) GetByName(ctx context.Context, name string) ([]*model.User, error) {
	var userList []*model.User
	kw := strings.TrimSpace(name)
	if kw == "" {
		return userList, nil
	}
	like := "%" + kw + "%"
	err := d.db.WithContext(ctx).
		Select("id, name, username").
		Where("name ILIKE ? OR username ILIKE ?", like, like).
		Limit(15).
		Find(&userList).Error
	if err != nil {
		return nil, err
	}
	return userList, nil
}

// RDB 供验证码等跨层使用
func (d *ProfileDal) RDB() *redis.Client {
	if d == nil {
		return nil
	}
	return d.rdb
}

// EmailTakenByOther 邮箱是否被其他用户占用
func (d *ProfileDal) EmailTakenByOther(ctx context.Context, email string, selfID uint) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false, nil
	}
	var n int64
	err := d.db.WithContext(ctx).Model(&model.User{}).
		Where("LOWER(email) = ? AND id <> ?", email, selfID).
		Count(&n).Error
	return n > 0, err
}

// InvalidateProfileCache 删除资料缓存（组织内称呼等旁路更新后调用）
func (d *ProfileDal) InvalidateProfileCache(ctx context.Context, userID uint) {
	if d == nil || d.rdb == nil || userID == 0 {
		return
	}
	_ = d.rdb.Del(ctx, fmt.Sprintf("user:%d:profile", userID)).Err()
	// 展示名缓存：资料/头像变更时清公共域 + 未知 org 依赖 TTL
	d.InvalidateDisplayCache(ctx, 0, int64(userID))
}

// --- P0 Redis：组织成员 / 展示名 ---

const (
	orgMembersCacheTTL = 5 * time.Minute
	displayCacheTTL    = 10 * time.Minute
	followingCacheTTL  = 10 * time.Minute
)

func orgMembersCacheKey(orgID uint) string {
	return fmt.Sprintf("user:org:members:v1:%d", orgID)
}

func displayCacheKey(orgID uint, userID int64) string {
	// v2：不在组织时回退公共域昵称（不再强制 username）
	return fmt.Sprintf("user:display:v2:o%d:u%d", orgID, userID)
}

func followingCacheKey(userID uint) string {
	return fmt.Sprintf("user:social:following:v1:%d", userID)
}

// InvalidateOrgMembersCache 成员变更后失效
func (d *ProfileDal) InvalidateOrgMembersCache(ctx context.Context, orgID uint) {
	if d == nil || d.rdb == nil || orgID == 0 {
		return
	}
	_ = d.rdb.Del(ctx, orgMembersCacheKey(orgID)).Err()
}

// InvalidateDisplayCache 展示名/头像变更：按 org+user 精确删；orgID=0 时只依赖 TTL
func (d *ProfileDal) InvalidateDisplayCache(ctx context.Context, orgID uint, userID int64) {
	if d == nil || d.rdb == nil || userID == 0 {
		return
	}
	if orgID > 0 {
		_ = d.rdb.Del(ctx, displayCacheKey(orgID, userID)).Err()
	}
	// 公共域也常见：顺带删 public org（若可解析）
	if pub, err := d.PublicOrgID(ctx); err == nil && pub > 0 && pub != orgID {
		_ = d.rdb.Del(ctx, displayCacheKey(pub, userID)).Err()
	}
}

// InvalidateFollowingCache 关注列表变更
func (d *ProfileDal) InvalidateFollowingCache(ctx context.Context, userID uint) {
	if d == nil || d.rdb == nil || userID == 0 {
		return
	}
	_ = d.rdb.Del(ctx, followingCacheKey(userID)).Err()
}

// GetUserIdsByOrgCached 组织成员列表（Redis 5min + 写路径失效）
func (d *ProfileDal) GetUserIdsByOrgCached(ctx context.Context, orgID uint) ([]int64, error) {
	if orgID == 0 {
		return []int64{}, nil
	}
	if d.rdb == nil {
		return d.GetUserIdsByOrg(ctx, orgID)
	}
	key := orgMembersCacheKey(orgID)
	ids, _, err := data2.GetCacheDalTTL[[]int64](ctx, d.rdb, key, orgMembersCacheTTL, func(data *[]int64) error {
		list, e := d.GetUserIdsByOrg(ctx, orgID)
		if e != nil {
			return e
		}
		if list == nil {
			list = []int64{}
		}
		*data = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ids == nil {
		return []int64{}, nil
	}
	return *ids, nil
}

// GetByIdsForOrgCached 批量展示名：MGET 部分命中 + miss 回源
func (d *ProfileDal) GetByIdsForOrgCached(ctx context.Context, orgID uint, userIds []int64) ([]UserProfile, error) {
	if len(userIds) == 0 {
		return nil, nil
	}
	if d.rdb == nil {
		return d.GetByIdsForOrg(ctx, orgID, userIds)
	}
	// 去重保序
	seen := make(map[int64]struct{}, len(userIds))
	ordered := make([]int64, 0, len(userIds))
	for _, id := range userIds {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	if len(ordered) == 0 {
		return nil, nil
	}
	if orgID == 0 {
		if pub, e := d.PublicOrgID(ctx); e == nil {
			orgID = pub
		}
	}

	keys := make([]string, len(ordered))
	for i, id := range ordered {
		keys[i] = displayCacheKey(orgID, id)
	}
	vals, err := d.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return d.GetByIdsForOrg(ctx, orgID, ordered)
	}

	outMap := make(map[int64]UserProfile, len(ordered))
	var miss []int64
	for i, id := range ordered {
		if vals[i] == nil {
			miss = append(miss, id)
			continue
		}
		s, ok := vals[i].(string)
		if !ok || s == "" {
			miss = append(miss, id)
			continue
		}
		var p UserProfile
		if e := utils.GobDecoder([]byte(s), &p); e != nil || p.ID == 0 {
			miss = append(miss, id)
			continue
		}
		outMap[id] = p
	}
	if len(miss) > 0 {
		loaded, e := d.GetByIdsForOrg(ctx, orgID, miss)
		if e != nil {
			return nil, e
		}
		pipe := d.rdb.Pipeline()
		for _, p := range loaded {
			outMap[int64(p.ID)] = p
			if b, e2 := utils.GobEncoder(p); e2 == nil {
				pipe.Set(ctx, displayCacheKey(orgID, int64(p.ID)), b, displayCacheTTL)
			}
		}
		_, _ = pipe.Exec(ctx)
	}
	out := make([]UserProfile, 0, len(ordered))
	for _, id := range ordered {
		if p, ok := outMap[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// FollowingIDsCached 关注列表缓存（由 service 在 follow/unfollow 失效）
func FollowingIDsCached(ctx context.Context, rdb *redis.Client, userID uint, load func() ([]int64, error)) ([]int64, error) {
	if userID == 0 {
		return []int64{}, nil
	}
	if rdb == nil {
		return load()
	}
	key := followingCacheKey(userID)
	ids, _, err := data2.GetCacheDalTTL[[]int64](ctx, rdb, key, followingCacheTTL, func(data *[]int64) error {
		list, e := load()
		if e != nil {
			return e
		}
		if list == nil {
			list = []int64{}
		}
		*data = list
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ids == nil {
		return []int64{}, nil
	}
	return *ids, nil
}

func InvalidateFollowingCacheRDB(ctx context.Context, rdb *redis.Client, userID uint) {
	if rdb == nil || userID == 0 {
		return
	}
	_ = rdb.Del(ctx, followingCacheKey(userID)).Err()
}

// UpdateAvatarEmail 更新头像；emailChanged 时同时写邮箱。不再改 name（昵称走组织内称呼）。
func (d *ProfileDal) UpdateAvatarEmail(ctx context.Context, profile model.User, emailChanged bool) error {
	cacheKey := fmt.Sprintf("user:%d:profile", profile.ID)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		updates := map[string]interface{}{
			"avatar": profile.Avatar,
		}
		if emailChanged {
			updates["email"] = strings.ToLower(strings.TrimSpace(profile.Email))
		}
		return d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", profile.ID).Updates(updates).Error
	})
}

// Update 兼容旧调用：头像+邮箱+昵称（管理端等）；新编辑资料走 UpdateAvatarEmail
func (d *ProfileDal) Update(ctx context.Context, profile model.User) error {
	cacheKey := fmt.Sprintf("user:%d:profile", profile.ID)
	err := data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		if err := d.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", profile.ID).Updates(map[string]interface{}{
			"avatar": profile.Avatar,
			"email":  profile.Email,
			"name":   profile.Name,
		}).Error; err != nil {
			return err
		}
		name := strings.TrimSpace(profile.Name)
		if name == "" {
			return nil
		}
		var publicID uint
		if err := d.db.WithContext(ctx).Model(&model.Org{}).
			Select("id").Where("slug = ?", model.PublicOrgSlug).
			Scan(&publicID).Error; err != nil || publicID == 0 {
			return nil
		}
		_ = d.db.WithContext(ctx).Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ?", publicID, profile.ID).
			Update("org_display_name", name).Error
		return nil
	})
	return err
}

// OrgDisplayNamesByUserIDs 批量取某组织内的组织内名称
func (d *ProfileDal) OrgDisplayNamesByUserIDs(ctx context.Context, orgID uint, userIDs []uint) (map[uint]string, error) {
	out := make(map[uint]string)
	if orgID == 0 || len(userIDs) == 0 {
		return out, nil
	}
	type row struct {
		UserID         uint
		OrgDisplayName string
	}
	var rows []row
	err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Select("user_id, org_display_name").
		Where("org_id = ? AND user_id IN ?", orgID, userIDs).
		Find(&rows).Error
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.UserID] = strings.TrimSpace(r.OrgDisplayName)
	}
	return out, nil
}

func (d *ProfileDal) GetList(ctx context.Context, pageSize, pageNum int64, keyword string, dormantOnly bool, inactiveDays int) ([]model.User, int64, error) {
	kw := strings.TrimSpace(keyword)
	q := d.db.WithContext(ctx).Model(&model.User{})
	if kw != "" {
		like := "%" + kw + "%"
		// 站内昵称 ≡ 公共域 org_display_name；一并模糊匹配
		if pubID, e := d.PublicOrgID(ctx); e == nil && pubID > 0 {
			q = q.Where(`username ILIKE ? OR name ILIKE ? OR EXISTS (
				SELECT 1 FROM org_members m
				WHERE m.user_id = users.id AND m.org_id = ? AND m.org_display_name ILIKE ?
			)`, like, like, pubID, like)
		} else {
			q = q.Where("username ILIKE ? OR name ILIKE ?", like, like)
		}
	}
	q = d.applyInactiveListFilter(ctx, q, "users", dormantOnly, inactiveDays)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.User
	err := q.
		Select("id", "username", "name", "group_id", "avatar", "role_id", "is_site_admin",
			"problem_fetch_enabled", "problem_ai_enabled",
			"spider_interval_min_override", "ai_summary_interval_min_override",
			"email_enabled", "email_weekly_enabled", "created_at",
			"sync_exempt", "last_login_at", "admin_force_dormant", "disabled").
		Order("id").
		Limit(int(pageSize)).Offset(int(pageNum-1) * int(pageSize)).
		Find(&list).Error
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// applyInactiveListFilter 列表筛选：
// - inactiveDays>0：「最近 N 天未登录」（站管一键冻结预览；**不排除豁免**，站管可冻任何人）
// - dormantOnly：当前已暂停同步（与 IsDormant 一致：站管强制冻结/禁用 或 超时且无豁免）
// 两者都不开则不过滤。
func (d *ProfileDal) applyInactiveListFilter(ctx context.Context, q *gorm.DB, userTable string, dormantOnly bool, inactiveDays int) *gorm.DB {
	if q == nil {
		return q
	}
	if inactiveDays > 0 {
		days := dormancy.ClampInactiveDays(inactiveDays)
		return d.applyInactiveByDaysFilter(q, userTable, days)
	}
	if dormantOnly {
		return d.applyCurrentlyDormantFilter(ctx, q, userTable)
	}
	return q
}

// applyInactiveByDaysFilter 最近 days 天未登录（含豁免用户，便于站管预览后强制冻结）
func (d *ProfileDal) applyInactiveByDaysFilter(q *gorm.DB, userTable string, days int) *gorm.DB {
	days = dormancy.ClampInactiveDays(days)
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	loginCol := userTable + ".last_login_at"
	return q.Where("("+loginCol+" IS NULL OR "+loginCol+" < ?)", cutoff)
}

// applyCurrentlyDormantFilter 当前已暂停同步：强制冻结 / 禁用 /（超时且无豁免）
func (d *ProfileDal) applyCurrentlyDormantFilter(ctx context.Context, q *gorm.DB, userTable string) *gorm.DB {
	days := d.GetInactiveDays(ctx)
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	loginCol := userTable + ".last_login_at"
	idCol := userTable + ".id"
	// 强制冻结或禁用 → 一律算已冻结
	// 否则：超时 + 无豁免（与 IsDormant 对齐）
	// 占位符须与参数一一对应：cutoff + status + 4 角色 + team/pro = 8
	return q.Where(`(
		`+userTable+`.admin_force_dormant = true
		OR `+userTable+`.disabled = true
		OR (
			(`+loginCol+` IS NULL OR `+loginCol+` < ?)
			AND `+userTable+`.is_site_admin = false
			AND `+userTable+`.sync_exempt = false
			AND NOT EXISTS (
				SELECT 1 FROM org_members m
				JOIN orgs o ON o.id = m.org_id
				WHERE m.user_id = `+idCol+` AND o.status = ?
				  AND (
					m.role IN (?, ?, ?, ?)
					OR o.force_sync = true
					OR o.plan IN (?, ?)
				  )
			)
		)
	)`, cutoff, model.OrgStatusActive,
		model.OrgRoleCoach, model.OrgRoleGroupLeader, model.OrgRoleCaptain, model.OrgRoleOrgAdmin,
		"team", "pro")
}

// applyFreezableInactiveFilter 保留旧名：一键/筛选「最近未登录」= 不排除豁免（站管可冻任何人）
func (d *ProfileDal) applyFreezableInactiveFilter(q *gorm.DB, userTable string, days int) *gorm.DB {
	return d.applyInactiveByDaysFilter(q, userTable, days)
}

// OrgBrief 用户所属组织摘要（列表 Badge）
type OrgBrief struct {
	OrgID uint
	Name  string
	Role  string
}

// ResolveDefaultGroupID 组织默认分组 id（无则创建）
func (d *ProfileDal) ResolveDefaultGroupID(ctx context.Context, orgID uint) (uint, error) {
	if orgID == 0 {
		return 0, fmt.Errorf("orgID 无效")
	}
	var g model.Group
	err := d.db.WithContext(ctx).
		Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").
		First(&g).Error
	if err == nil {
		if g.Name != nil && *g.Name == "未分组" {
			n := model.DefaultGroupName
			_ = d.db.WithContext(ctx).Model(&g).Updates(map[string]interface{}{
				"name": n, "describe": model.DefaultGroupDesc,
			}).Error
		}
		return g.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	n := model.DefaultGroupName
	g = model.Group{Name: &n, Describe: model.DefaultGroupDesc, OrgID: orgID}
	if err := d.db.WithContext(ctx).Create(&g).Error; err != nil {
		return 0, err
	}
	return g.ID, nil
}

// GetGroupNamesByIDs 批量查分组名（仅未删除）
func (d *ProfileDal) GetGroupNamesByIDs(ctx context.Context, groupIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string)
	if len(groupIDs) == 0 {
		return out, nil
	}
	uniq := make([]int64, 0, len(groupIDs))
	seen := map[int64]struct{}{}
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return out, nil
	}
	type row struct {
		ID   int64
		Name string
	}
	var rows []row
	err := d.db.WithContext(ctx).
		Table("groups").
		Select("id, name").
		Where("id IN ?", uniq).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		name := r.Name
		if name == "未分组" {
			name = model.DefaultGroupName
		}
		out[r.ID] = name
	}
	return out, nil
}

// GetOrgBriefsByUserIDs 批量查询用户所属组织
func (d *ProfileDal) GetOrgBriefsByUserIDs(ctx context.Context, userIDs []uint) (map[uint][]OrgBrief, error) {
	out := make(map[uint][]OrgBrief)
	if len(userIDs) == 0 {
		return out, nil
	}
	type row struct {
		UserID   uint
		OrgID    uint
		Name     string
		Role     string
		IsSystem bool
	}
	var rows []row
	err := d.db.WithContext(ctx).
		Table("org_members AS m").
		Select("m.user_id AS user_id, m.org_id AS org_id, o.name AS name, m.role AS role, o.is_system AS is_system").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("m.user_id IN ?", userIDs).
		Order("o.is_system DESC, o.id ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], OrgBrief{
			OrgID: r.OrgID,
			Name:  r.Name,
			Role:  r.Role,
		})
	}
	return out, nil
}

func (d *ProfileDal) MoveGroup(ctx context.Context, userID uint64, groupID int64, orgID uint) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.OrgMember{}).
			Where("user_id = ? AND org_id = ?", userID, orgID).
			Update("group_id", groupID)
		if result.Error != nil {
			return fmt.Errorf("移动用户组失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("用户不属于当前组织")
		}
		// 移组后退出本组织全部旧分队（避免人在 A 组却挂在 B 组分队）
		if err := tx.Exec(`
			DELETE FROM squad_members WHERE user_id = ? AND squad_id IN (
				SELECT id FROM squads WHERE org_id = ?
			)`, userID, orgID).Error; err != nil {
			return fmt.Errorf("清理分队成员失败: %w", err)
		}
		// 若该用户是队长且所管分队已不在新组：卸任队长并清 squad grant
		var role string
		_ = tx.Model(&model.OrgMember{}).Select("role").
			Where("org_id = ? AND user_id = ?", orgID, userID).Scan(&role).Error
		if role == model.OrgRoleCaptain {
			var grants []model.OrgScopeGrant
			_ = tx.Where("org_id = ? AND user_id = ? AND scope_type = ?",
				orgID, userID, model.ScopeTypeSquad).Find(&grants).Error
			for _, g := range grants {
				var sq model.Squad
				if tx.First(&sq, g.ScopeID).Error != nil || uint(groupID) != sq.GroupID {
					_ = tx.Model(&model.OrgMember{}).
						Where("org_id = ? AND user_id = ?", orgID, userID).
						Update("role", model.OrgRoleMember).Error
					_ = tx.Where("org_id = ? AND user_id = ?", orgID, userID).
						Delete(&model.OrgScopeGrant{}).Error
					break
				}
			}
		}
		return nil
	})
}

// GroupBelongsToOrg verifies the tenant boundary before assigning a member.
func (d *ProfileDal) GroupBelongsToOrg(ctx context.Context, groupID int64, orgID uint) bool {
	if groupID <= 0 || orgID == 0 {
		return false
	}
	var n int64
	_ = d.db.WithContext(ctx).Model(&model.Group{}).
		Where("id = ? AND org_id = ?", groupID, orgID).Count(&n).Error
	return n == 1
}

// SetEmailEnabled 设置用户日报邮件开关
func (d *ProfileDal) SetEmailEnabled(ctx context.Context, userId int64, enabled bool) error {
	cacheKey := fmt.Sprintf("user:%d:profile", userId)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.Model(&model.User{}).Where("id = ?", userId).Update("email_enabled", enabled).Error
	})
}

// SetEmailWeeklyEnabled 设置用户周报邮件开关
func (d *ProfileDal) SetEmailWeeklyEnabled(ctx context.Context, userId int64, enabled bool) error {
	cacheKey := fmt.Sprintf("user:%d:profile", userId)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.Model(&model.User{}).Where("id = ?", userId).Update("email_weekly_enabled", enabled).Error
	})
}

// GetEmailEnabled 获取用户日报邮件开关（失败默认关）
func (d *ProfileDal) GetEmailEnabled(ctx context.Context, userId int64) (bool, error) {
	var user model.User
	err := d.db.Select("email_enabled, email_weekly_enabled").Where("id = ?", userId).First(&user).Error
	if err != nil {
		return false, err
	}
	return user.EmailEnabled, nil
}

// UserHasOrgDailyEmailGrant 是否有任一组织授权日报邮件
func (d *ProfileDal) UserHasOrgDailyEmailGrant(ctx context.Context, userID int64) bool {
	var n int64
	_ = d.db.WithContext(ctx).Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("m.user_id = ? AND o.status = ? AND o.enable_ai_email = ?",
			userID, model.OrgStatusActive, true).
		Count(&n)
	return n > 0
}

// UserHasOrgWeeklyEmailGrant 是否在授权周报的组织中担任 staff 角色
func (d *ProfileDal) UserHasOrgWeeklyEmailGrant(ctx context.Context, userID int64) bool {
	var n int64
	_ = d.db.WithContext(ctx).Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where(`m.user_id = ? AND o.status = ?
			AND o.enable_ai_weekly_email = ? AND m.role IN ?`,
			userID, model.OrgStatusActive, true,
			[]string{model.OrgRoleCoach, model.OrgRoleGroupLeader, model.OrgRoleCaptain, model.OrgRoleOrgAdmin}).
		Count(&n)
	return n > 0
}

// StaffOrgIDsForWeekly 用户可收周报的组织（staff + 组织周报开）
func (d *ProfileDal) StaffOrgIDsForWeekly(ctx context.Context, userID int64) ([]uint, error) {
	var ids []uint
	err := d.db.WithContext(ctx).Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where(`m.user_id = ? AND o.status = ?
			AND o.enable_ai_weekly_email = ? AND m.role IN ?`,
			userID, model.OrgStatusActive, true,
			[]string{model.OrgRoleCoach, model.OrgRoleGroupLeader, model.OrgRoleCaptain, model.OrgRoleOrgAdmin}).
		Pluck("m.org_id", &ids).Error
	return ids, err
}

// PublicOrgID 公共域 id
func (d *ProfileDal) PublicOrgID(ctx context.Context) (uint, error) {
	var o model.Org
	if err := d.db.WithContext(ctx).Where("slug = ?", model.PublicOrgSlug).First(&o).Error; err != nil {
		return 0, err
	}
	return o.ID, nil
}

// GetUserIdsByOrg 组织成员 userId 列表
func (d *ProfileDal) GetUserIdsByOrg(ctx context.Context, orgID uint) ([]int64, error) {
	var ids []int64
	err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("org_id = ?", orgID).
		Pluck("user_id", &ids).Error
	return ids, err
}

// GetNonPublicOrgUserIds 至少属于一个非公共域/非系统组织的用户
func (d *ProfileDal) GetNonPublicOrgUserIds(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := d.db.WithContext(ctx).
		Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("o.slug <> ? AND COALESCE(o.is_system, false) = false", model.PublicOrgSlug).
		Distinct().
		Pluck("m.user_id", &ids).Error
	return ids, err
}

// GetProblemPipelineUserIds 题面爬取 / AI 资格用户：
// 默认=非公共域组织成员；个人 *bool 覆盖 true 强制开、false 强制关、null 跟默认。
func (d *ProfileDal) GetProblemPipelineUserIds(ctx context.Context) (fetchIDs, aiIDs []int64, err error) {
	orgIDs, err := d.GetNonPublicOrgUserIds(ctx)
	if err != nil {
		return nil, nil, err
	}
	orgSet := make(map[int64]struct{}, len(orgIDs))
	for _, id := range orgIDs {
		orgSet[id] = struct{}{}
	}

	type overrideRow struct {
		ID                  int64
		ProblemFetchEnabled *bool
		ProblemAIEnabled    *bool
	}
	var rows []overrideRow
	// 只拉有覆盖的用户 + 组织用户（组织用户也需读覆盖）
	// 简化：拉全部有覆盖的，再与 org 合并
	if err = d.db.WithContext(ctx).Model(&model.User{}).
		Select("id, problem_fetch_enabled, problem_ai_enabled").
		Where("problem_fetch_enabled IS NOT NULL OR problem_ai_enabled IS NOT NULL").
		Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	fetchOff := map[int64]struct{}{}
	fetchOn := map[int64]struct{}{}
	aiOff := map[int64]struct{}{}
	aiOn := map[int64]struct{}{}
	for _, r := range rows {
		if r.ProblemFetchEnabled != nil {
			if *r.ProblemFetchEnabled {
				fetchOn[r.ID] = struct{}{}
			} else {
				fetchOff[r.ID] = struct{}{}
			}
		}
		if r.ProblemAIEnabled != nil {
			if *r.ProblemAIEnabled {
				aiOn[r.ID] = struct{}{}
			} else {
				aiOff[r.ID] = struct{}{}
			}
		}
	}

	fetchSet := make(map[int64]struct{}, len(orgIDs)+len(fetchOn))
	aiSet := make(map[int64]struct{}, len(orgIDs)+len(aiOn))
	for id := range orgSet {
		if _, off := fetchOff[id]; !off {
			fetchSet[id] = struct{}{}
		}
		if _, off := aiOff[id]; !off {
			aiSet[id] = struct{}{}
		}
	}
	for id := range fetchOn {
		fetchSet[id] = struct{}{}
	}
	for id := range aiOn {
		aiSet[id] = struct{}{}
	}
	fetchIDs = make([]int64, 0, len(fetchSet))
	for id := range fetchSet {
		fetchIDs = append(fetchIDs, id)
	}
	aiIDs = make([]int64, 0, len(aiSet))
	for id := range aiSet {
		aiIDs = append(aiIDs, id)
	}
	return fetchIDs, aiIDs, nil
}

// SetProblemPipeline 设置题面爬取/AI 覆盖（强制 true/false）
func (d *ProfileDal) SetProblemPipeline(ctx context.Context, userID int64, kind string, enabled bool) error {
	col := "problem_fetch_enabled"
	if kind == "ai" {
		col = "problem_ai_enabled"
	}
	cacheKey := fmt.Sprintf("user:%d:profile", userID)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.WithContext(ctx).Model(&model.User{}).
			Where("id = ?", userID).
			Update(col, enabled).Error
	})
}

// SetSyncIntervalOverrides 站点管理员设置/清除个人定时间隔覆盖。
// spider/ai：nil=不改该项；指针 0 或负=清除覆盖；>0=强制分钟数。
func (d *ProfileDal) SetSyncIntervalOverrides(ctx context.Context, userID int64, spider *int, ai *int) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user id")
	}
	if spider == nil && ai == nil {
		return nil
	}
	updates := map[string]interface{}{}
	if spider != nil {
		if *spider <= 0 {
			updates["spider_interval_min_override"] = nil
		} else {
			updates["spider_interval_min_override"] = *spider
		}
	}
	if ai != nil {
		if *ai <= 0 {
			updates["ai_summary_interval_min_override"] = nil
		} else {
			updates["ai_summary_interval_min_override"] = *ai
		}
	}
	cacheKey := fmt.Sprintf("user:%d:profile", userID)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.WithContext(ctx).Model(&model.User{}).
			Where("id = ?", userID).
			Updates(updates).Error
	})
}

// EffectiveProblemPipeline 计算列表展示用有效开关（覆盖优先，否则是否非公共域组织）
func EffectiveProblemPipeline(override *bool, isNonPublicOrg bool) bool {
	if override != nil {
		return *override
	}
	return isNonPublicOrg
}

// 同步间隔合法范围（分钟）：与 profile SetSyncIntervals / 组织配置一致
const (
	syncIntervalMinM = 5
	syncIntervalMaxM = 7 * 24 * 60 // 10080
)

// clampSyncInterval 脏数据防御：<=0 用默认，否则夹到 [5, 10080]
func clampSyncInterval(v, def int) int {
	if v <= 0 {
		return def
	}
	if v < syncIntervalMinM {
		return syncIntervalMinM
	}
	if v > syncIntervalMaxM {
		return syncIntervalMaxM
	}
	return v
}

// UserSyncPolicy 一人多组织聚合后的定时策略
type UserSyncPolicy struct {
	UserID               int64
	EnableSpider         bool
	EnableAISummary      bool
	EnableAIEmail        bool // 组织授权日报（任一）
	EnableAIWeeklyEmail  bool // 组织授权周报且本人为 staff
	IsOrgStaff           bool // coach/group_leader/captain/org_admin 任一
	EmailEnabled         bool // 个人日报偏好
	EmailWeeklyEnabled   bool // 个人周报偏好
	SpiderIntervalMin    int
	AISummaryIntervalMin int
	SyncActive           bool // 非休眠或已豁免，允许后台定时
}

// GetInactiveDays 站点不活跃天数阈值
func (d *ProfileDal) GetInactiveDays(ctx context.Context) int {
	var days int
	if err := d.db.WithContext(ctx).Model(&model.SiteConfig{}).
		Select("inactive_days").Where("id = ?", 1).Scan(&days).Error; err != nil || days <= 0 {
		return dormancy.DefaultInactiveDays
	}
	return dormancy.ClampInactiveDays(days)
}

// GetSyncPolicies 对每个用户：取其所属 active 组织，开关=任一开启，间隔=开启组织中的 MIN；
// 休眠用户强制关闭后台开关。
func (d *ProfileDal) GetSyncPolicies(ctx context.Context, userIDs []int64) ([]UserSyncPolicy, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	inactiveDays := d.GetInactiveDays(ctx)
	now := time.Now()

	type row struct {
		UserID               int64
		Role                 string
		Plan                 string
		ForceSync            bool
		EnableSpider         bool
		EnableAISummary      bool
		EnableAIEmail        bool
		EnableAIWeeklyEmail  bool
		SpiderIntervalMin    int
		AISummaryIntervalMin int
	}
	var rows []row
	err := d.db.WithContext(ctx).
		Table("org_members AS m").
		Select(`m.user_id AS user_id, m.role AS role,
			o.plan AS plan, o.force_sync AS force_sync,
			o.enable_spider AS enable_spider,
			o.enable_ai_summary AS enable_ai_summary,
			o.enable_ai_email AS enable_ai_email,
			o.enable_ai_weekly_email AS enable_ai_weekly_email,
			o.spider_interval_min AS spider_interval_min,
			o.ai_summary_interval_min AS ai_summary_interval_min`).
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("m.user_id IN ? AND o.status = ?", userIDs, model.OrgStatusActive).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	type acc struct {
		spiderOn  bool
		aiOn      bool
		emailOn   bool
		weeklyOn  bool
		staff     bool
		forceSync bool
		paidPlan  bool
		spiderMin int
		aiMin     int
	}
	byUser := make(map[int64]*acc)
	for _, r := range rows {
		a := byUser[r.UserID]
		if a == nil {
			a = &acc{spiderMin: 0, aiMin: 0}
			byUser[r.UserID] = a
		}
		isStaff := model.IsOrgStaffRole(r.Role)
		if isStaff {
			a.staff = true
		}
		if r.ForceSync {
			a.forceSync = true
		}
		if dormancy.IsPaidPlan(r.Plan) {
			a.paidPlan = true
		}
		if r.EnableSpider {
			a.spiderOn = true
			iv := clampSyncInterval(r.SpiderIntervalMin, 60)
			if a.spiderMin == 0 || iv < a.spiderMin {
				a.spiderMin = iv
			}
		}
		if r.EnableAISummary {
			a.aiOn = true
			iv := clampSyncInterval(r.AISummaryIntervalMin, 180)
			if a.aiMin == 0 || iv < a.aiMin {
				a.aiMin = iv
			}
		}
		if r.EnableAIEmail {
			a.emailOn = true
		}
		if r.EnableAIWeeklyEmail && isStaff {
			a.weeklyOn = true
		}
	}

	// 个人邮件偏好 + 站管间隔覆盖 + 活跃/豁免 / 强制冻结 / 禁用
	type pref struct {
		ID                           int64
		EmailEnabled                 bool
		EmailWeeklyEnabled           bool
		SpiderIntervalMinOverride    *int
		AISummaryIntervalMinOverride *int
		IsSiteAdmin                  bool
		SyncExempt                   bool
		LastLoginAt                  *time.Time
		AdminForceDormant            bool
		Disabled                     bool
	}
	var prefs []pref
	_ = d.db.WithContext(ctx).Model(&model.User{}).
		Select(`id, email_enabled, email_weekly_enabled,
			spider_interval_min_override, ai_summary_interval_min_override,
			is_site_admin, sync_exempt, last_login_at, admin_force_dormant, disabled`).
		Where("id IN ?", userIDs).
		Scan(&prefs).Error
	prefMap := make(map[int64]pref, len(prefs))
	for _, p := range prefs {
		prefMap[p.ID] = p
	}

	out := make([]UserSyncPolicy, 0, len(userIDs))
	for _, uid := range userIDs {
		a := byUser[uid]
		pr := prefMap[uid]
		sp, ai := 60, 180
		spiderOn, aiOn, emailOn, weeklyOn, staff := false, false, false, false, false
		forceSync, paidPlan := false, false
		if a != nil {
			if a.spiderMin > 0 {
				sp = a.spiderMin
			}
			if a.aiMin > 0 {
				ai = a.aiMin
			}
			spiderOn, aiOn, emailOn, weeklyOn = a.spiderOn, a.aiOn, a.emailOn, a.weeklyOn
			staff, forceSync, paidPlan = a.staff, a.forceSync, a.paidPlan
		}
		// 站点管理员个人覆盖：优先级最高
		if pr.SpiderIntervalMinOverride != nil && *pr.SpiderIntervalMinOverride > 0 {
			sp = clampSyncInterval(*pr.SpiderIntervalMinOverride, 60)
		}
		if pr.AISummaryIntervalMinOverride != nil && *pr.AISummaryIntervalMinOverride > 0 {
			ai = clampSyncInterval(*pr.AISummaryIntervalMinOverride, 180)
		}

		ex := dormancy.ExemptFlags{
			IsSiteAdmin: pr.IsSiteAdmin,
			SyncExempt:  pr.SyncExempt,
			IsOrgStaff:  staff,
			ForceSync:   forceSync,
			PaidPlan:    paidPlan,
		}
		dormant := dormancy.IsDormant(pr.LastLoginAt, inactiveDays, ex, now, pr.AdminForceDormant, pr.Disabled)
		syncActive := !dormant
		if dormant {
			spiderOn, aiOn, emailOn, weeklyOn = false, false, false, false
		}

		out = append(out, UserSyncPolicy{
			UserID:               uid,
			EnableSpider:         spiderOn,
			EnableAISummary:      aiOn,
			EnableAIEmail:        emailOn,
			EnableAIWeeklyEmail:  weeklyOn,
			IsOrgStaff:           staff,
			EmailEnabled:         pr.EmailEnabled,
			EmailWeeklyEnabled:   pr.EmailWeeklyEnabled,
			SpiderIntervalMin:    sp,
			AISummaryIntervalMin: ai,
			SyncActive:           syncActive,
		})
	}
	return out, nil
}

// IsUserDormant 单用户休眠判定（登录唤醒用）
func (d *ProfileDal) IsUserDormant(ctx context.Context, u *model.User) bool {
	if u == nil {
		return false
	}
	if u.Disabled || u.AdminForceDormant {
		return true
	}
	policies, err := d.GetSyncPolicies(ctx, []int64{int64(u.ID)})
	if err != nil || len(policies) == 0 {
		// 兜底：仅看时间 + 站管/手动豁免
		ex := dormancy.ExemptFlags{IsSiteAdmin: u.IsSiteAdmin, SyncExempt: u.SyncExempt}
		return dormancy.IsDormant(u.LastLoginAt, d.GetInactiveDays(ctx), ex, time.Now(), u.AdminForceDormant, u.Disabled)
	}
	return !policies[0].SyncActive
}

// TouchLastLogin 更新最近活跃时间，并清除站管强制冻结标记
func (d *ProfileDal) TouchLastLogin(ctx context.Context, userID uint, at time.Time) error {
	if userID == 0 {
		return nil
	}
	return d.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"last_login_at":       at,
			"admin_force_dormant": false,
		}).Error
}

// TouchLastLoginBatch 批量刷新最近活跃时间并清除强制冻结，返回实际更新行数
func (d *ProfileDal) TouchLastLoginBatch(ctx context.Context, userIDs []int64, at time.Time) (int64, error) {
	ids := dedupePositiveIDs(userIDs)
	if len(ids) == 0 {
		return 0, nil
	}
	res := d.db.WithContext(ctx).Model(&model.User{}).
		Where("id IN ?", ids).
		Updates(map[string]interface{}{
			"last_login_at":       at,
			"admin_force_dormant": false,
		})
	return res.RowsAffected, res.Error
}

func dedupePositiveIDs(userIDs []int64) []int64 {
	ids := make([]int64, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, id := range userIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// FilterExistingUserIDs 在给定 ids 中保留真实存在的用户（站管可强制冻结任意人，不排除豁免）
func (d *ProfileDal) FilterExistingUserIDs(ctx context.Context, userIDs []int64) ([]int64, error) {
	ids := dedupePositiveIDs(userIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	var out []int64
	if err := d.db.WithContext(ctx).Model(&model.User{}).
		Select("id").
		Where("id IN ?", ids).
		Pluck("id", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// FilterFreezableUserIDs 兼容旧名：站管强制冻结不排除豁免，等价于存在用户
func (d *ProfileDal) FilterFreezableUserIDs(ctx context.Context, userIDs []int64) ([]int64, error) {
	return d.FilterExistingUserIDs(ctx, userIDs)
}

// ListFreezableInactiveUserIDs 全站：最近 inactiveDays 天未登录的用户 id（含豁免，站管可一键冻）
func (d *ProfileDal) ListFreezableInactiveUserIDs(ctx context.Context, inactiveDays int, limit int) ([]int64, error) {
	days := dormancy.ClampInactiveDays(inactiveDays)
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	q := d.db.WithContext(ctx).Model(&model.User{}).Select("id")
	q = d.applyInactiveByDaysFilter(q, "users", days)
	var out []int64
	if err := q.Order("id").Limit(limit).Pluck("id", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ForceDormantBatch 站管强制冻结：回拨 last_login + 标记 admin_force_dormant（覆盖豁免）
// 返回实际更新数。at 一般取 now - (siteInactiveDays+1) 天。
func (d *ProfileDal) ForceDormantBatch(ctx context.Context, userIDs []int64, at time.Time) (int64, error) {
	ids, err := d.FilterExistingUserIDs(ctx, userIDs)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := d.db.WithContext(ctx).Model(&model.User{}).
		Where("id IN ?", ids).
		Updates(map[string]interface{}{
			"last_login_at":       at,
			"admin_force_dormant": true,
		})
	return res.RowsAffected, res.Error
}

// SetSyncExempt 站管设置永不休眠
func (d *ProfileDal) SetSyncExempt(ctx context.Context, userID int64, exempt bool) error {
	return d.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).
		Update("sync_exempt", exempt).Error
}

// SetDisabled 站管禁用/启用账号
func (d *ProfileDal) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	updates := map[string]interface{}{"disabled": disabled}
	if disabled {
		// 禁用时一并暂停后台同步
		updates["admin_force_dormant"] = true
	}
	return d.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).
		Updates(updates).Error
}

// GetListByOrg 分页列出组织成员用户
// total 与列表一致：仅统计仍存在于 users 表的成员（忽略孤儿 org_members）
// keyword 非空时模糊匹配 username / name / org_display_name（ILIKE）
// dormantOnly / inactiveDays 见 applyInactiveListFilter
func (d *ProfileDal) GetListByOrg(ctx context.Context, orgID uint, pageSize, pageNum int64, keyword string, dormantOnly bool, inactiveDays int) ([]model.User, int64, error) {
	kw := strings.TrimSpace(keyword)
	countQ := d.db.WithContext(ctx).
		Table("org_members AS m").
		Joins("JOIN users AS u ON u.id = m.user_id").
		Where("m.org_id = ?", orgID)
	if kw != "" {
		like := "%" + kw + "%"
		countQ = countQ.Where("u.username ILIKE ? OR u.name ILIKE ? OR m.org_display_name ILIKE ?", like, like, like)
	}
	countQ = d.applyInactiveListFilter(ctx, countQ, "u", dormantOnly, inactiveDays)
	var total int64
	if err := countQ.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	listQ := d.db.WithContext(ctx).
		Table("users AS u").
		Select(`u.id, u.username, u.name, COALESCE(m.group_id, 0) AS group_id, u.avatar, u.role_id, u.is_site_admin,
			u.email_enabled, u.email_weekly_enabled,
			u.problem_fetch_enabled, u.problem_ai_enabled,
			u.spider_interval_min_override, u.ai_summary_interval_min_override, u.created_at,
			u.sync_exempt, u.last_login_at, u.admin_force_dormant, u.disabled`).
		Joins("JOIN org_members AS m ON m.user_id = u.id AND m.org_id = ?", orgID)
	if kw != "" {
		like := "%" + kw + "%"
		listQ = listQ.Where("u.username ILIKE ? OR u.name ILIKE ? OR m.org_display_name ILIKE ?", like, like, like)
	}
	listQ = d.applyInactiveListFilter(ctx, listQ, "u", dormantOnly, inactiveDays)
	var list []model.User
	err := listQ.
		Order("u.id").
		Limit(int(pageSize)).Offset(int(pageNum-1) * int(pageSize)).
		Find(&list).Error
	return list, total, err
}

// IsMemberOfOrg 用户是否为某组织成员
func (d *ProfileDal) IsMemberOfOrg(ctx context.Context, userID int64, orgID uint) bool {
	if userID <= 0 || orgID == 0 {
		return false
	}
	var n int64
	_ = d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("user_id = ? AND org_id = ?", userID, orgID).
		Count(&n)
	return n > 0
}

// BatchEmailGrants 批量查询日报/周报组织授权（任一组织满足即 true）
func (d *ProfileDal) BatchEmailGrants(ctx context.Context, userIDs []int64) (daily map[int64]bool, weekly map[int64]bool) {
	daily = map[int64]bool{}
	weekly = map[int64]bool{}
	if len(userIDs) == 0 {
		return daily, weekly
	}
	var dailyIDs []int64
	_ = d.db.WithContext(ctx).Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where("m.user_id IN ? AND o.status = ? AND o.enable_ai_email = ?",
			userIDs, model.OrgStatusActive, true).
		Distinct("m.user_id").
		Pluck("m.user_id", &dailyIDs)
	for _, id := range dailyIDs {
		daily[id] = true
	}
	var weeklyIDs []int64
	_ = d.db.WithContext(ctx).Table("org_members AS m").
		Joins("JOIN orgs o ON o.id = m.org_id").
		Where(`m.user_id IN ? AND o.status = ?
			AND o.enable_ai_weekly_email = ? AND m.role IN ?`,
			userIDs, model.OrgStatusActive, true,
			[]string{model.OrgRoleCoach, model.OrgRoleGroupLeader, model.OrgRoleCaptain, model.OrgRoleOrgAdmin}).
		Distinct("m.user_id").
		Pluck("m.user_id", &weeklyIDs)
	for _, id := range weeklyIDs {
		weekly[id] = true
	}
	return daily, weekly
}

// GetUserIdsByGroup 根据组ID获取用户ID列表
func (d *ProfileDal) GetUserIdsByGroup(ctx context.Context, groupId int64) ([]int64, error) {
	var ids []int64
	err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("group_id = ?", groupId).
		Pluck("user_id", &ids).Error
	return ids, err
}

func (d *ProfileDal) GroupIDForOrg(ctx context.Context, userID int64, orgID uint) int64 {
	if userID <= 0 || orgID == 0 {
		return 0
	}
	var row struct{ GroupID *uint }
	if err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Select("group_id").Where("user_id = ? AND org_id = ?", userID, orgID).
		Scan(&row).Error; err != nil || row.GroupID == nil {
		return 0
	}
	return int64(*row.GroupID)
}

// UserProfile 用户简要信息（供批量查询用）
type UserProfile struct {
	ID       uint
	Name     string // 展示名（调用方按组织解析后写入）
	Username string
	Avatar   string
}

// GetByIds 批量获取用户简要信息（原始 users 字段，Name=全局昵称）
func (d *ProfileDal) GetByIds(ctx context.Context, userIds []int64) ([]UserProfile, error) {
	if len(userIds) == 0 {
		return nil, nil
	}
	var profiles []UserProfile
	err := d.db.WithContext(ctx).Model(&model.User{}).
		Select("id, name, username, avatar").
		Where("id IN ?", userIds).
		Find(&profiles).Error
	return profiles, err
}

// GetByIdsForOrg 批量展示名：
// - 在当前组织：org_display_name（空则 username）
// - 不在当前组织：公共域称呼 users.name（空则 username）
// 注意：OrgDisplayNamesByUserIDs 仅返回成员；有 key 即在组织。
func (d *ProfileDal) GetByIdsForOrg(ctx context.Context, orgID uint, userIds []int64) ([]UserProfile, error) {
	profiles, err := d.GetByIds(ctx, userIds)
	if err != nil || len(profiles) == 0 {
		return profiles, err
	}
	if orgID == 0 {
		if pub, e := d.PublicOrgID(ctx); e == nil {
			orgID = pub
		}
	}
	uids := make([]uint, 0, len(profiles))
	for _, p := range profiles {
		uids = append(uids, p.ID)
	}
	displayMap, _ := d.OrgDisplayNamesByUserIDs(ctx, orgID, uids)
	for i := range profiles {
		if dname, inOrg := displayMap[profiles[i].ID]; inOrg {
			if dname != "" {
				profiles[i].Name = dname
			} else if profiles[i].Username != "" {
				profiles[i].Name = profiles[i].Username
			}
			continue
		}
		// 不在当前组织：保留 users.name（公共域昵称）；空则 username
		if strings.TrimSpace(profiles[i].Name) == "" && profiles[i].Username != "" {
			profiles[i].Name = profiles[i].Username
		}
	}
	return profiles, nil
}

// SetRoleId 设置用户角色ID
func (d *ProfileDal) SetRoleId(ctx context.Context, userId int64, roleId int) error {
	cacheKey := fmt.Sprintf("user:%d:profile", userId)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.Model(&model.User{}).Where("id = ?", userId).Update("role_id", roleId).Error
	})
}

// Delete 删除用户：清空本库关联数据后硬删除用户行，并清理 profile 缓存
func (d *ProfileDal) Delete(ctx context.Context, userId int64) error {
	cacheKey := fmt.Sprintf("user:%d:profile", userId)
	return data2.UpdateCacheDal(ctx, d.rdb, cacheKey, func() error {
		return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			uid := uint(userId)
			if err := tx.Where("follower_id = ? OR followee_id = ?", uid, uid).
				Delete(&model.UserFollow{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ?", uid).Delete(&model.OrgMember{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ?", uid).Delete(&model.OrgJoinRequest{}).Error; err != nil {
				return err
			}
			if err := tx.Where("user_id = ?", uid).Delete(&model.Paste{}).Error; err != nil {
				return err
			}
			result := tx.Delete(&model.User{}, userId)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("用户不存在")
			}
			return nil
		})
	})
}

// GetUserIdsBySquad 分队成员 userId 列表
func (d *ProfileDal) GetUserIdsBySquad(ctx context.Context, squadID int64) ([]int64, error) {
	if squadID <= 0 {
		return nil, nil
	}
	var ids []int64
	err := d.db.WithContext(ctx).Table("squad_members").
		Where("squad_id = ?", squadID).
		Pluck("user_id", &ids).Error
	return ids, err
}

// GetUserIdsByOrgGroup 组织内某分组成员
func (d *ProfileDal) GetUserIdsByOrgGroup(ctx context.Context, orgID uint, groupID int64) ([]int64, error) {
	if orgID == 0 || groupID <= 0 {
		return nil, nil
	}
	var ids []int64
	err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("org_id = ? AND group_id = ?", orgID, groupID).
		Pluck("user_id", &ids).Error
	return ids, err
}

// ListScopeGrants 用户在组织内的管理范围
func (d *ProfileDal) ListScopeGrants(ctx context.Context, orgID, userID uint) ([]model.OrgScopeGrant, error) {
	var rows []model.OrgScopeGrant
	err := d.db.WithContext(ctx).Where("org_id = ? AND user_id = ?", orgID, userID).Find(&rows).Error
	return rows, err
}

// ReplaceScopeGrants 覆盖写入管理范围
func (d *ProfileDal) ReplaceScopeGrants(ctx context.Context, orgID, userID uint, grants []model.OrgScopeGrant) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND user_id = ?", orgID, userID).Delete(&model.OrgScopeGrant{}).Error; err != nil {
			return err
		}
		if len(grants) == 0 {
			return nil
		}
		return tx.Create(&grants).Error
	})
}

