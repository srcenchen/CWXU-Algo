package model

import "time"

// 站内通知类型
const (
	NotifTypeProblemEditApproved = "problem_edit_approved"
	NotifTypeProblemEditRejected = "problem_edit_rejected"
	NotifTypeOrgJoinApproved     = "org_join_approved"
	NotifTypeOrgJoinRejected     = "org_join_rejected"
	NotifTypeOrgInvited          = "org_invited"
	NotifTypeOrgInviteAccepted   = "org_invite_accepted"
	NotifTypeOrgInviteDeclined   = "org_invite_declined"
	NotifTypeMention             = "mention"
	NotifTypeBlogArticleLike     = "blog_article_like"
	NotifTypeBlogComment         = "blog_comment"
	NotifTypeBlogCommentReply    = "blog_comment_reply"
	NotifTypeBlogCommentLike     = "blog_comment_like"
	NotifTypeSolutionLike        = "solution_like"
	NotifTypeCommentLike         = "comment_like"
	NotifTypeBlogReport          = "blog_report"
	NotifTypeCommunityReport     = "community_report"
	NotifTypeUserRegistered      = "user_registered"
	NotifTypeUserFrozen          = "user_frozen"
	NotifTypeUserUnfrozen        = "user_unfrozen"
	NotifTypeReviewPending             = "review_pending"
	NotifTypeResourceReviewerAppointed = "resource_reviewer_appointed"
	NotifTypeResourceReviewerRevoked   = "resource_reviewer_revoked"
	NotifTypeImageUploadApproved       = "image_upload_approved"
	NotifTypeImageUploadRejected       = "image_upload_rejected"
	// 工单（客户中心 webhook 触发）
	NotifTypeTicketReply         = "ticket_reply"
	NotifTypeTicketStatusChanged = "ticket_status_changed"
)

// Notification 站内信（按接收用户存储）
type Notification struct {
	ID        uint      `gorm:"primaryKey"`
	CreatedAt time.Time `gorm:"index"`
	// UserID 接收者
	UserID uint `gorm:"not null;index:idx_notif_user_read,priority:1;index:idx_notif_user_created,priority:1;comment:接收用户"`
	// Type 见 NotifType*
	Type string `gorm:"size:64;not null;comment:通知类型"`
	// Title 列表标题
	Title string `gorm:"size:256;not null;comment:标题"`
	// Body 摘要正文
	Body string `gorm:"type:text;comment:正文"`
	// ActorID 触发者（审核人 / @ 发起人）
	ActorID uint `gorm:"default:0;comment:触发用户"`
	// RefType 关联实体类型 problem_edit|org_join|comment|solution
	RefType string `gorm:"size:32;comment:关联类型"`
	// RefID 关联实体 id
	RefID uint `gorm:"default:0;comment:关联id"`
	// ProblemID 可选题目
	ProblemID uint `gorm:"default:0;index;comment:题目id"`
	// Payload 扩展 JSON
	Payload string `gorm:"type:text;comment:扩展JSON"`
	// IsRead 是否已读
	IsRead bool `gorm:"default:false;index:idx_notif_user_read,priority:2;comment:已读"`
}

func (Notification) TableName() string {
	return "notifications"
}
