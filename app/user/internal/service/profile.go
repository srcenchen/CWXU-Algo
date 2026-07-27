package service

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/api/core/v1/spider"
	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/permission"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/biz"
	"cwxu-algo/app/user/internal/biz/dormancy"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	grpc2 "google.golang.org/grpc"
	"gorm.io/gorm"
)

// queryBoolTrue 从 HTTP query 解析布尔（1/true/yes）；非 HTTP 上下文返回 false。
func queryBoolTrue(ctx context.Context, keys ...string) bool {
	if ctx == nil {
		return false
	}
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return false
	}
	ht, ok := tr.(*khttp.Transport)
	if !ok || ht.Request() == nil {
		return false
	}
	q := ht.Request().URL.Query()
	for _, k := range keys {
		v := strings.ToLower(strings.TrimSpace(q.Get(k)))
		if v == "1" || v == "true" || v == "yes" {
			return true
		}
	}
	return false
}

// queryInt32 从 HTTP query 解析正整数；失败返回 0。
func queryInt32(ctx context.Context, keys ...string) int32 {
	if ctx == nil {
		return 0
	}
	tr, ok := transport.FromServerContext(ctx)
	if !ok {
		return 0
	}
	ht, ok := tr.(*khttp.Transport)
	if !ok || ht.Request() == nil {
		return 0
	}
	q := ht.Request().URL.Query()
	for _, k := range keys {
		raw := strings.TrimSpace(q.Get(k))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 {
			continue
		}
		return int32(n)
	}
	return 0
}

// 与 auth 包校验规则一致
var profileEmailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

var (
	UpdateForbidden = errors.Forbidden("禁止访问", "您无权更新该用户资料")
	InternalServer  = errors.InternalServer("内部错误", "内部错误")
)

type ProfileService struct {
	profile.UnimplementedProfileServer
	reg            *discovery.Register
	profileDal     *dal.ProfileDal
	profileUseCase *biz.ProfileUseCase
	socialDal      *dal.SocialDal
	db             *gorm.DB
}

func (p *ProfileService) GetByName(ctx context.Context, req *profile.GetByNameReq) (*profile.GetByNameRes, error) {
	userList, err := p.profileDal.GetByName(ctx, req.Name)
	if err != nil {
		return nil, errors.InternalServer("内部错误", "查询时出错")
	}
	res := &profile.GetByNameRes{List: make([]*profile.GetByNameRes_UserList, 0)}
	for _, v := range userList {
		t := &profile.GetByNameRes_UserList{
			UserId:   int64(v.ID),
			Name:     v.Name,
			Username: v.Username,
		}
		res.List = append(res.List, t)
	}
	return res, nil
}

func (p *ProfileService) MoveGroup(ctx context.Context, req *profile.MoveGroupReq) (*profile.MoveGroupRes, error) {
	// 细粒度权限：分组管理
	if !auth.HasPerm(ctx, rbac.PermOrgGroupManage) {
		return nil, errors.Forbidden("权限不足", "需要分组管理权限")
	}
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.OrgID == 0 {
		return nil, errors.Forbidden("权限不足", "请先选择目标组织")
	}
	if !p.profileDal.IsMemberOfOrg(ctx, int64(req.UserId), pd.OrgID) {
		return nil, errors.Forbidden("权限不足", "该用户不属于当前组织")
	}
	gid := req.GroupId
	// groupId=0 表示归入当前组织默认分组（不再使用「未分组」）
	if gid <= 0 {
		orgID := pd.OrgID
		if orgID > 0 {
			// 通过 groupDal 路径不便，直接写默认：由 seed/list 保证存在
			// 使用 profileDal 的 db 查默认分组
			if def, e := p.profileDal.ResolveDefaultGroupID(ctx, orgID); e == nil && def > 0 {
				gid = int64(def)
			}
		}
	} else if !p.profileDal.GroupBelongsToOrg(ctx, gid, pd.OrgID) {
		return nil, errors.Forbidden("权限不足", "目标分组不属于当前组织")
	}
	err := p.profileDal.MoveGroup(ctx, req.UserId, gid, pd.OrgID)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.MoveGroupRes{
		Code:    0,
		Message: "移动成功",
	}, nil
}

// coreDataRPC 复用全进程共享的 core-data gRPC 长连接；调用方不得 Close。
func (p *ProfileService) coreDataRPC() (*grpc2.ClientConn, error) {
	return sharedCoreDataConn(p.reg)
}

func (p *ProfileService) GetList(ctx context.Context, req *profile.GetListReq) (*profile.GetListRes, error) {
	pageSize, pageNum := req.PageSize, req.PageNum
	if pageSize < 1 {
		pageSize = 20
	}
	if pageNum < 1 {
		pageNum = 1
	}
	var pf []model.User
	var total int64
	var err error
	// scope=org：当前组织；scope=site：全站（仅站管）；空：兼容（站管全站/否则组织）
	scope := req.Scope
	useSite := false
	if scope == "site" {
		if !auth.HasPerm(ctx, rbac.PermSiteUserList) {
			return nil, errors.Forbidden("权限不足", "需要全站用户列表权限")
		}
		useSite = true
	} else if scope == "org" {
		useSite = false
	} else {
		// 兼容旧客户端
		useSite = auth.HasPerm(ctx, rbac.PermSiteUserList)
	}
	keyword := strings.TrimSpace(req.GetKeyword())
	// BindQuery 对手写 proto 字段/bool 偶发丢参；再从 URL 兜底解析
	dormantOnly := req.GetDormantOnly() || queryBoolTrue(ctx, "dormantOnly", "dormant")
	inactiveDays := int(req.GetInactiveDays())
	if inactiveDays <= 0 {
		inactiveDays = int(queryInt32(ctx, "inactiveDays", "inactive_days"))
	}
	// inactiveDays 优先于 dormantOnly（自定义「最近 N 天未登录」）
	if inactiveDays > 0 {
		dormantOnly = false
		inactiveDays = dormancy.ClampInactiveDays(inactiveDays)
	}
	if useSite {
		pf, total, err = p.profileUseCase.GetList(ctx, pageSize, pageNum, keyword, dormantOnly, inactiveDays)
	} else {
		orgID := uint(0)
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			orgID = pd.OrgID
		}
		if orgID == 0 {
			orgID, _ = p.profileDal.PublicOrgID(ctx)
		}
		pf, total, err = p.profileDal.GetListByOrg(ctx, orgID, pageSize, pageNum, keyword, dormantOnly, inactiveDays)
	}
	if err != nil {
		return nil, InternalServer
	}
	ids := make([]int64, 0)
	for _, v := range pf {
		ids = append(ids, int64(v.ID))
	}
	// 获取 最后一次 提交时间；core-data 不可用时降级为空提交时间，不阻断整页列表
	timeMap := map[int64]int64{}
	if conn, err := p.coreDataRPC(); err != nil {
		log.Warnf("GetList core-data 连接不可用，最后提交时间降级为空: %v", err)
	} else {
		sb := submit_log.NewSubmitClient(conn)
		if sp, err := sb.LastSubmitTime(ctx, &submit_log.LastSubmitTimeReq{UserIds: ids}); err != nil {
			log.Warnf("GetList 查询最后提交时间失败，降级为空: %v", err)
		} else if err := utils.GobDecoder(sp.TimeMap, &timeMap); err != nil {
			log.Warnf("GetList 解析最后提交时间失败，降级为空: %v", err)
			timeMap = map[int64]int64{}
		}
	}

	uids := make([]uint, 0, len(pf))
	gids := make([]int64, 0, len(pf))
	for _, v := range pf {
		uids = append(uids, v.ID)
		if v.GroupId > 0 {
			gids = append(gids, v.GroupId)
		}
	}
	orgMap, _ := p.profileDal.GetOrgBriefsByUserIDs(ctx, uids)
	groupNames, _ := p.profileDal.GetGroupNamesByIDs(ctx, gids)

	// 列表名称：site=公共域外显名称（≡站内昵称）；org=当前组织内称呼
	displayByUID := map[uint]string{}
	orgIDForDisplay := uint(0)
	if useSite {
		orgIDForDisplay, _ = p.profileDal.PublicOrgID(ctx)
	} else if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgIDForDisplay = pd.OrgID
	}
	if orgIDForDisplay == 0 {
		orgIDForDisplay, _ = p.profileDal.PublicOrgID(ctx)
	}
	if orgIDForDisplay > 0 {
		if m, e := p.profileDal.OrgDisplayNamesByUserIDs(ctx, orgIDForDisplay, uids); e == nil {
			displayByUID = m
		}
	}

	dailyGrant, weeklyGrant := p.profileDal.BatchEmailGrants(ctx, ids)

	// 非公共域组织用户集合（题面流水线默认资格）
	nonPublicSet := map[int64]struct{}{}
	if npIDs, e := p.profileDal.GetNonPublicOrgUserIds(ctx); e == nil {
		for _, id := range npIDs {
			nonPublicSet[id] = struct{}{}
		}
	}

	// 有效定时间隔（含站管个人覆盖）
	policyByUID := map[int64]dal.UserSyncPolicy{}
	if policies, e := p.profileDal.GetSyncPolicies(ctx, ids); e == nil {
		for _, pol := range policies {
			policyByUID[pol.UserID] = pol
		}
	}

	// 自定义站点角色名（管理端 badge）
	siteRolesByUID := siteRoleNamesByUser(p.db, ids)

	res := &profile.GetListRes{
		List:  make([]*profile.GetListRes_List, 0),
		Total: total,
	}
	for _, v := range pf {
		var t string
		if ts, ok := timeMap[int64(v.ID)]; ok {
			t = strconv.Itoa(int(ts))
		}
		gName := ""
		if v.GroupId > 0 {
			if n, ok := groupNames[v.GroupId]; ok {
				gName = n
			} else {
				// 分组已删：显示默认分组，避免「分组11」
				gName = "默认分组"
			}
		} else {
			gName = "默认分组"
		}
		// site：公共域 org_display_name；org：当前组织称呼；空则回落 users.name / username
		displayName := v.Name
		if d := displayByUID[v.ID]; d != "" {
			displayName = d
		} else if !useSite && v.Username != "" {
			// 组织视图：无组织内称呼时用 username（不展示其他组织的昵称）
			displayName = v.Username
		} else if strings.TrimSpace(displayName) == "" && v.Username != "" {
			displayName = v.Username
		}
		uid := int64(v.ID)
		_, isNonPublic := nonPublicSet[uid]
		pol := policyByUID[uid]
		spMin := int32(pol.SpiderIntervalMin)
		if spMin <= 0 {
			spMin = 60
		}
		aiMin := int32(pol.AISummaryIntervalMin)
		if aiMin <= 0 {
			aiMin = 180
		}
		item := &profile.GetListRes_List{
			UserId:                      uint64(v.ID),
			Username:                    v.Username,
			Name:                        displayName,
			Avatar:                      v.Avatar,
			GroupId:                     v.GroupId,
			RoleId:                      int32(v.RoleID),
			LastSubmit:                  t,
			IsSiteAdmin:                 v.IsSiteAdmin,
			SiteRoles:                   siteRolesByUID[uid],
			GroupName:                   gName,
			EmailEnabled:                v.EmailEnabled,
			EmailWeeklyEnabled:          v.EmailWeeklyEnabled,
			EmailAllowedByOrg:           dailyGrant[uid],
			EmailWeeklyAllowedByOrg:     weeklyGrant[uid],
			ProblemFetchEnabled:         dal.EffectiveProblemPipeline(v.ProblemFetchEnabled, isNonPublic),
			ProblemAiEnabled:            dal.EffectiveProblemPipeline(v.ProblemAIEnabled, isNonPublic),
			SpiderIntervalMin:           spMin,
			AiSummaryIntervalMin:        aiMin,
			SpiderIntervalOverridden:    v.SpiderIntervalMinOverride != nil && *v.SpiderIntervalMinOverride > 0,
			AiSummaryIntervalOverridden: v.AISummaryIntervalMinOverride != nil && *v.AISummaryIntervalMinOverride > 0,
			SyncExempt:                  v.SyncExempt,
			AdminForceDormant:           v.AdminForceDormant,
			Disabled:                    v.Disabled,
			// 已暂停同步：优先策略；策略缺失时回落单用户判定
			// last_login 为空且无豁免 → 不活跃休眠；站管强制冻结/禁用覆盖豁免
			Dormant: func() bool {
				if v.Disabled || v.AdminForceDormant {
					return true
				}
				if pol.UserID != 0 {
					return !pol.SyncActive
				}
				if p.profileDal != nil {
					return p.profileDal.IsUserDormant(ctx, &v)
				}
				return dormancy.IsInactiveByTime(v.LastLoginAt, dormancy.DefaultInactiveDays, time.Now())
			}(),
		}
		if v.LastLoginAt != nil && !v.LastLoginAt.IsZero() {
			item.LastLoginAt = v.LastLoginAt.Unix()
		}
		if !v.CreatedAt.IsZero() {
			item.CreatedAt = v.CreatedAt.Unix()
		}
		if briefs := orgMap[v.ID]; len(briefs) > 0 {
			item.Orgs = make([]*profile.GetListRes_OrgBrief, 0, len(briefs))
			for _, b := range briefs {
				item.Orgs = append(item.Orgs, &profile.GetListRes_OrgBrief{
					OrgId: uint64(b.OrgID),
					Name:  b.Name,
					Role:  b.Role,
				})
			}
		}
		res.List = append(res.List, item)
	}
	return res, nil
}

func (p *ProfileService) Update(ctx context.Context, req *profile.UpdateReq) (*profile.UpdateRes, error) {
	// 校验 JWT：只能修改自己，或者管理员可以修改任何人
	if !auth.VerifySelfOrAbove(ctx, uint(req.UserId)) {
		return nil, UpdateForbidden
	}
	cur, err := p.profileDal.GetById(ctx, int64(req.UserId))
	if err != nil || cur == nil {
		return &profile.UpdateRes{Code: 1, Message: "用户不存在"}, nil
	}

	newEmail := strings.ToLower(strings.TrimSpace(req.GetEmail()))
	oldEmail := strings.ToLower(strings.TrimSpace(cur.Email))
	emailChanged := newEmail != oldEmail

	if emailChanged {
		if newEmail == "" {
			return &profile.UpdateRes{Code: 2, Message: "邮箱不能为空"}, nil
		}
		if !profileEmailRe.MatchString(newEmail) {
			return &profile.UpdateRes{Code: 3, Message: "请输入有效邮箱"}, nil
		}
		// 新邮箱验证码（purpose=change_email）
		code := strings.TrimSpace(req.GetEmailCode())
		if code == "" {
			return &profile.UpdateRes{Code: 4, Message: "请填写发往新邮箱的验证码"}, nil
		}
		if !VerifyEmailCode(ctx, p.profileDal.RDB(), purposeChangeEmail, newEmail, code) {
			return &profile.UpdateRes{Code: 5, Message: "验证码错误或已过期"}, nil
		}
		// 唯一性（排除自己）
		taken, tErr := p.profileDal.EmailTakenByOther(ctx, newEmail, cur.ID)
		if tErr != nil {
			return nil, errors.InternalServer("内部错误", tErr.Error())
		}
		if taken {
			return &profile.UpdateRes{Code: 6, Message: "该邮箱已被其他账号使用"}, nil
		}
	}

	// 昵称不再由此接口修改（请走组织内称呼）；仅更新头像 / 邮箱
	pro := model.User{
		ID:     uint(req.UserId),
		Avatar: req.Avatar,
		Email:  newEmail,
	}
	if !emailChanged {
		pro.Email = cur.Email
	}
	if err := p.profileDal.UpdateAvatarEmail(ctx, pro, emailChanged); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.UpdateRes{Code: 0, Message: "更新成功"}, nil
}

func (p *ProfileService) GetById(ctx context.Context, req *profile.GetByIdReq) (*profile.GetByIdRes, error) {
	pf, err := p.profileDal.GetById(ctx, req.UserId)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	if err := p.enforceProfileVisibility(ctx, pf.ID); err != nil {
		return nil, err
	}
	return p.buildGetByIdRes(ctx, pf)
}

func (p *ProfileService) GetByUsername(ctx context.Context, req *profile.GetByUsernameReq) (*profile.GetByIdRes, error) {
	if p.socialDal == nil {
		return nil, errors.InternalServer("内部错误", "社交模块未就绪")
	}
	pf, err := p.socialDal.GetByUsername(ctx, req.Username)
	if err != nil {
		return nil, errors.NotFound("未找到", "用户不存在")
	}
	if err := p.enforceProfileVisibility(ctx, pf.ID); err != nil {
		return nil, err
	}
	return p.buildGetByIdRes(ctx, pf)
}

func (p *ProfileService) GetFollowingIds(ctx context.Context, req *profile.GetFollowingIdsReq) (*profile.GetFollowingIdsRes, error) {
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &profile.GetFollowingIdsRes{UserIds: []int64{}}, nil
	}
	if p.socialDal == nil {
		return &profile.GetFollowingIdsRes{UserIds: []int64{}}, nil
	}
	ids, err := dal.FollowingIDsCached(ctx, p.profileDal.RDB(), uid, func() ([]int64, error) {
		return p.socialDal.FollowingIDs(ctx, uid)
	})
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.GetFollowingIdsRes{UserIds: ids}, nil
}

func (p *ProfileService) FilterPublicFeedUserIds(ctx context.Context, req *profile.FilterPublicFeedUserIdsReq) (*profile.FilterPublicFeedUserIdsRes, error) {
	if p.socialDal == nil || len(req.UserIds) == 0 {
		return &profile.FilterPublicFeedUserIdsRes{UserIds: req.UserIds}, nil
	}
	ids, err := p.socialDal.FilterPublicFeedUserIDs(ctx, req.UserIds)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.FilterPublicFeedUserIdsRes{UserIds: ids}, nil
}

// enforceProfileVisibility 公共域下尊重隐私；
// 私人域仅当「查看者与目标同属当前私人组织」时配置失效（不可因身处私人域就看全站隐藏资料）。
func (p *ProfileService) enforceProfileVisibility(ctx context.Context, targetUID uint) error {
	if auth.VerifySelfOrAbove(ctx, targetUID) {
		return nil
	}
	if p.socialDal == nil {
		return nil
	}
	orgID := uint(0)
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = pd.OrgID
	}
	// 私人域 + 目标同组织：隐私配置失效
	if orgID > 0 && !p.socialDal.IsPublicOrg(ctx, orgID) {
		if p.profileDal.IsMemberOfOrg(ctx, int64(targetUID), orgID) {
			return nil
		}
		// 非同组织成员：仍按公共域隐私处理
	}
	ok, err := p.socialDal.CanViewPublicProfile(ctx, targetUID)
	if err != nil {
		return errors.InternalServer("内部错误", err.Error())
	}
	if !ok {
		return errors.Forbidden("隐私限制", "该用户未开放公共域个人资料")
	}
	return nil
}

func (p *ProfileService) buildGetByIdRes(ctx context.Context, pf *model.User) (*profile.GetByIdRes, error) {
	// 获取 platform spider 信息（失败不阻断资料：旧用户/core 暂不可用时仍应能看邮箱与昵称）
	spiders := make([]*profile.GetByIdRes_Spiders, 0)
	var lastSyncAt int64
	if conn, err := p.coreDataRPC(); err != nil {
		log.Warnf("GetById spider dial user=%d: %v", pf.ID, err)
	} else {
		s := spider.NewSpiderClient(conn)
		if sp, err := s.GetSpider(ctx, &spider.GetSpiderReq{UserId: int64(pf.ID)}); err != nil {
			log.Warnf("GetById spider user=%d: %v", pf.ID, err)
		} else if sp != nil {
			for _, v := range sp.Data {
				spiders = append(spiders, &profile.GetByIdRes_Spiders{
					Platform:   v.Platform,
					Username:   v.Username,
					Rating:     v.GetRating(),
					HasRating:  v.GetHasRating(),
					LastSyncAt: v.GetLastSyncAt(),
					LastFailAt: v.GetLastFailAt(),
					LastError:  v.GetLastError(),
				})
			}
			lastSyncAt = sp.GetLastSyncAt()
		}
	}
	canViewPrivate := auth.VerifySelfOrAbove(ctx, pf.ID)
	dailyGrant, weeklyGrant := false, false
	emailOn, weeklyOn := false, false
	if canViewPrivate {
		dailyGrant = p.profileDal.UserHasOrgDailyEmailGrant(ctx, int64(pf.ID))
		weeklyGrant = p.profileDal.UserHasOrgWeeklyEmailGrant(ctx, int64(pf.ID))
		emailOn = pf.EmailEnabled && dailyGrant
		weeklyOn = pf.EmailWeeklyEnabled && weeklyGrant
	}
	// 当前组织视图：在组织用 org_display_name（空则 username）；不在组织用公共域昵称
	displayName := strings.TrimSpace(pf.Name)
	if displayName == "" {
		displayName = pf.Username
	}
	if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
		if m, e := p.profileDal.OrgDisplayNamesByUserIDs(ctx, pd.OrgID, []uint{pf.ID}); e == nil {
			if d, inOrg := m[pf.ID]; inOrg {
				if strings.TrimSpace(d) != "" {
					displayName = strings.TrimSpace(d)
				} else if pf.Username != "" {
					displayName = pf.Username
				}
			}
			// 不在当前组织：保持公共域昵称（users.name）
		}
	}
	reply := &profile.GetByIdRes{
		UserId:                  uint64(pf.ID),
		Username:                pf.Username,
		Name:                    displayName,
		Avatar:                  pf.Avatar,
		Spiders:                 spiders,
		EmailEnabled:            emailOn,
		EmailWeeklyEnabled:      weeklyOn,
		EmailAllowedByOrg:       dailyGrant,
		EmailWeeklyAllowedByOrg: weeklyGrant,
		LastSyncAt:              lastSyncAt,
	}
	if canViewPrivate {
		reply.Email = pf.Email
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			reply.GroupId = p.profileDal.GroupIDForOrg(ctx, int64(pf.ID), pd.OrgID)
		}
		reply.RoleId = int32(pf.RoleID)
	}
	return reply, nil
}

func NewProfileService(profileDal *dal.ProfileDal, reg *discovery.Register, profileUseCase *biz.ProfileUseCase, d *data.Data) *ProfileService {
	var db *gorm.DB
	if d != nil {
		db = d.DB
	}
	return &ProfileService{
		profileDal:     profileDal,
		reg:            reg,
		profileUseCase: profileUseCase,
		socialDal:      dal.NewSocialDal(d),
		db:             db,
	}
}

// GetUserIdsByGroup 根据组ID获取用户ID列表
func (p *ProfileService) GetUserIdsByGroup(ctx context.Context, req *profile.GetUserIdsByGroupReq) (*profile.GetUserIdsByGroupRes, error) {
	ids, err := p.profileUseCase.GetUserIdsByGroup(ctx, req.GroupId)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.GetUserIdsByGroupRes{
		UserIds: ids,
	}, nil
}

// GetSyncPolicies 定时任务用：多组织 MIN 间隔 + 开关任一开启
func (p *ProfileService) GetSyncPolicies(ctx context.Context, req *profile.GetSyncPoliciesReq) (*profile.GetSyncPoliciesRes, error) {
	ids := req.UserIds
	if len(ids) == 0 {
		return &profile.GetSyncPoliciesRes{Policies: nil}, nil
	}
	// 去重
	seen := make(map[int64]struct{}, len(ids))
	uniq := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	list, err := p.profileDal.GetSyncPolicies(ctx, uniq)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	res := &profile.GetSyncPoliciesRes{Policies: make([]*profile.UserSyncPolicy, 0, len(list))}
	for _, v := range list {
		res.Policies = append(res.Policies, &profile.UserSyncPolicy{
			UserId:               v.UserID,
			EnableSpider:         v.EnableSpider,
			EnableAiSummary:      v.EnableAISummary,
			EnableAiEmail:        v.EnableAIEmail,
			EnableAiWeeklyEmail:  v.EnableAIWeeklyEmail,
			IsOrgStaff:           v.IsOrgStaff,
			EmailEnabled:         v.EmailEnabled,
			EmailWeeklyEnabled:   v.EmailWeeklyEnabled,
			SpiderIntervalMin:    int32(v.SpiderIntervalMin),
			AiSummaryIntervalMin: int32(v.AISummaryIntervalMin),
			SyncActive:           v.SyncActive,
		})
	}
	return res, nil
}

// SetSyncExempt 站点管理员：永不休眠
func (p *ProfileService) SetSyncExempt(ctx context.Context, req *profile.SetSyncExemptReq) (*profile.SetSyncExemptRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("权限不足", "需要用户同步运维权限")
	}
	if req.UserId <= 0 {
		return nil, errors.BadRequest("参数错误", "用户ID无效")
	}
	if _, err := p.profileDal.GetById(ctx, req.UserId); err != nil {
		return nil, errors.BadRequest("参数错误", "用户不存在")
	}
	if err := p.profileDal.SetSyncExempt(ctx, req.UserId, req.Exempt); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	msg := "已取消永不休眠"
	if req.Exempt {
		msg = "已标记永不休眠"
	}
	return &profile.SetSyncExemptRes{Code: 0, Message: msg}, nil
}

// ClearDormant 站点管理员：批量刷新最近活跃时间，一次性解除休眠（非永久豁免）
func (p *ProfileService) ClearDormant(ctx context.Context, req *profile.ClearDormantReq) (*profile.ClearDormantRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("权限不足", "需要用户同步运维权限")
	}
	if req == nil || len(req.UserIds) == 0 {
		return nil, errors.BadRequest("参数错误", "请选择至少一个用户")
	}
	const maxBatch = 200
	if len(req.UserIds) > maxBatch {
		return nil, errors.BadRequest("参数错误", fmt.Sprintf("单次最多 %d 人", maxBatch))
	}
	n, err := p.profileDal.TouchLastLoginBatch(ctx, req.UserIds, time.Now())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	// 站内信通知被解冻用户（一次批量插入）
	if n > 0 && p.db != nil {
		p.batchNotifyUsers(req.UserIds, notify.TypeUserUnfrozen,
			"账号已恢复活跃", "站点管理员已解除你的不活跃状态，自动同步等功能将按规则恢复")
	}
	return &profile.ClearDormantRes{
		Code:    0,
		Message: fmt.Sprintf("已解除 %d 人的不活跃状态", n),
		Updated: int32(n),
	}, nil
}

// ForceDormant 站点管理员：强制冻结（不遵循组织约定/始终同步等豁免；登录或解除后恢复）
func (p *ProfileService) ForceDormant(ctx context.Context, req *profile.ForceDormantReq) (*profile.ForceDormantRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("权限不足", "需要用户同步运维权限")
	}
	if req == nil {
		return nil, errors.BadRequest("参数错误", "请求无效")
	}
	const maxBatch = 200
	callerID := int64(auth.GetCurrentUserId(ctx))
	var targetIDs []int64
	var requested int
	var skippedSelf int32
	if len(req.UserIds) > 0 {
		// 勾选模式：站管可冻任何人（除自己）
		seen := make(map[int64]struct{}, len(req.UserIds))
		for _, id := range req.UserIds {
			if id <= 0 {
				continue
			}
			if id == callerID {
				skippedSelf++
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			targetIDs = append(targetIDs, id)
		}
		requested = len(targetIDs) + int(skippedSelf)
		if len(targetIDs) == 0 {
			if skippedSelf > 0 {
				return nil, errors.BadRequest("参数错误", "不能冻结自己的账号")
			}
			return nil, errors.BadRequest("参数错误", "请选择至少一个用户")
		}
		if len(targetIDs) > maxBatch {
			return nil, errors.BadRequest("参数错误", fmt.Sprintf("单次最多 %d 人", maxBatch))
		}
	} else {
		// 一键模式：最近 N 天未登录（含原豁免用户）
		days := int(req.GetInactiveDays())
		if days <= 0 {
			return nil, errors.BadRequest("参数错误", "请填写未登录天数，或勾选要冻结的用户")
		}
		days = dormancy.ClampInactiveDays(days)
		ids, err := p.profileDal.ListFreezableInactiveUserIDs(ctx, days, maxBatch)
		if err != nil {
			return nil, errors.InternalServer("内部错误", err.Error())
		}
		for _, id := range ids {
			if id == callerID {
				skippedSelf++
				continue
			}
			targetIDs = append(targetIDs, id)
		}
		requested = len(ids)
		if len(targetIDs) == 0 {
			return &profile.ForceDormantRes{
				Code:    0,
				Message: fmt.Sprintf("没有符合「最近 %d 天未登录」的用户", days),
				Updated: 0,
				Skipped: skippedSelf,
			}, nil
		}
	}

	// 回拨到「超过站点阈值 1 天」+ admin_force_dormant，覆盖豁免
	siteDays := p.profileDal.GetInactiveDays(ctx)
	at := time.Now().Add(-time.Duration(siteDays+1) * 24 * time.Hour)
	existing, ferr := p.profileDal.FilterExistingUserIDs(ctx, targetIDs)
	if ferr != nil {
		return nil, errors.InternalServer("内部错误", ferr.Error())
	}
	n, err := p.profileDal.ForceDormantBatch(ctx, existing, at)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	if n > 0 && p.db != nil {
		// 一次批量插入站内信
		p.batchNotifyUsers(existing, notify.TypeUserFrozen,
			"账号同步已暂停", "站点管理员已暂停你的自动同步与部分后台任务")
	}
	skipped := int32(requested) - int32(n)
	if skipped < 0 {
		skipped = 0
	}
	msg := fmt.Sprintf("已冻结 %d 人", n)
	if skipped > 0 {
		msg = fmt.Sprintf("已冻结 %d 人，跳过 %d 人", n, skipped)
	}
	return &profile.ForceDormantRes{
		Code:    0,
		Message: msg,
		Updated: int32(n),
		Skipped: skipped,
	}, nil
}

// SetDisabled 站点管理员：禁用/启用账号（禁用后无法登录，后台同步一并暂停）
func (p *ProfileService) SetDisabled(ctx context.Context, req *profile.SetDisabledReq) (*profile.SetDisabledRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserDisable) {
		return nil, errors.Forbidden("权限不足", "需要禁用账号权限")
	}
	if req == nil || req.UserId <= 0 {
		return nil, errors.BadRequest("参数错误", "用户ID无效")
	}
	callerID := int64(auth.GetCurrentUserId(ctx))
	if callerID == req.UserId {
		return nil, errors.Forbidden("权限不足", "不能禁用自己的账号")
	}
	target, err := p.profileDal.GetById(ctx, req.UserId)
	if err != nil {
		return nil, errors.BadRequest("参数错误", "用户不存在")
	}
	// 禁止禁用其他站点管理员，避免锁死管理能力
	if target.IsSiteAdmin || target.RoleID == permission.RoleAdmin {
		return nil, errors.Forbidden("权限不足", "不能禁用站点管理员账号")
	}
	if err := p.profileDal.SetDisabled(ctx, req.UserId, req.Disabled); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	msg := "已启用该账号"
	if req.Disabled {
		msg = "已禁用该账号，对方将无法登录"
		if p.db != nil {
			_ = notify.Create(p.db, notify.Row{
				UserID:  uint(req.UserId),
				Type:    notify.TypeUserFrozen,
				Title:   "账号已被禁用",
				Body:    "站点管理员已禁用你的账号，暂时无法登录。如有疑问请联系管理员。",
				RefType: "user",
				RefID:   uint(req.UserId),
			})
		}
	}
	return &profile.SetDisabledRes{Code: 0, Message: msg}, nil
}

// GetUserIdsByOrg 组织成员 ID（数据隔离）
// 可选 groupId / squadId 缩小范围；若调用方是受限 staff，再与其管理范围求交。
func (p *ProfileService) GetUserIdsByOrg(ctx context.Context, req *profile.GetUserIdsByOrgReq) (*profile.GetUserIdsByOrgRes, error) {
	orgID := uint(req.OrgId)
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
			orgID = pd.OrgID
		}
	}
	if orgID == 0 {
		// 无 org 时回落公共域
		id, err := p.profileDal.PublicOrgID(ctx)
		if err == nil {
			orgID = id
		}
	}
	if orgID == 0 {
		return &profile.GetUserIdsByOrgRes{UserIds: []int64{}, OrgId: 0}, nil
	}

	var ids []int64
	var err error
	squadID := req.GetSquadId()
	groupID := req.GetGroupId()
	switch {
	case squadID > 0:
		ids, err = p.profileDal.GetUserIdsBySquad(ctx, squadID)
	case groupID > 0:
		ids, err = p.profileDal.GetUserIdsByOrgGroup(ctx, orgID, groupID)
	default:
		ids, err = p.profileDal.GetUserIdsByOrgCached(ctx, orgID)
	}
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}

	// staff 范围限制（站管 / 组织管理员不受限）
	if pd := auth.GetCurrentUser(ctx); pd != nil && !pd.IsSiteAdmin {
		ids = p.intersectStaffScope(ctx, orgID, pd.UserID, ids)
	}
	if ids == nil {
		ids = []int64{}
	}
	return &profile.GetUserIdsByOrgRes{UserIds: ids, OrgId: int64(orgID)}, nil
}

// intersectStaffScope 将候选成员与 staff 的管理范围求交。
// 无 grant = 全组织（兼容旧任命）；有 grant 则只能看到授权组/分队内成员。
func (p *ProfileService) intersectStaffScope(ctx context.Context, orgID, staffUID uint, candidates []int64) []int64 {
	if staffUID == 0 || orgID == 0 {
		return candidates
	}
	// org_admin 全组织
	var role string
	_ = p.profileDal.DB().WithContext(ctx).Model(&model.OrgMember{}).
		Select("role").Where("org_id = ? AND user_id = ?", orgID, staffUID).
		Scan(&role).Error
	if role == model.OrgRoleOrgAdmin {
		return candidates
	}
	grants, err := p.profileDal.ListScopeGrants(ctx, orgID, staffUID)
	if err != nil || len(grants) == 0 {
		return candidates
	}
	allowed := map[int64]struct{}{}
	for _, g := range grants {
		var part []int64
		switch g.ScopeType {
		case model.ScopeTypeSquad:
			part, _ = p.profileDal.GetUserIdsBySquad(ctx, int64(g.ScopeID))
		case model.ScopeTypeGroup:
			part, _ = p.profileDal.GetUserIdsByOrgGroup(ctx, orgID, int64(g.ScopeID))
		}
		for _, id := range part {
			allowed[id] = struct{}{}
		}
	}
	out := make([]int64, 0, len(candidates))
	for _, id := range candidates {
		if _, ok := allowed[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// GetStaffOrgIds 用户作为 coach/captain/org_admin 且组织开启周报的组织列表
func (p *ProfileService) GetStaffOrgIds(ctx context.Context, req *profile.GetStaffOrgIdsReq) (*profile.GetStaffOrgIdsRes, error) {
	if req.GetUserId() <= 0 {
		return &profile.GetStaffOrgIdsRes{OrgIds: nil}, nil
	}
	ids, err := p.profileDal.StaffOrgIDsForWeekly(ctx, req.GetUserId())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		out = append(out, int64(id))
	}
	return &profile.GetStaffOrgIdsRes{OrgIds: out}, nil
}

// GetNonPublicOrgUserIds 题面流水线资格用户（爬取 / AI；含个人覆盖）
func (p *ProfileService) GetNonPublicOrgUserIds(ctx context.Context, _ *profile.GetNonPublicOrgUserIdsReq) (*profile.GetNonPublicOrgUserIdsRes, error) {
	fetchIDs, aiIDs, err := p.profileDal.GetProblemPipelineUserIds(ctx)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	if fetchIDs == nil {
		fetchIDs = []int64{}
	}
	if aiIDs == nil {
		aiIDs = []int64{}
	}
	return &profile.GetNonPublicOrgUserIdsRes{
		UserIds:      fetchIDs,
		FetchUserIds: fetchIDs,
		AiUserIds:    aiIDs,
	}, nil
}

// SetProblemPipeline 站点管理员设置个人题面爬取 / AI 覆盖
func (p *ProfileService) SetProblemPipeline(ctx context.Context, req *profile.SetProblemPipelineReq) (*profile.SetProblemPipelineRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("权限不足", "需要用户同步运维权限")
	}
	if req.UserId <= 0 {
		return nil, errors.BadRequest("参数错误", "用户ID无效")
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "fetch"
	}
	if kind != "fetch" && kind != "ai" {
		return nil, errors.BadRequest("参数错误", "kind 须为 fetch 或 ai")
	}
	if _, err := p.profileDal.GetById(ctx, req.UserId); err != nil {
		return nil, errors.BadRequest("参数错误", "用户不存在")
	}
	if err := p.profileDal.SetProblemPipeline(ctx, req.UserId, kind, req.Enabled); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	msg := "已更新题面爬取开关"
	if kind == "ai" {
		msg = "已更新题面 AI 开关"
	}
	return &profile.SetProblemPipelineRes{Code: 0, Message: msg}, nil
}

// SetSyncIntervals 站点管理员设置个人爬取 / AI 总结间隔（优先级高于组织）
func (p *ProfileService) SetSyncIntervals(ctx context.Context, req *profile.SetSyncIntervalsReq) (*profile.SetSyncIntervalsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserSync) {
		return nil, errors.Forbidden("权限不足", "需要用户同步运维权限")
	}
	if req.UserId <= 0 {
		return nil, errors.BadRequest("参数错误", "用户ID无效")
	}
	if !req.SetSpider && !req.SetAi {
		return nil, errors.BadRequest("参数错误", "请至少指定一项间隔")
	}
	// 合法范围：5 分钟～7 天（与组织配置同量级）
	const minM, maxM = 5, 7 * 24 * 60
	var spiderPtr, aiPtr *int
	if req.SetSpider {
		v := int(req.SpiderIntervalMin)
		if v < 0 {
			return nil, errors.BadRequest("参数错误", "爬取间隔无效")
		}
		if v > 0 && (v < minM || v > maxM) {
			return nil, errors.BadRequest("参数错误", fmt.Sprintf("爬取间隔须为 0（清除）或 %d–%d 分钟", minM, maxM))
		}
		spiderPtr = &v
	}
	if req.SetAi {
		v := int(req.AiSummaryIntervalMin)
		if v < 0 {
			return nil, errors.BadRequest("参数错误", "AI 总结间隔无效")
		}
		if v > 0 && (v < minM || v > maxM) {
			return nil, errors.BadRequest("参数错误", fmt.Sprintf("AI 总结间隔须为 0（清除）或 %d–%d 分钟", minM, maxM))
		}
		aiPtr = &v
	}
	if _, err := p.profileDal.GetById(ctx, req.UserId); err != nil {
		return nil, errors.BadRequest("参数错误", "用户不存在")
	}
	if err := p.profileDal.SetSyncIntervalOverrides(ctx, req.UserId, spiderPtr, aiPtr); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.SetSyncIntervalsRes{Code: 0, Message: "已更新个人同步间隔"}, nil
}

// GetByIds 批量获取用户展示名（当前组织 / 指定 org 的组织内名称）
func (p *ProfileService) GetByIds(ctx context.Context, req *profile.GetByIdsReq) (*profile.GetByIdsRes, error) {
	orgID := uint(req.OrgId)
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
			orgID = pd.OrgID
		}
	}
	if orgID == 0 {
		orgID, _ = p.profileDal.PublicOrgID(ctx)
	}
	profiles, err := p.profileDal.GetByIdsForOrgCached(ctx, orgID, req.UserIds)
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	list := make([]*profile.GetByIdsRes_UserProfile, 0, len(profiles))
	for _, v := range profiles {
		list = append(list, &profile.GetByIdsRes_UserProfile{
			UserId:   int64(v.ID),
			Name:     v.Name,
			Avatar:   v.Avatar,
			Username: v.Username,
		})
	}
	return &profile.GetByIdsRes{Profiles: list}, nil
}

// Delete 站点管理员删除用户：清空 core 训练数据 + user 库关联后硬删除账号
func (p *ProfileService) Delete(ctx context.Context, req *profile.DeleteReq) (*profile.DeleteRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteUserDelete) {
		return nil, errors.Forbidden("权限不足", "需要删除账号权限")
	}
	if req.UserId <= 0 {
		return nil, errors.BadRequest("参数错误", "用户ID无效")
	}
	callerId := int64(auth.GetCurrentUserId(ctx))
	if callerId == req.UserId {
		return nil, errors.Forbidden("权限不足", "不能删除自己")
	}
	target, err := p.profileDal.GetById(ctx, req.UserId)
	if err != nil {
		return nil, errors.BadRequest("参数错误", "用户不存在")
	}
	// 禁止删除站点管理员
	if target.IsSiteAdmin || target.RoleID == permission.RoleAdmin {
		return nil, errors.Forbidden("权限不足", "不能删除站点管理员账号")
	}
	// 先清 core_data（OJ 绑定 / 提交 / 比赛），失败则中止，避免半删
	conn, err := p.coreDataRPC()
	if err != nil {
		return nil, errors.InternalServer("内部错误", "无法连接数据服务: "+err.Error())
	}
	sc := spider.NewSpiderClient(conn)
	purgeRes, err := sc.PurgeUserData(ctx, &spider.PurgeUserDataReq{UserId: req.UserId})
	if err != nil {
		return nil, errors.InternalServer("内部错误", "清空训练数据失败: "+err.Error())
	}
	if purgeRes != nil && purgeRes.Code != 0 {
		return nil, errors.InternalServer("内部错误", purgeRes.Message)
	}
	if err := p.profileDal.Delete(ctx, req.UserId); err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.DeleteRes{
		Code:    0,
		Message: "已删除用户并清空相关数据",
	}, nil
}

// batchNotifyUsers 批量冻结/解冻等场景的站内信：在 user 侧组好 []model.Notification
// 后一次 CreateInBatches 落库（notify 包仅提供逐条接口，且归 common 不在本侧修改）。
func (p *ProfileService) batchNotifyUsers(userIDs []int64, notifType, title, body string) {
	if p == nil || p.db == nil || len(userIDs) == 0 {
		return
	}
	now := time.Now()
	seen := make(map[int64]struct{}, len(userIDs))
	rows := make([]model.Notification, 0, len(userIDs))
	for _, id := range userIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		rows = append(rows, model.Notification{
			CreatedAt: now,
			UserID:    uint(id),
			Type:      notifType,
			Title:     title,
			Body:      body,
			RefType:   "user",
			RefID:     uint(id),
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := p.db.CreateInBatches(&rows, 200).Error; err != nil {
		log.Warnf("批量站内信写入失败 type=%s count=%d: %v", notifType, len(rows), err)
	}
}

// canManageEmailPrefs 本人、站点管理员，或当前组织 staff 管理本组织成员
func (p *ProfileService) canManageEmailPrefs(ctx context.Context, targetUserID int64) bool {
	if auth.VerifySelfOrAbove(ctx, uint(targetUserID)) {
		return true
	}
	if !auth.HasPerm(ctx, rbac.PermOrgMemberEmail) {
		return false
	}
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.OrgID == 0 {
		return false
	}
	return p.profileDal.IsMemberOfOrg(ctx, targetUserID, pd.OrgID)
}

// SetEmailEnabled 设置用户日报/周报邮件开关（kind=daily|weekly）
func (p *ProfileService) SetEmailEnabled(ctx context.Context, req *profile.SetEmailEnabledReq) (*profile.SetEmailEnabledRes, error) {
	if !p.canManageEmailPrefs(ctx, req.UserId) {
		return nil, UpdateForbidden
	}
	kind := req.Kind
	if kind == "" {
		kind = "daily"
	}
	if req.Enabled {
		if kind == "weekly" {
			if !p.profileDal.UserHasOrgWeeklyEmailGrant(ctx, req.UserId) {
				return &profile.SetEmailEnabledRes{
					Code:    1,
					Message: "无法开启周报：需在组织中为教练/队长/团队管理员，且组织已开通周报",
				}, nil
			}
		} else {
			if !p.profileDal.UserHasOrgDailyEmailGrant(ctx, req.UserId) {
				return &profile.SetEmailEnabledRes{
					Code:    1,
					Message: "无法开启日报：该用户所在组织未开通日报邮件权限",
				}, nil
			}
		}
	}
	var err error
	if kind == "weekly" {
		err = p.profileDal.SetEmailWeeklyEnabled(ctx, req.UserId, req.Enabled)
	} else {
		err = p.profileDal.SetEmailEnabled(ctx, req.UserId, req.Enabled)
	}
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	return &profile.SetEmailEnabledRes{
		Code:    0,
		Message: "设置成功",
	}, nil
}

// GetContactEmail 服务间调用：直接返回用户联系邮箱，不做隐私剥离、不拉 spider
func (p *ProfileService) GetContactEmail(ctx context.Context, req *profile.GetContactEmailReq) (*profile.GetContactEmailRes, error) {
	if req.GetUserId() <= 0 {
		return &profile.GetContactEmailRes{Email: ""}, nil
	}
	pf, err := p.profileDal.GetById(ctx, req.GetUserId())
	if err != nil || pf == nil {
		return &profile.GetContactEmailRes{Email: ""}, nil
	}
	return &profile.GetContactEmailRes{Email: strings.TrimSpace(pf.Email)}, nil
}
