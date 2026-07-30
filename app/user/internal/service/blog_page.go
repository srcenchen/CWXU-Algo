package service

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

const (
	maxBlogPageTitle   = 200
	maxBlogPageSlug    = 96
	maxBlogPageNav     = 64
	maxBlogPageContent = 256 << 10
)

var reservedBlogPageSlugs = map[string]struct{}{
	"api": {}, "manage": {}, "pages": {}, "categories": {}, "archives": {},
}

type blogPageWriteReq struct {
	ID        uint   `json:"id"`
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	ContentMD string `json:"contentMd"`
	Status    string `json:"status"`
	ShowInNav bool   `json:"showInNav"`
	NavLabel  string `json:"navLabel"`
	NavOrder  int    `json:"navOrder"`
}

type blogPageOrderItem struct {
	ID       uint `json:"id"`
	NavOrder int  `json:"navOrder"`
}

func normalizeBlogPageWrite(db *gorm.DB, userID uint, req blogPageWriteReq) (*model.BlogPage, string) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, "标题不能为空"
	}
	if utf8.RuneCountInString(title) > maxBlogPageTitle {
		return nil, "标题过长"
	}

	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	if slug == "" {
		return nil, "页面地址不能为空"
	}
	if utf8.RuneCountInString(slug) > maxBlogPageSlug {
		return nil, "页面地址过长"
	}
	if _, reserved := reservedBlogPageSlugs[slug]; reserved {
		return nil, "这个页面地址已被系统使用"
	}
	for _, r := range slug {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return nil, "页面地址只能包含小写字母、数字和连字符"
		}
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return nil, "页面地址格式不正确"
	}

	content := strings.ReplaceAll(req.ContentMD, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return nil, "正文不能为空"
	}
	content = blogimg.NormalizeStoredImageRefs(content)
	if len(content) > maxBlogPageContent {
		return nil, "正文过大，最大 256KB"
	}

	status := strings.ToLower(strings.TrimSpace(req.Status))
	if status == "" {
		status = model.BlogPageDraft
	}
	if status != model.BlogPageDraft && status != model.BlogPagePublished {
		return nil, "发布状态无效"
	}

	navLabel := strings.TrimSpace(req.NavLabel)
	if utf8.RuneCountInString(navLabel) > maxBlogPageNav {
		return nil, "导航名称过长"
	}

	imageHashes := blogimg.EncodeImageHashes(
		blogimg.ResolveContentHashes(db, userID, content, ""),
	)
	return &model.BlogPage{
		ID:          req.ID,
		Title:       title,
		Slug:        slug,
		ContentMD:   content,
		ImageHashes: imageHashes,
		Status:      status,
		ShowInNav:   req.ShowInNav,
		NavLabel:    navLabel,
		NavOrder:    req.NavOrder,
	}, ""
}

func (s *BlogService) listPublicBlogPages(userID uint) ([]model.BlogPage, error) {
	var list []model.BlogPage
	err := s.db.Where(
		"user_id = ? AND status = ? AND show_in_nav = ?",
		userID,
		model.BlogPagePublished,
		true,
	).Order("nav_order ASC, id ASC").Find(&list).Error
	return list, err
}

func (s *BlogService) getPublicBlogPage(userID uint, slug string) (*model.BlogPage, error) {
	var page model.BlogPage
	err := s.db.Where(
		"user_id = ? AND slug = ? AND status = ?",
		userID,
		strings.ToLower(strings.TrimSpace(slug)),
		model.BlogPagePublished,
	).First(&page).Error
	if err != nil {
		return nil, err
	}
	return &page, nil
}

func (s *BlogService) listMineBlogPages(userID uint) ([]model.BlogPage, error) {
	var list []model.BlogPage
	err := s.db.Where("user_id = ?", userID).
		Order("nav_order ASC, id ASC").Find(&list).Error
	return list, err
}

func (s *BlogService) reorderBlogPages(userID uint, items []blogPageOrderItem) error {
	if userID == 0 || len(items) == 0 {
		return errors.New("invalid reorder request")
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		seen := make(map[uint]struct{}, len(items))
		for _, item := range items {
			if item.ID == 0 {
				return errors.New("invalid page id")
			}
			if _, ok := seen[item.ID]; ok {
				return errors.New("duplicate page id")
			}
			seen[item.ID] = struct{}{}
			res := tx.Model(&model.BlogPage{}).
				Where("id = ? AND user_id = ?", item.ID, userID).
				Update("nav_order", item.NavOrder)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		}
		return nil
	})
}

func blogPageToMap(page *model.BlogPage, includeBody bool, imageBase string) map[string]interface{} {
	content := ""
	if includeBody {
		content = blogimg.ExpandStoredImageRefs(page.ContentMD, imageBase)
	}
	label := strings.TrimSpace(page.NavLabel)
	if label == "" {
		label = page.Title
	}
	return map[string]interface{}{
		"id":        page.ID,
		"title":     page.Title,
		"slug":      page.Slug,
		"contentMd": content,
		"status":    page.Status,
		"showInNav": page.ShowInNav,
		"navLabel":  label,
		"navOrder":  page.NavOrder,
		"createdAt": page.CreatedAt.Unix(),
		"updatedAt": page.UpdatedAt.Unix(),
	}
}

func blogPagesToMaps(list []model.BlogPage, includeBody bool, imageBase string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		out = append(out, blogPageToMap(&list[i], includeBody, imageBase))
	}
	return out
}

func (s *BlogService) handlePageListPublic(ctx khttp.Context) error {
	username := strings.TrimSpace(ctx.Request().URL.Query().Get("username"))
	if username == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少用户名"})
		return nil
	}
	u, err := s.findUserByUsername(username)
	if err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
		return nil
	}
	viewer := blogViewerID(ctx)
	if !s.isBlogActivated(u.ID) && viewer != u.ID {
		writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": []interface{}{}})
		return nil
	}
	list, err := s.listPublicBlogPages(u.ID)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": blogPagesToMaps(list, false, s.publicImageBase())})
	return nil
}

func (s *BlogService) handlePageGetPublic(ctx khttp.Context) error {
	username := strings.TrimSpace(ctx.Request().URL.Query().Get("username"))
	slug := strings.TrimSpace(ctx.Request().URL.Query().Get("slug"))
	if username == "" || slug == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少用户名或页面地址"})
		return nil
	}
	u, err := s.findUserByUsername(username)
	if err != nil || (!s.isBlogActivated(u.ID) && blogViewerID(ctx) != u.ID) {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "页面不存在"})
		return nil
	}
	page, err := s.getPublicBlogPage(u.ID, slug)
	if err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "页面不存在"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": blogPageToMap(page, true, s.publicImageBase())})
	return nil
}

func (s *BlogService) handlePageMine(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	list, err := s.listMineBlogPages(pd.UserID)
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": blogPagesToMaps(list, true, s.publicImageBase())})
	return nil
}

func (s *BlogService) handlePageCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !s.requireActivated(ctx, pd.UserID) {
		return nil
	}
	var req blogPageWriteReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	page, msg := normalizeBlogPageWrite(s.db, pd.UserID, req)
	if msg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": msg})
		return nil
	}
	page.ID = 0
	page.UserID = pd.UserID
	if err := s.db.Create(page).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "页面地址已被使用"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": blogPageToMap(page, true, s.publicImageBase())})
	return nil
}

func (s *BlogService) handlePageUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req blogPageWriteReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	page, msg := normalizeBlogPageWrite(s.db, pd.UserID, req)
	if msg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": msg})
		return nil
	}
	var existing model.BlogPage
	if err := s.db.Where("id = ? AND user_id = ?", req.ID, pd.UserID).First(&existing).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "页面不存在"})
		return nil
	}
	page.ID = existing.ID
	page.UserID = existing.UserID
	res := s.db.Model(&existing).
		Select("title", "slug", "content_md", "image_hashes", "status", "show_in_nav", "nav_label", "nav_order").
		Updates(page)
	if res.Error != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "页面地址已被使用"})
		return nil
	}
	if err := s.db.First(&existing, existing.ID).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": blogPageToMap(&existing, true, s.publicImageBase())})
	return nil
}

func (s *BlogService) handlePageDelete(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	res := s.db.Where("id = ? AND user_id = ?", body.ID, pd.UserID).Delete(&model.BlogPage{})
	if res.Error != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "删除失败"})
		return nil
	}
	if res.RowsAffected == 0 {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "页面不存在"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

func (s *BlogService) handlePageReorder(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		Items []blogPageOrderItem `json:"items"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || len(body.Items) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	if err := s.reorderBlogPages(pd.UserID, body.Items); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "排序内容无效"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已保存"})
	return nil
}
