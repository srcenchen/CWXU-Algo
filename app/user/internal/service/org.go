package service

import (
	"context"
	"crypto/rand"
	"cwxu-algo/app/common/utils/sqllike"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	orgpb "cwxu-algo/api/user/v1/org"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
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

func (s *OrgService) toOrgInfo(o *model.Org, includeInvite bool) *orgpb.OrgInfo {
	info := &orgpb.OrgInfo{
		Id:                   int64(o.ID),
		Name:                 o.Name,
		Slug:                 o.Slug,
		Plan:                 o.Plan,
		SeatLimit:            int32(effectiveSeatLimit(o.SeatLimit)),
		Status:               o.Status,
		IsSystem:             o.IsSystem,
		BrandTitle:           o.BrandTitle,
		BrandLogo:            o.BrandLogo,
		BrandFavicon:         o.BrandFavicon,
		JoinMode:             o.JoinMode,
		EnableAiSummary:      o.EnableAISummary,
		EnableAiEmail:        o.EnableAIEmail,
		EnableAiWeeklyEmail:  o.EnableAIWeeklyEmail,
		EnableSpider:         o.EnableSpider,
		SpiderIntervalMin:    int32(o.SpiderIntervalMin),
		AiSummaryIntervalMin: int32(o.AISummaryIntervalMin),
		AiEmailSchedule:      o.AIEmailSchedule,
		ForceSync:            o.ForceSync,
		MemberCount:          int32(countOrgSeats(s.db, o)),
	}
	if includeInvite {
		info.InviteCode = o.InviteCode
	}
	return info
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

// InvitePreview 公开：按识别码预览组织欢迎信息（不含敏感配置）
func (s *OrgService) InvitePreview(ctx context.Context, req *orgpb.InvitePreviewReq) (*orgpb.InvitePreviewRes, error) {
	code := strings.TrimSpace(strings.ToUpper(req.Code))
	if code == "" {
		return &orgpb.InvitePreviewRes{Code: 1, Message: "请提供邀请识别码"}, nil
	}
	var o model.Org
	if err := s.db.Where("UPPER(invite_code) = ? AND status = ?", code, model.OrgStatusActive).First(&o).Error; err != nil {
		return &orgpb.InvitePreviewRes{Code: 1, Message: "邀请链接无效或已失效"}, nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		return &orgpb.InvitePreviewRes{Code: 1, Message: "公共域无需邀请加入"}, nil
	}
	displayName := strings.TrimSpace(o.BrandTitle)
	if displayName == "" {
		displayName = o.Name
	}
	return &orgpb.InvitePreviewRes{
		Code: 0, Message: "success",
		OrgId: int64(o.ID), OrgName: displayName, Name: o.Name,
		BrandTitle: o.BrandTitle, BrandLogo: o.BrandLogo,
		JoinMode: o.JoinMode,
	}, nil
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

// Discover 组织广场：仅公开字段（名/logo/人数），无识别码与成员明细
func (s *OrgService) Discover(ctx context.Context, req *orgpb.DiscoverReq) (*orgpb.DiscoverRes, error) {
	pd := auth.GetCurrentUser(ctx)
	page := int(req.Page)
	pageSize := int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	kw := strings.TrimSpace(req.Q)

	dbq := s.db.Model(&model.Org{}).Where("status = ?", model.OrgStatusActive)
	if like := sqllike.Pattern(kw); like != "" {
		dbq = dbq.Where("name ILIKE ? OR brand_title ILIKE ?", like, like)
	}
	var total int64
	if err := dbq.Count(&total).Error; err != nil {
		return &orgpb.DiscoverRes{Code: 1, Message: "加载失败"}, nil
	}
	var orgs []model.Org
	if err := dbq.Order("is_system DESC, id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&orgs).Error; err != nil {
		return &orgpb.DiscoverRes{Code: 1, Message: "加载失败"}, nil
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

	list := make([]*orgpb.DiscoverOrgInfo, 0, len(orgs))
	for i := range orgs {
		o := &orgs[i]
		item := &orgpb.DiscoverOrgInfo{
			Id:          int64(o.ID),
			Name:        o.Name,
			BrandLogo:   o.BrandLogo,
			MemberCount: int32(countOrgSeats(s.db, o)),
			IsSystem:    o.IsSystem,
		}
		if pd != nil {
			item.IsMember = memberOf[o.ID]
			item.IsCurrent = o.ID == currentID
		}
		list = append(list, item)
	}
	return &orgpb.DiscoverRes{
		Code: 0, Message: "success", List: list, Total: int32(total),
	}, nil
}

func (s *OrgService) List(ctx context.Context, req *orgpb.ListReq) (*orgpb.ListRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.ListRes{Code: 1, Message: "请先登录"}, nil
	}
	mine := req.Mine != "0"

	var orgs []model.Org
	if req.All == "1" && auth.HasPerm(ctx, rbac.PermSiteOrgList) {
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
		list := make([]*orgpb.OrgInfo, 0, len(orgs))
		for i := range orgs {
			item := s.toOrgInfo(&orgs[i], false)
			item.MyRole = roleMap[orgs[i].ID]
			item.OrgDisplayName = displayMap[orgs[i].ID]
			item.IsCurrent = orgs[i].ID == pd.OrgID
			list = append(list, item)
		}
		return &orgpb.ListRes{Code: 0, Message: "success", List: list}, nil
	}

	list := make([]*orgpb.OrgInfo, 0, len(orgs))
	for i := range orgs {
		item := s.toOrgInfo(&orgs[i], pd.IsSiteAdmin)
		item.IsCurrent = orgs[i].ID == pd.OrgID
		list = append(list, item)
	}
	return &orgpb.ListRes{Code: 0, Message: "success", List: list}, nil
}

func (s *OrgService) Get(ctx context.Context, req *orgpb.GetReq) (*orgpb.GetRes, error) {
	pd := auth.GetCurrentUser(ctx)
	orgID := uint(req.Id)
	if orgID == 0 && pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		return &orgpb.GetRes{Code: 1, Message: "缺少组织 id"}, nil
	}
	var o model.Org
	if err := s.db.First(&o, orgID).Error; err != nil {
		return &orgpb.GetRes{Code: 1, Message: "组织不存在"}, nil
	}
	showInvite := pd != nil && (pd.IsSiteAdmin || hasPermInOrgDB(s.db, pd.UserID, orgID, rbac.PermOrgInviteView))
	item := s.toOrgInfo(&o, showInvite)
	if pd != nil {
		var m model.OrgMember
		if s.db.Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&m).Error == nil {
			item.MyRole = m.Role
			item.OrgDisplayName = strings.TrimSpace(m.OrgDisplayName)
		}
		item.IsCurrent = orgID == pd.OrgID
	}
	return &orgpb.GetRes{Code: 0, Message: "success", Data: item}, nil
}

func (s *OrgService) Create(ctx context.Context, req *orgpb.CreateReq) (*orgpb.CreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !auth.HasPerm(ctx, rbac.PermSiteOrgCreate) {
		return &orgpb.CreateRes{Code: 1, Message: "无创建组织权限"}, nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return &orgpb.CreateRes{Code: 1, Message: "组织名称不能为空"}, nil
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = "org-" + newInviteCode()
	}
	slug = strings.ToLower(slug)
	if slug == model.PublicOrgSlug {
		return &orgpb.CreateRes{Code: 1, Message: "slug 保留给公共域"}, nil
	}
	joinMode := req.JoinMode
	if joinMode != model.OrgJoinReview {
		joinMode = model.OrgJoinAuto
	}
	seatLimit := DefaultSeatLimit
	if req.SeatLimit != nil {
		if *req.SeatLimit < 1 {
			return &orgpb.CreateRes{Code: 1, Message: "用户数上限至少为 1"}, nil
		}
		seatLimit = int(*req.SeatLimit)
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
	adminUID := uint(req.AdminUserId)
	if adminUID == 0 {
		adminUID = pd.UserID
	}
	adminUser, err := s.loadUser(adminUID)
	if err != nil {
		return &orgpb.CreateRes{Code: 1, Message: "指定的组织管理员不存在"}, nil
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
		return &orgpb.CreateRes{Code: 1, Message: "创建失败，请稍后重试"}, nil
	}
	s.invalidateOrgMembersCache(o.ID)
	s.invalidateDisplayCache(o.ID, adminUID)
	syncOrgMemberSystemRole(s.db, o.ID, adminUID)
	return &orgpb.CreateRes{
		Code: 0, Message: "创建成功", Data: s.toOrgInfo(&o, true),
	}, nil
}

// Delete 站点管理员硬删除组织；公共域不可删
func (s *OrgService) Delete(ctx context.Context, req *orgpb.DeleteReq) (*orgpb.DeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !auth.HasPerm(ctx, rbac.PermSiteOrgDelete) {
		return &orgpb.DeleteRes{Code: 1, Message: "无删除组织权限"}, nil
	}
	orgID := uint(req.Id)
	if orgID == 0 {
		return &orgpb.DeleteRes{Code: 1, Message: "参数错误"}, nil
	}
	var o model.Org
	if s.db.First(&o, orgID).Error != nil {
		return &orgpb.DeleteRes{Code: 1, Message: "组织不存在"}, nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		return &orgpb.DeleteRes{Code: 1, Message: "公共域不可删除"}, nil
	}

	var pub model.Org
	if s.db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error != nil {
		return &orgpb.DeleteRes{Code: 1, Message: "公共域不存在，无法迁移用户"}, nil
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
		return &orgpb.DeleteRes{Code: 1, Message: "删除失败，请稍后重试"}, nil
	}
	s.invalidateOrgMembersCache(o.ID)
	return &orgpb.DeleteRes{Code: 0, Message: "已删除组织"}, nil
}

func (s *OrgService) Update(ctx context.Context, req *orgpb.UpdateReq) (*orgpb.UpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.UpdateRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.Id)
	if orgID == 0 {
		return &orgpb.UpdateRes{Code: 1, Message: "参数错误"}, nil
	}
	var o model.Org
	if err := s.db.First(&o, orgID).Error; err != nil {
		return &orgpb.UpdateRes{Code: 1, Message: "组织不存在"}, nil
	}
	siteAdmin := auth.VerifySiteAdmin(ctx)
	// 字段级权限：品牌/名称/加入方式=org.info.write；功能开关=org.policy.toggle；
	// 状态/席位/间隔/强制同步等站点策略=site.org.policy（站点管理员旁路全部）。
	canInfo := verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgInfoWrite)
	canToggle := verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgPolicyToggle)
	canSitePolicy := auth.HasPerm(ctx, rbac.PermSiteOrgPolicy)
	if !canInfo && !canToggle && !canSitePolicy {
		return &orgpb.UpdateRes{Code: 1, Message: "权限不足"}, nil
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
		if req.EnableAiSummary != nil {
			updates["enable_ai_summary"] = *req.EnableAiSummary
		}
		if req.EnableAiEmail != nil {
			updates["enable_ai_email"] = *req.EnableAiEmail
		}
		if req.EnableAiWeeklyEmail != nil {
			updates["enable_ai_weekly_email"] = *req.EnableAiWeeklyEmail
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
			v := int(*req.SpiderIntervalMin)
			if v < minM || v > maxM {
				return &orgpb.UpdateRes{
					Code: 1, Message: fmt.Sprintf("爬取间隔须为 %d–%d 分钟", minM, maxM),
				}, nil
			}
			updates["spider_interval_min"] = v
		}
		if req.AiSummaryIntervalMin != nil {
			v := int(*req.AiSummaryIntervalMin)
			if v < minM || v > maxM {
				return &orgpb.UpdateRes{
					Code: 1, Message: fmt.Sprintf("AI 总结间隔须为 %d–%d 分钟", minM, maxM),
				}, nil
			}
			updates["ai_summary_interval_min"] = v
		}
		if req.AiEmailSchedule != nil && strings.TrimSpace(*req.AiEmailSchedule) != "" {
			updates["ai_email_schedule"] = strings.TrimSpace(*req.AiEmailSchedule)
		}
		if req.SeatLimit != nil {
			if *req.SeatLimit < 1 {
				return &orgpb.UpdateRes{Code: 1, Message: "用户数上限至少为 1"}, nil
			}
			updates["seat_limit"] = int(*req.SeatLimit)
		}
		if req.ForceSync != nil {
			updates["force_sync"] = *req.ForceSync
		}
	}

	if err := s.db.Model(&o).Updates(updates).Error; err != nil {
		log.Errorf("org update: %v", err)
		return &orgpb.UpdateRes{Code: 1, Message: "保存失败，请稍后重试"}, nil
	}
	// 组织关闭日报授权后：无其它组织授权的用户强制关闭个人日报
	if req.EnableAiEmail != nil && !*req.EnableAiEmail {
		s.forceOffDailyEmailWithoutOrgGrant(orgID)
	}
	if req.EnableAiWeeklyEmail != nil && !*req.EnableAiWeeklyEmail {
		s.forceOffWeeklyEmailWithoutOrgGrant(orgID)
	}
	_ = s.db.First(&o, orgID)
	return &orgpb.UpdateRes{
		Code: 0, Message: "success", Data: s.toOrgInfo(&o, siteAdmin || canInfo || canToggle),
	}, nil
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

func (s *OrgService) Switch(ctx context.Context, req *orgpb.SwitchReq) (*orgpb.SwitchRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.SwitchRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		return &orgpb.SwitchRes{Code: 1, Message: "参数错误"}, nil
	}
	if !s.isMemberDB(pd.UserID, orgID) {
		return &orgpb.SwitchRes{Code: 1, Message: "你不是该组织成员"}, nil
	}
	var targetOrg model.Org
	if err := s.db.Select("id", "status").First(&targetOrg, orgID).Error; err != nil || targetOrg.Status != model.OrgStatusActive {
		return &orgpb.SwitchRes{Code: 1, Message: "该组织当前已暂停"}, nil
	}
	u, err := s.loadUser(pd.UserID)
	if err != nil {
		return &orgpb.SwitchRes{Code: 1, Message: "用户不存在"}, nil
	}
	_ = s.db.Model(u).Update("current_org_id", orgID).Error
	u.CurrentOrgID = orgID
	token, err := IssueJWT(s.db, u)
	if err != nil {
		return &orgpb.SwitchRes{Code: 1, Message: "签发 token 失败"}, nil
	}
	setSessionCookie(ctx, token)
	return &orgpb.SwitchRes{
		Code: 0, Message: "已切换组织", JwtToken: token, OrgId: req.OrgId,
	}, nil
}

func (s *OrgService) Join(ctx context.Context, req *orgpb.JoinReq) (*orgpb.JoinRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.JoinRes{Code: 1, Message: "请先登录"}, nil
	}
	code := strings.TrimSpace(strings.ToUpper(req.InviteCode))
	if code == "" {
		return &orgpb.JoinRes{Code: 1, Message: "请输入团队识别码"}, nil
	}
	displayName := strings.TrimSpace(req.OrgDisplayName)
	if displayName == "" {
		return &orgpb.JoinRes{Code: 1, Message: "请填写组织内名称（在本团队中展示的称呼）"}, nil
	}
	if len([]rune(displayName)) > 32 {
		return &orgpb.JoinRes{Code: 1, Message: "组织内名称过长（最多 32 字）"}, nil
	}
	var o model.Org
	if err := s.db.Where("UPPER(invite_code) = ? AND status = ?", code, model.OrgStatusActive).First(&o).Error; err != nil {
		return &orgpb.JoinRes{Code: 1, Message: "团队识别码无效"}, nil
	}
	if s.isMemberDB(pd.UserID, o.ID) {
		return &orgpb.JoinRes{Code: 0, Message: "你已在该组织中", Data: s.toOrgInfo(&o, false)}, nil
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
					return &orgpb.JoinRes{Code: 1, Message: "提交申请失败，请稍后重试"}, nil
				}
			}
			return &orgpb.JoinRes{Code: 0, Message: "申请已提交，等待团队管理员审批"}, nil
		}
		if err := s.db.Create(&model.OrgJoinRequest{
			OrgID:          o.ID,
			UserID:         pd.UserID,
			Status:         model.JoinReqPending,
			CodeUsed:       code,
			OrgDisplayName: displayName,
		}).Error; err != nil {
			return &orgpb.JoinRes{Code: 1, Message: "提交申请失败，请稍后重试"}, nil
		}
		return &orgpb.JoinRes{Code: 0, Message: "申请已提交，等待团队管理员审批"}, nil
	}
	if err := s.addOrgMemberAtomic(o.ID, pd.UserID, model.OrgRoleMember, displayName); err != nil {
		log.Errorf("org join ensure member: %v", err)
		return &orgpb.JoinRes{Code: 1, Message: err.Error()}, nil
	}
	return &orgpb.JoinRes{Code: 0, Message: "加入成功", Data: s.toOrgInfo(&o, false)}, nil
}

func (s *OrgService) Leave(ctx context.Context, req *orgpb.LeaveReq) (*orgpb.LeaveRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.LeaveRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		return &orgpb.LeaveRes{Code: 1, Message: "参数错误"}, nil
	}
	var o model.Org
	if err := s.db.First(&o, orgID).Error; err != nil {
		return &orgpb.LeaveRes{Code: 1, Message: "组织不存在"}, nil
	}
	if o.IsSystem || o.Slug == model.PublicOrgSlug {
		return &orgpb.LeaveRes{Code: 1, Message: "公共域不可退出"}, nil
	}
	var membership model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&membership).Error == nil && membership.Role == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", orgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			return &orgpb.LeaveRes{Code: 1, Message: "请先任命另一位组织管理员再退出"}, nil
		}
	}
	if err := s.db.Where("org_id = ? AND user_id = ?", orgID, pd.UserID).Delete(&model.OrgMember{}).Error; err != nil {
		log.Errorf("org leave: %v", err)
		return &orgpb.LeaveRes{Code: 1, Message: "退出失败，请稍后重试"}, nil
	}
	s.invalidateOrgMembersCache(orgID)
	s.invalidateDisplayCache(orgID, pd.UserID)
	// membership 已删 → 清除该组织全部角色指派（含自定义）
	syncOrgMemberSystemRole(s.db, orgID, pd.UserID)
	// 若当前组织是离开的组织，切回公共域
	u, _ := s.loadUser(pd.UserID)
	resp := &orgpb.LeaveRes{Code: 0, Message: "已退出组织"}
	if u != nil && u.CurrentOrgID == orgID {
		var pub model.Org
		if s.db.Where("slug = ?", model.PublicOrgSlug).First(&pub).Error == nil {
			_ = s.db.Model(u).Update("current_org_id", pub.ID).Error
			u.CurrentOrgID = pub.ID
			if token, _ := IssueJWT(s.db, u); token != "" {
				setSessionCookie(ctx, token)
				resp.JwtToken = token
			}
		}
	}
	return resp, nil
}

func (s *OrgService) Members(ctx context.Context, req *orgpb.MembersReq) (*orgpb.MembersRes, error) {
	avatarBase := avatarPublicBase(s.db)
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.MembersRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !auth.VerifySiteAdmin(ctx) && !s.isMemberDB(pd.UserID, orgID) {
		return &orgpb.MembersRes{Code: 1, Message: "权限不足"}, nil
	}
	page := int(req.Page)
	if page < 1 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	keyword := strings.TrimSpace(req.Keyword)

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
	if like := sqllike.Pattern(keyword); like != "" {
		base = base.Where("u.name ILIKE ? OR u.username ILIKE ? OR m.org_display_name ILIKE ?", like, like, like)
	}

	var total int64
	countQ := s.db.Table("org_members AS m").
		Joins("JOIN users u ON u.id = m.user_id").
		Where("m.org_id = ?", orgID)
	if like := sqllike.Pattern(keyword); like != "" {
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

	// 批量加载管理范围（组长/队长绑定的分组或分队），并解析可读名称
	uids := make([]uint, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.UserID)
	}
	scopeByUser := map[uint][]*orgpb.ScopeInfo{}
	if len(uids) > 0 {
		var grants []model.OrgScopeGrant
		_ = s.db.Where("org_id = ? AND user_id IN ?", orgID, uids).Find(&grants).Error
		groupIDs := map[uint]struct{}{}
		squadIDs := map[uint]struct{}{}
		for _, g := range grants {
			if g.ScopeType == model.ScopeTypeGroup {
				groupIDs[g.ScopeID] = struct{}{}
			} else if g.ScopeType == model.ScopeTypeSquad {
				squadIDs[g.ScopeID] = struct{}{}
			}
		}
		groupName := map[uint]string{}
		if len(groupIDs) > 0 {
			ids := make([]uint, 0, len(groupIDs))
			for id := range groupIDs {
				ids = append(ids, id)
			}
			var gs []model.Group
			_ = s.db.Where("id IN ?", ids).Find(&gs).Error
			for _, g := range gs {
				n := ""
				if g.Name != nil {
					n = *g.Name
				}
				if n == "" {
					n = "未命名分组"
				}
				groupName[g.ID] = n
			}
		}
		type squadRow struct {
			ID      uint
			Name    string
			GroupID uint
		}
		squadMeta := map[uint]squadRow{}
		if len(squadIDs) > 0 {
			ids := make([]uint, 0, len(squadIDs))
			for id := range squadIDs {
				ids = append(ids, id)
			}
			var sqs []model.Squad
			_ = s.db.Where("id IN ?", ids).Find(&sqs).Error
			needG := map[uint]struct{}{}
			for _, sq := range sqs {
				squadMeta[sq.ID] = squadRow{ID: sq.ID, Name: sq.Name, GroupID: sq.GroupID}
				if _, ok := groupName[sq.GroupID]; !ok {
					needG[sq.GroupID] = struct{}{}
				}
			}
			if len(needG) > 0 {
				ids := make([]uint, 0, len(needG))
				for id := range needG {
					ids = append(ids, id)
				}
				var gs []model.Group
				_ = s.db.Where("id IN ?", ids).Find(&gs).Error
				for _, g := range gs {
					n := ""
					if g.Name != nil {
						n = *g.Name
					}
					if n == "" {
						n = "未命名分组"
					}
					groupName[g.ID] = n
				}
			}
		}
		for _, g := range grants {
			item := &orgpb.ScopeInfo{
				ScopeType: g.ScopeType,
				ScopeId:   int64(g.ScopeID),
			}
			switch g.ScopeType {
			case model.ScopeTypeGroup:
				name := groupName[g.ScopeID]
				if name == "" {
					name = "未知分组"
				}
				item.ScopeName = name
				item.Label = "组长 · " + name
			case model.ScopeTypeSquad:
				sq := squadMeta[g.ScopeID]
				sname := sq.Name
				if sname == "" {
					sname = "未知分队"
				}
				gname := groupName[sq.GroupID]
				item.ScopeName = sname
				if gname != "" {
					item.GroupName = gname
					item.Label = "队长 · " + gname + " / " + sname
				} else {
					item.Label = "队长 · " + sname
				}
			}
			scopeByUser[g.UserID] = append(scopeByUser[g.UserID], item)
		}
	}

	list := make([]*orgpb.MemberInfo, 0, len(rows))
	for _, r := range rows {
		// 组织内展示仅用 org_display_name；空则回退 username（不再回退全局昵称）
		display := strings.TrimSpace(r.OrgDisplayName)
		if display == "" {
			display = r.Username
		}
		item := &orgpb.MemberInfo{
			UserId:         int64(r.UserID),
			Username:       r.Username,
			Name:           display,
			OrgDisplayName: r.OrgDisplayName,
			Avatar:         expandAvatarBase(avatarBase, r.Avatar),
			Role:           r.Role,
			JoinedAt:       r.JoinedAt.Unix(),
		}
		if r.GroupID != nil {
			item.GroupId = int64(*r.GroupID)
		}
		if sc := scopeByUser[r.UserID]; len(sc) > 0 {
			item.Scopes = sc
		}
		list = append(list, item)
	}
	return &orgpb.MembersRes{
		Code: 0, Message: "success", List: list, Total: int32(total), Page: int32(page), PageSize: int32(pageSize),
	}, nil
}

func (s *OrgService) MemberIds(ctx context.Context, req *orgpb.MemberIdsReq) (*orgpb.MemberIdsRes, error) {
	orgID := uint(req.OrgId)
	pd := auth.GetCurrentUser(ctx)
	if orgID == 0 && pd != nil {
		orgID = pd.OrgID
	}
	if orgID == 0 {
		return &orgpb.MemberIdsRes{Code: 1, Message: "缺少组织 id"}, nil
	}
	if pd == nil || (!auth.VerifySiteAdmin(ctx) && !s.isMemberDB(pd.UserID, orgID)) {
		return &orgpb.MemberIdsRes{Code: 1, Message: "权限不足"}, nil
	}
	groupID := req.GroupId
	squadID := req.SquadId
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
	return &orgpb.MemberIdsRes{
		Code: 0, Message: "success", UserIds: ids, OrgId: int64(orgID),
		GroupId: groupID, SquadId: squadID,
	}, nil
}

// AddMember 站点管理员搜索加入：按 userId 或 username
func (s *OrgService) AddMember(ctx context.Context, req *orgpb.AddMemberReq) (*orgpb.AddMemberRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.AddMemberRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		return &orgpb.AddMemberRes{Code: 1, Message: "参数错误"}, nil
	}
	// 站点管理员可操作任意 org；组织内需 org.member.add 权限
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgMemberAdd) {
		return &orgpb.AddMemberRes{Code: 1, Message: "权限不足"}, nil
	}
	uid := uint(req.UserId)
	if uid == 0 && strings.TrimSpace(req.Username) != "" {
		var u model.User
		if s.db.Where("username = ?", strings.TrimSpace(req.Username)).First(&u).Error != nil {
			// 尝试按昵称模糊
			if s.db.Where("name LIKE ?", "%"+strings.TrimSpace(req.Username)+"%").First(&u).Error != nil {
				return &orgpb.AddMemberRes{Code: 1, Message: "用户不存在"}, nil
			}
		}
		uid = u.ID
	}
	if uid == 0 {
		return &orgpb.AddMemberRes{Code: 1, Message: "请提供 userId 或 username"}, nil
	}
	role := req.Role
	if !model.ValidOrgRole(role) {
		role = model.OrgRoleMember
	}
	if s.isMemberDB(uid, orgID) {
		return &orgpb.AddMemberRes{Code: 0, Message: "用户已在组织中", UserId: int64(uid)}, nil
	}
	var addOrg model.Org
	if s.db.First(&addOrg, orgID).Error != nil {
		return &orgpb.AddMemberRes{Code: 1, Message: "组织不存在"}, nil
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
	if err := s.addOrgMemberAtomic(orgID, uid, role, displayName); err != nil {
		log.Errorf("org add member: %v", err)
		return &orgpb.AddMemberRes{Code: 1, Message: err.Error()}, nil
	}
	// 管理员拉入 → 设为默认组织（下次打开自动进入；用户之后 switch 即记忆）
	s.setDefaultOrg(uid, orgID)
	return &orgpb.AddMemberRes{Code: 0, Message: "已加入组织", UserId: int64(uid)}, nil
}

// SetDisplayName 本人或组织/站点管理员修改组织内名称
func (s *OrgService) SetDisplayName(ctx context.Context, req *orgpb.SetDisplayNameReq) (*orgpb.SetDisplayNameRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "参数错误"}, nil
	}
	uid := uint(req.UserId)
	if uid == 0 {
		uid = pd.UserID
	}
	displayName := strings.TrimSpace(req.OrgDisplayName)
	if displayName == "" {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "组织内名称不能为空"}, nil
	}
	if len([]rune(displayName)) > 32 {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "组织内名称过长（最多 32 字）"}, nil
	}
	if uid != pd.UserID && !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgMemberDisplayName) {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "权限不足"}, nil
	}
	if !s.isMemberDB(uid, orgID) {
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "用户不在该组织中"}, nil
	}
	if err := s.db.Model(&model.OrgMember{}).
		Where("org_id = ? AND user_id = ?", orgID, uid).
		Update("org_display_name", displayName).Error; err != nil {
		log.Errorf("org member display name: %v", err)
		return &orgpb.SetDisplayNameRes{Code: 1, Message: "保存失败，请稍后重试"}, nil
	}
	// 公共域称呼 ≡ 全局昵称 users.name
	var o model.Org
	if s.db.Select("id", "slug", "is_system").First(&o, orgID).Error == nil &&
		(o.IsSystem || o.Slug == model.PublicOrgSlug) {
		_ = s.db.Model(&model.User{}).Where("id = ?", uid).Update("name", displayName).Error
	}
	// 旁路更新 users.name 后清资料缓存，避免编辑页仍显示旧昵称/旧字段
	s.invalidateUserProfileCache(uid)
	s.invalidateDisplayCache(orgID, uid)
	return &orgpb.SetDisplayNameRes{Code: 0, Message: "已更新组织内名称"}, nil
}

func (s *OrgService) SetRole(ctx context.Context, req *orgpb.SetRoleReq) (*orgpb.SetRoleRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.SetRoleRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	userID := uint(req.UserId)
	if orgID == 0 || userID == 0 {
		return &orgpb.SetRoleRes{Code: 1, Message: "参数错误"}, nil
	}
	if !model.ValidOrgRole(req.Role) {
		return &orgpb.SetRoleRes{Code: 1, Message: "角色无效（member|captain|group_leader|coach|org_admin）"}, nil
	}

	isSite := auth.VerifySiteAdmin(ctx)
	if !isSite && !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgMemberRole) {
		return &orgpb.SetRoleRes{Code: 1, Message: "权限不足"}, nil
	}

	actorRole := model.OrgRoleMember
	if isSite {
		actorRole = model.OrgRoleOrgAdmin
	} else {
		var actor model.OrgMember
		if s.db.Where("org_id = ? AND user_id = ?", orgID, pd.UserID).First(&actor).Error == nil {
			actorRole = actor.Role
		}
	}

	targetCurrent := model.OrgRoleMember
	var m model.OrgMember
	inOrg := s.db.Where("org_id = ? AND user_id = ?", orgID, userID).First(&m).Error == nil
	if inOrg {
		targetCurrent = m.Role
	}

	if !isSite && !model.CanAppointOrgRole(actorRole, targetCurrent, req.Role) &&
		!(req.RemoveScope && model.CanAppointOrgRole(actorRole, model.OrgRoleMember, req.Role)) {
		// 卸任某一范围时，用 member 作目标档再验一次（允许卸下级领导职务）
		if !req.RemoveScope {
			return &orgpb.SetRoleRes{
				Code: 1, Message: "无权任命该角色或修改该成员（组织管理员可任命全部；其余只能任命低于自己的角色）",
			}, nil
		}
		if !model.CanAppointOrgRole(actorRole, targetCurrent, model.OrgRoleCaptain) &&
			!model.CanAppointOrgRole(actorRole, targetCurrent, model.OrgRoleGroupLeader) {
			return &orgpb.SetRoleRes{
				Code: 1, Message: "无权任命该角色或修改该成员（组织管理员可任命全部；其余只能任命低于自己的角色）",
			}, nil
		}
	}

	if req.Role != model.OrgRoleOrgAdmin && targetCurrent == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", orgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			return &orgpb.SetRoleRes{Code: 1, Message: "不能降权最后一位组织管理员"}, nil
		}
	}

	// —— 卸任某一管理范围（多组长/多队长）——
	if req.RemoveScope {
		st := strings.TrimSpace(req.ScopeType)
		if !model.ValidScopeType(st) || req.ScopeId == 0 {
			return &orgpb.SetRoleRes{Code: 1, Message: "请指定要卸任的分组或分队"}, nil
		}
		if !inOrg {
			return &orgpb.SetRoleRes{Code: 1, Message: "对方不在本组织"}, nil
		}
		if st == model.ScopeTypeSquad {
			var sq model.Squad
			if s.db.Where("id = ? AND org_id = ?", req.ScopeId, orgID).First(&sq).Error == nil &&
				!isSite && actorRole == model.OrgRoleGroupLeader &&
				!s.actorControlsGroup(orgID, pd.UserID, sq.GroupID) {
				return &orgpb.SetRoleRes{Code: 1, Message: "只能管理自己分组内的队长"}, nil
			}
		}
		_ = s.db.Where("org_id = ? AND user_id = ? AND scope_type = ? AND scope_id = ?",
			orgID, userID, st, req.ScopeId).Delete(&model.OrgScopeGrant{}).Error
		finalRole := s.syncRoleFromGrants(orgID, userID, targetCurrent)
		return &orgpb.SetRoleRes{
			Code: 0, Message: "已卸任该范围", Role: finalRole,
			Scopes: s.listScopeRefs(orgID, userID),
		}, nil
	}

	needType, needsScope := model.RoleNeedsScope(req.Role)
	if needsScope {
		st := strings.TrimSpace(req.ScopeType)
		if st == "" {
			st = needType
		}
		if st != needType || req.ScopeId == 0 {
			msg := "任命队长须指定分队"
			if req.Role == model.OrgRoleGroupLeader {
				msg = "任命组长须指定分组"
			}
			return &orgpb.SetRoleRes{Code: 1, Message: msg}, nil
		}
		if st == model.ScopeTypeGroup {
			var n int64
			_ = s.db.Model(&model.Group{}).Where("id = ? AND org_id = ?", req.ScopeId, orgID).Count(&n).Error
			if n == 0 {
				return &orgpb.SetRoleRes{Code: 1, Message: "分组不存在或不属于本组织"}, nil
			}
		} else {
			var sq model.Squad
			if s.db.Where("id = ? AND org_id = ?", req.ScopeId, orgID).First(&sq).Error != nil {
				return &orgpb.SetRoleRes{Code: 1, Message: "分队不存在或不属于本组织"}, nil
			}
			if !isSite && actorRole == model.OrgRoleGroupLeader {
				if !s.actorControlsGroup(orgID, pd.UserID, sq.GroupID) {
					return &orgpb.SetRoleRes{Code: 1, Message: "只能任命自己管理分组内的队长"}, nil
				}
			}
		}
		if !isSite && actorRole == model.OrgRoleGroupLeader && req.Role == model.OrgRoleGroupLeader {
			return &orgpb.SetRoleRes{Code: 1, Message: "组长不能任命其他组长"}, nil
		}

		if !inOrg {
			displayName := ""
			var u model.User
			if s.db.Select("name", "username").First(&u, userID).Error == nil {
				displayName = strings.TrimSpace(u.Name)
				if displayName == "" {
					displayName = u.Username
				}
			}
			// 先以 member 入组，再叠加领导职务
			if err := s.addOrgMemberAtomic(orgID, userID, model.OrgRoleMember, displayName); err != nil {
				log.Errorf("org set role ensure member: %v", err)
				return &orgpb.SetRoleRes{Code: 1, Message: err.Error()}, nil
			}
			s.setDefaultOrg(userID, orgID)
			inOrg = true
		}

		// 叠加写入（一人可多组组长 / 多队队长 / 同时组长+队长）
		if err := s.addUserScopeGrant(orgID, userID, st, uint(req.ScopeId)); err != nil {
			log.Errorf("org add scope: %v", err)
			return &orgpb.SetRoleRes{Code: 1, Message: "更新管理范围失败"}, nil
		}

		// 任命队长时加入该分队（不踢出其他分队，支持多队）
		if req.Role == model.OrgRoleCaptain {
			var sq model.Squad
			if s.db.First(&sq, req.ScopeId).Error == nil {
				sm := model.SquadMember{SquadID: sq.ID, UserID: userID}
				_ = s.db.Where("squad_id = ? AND user_id = ?", sq.ID, userID).FirstOrCreate(&sm).Error
			}
		}

		base := targetCurrent
		if base == model.OrgRoleMember || base == model.OrgRoleCaptain || base == model.OrgRoleGroupLeader {
			base = req.Role
		}
		finalRole := s.syncRoleFromGrants(orgID, userID, base)
		return &orgpb.SetRoleRes{
			Code: 0, Message: "已更新角色",
			Role: finalRole, Scopes: s.listScopeRefs(orgID, userID),
		}, nil
	}

	// 教练 / 组织管理员 / 成员：清空领导范围
	if !inOrg {
		displayName := ""
		var u model.User
		if s.db.Select("name", "username").First(&u, userID).Error == nil {
			displayName = strings.TrimSpace(u.Name)
			if displayName == "" {
				displayName = u.Username
			}
		}
		if err := s.addOrgMemberAtomic(orgID, userID, req.Role, displayName); err != nil {
			log.Errorf("org set role ensure member: %v", err)
			return &orgpb.SetRoleRes{Code: 1, Message: err.Error()}, nil
		}
		s.setDefaultOrg(userID, orgID)
	} else {
		if err := s.db.Model(&m).Update("role", req.Role).Error; err != nil {
			return &orgpb.SetRoleRes{Code: 1, Message: "更新角色失败"}, nil
		}
		syncOrgMemberSystemRole(s.db, orgID, userID)
	}
	_ = s.replaceUserScopeGrants(orgID, userID, nil)

	return &orgpb.SetRoleRes{
		Code: 0, Message: "已更新角色",
		Role: req.Role, Scopes: []*orgpb.ScopeRef{},
	}, nil
}

// addUserScopeGrant 追加一条管理范围（已存在则忽略）
func (s *OrgService) addUserScopeGrant(orgID, userID uint, scopeType string, scopeID uint) error {
	var n int64
	_ = s.db.Model(&model.OrgScopeGrant{}).
		Where("org_id = ? AND user_id = ? AND scope_type = ? AND scope_id = ?",
			orgID, userID, scopeType, scopeID).Count(&n).Error
	if n > 0 {
		return nil
	}
	return s.db.Create(&model.OrgScopeGrant{
		OrgID: orgID, UserID: userID, ScopeType: scopeType, ScopeID: scopeID,
	}).Error
}

// syncRoleFromGrants 按现有 grant 重算 org_members.role（保留 coach/org_admin）
func (s *OrgService) syncRoleFromGrants(orgID, userID uint, currentHint string) string {
	var cur string
	_ = s.db.Model(&model.OrgMember{}).Select("role").
		Where("org_id = ? AND user_id = ?", orgID, userID).Scan(&cur).Error
	if cur == "" {
		cur = currentHint
	}
	var grants []model.OrgScopeGrant
	_ = s.db.Where("org_id = ? AND user_id = ?", orgID, userID).Find(&grants).Error
	hasGroup, hasSquad := false, false
	for _, g := range grants {
		if g.ScopeType == model.ScopeTypeGroup {
			hasGroup = true
		}
		if g.ScopeType == model.ScopeTypeSquad {
			hasSquad = true
		}
	}
	final := model.EffectiveRoleFromGrants(cur, hasGroup, hasSquad)
	if final != cur {
		_ = s.db.Model(&model.OrgMember{}).
			Where("org_id = ? AND user_id = ?", orgID, userID).
			Update("role", final).Error
		syncOrgMemberSystemRole(s.db, orgID, userID)
	} else {
		syncOrgMemberSystemRole(s.db, orgID, userID)
	}
	return final
}

func (s *OrgService) listScopeRefs(orgID, userID uint) []*orgpb.ScopeRef {
	var grants []model.OrgScopeGrant
	_ = s.db.Where("org_id = ? AND user_id = ?", orgID, userID).Find(&grants).Error
	out := make([]*orgpb.ScopeRef, 0, len(grants))
	for _, g := range grants {
		out = append(out, &orgpb.ScopeRef{
			ScopeType: g.ScopeType, ScopeId: int64(g.ScopeID),
		})
	}
	return out
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

func (s *OrgService) RemoveMember(ctx context.Context, req *orgpb.RemoveMemberReq) (*orgpb.RemoveMemberRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.RemoveMemberRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	userID := uint(req.UserId)
	if orgID == 0 || userID == 0 {
		return &orgpb.RemoveMemberRes{Code: 1, Message: "参数错误"}, nil
	}
	var o model.Org
	if s.db.First(&o, orgID).Error != nil {
		return &orgpb.RemoveMemberRes{Code: 1, Message: "组织不存在"}, nil
	}
	if o.IsSystem {
		return &orgpb.RemoveMemberRes{Code: 1, Message: "不能将成员移出公共域"}, nil
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgMemberRemove) {
		return &orgpb.RemoveMemberRes{Code: 1, Message: "权限不足"}, nil
	}
	var target model.OrgMember
	if s.db.Where("org_id = ? AND user_id = ?", orgID, userID).First(&target).Error == nil && target.Role == model.OrgRoleOrgAdmin {
		var admins int64
		s.db.Model(&model.OrgMember{}).Where("org_id = ? AND role = ?", orgID, model.OrgRoleOrgAdmin).Count(&admins)
		if admins <= 1 {
			return &orgpb.RemoveMemberRes{Code: 1, Message: "不能移除最后一位组织管理员"}, nil
		}
	}
	if err := s.db.Where("org_id = ? AND user_id = ?", orgID, userID).Delete(&model.OrgMember{}).Error; err != nil {
		log.Errorf("org remove member: %v", err)
		return &orgpb.RemoveMemberRes{Code: 1, Message: "移除失败，请稍后重试"}, nil
	}
	s.invalidateOrgMembersCache(orgID)
	s.invalidateDisplayCache(orgID, userID)
	// membership 已删 → 清除该组织全部角色指派（含自定义）
	syncOrgMemberSystemRole(s.db, orgID, userID)
	// 若被移出的是其默认组织，回落公共域
	s.fallbackDefaultOrgIf(userID, orgID)
	return &orgpb.RemoveMemberRes{Code: 0, Message: "已移除成员"}, nil
}

func (s *OrgService) Invite(ctx context.Context, req *orgpb.InviteReq) (*orgpb.InviteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.InviteRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgInviteView) {
		return &orgpb.InviteRes{Code: 1, Message: "权限不足"}, nil
	}
	var o model.Org
	if s.db.First(&o, orgID).Error != nil {
		return &orgpb.InviteRes{Code: 1, Message: "组织不存在"}, nil
	}
	return &orgpb.InviteRes{
		Code: 0, Message: "success",
		InviteCode: o.InviteCode, JoinMode: o.JoinMode, OrgId: int64(o.ID),
	}, nil
}

func (s *OrgService) InviteRotate(ctx context.Context, req *orgpb.InviteRotateReq) (*orgpb.InviteRotateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.InviteRotateRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgInviteRotate) {
		return &orgpb.InviteRotateRes{Code: 1, Message: "权限不足"}, nil
	}
	code := newInviteCode()
	if err := s.db.Model(&model.Org{}).Where("id = ?", orgID).Update("invite_code", code).Error; err != nil {
		log.Errorf("org rotate invite: %v", err)
		return &orgpb.InviteRotateRes{Code: 1, Message: "更新失败，请稍后重试"}, nil
	}
	return &orgpb.InviteRotateRes{Code: 0, Message: "已更换团队识别码", InviteCode: code}, nil
}

func (s *OrgService) JoinRequests(ctx context.Context, req *orgpb.JoinRequestsReq) (*orgpb.JoinRequestsRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.JoinRequestsRes{Code: 1, Message: "请先登录"}, nil
	}
	orgID := uint(req.OrgId)
	if orgID == 0 {
		orgID = pd.OrgID
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, orgID, rbac.PermOrgJoinReview) {
		return &orgpb.JoinRequestsRes{Code: 1, Message: "权限不足"}, nil
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
	list := make([]*orgpb.JoinRequestInfo, 0, len(reqs))
	for _, r := range reqs {
		u := userByID[r.UserID]
		display := strings.TrimSpace(r.OrgDisplayName)
		if display == "" {
			display = u.Username
		}
		list = append(list, &orgpb.JoinRequestInfo{
			Id: int64(r.ID), UserId: int64(r.UserID), Username: u.Username,
			Name: display,
			OrgDisplayName: r.OrgDisplayName,
			Status:         r.Status, CreatedAt: r.CreatedAt.Unix(),
		})
	}
	return &orgpb.JoinRequestsRes{Code: 0, Message: "success", List: list}, nil
}

func (s *OrgService) JoinReview(ctx context.Context, req *orgpb.JoinReviewReq) (*orgpb.JoinReviewRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return &orgpb.JoinReviewRes{Code: 1, Message: "请先登录"}, nil
	}
	reqID := uint(req.Id)
	if reqID == 0 {
		return &orgpb.JoinReviewRes{Code: 1, Message: "参数错误"}, nil
	}
	var jr model.OrgJoinRequest
	if s.db.First(&jr, reqID).Error != nil {
		return &orgpb.JoinReviewRes{Code: 1, Message: "申请不存在"}, nil
	}
	if !verifyOrgPerm(ctx, s.db, pd.UserID, jr.OrgID, rbac.PermOrgJoinReview) {
		return &orgpb.JoinReviewRes{Code: 1, Message: "权限不足"}, nil
	}
	uid := pd.UserID
	if req.Approve {
		if !s.isMemberDB(jr.UserID, jr.OrgID) {
			var reviewOrg model.Org
			if s.db.First(&reviewOrg, jr.OrgID).Error != nil {
				return &orgpb.JoinReviewRes{Code: 1, Message: "组织不存在"}, nil
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
				return &orgpb.JoinReviewRes{Code: 1, Message: err.Error()}, nil
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
		return &orgpb.JoinReviewRes{Code: 0, Message: "已通过"}, nil
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
	return &orgpb.JoinReviewRes{Code: 0, Message: "已拒绝"}, nil
}

func (s *OrgService) orgName(orgID uint) string {
	var o model.Org
	if s.db.Select("name").First(&o, orgID).Error == nil && strings.TrimSpace(o.Name) != "" {
		return o.Name
	}
	return "组织"
}

func (s *OrgService) SetSiteAdmin(ctx context.Context, req *orgpb.SetSiteAdminReq) (*orgpb.SetSiteAdminRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteAppointAdmin) {
		return &orgpb.SetSiteAdminRes{Code: 1, Message: "无任命站点管理员权限"}, nil
	}
	userID := uint(req.UserId)
	if userID == 0 {
		return &orgpb.SetSiteAdminRes{Code: 1, Message: "参数错误"}, nil
	}
	// 防止撤销最后一个站点管理员
	if !req.IsSiteAdmin {
		var n int64
		s.db.Model(&model.User{}).Where("is_site_admin = ?", true).Count(&n)
		var target model.User
		if s.db.First(&target, userID).Error == nil && target.IsSiteAdmin && n <= 1 {
			return &orgpb.SetSiteAdminRes{Code: 1, Message: "不能撤销最后一位站点管理员"}, nil
		}
	}
	roleID := 0
	if req.IsSiteAdmin {
		roleID = 1
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]interface{}{
		"is_site_admin": req.IsSiteAdmin,
		"role_id":       roleID,
	}).Error; err != nil {
		log.Errorf("set site admin: %v", err)
		return &orgpb.SetSiteAdminRes{Code: 1, Message: "更新失败，请稍后重试"}, nil
	}
	syncSiteSystemRole(s.db, userID, rbac.RoleSiteAdmin, req.IsSiteAdmin)
	log.Infof("set site admin user=%d is=%v", userID, req.IsSiteAdmin)
	return &orgpb.SetSiteAdminRes{Code: 0, Message: "已更新"}, nil
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
