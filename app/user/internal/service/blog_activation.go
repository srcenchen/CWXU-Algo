package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"gorm.io/gorm"
)

// 个人博客开通协议正文（遵循中国法律与平台合规）
const blogAgreementTextCN = `GoAlgo 个人博客开通协议

生效版本：` + model.BlogAgreementVersionCurrent + `

欢迎使用 GoAlgo（以下简称「本平台」）提供的个人博客分站服务。在开通并使用个人博客前，请您仔细阅读并同意本协议全部条款。若您不同意，请勿开通。

一、服务说明
1. 个人博客是本平台主站的分站功能，账号体系与主站统一（同一登录态 / SSO）。
2. 开通后，您可在合规范围内发布文章、题解镜像、评论互动，并选择博客外观主题。
3. 本平台可根据运营与合规需要调整功能、规则或暂时中止部分能力，并将尽量通过站内公告等方式提示。

二、守法与内容规范
1. 您承诺遵守中华人民共和国法律法规、监管规定及本平台规则，不得利用博客从事违法违规活动。
2. 禁止发布、传播或链接包含下列内容的信息（包括但不限于）：
   （1）危害国家安全、泄露国家秘密、颠覆国家政权、破坏国家统一的；
   （2）煽动民族仇恨、民族歧视，破坏民族团结的；
   （3）破坏国家宗教政策，宣扬邪教和封建迷信的；
   （4）散布谣言，扰乱社会秩序，破坏社会稳定的；
   （5）散布淫秽、色情、赌博、暴力、凶杀、恐怖或教唆犯罪的；
   （6）侮辱或诽谤他人，侵害他人名誉权、隐私权、肖像权等合法权益的；
   （7）侵犯他人著作权、商标权、专利权等知识产权的；
   （8）含有虚假广告、诈骗信息、恶意软件或钓鱼链接的；
   （9）其他违反法律法规、公序良俗或本平台规则的内容。
3. 您对所发布内容的真实性、合法性、权利归属独立承担责任。

三、审核与处置
1. 本平台有权对博客内容进行审核、下架、隐藏或限制展示；必要时可暂停或关闭您的博客服务。
2. 用户可通过站内机制举报违法违规内容；本平台将依法依规处理。
3. 因您违反本协议导致的投诉、处罚或第三方索赔，由您自行承担相应责任。

四、个人信息与数据
1. 处理个人信息将遵循适用的个人信息保护法律法规及本平台隐私政策。
2. 博客内容可能被搜索引擎、分享预览等公开抓取；请勿发布不宜公开的个人信息。
3. 您可按产品能力管理、删除自己的文章；法律法规另有规定的除外。

五、账号与安全
1. 请妥善保管账号与密码；因保管不善导致的损失由您自行承担。
2. 不得出租、出借、售卖账号，不得利用自动化手段滥用接口或干扰服务。

六、免责与责任限制
1. 在法律允许范围内，因不可抗力、网络故障、第三方服务异常等导致的服务中断或数据丢失，本平台将尽力恢复，但不承担由此产生的间接损失。
2. 本平台不对用户之间或用户与第三方之间的纠纷承担保证责任，但可依法提供必要协助。

七、协议变更与终止
1. 本平台可更新本协议；重大变更将通过合理方式提示。继续使用视为接受更新后的协议。
2. 您可停止使用博客服务；本平台亦可在您严重违约时终止服务。

八、适用法律与争议解决
1. 本协议适用中华人民共和国法律（不含冲突法）。
2. 因本协议产生的争议，双方应友好协商；协商不成的，提交本平台运营主体所在地有管辖权的人民法院诉讼解决。

点击「同意并开通」即表示您已阅读、理解并同意受本协议约束。
`

func (s *BlogService) isBlogActivated(userID uint) bool {
	if userID == 0 {
		return false
	}
	var cfg model.BlogSiteConfig
	if err := s.db.Select("id", "agreement_accepted_at").Where("user_id = ?", userID).First(&cfg).Error; err != nil {
		return false
	}
	return cfg.AgreementAcceptedAt != nil
}

func (s *BlogService) requireActivated(ctx khttp.Context, userID uint) bool {
	if s.isBlogActivated(userID) {
		return true
	}
	writeJSON(ctx.Response(), 403, map[string]interface{}{
		"code": 1, "message": "请先签署开通协议后再使用博客功能",
		"needAgreement": true,
	})
	return false
}

func normalizeEmailNotifyStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case model.BlogEmailNotifyImmediate:
		return model.BlogEmailNotifyImmediate
	case model.BlogEmailNotifyDigest, "digest", "daily":
		return model.BlogEmailNotifyDigest
	case model.BlogEmailNotifyRandom:
		return model.BlogEmailNotifyRandom
	default:
		return model.BlogEmailNotifyOff
	}
}

func (s *BlogService) activationJSON(cfg *model.BlogSiteConfig, username string) map[string]interface{} {
	activated := cfg != nil && cfg.AgreementAcceptedAt != nil
	out := map[string]interface{}{
		"activated":           activated,
		"agreementVersion":    model.BlogAgreementVersionCurrent,
		"needAgreement":       !activated,
		"emailNotifyEnabled":  false,
		"emailNotifyStrategy": model.BlogEmailNotifyOff,
	}
	if cfg != nil {
		if cfg.AgreementVersion != "" {
			out["signedAgreementVersion"] = cfg.AgreementVersion
		}
		if cfg.AgreementAcceptedAt != nil {
			out["agreementAcceptedAt"] = cfg.AgreementAcceptedAt.Unix()
		}
		if cfg.ActivatedAt != nil {
			out["activatedAt"] = cfg.ActivatedAt.Unix()
		}
		out["emailNotifyEnabled"] = cfg.EmailNotifyEnabled
		out["emailNotifyStrategy"] = normalizeEmailNotifyStrategy(cfg.EmailNotifyStrategy)
		out["themeId"] = normalizeThemeID(cfg.ThemeID)
		out["subtitle"] = strings.TrimSpace(cfg.Subtitle)
	}
	if username != "" {
		out["username"] = username
	}
	return out
}

// handleAgreementGet 返回协议正文与当前用户开通状态（可匿名看正文）
func (s *BlogService) handleAgreementGet(ctx khttp.Context) error {
	viewer := blogViewerID(ctx)
	var cfg *model.BlogSiteConfig
	username := ""
	if viewer > 0 {
		var row model.BlogSiteConfig
		if err := s.db.Where("user_id = ?", viewer).First(&row).Error; err == nil {
			cfg = &row
		}
		var u model.User
		if s.db.Select("username").First(&u, viewer).Error == nil {
			username = u.Username
		}
	}
	data := s.activationJSON(cfg, username)
	data["title"] = "GoAlgo 个人博客开通协议"
	data["content"] = blogAgreementTextCN
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success", "data": data,
	})
	return nil
}

// handleActivationStatus 当前登录用户开通状态
func (s *BlogService) handleActivationStatus(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var cfg *model.BlogSiteConfig
	var row model.BlogSiteConfig
	if err := s.db.Where("user_id = ?", pd.UserID).First(&row).Error; err == nil {
		cfg = &row
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": s.activationJSON(cfg, pd.Username),
	})
	return nil
}

// handleActivate 签署协议并开通博客
func (s *BlogService) handleActivate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	var body struct {
		Accept            bool   `json:"accept"`
		AgreementVersion  string `json:"agreementVersion"`
		EmailNotifyEnabled *bool `json:"emailNotifyEnabled"`
		EmailNotifyStrategy string `json:"emailNotifyStrategy"`
	}
	_ = json.NewDecoder(ctx.Request().Body).Decode(&body)
	if !body.Accept {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code": 1, "message": "须勾选同意开通协议后才能开通博客",
		})
		return nil
	}
	ver := strings.TrimSpace(body.AgreementVersion)
	if ver == "" {
		ver = model.BlogAgreementVersionCurrent
	}
	if ver != model.BlogAgreementVersionCurrent {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code": 1, "message": "协议版本已更新，请刷新后重新阅读并同意",
		})
		return nil
	}
	now := time.Now()
	var cfg model.BlogSiteConfig
	err := s.db.Where("user_id = ?", pd.UserID).First(&cfg).Error
	if err != nil {
		cfg = model.BlogSiteConfig{
			UserID:              pd.UserID,
			ThemeID:             blogThemeMizuki,
			SocialLinks:         "[]",
			EmailNotifyStrategy: model.BlogEmailNotifyOff,
		}
	}
	// 已开通则仅可更新邮件偏好（仍要求 accept 时幂等）
	if cfg.AgreementAcceptedAt == nil {
		cfg.AgreementVersion = ver
		cfg.AgreementAcceptedAt = &now
		if cfg.ActivatedAt == nil {
			cfg.ActivatedAt = &now
		}
	}
	if body.EmailNotifyEnabled != nil {
		cfg.EmailNotifyEnabled = *body.EmailNotifyEnabled
	}
	if body.EmailNotifyStrategy != "" {
		cfg.EmailNotifyStrategy = normalizeEmailNotifyStrategy(body.EmailNotifyStrategy)
	}
	if !cfg.EmailNotifyEnabled {
		cfg.EmailNotifyStrategy = model.BlogEmailNotifyOff
	} else if cfg.EmailNotifyStrategy == model.BlogEmailNotifyOff {
		cfg.EmailNotifyStrategy = model.BlogEmailNotifyImmediate
	}
	if cfg.ID == 0 {
		if err := s.db.Create(&cfg).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "开通失败"})
			return nil
		}
	} else {
		if err := s.db.Save(&cfg).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "开通失败"})
			return nil
		}
	}
	// 确保默认分类存在
	_, _ = blogsyncEnsureDefaultCategory(s.db, pd.UserID)

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "已开通个人博客",
		"data": s.activationJSON(&cfg, pd.Username),
	})
	return nil
}

// 轻量：确保默认分类（避免循环依赖 blogsync 包的复杂逻辑）
func blogsyncEnsureDefaultCategory(db *gorm.DB, userID uint) (uint, error) {
	var cat model.BlogCategory
	err := db.Where("user_id = ? AND is_default = ?", userID, true).First(&cat).Error
	if err == nil {
		return cat.ID, nil
	}
	cat = model.BlogCategory{UserID: userID, Name: "默认", SortOrder: 0, IsDefault: true}
	if err := db.Create(&cat).Error; err != nil {
		// 并发下可能已有同名
		_ = db.Where("user_id = ? AND is_default = ?", userID, true).First(&cat).Error
		if cat.ID > 0 {
			return cat.ID, nil
		}
		return 0, err
	}
	return cat.ID, nil
}

// handleNotifyPref 更新互动邮件通知偏好（默认关）
func (s *BlogService) handleNotifyPref(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil || pd.UserID == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"code": 1, "message": "请先登录"})
		return nil
	}
	if !s.requireActivated(ctx, pd.UserID) {
		return nil
	}
	var body struct {
		EmailNotifyEnabled  *bool  `json:"emailNotifyEnabled"`
		EmailNotifyStrategy string `json:"emailNotifyStrategy"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	var cfg model.BlogSiteConfig
	if err := s.db.Where("user_id = ?", pd.UserID).First(&cfg).Error; err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请先开通博客"})
		return nil
	}
	if body.EmailNotifyEnabled != nil {
		cfg.EmailNotifyEnabled = *body.EmailNotifyEnabled
	}
	if body.EmailNotifyStrategy != "" {
		cfg.EmailNotifyStrategy = normalizeEmailNotifyStrategy(body.EmailNotifyStrategy)
	}
	if !cfg.EmailNotifyEnabled {
		cfg.EmailNotifyStrategy = model.BlogEmailNotifyOff
	} else if cfg.EmailNotifyStrategy == model.BlogEmailNotifyOff {
		cfg.EmailNotifyStrategy = model.BlogEmailNotifyImmediate
	}
	if err := s.db.Save(&cfg).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"emailNotifyEnabled":  cfg.EmailNotifyEnabled,
			"emailNotifyStrategy": normalizeEmailNotifyStrategy(cfg.EmailNotifyStrategy),
		},
	})
	return nil
}

// ---------- 站管：博客管理 ----------

func (s *BlogService) handleAdminOverview(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客数据看板权限"})
		return nil
	}
	var activated int64
	_ = s.db.Model(&model.BlogSiteConfig{}).Where("agreement_accepted_at IS NOT NULL").Count(&activated).Error
	var articles, views, likes, comments int64
	_ = s.db.Model(&model.BlogArticle{}).Count(&articles).Error
	type sumRow struct {
		Views    int64
		Likes    int64
		Comments int64
	}
	var sum sumRow
	_ = s.db.Model(&model.BlogArticle{}).
		Select("COALESCE(SUM(view_count),0) as views, COALESCE(SUM(like_count),0) as likes, COALESCE(SUM(comment_count),0) as comments").
		Scan(&sum).Error
	views, likes, comments = sum.Views, sum.Likes, sum.Comments
	var pending int64
	_ = s.db.Model(&model.BlogArticle{}).Where("moderation_status = ?", model.BlogModerationPending).Count(&pending).Error
	var rejected int64
	_ = s.db.Model(&model.BlogArticle{}).Where("moderation_status = ?", model.BlogModerationRejected).Count(&rejected).Error

	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"activatedUsers": activated,
			"totalArticles":  articles,
			"totalViews":     views,
			"totalLikes":     likes,
			"totalComments":  comments,
			"pendingReview":  pending,
			"rejected":       rejected,
		},
	})
	return nil
}

func (s *BlogService) handleAdminAuthors(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客数据看板权限"})
		return nil
	}
	page, pageSize := parsePage(ctx.Request())
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))

	// 以 site_config 中已签署协议的用户为主
	type row struct {
		UserID              uint
		Username            string
		Name                string
		Avatar              string
		ActivatedAt         *time.Time
		AgreementAcceptedAt *time.Time
		AgreementVersion    string
		EmailNotifyEnabled  bool
		EmailNotifyStrategy string
		ThemeID             string
		ArticleCount        int64
		ViewCount           int64
		LikeCount           int64
		CommentCount        int64
	}
	q := s.db.Table("blog_site_configs AS c").
		Select(`c.user_id, u.username, u.name, u.avatar,
			c.activated_at, c.agreement_accepted_at, c.agreement_version,
			c.email_notify_enabled, c.email_notify_strategy, c.theme_id,
			(SELECT COUNT(*) FROM blog_articles a WHERE a.user_id = c.user_id) AS article_count,
			(SELECT COALESCE(SUM(view_count),0) FROM blog_articles a WHERE a.user_id = c.user_id) AS view_count,
			(SELECT COALESCE(SUM(like_count),0) FROM blog_articles a WHERE a.user_id = c.user_id) AS like_count,
			(SELECT COALESCE(SUM(comment_count),0) FROM blog_articles a WHERE a.user_id = c.user_id) AS comment_count`).
		Joins("JOIN users u ON u.id = c.user_id").
		Where("c.agreement_accepted_at IS NOT NULL")
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("u.username ILIKE ? OR u.name ILIKE ?", like, like)
	}
	var total int64
	countQ := s.db.Table("blog_site_configs AS c").
		Joins("JOIN users u ON u.id = c.user_id").
		Where("c.agreement_accepted_at IS NOT NULL")
	if keyword != "" {
		like := "%" + keyword + "%"
		countQ = countQ.Where("u.username ILIKE ? OR u.name ILIKE ?", like, like)
	}
	_ = countQ.Count(&total).Error

	var list []row
	_ = q.Order("c.activated_at DESC NULLS LAST, c.id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Scan(&list).Error

	out := make([]map[string]interface{}, 0, len(list))
	for _, r := range list {
		item := map[string]interface{}{
			"userId":              r.UserID,
			"username":            r.Username,
			"name":                r.Name,
			"avatar":              r.Avatar,
			"agreementVersion":    r.AgreementVersion,
			"emailNotifyEnabled":  r.EmailNotifyEnabled,
			"emailNotifyStrategy": normalizeEmailNotifyStrategy(r.EmailNotifyStrategy),
			"themeId":             normalizeThemeID(r.ThemeID),
			"articleCount":        r.ArticleCount,
			"viewCount":           r.ViewCount,
			"likeCount":           r.LikeCount,
			"commentCount":        r.CommentCount,
			"activated":           true,
		}
		if r.ActivatedAt != nil {
			item["activatedAt"] = r.ActivatedAt.Unix()
		}
		if r.AgreementAcceptedAt != nil {
			item["agreementAcceptedAt"] = r.AgreementAcceptedAt.Unix()
		}
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

func (s *BlogService) handleAdminArticles(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermSiteBlogBoard) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客数据看板权限"})
		return nil
	}
	page, pageSize := parsePage(ctx.Request())
	keyword := strings.TrimSpace(ctx.Request().URL.Query().Get("keyword"))
	status := strings.TrimSpace(ctx.Request().URL.Query().Get("status"))
	visibility := strings.TrimSpace(ctx.Request().URL.Query().Get("visibility"))

	q := s.db.Model(&model.BlogArticle{})
	if status != "" {
		q = q.Where("moderation_status = ?", status)
	}
	if visibility != "" {
		q = q.Where("visibility = ?", visibility)
	}
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("title ILIKE ? OR summary ILIKE ?", like, like)
	}
	var total int64
	_ = q.Count(&total).Error
	var list []model.BlogArticle
	_ = q.Order("id DESC").Offset((page-1)*pageSize).Limit(pageSize).Find(&list).Error

	userIDs := make([]uint, 0, len(list))
	for _, a := range list {
		userIDs = append(userIDs, a.UserID)
	}
	authors := map[uint]model.User{}
	if len(userIDs) > 0 {
		var users []model.User
		_ = s.db.Select("id", "username", "name", "avatar").Where("id IN ?", userIDs).Find(&users).Error
		for _, u := range users {
			authors[u.ID] = u
		}
	}
	out := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		a := &list[i]
		author := authors[a.UserID]
		item := map[string]interface{}{
			"id":               a.ID,
			"slug":             a.Slug,
			"title":            a.Title,
			"summary":          a.Summary,
			"visibility":       a.Visibility,
			"recommend":        a.Recommend,
			"viewCount":        a.ViewCount,
			"likeCount":        a.LikeCount,
			"commentCount":     a.CommentCount,
			"moderationStatus": normalizeModeration(a.ModerationStatus),
			"moderationNote":   a.ModerationNote,
			"userId":           a.UserID,
			"username":         author.Username,
			"authorName":       author.Name,
			"createdAt":        a.CreatedAt.Unix(),
		}
		if a.PublishedAt != nil {
			item["publishedAt"] = a.PublishedAt.Unix()
		}
		if a.ModeratedAt != nil {
			item["moderatedAt"] = a.ModeratedAt.Unix()
		}
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

func normalizeModeration(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case model.BlogModerationPending:
		return model.BlogModerationPending
	case model.BlogModerationRejected:
		return model.BlogModerationRejected
	default:
		return model.BlogModerationApproved
	}
}

func (s *BlogService) handleAdminModerate(ctx khttp.Context) error {
	pd := auth.GetCurrentUser(ctx)
	if !auth.PayloadHasPerm(pd, rbac.PermContentBlogModerate) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"code": 1, "message": "需要博客审核权限"})
		return nil
	}
	var body struct {
		ID     uint   `json:"id"`
		Action string `json:"action"` // approve | reject | pending | feature | unfeature
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(ctx.Request().Body).Decode(&body); err != nil || body.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "参数错误"})
		return nil
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	var a model.BlogArticle
	if err := s.db.First(&a, body.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"code": 1, "message": "文章不存在"})
		return nil
	}

	// 精选开关：不改审核状态
	if action == "feature" || action == "unfeature" || action == "recommend" || action == "unrecommend" {
		want := action == "feature" || action == "recommend"
		if want {
			if strings.TrimSpace(a.Visibility) != model.BlogVisPublic {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "仅公开文章可设为精选"})
				return nil
			}
			if normalizeModeration(a.ModerationStatus) != model.BlogModerationApproved {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "请先通过审核再设精选"})
				return nil
			}
		}
		a.Recommend = want
		if err := s.db.Model(&a).Update("recommend", want).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "操作失败"})
			return nil
		}
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"code": 0, "message": "success",
			"data": map[string]interface{}{
				"id": a.ID, "recommend": want,
				"moderationStatus": normalizeModeration(a.ModerationStatus),
			},
		})
		return nil
	}

	status := ""
	switch action {
	case "approve", "approved":
		status = model.BlogModerationApproved
	case "reject", "rejected":
		status = model.BlogModerationRejected
	case "pending":
		status = model.BlogModerationPending
	default:
		writeJSON(ctx.Response(), 400, map[string]interface{}{"code": 1, "message": "action 须为 approve|reject|pending|feature|unfeature"})
		return nil
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > 500 {
		note = string([]rune(note)[:500])
	}
	now := time.Now()
	a.ModerationStatus = status
	a.ModerationNote = note
	a.ModeratedAt = &now
	a.ModeratedBy = pd.UserID
	// 驳回时取消精选
	if status == model.BlogModerationRejected {
		a.Recommend = false
	}
	if err := s.db.Save(&a).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"code": 1, "message": "审核失败"})
		return nil
	}
	// 通知作者
	title := "博客文章审核结果"
	bodyText := "你的文章《" + a.Title + "》"
	switch status {
	case model.BlogModerationApproved:
		bodyText += "已通过审核"
	case model.BlogModerationRejected:
		bodyText += "未通过审核"
		if note != "" {
			bodyText += "：" + note
		}
	default:
		bodyText += "已标记为待审核"
	}
	if a.UserID != pd.UserID {
		var author model.User
		_ = s.db.Select("id", "username").First(&author, a.UserID).Error
		_ = CreateNotification(s.db, model.Notification{
			UserID:  a.UserID,
			Type:    "blog_moderation",
			Title:   title,
			Body:    bodyText,
			ActorID: pd.UserID,
			RefType: "blog_article",
			RefID:   a.ID,
			Payload: mustJSON(map[string]interface{}{
				"slug": a.Slug, "blogSlug": a.Slug, "blogUsername": author.Username,
				"moderationStatus": status,
			}),
		})
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0, "message": "success",
		"data": map[string]interface{}{
			"id": a.ID, "moderationStatus": status, "moderationNote": note,
		},
	})
	return nil
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// moderationVisibleToPublic 非作者：仅 approved 可在公开列表/详情展示
func moderationVisibleToPublic(status string) bool {
	return normalizeModeration(status) == model.BlogModerationApproved
}

// parseUintQuery helper
func parseUintQuery(ctx khttp.Context, key string) uint {
	v, _ := strconv.ParseUint(strings.TrimSpace(ctx.Request().URL.Query().Get(key)), 10, 64)
	return uint(v)
}
