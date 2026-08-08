package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"cwxu-algo/app/common/utils/sqllike"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pb "cwxu-algo/api/user/v1/blog"
	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/blogsync"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/biz/blogaccess"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	maxBlogTitle    = 200
	maxBlogSummary  = 500
	maxBlogContent  = 512 << 10 // 512KB
	maxBlogCover    = 1024
	maxBlogSlug     = 96
	maxCommentLen   = 4000
	maxBlogCmtDepth = 3 // 顶层 depth=0，最多再嵌套 2 层回复
	blogUnlockTTL   = 12 * time.Hour
	maxBlogTags     = 20
	maxBlogTagLen   = 32
	maxSlotMD       = 64 << 10 // 64KB per slot page
)

// BlogService personal blog articles, comments, likes, categories, theme flags.
type BlogService struct {
	db     *gorm.DB
	coreDB *gorm.DB // optional: algo_core for shared solution UV / likes
}

func NewBlogService(d *data.Data) *BlogService {
	return &BlogService{db: d.DB, coreDB: d.CoreDB}
}



// ---------- helpers ----------

func blogViewerID(ctx context.Context) uint {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return 0
	}
	return pd.UserID
}

func blogIsSiteAdmin(ctx context.Context) bool {
	pd := auth.GetCurrentUser(ctx)
	return pd != nil && pd.IsSiteAdmin
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

// blogListPrefetch 列表页预取：整页文章的已赞集合、组织 id、tags，消除逐篇 N+1 查询。
type blogListPrefetch struct {
	liked  map[uint]bool
	orgIDs map[uint][]uint
	tags   map[uint][]string
}

// prefetchArticleExtras 对整页文章 id 各做一次 IN 批量查询（liked / orgIds / tags）。
func (s *BlogService) prefetchArticleExtras(list []model.BlogArticle, viewerID uint) *blogListPrefetch {
	pre := &blogListPrefetch{liked: map[uint]bool{}, orgIDs: map[uint][]uint{}, tags: map[uint][]string{}}
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
	type tagRow struct {
		ArticleID uint
		Name      string
	}
	var trows []tagRow
	_ = s.db.Table("blog_article_tags AS bat").
		Select("bat.article_id, bt.name").
		Joins("JOIN blog_tags bt ON bt.id = bat.tag_id").
		Where("bat.article_id IN ?", ids).
		Order("bt.name ASC").
		Scan(&trows).Error
	for _, r := range trows {
		pre.tags[r.ArticleID] = append(pre.tags[r.ArticleID], r.Name)
	}
	return pre
}

// articleToProto 序列化文章；pre 非空时使用预取好的 liked/orgIds/tags（列表页），
// 为 nil 时回落单篇查询（详情等单文章场景）。
func (s *BlogService) articleToProto(a *model.BlogArticle, author *model.User, d blogaccess.Decision, viewerID uint, includeBody bool, pre *blogListPrefetch) *pb.ArticleInfo {
	liked := false
	var orgIDs []uint
	var tags []string
	if pre != nil {
		liked = pre.liked[a.ID]
		orgIDs = pre.orgIDs[a.ID]
		if orgIDs == nil {
			orgIDs = []uint{}
		}
		tags = pre.tags[a.ID]
		if tags == nil {
			tags = []string{}
		}
	} else {
		liked = s.likedBy(a.ID, viewerID)
		orgIDs = s.loadOrgIDs(a.ID)
		tags = s.loadArticleTags(a.ID)
	}
	imgBase := ""
	if s != nil {
		imgBase = s.publicImageBase()
	}
	coverOut := blogimg.ExpandCoverURL(a.CoverURL, imgBase)
	m := &pb.ArticleInfo{
		Id:                int64(a.ID),
		Slug:              a.Slug,
		Title:             a.Title,
		Summary:           a.Summary,
		CoverUrl:          coverOut,
		Visibility:        a.Visibility,
		Recommend:         a.Recommend,
		SyncToMainProfile: a.SyncToMainProfile,
		Tags:              tags,
		ViewCount:         int64(a.ViewCount),
		LikeCount:         int64(a.LikeCount),
		CommentCount:      int64(a.CommentCount),
		Liked:             liked,
		RequiresPassword:  d.RequiresPassword,
		CanSeeBody:        d.CanSeeBody,
		ModerationStatus:  normalizeModeration(a.ModerationStatus),
		CreatedAt:         a.CreatedAt.Unix(),
		UpdatedAt:         a.UpdatedAt.Unix(),
		OrgIds:            toInt64s(orgIDs),
	}
	if a.CategoryID != nil {
		m.CategoryId = int64(*a.CategoryID)
	}
	if a.ModerationNote != "" && viewerID == a.UserID {
		// 作者可见备注
		m.ModerationNote = a.ModerationNote
	}
	if a.SourceSolutionID != nil && *a.SourceSolutionID > 0 {
		m.SourceSolutionId = int64(*a.SourceSolutionID)
	}
	if a.SourceProblemID != nil && *a.SourceProblemID > 0 {
		m.SourceProblemId = int64(*a.SourceProblemID)
	}
	// 摘要一律自动生成；字段保留兼容旧前端
	if includeBody {
		m.SummaryIsDefault = true
	}
	if a.PublishedAt != nil {
		m.PublishedAt = a.PublishedAt.Unix()
	} else {
		m.PublishedAt = a.CreatedAt.Unix()
	}
	if author != nil {
		m.Author = &pb.Author{
			Id:       int64(author.ID),
			Username: author.Username,
			Name:     author.Name,
			Avatar:   expandAvatarBase(imgBase, author.Avatar),
		}
		m.UserId = int64(author.ID)
		m.Username = author.Username
	} else {
		m.UserId = int64(a.UserID)
	}
	if includeBody && d.CanSeeBody {
		m.Content = blogimg.ExpandStoredImageRefs(a.Content, imgBase)
	}
	// never leak password hash
	return m
}

// publicImageBase returns the current UpYun public base (scheme://domain), or "".
func (s *BlogService) publicImageBase() string {
	if s == nil {
		return ""
	}
	return s.loadUpyunClient().PublicBaseURL()
}

// blogErr 构造 Kratos 错误：HTTP 状态码 = code（等价手写 writeJSON(status, {code:1, message})）。
func blogErr(code int, msg string) error {
	reason := "ERROR"
	switch code {
	case http.StatusBadRequest:
		reason = "BAD_REQUEST"
	case http.StatusUnauthorized:
		reason = "UNAUTHORIZED"
	case http.StatusForbidden:
		reason = "FORBIDDEN"
	case http.StatusNotFound:
		reason = "NOT_FOUND"
	case http.StatusConflict:
		reason = "CONFLICT"
	case http.StatusInternalServerError:
		reason = "INTERNAL"
	case http.StatusBadGateway:
		reason = "BAD_GATEWAY"
	}
	return kerrors.New(code, reason, msg)
}

// toInt64s uint 切片 → int64 切片（repeated int64 输出字符串数组）。
func toInt64s(in []uint) []int64 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v)
	}
	return out
}

// toUints int64 切片 → uint 切片。
func toUints(in []int64) []uint {
	if len(in) == 0 {
		return nil
	}
	out := make([]uint, len(in))
	for i, v := range in {
		out[i] = uint(v)
	}
	return out
}

// socialLinksToProto 社交链接 → proto。
func socialLinksToProto(in []blogSocialLink) []*pb.SocialLink {
	out := make([]*pb.SocialLink, 0, len(in))
	for _, l := range in {
		out = append(out, &pb.SocialLink{Type: l.Type, Url: l.URL, Label: l.Label})
	}
	return out
}

// articleWriteReqFromCreate proto 写请求 → 手写 blogArticleWriteReq（保留指针语义）。
func articleWriteReqFromCreate(req *pb.CreateReq) blogArticleWriteReq {
	var sync *bool
	if req.SyncToMainProfile != nil {
		v := req.SyncToMainProfile.Value
		sync = &v
	}
	var catID *uint
	if req.CategoryId > 0 {
		v := uint(req.CategoryId)
		catID = &v
	}
	return blogArticleWriteReq{
		ID:                   uint(req.Id),
		Title:                req.Title,
		Slug:                 req.Slug,
		Summary:              req.Summary,
		Content:              req.Content,
		CoverURL:             req.CoverUrl,
		Visibility:           req.Visibility,
		Password:             req.Password,
		Recommend:            req.Recommend,
		SyncToMainProfile:    sync,
		CategoryID:           catID,
		OrgIDs:               toUints(req.OrgIds),
		Tags:                 req.Tags,
		ClearPassword:        req.ClearPassword,
		UseFirstImageAsCover: req.UseFirstImageAsCover,
	}
}

// articleWriteReqFromUpdate proto 写请求 → 手写 blogArticleWriteReq。
func articleWriteReqFromUpdate(req *pb.UpdateReq) blogArticleWriteReq {
	var sync *bool
	if req.SyncToMainProfile != nil {
		v := req.SyncToMainProfile.Value
		sync = &v
	}
	var catID *uint
	if req.CategoryId > 0 {
		v := uint(req.CategoryId)
		catID = &v
	}
	return blogArticleWriteReq{
		ID:                   uint(req.Id),
		Title:                req.Title,
		Slug:                 req.Slug,
		Summary:              req.Summary,
		Content:              req.Content,
		CoverURL:             req.CoverUrl,
		Visibility:           req.Visibility,
		Password:             req.Password,
		Recommend:            req.Recommend,
		SyncToMainProfile:    sync,
		CategoryID:           catID,
		OrgIDs:               toUints(req.OrgIds),
		Tags:                 req.Tags,
		ClearPassword:        req.ClearPassword,
		UseFirstImageAsCover: req.UseFirstImageAsCover,
	}
}

// ---------- list by username ----------

// ListByUsername 公开：按用户名列出文章（含作者与博客壳配置；JWT 可选）
func (s *BlogService) ListByUsername(ctx context.Context, req *pb.ListByUsernameReq) (*pb.ListByUsernameRes, error) {
	imgBase := s.publicImageBase()
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名")
	}
	u, err := s.findUserByUsername(username)
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "用户不存在")
	}
	viewer := blogViewerID(ctx)
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	categoryID := uint(req.CategoryId)
	keyword := strings.TrimSpace(req.Keyword)
	tagFilter := strings.TrimSpace(req.Tag)

	q := s.db.Model(&model.BlogArticle{}).Where("user_id = ?", u.ID)
	// non-owner: only public + password (meta)；never private；且须审核通过
	if viewer != u.ID {
		q = q.Where("visibility IN ?", []string{blogaccess.VisibilityPublic, blogaccess.VisibilityPassword}).
			Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)
	}
	if categoryID > 0 {
		q = q.Where("category_id = ?", categoryID)
	}
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}
	if tagFilter != "" {
		// 模糊：子串匹配作者 tag 名
		like := sqllike.Pattern(tagFilter)
		q = q.Where(
			`id IN (
				SELECT bat.article_id FROM blog_article_tags bat
				JOIN blog_tags bt ON bt.id = bat.tag_id
				WHERE bt.user_id = ? AND bt.name ILIKE ?
			)`,
			u.ID, like,
		)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	var list []model.BlogArticle
	if err := q.Order("COALESCE(published_at, created_at) DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	pre := s.prefetchArticleExtras(list, viewer)
	out := make([]*pb.ArticleInfo, 0, len(list))
	for i := range list {
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility:  list[i].Visibility,
			OwnerID:     list[i].UserID,
			HasPassword: list[i].PasswordHash != "",
		}, viewer, false)
		if !d.CanSeeMeta {
			continue
		}
		out = append(out, s.articleToProto(&list[i], u, d, viewer, false, pre))
	}

	// theme status for blog shell
	themeOn := s.themeEnabledFor(u.ID)
	siteCfg := s.loadSiteConfig(u.ID)
	activated := s.isBlogActivated(u.ID)
	isOwner := viewer == u.ID

	// 未开通：对访客不暴露文章列表（壳层前端据此提示「此用户未开通博客」）
	if !activated && !isOwner {
		out = []*pb.ArticleInfo{}
		total = 0
	}

	return &pb.ListByUsernameRes{
		Code:    0,
		Message: "success",
		Data: &pb.ByUsernameAuthorData{
			Author: &pb.Author{
				Id:       int64(u.ID),
				Username: u.Username,
				Name:     u.Name,
				Avatar:   expandAvatarBase(imgBase, u.Avatar),
			},
			List:         out,
			Total:        total,
			Page:         int64(page),
			PageSize:     int64(pageSize),
			ThemeEnabled: themeOn,
			ThemeId:      siteCfg.ThemeID,
			ColorScheme:  siteCfg.ColorScheme,
			Subtitle:     siteCfg.Subtitle,
			SocialLinks:  socialLinksToProto(siteCfg.SocialLinks),
			AboutMd:      siteCfg.AboutMD,
			HomeIntroMd:  siteCfg.HomeIntroMD,
			FriendsMd:    siteCfg.FriendsMD,
			IsOwner:      isOwner,
			Activated:    activated,
		},
	}, nil
}

// ---------- get article ----------

// GetArticle 公开：文章详情（id 或 username+slug；password/unlockToken 解锁）
func (s *BlogService) GetArticle(ctx context.Context, req *pb.GetArticleReq) (*pb.GetArticleRes, error) {
	id := uint(req.Id)
	username := strings.TrimSpace(req.Username)
	slug := strings.TrimSpace(req.Slug)
	password := req.Password
	unlock := req.UnlockToken

	var a model.BlogArticle
	var err error
	if id > 0 {
		err = s.db.First(&a, id).Error
	} else if username != "" && slug != "" {
		u, e := s.findUserByUsername(username)
		if e != nil {
			return nil, blogErr(http.StatusNotFound, "文章不存在")
		}
		err = s.db.Where("user_id = ? AND slug = ?", u.ID, slug).First(&a).Error
	} else {
		return nil, blogErr(http.StatusBadRequest, "缺少 id 或 username+slug")
	}
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
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
		return nil, blogErr(http.StatusNotFound, "文章不存在或无权查看")
	}
	// 未通过审核：仅作者/站管可见
	if !moderationVisibleToPublic(a.ModerationStatus) && viewer != a.UserID && !blogIsSiteAdmin(ctx) {
		return nil, blogErr(http.StatusNotFound, "文章不存在或无权查看")
	}

	// UV view when body is visible (solution-linked shares UV with 题解)
	if d.CanSeeBody {
		httpReq, _ := khttp.RequestFromServerContext(ctx)
		var visitorKey string
		if httpReq != nil {
			visitorKey = blogVisitorKey(httpReq, viewer)
		}
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

	m := s.articleToProto(&a, &author, d, viewer, true, nil)
	if d.RequiresPassword {
		m.Message = "需要密码才能阅读全文"
	}
	return &pb.GetArticleRes{Code: 0, Message: "success", Data: m}, nil
}

// Unlock 公开：密码文解锁，返回 unlockToken
func (s *BlogService) Unlock(ctx context.Context, req *pb.UnlockReq) (*pb.UnlockRes, error) {
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, req.Id).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	if blogaccess.NormalizeVisibility(a.Visibility) != blogaccess.VisibilityPassword {
		return nil, blogErr(http.StatusBadRequest, "该文章无需密码")
	}
	if !checkBlogPassword(a.PasswordHash, req.Password) {
		return nil, blogErr(http.StatusForbidden, "密码不正确")
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
	httpReq, _ := khttp.RequestFromServerContext(ctx)
	var visitorKey string
	if httpReq != nil {
		visitorKey = blogVisitorKey(httpReq, viewer)
	}
	if s.recordBlogArticleUV(a.ID, visitorKey) {
		a.ViewCount++
	} else {
		_ = s.db.Select("view_count").First(&a, a.ID).Error
	}
	m := s.articleToProto(&a, &author, d, viewer, true, nil)
	m.UnlockToken = token
	return &pb.UnlockRes{Code: 0, Message: "success", Data: m}, nil
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
	Recommend bool `json:"recommend"`
	// SyncToMainProfile 公开文可写：true=进广场/组织发现；false=仅个人站公开。
	// 省略时公开默认 true；非公开强制 false。用指针区分「未传」与「显式 false」。
	SyncToMainProfile *bool    `json:"syncToMainProfile"`
	CategoryID        *uint    `json:"categoryId"`
	OrgIDs            []uint   `json:"orgIds"`
	Tags              []string `json:"tags"`
	// ClearPassword when true removes password on update (if visibility changes away).
	ClearPassword bool `json:"clearPassword"`
	// UseFirstImageAsCover：cover 为空时用正文第一张 http(s) 图写入 coverUrl；不入库。
	UseFirstImageAsCover bool `json:"useFirstImageAsCover"`
}

// Create 登录：创建文章
func (s *BlogService) Create(ctx context.Context, req *pb.CreateReq) (*pb.CreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if err := s.requireActivated(ctx, pd.UserID); err != nil {
		return nil, err
	}
	writeReq := articleWriteReqFromCreate(req)
	var a *model.BlogArticle
	var validationMsg string
	err := blogimg.WithUserImageReferenceTx(s.db, pd.UserID, func(tx *gorm.DB) error {
		txService := *s
		txService.db = tx
		a, validationMsg = txService.buildArticleFromReq(pd.UserID, 0, &writeReq, true)
		if validationMsg != "" {
			return gorm.ErrInvalidData
		}
		now := time.Now()
		a.PublishedAt = &now
		a.ModerationStatus = model.BlogModerationApproved
		if err := tx.Create(a).Error; err != nil {
			return err
		}
		if validationMsg = txService.replaceArticleTags(a.ID, a.UserID, writeReq.Tags); validationMsg != "" {
			return gorm.ErrInvalidData
		}
		return txService.applyAutoOrgSurface(a.ID, a.UserID, a.Visibility, a.SyncToMainProfile)
	})
	if validationMsg != "" {
		return nil, blogErr(http.StatusBadRequest, validationMsg)
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			return nil, blogErr(http.StatusBadRequest, "短链已被占用，请换一个")
		}
		return nil, blogErr(http.StatusInternalServerError, "保存失败")
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, true)
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error
	return &pb.CreateRes{
		Code: 0, Message: "success",
		Data: s.articleToProto(a, &author, d, pd.UserID, true, nil),
	}, nil
}

// Update 登录：更新文章（作者/站管/审核员）
func (s *BlogService) Update(ctx context.Context, req *pb.UpdateReq) (*pb.UpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var existing model.BlogArticle
	if err := s.db.First(&existing, req.Id).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	if !blogaccess.CanManage(existing.UserID, pd.UserID, auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate)) {
		return nil, blogErr(http.StatusForbidden, "只能管理自己的文章")
	}
	if existing.UserID == pd.UserID && !pd.IsSiteAdmin {
		if err := s.requireActivated(ctx, pd.UserID); err != nil {
			return nil, err
		}
	}
	writeReq := articleWriteReqFromUpdate(req)
	var a *model.BlogArticle
	var validationMsg string
	err := blogimg.WithUserImageReferenceTx(s.db, existing.UserID, func(tx *gorm.DB) error {
		if err := tx.First(&existing, req.Id).Error; err != nil {
			return err
		}
		txService := *s
		txService.db = tx
		a, validationMsg = txService.buildArticleFromReq(existing.UserID, existing.ID, &writeReq, false)
		if validationMsg != "" {
			return gorm.ErrInvalidData
		}
		// preserve counters, moderation, password and source link from the locked row.
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
		if a.PasswordHash == "" && !writeReq.ClearPassword && existing.PasswordHash != "" &&
			blogaccess.NormalizeVisibility(a.Visibility) == blogaccess.VisibilityPassword &&
			strings.TrimSpace(writeReq.Password) == "" {
			a.PasswordHash = existing.PasswordHash
		}
		a.SourceSolutionID = existing.SourceSolutionID
		a.SourceProblemID = existing.SourceProblemID
		if blogaccess.AutoSurface(a.Visibility) {
			a.Recommend = existing.Recommend
		}
		if writeReq.SyncToMainProfile == nil && blogaccess.AutoSurface(a.Visibility) {
			a.SyncToMainProfile = existing.SyncToMainProfile
		}
		if err := tx.Save(a).Error; err != nil {
			return err
		}
		if writeReq.Tags != nil {
			if validationMsg = txService.replaceArticleTags(a.ID, a.UserID, writeReq.Tags); validationMsg != "" {
				return gorm.ErrInvalidData
			}
		}
		return txService.applyAutoOrgSurface(a.ID, a.UserID, a.Visibility, a.SyncToMainProfile)
	})
	if validationMsg != "" {
		return nil, blogErr(http.StatusBadRequest, validationMsg)
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			return nil, blogErr(http.StatusBadRequest, "短链已被占用，请换一个")
		}
		return nil, blogErr(http.StatusInternalServerError, "保存失败")
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, true)
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, a.UserID).Error
	return &pb.UpdateRes{
		Code: 0, Message: "success",
		Data: s.articleToProto(a, &author, d, pd.UserID, true, nil),
	}, nil
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
	// 本站又拍云图存 path-only（/blog/{uid}/…）；读时再拼当前访问域名。
	content = blogimg.NormalizeStoredImageRefs(content)
	// 摘要一律按正文自动生成，忽略客户端手写
	summary := blogaccess.DefaultSummary(content)
	if utf8.RuneCountInString(summary) > maxBlogSummary {
		runes := []rune(summary)
		summary = string(runes[:maxBlogSummary])
	}
	// 头图：勾选「用正文第一张图」时每次保存都重识别（忽略旧 coverUrl）；
	// 未勾选则用手填/上传的 coverUrl，可为空。
	var cover string
	if req.UseFirstImageAsCover {
		cover = blogimg.ResolveCoverURL("", content, true, maxBlogCover)
	} else {
		cover = strings.TrimSpace(req.CoverURL)
		if cover != "" {
			if !blogimg.ValidCoverInput(cover) {
				return nil, "头图请使用 http(s) 链接或本站图床路径"
			}
			cover = blogimg.NormalizeCoverURL(cover)
			if len(cover) > maxBlogCover {
				return nil, "头图链接过长"
			}
		}
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
		// 更新时缺省 slug：保留原短链，禁止随标题重算（slug 为文章稳定标识）
		if !isCreate && existingID > 0 {
			var prev model.BlogArticle
			if err := s.db.Select("slug").First(&prev, existingID).Error; err == nil && strings.TrimSpace(prev.Slug) != "" {
				slug = prev.Slug
			}
		}
		if slug == "" {
			slug = slugifyTitle(title)
		}
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

	// 公开文默认同步主站；作者可显式 false 仅留个人站。非公开强制不同步。
	// 精选(recommend) 默认 false，由审核员设。
	sync := false
	if blogaccess.AutoSurface(vis) {
		if req.SyncToMainProfile == nil {
			sync = true
		} else {
			sync = *req.SyncToMainProfile
		}
	}
	// 落库正文图 content hash，GC 按 hash 判定引用（不依赖 URL/域名形态）。
	imageHashes := blogimg.EncodeImageHashes(
		blogimg.ResolveContentHashes(s.db, userID, content, cover),
	)
	return &model.BlogArticle{
		UserID:            userID,
		Slug:              slug,
		Title:             title,
		Summary:           summary,
		Content:           content,
		CoverURL:          cover,
		ImageHashes:       imageHashes,
		Visibility:        vis,
		PasswordHash:      pwHash,
		Recommend:         false,
		SyncToMainProfile: sync,
		CategoryID:        req.CategoryID,
	}, ""
}

// applyAutoOrgSurface syncs article to all orgs the author belongs to when public AND sync.
// Non-public or unsynced clears org surfaces.
func (s *BlogService) applyAutoOrgSurface(articleID, userID uint, visibility string, syncToMain bool) error {
	if !blogaccess.AutoSurface(visibility) || !syncToMain {
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

func blogVisitorKey(req *http.Request, viewerID uint) string {
	if viewerID > 0 {
		return fmt.Sprintf("u:%d", viewerID)
	}
	// cookie / header visitor id
	if c, err := req.Cookie("goalgo_vid"); err == nil && c != nil {
		v := strings.TrimSpace(c.Value)
		if v != "" && len(v) <= 64 {
			return "v:" + v
		}
	}
	if h := strings.TrimSpace(req.Header.Get("X-Visitor-Id")); h != "" && len(h) <= 64 {
		return "v:" + h
	}
	// fallback: IP + UA hash (best-effort anonymous UV)
	ip := req.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = req.RemoteAddr
	}
	ua := req.UserAgent()
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

// Delete 登录：删除文章（级联清理评论/点赞/标签/UV）
func (s *BlogService) Delete(ctx context.Context, req *pb.DeleteReq) (*pb.DeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, req.Id).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	if !blogaccess.CanManage(a.UserID, pd.UserID, auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate)) {
		return nil, blogErr(http.StatusForbidden, "只能删除自己的文章")
	}
	err := blogimg.WithUserImageReferenceTx(s.db, a.UserID, func(tx *gorm.DB) error {
		if err := tx.Where("article_id = ?", a.ID).Delete(&model.BlogArticleOrg{}).Error; err != nil {
			return err
		}
		if err := tx.Where("article_id = ?", a.ID).Delete(&model.BlogArticleTag{}).Error; err != nil {
			return err
		}
		// 先清评论点赞再删评论
		var cmtIDs []uint
		if err := tx.Model(&model.BlogComment{}).Where("article_id = ?", a.ID).Pluck("id", &cmtIDs).Error; err != nil {
			return err
		}
		if len(cmtIDs) > 0 {
			if err := tx.Where("comment_id IN ?", cmtIDs).Delete(&model.BlogCommentLike{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("article_id = ?", a.ID).Delete(&model.BlogComment{}).Error; err != nil {
			return err
		}
		if err := tx.Where("article_id = ?", a.ID).Delete(&model.BlogLike{}).Error; err != nil {
			return err
		}
		if err := tx.Where("article_id = ?", a.ID).Delete(&model.BlogArticleViewUV{}).Error; err != nil {
			return err
		}
		return tx.Delete(&a).Error
	})
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "删除失败")
	}
	return &pb.DeleteRes{Code: 0, Message: "已删除"}, nil
}

// Mine 登录：我的文章列表
func (s *BlogService) Mine(ctx context.Context, req *pb.MineReq) (*pb.MineRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	keyword := strings.TrimSpace(req.Keyword)
	q := s.db.Model(&model.BlogArticle{}).Where("user_id = ?", pd.UserID)
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}
	var total int64
	_ = q.Count(&total).Error
	var list []model.BlogArticle
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	var author model.User
	_ = s.db.Select("id", "username", "name", "avatar").First(&author, pd.UserID).Error
	pre := s.prefetchArticleExtras(list, pd.UserID)
	out := make([]*pb.ArticleInfo, 0, len(list))
	for i := range list {
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility:  list[i].Visibility,
			OwnerID:     list[i].UserID,
			HasPassword: list[i].PasswordHash != "",
		}, pd.UserID, true)
		out = append(out, s.articleToProto(&list[i], &author, d, pd.UserID, false, pre))
	}
	return &pb.MineRes{
		Code: 0, Message: "success",
		Data: &pb.ArticleListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// ---------- recommend ----------

// Recommend 公开：精选文章（推荐）
func (s *BlogService) Recommend(ctx context.Context, req *pb.RecommendReq) (*pb.RecommendRes, error) {
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	viewer := blogViewerID(ctx)
	// 仅站管/审核员标记精选(recommend=true) 的公开已审且同步主站文章
	q := s.db.Model(&model.BlogArticle{}).
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Where("sync_to_main_profile = ?", true).
		Where("recommend = ?", true).
		Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)

	// optional org filter: 公共域/缺省 → 全站公开文；私有域 → 仅该组织成员的文章
	// （作者所属各域均可见自己的公开文；私有域看不到非成员的公共域内容）
	orgID := uint(req.OrgId)
	if orgID > 0 {
		var o model.Org
		if s.db.Select("id", "is_system").First(&o, orgID).Error == nil && !o.IsSystem {
			q = q.Where(
				"user_id IN (SELECT user_id FROM org_members WHERE org_id = ?)",
				orgID,
			)
		}
	}
	// exclude solution-mirrored articles when excludeSolutions=1 (discover dedupe)
	if req.ExcludeSolutions == 1 {
		q = q.Where("source_solution_id IS NULL OR source_solution_id = 0")
	}

	var total int64
	_ = q.Count(&total).Error
	var list []model.BlogArticle
	if err := q.Order("COALESCE(published_at, created_at) DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	out := s.batchMapArticlesProto(list, viewer)
	return &pb.RecommendRes{
		Code: 0, Message: "success",
		Data: &pb.ArticleListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// ---------- plaza (main-site public feed) ----------

// Plaza 公开：主站博客广场文章流
func (s *BlogService) Plaza(ctx context.Context, req *pb.PlazaReq) (*pb.PlazaRes, error) {
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	// Prefer denser default for plaza cards
	if req.PageSize == 0 {
		pageSize = 12
	}
	viewer := blogViewerID(ctx)
	keyword := strings.TrimSpace(req.Keyword)
	sort := strings.ToLower(strings.TrimSpace(req.Sort))
	if sort == "" {
		sort = "latest"
	}

	// 最新/热门：公开已审且同步主站；精选：仅 recommend=true（站管/审核员标记）
	q := s.db.Model(&model.BlogArticle{}).
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Where("sync_to_main_profile = ?", true).
		Where("(moderation_status = ? OR moderation_status = '' OR moderation_status IS NULL)", model.BlogModerationApproved)
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}

	// optional org filter: 与 recommend 一致——公共域/缺省=全站；私有域=仅该组织成员作者
	orgID := uint(req.OrgId)
	if orgID > 0 {
		var o model.Org
		if s.db.Select("id", "is_system").First(&o, orgID).Error == nil && !o.IsSystem {
			q = q.Where(
				"user_id IN (SELECT user_id FROM org_members WHERE org_id = ?)",
				orgID,
			)
		}
	}
	// 发现页去重：排除题解镜像文（题解走 activity/feed）
	if req.ExcludeSolutions == 1 {
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
		return nil, blogErr(http.StatusBadRequest, "sort 须为 latest|hot|recommend")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}

	var list []model.BlogArticle
	if err := q.Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	out := s.batchMapArticlesProto(list, viewer)
	return &pb.PlazaRes{
		Code: 0, Message: "success",
		Data: &pb.ArticleListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// ---------- active authors (plaza side rail) ----------

// Authors 公开：最近有公开文的作者（广场侧栏）
func (s *BlogService) Authors(ctx context.Context, req *pb.AuthorsReq) (*pb.AuthorsRes, error) {
	imgBase := s.publicImageBase()
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	if pageSize > 30 {
		pageSize = 30
	}
	if req.PageSize == 0 {
		pageSize = 12
	}
	keyword := strings.TrimSpace(req.Keyword)

	// Aggregate public articles per author, ordered by last publish time.
	type aggRow struct {
		UserID          uint
		ArticleCount    int64
		LastPublishedAt *time.Time
	}
	base := s.db.Model(&model.BlogArticle{}).
		Select("user_id, COUNT(*) as article_count, MAX(COALESCE(published_at, created_at)) as last_published_at").
		Where("visibility = ?", blogaccess.VisibilityPublic).
		Where("sync_to_main_profile = ?", true).
		Group("user_id")

	// Optional name/username filter via join
	var total int64
	countQ := s.db.Table("(?) as author_agg", base)
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		countQ = s.db.Table("(?) as author_agg", base).
			Joins("JOIN users ON users.id = author_agg.user_id").
			Where("users.username ILIKE ? OR users.name ILIKE ?", like, like)
	}
	if err := countQ.Count(&total).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}

	var aggs []aggRow
	listQ := s.db.Table("(?) as author_agg", base).
		Select("author_agg.user_id, author_agg.article_count, author_agg.last_published_at")
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		listQ = listQ.Joins("JOIN users ON users.id = author_agg.user_id").
			Where("users.username ILIKE ? OR users.name ILIKE ?", like, like)
	}
	if err := listQ.Order("author_agg.last_published_at DESC NULLS LAST").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&aggs).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
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

	out := make([]*pb.PlazaAuthorInfo, 0, len(aggs))
	for _, a := range aggs {
		u := authors[a.UserID]
		item := &pb.PlazaAuthorInfo{
			Id:           int64(a.UserID),
			Username:     u.Username,
			Name:         u.Name,
			Avatar:       expandAvatarBase(imgBase, u.Avatar),
			ArticleCount: a.ArticleCount,
			LatestTitle:  latestTitle[a.UserID],
		}
		if a.LastPublishedAt != nil {
			item.LastPublishedAt = a.LastPublishedAt.Unix()
		}
		out = append(out, item)
	}

	return &pb.AuthorsRes{
		Code: 0, Message: "success",
		Data: &pb.PlazaAuthorListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// batchMapArticlesProto loads authors once and maps list items without body.
func (s *BlogService) batchMapArticlesProto(list []model.BlogArticle, viewer uint) []*pb.ArticleInfo {
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
	out := make([]*pb.ArticleInfo, 0, len(list))
	for i := range list {
		u := authors[list[i].UserID]
		d := blogaccess.Evaluate(blogaccess.ArticleAccess{
			Visibility: list[i].Visibility,
			OwnerID:    list[i].UserID,
		}, viewer, false)
		out = append(out, s.articleToProto(&list[i], &u, d, viewer, false, pre))
	}
	return out
}

// ---------- analytics ----------

// Analytics 登录：作者统计（汇总 + top 文章）
func (s *BlogService) Analytics(ctx context.Context, req *pb.AnalyticsReq) (*pb.AnalyticsRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
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
	data := &pb.AnalyticsData{
		TotalArticles: r.Articles,
		TotalViews:    r.Views,
		TotalLikes:    r.Likes,
		TotalComments: r.Comments,
	}
	for _, a := range top {
		data.TopArticles = append(data.TopArticles, &pb.AnalyticsTopArticle{
			Id:           int64(a.ID),
			Slug:         a.Slug,
			Title:        a.Title,
			ViewCount:    int64(a.ViewCount),
			LikeCount:    int64(a.LikeCount),
			CommentCount: int64(a.CommentCount),
			Visibility:   a.Visibility,
		})
	}
	return &pb.AnalyticsRes{Code: 0, Message: "success", Data: data}, nil
}

// ---------- categories ----------

// ListCategoriesPublic 公开：作者公开分类列表（仅含 >0 篇公开文章的分类）
func (s *BlogService) ListCategoriesPublic(ctx context.Context, req *pb.ListCategoriesPublicReq) (*pb.ListCategoriesPublicRes, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名")
	}
	u, err := s.findUserByUsername(username)
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "用户不存在")
	}
	// 访客侧不强制创建默认分类（避免写路径）；仅列出已有
	var list []model.BlogCategory
	_ = s.db.Where("user_id = ?", u.ID).Order("is_default DESC, sort_order ASC, id ASC").Find(&list).Error
	counts := s.categoryArticleCounts(list, true)
	// 前台不展示 0 篇公开文章的分类（访客侧仿佛不存在）
	out := make([]*pb.CategoryInfo, 0, len(list))
	for _, c := range list {
		n := counts[c.ID]
		if n <= 0 {
			continue
		}
		out = append(out, &pb.CategoryInfo{
			Id: int64(c.ID), Name: c.Name, SortOrder: int32(c.SortOrder), ArticleCount: n, IsDefault: c.IsDefault,
		})
	}
	return &pb.ListCategoriesPublicRes{Code: 0, Message: "success", List: out}, nil
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

// CategoryMine 登录：我的分类列表（自动确保默认分类存在）
func (s *BlogService) CategoryMine(ctx context.Context, req *pb.CategoryMineReq) (*pb.CategoryMineRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	// 管理页确保默认分类存在
	_, _ = blogsync.EnsureDefaultCategory(s.db, pd.UserID)
	var list []model.BlogCategory
	_ = s.db.Where("user_id = ?", pd.UserID).Order("is_default DESC, sort_order ASC, id ASC").Find(&list).Error
	counts := s.categoryArticleCounts(list, false)
	out := make([]*pb.CategoryInfo, 0, len(list))
	for _, c := range list {
		out = append(out, &pb.CategoryInfo{
			Id: int64(c.ID), Name: c.Name, SortOrder: int32(c.SortOrder), ArticleCount: counts[c.ID], IsDefault: c.IsDefault,
		})
	}
	return &pb.CategoryMineRes{Code: 0, Message: "success", List: out}, nil
}

// CategoryCreate 登录：创建分类
func (s *BlogService) CategoryCreate(ctx context.Context, req *pb.CategoryCreateReq) (*pb.CategoryCreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		return nil, blogErr(http.StatusBadRequest, "分类名无效")
	}
	c := model.BlogCategory{UserID: pd.UserID, Name: name, SortOrder: int(req.SortOrder), IsDefault: false}
	if err := s.db.Create(&c).Error; err != nil {
		return nil, blogErr(http.StatusBadRequest, "创建失败，可能重名")
	}
	return &pb.CategoryCreateRes{
		Code: 0, Message: "success",
		Data: &pb.CategoryCreateData{Id: int64(c.ID), Name: c.Name, SortOrder: int32(c.SortOrder), IsDefault: c.IsDefault},
	}, nil
}

// CategoryUpdate 登录：更新分类（name/sortOrder 省略保留原值）
func (s *BlogService) CategoryUpdate(ctx context.Context, req *pb.CategoryUpdateReq) (*pb.CategoryUpdateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var c model.BlogCategory
	if err := s.db.Where("id = ? AND user_id = ?", req.Id, pd.UserID).First(&c).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "分类不存在")
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		c.Name = n
	}
	if req.SortOrder != nil {
		c.SortOrder = int(req.SortOrder.Value)
	}
	if err := s.db.Save(&c).Error; err != nil {
		return nil, blogErr(http.StatusBadRequest, "保存失败")
	}
	return &pb.CategoryUpdateRes{
		Code: 0, Message: "success",
		Data: &pb.CategoryCreateData{Id: int64(c.ID), Name: c.Name, SortOrder: int32(c.SortOrder), IsDefault: c.IsDefault},
	}, nil
}

// CategoryDelete 登录：删除分类（默认分类不可删；文章改挂默认分类）
func (s *BlogService) CategoryDelete(ctx context.Context, req *pb.CategoryDeleteReq) (*pb.CategoryDeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var c model.BlogCategory
	if err := s.db.Where("id = ? AND user_id = ?", req.Id, pd.UserID).First(&c).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "分类不存在")
	}
	if c.IsDefault {
		return nil, blogErr(http.StatusBadRequest, "默认分类不能删除")
	}
	res := s.db.Where("id = ? AND user_id = ?", req.Id, pd.UserID).Delete(&model.BlogCategory{})
	if res.RowsAffected == 0 {
		return nil, blogErr(http.StatusNotFound, "分类不存在")
	}
	// 非默认分类文章改挂到默认分类
	if defID, err := blogsync.EnsureDefaultCategory(s.db, pd.UserID); err == nil && defID > 0 {
		_ = s.db.Model(&model.BlogArticle{}).Where("category_id = ?", req.Id).Update("category_id", defID).Error
	} else {
		_ = s.db.Model(&model.BlogArticle{}).Where("category_id = ?", req.Id).Update("category_id", nil).Error
	}
	return &pb.CategoryDeleteRes{Code: 0, Message: "已删除"}, nil
}

// ---------- comments ----------

// ListComments 公开：文章评论列表（顶层分页 + 嵌套 replies）
func (s *BlogService) ListComments(ctx context.Context, req *pb.ListCommentsReq) (*pb.ListCommentsRes, error) {
	imgBase := s.publicImageBase()
	articleID := uint(req.ArticleId)
	if articleID == 0 {
		return nil, blogErr(http.StatusBadRequest, "缺少 articleId")
	}
	// ensure article is at least meta-visible
	var a model.BlogArticle
	if err := s.db.First(&a, articleID).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	viewer := blogViewerID(ctx)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, viewer, false)
	// comments only when meta visible (public/password teaser/owner)
	if !d.CanSeeMeta {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}

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

	var buildNode func(id uint) *pb.CommentInfo
	buildNode = func(id uint) *pb.CommentInfo {
		c := byID[id]
		u := users[c.UserID]
		m := &pb.CommentInfo{
			Id:        int64(c.ID),
			ArticleId: int64(c.ArticleID),
			ParentId:  int64(c.ParentID),
			Content:   c.Content,
			CreatedAt: c.CreatedAt.Unix(),
			UserId:    int64(c.UserID),
			LikeCount: int32(c.LikeCount),
			Liked:     likedSet[c.ID],
			Author: &pb.Author{
				Id: int64(u.ID), Username: u.Username, Name: u.Name, Avatar: expandAvatarBase(imgBase, u.Avatar),
			},
		}
		if c.ParentID > 0 {
			if p, ok := byID[c.ParentID]; ok {
				pu := users[p.UserID]
				m.ReplyToUserId = int64(p.UserID)
				m.ReplyToUsername = pu.Username
				m.ReplyToName = pu.Name
			}
		}
		reps := children[id]
		for _, rid := range reps {
			m.Replies = append(m.Replies, buildNode(rid))
		}
		return m
	}

	out := make([]*pb.CommentInfo, 0, len(rootIDs))
	for _, id := range rootIDs {
		out = append(out, buildNode(id))
	}
	return &pb.ListCommentsRes{
		Code: 0, Message: "success",
		Data: &pb.CommentListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// CommentCreate 登录：发表评论
func (s *BlogService) CommentCreate(ctx context.Context, req *pb.CommentCreateReq) (*pb.CommentCreateRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.ArticleId == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	content := strings.TrimSpace(req.Content)
	if content == "" || utf8.RuneCountInString(content) > maxCommentLen {
		return nil, blogErr(http.StatusBadRequest, "评论内容无效")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, req.ArticleId).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	// only comment when body is readable without password OR viewer is owner
	// (password articles: must unlock first — we allow comment if public or owner)
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeBody && blogaccess.NormalizeVisibility(a.Visibility) != blogaccess.VisibilityPassword {
		return nil, blogErr(http.StatusForbidden, "无法评论此文章")
	}
	if blogaccess.NormalizeVisibility(a.Visibility) == blogaccess.VisibilityPrivate && pd.UserID != a.UserID {
		return nil, blogErr(http.StatusForbidden, "无法评论此文章")
	}
	if req.ParentId > 0 {
		var parent model.BlogComment
		if err := s.db.Where("id = ? AND article_id = ?", req.ParentId, req.ArticleId).First(&parent).Error; err != nil {
			return nil, blogErr(http.StatusBadRequest, "父评论不存在")
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
			return nil, blogErr(http.StatusBadRequest, "回复层级已达上限")
		}
	}
	c := model.BlogComment{
		ArticleID: uint(req.ArticleId),
		UserID:    pd.UserID,
		ParentID:  uint(req.ParentId),
		Content:   content,
	}
	if err := s.db.Create(&c).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "发表失败")
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
	if req.ParentId > 0 {
		var parent model.BlogComment
		if s.db.First(&parent, req.ParentId).Error == nil && parent.UserID > 0 && parent.UserID != pd.UserID {
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

	return &pb.CommentCreateRes{
		Code: 0, Message: "success",
		Data: &pb.CommentCreateData{
			Id: int64(c.ID), ArticleId: int64(c.ArticleID), ParentId: int64(c.ParentID),
			Content: c.Content, CreatedAt: c.CreatedAt.Unix(), UserId: int64(c.UserID),
		},
	}, nil
}

// CommentDelete 登录：删除评论（作者/文章作者/站管；级联删除子树 + 点赞）
func (s *BlogService) CommentDelete(ctx context.Context, req *pb.CommentDeleteReq) (*pb.CommentDeleteRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var c model.BlogComment
	if err := s.db.First(&c, req.Id).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "评论不存在")
	}
	var a model.BlogArticle
	_ = s.db.First(&a, c.ArticleID).Error
	if c.UserID != pd.UserID && a.UserID != pd.UserID && !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		return nil, blogErr(http.StatusForbidden, "无权删除")
	}
	// 级联删除子树 + 点赞
	ids := s.collectBlogCommentSubtree(c.ID, c.ArticleID)
	if len(ids) == 0 {
		ids = []uint{c.ID}
	}
	_ = s.db.Where("comment_id IN ?", ids).Delete(&model.BlogCommentLike{}).Error
	if err := s.db.Where("id IN ?", ids).Delete(&model.BlogComment{}).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "删除失败")
	}
	n := len(ids)
	_ = s.db.Model(&model.BlogArticle{}).
		Where("id = ? AND comment_count >= ?", c.ArticleID, n).
		UpdateColumn("comment_count", gorm.Expr("comment_count - ?", n)).Error
	// 防止负数
	_ = s.db.Model(&model.BlogArticle{}).
		Where("id = ? AND comment_count < 0", c.ArticleID).
		UpdateColumn("comment_count", 0).Error
	return &pb.CommentDeleteRes{Code: 0, Message: "已删除"}, nil
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
// CommentLikeToggle 登录：博客评论点赞 toggle
func (s *BlogService) CommentLikeToggle(ctx context.Context, req *pb.CommentLikeToggleReq) (*pb.CommentLikeToggleRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.CommentId == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var c model.BlogComment
	if err := s.db.First(&c, req.CommentId).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "评论不存在")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, c.ArticleID).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility: a.Visibility, OwnerID: a.UserID, HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeMeta {
		return nil, blogErr(http.StatusNotFound, "评论不存在")
	}

	liked := false
	var likeCount int
	notifyAuthor := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing model.BlogCommentLike
		qerr := tx.Where("comment_id = ? AND user_id = ?", req.CommentId, pd.UserID).First(&existing).Error
		if qerr == nil {
			if err := tx.Delete(&existing).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.BlogComment{}).Where("id = ? AND like_count > 0", req.CommentId).
				UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error; err != nil {
				return err
			}
			liked = false
		} else if qerr == gorm.ErrRecordNotFound {
			if err := tx.Create(&model.BlogCommentLike{CommentID: uint(req.CommentId), UserID: pd.UserID}).Error; err != nil {
				// 并发唯一冲突：视为已赞
				if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
					liked = true
					return nil
				}
				return err
			}
			if err := tx.Model(&model.BlogComment{}).Where("id = ?", req.CommentId).
				UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error; err != nil {
				return err
			}
			liked = true
			notifyAuthor = c.UserID > 0 && c.UserID != pd.UserID
		} else {
			return qerr
		}
		return tx.Model(&model.BlogComment{}).Select("like_count").Where("id = ?", req.CommentId).Scan(&likeCount).Error
	})
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "操作失败，请稍后重试")
	}
	if notifyAuthor {
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
	return &pb.CommentLikeToggleRes{
		Code: 0, Message: "success",
		Data: &pb.CommentLikeData{Liked: liked, LikeCount: int32(likeCount), CommentId: req.CommentId},
	}, nil
}

// LikeToggle 登录：文章点赞 toggle
func (s *BlogService) LikeToggle(ctx context.Context, req *pb.LikeToggleReq) (*pb.LikeToggleRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.ArticleId == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, req.ArticleId).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility: a.Visibility, OwnerID: a.UserID, HasPassword: a.PasswordHash != "",
	}, pd.UserID, false)
	if !d.CanSeeMeta {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	liked := false
	var likeCount int
	notifyAuthor := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing model.BlogLike
		qerr := tx.Where("article_id = ? AND user_id = ?", req.ArticleId, pd.UserID).First(&existing).Error
		if qerr == nil {
			if err := tx.Delete(&existing).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.BlogArticle{}).Where("id = ? AND like_count > 0", req.ArticleId).
				UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error; err != nil {
				return err
			}
			liked = false
		} else if qerr == gorm.ErrRecordNotFound {
			if err := tx.Create(&model.BlogLike{ArticleID: uint(req.ArticleId), UserID: pd.UserID}).Error; err != nil {
				if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
					liked = true
					return nil
				}
				return err
			}
			if err := tx.Model(&model.BlogArticle{}).Where("id = ?", req.ArticleId).
				UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error; err != nil {
				return err
			}
			liked = true
			notifyAuthor = a.UserID > 0 && a.UserID != pd.UserID
		} else {
			return qerr
		}
		return tx.Model(&model.BlogArticle{}).Select("like_count").Where("id = ?", req.ArticleId).Scan(&likeCount).Error
	})
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "操作失败，请稍后重试")
	}
	if notifyAuthor {
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
	return &pb.LikeToggleRes{
		Code: 0, Message: "success",
		Data: &pb.LikeData{Liked: liked, LikeCount: int32(likeCount)},
	}, nil
}

// ---------- report ----------

// Report 登录：举报博客文章
func (s *BlogService) Report(ctx context.Context, req *pb.ReportReq) (*pb.ReportRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if req.ArticleId == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	reason := strings.TrimSpace(strings.ReplaceAll(req.Reason, "\r\n", "\n"))
	if reason == "" {
		return nil, blogErr(http.StatusBadRequest, "请填写举报原因")
	}
	if utf8.RuneCountInString(reason) > 500 {
		return nil, blogErr(http.StatusBadRequest, "举报原因过长")
	}
	var a model.BlogArticle
	if err := s.db.First(&a, req.ArticleId).Error; err != nil {
		return nil, blogErr(http.StatusNotFound, "文章不存在")
	}
	if a.UserID == pd.UserID {
		return nil, blogErr(http.StatusBadRequest, "不能举报自己的文章")
	}
	var existing model.BlogReport
	if s.db.Where("user_id = ? AND article_id = ?", pd.UserID, req.ArticleId).First(&existing).Error == nil {
		return &pb.ReportRes{
			Code: 0, Message: "你已举报过该文章，我们会尽快处理",
			Data: &pb.ReportData{Id: int64(existing.ID), AlreadyReported: true},
		}, nil
	}
	row := model.BlogReport{
		UserID:    pd.UserID,
		ArticleID: uint(req.ArticleId),
		Reason:    reason,
		Status:    "pending",
	}
	if err := s.db.Create(&row).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "提交失败，请稍后重试")
	}
	s.notifyAdminsBlogReport(pd, &a, reason, row.ID)
	return &pb.ReportRes{
		Code: 0, Message: "已收到举报，我们会尽快处理",
		Data: &pb.ReportData{Id: int64(row.ID), AlreadyReported: false},
	}, nil
}

// handleReportList 举报处理台：博客文章举报列表（需 content.report.handle）。
// query: status=pending|resolved|dismissed|all（默认 pending）、page/pageSize
// ReportList 举报处理台：博客文章举报列表（需 content.report.handle）
// query: status=pending|resolved|dismissed|all（默认 pending）、page/pageSize
func (s *BlogService) ReportList(ctx context.Context, req *pb.ReportListReq) (*pb.ReportListRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermContentReportHandle) {
		return nil, blogErr(http.StatusForbidden, "需要举报处理权限")
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "pending"
	}
	if status != "all" && status != "pending" && status != "resolved" && status != "dismissed" {
		return nil, blogErr(http.StatusBadRequest, "不支持的状态筛选")
	}
	page, pageSize := int(req.Page), int(req.PageSize)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
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

	out := make([]*pb.ReportItem, 0, len(rows))
	for _, r := range rows {
		item := &pb.ReportItem{
			Id:        int64(r.ID),
			CreatedAt: r.CreatedAt.Unix(),
			Status:    r.Status,
			Reason:    r.Reason,
			ArticleId: int64(r.ArticleID),
			Reporter: &pb.ReportReporterInfo{
				UserId:   int64(r.UserID),
				Username: users[r.UserID].Username,
			},
			Target: &pb.ReportTargetInfo{Exists: false},
		}
		if a, ok := arts[r.ArticleID]; ok {
			item.Target = &pb.ReportTargetInfo{
				Exists:         true,
				Slug:           a.Slug,
				Title:          a.Title,
				AuthorUserId:   int64(a.UserID),
				AuthorUsername: users[a.UserID].Username,
			}
		}
		out = append(out, item)
	}
	return &pb.ReportListRes{
		Code: 0, Message: "success",
		Data: &pb.ReportListData{List: out, Total: total, Page: int64(page), PageSize: int64(pageSize)},
	}, nil
}

// handleReportHandle 处理博客举报：resolve=已处理 / dismiss=驳回（需 content.report.handle）
// ReportHandle 处理博客举报：resolve=已处理 / dismiss=驳回（需 content.report.handle）
func (s *BlogService) ReportHandle(ctx context.Context, req *pb.ReportHandleReq) (*pb.ReportHandleRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermContentReportHandle) {
		return nil, blogErr(http.StatusForbidden, "需要举报处理权限")
	}
	if req.Id == 0 {
		return nil, blogErr(http.StatusBadRequest, "参数错误")
	}
	var next string
	switch req.Action {
	case "resolve":
		next = "resolved"
	case "dismiss":
		next = "dismissed"
	default:
		return nil, blogErr(http.StatusBadRequest, "不支持的操作")
	}
	var row model.BlogReport
	if s.db.First(&row, req.Id).Error != nil {
		return nil, blogErr(http.StatusNotFound, "举报不存在")
	}
	if err := s.db.Model(&row).Update("status", next).Error; err != nil {
		return nil, blogErr(http.StatusInternalServerError, "操作失败，请稍后重试")
	}
	return &pb.ReportHandleRes{
		Code: 0, Message: "success",
		Data: &pb.ReportHandleData{Id: int64(row.ID), Status: next},
	}, nil
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
	// 博客默认明暗（读者侧可覆盖）
	blogColorLight  = "light"
	blogColorDark   = "dark"
	blogColorSystem = "system"
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
	ColorScheme string           `json:"colorScheme"`
	Subtitle    string           `json:"subtitle"`
	SocialLinks []blogSocialLink `json:"socialLinks"`
	AboutMD     string           `json:"aboutMd"`
	HomeIntroMD string           `json:"homeIntroMd"`
	FriendsMD   string           `json:"friendsMd"`
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

// normalizeColorScheme 默认 system（跟随系统）；仅接受 light|dark|system。
func normalizeColorScheme(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case blogColorLight:
		return blogColorLight
	case blogColorDark:
		return blogColorDark
	case blogColorSystem, "", "auto":
		return blogColorSystem
	default:
		return blogColorSystem
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
		ColorScheme: blogColorSystem,
		Subtitle:    "",
		SocialLinks: []blogSocialLink{},
		AboutMD:     "",
		HomeIntroMD: "",
		FriendsMD:   "",
	}
	if userID == 0 {
		return view
	}
	var cfg model.BlogSiteConfig
	if err := s.db.Where("user_id = ?", userID).First(&cfg).Error; err != nil {
		return view
	}
	view.ThemeID = normalizeThemeID(cfg.ThemeID)
	view.ColorScheme = normalizeColorScheme(cfg.ColorScheme)
	view.Subtitle = strings.TrimSpace(cfg.Subtitle)
	view.SocialLinks = parseSocialLinksJSON(cfg.SocialLinks)
	view.AboutMD = strings.ReplaceAll(cfg.AboutMD, "\r\n", "\n")
	view.HomeIntroMD = strings.ReplaceAll(cfg.HomeIntroMD, "\r\n", "\n")
	view.FriendsMD = strings.ReplaceAll(cfg.FriendsMD, "\r\n", "\n")
	return view
}

func normalizeSlotMD(raw string) (string, string) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	if len(s) > maxSlotMD {
		return "", "页面内容过大"
	}
	return s, ""
}

func (s *BlogService) loadArticleTags(articleID uint) []string {
	if articleID == 0 {
		return []string{}
	}
	var names []string
	_ = s.db.Table("blog_article_tags AS bat").
		Select("bt.name").
		Joins("JOIN blog_tags bt ON bt.id = bat.tag_id").
		Where("bat.article_id = ?", articleID).
		Order("bt.name ASC").
		Pluck("bt.name", &names).Error
	if names == nil {
		return []string{}
	}
	return names
}

// normalizeBlogTags 去重、截断；返回展示名列表。
func normalizeBlogTags(raw []string) ([]string, string) {
	if len(raw) == 0 {
		return []string{}, ""
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		name := strings.TrimSpace(t)
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) > maxBlogTagLen {
			return nil, "标签过长（最多 32 字）"
		}
		// 禁止控制字符
		if strings.ContainsAny(name, "\n\r\t") {
			return nil, "标签含非法字符"
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
		if len(out) > maxBlogTags {
			return nil, "标签最多 20 个"
		}
	}
	return out, ""
}

// replaceArticleTags 全量替换文章标签；raw 为 nil 时也清空（前端应始终传数组）。
func (s *BlogService) replaceArticleTags(articleID, userID uint, raw []string) string {
	names, msg := normalizeBlogTags(raw)
	if msg != "" {
		return msg
	}
	_ = s.db.Where("article_id = ?", articleID).Delete(&model.BlogArticleTag{}).Error
	if len(names) == 0 {
		return ""
	}
	for _, name := range names {
		lower := strings.ToLower(name)
		var tag model.BlogTag
		err := s.db.Where("user_id = ? AND name_lower = ?", userID, lower).First(&tag).Error
		if err != nil {
			tag = model.BlogTag{UserID: userID, Name: name, NameLower: lower}
			if err := s.db.Create(&tag).Error; err != nil {
				// 并发下可能已存在
				_ = s.db.Where("user_id = ? AND name_lower = ?", userID, lower).First(&tag).Error
			}
		} else if tag.Name != name {
			// 保留首次写法；不强制改大小写
		}
		if tag.ID == 0 {
			continue
		}
		_ = s.db.Create(&model.BlogArticleTag{ArticleID: articleID, TagID: tag.ID}).Error
	}
	return ""
}

type blogTagCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

func (s *BlogService) listBlogTagCounts(authorID, viewerID uint, keyword string, limit int) ([]blogTagCount, error) {
	if limit < 1 {
		limit = 12
	}
	if limit > 50 {
		limit = 50
	}
	var rows []blogTagCount
	q := s.db.Table("blog_tags AS bt").
		Select("bt.name, COUNT(DISTINCT bat.article_id) AS count").
		Joins("JOIN blog_article_tags bat ON bat.tag_id = bt.id").
		Joins("JOIN blog_articles a ON a.id = bat.article_id").
		Where("bt.user_id = ?", authorID)
	if viewerID != authorID {
		q = q.Where("a.visibility IN ?", []string{blogaccess.VisibilityPublic, blogaccess.VisibilityPassword}).
			Where("(a.moderation_status = ? OR a.moderation_status = '' OR a.moderation_status IS NULL)", model.BlogModerationApproved)
	}
	if pattern := sqllike.Pattern(keyword); pattern != "" {
		if s.db.Dialector.Name() == "postgres" {
			q = q.Where("bt.name ILIKE ?", pattern)
		} else {
			q = q.Where("LOWER(bt.name) LIKE LOWER(?)", pattern)
		}
	}
	err := q.Group("bt.id, bt.name").Having("COUNT(DISTINCT bat.article_id) > 0").
		Order("count DESC, bt.name ASC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}

// ListTagsPublic 公开：作者公开文标签聚合
func (s *BlogService) ListTagsPublic(ctx context.Context, req *pb.ListTagsPublicReq) (*pb.ListTagsPublicRes, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, blogErr(http.StatusBadRequest, "缺少用户名")
	}
	u, err := s.findUserByUsername(username)
	if err != nil {
		return nil, blogErr(http.StatusNotFound, "用户不存在")
	}
	rows, err := s.listBlogTagCounts(
		u.ID,
		blogViewerID(ctx),
		req.Keyword,
		int(req.Limit),
	)
	if err != nil {
		return nil, blogErr(http.StatusInternalServerError, "加载失败")
	}
	out := make([]*pb.TagCount, 0, len(rows))
	for _, t := range rows {
		out = append(out, &pb.TagCount{Name: t.Name, Count: t.Count})
	}
	return &pb.ListTagsPublicRes{Code: 0, Message: "success", Data: out}, nil
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

// ThemeStatus 公开：博客主题状态
func (s *BlogService) ThemeStatus(ctx context.Context, req *pb.ThemeStatusReq) (*pb.ThemeStatusRes, error) {
	username := strings.TrimSpace(req.Username)
	var userID uint
	if username != "" {
		u, err := s.findUserByUsername(username)
		if err != nil {
			return nil, blogErr(http.StatusNotFound, "用户不存在")
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
	return &pb.ThemeStatusRes{
		Code: 0, Message: "success",
		Data: &pb.ThemeStatusData{
			Enabled:     enabled,
			ThemeId:     siteCfg.ThemeID,
			ColorScheme: siteCfg.ColorScheme,
			Subtitle:    siteCfg.Subtitle,
			SocialLinks: socialLinksToProto(siteCfg.SocialLinks),
			AboutMd:     siteCfg.AboutMD,
			HomeIntroMd: siteCfg.HomeIntroMD,
			FriendsMd:   siteCfg.FriendsMD,
		},
	}, nil
}

// handleThemeConfigSave owner saves theme + slots + social links.
// ThemeConfigSave 登录：保存主题配置（themeId + social links + MD 槽位）
func (s *BlogService) ThemeConfigSave(ctx context.Context, req *pb.ThemeConfigSaveReq) (*pb.ThemeConfigSaveRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		return nil, blogErr(http.StatusUnauthorized, "请先登录")
	}
	if err := s.requireActivated(ctx, pd.UserID); err != nil {
		return nil, err
	}
	themeID := normalizeThemeID(req.ThemeId)
	colorScheme := normalizeColorScheme(req.ColorScheme)
	subtitle := strings.TrimSpace(req.Subtitle)
	if utf8.RuneCountInString(subtitle) > maxSubtitle {
		return nil, blogErr(http.StatusBadRequest, "副标题过长")
	}
	// re-validate social links via JSON round-trip
	links := make([]blogSocialLink, 0, len(req.SocialLinks))
	for _, l := range req.SocialLinks {
		if l == nil {
			continue
		}
		links = append(links, blogSocialLink{Type: l.Type, URL: l.Url, Label: l.Label})
	}
	rawLinks, _ := json.Marshal(links)
	parsed := parseSocialLinksJSON(string(rawLinks))
	// only allow http(s) / mailto for urls
	clean := make([]blogSocialLink, 0, len(parsed))
	for _, l := range parsed {
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

	var aboutMD, homeIntroMD, friendsMD string
	var haveAbout, haveHome, haveFriends bool
	if req.AboutMd != nil {
		haveAbout = true
		var msg string
		aboutMD, msg = normalizeSlotMD(req.AboutMd.Value)
		if msg != "" {
			return nil, blogErr(http.StatusBadRequest, msg)
		}
	}
	if req.HomeIntroMd != nil {
		haveHome = true
		var msg string
		homeIntroMD, msg = normalizeSlotMD(req.HomeIntroMd.Value)
		if msg != "" {
			return nil, blogErr(http.StatusBadRequest, msg)
		}
	}
	if req.FriendsMd != nil {
		haveFriends = true
		var msg string
		friendsMD, msg = normalizeSlotMD(req.FriendsMd.Value)
		if msg != "" {
			return nil, blogErr(http.StatusBadRequest, msg)
		}
	}

	var cfg model.BlogSiteConfig
	err := s.db.Where("user_id = ?", pd.UserID).First(&cfg).Error
	if err != nil {
		cfg = model.BlogSiteConfig{
			UserID:      pd.UserID,
			ThemeID:     themeID,
			ColorScheme: colorScheme,
			Subtitle:    subtitle,
			SocialLinks: string(linksJSON),
		}
		if haveAbout {
			cfg.AboutMD = aboutMD
		}
		if haveHome {
			cfg.HomeIntroMD = homeIntroMD
		}
		if haveFriends {
			cfg.FriendsMD = friendsMD
		}
		if err := s.db.Create(&cfg).Error; err != nil {
			return nil, blogErr(http.StatusInternalServerError, "保存失败")
		}
	} else {
		cfg.ThemeID = themeID
		cfg.ColorScheme = colorScheme
		cfg.Subtitle = subtitle
		cfg.SocialLinks = string(linksJSON)
		if haveAbout {
			cfg.AboutMD = aboutMD
		}
		if haveHome {
			cfg.HomeIntroMD = homeIntroMD
		}
		if haveFriends {
			cfg.FriendsMD = friendsMD
		}
		if err := s.db.Save(&cfg).Error; err != nil {
			return nil, blogErr(http.StatusInternalServerError, "保存失败")
		}
	}
	view := s.loadSiteConfig(pd.UserID)
	return &pb.ThemeConfigSaveRes{
		Code: 0, Message: "success",
		Data: &pb.ThemeConfigData{
			ThemeId:     view.ThemeID,
			ColorScheme: view.ColorScheme,
			Subtitle:    view.Subtitle,
			SocialLinks: socialLinksToProto(view.SocialLinks),
			AboutMd:     view.AboutMD,
			HomeIntroMd: view.HomeIntroMD,
			FriendsMd:   view.FriendsMD,
		},
	}, nil
}

// ThemeEnable 站管：主题开关（user|batch|all，遗留能力）
func (s *BlogService) ThemeEnable(ctx context.Context, req *pb.ThemeEnableReq) (*pb.ThemeEnableRes, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || !pd.IsSiteAdmin {
		return nil, blogErr(http.StatusForbidden, "仅站点管理员可操作")
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	switch mode {
	case "all":
		var g model.BlogThemeFlag
		err := s.db.Where("user_id = 0").First(&g).Error
		if err != nil {
			g = model.BlogThemeFlag{UserID: 0, Enabled: req.Enabled}
			_ = s.db.Create(&g).Error
		} else {
			g.Enabled = req.Enabled
			_ = s.db.Save(&g).Error
		}
	case "user":
		if req.UserId == 0 {
			return nil, blogErr(http.StatusBadRequest, "缺少 userId")
		}
		s.upsertThemeFlag(uint(req.UserId), req.Enabled)
	case "batch":
		for _, id := range req.UserIds {
			if id > 0 {
				s.upsertThemeFlag(uint(id), req.Enabled)
			}
		}
	default:
		return nil, blogErr(http.StatusBadRequest, "mode 须为 user|batch|all")
	}
	return &pb.ThemeEnableRes{Code: 0, Message: "success"}, nil
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
