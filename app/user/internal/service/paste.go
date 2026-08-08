package service

import (
	"context"
	"crypto/rand"
	pb "cwxu-algo/api/user/v1/paste"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/wrapperspb"
	"gorm.io/gorm"
)

const (
	maxPasteBytes     = 512 << 10 // 512KB
	maxPasteTitle     = 200
	pasteSlugLen      = 10
	pasteSlugAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// PasteService 文本/代码粘贴板
// 实现 proto：api/user/v1/paste/paste.proto（PasteHTTPServer）。
type PasteService struct {
	db *gorm.DB
}

func NewPasteService(d *data.Data) *PasteService {
	return &PasteService{db: d.DB}
}

func (s *PasteService) Create(ctx context.Context, req *pb.CreateReq) (*pb.CreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.CreateRes{Code: 1, Message: "请先登录"}, nil
	}
	content := strings.ReplaceAll(req.Content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return &pb.CreateRes{Code: 1, Message: "内容不能为空"}, nil
	}
	if len(content) > maxPasteBytes {
		return &pb.CreateRes{Code: 1, Message: "内容过大，最大 512KB"}, nil
	}
	title := strings.TrimSpace(req.Title)
	if utf8.RuneCountInString(title) > maxPasteTitle {
		return &pb.CreateRes{Code: 1, Message: "标题过长"}, nil
	}
	lang := normalizePasteLang(req.Language)
	expireAt, err := parsePasteExpire(req.Expire)
	if err != nil {
		return &pb.CreateRes{Code: 1, Message: err.Error()}, nil
	}

	var paste model.Paste
	for i := 0; i < 8; i++ {
		slug, genErr := randomPasteSlug(pasteSlugLen)
		if genErr != nil {
			return &pb.CreateRes{Code: 1, Message: "生成链接失败"}, nil
		}
		paste = model.Paste{
			Slug:     slug,
			Title:    title,
			Content:  content,
			Language: lang,
			UserID:   pd.UserID,
			ExpireAt: expireAt,
		}
		if err := s.db.WithContext(ctx).Create(&paste).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
				continue
			}
			return &pb.CreateRes{Code: 1, Message: "保存失败"}, nil
		}
		return &pb.CreateRes{Code: 0, Message: "success", Data: pasteToInfo(&paste, true)}, nil
	}
	return &pb.CreateRes{Code: 1, Message: "生成链接失败，请重试"}, nil
}

func (s *PasteService) Get(ctx context.Context, req *pb.GetReq) (*pb.GetRes, error) {
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		return &pb.GetRes{Code: 1, Message: "缺少 slug"}, nil
	}
	var p model.Paste
	if err := s.db.WithContext(ctx).Where("slug = ?", slug).First(&p).Error; err != nil {
		return &pb.GetRes{Code: 1, Message: "内容不存在或已删除"}, nil
	}
	if p.ExpireAt != nil && p.ExpireAt.Before(time.Now()) {
		_ = s.db.WithContext(ctx).Delete(&p).Error
		return &pb.GetRes{Code: 1, Message: "内容已过期"}, nil
	}
	return &pb.GetRes{Code: 0, Message: "success", Data: pasteToInfo(&p, true)}, nil
}

func (s *PasteService) Mine(ctx context.Context, req *pb.MineReq) (*pb.MineRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.MineRes{Code: 1, Message: "请先登录"}, nil
	}
	now := time.Now()
	// 顺手清掉已过期（硬删）；列表本身只查未过期，避免 Limit(50) 全是过期项时看起来「历史没了」
	_ = s.db.WithContext(ctx).Where("user_id = ? AND expire_at IS NOT NULL AND expire_at < ?", pd.UserID, now).
		Delete(&model.Paste{}).Error

	var list []model.Paste
	if err := s.db.WithContext(ctx).Where("user_id = ? AND (expire_at IS NULL OR expire_at >= ?)", pd.UserID, now).
		Order("id DESC").
		Limit(50).
		Find(&list).Error; err != nil {
		return &pb.MineRes{Code: 1, Message: "加载失败"}, nil
	}
	out := make([]*pb.PasteInfo, 0, len(list))
	for i := range list {
		out = append(out, pasteToInfo(&list[i], false))
	}
	return &pb.MineRes{Code: 0, Message: "success", List: out}, nil
}

func (s *PasteService) Delete(ctx context.Context, req *pb.DeleteReq) (*pb.DeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.DeleteRes{Code: 1, Message: "请先登录"}, nil
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		return &pb.DeleteRes{Code: 1, Message: "参数错误"}, nil
	}
	var p model.Paste
	if err := s.db.WithContext(ctx).Where("slug = ?", slug).First(&p).Error; err != nil {
		return &pb.DeleteRes{Code: 1, Message: "内容不存在"}, nil
	}
	if p.UserID != pd.UserID && !pd.IsSiteAdmin {
		return &pb.DeleteRes{Code: 1, Message: "只能删除自己的内容"}, nil
	}
	if err := s.db.WithContext(ctx).Delete(&p).Error; err != nil {
		return &pb.DeleteRes{Code: 1, Message: "删除失败"}, nil
	}
	return &pb.DeleteRes{Code: 0, Message: "已删除"}, nil
}

// pasteAdminRow 粘贴板审查：pastes 关联创建者昵称/用户名
type pasteAdminRow struct {
	model.Paste
	Username string `gorm:"column:username"`
	Name     string `gorm:"column:name"`
}

// AdminList 站管 / 内容治理：查看当前全部未过期粘贴内容（事后审查）。
// 过期内容由既有逻辑在读取时删除，这里只看「有效期内」的。
func (s *PasteService) AdminList(ctx context.Context, req *pb.AdminListReq) (*pb.AdminListRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return &pb.AdminListRes{Code: 1, Message: "请先登录"}, nil
	}
	if !pd.IsSiteAdmin && !auth.HasPerm(ctx, rbac.PermContentCommunityMod) {
		return &pb.AdminListRes{Code: 1, Message: "没有内容治理权限"}, nil
	}
	page := 1
	pageSize := 30
	if req.Page >= 1 {
		page = int(req.Page)
	}
	if req.PageSize >= 1 && req.PageSize <= 100 {
		pageSize = int(req.PageSize)
	}
	now := time.Now()

	var total int64
	if err := s.db.WithContext(ctx).Model(&model.Paste{}).
		Where("expire_at IS NULL OR expire_at >= ?", now).
		Count(&total).Error; err != nil {
		return &pb.AdminListRes{Code: 1, Message: "加载失败"}, nil
	}

	var rows []pasteAdminRow
	if err := s.db.WithContext(ctx).Table("pastes AS p").
		Select("p.*, u.username, u.name").
		Joins("LEFT JOIN users u ON u.id = p.user_id").
		Where("p.expire_at IS NULL OR p.expire_at >= ?", now).
		Order("p.id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Scan(&rows).Error; err != nil {
		return &pb.AdminListRes{Code: 1, Message: "加载失败"}, nil
	}
	out := make([]*pb.AdminPasteInfo, 0, len(rows))
	for i := range rows {
		out = append(out, pasteToAdminInfo(&rows[i].Paste, rows[i].Username, rows[i].Name))
	}
	return &pb.AdminListRes{
		Code:     0,
		Message:  "success",
		List:     out,
		Total:    int32(total),
		Page:     int32(page),
		PageSize: int32(pageSize),
	}, nil
}

func pasteToInfo(p *model.Paste, withContent bool) *pb.PasteInfo {
	m := &pb.PasteInfo{
		Id:        int64(p.ID),
		Slug:      p.Slug,
		Title:     p.Title,
		Language:  p.Language,
		UserId:    int64(p.UserID),
		CreatedAt: p.CreatedAt.Unix(),
	}
	if p.ExpireAt != nil {
		m.ExpireAt = wrapperspb.Int64(p.ExpireAt.Unix())
	}
	if withContent {
		m.Content = p.Content
	}
	return m
}

func pasteToAdminInfo(p *model.Paste, username, name string) *pb.AdminPasteInfo {
	m := &pb.AdminPasteInfo{
		Id:        int64(p.ID),
		Slug:      p.Slug,
		Title:     p.Title,
		Language:  p.Language,
		UserId:    int64(p.UserID),
		CreatedAt: p.CreatedAt.Unix(),
		Content:   p.Content,
		Username:  username,
		Name:      name,
	}
	if p.ExpireAt != nil {
		m.ExpireAt = wrapperspb.Int64(p.ExpireAt.Unix())
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
