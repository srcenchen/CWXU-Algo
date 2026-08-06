package service

import (
	"crypto/rand"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

const (
	maxPasteBytes   = 512 << 10 // 512KB
	maxPasteTitle   = 200
	pasteSlugLen    = 10
	pasteSlugAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// PasteService 文本/代码粘贴板
type PasteService struct {
	db *gorm.DB
}

func NewPasteService(d *data.Data) *PasteService {
	return &PasteService{db: d.DB}
}

// RegisterPasteRoutes 注册 paste 路由（与 org/upload 同模式）
func RegisterPasteRoutes(srv *khttp.Server, ps *PasteService) {
	r := srv.Route("/")
	r.POST("/v1/user/paste/create", ps.handleCreate)
	r.GET("/v1/user/paste/get", ps.handleGet)
	r.GET("/v1/user/paste/mine", ps.handleMine)
	r.POST("/v1/user/paste/delete", ps.handleDelete)
	r.GET("/v1/user/paste/admin-list", ps.handleAdminList)
}

type pasteCreateReq struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Language string `json:"language"`
	// expire: never | 1h | 1d | 1w | 1m | 1y
	Expire string `json:"expire"`
}

func (s *PasteService) handleCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req pasteCreateReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	content := strings.ReplaceAll(req.Content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "内容不能为空"})
		return nil
	}
	if len(content) > maxPasteBytes {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "内容过大，最大 512KB"})
		return nil
	}
	title := strings.TrimSpace(req.Title)
	if utf8.RuneCountInString(title) > maxPasteTitle {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "标题过长"})
		return nil
	}
	lang := normalizePasteLang(req.Language)
	expireAt, err := parsePasteExpire(req.Expire)
	if err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": err.Error()})
		return nil
	}

	var paste model.Paste
	for i := 0; i < 8; i++ {
		slug, genErr := randomPasteSlug(pasteSlugLen)
		if genErr != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "生成链接失败"})
			return nil
		}
		paste = model.Paste{
			Slug:     slug,
			Title:    title,
			Content:  content,
			Language: lang,
			UserID:   pd.UserID,
			ExpireAt: expireAt,
		}
		if err := s.db.Create(&paste).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
				continue
			}
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
			return nil
		}
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code":    0,
			"message": "success",
			"data":    pasteToMap(&paste, true),
		})
		return nil
	}
	writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "生成链接失败，请重试"})
	return nil
}

func (s *PasteService) handleGet(ctx khttp.Context) error {
	slug := strings.TrimSpace(ctx.Request().URL.Query().Get("slug"))
	if slug == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少 slug"})
		return nil
	}
	var p model.Paste
	if err := s.db.Where("slug = ?", slug).First(&p).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "内容不存在或已删除"})
		return nil
	}
	if p.ExpireAt != nil && p.ExpireAt.Before(time.Now()) {
		_ = s.db.Delete(&p).Error
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "内容已过期"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data":    pasteToMap(&p, true),
	})
	return nil
}

func (s *PasteService) handleMine(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	now := time.Now()
	// 顺手清掉已过期（硬删）；列表本身只查未过期，避免 Limit(50) 全是过期项时看起来「历史没了」
	_ = s.db.Where("user_id = ? AND expire_at IS NOT NULL AND expire_at < ?", pd.UserID, now).
		Delete(&model.Paste{}).Error

	var list []model.Paste
	if err := s.db.Where("user_id = ? AND (expire_at IS NULL OR expire_at >= ?)", pd.UserID, now).
		Order("id DESC").
		Limit(50).
		Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		out = append(out, pasteToMap(&list[i], false))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"list":    out,
	})
	return nil
}

func (s *PasteService) handleDelete(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || strings.TrimSpace(body.Slug) == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var p model.Paste
	if err := s.db.Where("slug = ?", strings.TrimSpace(body.Slug)).First(&p).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "内容不存在"})
		return nil
	}
	if p.UserID != pd.UserID && !pd.IsSiteAdmin {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "只能删除自己的内容"})
		return nil
	}
	if err := s.db.Delete(&p).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "删除失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

// pasteAdminRow 粘贴板审查：pastes 关联创建者昵称/用户名
type pasteAdminRow struct {
	model.Paste
	Username string `gorm:"column:username"`
	Name     string `gorm:"column:name"`
}

// handleAdminList 站管 / 内容治理：查看当前全部未过期粘贴内容（事后审查）。
// 过期内容由既有逻辑在读取时删除，这里只看「有效期内」的。
func (s *PasteService) handleAdminList(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !pd.IsSiteAdmin && !auth.HasPerm(ctx, rbac.PermContentCommunityMod) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "没有内容治理权限"})
		return nil
	}
	page := 1
	pageSize := 30
	if v, err := strconv.Atoi(ctx.Request().URL.Query().Get("page")); err == nil && v >= 1 {
		page = v
	}
	if v, err := strconv.Atoi(ctx.Request().URL.Query().Get("pageSize")); err == nil && v >= 1 && v <= 100 {
		pageSize = v
	}
	now := time.Now()

	var total int64
	if err := s.db.Model(&model.Paste{}).
		Where("expire_at IS NULL OR expire_at >= ?", now).
		Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}

	var rows []pasteAdminRow
	if err := s.db.Table("pastes AS p").
		Select("p.*, u.username, u.name").
		Joins("LEFT JOIN users u ON u.id = p.user_id").
		Where("p.expire_at IS NULL OR p.expire_at >= ?", now).
		Order("p.id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&rows).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		m := pasteToMap(&rows[i].Paste, true)
		m["username"] = rows[i].Username
		m["name"] = rows[i].Name
		out = append(out, m)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"list":    out,
		"total":   total,
		"page":    page,
		"pageSize": pageSize,
	})
	return nil
}

func pasteToMap(p *model.Paste, withContent bool) map[string]interface{} {
	m := map[string]interface{}{
		"id":        p.ID,
		"slug":      p.Slug,
		"title":     p.Title,
		"language":  p.Language,
		"userId":    p.UserID,
		"createdAt": p.CreatedAt.Unix(),
	}
	if p.ExpireAt != nil {
		m["expireAt"] = p.ExpireAt.Unix()
	} else {
		m["expireAt"] = nil
	}
	if withContent {
		m["content"] = p.Content
	}
	return m
}

func randomPasteSlug(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(pasteSlugAlphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = pasteSlugAlphabet[v.Int64()]
	}
	return string(b), nil
}

func parsePasteExpire(expire string) (*time.Time, error) {
	expire = strings.TrimSpace(strings.ToLower(expire))
	if expire == "" || expire == "never" {
		return nil, nil
	}
	now := time.Now()
	var d time.Duration
	switch expire {
	case "1h":
		d = time.Hour
	case "1d":
		d = 24 * time.Hour
	case "1w":
		d = 7 * 24 * time.Hour
	case "1m":
		d = 30 * 24 * time.Hour
	case "1y":
		d = 365 * 24 * time.Hour
	default:
		return nil, errPasteExpire
	}
	t := now.Add(d)
	return &t, nil
}

var errPasteExpire = &pasteExpireError{}

type pasteExpireError struct{}

func (e *pasteExpireError) Error() string {
	return "有效期无效（never|1h|1d|1w|1m|1y）"
}

func normalizePasteLang(lang string) string {
	lang = strings.TrimSpace(strings.ToLower(lang))
	if lang == "" {
		return "text"
	}
	// 允许常见别名
	switch lang {
	case "c++", "cpp", "cxx":
		return "cpp"
	case "c#", "cs", "csharp":
		return "csharp"
	case "js", "javascript":
		return "javascript"
	case "ts", "typescript":
		return "typescript"
	case "py", "python":
		return "python"
	case "golang", "go":
		return "go"
	case "sh", "bash", "shell":
		return "bash"
	case "yml":
		return "yaml"
	case "md", "markdown":
		return "markdown"
	case "plain", "plaintext", "txt":
		return "text"
	default:
		if len(lang) > 64 {
			return "text"
		}
		return lang
	}
}
