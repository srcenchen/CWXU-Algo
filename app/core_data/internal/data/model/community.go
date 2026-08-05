package model

import "time"

// 社区互动目标类型
const (
	CommunityTargetComment  = "comment"
	CommunityTargetSolution = "solution"
)

// 评论最大嵌套深度（0=顶层，最大为 MaxCommentDepth）
const MaxCommentDepth = 3

// ProblemComment 题目/题解评论（全站可见，不做组织隔离；支持层级回复）
// SolutionID=0 为题目讨论；>0 为挂在用户题解下的评论。
type ProblemComment struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
	ProblemID uint `gorm:"not null;index:idx_pc_problem_root,priority:1;comment:题目id"`
	// SolutionID 所属用户题解；0=题目讨论
	SolutionID uint   `gorm:"not null;default:0;index:idx_pc_solution_root,priority:1;comment:题解id 0=题目讨论"`
	UserID     uint   `gorm:"not null;index;comment:作者"`
	Content    string `gorm:"type:text;not null;comment:评论内容"`
	// ParentID 直接父评论；0 表示顶层
	ParentID uint `gorm:"not null;default:0;index;comment:父评论id"`
	// RootID 所属根评论；顶层创建后 = 自身 id
	RootID uint `gorm:"not null;default:0;index:idx_pc_problem_root,priority:2;index:idx_pc_solution_root,priority:2;comment:根评论id"`
	// ReplyToUserID 被回复用户（展示「回复 @xxx」）
	ReplyToUserID uint `gorm:"not null;default:0;comment:被回复用户"`
	// Depth 层级：0 顶层
	Depth int `gorm:"not null;default:0;comment:嵌套深度"`
	// LikeCount 冗余点赞数
	LikeCount int `gorm:"not null;default:0;comment:点赞数"`
}

func (ProblemComment) TableName() string {
	return "problem_comments"
}

// ProblemUserSolution 用户题解（全站可见；与 AI SolutionsMeta 无关）
type ProblemUserSolution struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
	ProblemID uint   `gorm:"not null;index:idx_pus_problem_created,priority:1;comment:题目id"`
	UserID    uint   `gorm:"not null;index;comment:作者"`
	Title     string `gorm:"size:200;not null;comment:题解标题"`
	ContentMD string `gorm:"type:text;not null;comment:Markdown 正文"`
	// LikeCount 冗余点赞数（与镜像博客共享）
	LikeCount int `gorm:"not null;default:0;comment:点赞数"`
	// ViewCount UV 浏览量（与镜像博客共享；迁移后清零）
	ViewCount int `gorm:"not null;default:0;comment:阅读数UV"`
	// CommentCount 题解下评论数（与镜像博客共享）
	CommentCount int `gorm:"not null;default:0;comment:评论数"`
	// BlogArticleID 同步到个人博客后的文章 id（algo_user.blog_articles）；0=未同步
	BlogArticleID uint `gorm:"not null;default:0;index;comment:博客文章id"`
}

// CommunityViewUV one unique visitor per target (solution / comment not used).
type CommunityViewUV struct {
	ID         uint `gorm:"primaryKey"`
	CreatedAt  time.Time
	TargetType string `gorm:"size:16;not null;uniqueIndex:idx_cv_uv,priority:1;comment:solution"`
	TargetID   uint   `gorm:"not null;uniqueIndex:idx_cv_uv,priority:2;comment:目标id"`
	VisitorKey string `gorm:"size:64;not null;uniqueIndex:idx_cv_uv,priority:3;comment:访客键"`
}

func (CommunityViewUV) TableName() string { return "community_view_uvs" }

func (ProblemUserSolution) TableName() string {
	return "problem_user_solutions"
}

// CommunityLike 评论/题解点赞（每用户每目标一条）
type CommunityLike struct {
	ID         uint      `gorm:"primaryKey"`
	CreatedAt  time.Time `gorm:"index"`
	UserID     uint      `gorm:"not null;uniqueIndex:idx_cl_user_target,priority:1;comment:点赞用户"`
	TargetType string    `gorm:"size:16;not null;uniqueIndex:idx_cl_user_target,priority:2;comment:comment|solution"`
	TargetID   uint      `gorm:"not null;uniqueIndex:idx_cl_user_target,priority:3;index:idx_cl_target,priority:2;comment:目标id"`
}

func (CommunityLike) TableName() string {
	return "community_likes"
}

// 举报状态
const (
	ReportStatusPending  = "pending"
	ReportStatusResolved = "resolved"
	ReportStatusDismissed = "dismissed"
)

// CommunityReport 评论/题解举报
type CommunityReport struct {
	ID         uint      `gorm:"primaryKey"`
	CreatedAt  time.Time `gorm:"index"`
	UserID     uint      `gorm:"not null;uniqueIndex:idx_cr_user_target,priority:1;comment:举报人"`
	TargetType string    `gorm:"size:16;not null;uniqueIndex:idx_cr_user_target,priority:2;index:idx_cr_target,priority:1;comment:comment|solution"`
	TargetID   uint      `gorm:"not null;uniqueIndex:idx_cr_user_target,priority:3;index:idx_cr_target,priority:2;comment:目标id"`
	Reason     string    `gorm:"size:500;not null;comment:举报原因"`
	Status     string    `gorm:"size:16;not null;default:pending;index;comment:pending|resolved|dismissed"`
}

func (CommunityReport) TableName() string {
	return "community_reports"
}

// ActivityFeedType 发现页动态类型
const (
	ActivityTypeComment  = "comment"
	ActivityTypeSolution = "solution"
)

// ActivityFeed 发现页动态。
// 题解写入作者所属全部组织；评论写当前组织（可选双写公共域）。
// 列表：公共域全站聚合；私有域按组织成员筛选（不看纯公共域外人）。
type ActivityFeed struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"index:idx_af_org_created,priority:2"`
	// OrgID 内容同步到的组织（题解可多行；公共域列表跨 org 聚合去重）
	OrgID uint `gorm:"not null;index:idx_af_org_created,priority:1;comment:组织id"`
	// UserID 作者
	UserID uint `gorm:"not null;index;comment:作者"`
	// Type comment|solution（与 ref_id 复合索引供公共域 MAX(id) GROUP BY）
	Type string `gorm:"size:16;not null;index;index:idx_af_type_ref,priority:1;comment:动态类型"`
	// RefID 评论或题解 id
	RefID uint `gorm:"not null;index:idx_af_type_ref,priority:2;comment:源记录id"`
	// ProblemID 关联题目
	ProblemID uint `gorm:"not null;index;comment:题目id"`
	// Title 展示标题（题解标题 / 评论摘要）
	Title string `gorm:"size:256;comment:标题"`
	// Excerpt 摘要
	Excerpt string `gorm:"size:512;comment:摘要"`
}

func (ActivityFeed) TableName() string {
	return "activity_feeds"
}
