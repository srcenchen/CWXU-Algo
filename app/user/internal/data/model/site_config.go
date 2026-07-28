package model

import "time"

// SiteConfig 站点配置（单行 id=1）：品牌 + 业务密钥
type SiteConfig struct {
	ID        uint      `gorm:"primaryKey"`
	SiteTitle string    `gorm:"size:128;not null;default:GoAlgo"`
	SiteLogo  string    `gorm:"size:512"`
	Favicon   string    `gorm:"size:512"`
	// FooterIcp 页脚备案号，空则前端用默认
	FooterIcp string `gorm:"size:128;column:footer_icp"`
	// SMTP
	SMTPHost     string `gorm:"size:256;column:smtp_host"`
	SMTPPort     int    `gorm:"column:smtp_port;default:465"`
	SMTPUsername string `gorm:"size:256;column:smtp_username"`
	SMTPPassword string `gorm:"size:512;column:smtp_password"`
	SMTPFrom     string `gorm:"size:256;column:smtp_from"`
	// Agent（火山 Ark / 日报周报）
	AgentModel  string `gorm:"size:128;column:agent_model"`
	AgentSecret string `gorm:"size:512;column:agent_secret"`
	// 题库 AI 分析（OpenAI 兼容）
	AiAnalyzeEndpoint string `gorm:"size:512;column:ai_analyze_endpoint"`
	AiAnalyzeModel    string `gorm:"size:128;column:ai_analyze_model"`
	AiAnalyzeSecret   string `gorm:"size:512;column:ai_analyze_secret"`
	// InactiveDays 超过该天数未活跃则休眠后台任务；默认 14
	InactiveDays int `gorm:"column:inactive_days;default:14;comment:不活跃天数阈值"`
	// AdminNotifyEmails 审核/举报邮件收件人（逗号或换行分隔）；空则发给全部站管账号邮箱
	AdminNotifyEmails string `gorm:"type:text;column:admin_notify_emails;comment:审核举报邮件收件人"`
	// 又拍云图床（博客/题解）；密码经 secretutil 加密存储
	UpyunBucket   string `gorm:"size:128;column:upyun_bucket;comment:又拍云服务名"`
	UpyunOperator string `gorm:"size:128;column:upyun_operator;comment:又拍云操作员"`
	UpyunPassword string `gorm:"size:512;column:upyun_password;comment:又拍云操作员密码加密"`
	// UpyunDomain 用户侧访问域名，如 zhiyuansofts.cn 或 http://zhiyuansofts.cn
	UpyunDomain string `gorm:"size:256;column:upyun_domain;comment:又拍云加速/访问域名"`
	// UpyunScheme http | https；空则从 domain 推断，默认 http
	UpyunScheme string `gorm:"size:16;column:upyun_scheme;comment:访问协议"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime"`
}

func (SiteConfig) TableName() string { return "site_configs" }
