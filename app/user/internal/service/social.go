package service

import (
	"context"
	"strconv"
	"strings"

	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

func logSocialErr(op string, uid uint, err error) {
	log.Errorf("social %s uid=%d: %v", op, uid, err)
}

// SocialService 关注 / 粉丝 / 隐私
type SocialService struct {
	social *dal.SocialDal
	dbData *data.Data
}

func NewSocialService(d *data.Data) *SocialService {
	return &SocialService{social: dal.NewSocialDal(d), dbData: d}
}

// RegisterSocialRoutes 注册社交与隐私 HTTP 路由
func RegisterSocialRoutes(srv *khttp.Server, s *SocialService) {
	r := srv.Route("/")
	r.POST("/v1/user/social/follow", s.handleFollow)
	r.POST("/v1/user/social/unfollow", s.handleUnfollow)
	r.GET("/v1/user/social/following", s.handleFollowing)
	r.GET("/v1/user/social/followers", s.handleFollowers)
	r.GET("/v1/user/social/counts", s.handleCounts)
	r.GET("/v1/user/social/relation", s.handleRelation)
	r.GET("/v1/user/social/search", s.handleSearch)
	r.GET("/v1/user/social/identity", s.handleIdentity)
	r.GET("/v1/user/privacy/get", s.handlePrivacyGet)
	r.POST("/v1/user/privacy/update", s.handlePrivacyUpdate)
	r.GET("/v1/user/privacy/status", s.handlePrivacyStatus)
}

func socialUserJSON(u dal.SocialUser, avatarBase string) map[string]interface{} {
	siteRoles := u.SiteRoles
	if siteRoles == nil {
		siteRoles = []string{}
	}
	shared := make([]map[string]interface{}, 0, len(u.SharedOrgs))
	for _, a := range u.SharedOrgs {
		shared = append(shared, map[string]interface{}{
			"orgId":       a.OrgID,
			"orgName":     a.OrgName,
			"displayName": a.DisplayName,
		})
	}
	return map[string]interface{}{
		"userId":       u.UserID,
		"username":     u.Username,
		"name":         u.Name,
		"avatar":       expandAvatarBase(avatarBase, u.Avatar),
		"inCurrentOrg": u.InCurrentOrg,
		"sharedOrgs":   shared,
		"isSiteAdmin":  u.IsSiteAdmin,
		// 自定义站点角色名（公共域 badge）
		"siteRoles": siteRoles,
	}
}

// viewerContext 从 JWT 取观众 userId / orgId
func viewerContext(ctx khttp.Context) (viewerID, viewerOrgID uint) {
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		return pd.UserID, pd.OrgID
	}
	return 0, 0
}

func (s *SocialService) enrichList(ctx khttp.Context, list []dal.SocialUser) []dal.SocialUser {
	viewerID, viewerOrgID := viewerContext(ctx)
	return s.social.EnrichDisplay(ctx, viewerID, viewerOrgID, list)
}

func (s *SocialService) handleFollow(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		UserID uint `json:"userId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定要关注的用户"})
		return nil
	}
	if err := s.social.Follow(ctx, pd.UserID, req.UserID); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": err.Error()})
		return nil
	}
	if s.dbData != nil {
		dal.InvalidateFollowingCacheRDB(context.Background(), s.dbData.RDB, pd.UserID)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "已关注"})
	return nil
}

func (s *SocialService) handleUnfollow(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		UserID uint `json:"userId"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil || req.UserID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	_ = s.social.Unfollow(ctx, pd.UserID, req.UserID)
	if s.dbData != nil {
		dal.InvalidateFollowingCacheRDB(context.Background(), s.dbData.RDB, pd.UserID)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "已取消关注"})
	return nil
}

func (s *SocialService) handleFollowing(ctx khttp.Context) error {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid, page, pageSize := socialListParams(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	list, total, err := s.social.ListFollowing(ctx, uid, page, pageSize)
	if err != nil {
		// 保留简短用户文案；细节打日志便于排查
		logSocialErr("ListFollowing", uid, err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	list = s.enrichList(ctx, list)
	items := make([]map[string]interface{}, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserJSON(u, avatarBase))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "list": items, "total": total,
	})
	return nil
}

func (s *SocialService) handleFollowers(ctx khttp.Context) error {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid, page, pageSize := socialListParams(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	list, total, err := s.social.ListFollowers(ctx, uid, page, pageSize)
	if err != nil {
		logSocialErr("ListFollowers", uid, err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	list = s.enrichList(ctx, list)
	items := make([]map[string]interface{}, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserJSON(u, avatarBase))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "list": items, "total": total,
	})
	return nil
}

func (s *SocialService) handleCounts(ctx khttp.Context) error {
	uid, _, _ := socialListParams(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	following, followers, err := s.social.Counts(ctx, uid)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"followingCount": following, "followerCount": followers,
	})
	return nil
}

func (s *SocialService) handleRelation(ctx khttp.Context) error {
	uid, _, _ := socialListParams(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	pd := auth.GetCurrentUser(ctx)
	isFollowing, isFollower := false, false
	if pd != nil && pd.UserID > 0 {
		isFollowing = s.social.IsFollowing(ctx, pd.UserID, uid)
		isFollower = s.social.IsFollowing(ctx, uid, pd.UserID)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"isFollowing": isFollowing, "isFollower": isFollower,
	})
	return nil
}

func (s *SocialService) handleSearch(ctx khttp.Context) error {
	avatarBase := avatarPublicBase(s.dbData.DB)
	q := strings.TrimSpace(ctx.Query().Get("q"))
	page, _ := strconv.Atoi(ctx.Query().Get("page"))
	pageSize, _ := strconv.Atoi(ctx.Query().Get("pageSize"))
	viewerID, viewerOrgID := viewerContext(ctx)
	list, total, err := s.social.SearchUsersInContext(ctx, q, page, pageSize, viewerID, viewerOrgID)
	if err != nil {
		logSocialErr("SearchUsers", 0, err)
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "搜索失败"})
		return nil
	}
	items := make([]map[string]interface{}, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserJSON(u, avatarBase))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "list": items, "total": total,
	})
	return nil
}

// handleIdentity 单用户域感知展示（资料页等复用）
func (s *SocialService) handleIdentity(ctx khttp.Context) error {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid, _, _ := socialListParams(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请指定用户"})
		return nil
	}
	var u struct {
		UserID      uint `gorm:"column:user_id"`
		Username    string
		Name        string
		Avatar      string
		IsSiteAdmin bool `gorm:"column:is_site_admin"`
	}
	err := s.dbData.DB.WithContext(ctx).Table("users").
		Select("id AS user_id, username, name, avatar, is_site_admin").
		Where("id = ?", uid).
		Take(&u).Error
	if err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "用户不存在"})
		return nil
	}
	list := s.enrichList(ctx, []dal.SocialUser{{
		UserID: u.UserID, Username: u.Username, Name: u.Name, Avatar: u.Avatar,
		IsSiteAdmin: u.IsSiteAdmin,
	}})
	if len(list) == 0 {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "用户不存在"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": socialUserJSON(list[0], avatarBase),
	})
	return nil
}

func (s *SocialService) handlePrivacyGet(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	configured, allowProfile, allowFeed, err := s.social.GetPrivacy(ctx, pd.UserID)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"privacyConfigured":  configured,
		"allowPublicProfile": allowProfile,
		"allowPublicFeed":    allowFeed,
	})
	return nil
}

func (s *SocialService) handlePrivacyUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		AllowPublicProfile *bool `json:"allowPublicProfile"`
		AllowPublicFeed    *bool `json:"allowPublicFeed"`
	}
	if err := readJSON(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	allowProfile, allowFeed := true, true
	if req.AllowPublicProfile != nil {
		allowProfile = *req.AllowPublicProfile
	}
	if req.AllowPublicFeed != nil {
		allowFeed = *req.AllowPublicFeed
	}
	if err := s.social.UpdatePrivacy(ctx, pd.UserID, allowProfile, allowFeed); err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "已保存",
		"privacyConfigured":  true,
		"allowPublicProfile": allowProfile,
		"allowPublicFeed":    allowFeed,
	})
	return nil
}

func (s *SocialService) handlePrivacyStatus(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"success": true, "privacyConfigured": true, // 未登录不弹窗
		})
		return nil
	}
	configured, _, _, err := s.social.GetPrivacy(ctx, pd.UserID)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "privacyConfigured": configured,
	})
	return nil
}

func socialListParams(ctx khttp.Context) (uid uint, page, pageSize int) {
	page, _ = strconv.Atoi(ctx.Query().Get("page"))
	pageSize, _ = strconv.Atoi(ctx.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if v := ctx.Query().Get("userId"); v != "" {
		n, _ := strconv.ParseUint(v, 10, 64)
		uid = uint(n)
	}
	if uid == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil {
			uid = pd.UserID
		}
	}
	return
}
