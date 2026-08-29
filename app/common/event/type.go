package event

type SpiderEvent struct {
	UserId  int64 `json:"user_id"`
	NeedAll bool  `json:"need_all"`
	// Platform 必填：一条消息只抓一个平台（入队侧按绑定展开）
	// 兼容旧消息：空则 consumer 侧仍可抓该用户全部绑定
	Platform string `json:"platform,omitempty"`
}

type SummaryEvent struct {
	UserId int64  `json:"user_id"`
	Type   string `json:"type"`
	// JobId 训练报告异步任务（Type=TrainingReport 时使用）
	JobId string `json:"job_id,omitempty"`
}

const (
	SummaryMailQueue = "summary_mail"
)

// ProblemFetchEvent 题面爬取任务（problem_fetch 队列）
type ProblemFetchEvent struct {
	ProblemID  uint   `json:"problem_id"`
	Platform   string `json:"platform"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
	// FallbackURLs 额外候选（如 NowCoder 比赛页 /acm/contest/{id}/{A}），题库页无权限时回退
	FallbackURLs []string `json:"fallback_urls,omitempty"`
	// Force 忽略用户题面爬取资格闸门（题单加题等主动场景）
	Force bool `json:"force,omitempty"`
	// SkipAnalyze 爬取成功后不入 AI 分析队列
	SkipAnalyze bool `json:"skip_analyze,omitempty"`
	// ActorUserID 主动触发者（题单加题等）；SkipAnalyze=false 时按该用户 AI 资格入队，绕过 submitter/6 月窗
	ActorUserID uint `json:"actor_user_id,omitempty"`
}

// ProblemAnalyzeEvent AI 打标任务（problem_analyze 队列，并发 3）
// 仅在题面 content_md 已落库后投递
type ProblemAnalyzeEvent struct {
	ProblemID uint `json:"problem_id"`
	// Force 用户主动触发（题单加题/手动入库）：跳过 6 月窗与 submitter AI 资格检查
	Force bool `json:"force,omitempty"`
}

// UserProfileEvent 用户题库画像预计算（user_profile 队列）
// 重 JOIN 在后台跑，HTTP 只读 Redis，避免高 AC 用户 504
type UserProfileEvent struct {
	UserId int64 `json:"user_id"`
	// Force 强制重建（每日全量 / 空雷达补刷）；false 时按指纹跳过未变化用户
	Force bool `json:"force,omitempty"`
	// ClaimToken 标识普通画像消息持有的 Redis pending claim；maintenance 消息为空。
	ClaimToken string `json:"claim_token,omitempty"`
	// IntentID 是能力维护 outbox 的持久意图标识。空值保留普通画像消息语义。
	IntentID string `json:"intent_id,omitempty"`
}
