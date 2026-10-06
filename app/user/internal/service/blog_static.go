package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	pb "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

const (
	maxStaticTitle = 200
	maxStaticSlug  = 64
	maxStaticNav   = 64
	maxStaticEntry = 180
)

var reservedStaticSlugs = map[string]struct{}{
	"api": {}, "manage": {}, "pages": {}, "categories": {}, "archives": {},
	"about": {}, "friends": {}, "static": {}, "new": {}, "edit": {},
}

type staticObjectWriter interface {
	Put(objectKey string, data []byte, contentType string) error
	Delete(objectKey string) error
	PublicURL(objectKey string) string
	Configured() bool
	PublicBaseURL() string
}

func normalizeStaticSlug(raw string) (string, string) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if slug == "" {
		return "", "访问路径不能为空"
	}
	if utf8.RuneCountInString(slug) > maxStaticSlug {
		return "", "访问路径过长"
	}
	if _, reserved := reservedStaticSlugs[slug]; reserved {
		return "", "这个访问路径已被系统使用"
	}
	for _, r := range slug {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return "", "访问路径只能包含小写字母、数字和连字符"
		}
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") || strings.Contains(slug, "--") {
		return "", "访问路径格式不正确"
	}
	return slug, ""
}

func normalizeStaticEntry(raw string) (string, string) {
	entry := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	entry = strings.TrimPrefix(entry, "/")
	if entry == "" {
		return "index.html", ""
	}
	clean, msg := cleanStaticRelPath(entry)
	if msg != "" {
		return "", "入口文件路径不合法"
	}
	ext := strings.ToLower(path.Ext(clean))
	if ext != ".html" && ext != ".htm" {
		return "", "入口必须是 html 文件"
	}
	if utf8.RuneCountInString(clean) > maxStaticEntry {
		return "", "入口文件路径过长"
	}
	return clean, ""
}

func staticSitePrefix(userID, siteID uint) string {
	return fmt.Sprintf("/blog-static/%d/%d", userID, siteID)
}

func (s *BlogService) requireStaticUpload(ctx context.Context, userID uint) error {
	if err := s.requireActivated(ctx, userID); err != nil {
		return err
	}
	client := s.loadUpyunClient()
	if !client.Configured() || client.PublicBaseURL() == "" {
		return blogErr(http.StatusForbidden, "站点尚未配置图床，请联系管理员")
	}
	if !blogimg.CanUpload(true, s.userImageUploadEnabled(userID)) {
		return blogErr(http.StatusForbidden, "尚未开通图片上传，请联系站点管理员在博客管理中授权")
	}
	return nil
}

func (s *BlogService) staticSiteToProto(site *model.BlogStaticSite, username string) *pb.BlogStaticSiteInfo {
	label := strings.TrimSpace(site.NavLabel)
	if label == "" {
		label = site.Title
	}
	publicPath := ""
	if username != "" && site.Slug != "" {
		publicPath = "/blog/" + username + "/static/" + site.Slug + "/"
	}
	return &pb.BlogStaticSiteInfo{
		Id:         int64(site.ID),
		Title:      site.Title,
		Slug:       site.Slug,
		Entry:      site.Entry,
		FileCount:  int32(site.FileCount),
		ShowInNav:  site.ShowInNav,
		NavLabel:   label,
		NavOrder:   int32(site.NavOrder),
		PublicPath: publicPath,
		CreatedAt:  site.CreatedAt.Unix(),
		UpdatedAt:  site.UpdatedAt.Unix(),
	}
}

// RegisterBlogStaticRoutes 注册 zip 上传与公开静态资源代读。
func RegisterBlogStaticRoutes(srv *khttp.Server, d *data.Data, blogSvc *BlogService) {
	r := srv.Route("/")
	r.POST("/v1/user/blog/static-site/upload", func(ctx khttp.Context) error {
		return handleStaticSiteUpload(ctx, d, blogSvc)
	})
	r.GET("/v1/user/blog/static/{username}/{slug}", serveBlogStatic(d))
	r.GET("/v1/user/blog/static/{username}/{slug}/{rest:.*}", serveBlogStatic(d))
}

func handleStaticSiteUpload(ctx khttp.Context, d *data.Data, blogSvc *BlogService) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return ctx.JSON(http.StatusUnauthorized, map[string]interface{}{
			"code": 1, "message": "请先登录",
		})
	}
	if blogSvc == nil || d == nil || d.DB == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"code": 1, "message": "上传服务暂不可用",
		})
	}
	if err := blogSvc.requireStaticUpload(ctx, pd.UserID); err != nil {
		return writeBlogErr(ctx, err)
	}
	req := ctx.Request()
	if err := req.ParseMultipartForm(maxStaticZipBytes); err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "解析表单失败或文件过大",
		})
	}
	title := strings.TrimSpace(req.FormValue("title"))
	if title == "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "名称不能为空",
		})
	}
	if utf8.RuneCountInString(title) > maxStaticTitle {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "名称过长",
		})
	}
	slug, msg := normalizeStaticSlug(req.FormValue("slug"))
	if msg != "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{"code": 1, "message": msg})
	}
	navLabel := strings.TrimSpace(req.FormValue("navLabel"))
	if utf8.RuneCountInString(navLabel) > maxStaticNav {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "导航名称过长",
		})
	}
	showInNav := req.FormValue("showInNav") == "1" || strings.EqualFold(req.FormValue("showInNav"), "true")
	replaceID, _ := strconv.ParseUint(strings.TrimSpace(req.FormValue("id")), 10, 64)

	file, hdr, err := req.FormFile("file")
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "请上传 zip 压缩包",
		})
	}
	defer file.Close()
	if hdr != nil && !strings.HasSuffix(strings.ToLower(hdr.Filename), ".zip") {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "请上传 zip 压缩包",
		})
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStaticZipBytes+1))
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "读取文件失败",
		})
	}
	files, msg := unpackStaticZip(raw)
	if msg != "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{"code": 1, "message": msg})
	}
	entry, msg := normalizeStaticEntry(req.FormValue("entry"))
	if msg != "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{"code": 1, "message": msg})
	}
	if strings.TrimSpace(req.FormValue("entry")) == "" {
		entry = defaultStaticEntry(files)
		if entry == "" {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "压缩包里没有可访问的 html，请包含 index.html 或指定入口",
			})
		}
	}
	if !staticZipHasEntry(files, entry) {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "入口文件不在压缩包里",
		})
	}

	var existing model.BlogStaticSite
	if replaceID > 0 {
		if err := d.DB.Where("id = ? AND user_id = ?", replaceID, pd.UserID).First(&existing).Error; err != nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "静态页不存在",
			})
		}
	}
	var dup model.BlogStaticSite
	q := d.DB.Where("user_id = ? AND slug = ?", pd.UserID, slug)
	if existing.ID > 0 {
		q = q.Where("id <> ?", existing.ID)
	}
	if err := q.First(&dup).Error; err == nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "访问路径已被使用",
		})
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"code": 1, "message": "保存失败",
		})
	}

	client := loadUpyunFromDB(d.DB)
	site := existing
	if site.ID == 0 {
		site = model.BlogStaticSite{UserID: pd.UserID}
	}
	oldPrefix := site.Prefix
	site.Title = title
	site.Slug = slug
	site.Entry = entry
	site.ShowInNav = showInNav
	site.NavLabel = navLabel
	site.FileCount = len(files)
	if site.ID == 0 {
		if err := d.DB.Create(&site).Error; err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "访问路径已被使用",
			})
		}
	}
	prefix := staticSitePrefix(pd.UserID, site.ID)
	if err := putStaticFiles(client, prefix, files); err != nil {
		if existing.ID == 0 {
			_ = d.DB.Delete(&model.BlogStaticSite{}, site.ID).Error
		}
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "上传到云存储失败，请稍后重试",
		})
	}
	site.Prefix = prefix
	if err := d.DB.Save(&site).Error; err != nil {
		if existing.ID == 0 {
			_ = d.DB.Delete(&model.BlogStaticSite{}, site.ID).Error
		}
		return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
			"code": 1, "message": "保存失败",
		})
	}
	if oldPrefix != "" && oldPrefix != prefix {
		_ = deleteStaticPrefix(client, oldPrefix, nil)
	}
	var user model.User
	_ = d.DB.Select("username").Where("id = ?", pd.UserID).First(&user).Error
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"code": 0, "message": "success",
		"data": blogSvc.staticSiteToProto(&site, user.Username),
	})
}

func putStaticFiles(client staticObjectWriter, prefix string, files []staticZipFile) error {
	if client == nil || !client.Configured() {
		return fmt.Errorf("upyun not configured")
	}
	for _, f := range files {
		key := prefix + "/" + f.RelPath
		if err := client.Put(key, f.Data, f.ContentType); err != nil {
			return err
		}
	}
	return nil
}

func deleteStaticPrefix(client staticObjectWriter, prefix string, files []staticZipFile) error {
	if client == nil || prefix == "" {
		return nil
	}
	var first error
	for _, f := range files {
		if err := client.Delete(prefix + "/" + f.RelPath); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func writeBlogErr(ctx khttp.Context, err error) error {
	if err == nil {
		return nil
	}
	code := http.StatusBadRequest
	msg := err.Error()
	if se := kerrors.FromError(err); se != nil && se.Message != "" {
		msg = se.Message
		if se.Code > 0 {
			code = int(se.Code)
		}
	}
	return ctx.JSON(code, map[string]interface{}{"code": 1, "message": msg})
}

type staticRequest interface {
	Vars() url.Values
	Request() *http.Request
	Response() http.ResponseWriter
	JSON(int, interface{}) error
}

func serveBlogStatic(d *data.Data) func(khttp.Context) error {
	return func(ctx khttp.Context) error {
		return serveBlogStaticCtx(d, ctx)
	}
}

func serveBlogStaticCtx(d *data.Data, ctx staticRequest) error {
	if d == nil || d.DB == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"code": 1, "message": "暂不可用",
		})
	}
	username := strings.TrimSpace(ctx.Vars().Get("username"))
	slug := strings.ToLower(strings.TrimSpace(ctx.Vars().Get("slug")))
	if username == "" || slug == "" {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"code": 1, "message": "页面不存在",
		})
	}
	var user model.User
	if err := d.DB.Select("id", "username").Where("username = ?", username).First(&user).Error; err != nil {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"code": 1, "message": "页面不存在",
		})
	}
	var site model.BlogStaticSite
	if err := d.DB.Where("user_id = ? AND slug = ?", user.ID, slug).First(&site).Error; err != nil {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"code": 1, "message": "页面不存在",
		})
	}
	rel := staticRelFromRequest(ctx)
	if rel == "" {
		http.Redirect(ctx.Response(), ctx.Request(), "/blog/"+username+"/static/"+slug+"/"+site.Entry, http.StatusFound)
		return nil
	}
	clean, msg := cleanStaticRelPath(rel)
	if msg != "" {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "路径不合法",
		})
	}
	client := loadUpyunFromDB(d.DB)
	url := client.PublicURL(site.Prefix + "/" + clean)
	if url == "" || !strings.HasPrefix(url, "http") {
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "静态资源暂不可用",
		})
	}
	upReq, err := http.NewRequestWithContext(ctx.Request().Context(), http.MethodGet, url, nil)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "静态资源暂不可用",
		})
	}
	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "静态资源暂不可用",
		})
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ctx.JSON(http.StatusNotFound, map[string]interface{}{
			"code": 1, "message": "文件不存在",
		})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "静态资源暂不可用",
		})
	}
	w := ctx.Response()
	w.Header().Set("Content-Type", staticContentType(clean))
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isHTMLExt(clean) {
		w.Header().Set("Content-Security-Policy", "default-src 'self' https: data: blob:; script-src 'self' 'unsafe-inline' 'unsafe-eval' https:; style-src 'self' 'unsafe-inline' https:; frame-ancestors 'self'")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxStaticFileBytes+1))
	return nil
}

func staticRelFromRequest(ctx staticRequest) string {
	if rest := strings.Trim(ctx.Vars().Get("rest"), "/"); rest != "" {
		return rest
	}
	req := ctx.Request()
	path := req.URL.Path
	marker := "/blog/static/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	tail := strings.Trim(path[i+len(marker):], "/")
	parts := strings.Split(tail, "/")
	if len(parts) <= 2 {
		return ""
	}
	return strings.Join(parts[2:], "/")
}

func isHTMLExt(rel string) bool {
	ext := strings.ToLower(path.Ext(rel))
	return ext == ".html" || ext == ".htm"
}

// StaticSiteMine GET /v1/user/blog/static-site/mine
func (s *BlogService) StaticSiteMine(ctx context.Context, _ *pb.StaticSiteMineReq) (*pb.StaticSiteMineRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	var list []model.BlogStaticSite
	if err := s.db.Where("user_id = ?", pd.UserID).Order("nav_order ASC, id ASC").Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	var user model.User
	_ = s.db.Select("username").Where("id = ?", pd.UserID).First(&user).Error
	out := make([]*pb.BlogStaticSiteInfo, 0, len(list))
	for i := range list {
		out = append(out, s.staticSiteToProto(&list[i], user.Username))
	}
	return &pb.StaticSiteMineRes{Code: 0, Message: "success", Data: out}, nil
}

// StaticSiteListPublic GET /v1/user/blog/static-site/list
func (s *BlogService) StaticSiteListPublic(ctx context.Context, req *pb.StaticSiteListPublicReq) (*pb.StaticSiteListPublicRes, error) {
	username := strings.TrimSpace(req.GetUsername())
	if username == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名")
	}
	var user model.User
	if err := s.db.Select("id", "username").Where("username = ?", username).First(&user).Error; err != nil {
		return &pb.StaticSiteListPublicRes{Code: 0, Message: "success", Data: []*pb.BlogStaticSiteInfo{}}, nil
	}
	var list []model.BlogStaticSite
	if err := s.db.Where("user_id = ? AND show_in_nav = ?", user.ID, true).
		Order("nav_order ASC, id ASC").Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	out := make([]*pb.BlogStaticSiteInfo, 0, len(list))
	for i := range list {
		out = append(out, s.staticSiteToProto(&list[i], user.Username))
	}
	return &pb.StaticSiteListPublicRes{Code: 0, Message: "success", Data: out}, nil
}

// StaticSiteUpdate POST /v1/user/blog/static-site/update
func (s *BlogService) StaticSiteUpdate(ctx context.Context, req *pb.StaticSiteUpdateReq) (*pb.StaticSiteUpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.GetId() == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var site model.BlogStaticSite
	if err := s.db.Where("id = ? AND user_id = ?", req.GetId(), pd.UserID).First(&site).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "静态页不存在")
	}
	title := strings.TrimSpace(req.GetTitle())
	if title == "" {
		return nil, blogErr(http.StatusBadRequest, "名称不能为空")
	}
	if utf8.RuneCountInString(title) > maxStaticTitle {
		return nil, blogErr(http.StatusBadRequest, "名称过长")
	}
	slug, msg := normalizeStaticSlug(req.GetSlug())
	if msg != "" {
		return nil, blogErr(http.StatusBadRequest, msg)
	}
	navLabel := strings.TrimSpace(req.GetNavLabel())
	if utf8.RuneCountInString(navLabel) > maxStaticNav {
		return nil, blogErr(http.StatusBadRequest, "导航名称过长")
	}
	var dup model.BlogStaticSite
	err := s.db.Where("user_id = ? AND slug = ? AND id <> ?", pd.UserID, slug, site.ID).First(&dup).Error
	if err == nil {
		return nil, blogErr(http.StatusBadRequest, "访问路径已被使用")
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, blogErr(http.StatusInternalServerError, "保存失败")
	}
	site.Title = title
	site.Slug = slug
	site.ShowInNav = req.GetShowInNav()
	site.NavLabel = navLabel
	site.NavOrder = int(req.GetNavOrder())
	if err := s.db.Save(&site).Error; err != nil {
		return nil, blogErr(http.StatusBadRequest, "访问路径已被使用")
	}
	var user model.User
	_ = s.db.Select("username").Where("id = ?", pd.UserID).First(&user).Error
	return &pb.StaticSiteUpdateRes{Code: 0, Message: "success", Data: s.staticSiteToProto(&site, user.Username)}, nil
}

// StaticSiteDelete POST /v1/user/blog/static-site/delete
func (s *BlogService) StaticSiteDelete(ctx context.Context, req *pb.StaticSiteDeleteReq) (*pb.StaticSiteDeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.GetId() == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var site model.BlogStaticSite
	if err := s.db.Where("id = ? AND user_id = ?", req.GetId(), pd.UserID).First(&site).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "静态页不存在")
	}
	if err := s.db.Delete(&site).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "删除失败")
	}
	return &pb.StaticSiteDeleteRes{Code: 0, Message: "success"}, nil
}
