package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/blogsync"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/biz/blogaccess"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	maxBlogTitle   = 200
	maxBlogSummary = 500
	maxBlogContent = 512 << 10 // 512KB
	maxBlogCover   = 1024
	maxBlogSlug    = 96
	maxCommentLen   = 4000
	maxBlogCmtDepth = 3 // 顶层 depth=0，最多再嵌套 2 层回复
	blogUnlockTTL   = 12 * time.Hour
)

// BlogService personal blog articles, comments, likes, categories, theme flags.
type BlogService struct {
	db     *gorm.DB
	coreDB *gorm.DB // optional: algo_core for shared solution UV / likes
}

func NewBlogService(d *data.Data) *BlogService {
	return &BlogService{db: d.DB, coreDB: d.CoreDB}
}

// RegisterBlogRoutes registers blog HTTP routes.
func RegisterBlogRoutes(srv *khttp.Server, bs *BlogService) {
	r := srv.Route("/")
	// Public / optional-JWT reads
	r.GET("/v1/user/blog/by-username", bs.handleListByUsername)
	r.GET("/v1/user/blog/article/get", bs.handleGetArticle)
	r.POST("/v1/user/blog/article/unlock", bs.handleUnlock)
	r.GET("/v1/user/blog/recommend", bs.handleRecommend)
	r.GET("/v1/user/blog/plaza", bs.handlePlaza)
	r.GET("/v1/user/blog/authors", bs.handleAuthors)
	r.GET("/v1/user/blog/categories", bs.handleListCategoriesPublic)
	r.GET("/v1/user/blog/comment/list", bs.handleListComments)
	r.GET("/v1/user/blog/theme/status", bs.handleThemeStatus)

	// Owner / authenticated writes
	r.POST("/v1/user/blog/article/create", bs.handleCreate)
	r.POST("/v1/user/blog/article/update", bs.handleUpdate)
	r.POST("/v1/user/blog/article/delete", bs.handleDelete)
	r.GET("/v1/user/blog/article/mine", bs.handleMine)
	r.GET("/v1/user/blog/analytics", bs.handleAnalytics)

	r.POST("/v1/user/blog/category/create", bs.handleCategoryCreate)
	r.POST("/v1/user/blog/category/update", bs.handleCategoryUpdate)
	r.POST("/v1/user/blog/category/delete", bs.handleCategoryDelete)
	r.GET("/v1/user/blog/category/mine", bs.handleCategoryMine)

	r.POST("/v1/user/blog/comment/create", bs.handleCommentCreate)
	r.POST("/v1/user/blog/comment/delete", bs.handleCommentDelete)
	r.POST("/v1/user/blog/comment/like", bs.handleCommentLikeToggle)
	r.POST("/v1/user/blog/like", bs.handleLikeToggle)

	// Owner theme config (themeId + social links)
	r.POST("/v1/user/blog/theme/config", bs.handleThemeConfigSave)
	// Site-admin theme enable (legacy custom-theme capability)
	r.POST("/v1/user/blog/theme/enable", bs.handleThemeEnable)

	// 开通协议 / 激活 / 邮件通知偏好
	r.GET("/v1/user/blog/agreement", bs.handleAgreementGet)
	r.GET("/v1/user/blog/activation/status", bs.handleActivationStatus)
	r.POST("/v1/user/blog/activate", bs.handleActivate)
	r.POST("/v1/user/blog/notify-pref", bs.handleNotifyPref)

	// 图片上传能力（又拍云；需站管授权 + 站点配置）
	r.GET("/v1/user/blog/image-upload/status", bs.handleImageUploadStatus)
	r.POST("/v1/user/blog/admin/image-upload", bs.handleAdminImageUpload)

	// 站管：博客管理
	r.GET("/v1/user/blog/admin/overview", bs.handleAdminOverview)
	r.GET("/v1/user/blog/admin/authors", bs.handleAdminAuthors)
	r.GET("/v1/user/blog/admin/articles", bs.handleAdminArticles)
	r.POST("/v1/user/blog/admin/moderate", bs.handleAdminModerate)

	// 举报
	r.POST("/v1/user/blog/report", bs.handleReport)
	// 举报处理台（content.report.handle）
	r.GET("/v1/user/blog/report/list", bs.handleReportList)
	r.POST("/v1/user/blog/report/handle", bs.handleReportHandle)
}

// ---------- helpers ----------

func blogViewerID(ctx khttp.Context) uint {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return 0
	}
	return pd.UserID
}

func blogIsSiteAdmin(ctx khttp.Context) bool {
	pd := auth.GetCurrentUser(ctx)
	return pd != nil && pd.IsSiteAdmin
}

// blogIsContentModerator 具备博客审核权限（博客审核/举报处理）
func blogIsContentModerator(ctx khttp.Context) bool {
	return auth.HasPerm(ctx, rbac.PermContentBlogModerate)
}

func (s *BlogService) publicOrgID() uint {
	id, err := data.EnsurePublicOrgID(s.db)
	if err != nil {
		return 0
	}
	return id
}

func (s *BlogService) isSystemOrg(orgID uint) bool {
	var o model.Org
	if err := s.db.Select("id", "is_system").Where("id = ?", orgID).First(&o).Error; err != nil {
		return false
	}
	return o.IsSystem
}

func (s *BlogService) findUserByUsername(username string) (*model.User, error) {
	var u model.User
	err := s.db.Where("username = ?", strings.TrimSpace(username)).First(&u).Error
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func hashBlogPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func checkBlogPassword(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// unlock token: base64(articleID:expUnix:hmac)
func (s *BlogService) makeUnlockToken(articleID uint) string {
	exp := time.Now().Add(blogUnlockTTL).Unix()
	payload := fmt.Sprintf("%d:%d", articleID, exp)
	mac := hmac.New(sha256.New, blogUnlockKey())
	_, _ = mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + ":" + sig))
}

func (s *BlogService) verifyUnlockToken(token string, articleID uint) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 {
		return false
	}
	id, _ := strconv.ParseUint(parts[0], 10, 64)
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	if uint(id) != articleID || exp < time.Now().Unix() {
		return false
	}
	payload := parts[0] + ":" + parts[1]
	mac := hmac.New(sha256.New, blogUnlockKey())
	_, _ = mac.Write([]byte(payload))
	expect := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expect), []byte(parts[2]))
}

func blogUnlockKey() []byte {
	// derive from JWT secret so tokens invalidate when secret rotates
	h := sha256.Sum256([]byte("blog-unlock:" + _const.JWTSecret()))
	return h[:]
}

func randomBlogSlug(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		var rb [1]byte
		if _, err := rand.Read(rb[:]); err != nil {
			return "", err
		}
		b[i] = alphabet[int(rb[0])%len(alphabet)]
	}
	return string(b), nil
}

func slugifyTitle(title string) string {
	// keep alnum and hyphen; fallback random
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == ' ' || r == '_' || r == '-' {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) > 60 {
		runes := []rune(s)
		s = string(runes[:60])
		s = strings.Trim(s, "-")
	}
	return s
}

func (s *BlogService) loadOrgIDs(articleID uint) []uint {
	var rows []model.BlogArticleOrg
	_ = s.db.Where("article_id = ?", articleID).Find(&rows).Error
	out := make([]uint, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.OrgID)
	}
	return out
}

func (s *BlogService) replaceOrgSync(articleID uint, orgIDs []uint) error {
	pub := s.publicOrgID()
	expanded := blogaccess.ExpandSyncOrgIDs(orgIDs, pub, s.isSystemOrg)
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("article_id = ?", articleID).Delete(&model.BlogArticleOrg{}).Error; err != nil {
			return err
		}
		for _, oid := range expanded {
			if err := tx.Create(&model.BlogArticleOrg{ArticleID: articleID, OrgID: oid}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BlogService) likedBy(articleID, userID uint) bool {
	if userID == 0 {
		return false
	}
	var n int64
	s.db.Model(&model.BlogLike{}).Where("article_id = ? AND user_id = ?", articleID, userID).Count(&n)
	return n > 0
}

// blogListPrefetch 列表页预取：整页文章的已赞集合与组织 id 分组，消除逐篇 N+1 查询。
type blogListPrefetch struct {
	liked  map[uint]bool
	orgIDs map[uint][]uint
}

// prefetchArticleExtras 对整页文章 id 各做一次 IN 批量查询（liked / orgIds）。
func (s *BlogService) prefetchArticleExtras(list []model.BlogArticle, viewerID uint) *blogListPrefetch {
	pre := &blogListPrefetch{liked: map[uint]bool{}, orgIDs: map[uint][]uint{}}
	if len(list) == 0 {
		return pre
	}
	ids := make([]uint, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].ID)
	}
	if viewerID > 0 {
		var likes []model.BlogLike
		_ = s.db.Where("user_id = ? AND article_id IN ?", viewerID, ids).Find(&likes).Error
		for _, l := range likes {
			pre.liked[l.ArticleID] = true
		}
	}
	var rows []model.BlogArticleOrg
	_ = s.db.Where("article_id IN ?", ids).Find(&rows).Error
	for _, r := range rows {
		pre.orgIDs[r.ArticleID] = append(pre.orgIDs[r.ArticleID], r.OrgID)
	}
	return pre
}

// articleToMap 序列化文章；pre 非空时使用预取好的 liked/orgIds（列表页），
// 为 nil 时回落单篇查询（详情等单文章场景）。
func (s *BlogService) articleToMap(a *model.BlogArticle, author *model.User, d blogaccess.Decision, viewerID uint, includeBody bool, pre *blogListPrefetch) map[string]interface{} {
	liked := false
	var orgIDs []uint
	if pre != nil {
		liked = pre.liked[a.ID]
		orgIDs = pre.orgIDs[a.ID]
		if orgIDs == nil {
			orgIDs = []uint{}
		}
	} else {
		liked = s.likedBy(a.ID, viewerID)
		orgIDs = s.loadOrgIDs(a.ID)
	}
	m := map[string]interface{}{
		"id":                a.ID,
		"slug":              a.Slug,
		"title":             a.Title,
		"summary":           a.Summary,
		"coverUrl":          a.CoverURL,
		"visibility":        a.Visibility,
		"recommend":         a.Recommend,
		"syncToMainProfile": a.SyncToMainProfile,
		"categoryId":        a.CategoryID,
		"viewCount":         a.ViewCount,
		"likeCount":         a.LikeCount,
		"commentCount":      a.CommentCount,
		"liked":             liked,
		"requiresPassword":  d.RequiresPassword,
		"canSeeBody":        d.CanSeeBody,
		"moderationStatus":  normalizeModeration(a.ModerationStatus),
		"createdAt":         a.CreatedAt.Unix(),
		"updatedAt":         a.UpdatedAt.Unix(),
		"orgIds":            orgIDs,
	}
	if a.ModerationNote != "" && (viewerID == a.UserID || viewerID > 0) {
		// 作者可见备注；列表对非作者不强制
		if viewerID == a.UserID {
			m["moderationNote"] = a.ModerationNote
		}
	}
	if a.SourceSolutionID != nil && *a.SourceSolutionID > 0 {
		m["sourceSolutionId"] = *a.SourceSolutionID
	}
	if a.SourceProblemID != nil && *a.SourceProblemID > 0 {
		m["sourceProblemId"] = *a.SourceProblemID
	}
	// editor helper: whether stored summary is system default (do not backfill)
	if includeBody {
		m["summaryIsDefault"] = blogaccess.IsDefaultSummary(a.Summary, a.Content)
	}
	if a.PublishedAt != nil {
		m["publishedAt"] = a.PublishedAt.Unix()
	} else {
		m["publishedAt"] = a.CreatedAt.Unix()
	}
	if author != nil {
		m["author"] = map[string]interface{}{
			"id":       author.ID,
			"username": author.Username,
			"name":     author.Name,
			"avatar":   author.Avatar,
		}
		m["userId"] = author.ID
		m["username"] = author.Username
	} else {
		m["userId"] = a.UserID
	}
	if includeBody && d.CanSeeBody {
		m["content"] = a.Content
	} else {
		m["content"] = ""
	}
	// never leak password hash
	return m
}

func parsePage(q *http.Request) (page, pageSize int) {
	page, _ = strconv.Atoi(q.URL.Query().Get("page"))
	pageSize, _ = strconv.Atoi(q.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	return page, pageSize
}

// ---------- list by username ----------

func (s *BlogService) handleListByUsername(ctx khttp.Context) error {
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
	page, pageSize := parsePage(ctx.Request())
	categoryID, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("categoryId"), 10, 64)
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))

	q := s.db.Model(&model.BlogArticle{}).Where("user_id = ?", u.ID)
	// non-owner: only public + password (meta); never private；且须审核通过
	if viewer != u.ID {
		q = q.Where("visibility IN ?", []string{blogaccess.VisibilityPublic, blogaccess.VisibilityPassword}).
			Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)
	}
	if categoryID > 0 {
		q = q.Where("category_id = ?", categoryID)
	}
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	var list []model.BlogArticle
	if err := q.Order("COALESCE(published_at, created_at) DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	pre := s.prefetchArticleExtras(list, viewer)
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility:  list[i].Visibility,
			OwnerID:     list[i].UserID,
			HasPassword: list[i].PasswordHash != "",
		}, viewer, false)
		if !d.CanSeeMeta {
			continue
		}
		out = append(out, s.articleToMap(&list[i], u, d, viewer, false, pre))
	}

	// theme status for blog shell
	themeOn := s.themeEnabledFor(u.ID)
	siteCfg := s.loadSiteConfig(u.ID)
	activated := s.isBlogActivated(u.ID)
	isOwner := viewer == u.ID

	// 未开通：对访客不暴露文章列表（壳层前端据此提示「此用户未开通博客」）
	if !activated && !isOwner {
		out = []map[string]interface{}{}
		total = 0
	}

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"author": map[string]interface{}{
				"id":       u.ID,
				"username": u.Username,
				"name":     u.Name,
				"avatar":   u.Avatar,
			},
			"list":         out,
			"total":        total,
			"page":         page,
			"pageSize":     pageSize,
			"themeEnabled": themeOn,
			"themeId":      siteCfg.ThemeID,
			"subtitle":     siteCfg.Subtitle,
			"socialLinks":  siteCfg.SocialLinks,
			"isOwner":      isOwner,
			"activated":    activated,
		},
	})
	return nil
}

// ---------- get article ----------

func (s *BlogService) handleGetArticle(ctx khttp.Context) error {
	id, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("id"), 10, 64)
	username := strings.TrimSpace(ctx.Request().URL.Query().Get("username"))
	slug := strings.TrimSpace(ctx.Request().URL.Query().Get("slug"))
	password := ctx.Request().URL.Query().Get("password")
	unlock := ctx.Request().URL.Query().Get("unlockToken")

	var a model.BlogArticle
	var err error
	if id > 0 {
		err = s.db.First(&a, id).Error
	} else if username != "" && slug != "" {
		u, e := s.findUserByUsername(username)
		if e != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
			return nil
		}
		err = s.db.Where("user_id = ? AND slug = ?", u.ID, slug).First(&a).Error
	} else {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少 id 或 username+slug"})
		return nil
	}
	if err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}

	viewer := blogViewerID(ctx)
	passwordOK := false
	if unlock != "" && s.verifyUnlockToken(unlock, a.ID) {
		passwordOK = true
	} else if password != "" && checkBlogPassword(a.PasswordHash, password) {
		passwordOK = true
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, viewer, passwordOK)

	if !d.CanSeeMeta {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在或无权查看"})
		return nil
	}
	// 未通过审核：仅作者/站管可见
	if !moderationVisibleToPublic(a.ModerationStatus) && viewer != a.UserID && !blogIsSiteAdmin(ctx) {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在或无权查看"})
		return nil
	}

	// UV view when body is visible (solution-linked shares UV with 题解)
	if d.CanSeeBody {
		visitorKey := blogVisitorKey(ctx, viewer)
		if a.SourceSolutionID != nil && *a.SourceSolutionID > 0 {
			s.recordLinkedSolutionUV(*a.SourceSolutionID, a.ID, visitorKey)
			_ = s.db.Select("view_count", "like_count", "comment_count").First(&a, a.ID).Error
		} else if s.recordBlogArticleUV(a.ID, visitorKey) {
			a.ViewCount++
		} else {
			_ = s.db.Select("view_count").First(&a, a.ID).Error
		}
	}

	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error

	m := s.articleToMap(&a, &author, d, viewer, true, nil)
	if d.RequiresPassword {
		m["message"] = "需要密码才能阅读全文"
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data":    m,
	})
	return nil
}

func (s *BlogService) handleUnlock(ctx khttp.Context) error {
	var body struct {
		ID       uint   `json:"id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var a model.BlogArticle
	if err := s.db.First(&a, body.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	if blogaccess.NormalizeVisibility(a.Visibility) != blogaccess.VisibilityPassword {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "该文章无需密码"})
		return nil
	}
	if !checkBlogPassword(a.PasswordHash, body.Password) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "密码不正确"})
		return nil
	}
	token := s.makeUnlockToken(a.ID)
	viewer := blogViewerID(ctx)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: true,
	}, viewer, true)
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error
	visitorKey := blogVisitorKey(ctx, viewer)
	if s.recordBlogArticleUV(a.ID, visitorKey) {
		a.ViewCount++
	} else {
		_ = s.db.Select("view_count").First(&a, a.ID).Error
	}
	m := s.articleToMap(&a, &author, d, viewer, true, nil)
	m["unlockToken"] = token
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data":    m,
	})
	return nil
}

// ---------- CRUD ----------

type blogArticleWriteReq struct {
	ID         uint   `json:"id"`
	Title      string `json:"title"`
	Slug       string `json:"slug"`
	Summary    string `json:"summary"`
	Content    string `json:"content"`
	CoverURL   string `json:"coverUrl"`
	Visibility string `json:"visibility"`
	Password   string `json:"password"`
	// Recommend 作者端不可写：仅站管/资源审核员经 admin/moderate 设精选。
	// SyncToMainProfile / OrgIDs 对公开文仍自动同步资料与组织发现。
	Recommend         bool   `json:"recommend"`
	SyncToMainProfile bool   `json:"syncToMainProfile"`
	CategoryID        *uint  `json:"categoryId"`
	OrgIDs            []uint `json:"orgIds"`
	// ClearPassword when true removes password on update (if visibility changes away).
	ClearPassword bool `json:"clearPassword"`
}

func (s *BlogService) handleCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !s.requireActivated(ctx, pd.UserID) {
		return nil
	}
	var req blogArticleWriteReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	a, msg := s.buildArticleFromReq(pd.UserID, 0, &req, true)
	if msg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": msg})
		return nil
	}
	now := time.Now()
	a.PublishedAt = &now
	a.ModerationStatus = model.BlogModerationApproved
	if err := s.db.Create(a).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "短链已被占用，请换一个"})
			return nil
		}
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	_ = s.applyAutoOrgSurface(a.ID, a.UserID, a.Visibility)
	go s.gcUserBlogImages(a.UserID)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, true)
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data":    s.articleToMap(a, &author, d, pd.UserID, true, nil),
	})
	return nil
}

func (s *BlogService) handleUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var req blogArticleWriteReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var existing model.BlogArticle
	if err := s.db.First(&existing, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	if !blogaccess.CanManage(existing.UserID, pd.UserID, auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate)) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "只能管理自己的文章"})
		return nil
	}
	if existing.UserID == pd.UserID && !pd.IsSiteAdmin && !s.requireActivated(ctx, pd.UserID) {
		return nil
	}
	a, msg := s.buildArticleFromReq(existing.UserID, existing.ID, &req, false)
	if msg != "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": msg})
		return nil
	}
	// preserve counters & created
	a.ID = existing.ID
	a.CreatedAt = existing.CreatedAt
	a.ViewCount = existing.ViewCount
	a.LikeCount = existing.LikeCount
	a.CommentCount = existing.CommentCount
	a.PublishedAt = existing.PublishedAt
	a.ModerationStatus = existing.ModerationStatus
	a.ModerationNote = existing.ModerationNote
	a.ModeratedAt = existing.ModeratedAt
	a.ModeratedBy = existing.ModeratedBy
	if a.PasswordHash == "" && !req.ClearPassword && existing.PasswordHash != "" &&
		blogaccess.NormalizeVisibility(a.Visibility) == blogaccess.VisibilityPassword &&
		strings.TrimSpace(req.Password) == "" {
		a.PasswordHash = existing.PasswordHash
	}
	// preserve solution link
	a.SourceSolutionID = existing.SourceSolutionID
	a.SourceProblemID = existing.SourceProblemID
	// 精选仅审核员可改：作者更新时保留；非公开则强制取消
	if blogaccess.AutoSurface(a.Visibility) {
		a.Recommend = existing.Recommend
	} else {
		a.Recommend = false
	}
	if err := s.db.Save(a).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "短链已被占用，请换一个"})
			return nil
		}
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	_ = s.applyAutoOrgSurface(a.ID, a.UserID, a.Visibility)
	// 保存后清理未写入正文/头图的又拍云图
	go s.gcUserBlogImages(a.UserID)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, true)
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data":    s.articleToMap(a, &author, d, pd.UserID, true, nil),
	})
	return nil
}

func (s *BlogService) buildArticleFromReq(userID, existingID uint, req *blogArticleWriteReq, isCreate bool) (*model.BlogArticle, string) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, "标题不能为空"
	}
	if utf8.RuneCountInString(title) > maxBlogTitle {
		return nil, "标题过长"
	}
	content := strings.ReplaceAll(req.Content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return nil, "正文不能为空"
	}
	if len(content) > maxBlogContent {
		return nil, "正文过大，最大 512KB"
	}
	// 摘要：空 → 从正文生成默认简述；作者手填则保留
	summary := blogaccess.ResolveSummaryForSave(req.Summary, content)
	if utf8.RuneCountInString(summary) > maxBlogSummary {
		runes := []rune(summary)
		summary = string(runes[:maxBlogSummary])
	}
	cover := strings.TrimSpace(req.CoverURL)
	if len(cover) > maxBlogCover {
		return nil, "头图链接过长"
	}
	// no file upload — only http(s) links allowed when set
	if cover != "" && !strings.HasPrefix(cover, "http://") && !strings.HasPrefix(cover, "https://") {
		return nil, "头图请使用 http(s) 链接，暂不支持本地上传"
	}
	vis := blogaccess.NormalizeVisibility(req.Visibility)
	if !blogaccess.ValidVisibility(vis) {
		return nil, "可见性无效"
	}
	var pwHash string
	if vis == blogaccess.VisibilityPassword {
		pw := strings.TrimSpace(req.Password)
		if isCreate && pw == "" {
			return nil, "密码访问需要设置密码"
		}
		if pw != "" {
			h, err := hashBlogPassword(pw)
			if err != nil {
				return nil, "密码处理失败"
			}
			pwHash = h
		}
	} else if req.ClearPassword || isCreate {
		pwHash = ""
	}

	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugifyTitle(title)
	}
	slug = strings.ToLower(slug)
	// sanitize slug
	var sb strings.Builder
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sb.WriteRune(r)
		}
	}
	slug = strings.Trim(sb.String(), "-")
	if slug == "" {
		s2, err := randomBlogSlug(10)
		if err != nil {
			return nil, "生成短链失败"
		}
		slug = s2
	}
	if utf8.RuneCountInString(slug) > maxBlogSlug {
		return nil, "短链过长"
	}

	// unique slug per user (exclude self on update)
	var n int64
	q := s.db.Model(&model.BlogArticle{}).Where("user_id = ? AND slug = ?", userID, slug)
	if existingID > 0 {
		q = q.Where("id <> ?", existingID)
	}
	q.Count(&n)
	if n > 0 {
		// auto suffix on create
		if isCreate {
			for i := 0; i < 6; i++ {
				suf, _ := randomBlogSlug(4)
				cand := slug + "-" + suf
				var n2 int64
				s.db.Model(&model.BlogArticle{}).Where("user_id = ? AND slug = ?", userID, cand).Count(&n2)
				if n2 == 0 {
					slug = cand
					break
				}
			}
		} else {
			return nil, "短链已被占用，请换一个"
		}
	}

	if req.CategoryID != nil && *req.CategoryID > 0 {
		var cat model.BlogCategory
		if err := s.db.Where("id = ? AND user_id = ?", *req.CategoryID, userID).First(&cat).Error; err != nil {
			return nil, "分类不存在"
		}
	}

	// 公开文自动同步主站资料/组织发现；精选(recommend) 默认 false，由审核员设
	auto := blogaccess.AutoSurface(vis)
	return &model.BlogArticle{
		UserID:            userID,
		Slug:              slug,
		Title:             title,
		Summary:           summary,
		Content:           content,
		CoverURL:          cover,
		Visibility:        vis,
		PasswordHash:      pwHash,
		Recommend:         false,
		SyncToMainProfile: auto,
		CategoryID:        req.CategoryID,
	}, ""
}

// applyAutoOrgSurface syncs article to all orgs the author belongs to when public.
// Non-public clears org surfaces.
func (s *BlogService) applyAutoOrgSurface(articleID, userID uint, visibility string) error {
	if !blogaccess.AutoSurface(visibility) {
		return s.db.Where("article_id = ?", articleID).Delete(&model.BlogArticleOrg{}).Error
	}
	var orgIDs []uint
	_ = s.db.Model(&model.OrgMember{}).Where("user_id = ?", userID).Pluck("org_id", &orgIDs).Error
	if len(orgIDs) == 0 {
		// at least public domain
		if pub := s.publicOrgID(); pub > 0 {
			orgIDs = []uint{pub}
		}
	}
	return s.replaceOrgSync(articleID, orgIDs)
}

func blogVisitorKey(ctx khttp.Context, viewerID uint) string {
	if viewerID > 0 {
		return fmt.Sprintf("u:%d", viewerID)
	}
	// cookie / header visitor id
	if c, err := ctx.Request().Cookie("goalgo_vid"); err == nil && c != nil {
		v := strings.TrimSpace(c.Value)
		if v != "" && len(v) <= 64 {
			return "v:" + v
		}
	}
	if h := strings.TrimSpace(ctx.Request().Header.Get("X-Visitor-Id")); h != "" && len(h) <= 64 {
		return "v:" + h
	}
	// fallback: IP + UA hash (best-effort anonymous UV)
	ip := ctx.Request().Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = ctx.Request().RemoteAddr
	}
	ua := ctx.Request().UserAgent()
	sum := sha256.Sum256([]byte(ip + "|" + ua))
	return "a:" + hex.EncodeToString(sum[:8])
}

// recordBlogArticleUV returns true if this is a new unique view (counter incremented).
func (s *BlogService) recordBlogArticleUV(articleID uint, visitorKey string) bool {
	if articleID == 0 || visitorKey == "" {
		return false
	}
	row := model.BlogArticleViewUV{ArticleID: articleID, VisitorKey: visitorKey}
	if err := s.db.Create(&row).Error; err != nil {
		// unique conflict → already counted
		return false
	}
	_ = s.db.Model(&model.BlogArticle{}).Where("id = ?", articleID).
		UpdateColumn("view_count", gorm.Expr("view_count + 1")).Error
	return true
}

// recordLinkedSolutionUV shares UV with the main-site solution (core DB) and mirrors count.
func (s *BlogService) recordLinkedSolutionUV(solutionID, articleID uint, visitorKey string) {
	if solutionID == 0 || visitorKey == "" {
		return
	}
	// try core community_view_uvs
	if s.coreDB != nil {
		err := s.coreDB.Exec(
			`INSERT INTO community_view_uvs (created_at, target_type, target_id, visitor_key)
			 VALUES (NOW(), 'solution', ?, ?)
			 ON CONFLICT DO NOTHING`,
			solutionID, visitorKey,
		).Error
		// also try without ON CONFLICT for drivers that differ — unique fail is fine
		_ = err
		// increment if row exists for this visitor was just inserted: compare counts
		var n int64
		_ = s.coreDB.Table("community_view_uvs").
			Where("target_type = ? AND target_id = ? AND visitor_key = ?", "solution", solutionID, visitorKey).
			Count(&n).Error
		if n == 1 {
			// may be first insert this process; still bump once using a check on blog uv table
		}
		// Use blog UV table as secondary uniqueness for this article
		if s.recordBlogArticleUV(articleID, visitorKey) {
			_ = s.coreDB.Exec(
				`UPDATE problem_user_solutions SET view_count = view_count + 1 WHERE id = ?`,
				solutionID,
			).Error
			var vc int
			_ = s.coreDB.Table("problem_user_solutions").Select("view_count").Where("id = ?", solutionID).Scan(&vc).Error
			if vc > 0 {
				_ = s.db.Model(&model.BlogArticle{}).Where("id = ?", articleID).UpdateColumn("view_count", vc).Error
			}
		} else {
			// already counted: align blog counter to solution
			var vc int
			_ = s.coreDB.Table("problem_user_solutions").Select("view_count").Where("id = ?", solutionID).Scan(&vc).Error
			if vc >= 0 {
				_ = s.db.Model(&model.BlogArticle{}).Where("id = ?", articleID).UpdateColumn("view_count", vc).Error
			}
		}
		return
	}
	// no core DB: pure blog UV
	_ = s.recordBlogArticleUV(articleID, visitorKey)
}

func (s *BlogService) handleDelete(ctx khttp.Context) error {
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
	var a model.BlogArticle
	if err := s.db.First(&a, body.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	if !blogaccess.CanManage(a.UserID, pd.UserID, auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate)) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "只能删除自己的文章"})
		return nil
	}
	ownerID := a.UserID
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		_ = tx.Where("article_id = ?", a.ID).Delete(&model.BlogArticleOrg{}).Error
		// 先清评论点赞再删评论
		var cmtIDs []uint
		_ = tx.Model(&model.BlogComment{}).Where("article_id = ?", a.ID).Pluck("id", &cmtIDs).Error
		if len(cmtIDs) > 0 {
			_ = tx.Where("comment_id IN ?", cmtIDs).Delete(&model.BlogCommentLike{}).Error
		}
		_ = tx.Where("article_id = ?", a.ID).Delete(&model.BlogComment{}).Error
		_ = tx.Where("article_id = ?", a.ID).Delete(&model.BlogLike{}).Error
		_ = tx.Where("article_id = ?", a.ID).Delete(&model.BlogArticleViewUV{}).Error
		return tx.Delete(&a).Error
	})
	// 删文后清理不再被引用的又拍云图
	go s.gcUserBlogImages(ownerID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

func (s *BlogService) handleMine(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	page, pageSize := parsePage(ctx.Request())
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))
	q := s.db.Model(&model.BlogArticle{}).Where("user_id = ?", pd.UserID)
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}
	var total int64
	_ = q.Count(&total).Error
	var list []model.BlogArticle
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, pd.UserID).Error
	pre := s.prefetchArticleExtras(list, pd.UserID)
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility:  list[i].Visibility,
			OwnerID:     list[i].UserID,
			HasPassword: list[i].PasswordHash != "",
		}, pd.UserID, true)
		out = append(out, s.articleToMap(&list[i], &author, d, pd.UserID, false, pre))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"list":     out,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
	return nil
}

// ---------- recommend ----------

func (s *BlogService) handleRecommend(ctx khttp.Context) error {
	page, pageSize := parsePage(ctx.Request())
	viewer := blogViewerID(ctx)
	// 仅站管/审核员标记精选(recommend=true) 的公开已审文章
	q := s.db.Model(&model.BlogArticle{}).
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Where("recommend = ?", true).
		Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)

	// optional org filter: 公共域/缺省 → 全站公开文；私有域 → 仅该组织成员的文章
	// （作者所属各域均可见自己的公开文；私有域看不到非成员的公共域内容）
	orgID, _ := strconv.ParseUint(strings.TrimSpace(ctx.Request().URL.Query().Get("orgId")), 10, 64)
	if orgID > 0 {
		var o model.Org
		if s.db.Select("id", "is_system").First(&o, uint(orgID)).Error == nil && !o.IsSystem {
			q = q.Where(
				"user_id IN (SELECT user_id FROM org_members WHERE org_id = ?)",
				uint(orgID),
			)
		}
	}
	// exclude solution-mirrored articles when excludeSolutions=1 (discover dedupe)
	if strings.TrimSpace(ctx.Request().URL.Query().Get("excludeSolutions")) == "1" {
		q = q.Where("source_solution_id IS NULL OR source_solution_id = 0")
	}

	var total int64
	_ = q.Count(&total).Error
	var list []model.BlogArticle
	if err := q.Order("COALESCE(published_at, created_at) DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	out := s.batchMapArticles(list, viewer)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"list":     out,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
	return nil
}

// ---------- plaza (main-site public feed) ----------

func (s *BlogService) handlePlaza(ctx khttp.Context) error {
	page, pageSize := parsePage(ctx.Request())
	// Prefer denser default for plaza cards
	if ctx.Request().URL.Query().Get("pageSize") == "" {
		pageSize = 12
	}
	viewer := blogViewerID(ctx)
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))
	sort := strings.ToLower(strings.TrimSpace(ctx.Request().URL.Query().Get("sort")))
	if sort == "" {
		sort = "latest"
	}

	// 最新/热门：全部公开已审；精选：仅 recommend=true（站管/审核员标记）
	q := s.db.Model(&model.BlogArticle{}).
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}

	// optional org filter: 与 recommend 一致——公共域/缺省=全站；私有域=仅该组织成员作者
	orgID, _ := strconv.ParseUint(strings.TrimSpace(ctx.Request().URL.Query().Get("orgId")), 10, 64)
	if orgID > 0 {
		var o model.Org
		if s.db.Select("id", "is_system").First(&o, uint(orgID)).Error == nil && !o.IsSystem {
			q = q.Where(
				"user_id IN (SELECT user_id FROM org_members WHERE org_id = ?)",
				uint(orgID),
			)
		}
	}
	// 发现页去重：排除题解镜像文（题解走 activity/feed）
	if strings.TrimSpace(ctx.Request().URL.Query().Get("excludeSolutions")) == "1" {
		q = q.Where("source_solution_id IS NULL OR source_solution_id = 0")
	}

	switch sort {
	case "recommend":
		q = q.Where("recommend = ?", true)
		q = q.Order("COALESCE(published_at, created_at) DESC")
	case "hot":
		q = q.Order("view_count DESC, like_count DESC, COALESCE(published_at, created_at) DESC")
	case "latest":
		q = q.Order("COALESCE(published_at, created_at) DESC")
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code":    1,
			"message": "sort 须为 latest|hot|recommend",
		})
		return nil
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}

	var list []model.BlogArticle
	if err := q.Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}
	out := s.batchMapArticles(list, viewer)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"list":     out,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
	return nil
}

// ---------- active authors (plaza side rail) ----------

func (s *BlogService) handleAuthors(ctx khttp.Context) error {
	page, pageSize := parsePage(ctx.Request())
	if pageSize > 30 {
		pageSize = 30
	}
	if ctx.Request().URL.Query().Get("pageSize") == "" {
		pageSize = 12
	}
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))

	// Aggregate public articles per author, ordered by last publish time.
	type aggRow struct {
		UserID          uint
		ArticleCount    int64
		LastPublishedAt *time.Time
	}
	base := s.db.Model(&model.BlogArticle{}).
		Select("user_id, COUNT(*) as article_count, MAX(COALESCE(published_at, created_at)) as last_published_at").
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Group("user_id")

	// Optional name/username filter via join
	var total int64
	countQ := s.db.Table("(?) as author_agg", base)
	if keyword != "" {
		like := "%" + keyword + "%"
		countQ = s.db.Table("(?) as author_agg", base).
			Joins("JOIN users ON users.id = author_agg.user_id").
			Where("users.username ILIKE ? OR users.name ILIKE ?", like, like)
	}
	if err := countQ.Count(&total).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}

	var aggs []aggRow
	listQ := s.db.Table("(?) as author_agg", base).
		Select("author_agg.user_id, author_agg.article_count, author_agg.last_published_at")
	if keyword != "" {
		like := "%" + keyword + "%"
		listQ = listQ.Joins("JOIN users ON users.id = author_agg.user_id").
			Where("users.username ILIKE ? OR users.name ILIKE ?", like, like)
	}
	if err := listQ.Order("author_agg.last_published_at DESC NULLS LAST").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&aggs).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "加载失败"})
		return nil
	}

	ids := make([]uint, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.UserID)
	}
	authors := map[uint]model.User{}
	if len(ids) > 0 {
		var us []model.User
		_ = s.db.Select("id", "username", "name", "avatar").Where("id IN ?", ids).Find(&us).Error
		for _, u := range us {
			authors[u.ID] = u
		}
	}

	// latest public title per author (one query)
	latestTitle := map[uint]string{}
	if len(ids) > 0 {
		type titleRow struct {
			UserID uint
			Title  string
		}
		var titles []titleRow
		// DISTINCT ON is Postgres-specific; project uses postgres.
		_ = s.db.Raw(`
			SELECT DISTINCT ON (user_id) user_id, title
			FROM blog_articles
			WHERE visibility = ? AND user_id IN ?
			ORDER BY user_id, COALESCE(published_at, created_at) DESC
		`, blogaccess.VisibilityPublic, ids).Scan(&titles).Error
		for _, t := range titles {
			latestTitle[t.UserID] = t.Title
		}
	}

	out := make([]map[string]interface{}, 0, len(aggs))
	for _, a := range aggs {
		u := authors[a.UserID]
		item := map[string]interface{}{
			"id":            a.UserID,
			"username":      u.Username,
			"name":          u.Name,
			"avatar":        u.Avatar,
			"articleCount":  a.ArticleCount,
			"latestTitle":   latestTitle[a.UserID],
		}
		if a.LastPublishedAt != nil {
			item["lastPublishedAt"] = a.LastPublishedAt.Unix()
		}
		out = append(out, item)
	}

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"list":     out,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		},
	})
	return nil
}

// batchMapArticles loads authors once and maps list items without body.
func (s *BlogService) batchMapArticles(list []model.BlogArticle, viewer uint) []map[string]interface{} {
	ids := make([]uint, 0, len(list))
	seen := map[uint]struct{}{}
	for _, a := range list {
		if _, ok := seen[a.UserID]; !ok {
			seen[a.UserID] = struct{}{}
			ids = append(ids, a.UserID)
		}
	}
	authors := map[uint]model.User{}
	if len(ids) > 0 {
		var us []model.User
		_ = s.db.Select("id", "username", "name", "avatar").Where("id IN ?", ids).Find(&us).Error
		for _, u := range us {
			authors[u.ID] = u
		}
	}
	pre := s.prefetchArticleExtras(list, viewer)
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		u := authors[list[i].UserID]
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility: list[i].Visibility,
			OwnerID:    list[i].UserID,
		}, viewer, false)
		out = append(out, s.articleToMap(&list[i], &u, d, viewer, false, pre))
	}
	return out
}

// ---------- analytics ----------

func (s *BlogService) handleAnalytics(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	type row struct {
		Views    int64
		Likes    int64
		Comments int64
		Articles int64
	}
	var r row
	_ = s.db.Model(&model.BlogArticle{}).
		Select("COALESCE(SUM(view_count),0) as views, COALESCE(SUM(like_count),0) as likes, COALESCE(SUM(comment_count),0) as comments, COUNT(*) as articles").
		Where("user_id = ?", pd.UserID).
		Scan(&r).Error

	// top articles by views
	var top []model.BlogArticle
	_ = s.db.Where("user_id = ?", pd.UserID).Order("view_count DESC").Limit(10).Find(&top).Error
	topOut := make([]map[string]interface{}, 0, len(top))
	for _, a := range top {
		topOut = append(topOut, map[string]interface{}{
			"id":           a.ID,
			"slug":         a.Slug,
			"title":        a.Title,
			"viewCount":    a.ViewCount,
			"likeCount":    a.LikeCount,
			"commentCount": a.CommentCount,
			"visibility":   a.Visibility,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"totalArticles": r.Articles,
			"totalViews":    r.Views,
			"totalLikes":    r.Likes,
			"totalComments": r.Comments,
			"topArticles":   topOut,
		},
	})
	return nil
}

// ---------- categories ----------

func (s *BlogService) handleListCategoriesPublic(ctx khttp.Context) error {
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
	// 访客侧不强制创建默认分类（避免写路径）；仅列出已有
	var list []model.BlogCategory
	_ = s.db.Where("user_id = ?", u.ID).Order("is_default DESC, sort_order ASC, id ASC").Find(&list).Error
	counts := s.categoryArticleCounts(list, true)
	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		out = append(out, map[string]interface{}{
			"id": c.ID, "name": c.Name, "sortOrder": c.SortOrder, "articleCount": counts[c.ID], "isDefault": c.IsDefault,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": out})
	return nil
}

// categoryArticleCounts 分类计数一次 GROUP BY 聚合；publicOnly 时仅统计公开文章。
func (s *BlogService) categoryArticleCounts(list []model.BlogCategory, publicOnly bool) map[uint]int64 {
	out := map[uint]int64{}
	if len(list) == 0 {
		return out
	}
	ids := make([]uint, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	type catCount struct {
		CategoryID uint
		Cnt        int64
	}
	q := s.db.Model(&model.BlogArticle{}).
		Select("category_id, COUNT(*) AS cnt").
		Where("category_id IN ?", ids)
	if publicOnly {
		q = q.Where("visibility = ?", blogaccess.VisibilityPublic)
	}
	var rows []catCount
	_ = q.Group("category_id").Scan(&rows).Error
	for _, r := range rows {
		out[r.CategoryID] = r.Cnt
	}
	return out
}

func (s *BlogService) handleCategoryMine(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	// 管理页确保默认分类存在
	_, _ = blogsync.EnsureDefaultCategory(s.db, pd.UserID)
	var list []model.BlogCategory
	_ = s.db.Where("user_id = ?", pd.UserID).Order("is_default DESC, sort_order ASC, id ASC").Find(&list).Error
	counts := s.categoryArticleCounts(list, false)
	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		out = append(out, map[string]interface{}{
			"id": c.ID, "name": c.Name, "sortOrder": c.SortOrder, "articleCount": counts[c.ID], "isDefault": c.IsDefault,
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "list": out})
	return nil
}

func (s *BlogService) handleCategoryCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		Name      string `json:"name"`
		SortOrder int    `json:"sortOrder"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "分类名无效"})
		return nil
	}
	c := model.BlogCategory{UserID: pd.UserID, Name: name, SortOrder: body.SortOrder, IsDefault: false}
	if err := s.db.Create(&c).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "创建失败，可能重名"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{"id": c.ID, "name": c.Name, "sortOrder": c.SortOrder, "isDefault": c.IsDefault},
	})
	return nil
}

func (s *BlogService) handleCategoryUpdate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		ID        uint   `json:"id"`
		Name      string `json:"name"`
		SortOrder *int   `json:"sortOrder"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var c model.BlogCategory
	if err := s.db.Where("id = ? AND user_id = ?", body.ID, pd.UserID).First(&c).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分类不存在"})
		return nil
	}
	if n := strings.TrimSpace(body.Name); n != "" {
		c.Name = n
	}
	if body.SortOrder != nil {
		c.SortOrder = *body.SortOrder
	}
	if err := s.db.Save(&c).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success", "data": map[string]interface{}{
		"id": c.ID, "name": c.Name, "sortOrder": c.SortOrder, "isDefault": c.IsDefault,
	}})
	return nil
}

func (s *BlogService) handleCategoryDelete(ctx khttp.Context) error {
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
	var c model.BlogCategory
	if err := s.db.Where("id = ? AND user_id = ?", body.ID, pd.UserID).First(&c).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分类不存在"})
		return nil
	}
	if c.IsDefault {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "默认分类不能删除"})
		return nil
	}
	res := s.db.Where("id = ? AND user_id = ?", body.ID, pd.UserID).Delete(&model.BlogCategory{})
	if res.RowsAffected == 0 {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "分类不存在"})
		return nil
	}
	// 非默认分类文章改挂到默认分类
	if defID, err := blogsync.EnsureDefaultCategory(s.db, pd.UserID); err == nil && defID > 0 {
		_ = s.db.Model(&model.BlogArticle{}).Where("category_id = ?", body.ID).Update("category_id", defID).Error
	} else {
		_ = s.db.Model(&model.BlogArticle{}).Where("category_id = ?", body.ID).Update("category_id", nil).Error
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

// ---------- comments ----------

func (s *BlogService) handleListComments(ctx khttp.Context) error {
	articleID, _ := strconv.ParseUint(ctx.Request().URL.Query().Get("articleId"), 10, 64)
	if articleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少 articleId"})
		return nil
	}
	// ensure article is at least meta-visible
	var a model.BlogArticle
	if err := s.db.First(&a, articleID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	viewer := blogViewerID(ctx)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, viewer, false)
	// comments only when meta visible (public/password teaser/owner)
	if !d.CanSeeMeta {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	page, pageSize := parsePage(ctx.Request())

	// 仅分页顶层；子回复嵌套在 replies 中返回（与题解评论一致）。
	// 顶层用 SQL 分页（parent_id = 0 + LIMIT/OFFSET），再按层级批量拉取本页顶层的后代，
	// 不再整表加载全部评论。
	var total int64
	s.db.Model(&model.BlogComment{}).
		Where("article_id = ? AND parent_id = 0", articleID).Count(&total)

	var roots []model.BlogComment
	_ = s.db.Where("article_id = ? AND parent_id = 0", articleID).
		Order("id ASC").Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&roots).Error

	byID := map[uint]model.BlogComment{}
	children := map[uint][]uint{}
	rootIDs := make([]uint, 0, len(roots))
	frontier := make([]uint, 0, len(roots))
	for _, c := range roots {
		byID[c.ID] = c
		rootIDs = append(rootIDs, c.ID)
		frontier = append(frontier, c.ID)
	}
	// 逐层批量查后代；深度上限防脏数据成环导致死循环
	const maxBlogCmtTreeDepth = 64
	for depth := 0; depth < maxBlogCmtTreeDepth && len(frontier) > 0; depth++ {
		var level []model.BlogComment
		_ = s.db.Where("article_id = ? AND parent_id IN ?", articleID, frontier).
			Order("id ASC").Find(&level).Error
		next := make([]uint, 0, len(level))
		for _, c := range level {
			if _, ok := byID[c.ID]; ok {
				continue // 防环
			}
			byID[c.ID] = c
			children[c.ParentID] = append(children[c.ParentID], c.ID)
			next = append(next, c.ID)
		}
		frontier = next
	}

	// 收集本页用到的全部节点 id
	pageIDs := make([]uint, 0, len(byID))
	uids := map[uint]struct{}{}
	for id, c := range byID {
		pageIDs = append(pageIDs, id)
		uids[c.UserID] = struct{}{}
	}
	idList := make([]uint, 0, len(uids))
	for id := range uids {
		idList = append(idList, id)
	}
	users := map[uint]model.User{}
	if len(idList) > 0 {
		var us []model.User
		_ = s.db.Select("id", "username", "name", "avatar").Where("id IN ?", idList).Find(&us).Error
		for _, u := range us {
			users[u.ID] = u
		}
	}

	// 当前用户已赞集合
	likedSet := map[uint]bool{}
	if viewer > 0 && len(pageIDs) > 0 {
		var likes []model.BlogCommentLike
		_ = s.db.Where("user_id = ? AND comment_id IN ?", viewer, pageIDs).Find(&likes).Error
		for _, l := range likes {
			likedSet[l.CommentID] = true
		}
	}

	var buildNode func(id uint) map[string]interface{}
	buildNode = func(id uint) map[string]interface{} {
		c := byID[id]
		u := users[c.UserID]
		m := map[string]interface{}{
			"id": c.ID, "articleId": c.ArticleID, "parentId": c.ParentID,
			"content": c.Content, "createdAt": c.CreatedAt.Unix(),
			"userId": c.UserID, "likeCount": c.LikeCount, "liked": likedSet[c.ID],
			"author": map[string]interface{}{
				"id": u.ID, "username": u.Username, "name": u.Name, "avatar": u.Avatar,
			},
		}
		if c.ParentID > 0 {
			if p, ok := byID[c.ParentID]; ok {
				pu := users[p.UserID]
				m["replyToUserId"] = p.UserID
				m["replyToUsername"] = pu.Username
				m["replyToName"] = pu.Name
			}
		}
		reps := children[id]
		outReps := make([]map[string]interface{}, 0, len(reps))
		for _, rid := range reps {
			outReps = append(outReps, buildNode(rid))
		}
		m["replies"] = outReps
		return m
	}

	out := make([]map[string]interface{}, 0, len(rootIDs))
	for _, id := range rootIDs {
		out = append(out, buildNode(id))
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{"list": out, "total": total, "page": page, "pageSize": pageSize},
	})
	return nil
}

func (s *BlogService) handleCommentCreate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		ArticleID uint   `json:"articleId"`
		ParentID  uint   `json:"parentId"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ArticleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	content := strings.TrimSpace(body.Content)
	if content == "" || utf8.RuneCountInString(content) > maxCommentLen {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "评论内容无效"})
		return nil
	}
	var a model.BlogArticle
	if err := s.db.First(&a, body.ArticleID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	// only comment when body is readable without password OR viewer is owner
	// (password articles: must unlock first — we allow comment if public or owner)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeBody && blogaccess.NormalizeVisibility(a.Visibility) != blogaccess.VisibilityPassword {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无法评论此文章"})
		return nil
	}
	if blogaccess.NormalizeVisibility(a.Visibility) == blogaccess.VisibilityPrivate && pd.UserID != a.UserID {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无法评论此文章"})
		return nil
	}
	if body.ParentID > 0 {
		var parent model.BlogComment
		if err := s.db.Where("id = ? AND article_id = ?", body.ParentID, body.ArticleID).First(&parent).Error; err != nil {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "父评论不存在"})
			return nil
		}
		// 限制嵌套深度（与题解评论一致：最多 3 层）
		depth := 1
		pid := parent.ParentID
		for pid > 0 && depth < 16 {
			var p model.BlogComment
			if err := s.db.Select("id", "parent_id").First(&p, pid).Error; err != nil {
				break
			}
			depth++
			pid = p.ParentID
		}
		if depth >= maxBlogCmtDepth {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "回复层级已达上限"})
			return nil
		}
	}
	c := model.BlogComment{
		ArticleID: body.ArticleID,
		UserID:    pd.UserID,
		ParentID:  body.ParentID,
		Content:   content,
	}
	if err := s.db.Create(&c).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "发表失败"})
		return nil
	}
	_ = s.db.Model(&model.BlogArticle{}).Where("id = ?", a.ID).
		UpdateColumn("comment_count", gorm.Expr("comment_count + 1")).Error

	// 站内通知：文章作者 / 父评论作者
	actorName := pd.Name
	if actorName == "" {
		actorName = pd.Username
	}
	var authorU model.User
	_ = s.db.Select("id", "username").First(&authorU, a.UserID).Error
	payload := mustJSON(map[string]interface{}{
		"blogUsername": authorU.Username,
		"blogSlug":     a.Slug,
		"articleId":    a.ID,
		"articleTitle": a.Title,
		"commentId":    c.ID,
	})
	if body.ParentID > 0 {
		var parent model.BlogComment
		if s.db.First(&parent, body.ParentID).Error == nil && parent.UserID > 0 && parent.UserID != pd.UserID {
			_ = CreateNotification(s.db, model.Notification{
				UserID:  parent.UserID,
				Type:    model.NotifTypeBlogCommentReply,
				Title:   "有人回复了你的博客评论",
				Body:    actorName + " 回复了你在《" + a.Title + "》下的评论",
				ActorID: pd.UserID,
				RefType: "blog_comment",
				RefID:   c.ID,
				Payload: payload,
			})
		}
	} else if a.UserID > 0 && a.UserID != pd.UserID {
		_ = CreateNotification(s.db, model.Notification{
			UserID:  a.UserID,
			Type:    model.NotifTypeBlogComment,
			Title:   "有人评论了你的博客文章",
			Body:    actorName + " 评论了《" + a.Title + "》",
			ActorID: pd.UserID,
			RefType: "blog_article",
			RefID:   a.ID,
			Payload: payload,
		})
	}

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"id": c.ID, "articleId": c.ArticleID, "parentId": c.ParentID,
			"content": c.Content, "createdAt": c.CreatedAt.Unix(), "userId": c.UserID,
		},
	})
	return nil
}

func (s *BlogService) handleCommentDelete(ctx khttp.Context) error {
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
	var c model.BlogComment
	if err := s.db.First(&c, body.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "评论不存在"})
		return nil
	}
	var a model.BlogArticle
	_ = s.db.First(&a, c.ArticleID).Error
	if c.UserID != pd.UserID && a.UserID != pd.UserID && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "无权删除"})
		return nil
	}
	// 级联删除子树 + 点赞
	ids := s.collectBlogCommentSubtree(c.ID, c.ArticleID)
	if len(ids) == 0 {
		ids = []uint{c.ID}
	}
	_ = s.db.Where("comment_id IN ?", ids).Delete(&model.BlogCommentLike{}).Error
	if err := s.db.Where("id IN ?", ids).Delete(&model.BlogComment{}).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "删除失败"})
		return nil
	}
	n := len(ids)
	_ = s.db.Model(&model.BlogArticle{}).
		Where("id = ? AND comment_count >= ?", c.ArticleID, n).
		UpdateColumn("comment_count", gorm.Expr("comment_count - ?", n)).Error
	// 防止负数
	_ = s.db.Model(&model.BlogArticle{}).
		Where("id = ? AND comment_count < 0", c.ArticleID).
		UpdateColumn("comment_count", 0).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "已删除"})
	return nil
}

// collectBlogCommentSubtree returns id + all descendants under the same article.
func (s *BlogService) collectBlogCommentSubtree(rootID, articleID uint) []uint {
	var all []model.BlogComment
	_ = s.db.Select("id", "parent_id").Where("article_id = ?", articleID).Find(&all).Error
	children := map[uint][]uint{}
	for _, c := range all {
		if c.ParentID > 0 {
			children[c.ParentID] = append(children[c.ParentID], c.ID)
		}
	}
	out := make([]uint, 0)
	var walk func(id uint)
	walk = func(id uint) {
		out = append(out, id)
		for _, cid := range children[id] {
			walk(cid)
		}
	}
	walk(rootID)
	return out
}

// ---------- likes ----------

// handleCommentLikeToggle 博客评论点赞 toggle。
func (s *BlogService) handleCommentLikeToggle(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		CommentID uint `json:"commentId"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.CommentID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var c model.BlogComment
	if err := s.db.First(&c, body.CommentID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "评论不存在"})
		return nil
	}
	var a model.BlogArticle
	if err := s.db.First(&a, c.ArticleID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility: a.Visibility, OwnerID: a.UserID, HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeMeta {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "评论不存在"})
		return nil
	}

	var existing model.BlogCommentLike
	err := s.db.Where("comment_id = ? AND user_id = ?", body.CommentID, pd.UserID).First(&existing).Error
	liked := false
	if err == nil {
		_ = s.db.Delete(&existing).Error
		_ = s.db.Model(&model.BlogComment{}).Where("id = ? AND like_count > 0", body.CommentID).
			UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error
		liked = false
	} else {
		if err := s.db.Create(&model.BlogCommentLike{CommentID: body.CommentID, UserID: pd.UserID}).Error; err != nil {
			// 并发唯一冲突：视为已赞
			liked = true
		} else {
			_ = s.db.Model(&model.BlogComment{}).Where("id = ?", body.CommentID).
				UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error
			liked = true
			// 通知评论作者（取消不通知、不通知自己）
			if c.UserID > 0 && c.UserID != pd.UserID {
				actorName := pd.Name
				if actorName == "" {
					actorName = pd.Username
				}
				var authorU model.User
				_ = s.db.Select("username").First(&authorU, a.UserID).Error
				_ = CreateNotification(s.db, model.Notification{
					UserID:  c.UserID,
					Type:    model.NotifTypeBlogCommentLike,
					Title:   "有人赞了你的博客评论",
					Body:    actorName + " 赞了你在《" + a.Title + "》下的评论",
					ActorID: pd.UserID,
					RefType: "blog_comment",
					RefID:   c.ID,
					Payload: mustJSON(map[string]interface{}{
						"blogUsername": authorU.Username,
						"blogSlug":     a.Slug,
						"articleId":    a.ID,
						"articleTitle": a.Title,
						"commentId":    c.ID,
					}),
				})
			}
		}
	}
	var likeCount int
	_ = s.db.Model(&model.BlogComment{}).Select("like_count").Where("id = ?", body.CommentID).Scan(&likeCount).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{"liked": liked, "likeCount": likeCount, "commentId": body.CommentID},
	})
	return nil
}

func (s *BlogService) handleLikeToggle(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		ArticleID uint `json:"articleId"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ArticleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var a model.BlogArticle
	if err := s.db.First(&a, body.ArticleID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility: a.Visibility, OwnerID: a.UserID, HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeMeta {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	var existing model.BlogLike
	err := s.db.Where("article_id = ? AND user_id = ?", body.ArticleID, pd.UserID).First(&existing).Error
	liked := false
	if err == nil {
		_ = s.db.Delete(&existing).Error
		_ = s.db.Model(&model.BlogArticle{}).Where("id = ? AND like_count > 0", body.ArticleID).
			UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error
		liked = false
	} else {
		_ = s.db.Create(&model.BlogLike{ArticleID: body.ArticleID, UserID: pd.UserID}).Error
		_ = s.db.Model(&model.BlogArticle{}).Where("id = ?", body.ArticleID).
			UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error
		liked = true
		// 点赞同步主站通知（取消不通知）
		if a.UserID > 0 && a.UserID != pd.UserID {
			actorName := pd.Name
			if actorName == "" {
				actorName = pd.Username
			}
			var authorU model.User
			_ = s.db.Select("username").First(&authorU, a.UserID).Error
			_ = CreateNotification(s.db, model.Notification{
				UserID:  a.UserID,
				Type:    model.NotifTypeBlogArticleLike,
				Title:   "有人赞了你的博客文章",
				Body:    actorName + " 赞了《" + a.Title + "》",
				ActorID: pd.UserID,
				RefType: "blog_article",
				RefID:   a.ID,
				Payload: mustJSON(map[string]interface{}{
					"blogUsername": authorU.Username,
					"blogSlug":     a.Slug,
					"articleId":    a.ID,
					"articleTitle": a.Title,
				}),
			})
		}
	}
	var likeCount int
	_ = s.db.Model(&model.BlogArticle{}).Select("like_count").Where("id = ?", body.ArticleID).Scan(&likeCount).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{"liked": liked, "likeCount": likeCount},
	})
	return nil
}

// ---------- report ----------

func (s *BlogService) handleReport(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		ArticleID uint   `json:"articleId"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ArticleID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	reason := strings.TrimSpace(strings.ReplaceAll(body.Reason, "\r\n", "\n"))
	if reason == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请填写举报原因"})
		return nil
	}
	if utf8.RuneCountInString(reason) > 500 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "举报原因过长"})
		return nil
	}
	var a model.BlogArticle
	if err := s.db.First(&a, body.ArticleID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}
	if a.UserID == pd.UserID {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不能举报自己的文章"})
		return nil
	}
	var existing model.BlogReport
	if s.db.Where("user_id = ? AND article_id = ?", pd.UserID, body.ArticleID).First(&existing).Error == nil {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "你已举报过该文章，我们会尽快处理",
			"data": map[string]interface{}{"id": existing.ID, "alreadyReported": true},
		})
		return nil
	}
	row := model.BlogReport{
		UserID:    pd.UserID,
		ArticleID: body.ArticleID,
		Reason:    reason,
		Status:    "pending",
	}
	if err := s.db.Create(&row).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "提交失败，请稍后重试"})
		return nil
	}
	s.notifyAdminsBlogReport(pd, &a, reason, row.ID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已收到举报，我们会尽快处理",
		"data": map[string]interface{}{"id": row.ID, "alreadyReported": false},
	})
	return nil
}

// handleReportList 举报处理台：博客文章举报列表（需 content.report.handle）。
// query: status=pending|resolved|dismissed|all（默认 pending）、page/pageSize
func (s *BlogService) handleReportList(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermContentReportHandle) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要举报处理权限"})
		return nil
	}
	status := strings.TrimSpace(ctx.Request().URL.Query().Get("status"))
	if status == "" {
		status = "pending"
	}
	if status != "all" && status != "pending" && status != "resolved" && status != "dismissed" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不支持的状态筛选"})
		return nil
	}
	page, pageSize := parsePage(ctx.Request())
	q := s.db.Model(&model.BlogReport{})
	if status != "all" {
		q = q.Where("status = ?", status)
	}
	var total int64
	_ = q.Count(&total).Error
	var rows []model.BlogReport
	_ = q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error

	// 文章可能已被删除（exists=false）；作者与举报人一次批量取
	artIDs := make([]uint, 0, len(rows))
	for _, r := range rows {
		artIDs = append(artIDs, r.ArticleID)
	}
	arts := map[uint]model.BlogArticle{}
	if len(artIDs) > 0 {
		var list []model.BlogArticle
		_ = s.db.Select("id", "slug", "title", "user_id").Where("id IN ?", artIDs).Find(&list).Error
		for _, a := range list {
			arts[a.ID] = a
		}
	}
	uidSet := map[uint]struct{}{}
	uids := make([]uint, 0, len(rows)*2)
	addUID := func(id uint) {
		if id == 0 {
			return
		}
		if _, ok := uidSet[id]; ok {
			return
		}
		uidSet[id] = struct{}{}
		uids = append(uids, id)
	}
	for _, r := range rows {
		addUID(r.UserID)
		addUID(arts[r.ArticleID].UserID)
	}
	users := map[uint]model.User{}
	if len(uids) > 0 {
		var list []model.User
		_ = s.db.Select("id", "username", "name").Where("id IN ?", uids).Find(&list).Error
		for _, u := range list {
			users[u.ID] = u
		}
	}

	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		item := map[string]interface{}{
			"id":        r.ID,
			"createdAt": r.CreatedAt.Unix(),
			"status":    r.Status,
			"reason":    r.Reason,
			"articleId": r.ArticleID,
			"reporter": map[string]interface{}{
				"userId":   r.UserID,
				"username": users[r.UserID].Username,
			},
		}
		target := map[string]interface{}{"exists": false}
		if a, ok := arts[r.ArticleID]; ok {
			target = map[string]interface{}{
				"exists":         true,
				"slug":           a.Slug,
				"title":          a.Title,
				"authorUserId":   a.UserID,
				"authorUsername": users[a.UserID].Username,
			}
		}
		item["target"] = target
		out = append(out, item)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"list": out, "total": total, "page": page, "pageSize": pageSize,
		},
	})
	return nil
}

// handleReportHandle 处理博客举报：resolve=已处理 / dismiss=驳回（需 content.report.handle）
func (s *BlogService) handleReportHandle(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermContentReportHandle) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要举报处理权限"})
		return nil
	}
	var body struct {
		ID     uint   `json:"id"`
		Action string `json:"action"` // resolve|dismiss
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var next string
	switch body.Action {
	case "resolve":
		next = "resolved"
	case "dismiss":
		next = "dismissed"
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "不支持的操作"})
		return nil
	}
	var row model.BlogReport
	if s.db.First(&row, body.ID).Error != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "举报不存在"})
		return nil
	}
	if err := s.db.Model(&row).Update("status", next).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "操作失败，请稍后重试"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{"id": row.ID, "status": next},
	})
	return nil
}

// notifyAdminsBlogReport 站内通知全部站管 + 可配置收件人邮件
func (s *BlogService) notifyAdminsBlogReport(pd *auth.JwtPayload, a *model.BlogArticle, reason string, reportID uint) {
	if a == nil || pd == nil {
		return
	}
	actorName := pd.Name
	if actorName == "" {
		actorName = pd.Username
	}
	var author model.User
	_ = s.db.Select("id", "username", "name").First(&author, a.UserID).Error
	title := "博客文章举报"
	bodyText := fmt.Sprintf("%s 举报了文章《%s》（作者 @%s）：%s",
		actorName, a.Title, author.Username, reason)
	payload := mustJSON(map[string]interface{}{
		"articleId":      a.ID,
		"slug":           a.Slug,
		"blogSlug":       a.Slug,
		"blogUsername":   author.Username,
		"authorUsername": author.Username,
		"reportId":       reportID,
		"reason":         reason,
	})
	inner := fmt.Sprintf(`
<p style="margin:0 0 12px;">收到一篇博客文章举报，请尽快处理。</p>
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:14px;">
<tr><td style="padding:6px 12px 6px 0;color:#737373;width:88px;">举报人</td><td style="padding:6px 0;">%s</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">文章</td><td style="padding:6px 0;">《%s》</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">作者</td><td style="padding:6px 0;">@%s</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">文章 ID</td><td style="padding:6px 0;">%d</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;">Slug</td><td style="padding:6px 0;">%s</td></tr>
<tr><td style="padding:6px 12px 6px 0;color:#737373;vertical-align:top;">原因</td><td style="padding:6px 0;">%s</td></tr>
</table>
<p style="margin:14px 0 0;font-size:13px;color:#737373;">请登录站点管理端查看举报并处理。</p>
`, mail.Escape(actorName), mail.Escape(a.Title), mail.Escape(author.Username), a.ID, mail.Escape(a.Slug), mail.Escape(reason))
	html := mail.Wrap(mail.LayoutOpts{Brand: mail.DefaultBrand, Title: "博客文章举报", Preheader: bodyText}, inner)
	notify.NotifySiteAdminsWithEmail(s.db, notify.AdminNotif{
		Type:       notify.TypeBlogReport,
		Title:      title,
		Body:       bodyText,
		ActorID:    pd.UserID,
		RefType:    "blog_article",
		RefID:      a.ID,
		Payload:    payload,
		SkipUserID: pd.UserID,
	}, title, html)
}

// ---------- theme ----------

const (
	blogThemeChirpy = "chirpy"
	blogThemeSimple = "simple"
	blogThemeMizuki = "mizuki"
	maxSocialLinks  = 12
	maxSocialURL    = 512
	maxSubtitle     = 200
)

type blogSocialLink struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

type blogSiteConfigView struct {
	ThemeID     string           `json:"themeId"`
	Subtitle    string           `json:"subtitle"`
	SocialLinks []blogSocialLink `json:"socialLinks"`
}

func normalizeThemeID(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case blogThemeChirpy:
		return blogThemeChirpy
	case blogThemeSimple:
		return blogThemeSimple
	case blogThemeMizuki:
		return blogThemeMizuki
	default:
		return blogThemeMizuki
	}
}

func parseSocialLinksJSON(raw string) []blogSocialLink {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []blogSocialLink{}
	}
	var list []blogSocialLink
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return []blogSocialLink{}
	}
	out := make([]blogSocialLink, 0, len(list))
	for _, l := range list {
		t := strings.ToLower(strings.TrimSpace(l.Type))
		u := strings.TrimSpace(l.URL)
		if t == "" || u == "" {
			continue
		}
		if len(u) > maxSocialURL {
			u = u[:maxSocialURL]
		}
		label := strings.TrimSpace(l.Label)
		if utf8.RuneCountInString(label) > 32 {
			label = string([]rune(label)[:32])
		}
		out = append(out, blogSocialLink{Type: t, URL: u, Label: label})
		if len(out) >= maxSocialLinks {
			break
		}
	}
	return out
}

func (s *BlogService) loadSiteConfig(userID uint) blogSiteConfigView {
	view := blogSiteConfigView{
		ThemeID:     blogThemeMizuki,
		Subtitle:    "",
		SocialLinks: []blogSocialLink{},
	}
	if userID == 0 {
		return view
	}
	var cfg model.BlogSiteConfig
	if err := s.db.Where("user_id = ?", userID).First(&cfg).Error; err != nil {
		return view
	}
	view.ThemeID = normalizeThemeID(cfg.ThemeID)
	view.Subtitle = strings.TrimSpace(cfg.Subtitle)
	view.SocialLinks = parseSocialLinksJSON(cfg.SocialLinks)
	return view
}

func (s *BlogService) themeEnabledFor(userID uint) bool {
	var global model.BlogThemeFlag
	globalAll := false
	if err := s.db.Where("user_id = 0").First(&global).Error; err == nil {
		globalAll = global.Enabled
	}
	var per model.BlogThemeFlag
	if err := s.db.Where("user_id = ?", userID).First(&per).Error; err == nil {
		v := per.Enabled
		return blogaccess.ThemeEnabled(globalAll, &v)
	}
	return blogaccess.ThemeEnabled(globalAll, nil)
}

func (s *BlogService) handleThemeStatus(ctx khttp.Context) error {
	username := strings.TrimSpace(ctx.Request().URL.Query().Get("username"))
	var userID uint
	if username != "" {
		u, err := s.findUserByUsername(username)
		if err != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "用户不存在"})
			return nil
		}
		userID = u.ID
	} else {
		userID = blogViewerID(ctx)
	}
	enabled := false
	if userID > 0 {
		enabled = s.themeEnabledFor(userID)
	}
	siteCfg := s.loadSiteConfig(userID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"enabled":     enabled,
			"themeId":     siteCfg.ThemeID,
			"subtitle":    siteCfg.Subtitle,
			"socialLinks": siteCfg.SocialLinks,
			"customTheme": nil, // reserved extension point
		},
	})
	return nil
}

// handleThemeConfigSave owner saves theme id + subtitle + social links.
func (s *BlogService) handleThemeConfigSave(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !s.requireActivated(ctx, pd.UserID) {
		return nil
	}
	var body struct {
		ThemeID     string           `json:"themeId"`
		Subtitle    string           `json:"subtitle"`
		SocialLinks []blogSocialLink `json:"socialLinks"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	themeID := normalizeThemeID(body.ThemeID)
	subtitle := strings.TrimSpace(body.Subtitle)
	if utf8.RuneCountInString(subtitle) > maxSubtitle {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "副标题过长"})
		return nil
	}
	// re-validate social links via JSON round-trip
	rawLinks, _ := json.Marshal(body.SocialLinks)
	links := parseSocialLinksJSON(string(rawLinks))
	// only allow http(s) / mailto for urls
	clean := make([]blogSocialLink, 0, len(links))
	for _, l := range links {
		u := l.URL
		lu := strings.ToLower(u)
		if strings.HasPrefix(lu, "javascript:") || strings.HasPrefix(lu, "data:") {
			continue
		}
		if l.Type == "email" {
			if !strings.HasPrefix(lu, "mailto:") && !strings.Contains(u, "@") {
				continue
			}
			if !strings.HasPrefix(lu, "mailto:") {
				u = "mailto:" + u
			}
		} else if !strings.HasPrefix(lu, "http://") && !strings.HasPrefix(lu, "https://") {
			continue
		}
		clean = append(clean, blogSocialLink{Type: l.Type, URL: u, Label: l.Label})
	}
	linksJSON, _ := json.Marshal(clean)

	var cfg model.BlogSiteConfig
	err := s.db.Where("user_id = ?", pd.UserID).First(&cfg).Error
	if err != nil {
		cfg = model.BlogSiteConfig{
			UserID:      pd.UserID,
			ThemeID:     themeID,
			Subtitle:    subtitle,
			SocialLinks: string(linksJSON),
		}
		if err := s.db.Create(&cfg).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
			return nil
		}
	} else {
		cfg.ThemeID = themeID
		cfg.Subtitle = subtitle
		cfg.SocialLinks = string(linksJSON)
		if err := s.db.Save(&cfg).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
			return nil
		}
	}
	view := s.loadSiteConfig(pd.UserID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"themeId":     view.ThemeID,
			"subtitle":    view.Subtitle,
			"socialLinks": view.SocialLinks,
		},
	})
	return nil
}

func (s *BlogService) handleThemeEnable(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !pd.IsSiteAdmin {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "仅站点管理员可操作"})
		return nil
	}
	var body struct {
		// Mode: user | batch | all
		Mode    string `json:"mode"`
		UserID  uint   `json:"userId"`
		UserIDs []uint `json:"userIds"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	switch mode {
	case "all":
		var g model.BlogThemeFlag
		err := s.db.Where("user_id = 0").First(&g).Error
		if err != nil {
			g = model.BlogThemeFlag{UserID: 0, Enabled: body.Enabled}
			_ = s.db.Create(&g).Error
		} else {
			g.Enabled = body.Enabled
			_ = s.db.Save(&g).Error
		}
	case "user":
		if body.UserID == 0 {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "缺少 userId"})
			return nil
		}
		s.upsertThemeFlag(body.UserID, body.Enabled)
	case "batch":
		for _, id := range body.UserIDs {
			if id > 0 {
				s.upsertThemeFlag(id, body.Enabled)
			}
		}
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "mode 须为 user|batch|all"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"code": 0, "message": "success"})
	return nil
}

func (s *BlogService) upsertThemeFlag(userID uint, enabled bool) {
	var f model.BlogThemeFlag
	err := s.db.Where("user_id = ?", userID).First(&f).Error
	if err != nil {
		_ = s.db.Create(&model.BlogThemeFlag{UserID: userID, Enabled: enabled}).Error
		return
	}
	f.Enabled = enabled
	_ = s.db.Save(&f).Error
}
