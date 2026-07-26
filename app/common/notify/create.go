// Package notify 提供跨服务写入站内通知的最小能力（core → user DB）。
package notify

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// Type constants（与 user model 保持一致）
const (
	TypeProblemEditApproved = "problem_edit_approved"
	TypeProblemEditRejected = "problem_edit_rejected"
	TypeOrgJoinApproved     = "org_join_approved"
	TypeOrgJoinRejected     = "org_join_rejected"
	TypeMention             = "mention"
	TypeCommentReply        = "comment_reply"
	// 博客 / 题解互动
	TypeBlogArticleLike   = "blog_article_like"
	TypeBlogComment       = "blog_comment"
	TypeBlogCommentReply  = "blog_comment_reply"
	TypeSolutionLike      = "solution_like"
	TypeCommentLike       = "comment_like"
	TypeBlogReport        = "blog_report"
	TypeCommunityReport   = "community_report"
	// 站点运营
	TypeUserRegistered = "user_registered"
	TypeUserFrozen     = "user_frozen"
	TypeUserUnfrozen   = "user_unfrozen"
	TypeReviewPending  = "review_pending"
	// 资源审核员任命
	TypeResourceReviewerAppointed = "resource_reviewer_appointed"
	TypeResourceReviewerRevoked   = "resource_reviewer_revoked"
)

// Row 与 user.notifications 表对齐（避免 core 依赖 user 包）
type Row struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UserID    uint   `gorm:"column:user_id"`
	Type      string `gorm:"column:type"`
	Title     string `gorm:"column:title"`
	Body      string `gorm:"column:body"`
	ActorID   uint   `gorm:"column:actor_id"`
	RefType   string `gorm:"column:ref_type"`
	RefID     uint   `gorm:"column:ref_id"`
	ProblemID uint   `gorm:"column:problem_id"`
	Payload   string `gorm:"column:payload"`
	IsRead    bool   `gorm:"column:is_read"`
}

func (Row) TableName() string { return "notifications" }

// Create 写入一条通知；db 为 nil 时静默跳过
func Create(db *gorm.DB, n Row) error {
	if db == nil || n.UserID == 0 {
		return nil
	}
	n.Title = strings.TrimSpace(n.Title)
	if n.Title == "" {
		n.Title = "通知"
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now()
	}
	return db.Create(&n).Error
}

// CreateMany 批量写入（去重接收者）；改用 CreateInBatches 减少逐行往返
func CreateMany(db *gorm.DB, rows []Row) error {
	if db == nil || len(rows) == 0 {
		return nil
	}
	seen := map[uint]struct{}{}
	now := time.Now()
	batch := make([]Row, 0, len(rows))
	for _, r := range rows {
		if r.UserID == 0 {
			continue
		}
		if _, ok := seen[r.UserID]; ok {
			continue
		}
		seen[r.UserID] = struct{}{}
		r.Title = strings.TrimSpace(r.Title)
		if r.Title == "" {
			r.Title = "通知"
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = now
		}
		batch = append(batch, r)
	}
	if len(batch) == 0 {
		return nil
	}
	return db.CreateInBatches(&batch, 200).Error
}
