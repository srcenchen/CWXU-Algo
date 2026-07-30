package model

import "time"

// Blog visibility constants (mirror blogaccess).
const (
	BlogVisPublic     = "public"
	BlogVisPrivate    = "private"
	BlogVisPassword   = "password"
	BlogPageDraft     = "draft"
	BlogPagePublished = "published"
)

// BlogArticle is the single shared article entity (blog shell + main-site surfaces).
type BlogArticle struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	UserID   uint   `gorm:"not null;index:idx_blog_user_created,priority:1;uniqueIndex:idx_blog_user_slug,priority:1;comment:作者"`
	Slug     string `gorm:"size:96;not null;uniqueIndex:idx_blog_user_slug,priority:2;comment:作者内短链"`
	Title    string `gorm:"size:200;not null;comment:标题"`
	Summary  string `gorm:"size:500;comment:摘要"`
	Content  string `gorm:"type:text;not null;comment:Markdown 正文"`
	CoverURL string `gorm:"size:1024;comment:头图外链"`
	// ImageHashes JSON 数组：正文/头图引用的又拍云图 content hash（SHA-256 hex），供 GC 用。
	ImageHashes string `gorm:"type:text;comment:正文图片content hash JSON"`

	// Visibility: public | private | password
	Visibility   string `gorm:"size:16;not null;default:public;index;comment:可见性"`
	PasswordHash string `gorm:"size:255;comment:访问密码 bcrypt"`

	// Recommend: show on main-site recommend page when public.
	Recommend bool `gorm:"not null;default:false;index;comment:主站推荐"`

	// SyncToMainProfile: allow main-site profile activity to surface this article.
	SyncToMainProfile bool `gorm:"not null;default:false;comment:同步到主站资料动态"`

	CategoryID *uint `gorm:"index;comment:分类"`

	// SourceSolutionID: when set, this article was synced from a main-site problem solution.
	// Unique so one solution maps to at most one blog post.
	SourceSolutionID *uint `gorm:"uniqueIndex:idx_blog_source_solution;comment:主站题解id"`
	// SourceProblemID: problem of the linked solution (for UI routing / shared comments).
	SourceProblemID *uint `gorm:"index;comment:主站题目id"`

	// Denormalized counters for owner analytics.
	// ViewCount is UV (unique visitors) after migration; historical PV zeroed on migrate.
	ViewCount    int `gorm:"not null;default:0;comment:阅读数UV"`
	LikeCount    int `gorm:"not null;default:0;comment:点赞数"`
	CommentCount int `gorm:"not null;default:0;comment:评论数"`

	// ModerationStatus: approved | pending | rejected（站管审核公开文）
	ModerationStatus string `gorm:"size:16;not null;default:approved;index;comment:审核状态"`
	ModerationNote   string `gorm:"size:500;comment:审核备注"`
	ModeratedAt      *time.Time
	ModeratedBy      uint `gorm:"default:0;comment:审核人"`

	PublishedAt *time.Time `gorm:"index:idx_blog_user_created,priority:2;comment:发布时间"`
}

func (BlogArticle) TableName() string { return "blog_articles" }

// BlogPage is an author-owned standalone Markdown page rendered by every blog theme.
// Article slugs and page slugs live under different public route namespaces.
type BlogPage struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time

	UserID    uint   `gorm:"not null;uniqueIndex:idx_blog_page_user_slug,priority:1;index:idx_blog_page_user_nav,priority:1;comment:作者"`
	Title     string `gorm:"size:200;not null;comment:页面标题"`
	Slug      string `gorm:"size:96;not null;uniqueIndex:idx_blog_page_user_slug,priority:2;comment:作者内页面短链"`
	ContentMD string `gorm:"type:text;not null;comment:Markdown 正文"`
	// ImageHashes JSON 数组：页面正文引用图 content hash，供 GC 用。
	ImageHashes string `gorm:"type:text;comment:正文图片content hash JSON"`
	Status      string `gorm:"size:16;not null;default:draft;index;comment:draft|published"`
	ShowInNav bool   `gorm:"not null;default:false;index:idx_blog_page_user_nav,priority:2;comment:是否加入博客导航"`
	NavLabel  string `gorm:"size:64;comment:导航名称，空则使用标题"`
	NavOrder  int    `gorm:"not null;default:0;index:idx_blog_page_user_nav,priority:3;comment:导航排序"`
}

func (BlogPage) TableName() string { return "blog_pages" }

// BlogCategory is a per-user article category.
type BlogCategory struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID    uint   `gorm:"not null;uniqueIndex:idx_blog_cat_user_name,priority:1;comment:作者"`
	Name      string `gorm:"size:64;not null;uniqueIndex:idx_blog_cat_user_name,priority:2;comment:分类名"`
	SortOrder int    `gorm:"not null;default:0;comment:排序"`
	// IsDefault: 每用户至多一个（业务保证）；主站题解同步到此分类。不可删除。
	IsDefault bool `gorm:"not null;default:false;comment:默认分类"`
}

func (BlogCategory) TableName() string { return "blog_categories" }

// BlogArticleOrg marks which orgs an article is synced to.
// Product rule: private org sync auto-includes public domain (enforced in service).
type BlogArticleOrg struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	ArticleID uint `gorm:"not null;uniqueIndex:idx_blog_art_org,priority:1;comment:文章"`
	OrgID     uint `gorm:"not null;uniqueIndex:idx_blog_art_org,priority:2;index;comment:组织"`
}

func (BlogArticleOrg) TableName() string { return "blog_article_orgs" }

// BlogComment is a top-level or reply comment on an article.
// ParentID=0 为顶层；回复挂在父评论下（最大深度 3：0/1/2）。
type BlogComment struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	ArticleID uint   `gorm:"not null;index:idx_blog_cmt_art,priority:1;comment:文章"`
	UserID    uint   `gorm:"not null;index;comment:作者"`
	ParentID  uint   `gorm:"not null;default:0;index;comment:父评论"`
	Content   string `gorm:"type:text;not null;comment:内容"`
	// LikeCount 冗余计数，与 blog_comment_likes 同步。
	LikeCount int `gorm:"not null;default:0;comment:点赞数"`
}

func (BlogComment) TableName() string { return "blog_comments" }

// BlogCommentLike is one like per user per blog comment.
type BlogCommentLike struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	CommentID uint `gorm:"not null;uniqueIndex:idx_blog_cmt_like_cmt_user,priority:1;comment:评论"`
	UserID    uint `gorm:"not null;uniqueIndex:idx_blog_cmt_like_cmt_user,priority:2;comment:用户"`
}

func (BlogCommentLike) TableName() string { return "blog_comment_likes" }

// BlogLike is one like per user per article.
type BlogLike struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	ArticleID uint `gorm:"not null;uniqueIndex:idx_blog_like_art_user,priority:1;comment:文章"`
	UserID    uint `gorm:"not null;uniqueIndex:idx_blog_like_art_user,priority:2;comment:用户"`
}

func (BlogLike) TableName() string { return "blog_likes" }

// BlogArticleViewUV records one unique visitor per article (login user or visitor key).
// Linked solution↔blog shares one logical UV stream via solution-side table; pure blogs use this.
type BlogArticleViewUV struct {
	ID         uint `gorm:"primaryKey"`
	CreatedAt  time.Time
	ArticleID  uint   `gorm:"not null;uniqueIndex:idx_blog_uv_art_vis,priority:1;comment:文章"`
	VisitorKey string `gorm:"size:64;not null;uniqueIndex:idx_blog_uv_art_vis,priority:2;comment:访客键"`
}

func (BlogArticleViewUV) TableName() string { return "blog_article_view_uvs" }

// BlogReport is a user report on a blog article (for site admin review).
type BlogReport struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UserID    uint   `gorm:"not null;uniqueIndex:idx_blog_report_user_art,priority:1;comment:举报人"`
	ArticleID uint   `gorm:"not null;uniqueIndex:idx_blog_report_user_art,priority:2;index;comment:文章"`
	Reason    string `gorm:"size:500;not null;comment:原因"`
	Status    string `gorm:"size:16;not null;default:pending;index;comment:pending|resolved|dismissed"`
}

func (BlogReport) TableName() string { return "blog_reports" }

// SchemaPatch records one-shot data migrations (idempotent keys).
type SchemaPatch struct {
	Key       string    `gorm:"primaryKey;size:64"`
	AppliedAt time.Time `gorm:"not null"`
}

func (SchemaPatch) TableName() string { return "schema_patches" }

// BlogThemeFlag stores custom-theme enablement (legacy admin switch).
// UserID=0 row holds the global "all users" flag (Enabled=true means open for everyone).
// Per-user rows override global when present.
type BlogThemeFlag struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	// UserID 0 = global-all flag
	UserID  uint `gorm:"not null;uniqueIndex;comment:0=全局 否则用户"`
	Enabled bool `gorm:"not null;default:false;comment:是否开放自定义主题"`
}

func (BlogThemeFlag) TableName() string { return "blog_theme_flags" }

// Blog activation / agreement / moderation constants.
const (
	BlogAgreementVersionCurrent = "v1-cn-2026"
	BlogModerationApproved      = "approved"
	BlogModerationPending       = "pending"
	BlogModerationRejected      = "rejected"
	// Email notify strategy（默认 off）
	BlogEmailNotifyOff       = "off"
	BlogEmailNotifyImmediate = "immediate"
	BlogEmailNotifyDigest    = "digest_daily"
	BlogEmailNotifyRandom    = "random"
)

// BlogSiteConfig is per-author blog shell settings (theme + social links).
// ThemeID: mizuki (default) | chirpy | simple
// ColorScheme: light | dark | system（读者默认明暗；未设/空=system 跟随系统）
// SocialLinks: JSON array of {type,url,label?}
// Activation: AgreementAcceptedAt 非空表示已签署开通协议并激活博客。
type BlogSiteConfig struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID    uint   `gorm:"not null;uniqueIndex;comment:作者"`
	ThemeID   string `gorm:"size:32;not null;default:mizuki;comment:主题 mizuki|chirpy|simple"`
	// ColorScheme 博客默认明暗：light|dark|system（默认 system）
	ColorScheme string `gorm:"size:16;not null;default:system;comment:默认明暗 light|dark|system"`
	Subtitle    string `gorm:"size:200;comment:侧栏副标题"`
	// SocialLinks JSON: [{"type":"github","url":"https://...","label":"GitHub"},...]
	SocialLinks string `gorm:"type:text;comment:侧栏社交链接JSON"`

	// 开通协议
	ActivatedAt         *time.Time `gorm:"index;comment:开通时间"`
	AgreementVersion    string     `gorm:"size:32;comment:协议版本"`
	AgreementAcceptedAt *time.Time `gorm:"comment:协议签署时间"`

	// 互动邮件通知（默认关）
	EmailNotifyEnabled  bool   `gorm:"not null;default:false;comment:互动邮件通知"`
	EmailNotifyStrategy string `gorm:"size:32;not null;default:off;comment:off|immediate|digest_daily|random"`

	// ImageUploadEnabled 站管在 /admin/blog 授权后，该作者可上传又拍云图片（默认关）
	ImageUploadEnabled bool `gorm:"not null;default:false;comment:是否允许图片上传"`

	// 固定槽位页面（Markdown；空=前端默认）
	AboutMD     string `gorm:"type:text;comment:关于页Markdown"`
	HomeIntroMD string `gorm:"type:text;comment:首页介绍Markdown"`
	FriendsMD   string `gorm:"type:text;comment:友链页Markdown"`
}

func (BlogSiteConfig) TableName() string { return "blog_site_configs" }

// BlogTag is a per-user free-form tag (same author merges by lower name).
type BlogTag struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID    uint   `gorm:"not null;uniqueIndex:idx_blog_tag_user_lower,priority:1;comment:作者"`
	Name      string `gorm:"size:32;not null;comment:标签名"`
	// NameLower 用于大小写不敏感合并
	NameLower string `gorm:"size:32;not null;uniqueIndex:idx_blog_tag_user_lower,priority:2;comment:小写名"`
}

func (BlogTag) TableName() string { return "blog_tags" }

// BlogArticleTag is M2M between articles and tags.
type BlogArticleTag struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	ArticleID uint `gorm:"not null;uniqueIndex:idx_blog_art_tag,priority:1;index;comment:文章"`
	TagID     uint `gorm:"not null;uniqueIndex:idx_blog_art_tag,priority:2;index;comment:标签"`
}

func (BlogArticleTag) TableName() string { return "blog_article_tags" }

// BlogImageAsset 用户经又拍云上传的图片资产登记，供未引用 GC。
type BlogImageAsset struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID uint `gorm:"not null;index:idx_blog_img_user;index:idx_blog_img_user_hash,priority:1;comment:上传者"`
	// ObjectKey 又拍云对象路径：新图为 /blog/{uid}/{sha256}{ext}（内容寻址）
	ObjectKey string `gorm:"size:512;not null;uniqueIndex;comment:对象key"`
	// URL 库内多为 path-only（与 object_key 一致）；历史上可能是完整公网 URL
	URL string `gorm:"size:1024;not null;comment:访问URL"`
	// ContentHash 上传落库字节的 SHA-256 hex；GC 与插件校验主键
	ContentHash string `gorm:"size:64;index:idx_blog_img_user_hash,priority:2;comment:内容SHA256"`
	// ArticleID 最近一次关联文章（可选，GC 以 hash/正文引用为准）
	ArticleID *uint `gorm:"index;comment:关联文章"`
	// Purpose cover | content | misc
	Purpose string `gorm:"size:32;not null;default:content;comment:用途"`
}

func (BlogImageAsset) TableName() string { return "blog_image_assets" }

// BlogImageUploadRequest 作者申请又拍云图片上传权限（须填理由，站管审核）。
type BlogImageUploadRequest struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID    uint   `gorm:"not null;index:idx_blog_img_req_user_status,priority:1;comment:申请人"`
	// Reason 申请理由（必填）
	Reason string `gorm:"type:text;not null;comment:申请理由"`
	// Status pending|approved|rejected
	Status string `gorm:"size:16;not null;default:pending;index:idx_blog_img_req_user_status,priority:2;index;comment:pending|approved|rejected"`
	// ReviewNote 审核备注（驳回时建议填写）
	ReviewNote string `gorm:"type:text;comment:审核备注"`
	ReviewerID uint   `gorm:"default:0;comment:审核人"`
	ReviewedAt *time.Time
}

func (BlogImageUploadRequest) TableName() string { return "blog_image_upload_requests" }

const (
	BlogImageUploadPending  = "pending"
	BlogImageUploadApproved = "approved"
	BlogImageUploadRejected = "rejected"
)
