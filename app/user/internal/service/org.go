package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OrgService struct {
	db  *gorm.DB
	rdb *redis.Client
}

func NewOrgService(d *data.Data) *OrgService {
	return &OrgService{db: d.DB, rdb: d.RDB}
}

func (s *OrgService) invalidateUserProfileCache(userID uint) {
	if s == nil || s.rdb == nil || userID == 0 {
		return
	}
	_ = s.rdb.Del(context.Background(), fmt.Sprintf("user:%d:profile", userID)).Err()
}

func (s *OrgService) invalidateOrgMembersCache(orgID uint) {
	if s == nil || s.rdb == nil || orgID == 0 {
		return
	}
	_ = s.rdb.Del(context.Background(), fmt.Sprintf("user:org:members:v1:%d", orgID)).Err()
}

func (s *OrgService) invalidateDisplayCache(orgID uint, userID uint) {
	if s == nil || s.rdb == nil || userID == 0 {
		return
	}
	ctx := context.Background()
	if orgID > 0 {
		_ = s.rdb.Del(ctx, fmt.Sprintf("user:display:v1:o%d:u%d", orgID, userID)).Err()
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, dst interface{}) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

func newInviteCode() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// DefaultSeatLimit 新建组织 / 未配置时的默认用户数上限
const DefaultSeatLimit = 50

func effectiveSeatLimit(limit int) int {
	if limit <= 0 {
		return DefaultSeatLimit
	}
	return limit
}

// countOrgSeats 占用席位数。普通组织=成员总数；
// 公共域仅统计「只属于公共域、未加入任何其它组织」的用户。
// 均只统计 users 表仍存在的成员，避免孤儿 org_members 虚高。
func countOrgSeats(db *gorm.DB, o *model.Org) int64 {
	if o == nil || db == nil {
		return 0
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		var n int64
		_ = db.Raw(`
			SELECT COUNT(*) FROM org_members m
			JOIN users u ON u.id = m.user_id
			WHERE m.org_id = ?
			AND NOT EXISTS (
				SELECT 1 FROM org_members m2
				WHERE m2.user_id = m.user_id
				  AND m2.org_id <> m.org_id
			)
		`, o.ID).Scan(&n).Error
		return n
	}
	var n int64
	_ = db.Table("org_members AS m").
		Joins("JOIN users u ON u.id = m.user_id").
		Where("m.org_id = ?", o.ID).
		Count(&n).Error
	return n
}

// seatFullMessage 若已达上限返回错误文案，否则空串
func seatFullMessage(db *gorm.DB, o *model.Org) string {
	if o == nil {
		return ""
	}
	limit := effectiveSeatLimit(o.SeatLimit)
	used := countOrgSeats(db, o)
	if used >= int64(limit) {
		if o.IsSystem || o.Slug == model.PublicOrgSlug {
			return fmt.Sprintf("公共域仅属用户已达上限（%d/%d），暂时无法注册", used, limit)
		}
		return fmt.Sprintf("该组织用户数已达上限（%d/%d），无法再加入", used, limit)
	}
	return ""
}

func orgToMap(o *model.Org, includeInvite bool) map[string]interface{} {
	m := map[string]interface{}{
		"id":                   o.ID,
		"name":                 o.Name,
		"slug":                 o.Slug,
		"plan":                 o.Plan,
		"seatLimit":            effectiveSeatLimit(o.SeatLimit),
		"status":               o.Status,
		"isSystem":             o.IsSystem,
		"brandTitle":           o.BrandTitle,
		"brandLogo":            o.BrandLogo,
		"brandFavicon":         o.BrandFavicon,
		"joinMode":             o.JoinMode,
		"enableAiSummary":      o.EnableAISummary,
		"enableAiEmail":        o.EnableAIEmail,
		"enableAiWeeklyEmail":  o.EnableAIWeeklyEmail,
		"enableSpider":         o.EnableSpider,
		"spiderIntervalMin":    o.SpiderIntervalMin,
		"aiSummaryIntervalMin": o.AISummaryIntervalMin,
		"aiEmailSchedule":      o.AIEmailSchedule,
		"forceSync":            o.ForceSync,
	}
	if includeInvite {
		m["inviteCode"] = o.InviteCode
	}
	return m
}

func (s *OrgService) orgToMapWithSeats(o *model.Org, includeInvite bool) map[string]interface{} {
	m := orgToMap(o, includeInvite)
	m["memberCount"] = countOrgSeats(s.db, o)
	return m
}

func (s *OrgService) loadUser(id uint) (*model.User, error) {
	var u model.User
	if err := s.db.First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *OrgService) isOrgAdminDB(userID, orgID uint) bool {
	var m model.OrgMember
	if err := s.db.Where("org_id = ? AND user_id = ? AND role = ?", orgID, userID, model.OrgRoleOrgAdmin).First(&m).Error; err != nil {
		return false
	}
	return true
}

func (s *OrgService) isMemberDB(userID, orgID uint) bool {
	var n int64
	s.db.Model(&model.OrgMember{}).Where("org_id = ? AND user_id = ?", orgID, userID).Count(&n)
	return n > 0
}

// ensureOrgMember 保证用户为组织成员；已存在则更新角色/分组/称呼，否则创建。
// groupID 为 nil 时：新建挂组织默认分组；更新不改原分组（仅当原分组为空时补默认）。
func (s *OrgService) ensureOrgMember(orgID, userID uint, role string, groupID *uint, displayName string) error {
	if orgID == 0 || userID == 0 {
		return errors.New("invalid org or user")
	}
	if !model.ValidOrgRole(role) {
		role = model.OrgRoleMember
	}
	displayName = strings.TrimSpace(displayName)
	now := time.Now()

	var m model.OrgMember
	err := s.db.Where("org_id = ? AND user_id = ?", orgID, userID).First(&m).Error
	if err == nil {
		updates := map[string]interface{}{
			"role":             role,
			"org_display_name": displayName,
		}
		if groupID != nil {
			updates["group_id"] = groupID
		} else if m.GroupID == nil || *m.GroupID == 0 {
			if def := s.ensureDefaultGroupID(orgID); def > 0 {
				updates["group_id"] = def
			}
		}
		err = s.db.Model(&m).Updates(updates).Error
		if err == nil {
			s.invalidateOrgMembersCache(orgID)
			s.invalidateDisplayCache(orgID, userID)
			syncOrgMemberSystemRole(s.db, orgID, userID)
		}
		return err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if groupID == nil {
		if def := s.ensureDefaultGroupID(orgID); def > 0 {
			groupID = &def
		}
	}
	err = s.db.Create(&model.OrgMember{
		OrgID:          orgID,
		UserID:         userID,
		Role:           role,
		GroupID:        groupID,
		OrgDisplayName: displayName,
		JoinedAt:       now,
	}).Error
	if err == nil {
		s.invalidateOrgMembersCache(orgID)
		s.invalidateDisplayCache(orgID, userID)
		syncOrgMemberSystemRole(s.db, orgID, userID)
	}
	return err
}

// setDefaultOrg 将组织设为用户默认（current_org_id）；登录/打开站点自动进入该组织。
// 用户之后只需 switch 切换，无需单独「设默认」。
func (s *OrgService) setDefaultOrg(userID, orgID uint) {
	if userID == 0 || orgID == 0 {
		return
	}
	_ = s.db.Model(&model.User{}).Where("id = ?", userID).Update("current_org_id", orgID).Error
}

// fallbackDefaultOrgIf 若用户当前组织是 orgID，则切回公共域
func (s *OrgService) fallbackDefaultOrgIf(userID, orgID uint) {
	if userID == 0 || orgID == 0 {
		return
	}
	var u model.User
	if s.db.Select("id", "current_org_id").First(&u, userID).Error != nil {
		return
	}
	if u.CurrentOrgID != orgID {
		return
	}
	var pub model.Org
	if s.db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error != nil {
		return
	}
	_ = s.db.Model(&u).Update("current_org_id", pub.ID).Error
}

// ensureDefaultGroupID 组织默认分组 ID（无则创建）
func (s *OrgService) ensureDefaultGroupID(orgID uint) uint {
	var g model.Group
	if s.db.Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").First(&g).Error == nil {
		if g.Name != nil && *g.Name == "未分组" {
			n := model.DefaultGroupName
			_ = s.db.Model(&g).Updates(map[string]interface{}{
				"name": n, "describe": model.DefaultGroupDesc,
			}).Error
		}
		return g.ID
	}
	n := model.DefaultGroupName
	g = model.Group{Name: &n, Describe: model.DefaultGroupDesc, OrgID: orgID}
	if s.db.Create(&g).Error != nil {
		return 0
	}
	return g.ID
}

// addOrgMemberAtomic serializes membership creation per organization so the
// seat limit cannot be exceeded by concurrent join/add/review requests.
func (s *OrgService) addOrgMemberAtomic(orgID, userID uint, role, displayName string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var o model.Org
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, orgID).Error; err != nil {
			return err
		}
		if o.Status != model.OrgStatusActive {
			return errors.New("该组织当前已暂停")
		}
		var existing model.OrgMember
		err := tx.Where("org_id = ? AND user_id = ?", orgID, userID).First(&existing).Error
		if err == nil {
			// 已是成员：补空/0 分组到默认组（不改已有有效分组）
			if existing.GroupID == nil || *existing.GroupID == 0 {
				var group model.Group
				gerr := tx.Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
					Order("id ASC").First(&group).Error
				if errors.Is(gerr, gorm.ErrRecordNotFound) {
					name := model.DefaultGroupName
					group = model.Group{Name: &name, Describe: model.DefaultGroupDesc, OrgID: orgID}
					if gerr = tx.Create(&group).Error; gerr != nil {
						return gerr
					}
				} else if gerr != nil {
					return gerr
				}
				gid := group.ID
				return tx.Model(&existing).Update("group_id", gid).Error
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if msg := seatFullMessage(tx, &o); msg != "" {
			return errors.New(msg)
		}
		var group model.Group
		err = tx.Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
			Order("id ASC").First(&group).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			name := model.DefaultGroupName
			group = model.Group{Name: &name, Describe: model.DefaultGroupDesc, OrgID: orgID}
			if err = tx.Create(&group).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !model.ValidOrgRole(role) {
			role = model.OrgRoleMember
		}
		groupID := group.ID
		return tx.Create(&model.OrgMember{
			OrgID: orgID, UserID: userID, Role: role, GroupID: &groupID,
			OrgDisplayName: strings.TrimSpace(displayName), JoinedAt: time.Now(),
		}).Error
	})
	if err == nil {
		s.invalidateOrgMembersCache(orgID)
		s.invalidateDisplayCache(orgID, userID)
		syncOrgMemberSystemRole(s.db, orgID, userID)
	}
	return err
}

// RegisterOrgRoutes HTTP 路由（与 upload 同模式）
func RegisterOrgRoutes(srv *khttp.Server, org *OrgService) {
	r := srv.Route("/")
	r.GET("/v1/user/org/list", org.handleList)
	r.GET("/v1/user/org/discover", org.handleDiscover)
	r.GET("/v1/user/org/get", org.handleGet)
	r.POST("/v1/user/org/create", org.handleCreate)
	r.POST("/v1/user/org/update", org.handleUpdate)
	r.POST("/v1/user/org/delete", org.handleDelete)
	r.POST("/v1/user/org/switch", org.handleSwitch)
	r.POST("/v1/user/org/join", org.handleJoin)
	r.POST("/v1/user/org/leave", org.handleLeave)
	r.GET("/v1/user/org/members", org.handleMembers)
	r.POST("/v1/user/org/members/set-role", org.handleSetRole)
	r.POST("/v1/user/org/members/remove", org.handleRemoveMember)
	r.POST("/v1/user/org/members/add", org.handleAddMember)
	r.POST("/v1/user/org/members/set-display-name", org.handleSetDisplayName)
	r.GET("/v1/user/org/member-ids", org.handleMemberIds)
	r.GET("/v1/user/org/invite", org.handleInviteGet)
	r.GET("/v1/user/org/invite/preview", org.handleInvitePreview)
	r.POST("/v1/user/org/invite/rotate", org.handleInviteRotate)
	r.GET("/v1/user/org/join-requests", org.handleJoinRequests)
	r.POST("/v1/user/org/join-requests/review", org.handleJoinReview)
	r.POST("/v1/user/platform/set-site-admin", org.handleSetSiteAdmin)
}

// handleInvitePreview 公开：按识别码预览组织欢迎信息（不含敏感配置）
func (s *OrgService) handleInvitePreview(ctx khttp.Context) error {
	code := strings.TrimSpace(strings.ToUpper(ctx.Request().URL.Query().Get("code")))
	if code == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请提供邀请识别码"})
		return nil
	}
	var o model.Org
	if err := s.db.Where("UPPER(invite_code) = ? AND status = ?", code, model.OrgStatusActive).First(&o).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "邀请链接无效或已失效"})
		return nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "公共域无需邀请加入"})
		return nil
	}
	displayName := strings.TrimSpace(o.BrandTitle)
	if displayName == "" {
		displayName = o.Name
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"orgId": o.ID, "orgName": displayName, "name": o.Name,
		"brandTitle": o.BrandTitle, "brandLogo": o.BrandLogo,
		"joinMode": o.JoinMode,
	})
	return nil
}

// applyInviteOnRegister 注册成功后按识别码加入组织。
// auto：直接成为成员并设为默认组织；review：提交待审批（仍属公共域默认）。
// 返回给用户看的补充说明；识别码无效时返回非空 error 文案但不阻断注册。
func applyInviteOnRegister(db *gorm.DB, userID uint, inviteCode, displayName string) string {
	code := strings.TrimSpace(strings.ToUpper(inviteCode))
	if code == "" || userID == 0 {
		return ""
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return ""
	}
	if len([]rune(displayName)) > 32 {
		displayName = string([]rune(displayName)[:32])
	}
	var o model.Org
	if err := db.Where("UPPER(invite_code) = ? AND status = ?", code, model.OrgStatusActive).First(&o).Error; err != nil {
		return "账号已创建，但邀请识别码无效，未能加入组织"
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		return ""
	}
	orgLabel := strings.TrimSpace(o.BrandTitle)
	if orgLabel == "" {
		orgLabel = o.Name
	}
	var existing model.OrgMember
	if db.Where("org_id = ? AND user_id = ?", o.ID, userID).First(&existing).Error == nil {
		_ = db.Model(&model.User{}).Where("id = ?", userID).Update("current_org_id", o.ID).Error
		return fmt.Sprintf("已加入「%s」", orgLabel)
	}
	if o.JoinMode == model.OrgJoinReview {
		var jr model.OrgJoinRequest
		err := db.Where("org_id = ? AND user_id = ?", o.ID, userID).First(&jr).Error
		if err == nil {
			if jr.Status != model.JoinReqPending {
				_ = db.Model(&jr).Updates(map[string]interface{}{
					"status": model.JoinReqPending, "code_used": code,
					"org_display_name": displayName, "reviewed_by": nil,
				}).Error
			}
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			if createErr := db.Create(&model.OrgJoinRequest{
				OrgID: o.ID, UserID: userID, Status: model.JoinReqPending,
				CodeUsed: code, OrgDisplayName: displayName,
			}).Error; createErr != nil {
				log.Errorf("register invite join-request: %v", createErr)
				return "账号已创建，但提交加入申请失败，请稍后在「我的组织」用识别码重试"
			}
		} else {
			log.Errorf("register invite join-request lookup: %v", err)
			return "账号已创建，但提交加入申请失败，请稍后重试"
		}
		return fmt.Sprintf("账号已创建，已申请加入「%s」，等待管理员通过", orgLabel)
	}
	// auto：席位 + 成员 + 默认组织
	err := db.Transaction(func(tx *gorm.DB) error {
		var locked model.Org
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, o.ID).Error; e != nil {
			return e
		}
		if locked.Status != model.OrgStatusActive {
			return errors.New("该组织当前已暂停")
		}
		if msg := seatFullMessage(tx, &locked); msg != "" {
			return errors.New(msg)
		}
		var group model.Group
		gerr := tx.Where("org_id = ? AND name IN ?", locked.ID, []string{model.DefaultGroupName, "未分组"}).
			Order("id ASC").First(&group).Error
		if errors.Is(gerr, gorm.ErrRecordNotFound) {
			n := model.DefaultGroupName
			group = model.Group{Name: &n, Describe: model.DefaultGroupDesc, OrgID: locked.ID}
			if gerr = tx.Create(&group).Error; gerr != nil {
				return gerr
			}
		} else if gerr != nil {
			return gerr
		}
		gid := group.ID
		if e := tx.Create(&model.OrgMember{
			OrgID: locked.ID, UserID: userID, Role: model.OrgRoleMember, GroupID: &gid,
			OrgDisplayName: displayName, JoinedAt: time.Now(),
		}).Error; e != nil {
			return e
		}
		return tx.Model(&model.User{}).Where("id = ?", userID).Update("current_org_id", locked.ID).Error
	})
	if err != nil {
		log.Errorf("register invite auto-join: %v", err)
		return fmt.Sprintf("账号已创建，但未能加入「%s」：%s", orgLabel, err.Error())
	}
	return fmt.Sprintf("注册成功，已加入「%s」", orgLabel)
}

// handleDiscover 组织广场：仅公开字段（名/logo/人数），无识别码与成员明细
func (s *OrgService) handleDiscover(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	q := ctx.Request().URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	kw := strings.TrimSpace(q.Get("q"))

	dbq := s.db.Model(&model.Org{}).Where("status = ?", model.OrgStatusActive)
	if kw != "" {
		like := "%" + kw + "%"
		dbq = dbq.Where("name ILIKE ? OR brand_title ILIKE ?", like, like)
	}
	var total int64
	if err := dbq.Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	var orgs []model.Org
	if err := dbq.Order("is_system DESC, id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&orgs).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}

	memberOf := map[uint]bool{}
	currentID := uint(0)
	if pd != nil {
		currentID = pd.OrgID
		var mems []model.OrgMember
		_ = s.db.Where("user_id = ?", pd.UserID).Find(&mems).Error
		for _, m := range mems {
			memberOf[m.OrgID] = true
		}
	}

	list := make([]map[string]interface{}, 0, len(orgs))
	for i := range orgs {
		o := &orgs[i]
		logo := o.BrandLogo
		item := map[string]interface{}{
			"id":          o.ID,
			"name":        o.Name,
			"brandLogo":   logo,
			"memberCount": countOrgSeats(s.db, o),
			"isSystem":    o.IsSystem,
		}
		if pd != nil {
			item["isMember"] = memberOf[o.ID]
			item["isCurrent"] = o.ID == currentID
		}
		list = append(list, item)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "list": list, "total": total,
	})
	return nil
}

func (s *OrgService) handleList(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	q := ctx.Request().URL.Query()
	mine := q.Get("mine") != "0"

	var orgs []model.Org
	if q.Get("all") == "1" && auth.HasPerm(ctx, rbac.PermSiteOrgList) {
		_ = s.db.Order("is_system DESC, id ASC").Find(&orgs).Error
	} else if mine {
		var mems []model.OrgMember
		_ = s.db.Where("user_id = ?", pd.UserID).Find(&mems).Error
		ids := make([]uint, 0, len(mems))
		roleMap := map[uint]string{}
		displayMap := map[uint]string{}
		for _, m := range mems {
			ids = append(ids, m.OrgID)
			roleMap[m.OrgID] = m.Role
			displayMap[m.OrgID] = strings.TrimSpace(m.OrgDisplayName)
		}
		if len(ids) > 0 {
			_ = s.db.Where("id IN ?", ids).Order("is_system DESC, id ASC").Find(&orgs).Error
		}
		list := make([]map[string]interface{}, 0, len(orgs))
		for i := range orgs {
			item := s.orgToMapWithSeats(&orgs[i], false)
			item["myRole"] = roleMap[orgs[i].ID]
			item["orgDisplayName"] = displayMap[orgs[i].ID]
			item["isCurrent"] = orgs[i].ID == pd.OrgID
			list = append(list, item)
		}
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": list})
		return nil
	}

	list := make([]map[string]interface{}, 0, len(orgs))
	for i := range orgs {
		item := s.orgToMapWithSeats(&orgs[i], pd.IsSiteAdmin)
		item["isCurrent"] = orgs[i].ID == pd.OrgID
		list = append(list, item)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": list})
	return nil
}

func (s *OrgService) handleGet(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	idStr := ctx.Request().URL.Query().Get("id")
	id64, _ := strconv.ParseUint(idStr, 10, 64)
	orgID := uint(id64)
	if orgID == 0 && pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
		return nil
	}
	var o model.Org
	if err := s.db.First(&o, orgID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	showInvite := pd != nil && (pd.IsSiteAdmin || hasPermInOrgDB(s.db, pd.UserID, orgID, rbac.PermOrgInviteView))
	item := s.orgToMapWithSeats(&o, showInvite)
	if pd != nil {
		var m model.OrgMember
		if s.db.Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&m).Error == nil {
			item["myRole"] = m.Role
			item["orgDisplayName"] = strings.TrimSpace(m.OrgDisplayName)
		}
		item["isCurrent"] = orgID == pd.OrgID
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": item})
	return nil
}

func (s *OrgService) handleCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !auth.HasPerm(ctx, rbac.PermSiteOrgCreate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无创建组织权限"})
		return nil
	}
	var req struct {
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		AdminUserID uint   `json:"adminUserId"`
		JoinMode    string `json:"joinMode"`
		SeatLimit   *int   `json:"seatLimit"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "组织名称不能为空"})
		return nil
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = "org-" + newInviteCode()
	}
	slug = strings.ToLower(slug)
	if slug == model.PublicOrgSlug {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "slug 保留给公共域"})
		return nil
	}
	joinMode := req.JoinMode
	if joinMode != model.OrgJoinReview {
		joinMode = model.OrgJoinAuto
	}
	seatLimit := DefaultSeatLimit
	if req.SeatLimit != nil {
		if *req.SeatLimit < 1 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "用户数上限至少为 1"})
			return nil
		}
		seatLimit = *req.SeatLimit
	}
	o := model.Org{
		Name:                 name,
		Slug:                 slug,
		Plan:                 "team",
		SeatLimit:            seatLimit,
		Status:               model.OrgStatusActive,
		IsSystem:             false,
		JoinMode:             joinMode,
		InviteCode:           newInviteCode(),
		EnableAISummary:      true,
		EnableAIEmail:        true,
		EnableAIWeeklyEmail:  true,
		EnableSpider:         true,
		SpiderIntervalMin:    60,
		AISummaryIntervalMin: 180,
		AIEmailSchedule:      "30 7 * * *",
	}
	adminUID := req.AdminUserID
	if adminUID == 0 {
		adminUID = pd.UserID
	}
	adminUser, err := s.loadUser(adminUID)
	if err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "指定的组织管理员不存在"})
		return nil
	}
	displayName := strings.TrimSpace(adminUser.Name)
	if displayName == "" {
		displayName = adminUser.Username
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&o).Error; err != nil {
			return err
		}
		groupName := model.DefaultGroupName
		group := model.Group{Name: &groupName, Describe: model.DefaultGroupDesc, OrgID: o.ID}
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		groupID := group.ID
		if err := tx.Create(&model.OrgMember{
			OrgID: o.ID, UserID: adminUID, Role: model.OrgRoleOrgAdmin,
			GroupID: &groupID, OrgDisplayName: displayName, JoinedAt: time.Now(),
		}).Error; err != nil {
			return err
		}
		return tx.Model(&model.User{}).Where("id = ?", adminUID).Update("current_org_id", o.ID).Error
	}); err != nil {
		log.Errorf("org create transaction: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "创建失败，请稍后重试"})
		return nil
	}
	s.invalidateOrgMembersCache(o.ID)
	s.invalidateDisplayCache(o.ID, adminUID)
	syncOrgMemberSystemRole(s.db, o.ID, adminUID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "创建成功", "data": s.orgToMapWithSeats(&o, true),
	})
	return nil
}

// handleDelete 站点管理员硬删除组织；公共域不可删
func (s *OrgService) handleDelete(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !auth.HasPerm(ctx, rbac.PermSiteOrgDelete) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无删除组织权限"})
		return nil
	}
	var req struct {
		ID uint `json:"id"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var o model.Org
	if s.db.First(&o, req.ID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "公共域不可删除"})
		return nil
	}

	var pub model.Org
	if s.db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "公共域不存在，无法迁移用户"})
		return nil
	}
	// 优先非 0 的默认分组（历史数据可能存在 id=0 的「默认分组」）
	pubDefID := s.ensureDefaultGroupID(pub.ID)
	var pubDefAlt uint
	_ = s.db.Model(&model.Group{}).
		Where("org_id = ? AND name IN ? AND id > 0", pub.ID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").Limit(1).Pluck("id", &pubDefAlt).Error
	if pubDefAlt > 0 {
		pubDefID = pubDefAlt
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 当前组织指向被删组织的用户 → 切回公共域
		if err := tx.Model(&model.User{}).
			Where("current_org_id = ?", o.ID).
			Update("current_org_id", pub.ID).Error; err != nil {
			return err
		}

		// 组织内分组 id（先迁用户，再删分组，避免 users.group_id 悬空）
		var groupIDs []uint
		if err := tx.Model(&model.Group{}).Where("org_id = ?", o.ID).Pluck("id", &groupIDs).Error; err != nil {
			return err
		}
		if len(groupIDs) > 0 {
			if err := tx.Model(&model.User{}).
				Where("group_id IN ?", groupIDs).
				Update("group_id", pubDefID).Error; err != nil {
				return err
			}
		}

		if err := tx.Where("org_id = ?", o.ID).Delete(&model.OrgMember{}).Error; err != nil {
			return err
		}
		if err := tx.Where("org_id = ?", o.ID).Delete(&model.OrgJoinRequest{}).Error; err != nil {
			return err
		}
		if err := tx.Where("org_id = ?", o.ID).Delete(&model.Group{}).Error; err != nil {
			return err
		}
		// RBAC：组织角色指派、组织自定义角色及其权限一并清理
		if err := tx.Where("org_id = ?", o.ID).Delete(&model.UserRole{}).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM role_permissions rp USING roles r WHERE rp.role_id = r.id AND r.org_id = ?`, o.ID).Error; err != nil {
			return err
		}
		if err := tx.Where("org_id = ?", o.ID).Delete(&model.Role{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&o).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		log.Errorf("org delete: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "删除失败，请稍后重试"})
		return nil
	}
	s.invalidateOrgMembersCache(o.ID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除组织"})
	return nil
}

func (s *OrgService) handleUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID                   uint    `json:"id"`
		Name                 *string `json:"name"`
		Status               *string `json:"status"`
		BrandTitle           *string `json:"brandTitle"`
		BrandLogo            *string `json:"brandLogo"`
		BrandFavicon         *string `json:"brandFavicon"`
		JoinMode             *string `json:"joinMode"`
		EnableAISummary      *bool   `json:"enableAiSummary"`
		EnableAIEmail        *bool   `json:"enableAiEmail"`
		EnableAIWeeklyEmail  *bool   `json:"enableAiWeeklyEmail"`
		EnableSpider         *bool   `json:"enableSpider"`
		SpiderIntervalMin    *int    `json:"spiderIntervalMin"`
		AISummaryIntervalMin *int    `json:"aiSummaryIntervalMin"`
		AIEmailSchedule      *string `json:"aiEmailSchedule"`
		SeatLimit            *int    `json:"seatLimit"`
		ForceSync            *bool   `json:"forceSync"` // 仅站管
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var o model.Org
	if err := s.db.First(&o, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	siteAdmin := auth.VerifySiteAdmin(ctx)
	// 字段级权限：品牌/名称/加入方式=org.info.write；功能开关=org.policy.toggle；
	// 状态/席位/间隔/强制同步等站点策略=site.org.policy（站点管理员旁路全部）。
	canInfo := verifyOrgPerm(ctx, s.db, pd.UserID, req.ID, rbac.PermOrgInfoWrite)
	canToggle := verifyOrgPerm(ctx, s.db, pd.UserID, req.ID, rbac.PermOrgPolicyToggle)
	canSitePolicy := auth.HasPerm(ctx, rbac.PermSiteOrgPolicy)
	if !canInfo && !canToggle && !canSitePolicy {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}

	updates := map[string]interface{}{}
	if canInfo {
		// 品牌字段使用 PATCH 语义；显式传空串才表示清空。
		if req.BrandTitle != nil {
			updates["brand_title"] = strings.TrimSpace(*req.BrandTitle)
		}
		if req.BrandLogo != nil {
			updates["brand_logo"] = strings.TrimSpace(*req.BrandLogo)
		}
		if req.BrandFavicon != nil {
			updates["brand_favicon"] = strings.TrimSpace(*req.BrandFavicon)
		}
		if req.JoinMode != nil && (*req.JoinMode == model.OrgJoinAuto || *req.JoinMode == model.OrgJoinReview) {
			updates["join_mode"] = *req.JoinMode
		}
		// 名称：公共域改名属站点策略
		if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
			if !o.IsSystem || canSitePolicy {
				updates["name"] = strings.TrimSpace(*req.Name)
			}
		}
	}
	if canToggle {
		if req.EnableAISummary != nil {
			updates["enable_ai_summary"] = *req.EnableAISummary
		}
		if req.EnableAIEmail != nil {
			updates["enable_ai_email"] = *req.EnableAIEmail
		}
		if req.EnableAIWeeklyEmail != nil {
			updates["enable_ai_weekly_email"] = *req.EnableAIWeeklyEmail
		}
		if req.EnableSpider != nil {
			updates["enable_spider"] = *req.EnableSpider
		}
	}

	// 间隔 / 状态 / 用户数上限 / 强制同步：站点策略
	if canSitePolicy {
		if req.Status != nil && (*req.Status == model.OrgStatusActive || *req.Status == model.OrgStatusSuspended) {
			if !o.IsSystem {
				updates["status"] = *req.Status
			}
		}
		// 间隔：5 分钟～7 天（与个人覆盖 / cron claim 一致）
		const minM, maxM = 5, 7 * 24 * 60
		if req.SpiderIntervalMin != nil {
			v := *req.SpiderIntervalMin
			if v < minM || v > maxM {
				writeJSON(ctx.Response(), 400, map[string]interface{}{
					"code": 1, "message": fmt.Sprintf("爬取间隔须为 %d–%d 分钟", minM, maxM),
				})
				return nil
			}
			updates["spider_interval_min"] = v
		}
		if req.AISummaryIntervalMin != nil {
			v := *req.AISummaryIntervalMin
			if v < minM || v > maxM {
				writeJSON(ctx.Response(), 400, map[string]interface{}{
					"code": 1, "message": fmt.Sprintf("AI 总结间隔须为 %d–%d 分钟", minM, maxM),
				})
				return nil
			}
			updates["ai_summary_interval_min"] = v
		}
		if req.AIEmailSchedule != nil && strings.TrimSpace(*req.AIEmailSchedule) != "" {
			updates["ai_email_schedule"] = strings.TrimSpace(*req.AIEmailSchedule)
		}
		if req.SeatLimit != nil {
			if *req.SeatLimit < 1 {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "用户数上限至少为 1"})
				return nil
			}
			updates["seat_limit"] = *req.SeatLimit
		}
		if req.ForceSync != nil {
			updates["force_sync"] = *req.ForceSync
		}
	}

	if err := s.db.Model(&o).Updates(updates).Error; err != nil {
		log.Errorf("org update: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败，请稍后重试"})
		return nil
	}
	// 组织关闭日报授权后：无其它组织授权的用户强制关闭个人日报
	if req.EnableAIEmail != nil && !*req.EnableAIEmail {
		s.forceOffDailyEmailWithoutOrgGrant(req.ID)
	}
	if req.EnableAIWeeklyEmail != nil && !*req.EnableAIWeeklyEmail {
		s.forceOffWeeklyEmailWithoutOrgGrant(req.ID)
	}
	_ = s.db.First(&o, req.ID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "data": s.orgToMapWithSeats(&o, siteAdmin || canInfo || canToggle),
	})
	return nil
}

// forceOffDailyEmailWithoutOrgGrant 关闭日报组织授权后，对仅依赖该组织授权的用户关个人日报。
// 集合式单条 UPDATE + RETURNING 受影响 id，避免逐用户 N 次查询/更新。
func (s *OrgService) forceOffDailyEmailWithoutOrgGrant(changedOrgID uint) {
	var affected []uint
	err := s.db.Raw(`
		UPDATE users SET email_enabled = false
		WHERE email_enabled = true
		  AND id IN (SELECT user_id FROM org_members WHERE org_id = ?)
		  AND NOT EXISTS (
			SELECT 1 FROM org_members m
			JOIN orgs o ON o.id = m.org_id
			WHERE m.user_id = users.id AND o.status = ? AND o.enable_ai_email = true
		  )
		RETURNING id
	`, changedOrgID, model.OrgStatusActive).Scan(&affected).Error
	if err != nil {
		log.Errorf("org force off daily email org=%d: %v", changedOrgID, err)
		return
	}
	s.invalidateUserProfileCaches(affected)
}

func (s *OrgService) forceOffWeeklyEmailWithoutOrgGrant(changedOrgID uint) {
	var affected []uint
	err := s.db.Raw(`
		UPDATE users SET email_weekly_enabled = false
		WHERE email_weekly_enabled = true
		  AND id IN (SELECT user_id FROM org_members WHERE org_id = ?)
		  AND NOT EXISTS (
			SELECT 1 FROM org_members m
			JOIN orgs o ON o.id = m.org_id
			WHERE m.user_id = users.id AND o.status = ?
			  AND o.enable_ai_weekly_email = true AND m.role IN ?
		  )
		RETURNING id
	`, changedOrgID, model.OrgStatusActive,
		[]string{model.OrgRoleCoach, model.OrgRoleGroupLeader, model.OrgRoleCaptain, model.OrgRoleOrgAdmin}).Scan(&affected).Error
	if err != nil {
		log.Errorf("org force off weekly email org=%d: %v", changedOrgID, err)
		return
	}
	s.invalidateUserProfileCaches(affected)
}

// invalidateUserProfileCaches 按受影响的用户 id 列表批量失效缓存（单次 DEL 多 key）
func (s *OrgService) invalidateUserProfileCaches(userIDs []uint) {
	if s == nil || s.rdb == nil || len(userIDs) == 0 {
		return
	}
	keys := make([]string, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == 0 {
			continue
		}
		keys = append(keys, fmt.Sprintf("user:%d:profile", uid))
	}
	if len(keys) == 0 {
		return
	}
	_ = s.rdb.Del(context.Background(), keys...).Err()
}

func (s *OrgService) handleSwitch(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID uint `json:"orgId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if !s.isMemberDB(pd.UserID, req.OrgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "你不是该组织成员"})
		return nil
	}
	var targetOrg model.Org
	if err := s.db.Select("id", "status").First(&targetOrg, req.OrgID).Error; err != nil || targetOrg.Status != model.OrgStatusActive {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "该组织当前已暂停"})
		return nil
	}
	u, err := s.loadUser(pd.UserID)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "用户不存在"})
		return nil
	}
	_ = s.db.Model(u).Update("current_org_id", req.OrgID).Error
	u.CurrentOrgID = req.OrgID
	token, err := IssueJWT(s.db, u)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "签发 token 失败"})
		return nil
	}
	setSessionCookie(ctx, token)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已切换组织", "jwtToken": token, "orgId": req.OrgID,
	})
	return nil
}

func (s *OrgService) handleJoin(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		InviteCode     string `json:"inviteCode"`
		OrgDisplayName string `json:"orgDisplayName"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	code := strings.TrimSpace(strings.ToUpper(req.InviteCode))
	if code == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请输入团队识别码"})
		return nil
	}
	displayName := strings.TrimSpace(req.OrgDisplayName)
	if displayName == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请填写组织内名称（在本团队中展示的称呼）"})
		return nil
	}
	if len([]rune(displayName)) > 32 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "组织内名称过长（最多 32 字）"})
		return nil
	}
	var o model.Org
	if err := s.db.Where("UPPER(invite_code) = ? AND status = ?", code, model.OrgStatusActive).First(&o).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "团队识别码无效"})
		return nil
	}
	if s.isMemberDB(pd.UserID, o.ID) {
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "你已在该组织中", "data": s.orgToMapWithSeats(&o, false)})
		return nil
	}
	if o.JoinMode == model.OrgJoinReview {
		var existing model.OrgJoinRequest
		err := s.db.Where("org_id = ? AND user_id = ?", o.ID, pd.UserID).First(&existing).Error
		if err == nil {
			if existing.Status != model.JoinReqPending {
				if updateErr := s.db.Model(&existing).Updates(map[string]interface{}{
					"status": model.JoinReqPending, "code_used": code,
					"org_display_name": displayName, "reviewed_by": nil,
				}).Error; updateErr != nil {
					writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "提交申请失败，请稍后重试"})
					return nil
				}
			}
			writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "申请已提交，等待团队管理员审批"})
			return nil
		}
		if err := s.db.Create(&model.OrgJoinRequest{
			OrgID:          o.ID,
			UserID:         pd.UserID,
			Status:         model.JoinReqPending,
			CodeUsed:       code,
			OrgDisplayName: displayName,
		}).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "提交申请失败，请稍后重试"})
			return nil
		}
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "申请已提交，等待团队管理员审批"})
		return nil
	}
	if err := s.addOrgMemberAtomic(o.ID, pd.UserID, model.OrgRoleMember, displayName); err != nil {
		log.Errorf("org join ensure member: %v", err)
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": err.Error()})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "加入成功", "data": s.orgToMapWithSeats(&o, false)})
	return nil
}

func (s *OrgService) handleLeave(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID uint `json:"orgId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var o model.Org
	if err := s.db.First(&o, req.OrgID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "公共域不可退出"})
		return nil
	}
	var membership model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", req.OrgID, pd.UserID).First(&membership).Error == nil && membership.Role == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", req.OrgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请先任命另一位组织管理员再退出"})
			return nil
		}
	}
	if err := s.db.Where("org_id = ? AND user_id = ?", req.OrgID, pd.UserID).Delete(&model.OrgMember{}).Error; err != nil {
		log.Errorf("org leave: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "退出失败，请稍后重试"})
		return nil
	}
	s.invalidateOrgMembersCache(req.OrgID)
	s.invalidateDisplayCache(req.OrgID, pd.UserID)
	// membership 已删 → 清除该组织全部角色指派（含自定义）
	syncOrgMemberSystemRole(s.db, req.OrgID, pd.UserID)
	// 若当前组织是离开的组织，切回公共域
	u, _ := s.loadUser(pd.UserID)
	token := ""
	if u != nil && u.CurrentOrgID == req.OrgID {
		var pub model.Org
		if s.db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error == nil {
			_ = s.db.Model(u).Update("current_org_id", pub.ID).Error
			u.CurrentOrgID = pub.ID
			token, _ = IssueJWT(s.db, u)
		}
	}
	resp := map[string]interface{}{"code": 0, "message": "已退出组织"}
	if token != "" {
		setSessionCookie(ctx, token)
		resp["jwtToken"] = token
	}
	writeJSON(ctx.Response(), 200, resp)
	return nil
}

func (s *OrgService) handleMembers(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	q := ctx.Request().URL.Query()
	id64, _ := strconv.ParseUint(q.Get("orgId"), 10, 64)
	orgID := uint(id64)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !auth.VerifySiteAdmin(ctx) && !s.isMemberDB(pd.UserID, orgID) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	keyword := strings.TrimSpace(q.Get("keyword"))

	type row struct {
		UserID         uint
		Username       string
		Name           string
		OrgDisplayName string
		Avatar         string
		Role           string
		GroupID        *uint
		JoinedAt       time.Time
	}
	base := s.db.Table("org_members AS m").
		Select(`m.user_id AS user_id, u.username AS username, u.name AS name,
			COALESCE(m.org_display_name,'') AS org_display_name,
			u.avatar AS avatar, m.role AS role, m.group_id AS group_id, m.joined_at AS joined_at`).
		Joins("JOIN users u ON u.id = m.user_id").
		Where("m.org_id = ?", orgID)
	if keyword != "" {
		like := "%" + keyword + "%"
		base = base.Where("u.name ILIKE ? OR u.username ILIKE ? OR m.org_display_name ILIKE ?", like, like, like)
	}

	var total int64
	countQ := s.db.Table("org_members AS m").
		Joins("JOIN users u ON u.id = m.user_id").
		Where("m.org_id = ?", orgID)
	if keyword != "" {
		like := "%" + keyword + "%"
		countQ = countQ.Where("u.name ILIKE ? OR u.username ILIKE ? OR m.org_display_name ILIKE ?", like, like, like)
	}
	_ = countQ.Count(&total).Error

	var rows []row
	// 角色等级：组织管理员 > 教练 > 组长 > 队长 > 成员
	_ = base.Order(`CASE m.role
		WHEN 'org_admin' THEN 1
		WHEN 'coach' THEN 2
		WHEN 'group_leader' THEN 3
		WHEN 'captain' THEN 4
		WHEN 'member' THEN 5
		ELSE 6
	END ASC, m.id ASC`).
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&rows).Error

	// 批量加载管理范围（组长/队长绑定的分组或分队）
	uids := make([]uint, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.UserID)
	}
	scopeByUser := map[uint][]map[string]interface{}{}
	if len(uids) > 0 {
		var grants []model.OrgScopeGrant
		_ = s.db.Where("org_id = ? AND user_id IN ?", orgID, uids).Find(&grants).Error
		for _, g := range grants {
			scopeByUser[g.UserID] = append(scopeByUser[g.UserID], map[string]interface{}{
				"scopeType": g.ScopeType,
				"scopeId":   g.ScopeID,
			})
		}
	}

	list := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		// 组织内展示仅用 org_display_name；空则回退 username（不再回退全局昵称）
		display := strings.TrimSpace(r.OrgDisplayName)
		if display == "" {
			display = r.Username
		}
		item := map[string]interface{}{
			"userId":         r.UserID,
			"username":       r.Username,
			"name":           display,
			"orgDisplayName": r.OrgDisplayName,
			"avatar":         r.Avatar,
			"role":           r.Role,
			"groupId":        r.GroupID,
			"joinedAt":       r.JoinedAt.Unix(),
		}
		if sc := scopeByUser[r.UserID]; len(sc) > 0 {
			item["scopes"] = sc
		}
		list = append(list, item)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "list": list, "total": total, "page": page, "pageSize": pageSize,
	})
	return nil
}

func (s *OrgService) handleMemberIds(ctx khttp.Context) error {
	q := ctx.Request().URL.Query()
	id64, _ := strconv.ParseUint(q.Get("orgId"), 10, 64)
	orgID := uint(id64)
	pd := auth.GetCurrentUser(ctx)
	if orgID == 0 && pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少组织 id"})
		return nil
	}
	if pd == nil || (!auth.VerifySiteAdmin(ctx) && !s.isMemberDB(pd.UserID, orgID)) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	groupID, _ := strconv.ParseInt(q.Get("groupId"), 10, 64)
	squadID, _ := strconv.ParseInt(q.Get("squadId"), 10, 64)
	var ids []int64
	switch {
	case squadID > 0:
		_ = s.db.Table("squad_members").Where("squad_id = ?", squadID).Pluck("user_id", &ids)
	case groupID > 0:
		_ = s.db.Table("org_members").Where("org_id = ? AND group_id = ?", orgID, groupID).Pluck("user_id", &ids)
	default:
		_ = s.db.Table("org_members AS m").
			Joins("JOIN users u ON u.id = m.user_id").
			Where("m.org_id = ?", orgID).
			Pluck("m.user_id", &ids)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "userIds": ids, "orgId": orgID,
		"groupId": groupID, "squadId": squadID,
	})
	return nil
}

// handleAddMember 站点管理员搜索加入：按 userId 或 username
func (s *OrgService) handleAddMember(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID          uint   `json:"orgId"`
		UserID         uint   `json:"userId"`
		Username       string `json:"username"`
		Role           string `json:"role"`
		OrgDisplayName string `json:"orgDisplayName"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	// 站点管理员可操作任意 org；组织内需 org.member.add 权限
	if !verifyOrgPerm(ctx, s.db, pd.UserID, req.OrgID, rbac.PermOrgMemberAdd) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	uid := req.UserID
	if uid == 0 && strings.TrimSpace(req.Username) != "" {
		var u model.User
		if s.db.Where("username = ?", strings.TrimSpace(req.Username)).First(&u).Error != nil {
			// 尝试按昵称模糊
			if s.db.Where("name LIKE ?", "%"+strings.TrimSpace(req.Username)+"%").First(&u).Error != nil {
				writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
				return nil
			}
		}
		uid = u.ID
	}
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请提供 userId 或 username"})
		return nil
	}
	role := req.Role
	if !model.ValidOrgRole(role) {
		role = model.OrgRoleMember
	}
	if s.isMemberDB(uid, req.OrgID) {
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "用户已在组织中", "userId": uid})
		return nil
	}
	var addOrg model.Org
	if s.db.First(&addOrg, req.OrgID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	displayName := strings.TrimSpace(req.OrgDisplayName)
	if displayName == "" {
		// 管理员未填：用目标用户全局昵称作占位，用户可再改
		var u model.User
		if s.db.Select("name", "username").First(&u, uid).Error == nil {
			displayName = strings.TrimSpace(u.Name)
			if displayName == "" {
				displayName = u.Username
			}
		}
	}
	if err := s.addOrgMemberAtomic(req.OrgID, uid, role, displayName); err != nil {
		log.Errorf("org add member: %v", err)
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": err.Error()})
		return nil
	}
	// 管理员拉入 → 设为默认组织（下次打开自动进入；用户之后 switch 即记忆）
	s.setDefaultOrg(uid, req.OrgID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已加入组织", "userId": uid})
	return nil
}

// handleSetDisplayName 本人或组织/站点管理员修改组织内名称
func (s *OrgService) handleSetDisplayName(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID          uint   `json:"orgId"`
		UserID         uint   `json:"userId"`
		OrgDisplayName string `json:"orgDisplayName"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	uid := req.UserID
	if uid == 0 {
		uid = pd.UserID
	}
	displayName := strings.TrimSpace(req.OrgDisplayName)
	if displayName == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "组织内名称不能为空"})
		return nil
	}
	if len([]rune(displayName)) > 32 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "组织内名称过长（最多 32 字）"})
		return nil
	}
	if uid != pd.UserID && !verifyOrgPerm(ctx, s.db, pd.UserID, req.OrgID, rbac.PermOrgMemberDisplayName) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	if !s.isMemberDB(uid, req.OrgID) {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不在该组织中"})
		return nil
	}
	if err := s.db.Model(&model.OrgMember{}).
		Where("org_id = ? AND user_id = ?", req.OrgID, uid).
		Update("org_display_name", displayName).Error; err != nil {
		log.Errorf("org member display name: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败，请稍后重试"})
		return nil
	}
	// 公共域称呼 ≡ 全局昵称 users.name
	var o model.Org
	if s.db.Select("id", "slug", "is_system").First(&o, req.OrgID).Error == nil &&
		(o.IsSystem || o.Slug == model.PublicOrgSlug) {
		_ = s.db.Model(&model.User{}).Where("id = ?", uid).Update("name", displayName).Error
	}
	// 旁路更新 users.name 后清资料缓存，避免编辑页仍显示旧昵称/旧字段
	s.invalidateUserProfileCache(uid)
	s.invalidateDisplayCache(req.OrgID, uid)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已更新组织内名称"})
	return nil
}

func (s *OrgService) handleSetRole(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID     uint   `json:"orgId"`
		UserID    uint   `json:"userId"`
		Role      string `json:"role"`
		ScopeType string `json:"scopeType"` // captain→squad；group_leader→group
		ScopeID   uint   `json:"scopeId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if !model.ValidOrgRole(req.Role) {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "角色无效（member|captain|group_leader|coach|org_admin）"})
		return nil
	}

	// 权限：站管 / 持有 org.member.role（教练、组长、组织管理员默认模板含此权限）
	isSite := auth.VerifySiteAdmin(ctx)
	if !isSite && !verifyOrgPerm(ctx, s.db, pd.UserID, req.OrgID, rbac.PermOrgMemberRole) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}

	// 操作者在本组织的角色（站管视为最高，可任命任意）
	actorRole := model.OrgRoleMember
	if isSite {
		actorRole = model.OrgRoleOrgAdmin
	} else {
		var actor model.OrgMember
		if s.db.Where("org_id = ? AND user_id = ?", req.OrgID, pd.UserID).First(&actor).Error == nil {
			actorRole = actor.Role
		}
	}

	// 目标当前角色
	targetCurrent := model.OrgRoleMember
	var m model.OrgMember
	inOrg := s.db.Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).First(&m).Error == nil
	if inOrg {
		targetCurrent = m.Role
	}

	// 等级：只能任命严格低于自己的角色，且不能改同级或更高
	if !isSite && !model.CanAppointOrgRole(actorRole, targetCurrent, req.Role) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{
			"code": 1, "message": "只能任命低于自己的角色，且不能修改同级或更高身份的成员",
		})
		return nil
	}

	// 最后一位组织管理员不可降权
	if req.Role != model.OrgRoleOrgAdmin && targetCurrent == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", req.OrgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不能降权最后一位组织管理员"})
			return nil
		}
	}

	// 组长/队长必须绑定范围；教练/组织管理员/成员清空范围
	var grants []model.OrgScopeGrant
	if needType, need := model.RoleNeedsScope(req.Role); need {
		st := strings.TrimSpace(req.ScopeType)
		if st == "" {
			st = needType
		}
		if st != needType || req.ScopeID == 0 {
			msg := "任命队长须指定分队"
			if req.Role == model.OrgRoleGroupLeader {
				msg = "任命组长须指定分组"
			}
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": msg})
			return nil
		}
		// scope 须属本组织
		if st == model.ScopeTypeGroup {
			var n int64
			_ = s.db.Model(&model.Group{}).Where("id = ? AND org_id = ?", req.ScopeID, req.OrgID).Count(&n).Error
			if n == 0 {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "分组不存在或不属于本组织"})
				return nil
			}
		} else {
			var sq model.Squad
			if s.db.Where("id = ? AND org_id = ?", req.ScopeID, req.OrgID).First(&sq).Error != nil {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "分队不存在或不属于本组织"})
				return nil
			}
			// 组长任命队长：分队必须落在自己管理的分组内
			if !isSite && actorRole == model.OrgRoleGroupLeader {
				if !s.actorControlsGroup(req.OrgID, pd.UserID, sq.GroupID) {
					writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "只能任命自己管理分组内的队长"})
					return nil
				}
			}
		}
		// 组长任命组长：只能指定自己管理的分组（一般不应发生，等级已禁止；双保险）
		if !isSite && actorRole == model.OrgRoleGroupLeader && req.Role == model.OrgRoleGroupLeader {
			if !s.actorControlsGroup(req.OrgID, pd.UserID, req.ScopeID) {
				writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "只能在自己管理的分组内操作"})
				return nil
			}
		}
		grants = []model.OrgScopeGrant{{
			OrgID: req.OrgID, UserID: req.UserID, ScopeType: st, ScopeID: req.ScopeID,
		}}
	}

	if !inOrg {
		var roleOrg model.Org
		if s.db.First(&roleOrg, req.OrgID).Error != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
			return nil
		}
		displayName := ""
		var u model.User
		if s.db.Select("name", "username").First(&u, req.UserID).Error == nil {
			displayName = strings.TrimSpace(u.Name)
			if displayName == "" {
				displayName = u.Username
			}
		}
		if err := s.addOrgMemberAtomic(req.OrgID, req.UserID, req.Role, displayName); err != nil {
			log.Errorf("org set role ensure member: %v", err)
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": err.Error()})
			return nil
		}
		s.setDefaultOrg(req.UserID, req.OrgID)
	} else {
		if err := s.db.Model(&m).Update("role", req.Role).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "更新角色失败"})
			return nil
		}
		syncOrgMemberSystemRole(s.db, req.OrgID, req.UserID)
	}

	// 覆盖写入管理范围（教练/组织管理员/成员 → 清空）
	if err := s.replaceUserScopeGrants(req.OrgID, req.UserID, grants); err != nil {
		log.Errorf("org set role scopes: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "更新管理范围失败"})
		return nil
	}

	// 任命组长时，将其 org_members.group_id 同步到所管分组（便于列表展示）
	if req.Role == model.OrgRoleGroupLeader && req.ScopeID > 0 {
		_ = s.db.Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).
			Update("group_id", req.ScopeID).Error
	}
	// 任命队长时，同步到分队所属分组并加入该分队
	if req.Role == model.OrgRoleCaptain && req.ScopeID > 0 {
		var sq model.Squad
		if s.db.First(&sq, req.ScopeID).Error == nil {
			_ = s.db.Model(&model.OrgMember{}).
				Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).
				Update("group_id", sq.GroupID).Error
			_ = s.db.Exec(`
				DELETE FROM squad_members WHERE user_id = ? AND squad_id IN (
					SELECT id FROM squads WHERE org_id = ?
				)`, req.UserID, req.OrgID).Error
			sm := model.SquadMember{SquadID: sq.ID, UserID: req.UserID}
			_ = s.db.Where("squad_id = ? AND user_id = ?", sq.ID, req.UserID).FirstOrCreate(&sm).Error
		}
	}

	outScopes := make([]map[string]interface{}, 0, len(grants))
	for _, g := range grants {
		outScopes = append(outScopes, map[string]interface{}{
			"scopeType": g.ScopeType, "scopeId": g.ScopeID,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已更新角色",
		"role": req.Role, "scopes": outScopes,
	})
	return nil
}

// actorControlsGroup 操作者是否管理该分组（组长 grant 含该 group，或全组织角色）
func (s *OrgService) actorControlsGroup(orgID, userID, groupID uint) bool {
	if groupID == 0 || userID == 0 {
		return false
	}
	var role string
	_ = s.db.Model(&model.OrgMember{}).Select("role").
		Where("org_id = ? AND user_id = ?", orgID, userID).Scan(&role).Error
	if model.IsOrgFullScopeRole(role) {
		return true
	}
	var n int64
	_ = s.db.Model(&model.OrgScopeGrant{}).
		Where("org_id = ? AND user_id = ? AND scope_type = ? AND scope_id = ?",
			orgID, userID, model.ScopeTypeGroup, groupID).Count(&n).Error
	return n > 0
}

// replaceUserScopeGrants 覆盖用户在组织内的管理范围
func (s *OrgService) replaceUserScopeGrants(orgID, userID uint, grants []model.OrgScopeGrant) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("org_id = ? AND user_id = ?", orgID, userID).
			Delete(&model.OrgScopeGrant{}).Error; err != nil {
			return err
		}
		if len(grants) == 0 {
			return nil
		}
		for i := range grants {
			grants[i].OrgID = orgID
			grants[i].UserID = userID
		}
		return tx.Create(&grants).Error
	})
}

func (s *OrgService) handleRemoveMember(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID  uint `json:"orgId"`
		UserID uint `json:"userId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.OrgID == 0 || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var o model.Org
	if s.db.First(&o, req.OrgID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	if o.IsSystem {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "不能将成员移出公共域"})
		return nil
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, req.OrgID, rbac.PermOrgMemberRemove) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	var target model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).First(&target).Error == nil && target.Role == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", req.OrgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不能移除最后一位组织管理员"})
			return nil
		}
	}
	if err := s.db.Where("org_id = ? AND user_id = ?", req.OrgID, req.UserID).Delete(&model.OrgMember{}).Error; err != nil {
		log.Errorf("org remove member: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "移除失败，请稍后重试"})
		return nil
	}
	s.invalidateOrgMembersCache(req.OrgID)
	s.invalidateDisplayCache(req.OrgID, req.UserID)
	// membership 已删 → 清除该组织全部角色指派（含自定义）
	syncOrgMemberSystemRole(s.db, req.OrgID, req.UserID)
	// 若被移出的是其默认组织，回落公共域
	s.fallbackDefaultOrgIf(req.UserID, req.OrgID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已移除成员"})
	return nil
}

func (s *OrgService) handleInviteGet(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	id64, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("orgId"), 10, 64)
	orgID := uint(id64)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgInviteView) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	var o model.Org
	if s.db.First(&o, orgID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"inviteCode": o.InviteCode, "joinMode": o.JoinMode, "orgId": o.ID,
	})
	return nil
}

func (s *OrgService) handleInviteRotate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		OrgID uint `json:"orgId"`
	}
	_ = readJSON(ctx.Request(), &req)
	orgID := req.OrgID
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgInviteRotate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	code := newInviteCode()
	if err := s.db.Model(&model.Org{}).Where("id = ?", orgID).Update("invite_code", code).Error; err != nil {
		log.Errorf("org rotate invite: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "更新失败，请稍后重试"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已更换团队识别码", "inviteCode": code})
	return nil
}

func (s *OrgService) handleJoinRequests(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	id64, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("orgId"), 10, 64)
	orgID := uint(id64)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgJoinReview) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	// 最多取最近 200 条待审申请；用户信息批量 IN 查询，消除逐条 First
	const maxJoinRequests = 200
	var reqs []model.OrgJoinRequest
	_ = s.db.Where("org_id = ? AND status = ?", orgID, model.JoinReqPending).
		Order("id DESC").Limit(maxJoinRequests).Find(&reqs).Error
	userIDs := make([]uint, 0, len(reqs))
	for _, r := range reqs {
		if r.UserID > 0 {
			userIDs = append(userIDs, r.UserID)
		}
	}
	userByID := map[uint]model.User{}
	if len(userIDs) > 0 {
		var us []model.User
		_ = s.db.Select("id", "username").Where("id IN ?", userIDs).Find(&us).Error
		for _, u := range us {
			userByID[u.ID] = u
		}
	}
	list := make([]map[string]interface{}, 0, len(reqs))
	for _, r := range reqs {
		u := userByID[r.UserID]
		display := strings.TrimSpace(r.OrgDisplayName)
		if display == "" {
			display = u.Username
		}
		list = append(list, map[string]interface{}{
			"id": r.ID, "userId": r.UserID, "username": u.Username,
			"name":           display,
			"orgDisplayName": r.OrgDisplayName,
			"status":         r.Status, "createdAt": r.CreatedAt.Unix(),
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": list})
	return nil
}

func (s *OrgService) handleJoinReview(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID      uint `json:"id"`
		Approve bool `json:"approve"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var jr model.OrgJoinRequest
	if s.db.First(&jr, req.ID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "申请不存在"})
		return nil
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, jr.OrgID, rbac.PermOrgJoinReview) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "权限不足"})
		return nil
	}
	uid := pd.UserID
	if req.Approve {
		if !s.isMemberDB(jr.UserID, jr.OrgID) {
			var reviewOrg model.Org
			if s.db.First(&reviewOrg, jr.OrgID).Error != nil {
				writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "组织不存在"})
				return nil
			}
			displayName := strings.TrimSpace(jr.OrgDisplayName)
			if displayName == "" {
				var u model.User
				if s.db.Select("name", "username").First(&u, jr.UserID).Error == nil {
					displayName = strings.TrimSpace(u.Name)
					if displayName == "" {
						displayName = u.Username
					}
				}
			}
			// 先写入/恢复成员，成功后再标记申请通过，避免“已通过却未入组”
			if err := s.addOrgMemberAtomic(jr.OrgID, jr.UserID, model.OrgRoleMember, displayName); err != nil {
				log.Errorf("org join review ensure member: %v", err)
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": err.Error()})
				return nil
			}
			s.setDefaultOrg(jr.UserID, jr.OrgID)
		}
		_ = s.db.Model(&jr).Updates(map[string]interface{}{
			"status": model.JoinReqApproved, "reviewed_by": uid,
		}).Error
		orgName := s.orgName(jr.OrgID)
		_ = CreateNotification(s.db, model.Notification{
			UserID:  jr.UserID,
			Type:    model.NotifTypeOrgJoinApproved,
			Title:   "加入组织申请已通过",
			Body:    "你加入「" + orgName + "」的申请已通过",
			ActorID: uid,
			RefType: "org_join",
			RefID:   jr.ID,
		})
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已通过"})
		return nil
	}
	_ = s.db.Model(&jr).Updates(map[string]interface{}{
		"status": model.JoinReqRejected, "reviewed_by": uid,
	}).Error
	orgName := s.orgName(jr.OrgID)
	_ = CreateNotification(s.db, model.Notification{
		UserID:  jr.UserID,
		Type:    model.NotifTypeOrgJoinRejected,
		Title:   "加入组织申请未通过",
		Body:    "你加入「" + orgName + "」的申请未通过",
		ActorID: uid,
		RefType: "org_join",
		RefID:   jr.ID,
	})
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已拒绝"})
	return nil
}

func (s *OrgService) orgName(orgID uint) string {
	var o model.Org
	if s.db.Select("name").First(&o, orgID).Error == nil && strings.TrimSpace(o.Name) != "" {
		return o.Name
	}
	return "组织"
}

func (s *OrgService) handleSetSiteAdmin(ctx khttp.Context) error {
	if !auth.HasPerm(ctx, rbac.PermSiteAppointAdmin) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无任命站点管理员权限"})
		return nil
	}
	var req struct {
		UserID      uint `json:"userId"`
		IsSiteAdmin bool `json:"isSiteAdmin"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	// 防止撤销最后一个站点管理员
	if !req.IsSiteAdmin {
		var n int64
		s.db.Model(&model.User{}).Where("is_site_admin = ?", true).Count(&n)
		var target model.User
		if s.db.First(&target, req.UserID).Error == nil && target.IsSiteAdmin && n <= 1 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不能撤销最后一位站点管理员"})
			return nil
		}
	}
	roleID := 0
	if req.IsSiteAdmin {
		roleID = 1
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", req.UserID).Updates(map[string]interface{}{
		"is_site_admin": req.IsSiteAdmin,
		"role_id":       roleID,
	}).Error; err != nil {
		log.Errorf("set site admin: %v", err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "更新失败，请稍后重试"})
		return nil
	}
	syncSiteSystemRole(s.db, req.UserID, rbac.RoleSiteAdmin, req.IsSiteAdmin)
	log.Infof("set site admin user=%d is=%v", req.UserID, req.IsSiteAdmin)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已更新"})
	return nil
}

func htmlEscapeName(name, username string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		n = strings.TrimSpace(username)
	}
	if n == "" {
		return ""
	}
	// 轻量转义，避免邮件 HTML 注入
	n = strings.ReplaceAll(n, "&", "&amp;")
	n = strings.ReplaceAll(n, "<", "&lt;")
	n = strings.ReplaceAll(n, ">", "&gt;")
	n = strings.ReplaceAll(n, `"`, "&quot;")
	return "，" + n
}
