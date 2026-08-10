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
	// OpsNotifyEmails 运维告警邮件收件人（逗号或换行分隔）；空则不发（OJ 大面积同步出错 / 资源长期占用过高等）
	OpsNotifyEmails string `gorm:"type:text;column:ops_notify_emails;comment:运维告警邮件收件人"`
	// DataDiskPath 运维磁盘统计目录（数据盘挂载点；空=默认 /data，未挂载回退 /）
	DataDiskPath string `gorm:"size:256;column:data_disk_path;comment:运维磁盘统计目录"`
	// 又拍云图床（博客/题解）；密码经 secretutil 加密存储
	UpyunBucket   string `gorm:"size:128;column:upyun_bucket;comment:又拍云服务名"`
	UpyunOperator string `gorm:"size:128;column:upyun_operator;comment:又拍云操作员"`
	UpyunPassword string `gorm:"size:512;column:upyun_password;comment:又拍云操作员密码加密"`
	// UpyunDomain 用户侧访问域名，如 zhiyuansofts.cn 或 http://zhiyuansofts.cn
	UpyunDomain string `gorm:"size:256;column:upyun_domain;comment:又拍云加速/访问域名"`
	// UpyunScheme http | https；空则从 domain 推断，默认 http
	UpyunScheme string `gorm:"size:16;column:upyun_scheme;comment:访问协议"`
	// OJ 爬虫账号（系统级，用于爬取用户提交记录）；密码经 secretutil 加密存储
	OjLuoguUsername string `gorm:"size:128;column:oj_luogu_username;comment:洛谷爬虫账号"`
	OjLuoguPassword string `gorm:"size:512;column:oj_luogu_password;comment:洛谷爬虫密码加密"`
	OjQojUsername   string `gorm:"size:128;column:oj_qoj_username;comment:QOJ 爬虫账号"`
	OjQojPassword   string `gorm:"size:512;column:oj_qoj_password;comment:QOJ 爬虫密码加密"`
	// OJ 登录状态（由爬虫实际登录后回写：ok / fail / unchecked）
	OjLuoguStatus    string `gorm:"size:16;column:oj_luogu_status;default:unchecked;comment:洛谷登录状态"`
	OjLuoguStatusAt  int64  `gorm:"column:oj_luogu_status_at;default:0;comment:洛谷状态更新时间 unix"`
	OjLuoguErrMsg    string `gorm:"type:text;column:oj_luogu_err_msg;comment:洛谷最近错误"`
	OjQojStatus      string `gorm:"size:16;column:oj_qoj_status;default:unchecked;comment:QOJ 登录状态"`
	OjQojStatusAt    int64  `gorm:"column:oj_qoj_status_at;default:0;comment:QOJ 状态更新时间 unix"`
	OjQojErrMsg      string `gorm:"type:text;column:oj_qoj_err_msg;comment:QOJ 最近错误"`
	// AI 服务状态（由 agent/core_data 实际调用后回写）
	AgentStatus       string `gorm:"size:16;column:agent_status;default:unchecked;comment:日报模型状态"`
	AgentStatusAt     int64  `gorm:"column:agent_status_at;default:0;comment:日报模型状态更新时间 unix"`
	AgentErrMsg       string `gorm:"type:text;column:agent_err_msg;comment:日报模型最近错误"`
	AiAnalyzeStatus   string `gorm:"size:16;column:ai_analyze_status;default:unchecked;comment:题库分析状态"`
	AiAnalyzeStatusAt int64  `gorm:"column:ai_analyze_status_at;default:0;comment:题库分析状态更新时间 unix"`
	AiAnalyzeErrMsg   string `gorm:"type:text;column:ai_analyze_err_msg;comment:题库分析最近错误"`
	// SMTP 邮件服务状态（由发送邮件后回写）
	SmtpStatus   string `gorm:"size:16;column:smtp_status;default:unchecked;comment:邮件服务状态"`
	SmtpStatusAt int64  `gorm:"column:smtp_status_at;default:0;comment:邮件状态更新时间 unix"`
	SmtpErrMsg   string `gorm:"type:text;column:smtp_err_msg;comment:邮件最近错误"`
	// 支付FM（C 端订阅在线支付；聚合支付 https://docs.zhifux.com）；密钥经 secretutil 加密存储
	PayFmApiBase    string `gorm:"size:256;column:payfm_api_base;default:'';comment:支付FM接口根地址"`
	PayFmMerchantNo string `gorm:"size:64;column:payfm_merchant_no;default:'';comment:支付FM商户号"`
	PayFmSecret     string `gorm:"type:text;column:payfm_secret;default:'';comment:支付FM接入密钥(加密)"`
	// PayFmPayType 支付方式（如 aloop=支付宝轮循池；空=默认 aloop）
	PayFmPayType string `gorm:"size:32;column:payfm_pay_type;default:'';comment:支付FM支付方式"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
}

func (SiteConfig) TableName() string { return "site_configs" }
