package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/utils/auth"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

func requireAdminBlogImages(ctx khttp.Context) bool {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), http.StatusUnauthorized, map[string]interface{}{
			"code": 1, "message": "请先登录",
		})
		return false
	}
	if !pd.IsSiteAdmin {
		writeJSON(ctx.Response(), http.StatusForbidden, map[string]interface{}{
			"code": 1, "message": "仅站点管理员可操作",
		})
		return false
	}
	return true
}

func adminBlogImageListOptions(ctx khttp.Context) blogimg.AdminImageListOptions {
	page, _ := strconv.Atoi(strings.TrimSpace(ctx.Query().Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(ctx.Query().Get("pageSize")))
	return blogimg.AdminImageListOptions{
		Mode: strings.TrimSpace(ctx.Query().Get("mode")), Page: page, PageSize: pageSize,
	}
}

func (s *BlogService) handleAdminBlogImages(ctx khttp.Context) error {
	if !requireAdminBlogImages(ctx) {
		return nil
	}
	client := blogimg.LoadUpyunClient(s.db)
	result, err := blogimg.ListAdminImageAssets(s.db, client.PublicBaseURL(), adminBlogImageListOptions(ctx))
	if err != nil {
		writeJSON(ctx.Response(), http.StatusInternalServerError, map[string]interface{}{
			"code": 1, "message": "加载图片失败",
		})
		return nil
	}
	writeJSON(ctx.Response(), http.StatusOK, map[string]interface{}{
		"code": 0, "message": "success", "data": result,
	})
	return nil
}

func writeAdminBlogImageDeleteError(ctx khttp.Context, err error, deleted int) {
	if errors.Is(err, blogimg.ErrAdminImageNotCandidate) ||
		errors.Is(err, blogimg.ErrAdminImageSnapshotStale) {
		writeJSON(ctx.Response(), http.StatusConflict, map[string]interface{}{
			"code": 1, "message": "图片状态已变化，请刷新后重试",
			"data": map[string]interface{}{"deleted": deleted, "stale": true},
		})
		return
	}
	writeJSON(ctx.Response(), http.StatusBadGateway, map[string]interface{}{
		"code": 1, "message": "删除图片失败",
		"data": map[string]interface{}{"deleted": deleted},
	})
}

func (s *BlogService) handleAdminBlogImageDelete(ctx khttp.Context) error {
	if !requireAdminBlogImages(ctx) {
		return nil
	}
	var body struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "缺少图片 id",
		})
		return nil
	}
	deleted, err := blogimg.DeleteAdminImage(s.db, blogimg.LoadUpyunClient(s.db), body.ID)
	if err != nil {
		writeAdminBlogImageDeleteError(ctx, err, 0)
		return nil
	}
	count := 0
	if deleted {
		count = 1
	}
	writeJSON(ctx.Response(), http.StatusOK, map[string]interface{}{
		"code": 0, "message": "success", "data": map[string]interface{}{"deleted": count},
	})
	return nil
}

func (s *BlogService) handleAdminBlogImagesDeleteBatch(ctx khttp.Context) error {
	if !requireAdminBlogImages(ctx) {
		return nil
	}
	var body struct {
		IDs      []uint `json:"ids"`
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil ||
		len(body.IDs) == 0 || strings.TrimSpace(body.Snapshot) == "" {
		writeJSON(ctx.Response(), http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "缺少清理候选或快照",
		})
		return nil
	}
	deleted, err := blogimg.DeleteAdminImagesSnapshot(
		s.db, blogimg.LoadUpyunClient(s.db), body.IDs, body.Snapshot,
	)
	if err != nil {
		writeAdminBlogImageDeleteError(ctx, err, deleted)
		return nil
	}
	writeJSON(ctx.Response(), http.StatusOK, map[string]interface{}{
		"code": 0, "message": "success", "data": map[string]interface{}{"deleted": deleted},
	})
	return nil
}
