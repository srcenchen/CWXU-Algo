package service

import (
	"bytes"
	"crypto/rand"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
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

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

const (
	maxUploadBytes    = 3 << 20 // 3MB
	staticURLPrefix   = "/api/user/static"
	staticRoutePrefix = "/v1/user/static"
)

var imageExts = []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".ico"}

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
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".ico":
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
		"image/x-icon", "image/vnd.microsoft.icon":
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

// RegisterUploadRoutes 注册 multipart 上传与静态文件
func RegisterUploadRoutes(srv *khttp.Server) {
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
		if err := req.ParseMultipartForm(maxUploadBytes); err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "解析表单失败或文件过大(≤3MB)",
			})
		}
		file, hdr, err := req.FormFile("file")
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "缺少 file 字段",
			})
		}
		defer file.Close()

		data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "读取文件失败",
			})
		}
		if int64(len(data)) > maxUploadBytes {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "文件过大，最大 3MB",
			})
		}
		ct := http.DetectContentType(data)
		if !allowedImage(ct) || !validImageData(data, ct) {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "仅支持有效的 jpg/png/gif/webp/ico 图片",
			})
		}

		purpose := strings.TrimSpace(req.FormValue("purpose"))
		switch purpose {
		case "avatar", "site", "bulletin", "misc":
		default:
			purpose = "misc"
		}
		if purpose == "site" && !auth.HasPerm(ctx, rbac.PermSiteConfigWrite) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要修改站点配置权限",
			})
		}
		if purpose == "bulletin" && !auth.HasPerm(ctx, rbac.PermOrgBulletinManage) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要组织公告管理权限",
			})
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
		if err := os.WriteFile(absPath, data, 0o644); err != nil {
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
