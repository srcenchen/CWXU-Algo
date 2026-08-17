package service

import (
	"bytes"
	"crypto/rand"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

const (
	maxUploadBytes     = 3 << 20  // 3MB avatar/branding/local uploads
	maxBlogUploadBytes = 12 << 20 // raw form before compress (blog/upyun)
	staticURLPrefix    = "/api/user/static"
	staticRoutePrefix  = "/v1/user/static"
)

var imageExts = []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".ico", ".svg"}

// svgDangerous 是 svg 里可用于执行脚本 / 外链的构造，命中即拒收。
// 静态服务已带 CSP(default-src 'none'; sandbox)，这里再做一层入库前过滤。
var svgDangerous = []string{
	"<script", "<foreignobject", "<iframe", "<embed", "<object",
	"javascript:", "data:text/html", "<set", "<handler",
}

// looksLikeSVG 判断字节流是否为 svg 文档（允许 BOM / xml 声明 / 注释 / DOCTYPE 前缀）。
func looksLikeSVG(data []byte) bool {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	s := strings.ToLower(string(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))))
	i := strings.Index(s, "<svg")
	if i < 0 {
		return false
	}
	// <svg 之前只允许出现空白、xml 声明、注释、DOCTYPE
	prefix := strings.TrimSpace(s[:i])
	for prefix != "" {
		switch {
		case strings.HasPrefix(prefix, "<?xml"):
			end := strings.Index(prefix, "?>")
			if end < 0 {
				return false
			}
			prefix = strings.TrimSpace(prefix[end+2:])
		case strings.HasPrefix(prefix, "<!--"):
			end := strings.Index(prefix, "-->")
			if end < 0 {
				return false
			}
			prefix = strings.TrimSpace(prefix[end+3:])
		case strings.HasPrefix(prefix, "<!doctype"):
			end := strings.Index(prefix, ">")
			if end < 0 {
				return false
			}
			prefix = strings.TrimSpace(prefix[end+1:])
		default:
			return false
		}
	}
	return true
}

// safeSVG 拒绝含脚本 / 事件处理器 / 外部引用的 svg。
func safeSVG(data []byte) bool {
	s := strings.ToLower(string(data))
	for _, bad := range svgDangerous {
		if strings.Contains(s, bad) {
			return false
		}
	}
	// on* 事件处理器：onload= / onclick= 等。
	// 必须处在属性名起始位置，否则 font="..." 这类合法属性会被误杀。
	isBoundary := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '<' || c == '/' || c == '"' || c == '\''
	}
	for i := 0; i+2 < len(s); i++ {
		if s[i] == 'o' && s[i+1] == 'n' && (i == 0 || isBoundary(s[i-1])) {
			j := i + 2
			for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
				j++
			}
			if j == i+2 { // "on" 后没有事件名，不是处理器
				continue
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && s[j] == '=' {
				return false
			}
		}
	}
	return true
}

func UploadDir() string {
	if d := os.Getenv("CWXU_UPLOAD_DIR"); d != "" {
		return d
	}
	return "./data/uploads"
}

func ensureUploadDir() error {
	return os.MkdirAll(UploadDir(), 0o755)
}

func randomName() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102") + "_" + hex.EncodeToString(b)
}

func extFromContentType(ct, filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".ico", ".svg":
		return ext
	}
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		return strings.ToLower(exts[0])
	}
	return ""
}

func allowedImage(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp",
		"image/x-icon", "image/vnd.microsoft.icon", "image/svg+xml":
		return true
	default:
		return false
	}
}

func contentTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".svg":
		return "image/svg+xml"
	default:
		return ""
	}
}

func validImageData(data []byte, ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch ct {
	case "image/jpeg", "image/png", "image/gif":
		_, _, err := image.DecodeConfig(bytes.NewReader(data))
		return err == nil
	case "image/webp":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return len(data) >= 6 && data[0] == 0 && data[1] == 0 && data[2] == 1 && data[3] == 0
	case "image/svg+xml":
		return looksLikeSVG(data) && safeSVG(data)
	default:
		return false
	}
}

func isImageExt(ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range imageExts {
		if e == ext {
			return true
		}
	}
	return false
}

// resolveUploadFile 在上传目录内安全解析相对路径。
// 支持：精确路径、无后缀（探测常见图片后缀）、错误后缀（剥后缀再探测）。
func resolveUploadFile(rel string) (abs string, ext string, err error) {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.Contains(rel, "..") {
		return "", "", os.ErrNotExist
	}
	base := UploadDir()
	try := func(p string) (string, string, error) {
		p = filepath.Clean(p)
		absPath := filepath.Join(base, p)
		relCheck, e := filepath.Rel(base, absPath)
		if e != nil || strings.HasPrefix(relCheck, "..") {
			return "", "", os.ErrNotExist
		}
		st, e := os.Stat(absPath)
		if e != nil || st.IsDir() {
			return "", "", os.ErrNotExist
		}
		return absPath, strings.ToLower(filepath.Ext(absPath)), nil
	}

	if abs, ext, e := try(rel); e == nil {
		return abs, ext, nil
	}

	// 无后缀或带图片后缀但磁盘后缀不一致：按 stem 探测
	stem := rel
	if e := filepath.Ext(rel); isImageExt(e) {
		stem = strings.TrimSuffix(rel, e)
	}
	for _, e := range imageExts {
		if abs, ext, err := try(stem + e); err == nil {
			return abs, ext, nil
		}
	}
	return "", "", os.ErrNotExist
}

func serveUploadFile(w http.ResponseWriter, r *http.Request, prefix string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, prefix)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasSuffix(r.URL.Path, "/") {
		http.NotFound(w, r)
		return
	}

	abs, ext, err := resolveUploadFile(rel)
	if err != nil || !isImageExt(ext) {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	head := make([]byte, 512)
	n, _ := f.Read(head)
	ct := contentTypeFromExt(ext)
	if ct == "" {
		ct = http.DetectContentType(head[:n])
	}
	if ct == "" || ct == "application/octet-stream" {
		if t := contentTypeFromExt(ext); t != "" {
			ct = t
		} else {
			ct = "application/octet-stream"
		}
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(abs), st.ModTime(), f)
}

// RegisterUploadRoutes 注册 multipart 上传与静态文件。
// d 可为 nil（仅本地上传；博客又拍云路径将拒绝）。
func RegisterUploadRoutes(srv *khttp.Server, d *data.Data) {
	_ = ensureUploadDir()
	r := srv.Route("/")

	r.POST("/v1/user/upload", func(ctx khttp.Context) error {
		pd := auth.GetCurrentUser(ctx)
		if pd == nil || pd.UserID == 0 {
			return ctx.JSON(http.StatusUnauthorized, map[string]interface{}{
				"code": 1, "message": "请先登录",
			})
		}

		req := ctx.Request()
		// 表单尚未解析时 purpose 为空，用更大 limit 兜底再读 purpose
		if err := req.ParseMultipartForm(maxBlogUploadBytes); err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "解析表单失败或文件过大",
			})
		}
		purpose := strings.TrimSpace(req.FormValue("purpose"))
		switch purpose {
		case "avatar", "site", "bulletin", "misc", "blog", "blog_cover":
		default:
			purpose = "misc"
		}

		file, hdr, err := req.FormFile("file")
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "缺少 file 字段",
			})
		}
		defer file.Close()

		limit := int64(maxUploadBytes)
		if purpose == "blog" || purpose == "blog_cover" {
			limit = maxBlogUploadBytes
		}
		raw, err := io.ReadAll(io.LimitReader(file, limit+1))
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "读取文件失败",
			})
		}
		if int64(len(raw)) > limit {
			msg := "文件过大，最大 3MB"
			if purpose == "blog" || purpose == "blog_cover" {
				msg = fmt.Sprintf("文件过大，最大 %dMB", maxBlogUploadBytes>>20)
			}
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": msg,
			})
		}
		// DetectContentType 对 svg 只会给出 text/xml|text/plain，需单独识别
		ct := http.DetectContentType(raw)
		if looksLikeSVG(raw) {
			ct = "image/svg+xml"
		}
		if !allowedImage(ct) || !validImageData(raw, ct) {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "仅支持有效的 jpg/png/gif/webp/ico/svg 图片（svg 不得含脚本或事件处理器）",
			})
		}

		if purpose == "site" &&
			!auth.HasPerm(ctx, rbac.PermSiteConfigWrite) &&
			!auth.HasPerm(ctx, rbac.PermOrgInfoWrite) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要修改站点或组织信息权限",
			})
		}
		if purpose == "bulletin" && !auth.HasPerm(ctx, rbac.PermOrgBulletinManage) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要组织公告管理权限",
			})
		}

		// —— 博客/题解图：又拍云（需站点配置 + 用户授权）——
		if purpose == "blog" || purpose == "blog_cover" {
			return handleBlogUpyunUpload(ctx, d, pd.UserID, raw, ct, purpose, hdr.Filename)
		}
		// —— 头像：又拍云（需站点配置，无需博客授权）——
		if purpose == "avatar" {
			return handleAvatarUpyunUpload(ctx, d, pd.UserID, raw, ct, hdr.Filename)
		}
		// —— 站点/组织品牌图：又拍云（沿用站点图床配置）——
		if purpose == "site" {
			return handleBrandingUpyunUpload(ctx, d, pd.UserID, raw, ct, hdr.Filename)
		}

		ext := extFromContentType(ct, hdr.Filename)
		if ext == "" {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "无法识别图片格式",
			})
		}
		diskName := randomName() + ext
		relDir := filepath.Join(purpose, fmt.Sprintf("%d", pd.UserID))
		absDir := filepath.Join(UploadDir(), relDir)
		if err := os.MkdirAll(absDir, 0o755); err != nil {
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "创建目录失败",
			})
		}
		absPath := filepath.Join(absDir, diskName)
		if err := os.WriteFile(absPath, raw, 0o644); err != nil {
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "保存失败",
			})
		}

		urlPath := staticURLPrefix + "/" + filepath.ToSlash(filepath.Join(relDir, diskName))
		return ctx.JSON(http.StatusOK, map[string]interface{}{
			"code":    0,
			"message": "success",
			"url":     urlPath,
		})
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case strings.HasPrefix(path, staticURLPrefix+"/"):
			serveUploadFile(w, req, staticURLPrefix+"/")
		case strings.HasPrefix(path, staticRoutePrefix+"/"):
			serveUploadFile(w, req, staticRoutePrefix+"/")
		case path == staticURLPrefix || path == staticRoutePrefix ||
			path == staticURLPrefix+"/" || path == staticRoutePrefix+"/":
			http.NotFound(w, req)
		default:
			http.NotFound(w, req)
		}
	})
	srv.HandlePrefix(staticRoutePrefix+"/", handler)
	srv.HandlePrefix(staticURLPrefix+"/", handler)
}

// handleBrandingUpyunUpload PUTs site and organization logos directly to UpYun.
func handleBrandingUpyunUpload(
	ctx khttp.Context,
	d *data.Data,
	userID uint,
	raw []byte,
	ct string,
	filename string,
) error {
	if d == nil || d.DB == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"code": 1, "message": "上传服务暂不可用",
		})
	}
	client := loadUpyunFromDB(d.DB)
	if !client.Configured() || client.PublicBaseURL() == "" {
		return ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"code": 1, "message": "站点尚未配置图床，请联系管理员",
		})
	}

	compressed, err := blogimg.CompressForUpload(raw, ct)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": err.Error(),
		})
	}
	ext := compressed.Ext
	if ext == "" || ext == ".bin" {
		ext = extFromContentType(compressed.ContentType, filename)
	}
	if ext == "" {
		ext = ".png"
	}
	contentHash := blogimg.ContentHash(compressed.Data)
	objectKey := blogimg.BrandingObjectKeyForHash(userID, contentHash, ext)
	if objectKey == "" {
		objectKey = fmt.Sprintf("/branding/%d/%s%s", userID, randomName(), ext)
	}
	if err := client.Put(objectKey, compressed.Data, compressed.ContentType); err != nil {
		log.Errorf("upload/branding put: %v", err)
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "图床上传失败，请稍后重试",
		})
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "success",
		"url":     client.PublicURL(objectKey),
		"hash":    contentHash,
	})
}

// handleBlogUpyunUpload compresses (clarity-first) and PUTs to UpYun.
func handleBlogUpyunUpload(
	ctx khttp.Context,
	d *data.Data,
	userID uint,
	raw []byte,
	ct string,
	purpose string,
	filename string,
) error {
	if d == nil || d.DB == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"code": 1, "message": "上传服务暂不可用",
		})
	}
	// 拒绝 svg 上云（XSS 风险）；博客正文用栅格图
	if strings.Contains(strings.ToLower(ct), "svg") {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "博客图片暂不支持 SVG，请使用 jpg/png/gif/webp",
		})
	}

	client := loadUpyunFromDB(d.DB)
	if !client.Configured() || client.PublicBaseURL() == "" {
		return ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"code": 1, "message": "站点尚未配置图床，请联系管理员",
		})
	}
	var cfg model.BlogSiteConfig
	authorized := false
	if err := d.DB.Select("image_upload_enabled").Where("user_id = ?", userID).First(&cfg).Error; err == nil {
		authorized = cfg.ImageUploadEnabled
	}
	if !blogimg.CanUpload(true, authorized) {
		return ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"code": 1, "message": "尚未开通图片上传，请联系站点管理员在博客管理中授权",
		})
	}

	compressed, err := blogimg.CompressForUpload(raw, ct)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": err.Error(),
		})
	}
	ext := compressed.Ext
	if ext == "" || ext == ".bin" {
		ext = extFromContentType(compressed.ContentType, filename)
	}
	if ext == "" {
		ext = ".jpg"
	}
	// 内容寻址：同用户相同字节 → 同一 object key，GC 与插件按 hash 对齐。
	contentHash := blogimg.ContentHash(compressed.Data)
	objectKey := blogimg.ObjectKeyForHash(userID, contentHash, ext)
	if objectKey == "" {
		objectKey = fmt.Sprintf("/blog/%d/%s%s", userID, randomName(), ext)
	}
	publicURL := client.PublicURL(objectKey)
	// 资产表存 path-only，与正文 canonical 一致；读时/客户端仍用完整 publicURL。
	storedURL := blogimg.NormalizeObjectKey(objectKey)
	if storedURL == "" {
		storedURL = publicURL
	}
	assetPurpose := "content"
	if purpose == "blog_cover" {
		assetPurpose = "cover"
	}
	if err := putAndRegisterBlogImage(
		d.DB, client, userID, objectKey, compressed.Data, compressed.ContentType,
		storedURL, contentHash, assetPurpose,
	); err != nil {
		log.Errorf("upload/register blog image: %v", err)
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "图床上传失败，请稍后重试",
		})
	}
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "success",
		"url":     publicURL,
		"hash":    contentHash,
	})
}

// handleAvatarUpyunUpload compresses and PUTs a user avatar to UpYun.
// 与博客图同存储：内容寻址 /avatar/{uid}/{sha256}{ext}，实时读 site_configs 域名。
func handleAvatarUpyunUpload(
	ctx khttp.Context,
	d *data.Data,
	userID uint,
	raw []byte,
	ct string,
	filename string,
) error {
	if d == nil || d.DB == nil {
		return ctx.JSON(http.StatusServiceUnavailable, map[string]interface{}{
			"code": 1, "message": "上传服务暂不可用",
		})
	}
	// 与博客一致：svg 不上云（XSS 风险）
	if strings.Contains(strings.ToLower(ct), "svg") {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": "头像暂不支持 SVG，请使用 jpg/png/gif/webp",
		})
	}

	client := loadUpyunFromDB(d.DB)
	if !client.Configured() || client.PublicBaseURL() == "" {
		return ctx.JSON(http.StatusForbidden, map[string]interface{}{
			"code": 1, "message": "站点尚未配置图床，请联系管理员",
		})
	}

	compressed, err := blogimg.CompressForUpload(raw, ct)
	if err != nil {
		return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
			"code": 1, "message": err.Error(),
		})
	}
	ext := compressed.Ext
	if ext == "" || ext == ".bin" {
		ext = extFromContentType(compressed.ContentType, filename)
	}
	if ext == "" {
		ext = ".jpg"
	}
	// 内容寻址：同用户相同字节 → 同一 object key，换头像时旧 key 可精确删除
	contentHash := blogimg.ContentHash(compressed.Data)
	objectKey := blogimg.AvatarObjectKeyForHash(userID, contentHash, ext)
	if objectKey == "" {
		objectKey = fmt.Sprintf("/avatar/%d/%s%s", userID, randomName(), ext)
	}
	if err := client.Put(objectKey, compressed.Data, compressed.ContentType); err != nil {
		log.Errorf("upload/avatar put: %v", err)
		return ctx.JSON(http.StatusBadGateway, map[string]interface{}{
			"code": 1, "message": "图床上传失败，请稍后重试",
		})
	}
	publicURL := client.PublicURL(objectKey)
	return ctx.JSON(http.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "success",
		"url":     publicURL,
		"hash":    contentHash,
	})
}
