package service

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/user/internal/biz/blogaccess"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

const (
	defaultPublicOrigin = "https://algo.zhiyuansofts.cn"
	defaultSiteTitle    = "GoAlgo"
	defaultSiteDesc     = "算法训练与竞赛社区：题库、博客、比赛与数据统计。"
	maxSEODescRunes     = 160
	maxSitemapArticles  = 5000
)

// seoPageCacheTTL resolvePage 结果进程内缓存 60s：
// 爬虫抓取高频重复路径时避免每次都查库（站点品牌 + 文章/题面 meta）。
const seoPageCacheTTL = 60 * time.Second

// seoPageCacheMax 缓存条目上限，超过后整体清空，防止恶意路径撑爆内存
const seoPageCacheMax = 1024

type seoPageCacheEntry struct {
	page seoPage
	exp  time.Time
}

// SEOService serves crawler-friendly HTML meta for SPA public routes.
// Does not increment blog view_count.
type SEOService struct {
	data *data.Data

	pageCacheMu sync.Mutex
	pageCache   map[string]seoPageCacheEntry
}

func NewSEOService(d *data.Data) *SEOService {
	return &SEOService{data: d, pageCache: map[string]seoPageCacheEntry{}}
}

func (s *SEOService) cachedPage(key string) (seoPage, bool) {
	s.pageCacheMu.Lock()
	defer s.pageCacheMu.Unlock()
	e, ok := s.pageCache[key]
	if !ok || time.Now().After(e.exp) {
		return seoPage{}, false
	}
	return e.page, true
}

func (s *SEOService) storePage(key string, p seoPage) {
	s.pageCacheMu.Lock()
	defer s.pageCacheMu.Unlock()
	if len(s.pageCache) >= seoPageCacheMax {
		s.pageCache = map[string]seoPageCacheEntry{}
	}
	s.pageCache[key] = seoPageCacheEntry{page: p, exp: time.Now().Add(seoPageCacheTTL)}
}

// RegisterSEORoutes public SEO endpoints (no JWT).
func RegisterSEORoutes(srv *khttp.Server, ss *SEOService) {
	r := srv.Route("/")
	r.GET("/v1/user/seo/html", ss.handleHTML)
	r.GET("/v1/user/seo/meta", ss.handleMetaJSON)
	r.GET("/v1/user/seo/sitemap.xml", ss.handleSitemap)
}

type seoPage struct {
	Title       string
	Description string
	Image       string
	URL         string
	Type        string // website | article | profile
	SiteName    string
	// Optional body for noscript / first paint
	BodyTitle string
	BodyText  string
	Author    string
}

func publicOrigin(req *http.Request) string {
	if v := strings.TrimSpace(os.Getenv("CWXU_PUBLIC_ORIGIN")); v != "" {
		return strings.TrimRight(v, "/")
	}
	// Production default: always absolute HTTPS site origin (share crawlers require https images).
	if req != nil {
		host := strings.TrimSpace(req.Header.Get("X-Forwarded-Host"))
		if host == "" {
			host = strings.TrimSpace(req.Host)
		}
		// Strip port for public site URL
		if i := strings.IndexByte(host, ':'); i > 0 && !strings.Contains(host, "]") {
			h := host[:i]
			if h == "algo.zhiyuansofts.cn" || strings.HasSuffix(h, ".zhiyuansofts.cn") {
				return "https://" + h
			}
		}
		if host == "algo.zhiyuansofts.cn" || strings.HasSuffix(host, ".zhiyuansofts.cn") {
			return "https://" + host
		}
		if xf := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto")); xf != "" && host != "" {
			// Prefer https when proto missing or internal http hop
			if xf == "https" || xf == "http" {
				if xf == "http" && (host == "algo.zhiyuansofts.cn" || strings.Contains(host, "zhiyuansofts")) {
					return "https://" + host
				}
				return xf + "://" + host
			}
		}
		if host != "" && !strings.Contains(host, "localhost") && !strings.HasPrefix(host, "127.") {
			if req.TLS != nil {
				return "https://" + host
			}
			return "https://" + host
		}
	}
	return defaultPublicOrigin
}

func (s *SEOService) siteBrand() (title, logo, favicon string) {
	title = defaultSiteTitle
	var row model.SiteConfig
	if s.data != nil && s.data.DB != nil {
		if err := s.data.DB.Select("site_title", "site_logo", "favicon").First(&row, 1).Error; err == nil {
			if t := strings.TrimSpace(row.SiteTitle); t != "" {
				title = t
			}
			logo = strings.TrimSpace(row.SiteLogo)
			favicon = strings.TrimSpace(row.Favicon)
		}
	}
	return title, logo, favicon
}

func absURL(origin, pathOrURL string) string {
	pathOrURL = strings.TrimSpace(pathOrURL)
	if pathOrURL == "" {
		return ""
	}
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	if !strings.HasPrefix(pathOrURL, "/") {
		pathOrURL = "/" + pathOrURL
	}
	return strings.TrimRight(origin, "/") + pathOrURL
}

func clipDesc(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// strip simple markdown noise for meta
	s = regexp.MustCompile("`+").ReplaceAllString(s, "")
	s = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`[#>*_~\-]+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if max <= 0 {
		max = maxSEODescRunes
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func normalizePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}
	if u, err := url.Parse(raw); err == nil {
		if u.Path != "" {
			raw = u.Path
			if u.RawQuery != "" && (strings.HasPrefix(u.Path, "/profile") || strings.Contains(u.RawQuery, "id=")) {
				// keep query for profile?id=
				raw = u.Path + "?" + u.RawQuery
			}
		}
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	// strip fragment
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

func (s *SEOService) resolvePage(req *http.Request, path string) seoPage {
	origin := publicOrigin(req)
	path = normalizePath(path)
	cacheKey := origin + "|" + path
	if p, ok := s.cachedPage(cacheKey); ok {
		return p
	}
	p := s.resolvePageUncached(req, origin, path)
	s.storePage(cacheKey, p)
	return p
}

func (s *SEOService) resolvePageUncached(req *http.Request, origin, path string) seoPage {
	siteTitle, siteLogo, siteFav := s.siteBrand()

	// split path and query
	pathOnly := path
	q := url.Values{}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		pathOnly = path[:i]
		q, _ = url.ParseQuery(path[i+1:])
	}
	pathOnly = strings.TrimRight(pathOnly, "/")
	if pathOnly == "" {
		pathOnly = "/"
	}

	defaultImg := absURL(origin, firstNonEmpty(siteLogo, siteFav, "/favicon.png"))
	page := seoPage{
		Title:       siteTitle,
		Description: defaultSiteDesc,
		Image:       defaultImg,
		URL:         absURL(origin, pathOnly),
		Type:        "website",
		SiteName:    siteTitle,
		BodyTitle:   siteTitle,
		BodyText:    defaultSiteDesc,
	}

	// /blog/:user/:slug  (article) — not manage/categories/archives/about
	if m := regexp.MustCompile(`^/blog/([^/]+)/([^/]+)$`).FindStringSubmatch(pathOnly); len(m) == 3 {
		user, slug := m[1], m[2]
		if slug != "manage" && slug != "categories" && slug != "archives" && slug != "about" {
			if p, ok := s.metaBlogArticle(origin, siteTitle, defaultImg, user, slug); ok {
				return p
			}
		}
	}

	// /blog/:user or secondary pages
	if m := regexp.MustCompile(`^/blog/([^/]+)(?:/(categories|archives|about))?$`).FindStringSubmatch(pathOnly); len(m) >= 2 {
		if p, ok := s.metaBlogHome(origin, siteTitle, defaultImg, m[1], ""); ok {
			if len(m) >= 3 && m[2] != "" {
				label := map[string]string{"categories": "分类", "archives": "归档", "about": "关于"}[m[2]]
				if label != "" {
					p.Title = label + " - " + p.Title
					p.BodyTitle = p.Title
				}
			}
			return p
		}
	}

	// /blog-plaza
	if pathOnly == "/blog-plaza" {
		page.Title = "博客广场 - " + siteTitle
		page.Description = "浏览社区公开博客与题解分享。"
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, "/blog-plaza")
		return page
	}

	// /question-bank/detail/:id/solution/:solutionId  (题解，先于题面匹配)
	if m := regexp.MustCompile(`^/question-bank/detail/(\d+)/solution/(\d+)$`).FindStringSubmatch(pathOnly); len(m) == 3 {
		pid, _ := strconv.ParseUint(m[1], 10, 64)
		sid, _ := strconv.ParseUint(m[2], 10, 64)
		if pid > 0 && sid > 0 {
			if p, ok := s.metaSolution(origin, siteTitle, defaultImg, uint(pid), uint(sid)); ok {
				return p
			}
		}
		page.Title = "题解 - " + siteTitle
		page.Description = "在 " + siteTitle + " 查看这道题的题解分享。"
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, pathOnly)
		return page
	}

	// /question-bank/detail/:id  (题面)
	if m := regexp.MustCompile(`^/question-bank/detail/(\d+)$`).FindStringSubmatch(pathOnly); len(m) == 2 {
		if id, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			if p, ok := s.metaProblem(origin, siteTitle, defaultImg, uint(id)); ok {
				return p
			}
		}
	}

	// /problemset/:id  (题单)
	if m := regexp.MustCompile(`^/problemset/(\d+)$`).FindStringSubmatch(pathOnly); len(m) == 2 {
		if id, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			if p, ok := s.metaProblemset(origin, siteTitle, defaultImg, uint(id)); ok {
				return p
			}
		}
		page.Title = "题单 - " + siteTitle
		page.Description = "在 " + siteTitle + " 浏览算法题单。"
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, pathOnly)
		return page
	}
	if pathOnly == "/problemset" {
		page.Title = "题单广场 - " + siteTitle
		page.Description = "浏览社区公开题单，系统化刷题 · " + siteTitle
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, "/problemset")
		return page
	}

	// /profile?id= or /profile/:username or /profile
	if pathOnly == "/profile" {
		if idStr := strings.TrimSpace(q.Get("id")); idStr != "" {
			if id, err := strconv.ParseUint(idStr, 10, 64); err == nil {
				if p, ok := s.metaProfile(origin, siteTitle, defaultImg, uint(id), ""); ok {
					return p
				}
			}
		}
		page.Title = "个人资料 - " + siteTitle
		page.Description = "查看选手训练数据与资料 · " + siteTitle
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, "/profile")
		return page
	}
	if m := regexp.MustCompile(`^/profile/([^/]+)$`).FindStringSubmatch(pathOnly); len(m) == 2 {
		uname := m[1]
		if id, err := strconv.ParseUint(uname, 10, 64); err == nil {
			if p, ok := s.metaProfile(origin, siteTitle, defaultImg, uint(id), ""); ok {
				return p
			}
		}
		if p, ok := s.metaProfile(origin, siteTitle, defaultImg, 0, uname); ok {
			return p
		}
		page.Title = "个人资料 - " + siteTitle
		page.URL = absURL(origin, pathOnly)
		return page
	}

	// /p/:slug paste
	if m := regexp.MustCompile(`^/p/([A-Za-z0-9]+)$`).FindStringSubmatch(pathOnly); len(m) == 2 {
		if p, ok := s.metaPaste(origin, siteTitle, defaultImg, m[1]); ok {
			return p
		}
		page.Title = "粘贴板 - " + siteTitle
		page.Description = "内容不存在或已过期 · " + siteTitle
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, pathOnly)
		return page
	}

	// /tools/paste
	if pathOnly == "/tools/paste" {
		page.Title = "粘贴板 - " + siteTitle
		page.Description = "在 " + siteTitle + " 发布与分享代码、文本片段。"
		page.BodyTitle = page.Title
		page.BodyText = page.Description
		page.URL = absURL(origin, "/tools/paste")
		return page
	}

	// home & defaults
	if pathOnly == "/" {
		page.Title = siteTitle
		page.URL = absURL(origin, "/")
		return page
	}

	// generic: keep site defaults with path canonical
	page.URL = absURL(origin, pathOnly)
	return page
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (s *SEOService) metaBlogArticle(origin, siteTitle, defaultImg, username, slug string) (seoPage, bool) {
	if s.data == nil || s.data.DB == nil {
		return seoPage{}, false
	}
	var u model.User
	if err := s.data.DB.Select("id", "username", "name", "avatar").Where("username = ?", username).First(&u).Error; err != nil {
		return seoPage{}, false
	}
	var a model.BlogArticle
	if err := s.data.DB.Where("user_id = ? AND slug = ?", u.ID, slug).First(&a).Error; err != nil {
		return seoPage{}, false
	}
	d := blogaccess.Evaluate(blogaccess.ArticleAccess{
		Visibility:  a.Visibility,
		OwnerID:     a.UserID,
		HasPassword: a.PasswordHash != "",
	}, 0, false)
	if !d.CanSeeMeta {
		return seoPage{}, false
	}

	authorName := firstNonEmpty(u.Name, u.Username)
	desc := clipDesc(a.Summary, maxSEODescRunes)
	if desc == "" && d.CanSeeBody {
		desc = clipDesc(a.Content, maxSEODescRunes)
	}
	if desc == "" {
		desc = authorName + " 的文章 · " + siteTitle
	}
	// 博文分享图：优先博主头像（保留 GoAlgo siteName）
	// cover 可能是 path-only 本站图床键，先按又拍云域名展开再 abs。
	coverExpanded := blogimg.ExpandCoverURL(a.CoverURL, blogimg.LoadUpyunClient(s.data.DB).PublicBaseURL())
	img := absURL(origin, firstNonEmpty(u.Avatar, coverExpanded, defaultImg))
	path := fmt.Sprintf("/blog/%s/%s", u.Username, a.Slug)
	title := a.Title
	if authorName != "" {
		title = a.Title + " · " + authorName
	}
	title = title + " - " + siteTitle
	return seoPage{
		Title:       title,
		Description: desc,
		Image:       img,
		URL:         absURL(origin, path),
		Type:        "article",
		SiteName:    siteTitle,
		BodyTitle:   a.Title,
		BodyText:    desc,
		Author:      authorName,
	}, true
}

func (s *SEOService) metaBlogHome(origin, siteTitle, defaultImg, username, _ string) (seoPage, bool) {
	if s.data == nil || s.data.DB == nil {
		return seoPage{}, false
	}
	var u model.User
	if err := s.data.DB.Select("id", "username", "name", "avatar").Where("username = ?", username).First(&u).Error; err != nil {
		return seoPage{}, false
	}
	authorName := firstNonEmpty(u.Name, u.Username)
	subtitle := ""
	var cfg model.BlogSiteConfig
	if err := s.data.DB.Where("user_id = ?", u.ID).First(&cfg).Error; err == nil {
		subtitle = strings.TrimSpace(cfg.Subtitle)
	}
	// 个人博客分享图：博客身份图（作者头像代表博客）
	desc := clipDesc(firstNonEmpty(subtitle, authorName+" 的算法博客，分享题解与训练笔记 · "+siteTitle), maxSEODescRunes)
	img := absURL(origin, firstNonEmpty(u.Avatar, defaultImg))
	path := "/blog/" + u.Username
	return seoPage{
		Title:       authorName + " 的博客 - " + siteTitle,
		Description: desc,
		Image:       img,
		URL:         absURL(origin, path),
		Type:        "profile",
		SiteName:    siteTitle,
		BodyTitle:   authorName + " 的博客",
		BodyText:    desc,
		Author:      authorName,
	}, true
}

func (s *SEOService) metaProfile(origin, siteTitle, defaultImg string, id uint, username string) (seoPage, bool) {
	if s.data == nil || s.data.DB == nil {
		return seoPage{}, false
	}
	var u model.User
	q := s.data.DB.Select("id", "username", "name", "avatar")
	var err error
	if id > 0 {
		err = q.First(&u, id).Error
	} else if username != "" {
		err = q.Where("username = ?", username).First(&u).Error
	} else {
		return seoPage{}, false
	}
	if err != nil {
		return seoPage{}, false
	}
	name := firstNonEmpty(u.Name, u.Username)
	desc := name + " 的个人资料 · " + siteTitle
	img := absURL(origin, firstNonEmpty(u.Avatar, defaultImg))
	path := fmt.Sprintf("/profile?id=%d", u.ID)
	return seoPage{
		Title:       name + " - " + siteTitle,
		Description: desc,
		Image:       img,
		URL:         absURL(origin, path),
		Type:        "profile",
		SiteName:    siteTitle,
		BodyTitle:   name,
		BodyText:    desc,
		Author:      name,
	}, true
}

// core problem row (read-only from CoreDB when available)
type seoProblemRow struct {
	ID        uint   `gorm:"column:id"`
	Title     string `gorm:"column:title"`
	Platform  string `gorm:"column:platform"`
	Diff      string `gorm:"column:difficulty"`
	ContentMD string `gorm:"column:content_md"`
}

func (seoProblemRow) TableName() string { return "problems" }

type seoSolutionRow struct {
	ID        uint   `gorm:"column:id"`
	ProblemID uint   `gorm:"column:problem_id"`
	UserID    uint   `gorm:"column:user_id"`
	Title     string `gorm:"column:title"`
	ContentMD string `gorm:"column:content_md"`
}

func (seoSolutionRow) TableName() string { return "problem_user_solutions" }

type seoProblemsetRow struct {
	ID          uint   `gorm:"column:id"`
	OwnerID     uint   `gorm:"column:owner_id"`
	Title       string `gorm:"column:title"`
	Description string `gorm:"column:description"`
	Visibility  string `gorm:"column:visibility"`
	ItemCount   int    `gorm:"column:item_count"`
	LikeCount   int    `gorm:"column:like_count"`
}

func (seoProblemsetRow) TableName() string { return "problemsets" }

func (s *SEOService) metaPaste(origin, siteTitle, defaultImg, slug string) (seoPage, bool) {
	if s.data == nil || s.data.DB == nil || slug == "" {
		return seoPage{}, false
	}
	var p model.Paste
	if err := s.data.DB.Where("slug = ?", slug).First(&p).Error; err != nil {
		return seoPage{}, false
	}
	if p.ExpireAt != nil && p.ExpireAt.Before(time.Now()) {
		return seoPage{}, false
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		lang := strings.TrimSpace(p.Language)
		if lang == "" || lang == "text" {
			title = "粘贴板分享"
		} else {
			title = lang + " 代码片段"
		}
	}
	desc := clipDesc(p.Content, maxSEODescRunes)
	if desc == "" {
		desc = "在 " + siteTitle + " 查看这段分享内容"
	}
	lang := strings.TrimSpace(p.Language)
	if lang != "" && lang != "text" {
		desc = lang + " · " + desc
	}
	path := "/p/" + p.Slug
	if desc == "" {
		desc = "在 " + siteTitle + " 粘贴板查看这段分享"
	} else if !strings.Contains(desc, siteTitle) {
		desc = clipDesc(desc+" · "+siteTitle, maxSEODescRunes)
	}
	return seoPage{
		Title:       title + " - " + siteTitle,
		Description: desc,
		Image:       defaultImg,
		URL:         absURL(origin, path),
		Type:        "article",
		SiteName:    siteTitle,
		BodyTitle:   title,
		BodyText:    desc,
	}, true
}

func (s *SEOService) metaProblem(origin, siteTitle, defaultImg string, id uint) (seoPage, bool) {
	if s.data == nil || s.data.CoreDB == nil {
		return seoPage{}, false
	}
	var p seoProblemRow
	if err := s.data.CoreDB.Select("id", "title", "platform", "difficulty", "content_md").First(&p, id).Error; err != nil {
		return seoPage{}, false
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = fmt.Sprintf("题目 #%d", p.ID)
	}
	parts := []string{}
	if pl := strings.TrimSpace(p.Platform); pl != "" {
		parts = append(parts, pl)
	}
	if d := strings.TrimSpace(p.Diff); d != "" {
		parts = append(parts, "难度 "+d)
	}
	bodyClip := clipDesc(p.ContentMD, 120)
	var desc string
	if bodyClip != "" {
		desc = strings.Join(append(parts, bodyClip), " · ")
	} else {
		parts = append(parts, "题面详情 · "+siteTitle)
		desc = strings.Join(parts, " · ")
	}
	desc = clipDesc(desc+" · "+siteTitle, maxSEODescRunes)
	path := fmt.Sprintf("/question-bank/detail/%d", p.ID)
	return seoPage{
		Title:       title + " - " + siteTitle,
		Description: desc,
		Image:       defaultImg,
		URL:         absURL(origin, path),
		Type:        "article",
		SiteName:    siteTitle,
		BodyTitle:   title,
		BodyText:    desc,
	}, true
}

func (s *SEOService) metaSolution(origin, siteTitle, defaultImg string, problemID, solutionID uint) (seoPage, bool) {
	if s.data == nil || s.data.CoreDB == nil || solutionID == 0 {
		return seoPage{}, false
	}
	var sol seoSolutionRow
	if err := s.data.CoreDB.Select("id", "problem_id", "user_id", "title", "content_md").
		Where("id = ?", solutionID).First(&sol).Error; err != nil {
		return seoPage{}, false
	}
	if problemID > 0 && sol.ProblemID != 0 && sol.ProblemID != problemID {
		// path problem id mismatch — still allow if solution exists
	}
	pid := sol.ProblemID
	if pid == 0 {
		pid = problemID
	}
	problemTitle := ""
	if pid > 0 {
		var pr seoProblemRow
		if err := s.data.CoreDB.Select("id", "title").First(&pr, pid).Error; err == nil {
			problemTitle = strings.TrimSpace(pr.Title)
		}
	}
	authorName := ""
	authorAvatar := ""
	if s.data.DB != nil && sol.UserID > 0 {
		var u model.User
		if err := s.data.DB.Select("id", "username", "name", "avatar").First(&u, sol.UserID).Error; err == nil {
			authorName = firstNonEmpty(u.Name, u.Username)
			authorAvatar = strings.TrimSpace(u.Avatar)
		}
	}
	title := strings.TrimSpace(sol.Title)
	if title == "" {
		title = "题解"
		if problemTitle != "" {
			title = problemTitle + " 的题解"
		}
	}
	desc := clipDesc(sol.ContentMD, maxSEODescRunes)
	if desc == "" {
		if authorName != "" && problemTitle != "" {
			desc = authorName + " 分享的「" + problemTitle + "」题解 · " + siteTitle
		} else {
			desc = "在 " + siteTitle + " 查看这道题的题解"
		}
	} else if authorName != "" {
		desc = clipDesc(authorName+" · "+desc+" · "+siteTitle, maxSEODescRunes)
	} else {
		desc = clipDesc(desc+" · "+siteTitle, maxSEODescRunes)
	}
	displayTitle := title
	if authorName != "" {
		displayTitle = title + " · " + authorName
	}
	path := fmt.Sprintf("/question-bank/detail/%d/solution/%d", pid, sol.ID)
	return seoPage{
		Title:       displayTitle + " - " + siteTitle,
		Description: desc,
		Image:       absURL(origin, firstNonEmpty(authorAvatar, defaultImg)),
		URL:         absURL(origin, path),
		Type:        "article",
		SiteName:    siteTitle,
		BodyTitle:   title,
		BodyText:    desc,
		Author:      authorName,
	}, true
}

func (s *SEOService) metaProblemset(origin, siteTitle, defaultImg string, id uint) (seoPage, bool) {
	if s.data == nil || s.data.CoreDB == nil || id == 0 {
		return seoPage{}, false
	}
	var ps seoProblemsetRow
	if err := s.data.CoreDB.Select("id", "owner_id", "title", "description", "visibility", "item_count", "like_count").
		First(&ps, id).Error; err != nil {
		return seoPage{}, false
	}
	vis := strings.TrimSpace(ps.Visibility)
	// 私有题单不暴露详情；密码题单可露标题
	if vis == "private" {
		return seoPage{}, false
	}
	ownerName := ""
	ownerAvatar := ""
	if s.data.DB != nil && ps.OwnerID > 0 {
		var u model.User
		if err := s.data.DB.Select("id", "username", "name", "avatar").First(&u, ps.OwnerID).Error; err == nil {
			ownerName = firstNonEmpty(u.Name, u.Username)
			ownerAvatar = strings.TrimSpace(u.Avatar)
		}
	}
	title := strings.TrimSpace(ps.Title)
	if title == "" {
		title = fmt.Sprintf("题单 #%d", ps.ID)
	}
	var desc string
	if vis == "password" {
		desc = "加密题单"
		if ownerName != "" {
			desc = ownerName + " 的加密题单"
		}
		desc = desc + " · 需密码访问 · " + siteTitle
	} else {
		desc = clipDesc(ps.Description, maxSEODescRunes)
		if desc == "" {
			parts := []string{}
			if ownerName != "" {
				parts = append(parts, ownerName+" 创建")
			}
			if ps.ItemCount > 0 {
				parts = append(parts, fmt.Sprintf("%d 道题", ps.ItemCount))
			}
			parts = append(parts, "公开题单 · "+siteTitle)
			desc = strings.Join(parts, " · ")
		} else {
			desc = clipDesc(desc+" · "+siteTitle, maxSEODescRunes)
		}
	}
	path := fmt.Sprintf("/problemset/%d", ps.ID)
	return seoPage{
		Title:       title + " - " + siteTitle,
		Description: desc,
		Image:       absURL(origin, firstNonEmpty(ownerAvatar, defaultImg)),
		URL:         absURL(origin, path),
		Type:        "website",
		SiteName:    siteTitle,
		BodyTitle:   title,
		BodyText:    desc,
		Author:      ownerName,
	}, true
}

// seoPathFromRequest prefers X-Original-URI (nginx) so query strings like
// /profile?id=2 are not broken by putting unescaped path into ?path=.
func seoPathFromRequest(req *http.Request) string {
	if req == nil {
		return "/"
	}
	if v := strings.TrimSpace(req.Header.Get("X-Original-URI")); v != "" {
		return v
	}
	if v := strings.TrimSpace(req.URL.Query().Get("path")); v != "" {
		return v
	}
	if v := strings.TrimSpace(req.URL.Query().Get("url")); v != "" {
		return v
	}
	return "/"
}

func (s *SEOService) handleMetaJSON(ctx khttp.Context) error {
	req := ctx.Request()
	p := s.resolvePage(req, seoPathFromRequest(req))
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "success",
		"data": map[string]interface{}{
			"title":       p.Title,
			"description": p.Description,
			"image":       p.Image,
			"url":         p.URL,
			"type":        p.Type,
			"siteName":    p.SiteName,
			"author":      p.Author,
		},
	})
	return nil
}

func (s *SEOService) handleHTML(ctx khttp.Context) error {
	req := ctx.Request()
	p := s.resolvePage(req, seoPathFromRequest(req))
	w := ctx.Response()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache SEO HTML: CDN must not serve bot page to real users.
	w.Header().Set("Cache-Control", "private, no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Vary", "User-Agent")
	w.Header().Set("X-Robots-Tag", "all")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(renderSEOHTML(p)))
	return nil
}

func renderSEOHTML(p seoPage) string {
	esc := html.EscapeString
	title := esc(p.Title)
	desc := esc(p.Description)
	img := esc(p.Image)
	pageURL := esc(p.URL)
	site := esc(p.SiteName)
	ogType := esc(firstNonEmpty(p.Type, "website"))
	bodyTitle := esc(firstNonEmpty(p.BodyTitle, p.Title))
	bodyText := esc(firstNonEmpty(p.BodyText, p.Description))
	author := esc(p.Author)

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"zh-CN\">\n<head>\n")
	b.WriteString("<meta charset=\"UTF-8\" />\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0, viewport-fit=cover\" />\n")
	b.WriteString("<meta name=\"theme-color\" content=\"#0f172a\" />\n")
	b.WriteString("<title>")
	b.WriteString(title)
	b.WriteString("</title>\n")
	b.WriteString("<meta name=\"description\" content=\"")
	b.WriteString(desc)
	b.WriteString("\" />\n")
	b.WriteString("<link rel=\"canonical\" href=\"")
	b.WriteString(pageURL)
	b.WriteString("\" />\n")
	// ?v=2 绕开 CDN 对旧图标的 immutable 缓存，与前端 index.html 保持一致
	b.WriteString("<link rel=\"icon\" href=\"/favicon.svg?v=2\" type=\"image/svg+xml\" />\n")
	b.WriteString("<link rel=\"alternate icon\" href=\"/favicon.png?v=2\" type=\"image/png\" />\n")
	b.WriteString("<link rel=\"apple-touch-icon\" href=\"/apple-touch-icon.png?v=2\" />\n")
	// Open Graph
	b.WriteString("<meta property=\"og:site_name\" content=\"")
	b.WriteString(site)
	b.WriteString("\" />\n")
	b.WriteString("<meta property=\"og:type\" content=\"")
	b.WriteString(ogType)
	b.WriteString("\" />\n")
	b.WriteString("<meta property=\"og:title\" content=\"")
	b.WriteString(title)
	b.WriteString("\" />\n")
	b.WriteString("<meta property=\"og:description\" content=\"")
	b.WriteString(desc)
	b.WriteString("\" />\n")
	b.WriteString("<meta property=\"og:url\" content=\"")
	b.WriteString(pageURL)
	b.WriteString("\" />\n")
	if img != "" {
		b.WriteString("<meta property=\"og:image\" content=\"")
		b.WriteString(img)
		b.WriteString("\" />\n")
	}
	if author != "" {
		b.WriteString("<meta property=\"article:author\" content=\"")
		b.WriteString(author)
		b.WriteString("\" />\n")
	}
	// Twitter
	b.WriteString("<meta name=\"twitter:card\" content=\"summary_large_image\" />\n")
	b.WriteString("<meta name=\"twitter:title\" content=\"")
	b.WriteString(title)
	b.WriteString("\" />\n")
	b.WriteString("<meta name=\"twitter:description\" content=\"")
	b.WriteString(desc)
	b.WriteString("\" />\n")
	if img != "" {
		b.WriteString("<meta name=\"twitter:image\" content=\"")
		b.WriteString(img)
		b.WriteString("\" />\n")
	}
	// Loading shell styles (formal; no bare text flash)
	b.WriteString(`<style>
html,body{margin:0;min-height:100%;background:#f8fafc;color:#0f172a;
  font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif}
.seo-shell{min-height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;
  padding:2rem 1.25rem;box-sizing:border-box}
.seo-card{width:100%;max-width:28rem;background:#fff;border:1px solid #e2e8f0;border-radius:1rem;
  box-shadow:0 10px 40px -12px rgba(15,23,42,.12);padding:1.75rem 1.5rem;text-align:center}
.seo-brand{display:inline-flex;align-items:center;gap:.5rem;margin-bottom:1.25rem;color:#64748b;font-size:.8125rem;font-weight:500;letter-spacing:.02em}
.seo-brand img{width:1.25rem;height:1.25rem;border-radius:.25rem;object-fit:cover}
.seo-spin{width:2rem;height:2rem;margin:0 auto 1rem;border:2.5px solid #e2e8f0;border-top-color:#0f172a;
  border-radius:50%;animation:seo-rot .7s linear infinite}
@keyframes seo-rot{to{transform:rotate(360deg)}}
.seo-title{margin:0 0 .5rem;font-size:1.125rem;font-weight:600;line-height:1.4;color:#0f172a;
  display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.seo-meta{margin:0 0 1rem;font-size:.875rem;line-height:1.55;color:#64748b;
  display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.seo-hint{margin:0;font-size:.75rem;color:#94a3b8}
.seo-actions{margin-top:1.25rem;display:flex;gap:.5rem;justify-content:center;flex-wrap:wrap}
.seo-btn{display:inline-flex;align-items:center;justify-content:center;padding:.5rem 1rem;border-radius:.5rem;
  font-size:.875rem;font-weight:500;font-family:inherit;text-decoration:none;border:1px solid #e2e8f0;color:#0f172a;background:#fff;cursor:pointer}
.seo-btn-primary{background:#0f172a;color:#fff;border-color:#0f172a}
.seo-noscript{max-width:40rem;margin:2rem auto;padding:0 1rem}
.seo-noscript h1{font-size:1.25rem;margin:0 0 .75rem}
.seo-noscript p{color:#475569;line-height:1.6}
@media (prefers-color-scheme:dark){
  html,body{background:#0b1220;color:#e2e8f0}
  .seo-card{background:#111827;border-color:#1f2937;box-shadow:0 10px 40px -12px rgba(0,0,0,.45)}
  .seo-brand,.seo-meta{color:#94a3b8}
  .seo-title{color:#f1f5f9}
  .seo-hint{color:#64748b}
  .seo-spin{border-color:#1f2937;border-top-color:#e2e8f0}
  .seo-btn{background:#111827;border-color:#334155;color:#e2e8f0}
  .seo-btn-primary{background:#e2e8f0;color:#0f172a;border-color:#e2e8f0}
}
</style>
`)
	// Boot SPA ASAP; if boot fails, show retry (外链入口：必须正确解析 /assets 绝对路径)
	b.WriteString(`<script>
(function(){
  var booted=false,failT=null,entryLoaded=false;
  function showFail(msg){
    var el=document.getElementById("seo-fail");
    if(el)el.hidden=false;
    var h=document.getElementById("seo-hint");
    if(h)h.textContent=msg||"页面加载不顺利，请点下方重试";
  }
  function absUrl(u){
    if(!u)return "";
    if(/^https?:\/\//i.test(u)||u.indexOf("//")===0)return u;
    if(u.charAt(0)==="/")return location.origin+u;
    try{return new URL(u,location.href).href;}catch(e){return u;}
  }
  function clearFailTimer(){
    if(failT){clearTimeout(failT);failT=null;}
  }
  function boot(){
    if(booted)return;
    booted=true;
    failT=setTimeout(function(){if(!entryLoaded)showFail("加载较慢，可点击下方重试");},10000);
    fetch("/index.html",{credentials:"same-origin",cache:"no-store"})
      .then(function(r){if(!r.ok)throw new Error("index "+r.status);return r.text()})
      .then(function(html){
        var doc=new DOMParser().parseFromString(html,"text/html");
        var head=document.head;
        // 样式与 modulepreload：用 getAttribute 再绝对化，避免 DOMParser 下 .href 解析异常
        doc.querySelectorAll('link[rel="stylesheet"],link[rel="modulepreload"]').forEach(function(n){
          var href=n.getAttribute("href");
          if(!href)return;
          var abs=absUrl(href);
          if(document.querySelector('link[data-seo-boot="1"][href="'+abs+'"]'))return;
          var link=document.createElement("link");
          link.setAttribute("data-seo-boot","1");
          link.rel=n.getAttribute("rel")||"stylesheet";
          var asv=n.getAttribute("as");
          if(asv)link.as=asv;
          if(n.hasAttribute("crossorigin"))link.crossOrigin=n.getAttribute("crossorigin")||"";
          link.href=abs;
          head.appendChild(link);
        });
        var scripts=Array.prototype.slice.call(doc.querySelectorAll("script[src]"));
        if(!scripts.length){showFail();return;}
        function next(i){
          if(i>=scripts.length)return;
          var s=scripts[i];
          var src=absUrl(s.getAttribute("src"));
          if(!src){next(i+1);return;}
          var el=document.createElement("script");
          var typ=s.getAttribute("type");
          if(typ)el.type=typ;
          if(s.hasAttribute("crossorigin"))el.crossOrigin=s.getAttribute("crossorigin")||"";
          el.src=src;
          el.onload=function(){
            entryLoaded=true;
            clearFailTimer();
            next(i+1);
          };
          el.onerror=function(){
            showFail("前端资源加载失败，请重试");
          };
          head.appendChild(el);
        }
        next(0);
      })
      .catch(function(){showFail("无法获取页面入口，请重试");});
  }
  window.__goalgoSeoRetry=function(){
    booted=false;
    entryLoaded=false;
    clearFailTimer();
    var fail=document.getElementById("seo-fail");
    if(fail)fail.hidden=true;
    var h=document.getElementById("seo-hint");
    if(h)h.textContent="正在进入完整页面…";
    // 去掉可能半加载的 boot 脚本/样式后重来
    document.querySelectorAll('[data-seo-boot="1"]').forEach(function(n){n.parentNode&&n.parentNode.removeChild(n);});
    boot();
  };
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",boot);
  else boot();
})();
</script>
`)
	b.WriteString("</head>\n<body>\n")
	// #root: React mounts here and replaces shell
	b.WriteString("<div id=\"root\">\n")
	b.WriteString("<div class=\"seo-shell\" data-seo-shell=\"1\">\n")
	b.WriteString("<div class=\"seo-card\">\n")
	b.WriteString("<div class=\"seo-brand\">")
	if img != "" {
		b.WriteString("<img src=\"")
		b.WriteString(img)
		b.WriteString("\" alt=\"\" width=\"20\" height=\"20\" />")
	}
	b.WriteString("<span>")
	b.WriteString(site)
	b.WriteString("</span></div>\n")
	b.WriteString("<div class=\"seo-spin\" aria-hidden=\"true\"></div>\n")
	b.WriteString("<h1 class=\"seo-title\">")
	b.WriteString(bodyTitle)
	b.WriteString("</h1>\n")
	if author != "" {
		b.WriteString("<p class=\"seo-meta\">")
		b.WriteString(author)
		b.WriteString("</p>\n")
	}
	if bodyText != "" {
		b.WriteString("<p class=\"seo-meta\">")
		b.WriteString(bodyText)
		b.WriteString("</p>\n")
	}
	b.WriteString("<p class=\"seo-hint\" id=\"seo-hint\">正在进入完整页面…</p>\n")
	// 失败时重试 boot（勿链回同一 SEO URL 造成空转）
	b.WriteString("<div class=\"seo-actions\" id=\"seo-fail\" hidden>\n")
	b.WriteString("<button type=\"button\" class=\"seo-btn seo-btn-primary\" onclick=\"window.__goalgoSeoRetry&&window.__goalgoSeoRetry()\">重试加载</button>\n")
	b.WriteString("<a class=\"seo-btn\" href=\"/\">返回首页</a>\n")
	b.WriteString("</div>\n")
	b.WriteString("</div>\n</div>\n")
	// noscript for pure crawlers without JS
	b.WriteString("<noscript>\n<div class=\"seo-noscript\">\n<h1>")
	b.WriteString(bodyTitle)
	b.WriteString("</h1>\n")
	if author != "" {
		b.WriteString("<p>")
		b.WriteString(author)
		b.WriteString("</p>\n")
	}
	b.WriteString("<p>")
	b.WriteString(bodyText)
	b.WriteString("</p>\n<p><a href=\"")
	b.WriteString(pageURL)
	b.WriteString("\">打开页面</a></p>\n</div>\n</noscript>\n")
	b.WriteString("</div>\n</body>\n</html>\n")
	return b.String()
}

func (s *SEOService) handleSitemap(ctx khttp.Context) error {
	req := ctx.Request()
	origin := publicOrigin(req)
	siteTitle, _, _ := s.siteBrand()
	_ = siteTitle

	var urls []string
	urls = append(urls, origin+"/")
	urls = append(urls, origin+"/blog-plaza")
	urls = append(urls, origin+"/question-bank")
	urls = append(urls, origin+"/contest")
	urls = append(urls, origin+"/bulletin")

	if s.data != nil && s.data.DB != nil {
		type row struct {
			Username string
			Slug     string
			Updated  time.Time
		}
		var list []row
		// public articles only; join users for username
		_ = s.data.DB.Raw(`
			SELECT u.username AS username, a.slug AS slug, a.updated_at AS updated
			FROM blog_articles a
			INNER JOIN users u ON u.id = a.user_id
			WHERE a.visibility = ?
			ORDER BY a.updated_at DESC
			LIMIT ?
		`, model.BlogVisPublic, maxSitemapArticles).Scan(&list).Error
		seenAuthor := map[string]struct{}{}
		for _, r := range list {
			if r.Username == "" || r.Slug == "" {
				continue
			}
			urls = append(urls, fmt.Sprintf("%s/blog/%s/%s", origin, url.PathEscape(r.Username), url.PathEscape(r.Slug)))
			if _, ok := seenAuthor[r.Username]; !ok {
				seenAuthor[r.Username] = struct{}{}
				urls = append(urls, fmt.Sprintf("%s/blog/%s", origin, url.PathEscape(r.Username)))
			}
		}
	}

	w := ctx.Response()
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.WriteHeader(200)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, u := range urls {
		b.WriteString("  <url><loc>")
		b.WriteString(html.EscapeString(u))
		b.WriteString("</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	_, _ = w.Write([]byte(b.String()))
	return nil
}
