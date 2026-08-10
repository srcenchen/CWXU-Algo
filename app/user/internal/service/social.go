package service

import (
	"context"
	"strings"

	pb "cwxu-algo/api/user/v1/social"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
)

func logSocialErr(op string, uid uint, err error) {
	log.Errorf("social %s uid=%d: %v", op, uid, err)
}

// SocialService 关注 / 粉丝 / 隐私
// 实现 proto：api/user/v1/social/social.proto（SocialHTTPServer）。
type SocialService struct {
	social *dal.SocialDal
	dbData *data.Data
}

func NewSocialService(d *data.Data) *SocialService {
	return &SocialService{social: dal.NewSocialDal(d), dbData: d}
}

func socialUserPB(u dal.SocialUser, avatarBase string) *pb.SocialUser {
	siteRoles := u.SiteRoles
	if siteRoles == nil {
		siteRoles = []string{}
	}
	shared := make([]*pb.SharedOrg, 0, len(u.SharedOrgs))
	for _, a := range u.SharedOrgs {
		shared = append(shared, &pb.SharedOrg{
			OrgId:       int64(a.OrgID),
			OrgName:     a.OrgName,
			DisplayName: a.DisplayName,
		})
	}
	return &pb.SocialUser{
		UserId:       int64(u.UserID),
		Username:     u.Username,
		Name:         u.Name,
		Avatar:       expandAvatarBase(avatarBase, u.Avatar),
		InCurrentOrg: u.InCurrentOrg,
		SharedOrgs:   shared,
		IsSiteAdmin:  u.IsSiteAdmin,
		// 自定义站点角色名（公共域 badge）
		SiteRoles: siteRoles,
		// C 端订阅档（badge 数据源；已过期映射为空）
		SubTier: u.SubTier,
	}
}

// viewerContext 从 JWT 取观众 userId / orgId
func viewerContext(ctx context.Context) (viewerID, viewerOrgID uint) {
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		return pd.UserID, pd.OrgID
	}
	return 0, 0
}

func (s *SocialService) enrichList(ctx context.Context, list []dal.SocialUser) []dal.SocialUser {
	viewerID, viewerOrgID := viewerContext(ctx)
	return s.social.EnrichDisplay(ctx, viewerID, viewerOrgID, list)
}

// Follow 关注（需登录）
func (s *SocialService) Follow(ctx context.Context, req *pb.FollowReq) (*pb.FollowRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.FollowRes{Success: false, Message: "请先登录"}, nil
	}
	userID := uint(req.UserId)
	if userID == 0 {
		return &pb.FollowRes{Success: false, Message: "请指定要关注的用户"}, nil
	}
	if err := s.social.Follow(ctx, pd.UserID, userID); err != nil {
		return &pb.FollowRes{Success: false, Message: err.Error()}, nil
	}
	if s.dbData != nil {
		dal.InvalidateFollowingCacheRDB(context.Background(), s.dbData.RDB, pd.UserID)
	}
	return &pb.FollowRes{Success: true, Message: "已关注"}, nil
}

// Unfollow 取消关注（需登录）
func (s *SocialService) Unfollow(ctx context.Context, req *pb.UnfollowReq) (*pb.UnfollowRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.UnfollowRes{Success: false, Message: "请先登录"}, nil
	}
	userID := uint(req.UserId)
	if userID == 0 {
		return &pb.UnfollowRes{Success: false, Message: "请指定用户"}, nil
	}
	_ = s.social.Unfollow(ctx, pd.UserID, userID)
	if s.dbData != nil {
		dal.InvalidateFollowingCacheRDB(context.Background(), s.dbData.RDB, pd.UserID)
	}
	return &pb.UnfollowRes{Success: true, Message: "已取消关注"}, nil
}

// Following 关注列表（公开读；userId 缺省为当前登录用户）
func (s *SocialService) Following(ctx context.Context, req *pb.FollowingReq) (*pb.FollowingRes, error) {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &pb.FollowingRes{Success: false, Message: "请指定用户"}, nil
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	list, total, err := s.social.ListFollowing(ctx, uid, page, pageSize)
	if err != nil {
		// 保留简短用户文案；细节打日志便于排查
		logSocialErr("ListFollowing", uid, err)
		return &pb.FollowingRes{Success: false, Message: "加载失败"}, nil
	}
	list = s.enrichList(ctx, list)
	items := make([]*pb.SocialUser, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserPB(u, avatarBase))
	}
	return &pb.FollowingRes{Success: true, Message: "ok", List: items, Total: total}, nil
}

// Followers 粉丝列表（公开读；userId 缺省为当前登录用户）
func (s *SocialService) Followers(ctx context.Context, req *pb.FollowersReq) (*pb.FollowersRes, error) {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &pb.FollowersRes{Success: false, Message: "请指定用户"}, nil
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	list, total, err := s.social.ListFollowers(ctx, uid, page, pageSize)
	if err != nil {
		logSocialErr("ListFollowers", uid, err)
		return &pb.FollowersRes{Success: false, Message: "加载失败"}, nil
	}
	list = s.enrichList(ctx, list)
	items := make([]*pb.SocialUser, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserPB(u, avatarBase))
	}
	return &pb.FollowersRes{Success: true, Message: "ok", List: items, Total: total}, nil
}

// Counts 关注 / 粉丝计数（公开读）
func (s *SocialService) Counts(ctx context.Context, req *pb.CountsReq) (*pb.CountsRes, error) {
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &pb.CountsRes{Success: false, Message: "请指定用户"}, nil
	}
	following, followers, err := s.social.Counts(ctx, uid)
	if err != nil {
		return &pb.CountsRes{Success: false, Message: "加载失败"}, nil
	}
	return &pb.CountsRes{
		Success: true, Message: "ok",
		FollowingCount: following, FollowerCount: followers,
	}, nil
}

// Relation 与目标用户的关系（公开读；JWT 可选）
func (s *SocialService) Relation(ctx context.Context, req *pb.RelationReq) (*pb.RelationRes, error) {
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &pb.RelationRes{Success: false, Message: "请指定用户"}, nil
	}
	pd := auth.GetCurrentUser(ctx)
	isFollowing, isFollower := false, false
	if pd != nil && pd.UserID > 0 {
		isFollowing = s.social.IsFollowing(ctx, pd.UserID, uid)
		isFollower = s.social.IsFollowing(ctx, uid, pd.UserID)
	}
	return &pb.RelationRes{
		Success: true, Message: "ok",
		IsFollowing: isFollowing, IsFollower: isFollower,
	}, nil
}

// Search 用户搜索（公共域；公开读，JWT 可选）
func (s *SocialService) Search(ctx context.Context, req *pb.SearchReq) (*pb.SearchRes, error) {
	avatarBase := avatarPublicBase(s.dbData.DB)
	q := strings.TrimSpace(req.Q)
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	viewerID, viewerOrgID := viewerContext(ctx)
	list, total, err := s.social.SearchUsersInContext(ctx, q, page, pageSize, viewerID, viewerOrgID)
	if err != nil {
		logSocialErr("SearchUsers", 0, err)
		return &pb.SearchRes{Success: false, Message: "搜索失败"}, nil
	}
	items := make([]*pb.SocialUser, 0, len(list))
	for _, u := range list {
		items = append(items, socialUserPB(u, avatarBase))
	}
	return &pb.SearchRes{Success: true, Message: "ok", List: items, Total: total}, nil
}

// Identity 单用户域感知展示（资料页等复用；公开读，JWT 可选）
func (s *SocialService) Identity(ctx context.Context, req *pb.IdentityReq) (*pb.IdentityRes, error) {
	avatarBase := avatarPublicBase(s.dbData.DB)
	uid := uint(req.UserId)
	if uid == 0 {
		uid = auth.GetCurrentUserId(ctx)
	}
	if uid == 0 {
		return &pb.IdentityRes{Success: false, Message: "请指定用户"}, nil
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
		return &pb.IdentityRes{Success: false, Message: "用户不存在"}, nil
	}
	list := s.enrichList(ctx, []dal.SocialUser{{
		UserID: u.UserID, Username: u.Username, Name: u.Name, Avatar: u.Avatar,
		IsSiteAdmin: u.IsSiteAdmin,
	}})
	if len(list) == 0 {
		return &pb.IdentityRes{Success: false, Message: "用户不存在"}, nil
	}
	return &pb.IdentityRes{Success: true, Message: "ok", Data: socialUserPB(list[0], avatarBase)}, nil
}

// PrivacyGet 读取隐私配置（需登录）
func (s *SocialService) PrivacyGet(ctx context.Context, req *pb.PrivacyGetReq) (*pb.PrivacyGetRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.PrivacyGetRes{Success: false, Message: "请先登录"}, nil
	}
	configured, allowProfile, allowFeed, err := s.social.GetPrivacy(ctx, pd.UserID)
	if err != nil {
		return &pb.PrivacyGetRes{Success: false, Message: "加载失败"}, nil
	}
	return &pb.PrivacyGetRes{
		Success: true, Message: "ok",
		PrivacyConfigured:  configured,
		AllowPublicProfile: allowProfile,
		AllowPublicFeed:    allowFeed,
	}, nil
}

// PrivacyUpdate 更新隐私配置（需登录；未传字段保持默认开启）
func (s *SocialService) PrivacyUpdate(ctx context.Context, req *pb.PrivacyUpdateReq) (*pb.PrivacyUpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.PrivacyUpdateRes{Success: false, Message: "请先登录"}, nil
	}
	allowProfile, allowFeed := true, true
	if req.AllowPublicProfile != nil {
		allowProfile = *req.AllowPublicProfile
	}
	if req.AllowPublicFeed != nil {
		allowFeed = *req.AllowPublicFeed
	}
	if err := s.social.UpdatePrivacy(ctx, pd.UserID, allowProfile, allowFeed); err != nil {
		return &pb.PrivacyUpdateRes{Success: false, Message: "保存失败"}, nil
	}
	return &pb.PrivacyUpdateRes{
		Success: true, Message: "已保存",
		PrivacyConfigured:  true,
		AllowPublicProfile: allowProfile,
		AllowPublicFeed:    allowFeed,
	}, nil
}

// PrivacyStatus 隐私是否已配置（公开读；未登录不弹窗）
func (s *SocialService) PrivacyStatus(ctx context.Context, req *pb.PrivacyStatusReq) (*pb.PrivacyStatusRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.PrivacyStatusRes{Success: true, PrivacyConfigured: true}, nil // 未登录不弹窗
	}
	configured, _, _, err := s.social.GetPrivacy(ctx, pd.UserID)
	if err != nil {
		return &pb.PrivacyStatusRes{Success: false, Message: "加载失败"}, nil
	}
	return &pb.PrivacyStatusRes{Success: true, PrivacyConfigured: configured}, nil
}
