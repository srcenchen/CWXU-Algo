package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	pb "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

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

func blogPageToProto(page *model.BlogPage, includeBody bool, imageBase string) *pb.BlogPageInfo {
	content := ""
	if includeBody {
		content = blogimg.ExpandStoredImageRefs(page.ContentMD, imageBase)
	}
	label := strings.TrimSpace(page.NavLabel)
	if label == "" {
		label = page.Title
	}
	return &pb.BlogPageInfo{
		Id:        int64(page.ID),
		Title:     page.Title,
		Slug:      page.Slug,
		ContentMd: content,
		Status:    page.Status,
		ShowInNav: page.ShowInNav,
		NavLabel:  label,
		NavOrder:  int32(page.NavOrder),
		CreatedAt: page.CreatedAt.Unix(),
		UpdatedAt: page.UpdatedAt.Unix(),
	}
}

func blogPagesToProtos(list []model.BlogPage, includeBody bool, imageBase string) []*pb.BlogPageInfo {
	out := make([]*pb.BlogPageInfo, 0, len(list))
	for i := range list {
		out = append(out, blogPageToProto(&list[i], includeBody, imageBase))
	}
	return out
}

// PageListPublic GET /v1/user/blog/page/list
func (s *BlogService) PageListPublic(ctx context.Context, req *pb.PageListPublicReq) (*pb.PageListPublicRes, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名")
	}
	u, err := s.findUserByUsername(username)
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "用户不存在")
	}
	viewer := blogViewerID(ctx)
	if !s.isBlogActivated(u.ID) && viewer != u.ID {
		return &pb.PageListPublicRes{Code: 0, Message: "success", Data: []*pb.BlogPageInfo{}}, nil
	}
	list, err := s.listPublicBlogPages(u.ID)
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	return &pb.PageListPublicRes{
		Code: 0, Message: "success",
		Data: blogPagesToProtos(list, false, s.publicImageBase()),
	}, nil
}

// PageGetPublic GET /v1/user/blog/page/get
func (s *BlogService) PageGetPublic(ctx context.Context, req *pb.PageGetPublicReq) (*pb.PageGetPublicRes, error) {
	username := strings.TrimSpace(req.Username)
	slug := strings.TrimSpace(req.Slug)
	if username == "" || slug == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名或页面地址")
	}
	u, err := s.findUserByUsername(username)
	if err != nil || (!s.isBlogActivated(u.ID) && blogViewerID(ctx) != u.ID) {
		return nil, blogErr(http.StatusNotFound, "页面不存在")
	}
	page, err := s.getPublicBlogPage(u.ID, slug)
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "页面不存在")
	}
	return &pb.PageGetPublicRes{
		Code: 0, Message: "success",
		Data: blogPageToProto(page, true, s.publicImageBase()),
	}, nil
}

// PageMine GET /v1/user/blog/page/mine
func (s *BlogService) PageMine(ctx context.Context, req *pb.PageMineReq) (*pb.PageMineRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	list, err := s.listMineBlogPages(pd.UserID)
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	return &pb.PageMineRes{
		Code: 0, Message: "success",
		Data: blogPagesToProtos(list, true, s.publicImageBase()),
	}, nil
}

// PageCreate POST /v1/user/blog/page/create
func (s *BlogService) PageCreate(ctx context.Context, req *pb.PageCreateReq) (*pb.PageCreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if err := s.requireActivated(ctx, pd.UserID); err != nil {
		return nil, err
	}
	writeReq := blogPageWriteReq{
		Title:     req.Title,
		Slug:      req.Slug,
		ContentMD: req.ContentMd,
		Status:    req.Status,
		ShowInNav: req.ShowInNav,
		NavLabel:  req.NavLabel,
		NavOrder:  int(req.NavOrder),
	}
	var page *model.BlogPage
	var validationMsg string
	err := blogimg.WithUserImageReferenceTx(s.db, pd.UserID, func(tx *gorm.DB) error {
		page, validationMsg = normalizeBlogPageWrite(tx, pd.UserID, writeReq)
		if validationMsg != "" {
			return gorm.ErrInvalidData
		}
		page.ID = 0
		page.UserID = pd.UserID
		return tx.Create(page).Error
	})
	if validationMsg != "" {
		return nil, blogErr(http.StatusBadRequest, validationMsg)
	}
	if err != nil {
		return nil, blogErr(http.StatusBadRequest, "页面地址已被使用")
	}
	return &pb.PageCreateRes{
		Code: 0, Message: "success",
		Data: blogPageToProto(page, true, s.publicImageBase()),
	}, nil
}

// PageUpdate POST /v1/user/blog/page/update
func (s *BlogService) PageUpdate(ctx context.Context, req *pb.PageUpdateReq) (*pb.PageUpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	writeReq := blogPageWriteReq{
		ID:        uint(req.Id),
		Title:     req.Title,
		Slug:      req.Slug,
		ContentMD: req.ContentMd,
		Status:    req.Status,
		ShowInNav: req.ShowInNav,
		NavLabel:  req.NavLabel,
		NavOrder:  int(req.NavOrder),
	}
	var page *model.BlogPage
	var existing model.BlogPage
	var validationMsg string
	err := blogimg.WithUserImageReferenceTx(s.db, pd.UserID, func(tx *gorm.DB) error {
		page, validationMsg = normalizeBlogPageWrite(tx, pd.UserID, writeReq)
		if validationMsg != "" {
			return gorm.ErrInvalidData
		}
		if err := tx.Where("id = ? AND user_id = ?", req.Id, pd.UserID).First(&existing).Error; err != nil {
			return err
		}
		page.ID = existing.ID
		page.UserID = existing.UserID
		res := tx.Model(&existing).
			Select("title", "slug", "content_md", "image_hashes", "status", "show_in_nav", "nav_label", "nav_order").
			Updates(page)
		if res.Error != nil {
			return res.Error
		}
		return tx.First(&existing, existing.ID).Error
	})
	if validationMsg != "" {
		return nil, blogErr(http.StatusBadRequest, validationMsg)
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, blogErr(http.StatusNotFound, "页面不存在")
	}
	if err != nil {
		return nil, blogErr(http.StatusBadRequest, "页面地址已被使用")
	}
	return &pb.PageUpdateRes{
		Code: 0, Message: "success",
		Data: blogPageToProto(&existing, true, s.publicImageBase()),
	}, nil
}

// PageDelete POST /v1/user/blog/page/delete
func (s *BlogService) PageDelete(ctx context.Context, req *pb.PageDeleteReq) (*pb.PageDeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var deleted int64
	err := blogimg.WithUserImageReferenceTx(s.db, pd.UserID, func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND user_id = ?", req.Id, pd.UserID).Delete(&model.BlogPage{})
		deleted = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "删除失败")
	}
	if deleted == 0 {
		return nil, blogErr(http.StatusNotFound, "页面不存在")
	}
	return &pb.PageDeleteRes{Code: 0, Message: "已删除"}, nil
}

// PageReorder POST /v1/user/blog/page/reorder
func (s *BlogService) PageReorder(ctx context.Context, req *pb.PageReorderReq) (*pb.PageReorderRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if len(req.Items) == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	items := make([]blogPageOrderItem, 0, len(req.Items))
	for _, it := range req.Items {
		if it == nil {
			continue
		}
		items = append(items, blogPageOrderItem{ID: uint(it.Id), NavOrder: int(it.NavOrder)})
	}
	if err := s.reorderBlogPages(pd.UserID, items); err != nil {
		return nil, blogErr(http.StatusBadRequest, "排序内容无效")
	}
	return &pb.PageReorderRes{Code: 0, Message: "已保存"}, nil
}
