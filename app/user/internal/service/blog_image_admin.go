package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	pb "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/utils/auth"
)

// requireAdminBlogImages 校验站点管理员；未通过返回带状态码的 Kratos 错误。
func requireAdminBlogImages(ctx context.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return blogErr(http.StatusUnauthorized, "请先登录")
	}
	if !pd.IsSiteAdmin {
		return blogErr(http.StatusForbidden, "仅站点管理员可操作")
	}
	return nil
}

// AdminBlogImages GET /v1/user/blog/admin/images（mode=all|cleanup）
func (s *BlogService) AdminBlogImages(ctx context.Context, req *pb.AdminBlogImagesReq) (*pb.AdminBlogImagesRes, error) {
	if err := requireAdminBlogImages(ctx); err != nil {
		return nil, err
	}
	client := blogimg.LoadUpyunClient(s.db)
	result, err := blogimg.ListAdminImageAssets(s.db, client.PublicBaseURL(), blogimg.AdminImageListOptions{
		Mode:     strings.TrimSpace(req.Mode),
		Page:     int(req.Page),
		PageSize: int(req.PageSize),
	})
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载图片失败")
	}
	data := &pb.AdminImageListData{
		Total:        int64(result.Total),
		Page:         int64(result.Page),
		PageSize:     int64(result.PageSize),
		Mode:         result.Mode,
		CandidateIds: toInt64s(result.CandidateIDs),
		Snapshot:     result.Snapshot,
	}
	for i := range result.List {
		item := &result.List[i]
		data.List = append(data.List, &pb.AdminImageAssetInfo{
			Id:          int64(item.ID),
			UserId:      int64(item.UserID),
			Username:    item.Username,
			Name:        item.Name,
			ObjectKey:   item.ObjectKey,
			Url:         item.URL,
			ContentHash: item.ContentHash,
			Purpose:     item.Purpose,
			CreatedAt:   item.CreatedAt.Format(time.RFC3339Nano),
			Referenced:  item.Referenced,
		})
	}
	return &pb.AdminBlogImagesRes{Code: 0, Message: "success", Data: data}, nil
}

// adminBlogImageDeleteErr 删除失败：候选/快照变化 → 409，其余 → 502。
func adminBlogImageDeleteErr(err error) error {
	if errors.Is(err, blogimg.ErrAdminImageNotCandidate) ||
		errors.Is(err, blogimg.ErrAdminImageSnapshotStale) {
		return blogErr(http.StatusConflict, "图片状态已变化，请刷新后重试")
	}
	return blogErr(http.StatusBadGateway, "删除图片失败")
}

// AdminBlogImageDelete POST /v1/user/blog/admin/images/delete
func (s *BlogService) AdminBlogImageDelete(ctx context.Context, req *pb.AdminBlogImageDeleteReq) (*pb.AdminBlogImageDeleteRes, error) {
	if err := requireAdminBlogImages(ctx); err != nil {
		return nil, err
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "缺少图片 id")
	}
	deleted, err := blogimg.DeleteAdminImage(s.db, blogimg.LoadUpyunClient(s.db), uint(req.Id))
	if err != nil {
		return nil, adminBlogImageDeleteErr(err)
	}
	count := 0
	if deleted {
		count = 1
	}
	return &pb.AdminBlogImageDeleteRes{
		Code: 0, Message: "success",
		Data: &pb.AdminImageDeleteData{Deleted: int32(count)},
	}, nil
}

// AdminBlogImagesDeleteBatch POST /v1/user/blog/admin/images/delete-batch
func (s *BlogService) AdminBlogImagesDeleteBatch(ctx context.Context, req *pb.AdminBlogImagesDeleteBatchReq) (*pb.AdminBlogImagesDeleteBatchRes, error) {
	if err := requireAdminBlogImages(ctx); err != nil {
		return nil, err
	}
	if len(req.Ids) == 0 || strings.TrimSpace(req.Snapshot) == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少清理候选或快照")
	}
	ids := make([]uint, 0, len(req.Ids))
	for _, id := range req.Ids {
		ids = append(ids, uint(id))
	}
	deleted, err := blogimg.DeleteAdminImagesSnapshot(
		s.db, blogimg.LoadUpyunClient(s.db), ids, req.Snapshot,
	)
	if err != nil {
		return nil, adminBlogImageDeleteErr(err)
	}
	return &pb.AdminBlogImagesDeleteBatchRes{
		Code: 0, Message: "success",
		Data: &pb.AdminImageDeleteData{Deleted: int32(deleted)},
	}, nil
}
