package model

import "time"

type User struct {
	ID           uint `gorm:"primaryKey"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Username     string `gorm:"size:64;not null;uniqueIndex;comment:用户名"`
	Password     string `gorm:"size:255;not null;comment:bcrypt(客户端SHA256值)"`
	Avatar       string `gorm:"comment:头像"`
	Name         string `gorm:"comment:全局昵称(非真实姓名)"`
	Email        string `gorm:"size:320;not null;uniqueIndex;comment:邮箱(统一小写)"`
	GroupId      int64  `gorm:"comment:组id(兼容旧字段;组织内分组见 org_members.group_id)"`
	Group        Group  `gorm:"foreignKey:GroupId;references:ID"`
	RoleID       int    `gorm:"comment:角色ID兼容;default:0"` // 迁移后以 is_site_admin + org 为准
	IsSiteAdmin  bool   `gorm:"default:false;comment:站点管理员"`
	// IsResourceReviewer 已下线的「资源审核员」内置身份，存量已全部置 false。
	// 列保留仅为可回滚；不再读写，内容审核权限改由站点自定义角色授予。
	// Deprecated: 见 rbac_seed.go 的 rbac_drop_resource_reviewer_v1。
	IsResourceReviewer bool `gorm:"default:false;comment:已废弃(资源审核员下线)"`
	CurrentOrgID       uint `gorm:"default:0;comment:当前组织ID"`
	// EmailEnabled 个人日报邮件；默认关，且须组织 enable_ai_email 才可开
	EmailEnabled bool `gorm:"comment:个人日报邮件;default:false"`
	// EmailWeeklyEnabled 个人周报（教练/队长/组织管理员）；与日报独立
	EmailWeeklyEnabled bool `gorm:"comment:个人周报邮件;default:false"`

	// —— 公共域隐私（私人域组织内本配置不生效）——
	// PrivacyConfigured 是否已确认过隐私设置；未配置时前端强制弹窗
	PrivacyConfigured bool `gorm:"default:false;comment:已配置公共域隐私"`
	// AllowPublicProfile 公共域中是否允许他人查看个人资料（默认允许）
	AllowPublicProfile bool `gorm:"default:true;comment:公共域允许查看资料"`
	// AllowPublicFeed 是否出现在公共域动态中（默认加入）
	AllowPublicFeed bool `gorm:"default:true;comment:公共域动态可见"`

	// —— 题面流水线覆盖（null=按是否属于非公共域组织；true/false=强制）——
	// ProblemFetchEnabled 该用户近窗提交是否触发题面爬取
	ProblemFetchEnabled *bool `gorm:"comment:题面爬取覆盖 null=按组织"`
	// ProblemAIEnabled 该用户近窗提交是否触发题面 AI 分析
	ProblemAIEnabled *bool `gorm:"comment:题面AI覆盖 null=按组织"`

	// —— 定时策略覆盖（站点管理员指定；null=回落组织 MIN；优先级最高）——
	// SpiderIntervalMinOverride 爬取间隔（分钟）
	SpiderIntervalMinOverride *int `gorm:"comment:爬取间隔覆盖分钟 null=组织MIN"`
	// AISummaryIntervalMinOverride AI 总结间隔（分钟）
	AISummaryIntervalMinOverride *int `gorm:"comment:AI总结间隔覆盖分钟 null=组织MIN"`
	// DailyRefreshQuotaOverride 每日手动刷新做题记录次数覆盖（null=全局默认 2；0=禁止；>0=每日次数）
	DailyRefreshQuotaOverride *int `gorm:"comment:每日手动刷新配额覆盖 null=默认2 0=禁止"`

	// —— 活跃 / 休眠 ——
	// LastLoginAt 最近一次登录或已登录 VisitPing 触达
	LastLoginAt *time.Time `gorm:"index;comment:最近活跃时间"`
	// SyncExempt 站管手动永不休眠（跳过不活跃判定）
	SyncExempt bool `gorm:"default:false;comment:永不休眠(站管)"`
	// AdminForceDormant 站管强制冻结同步（覆盖组织/个人豁免；登录或解除后清除）
	AdminForceDormant bool `gorm:"default:false;index;comment:站管强制冻结同步"`
	// Disabled 站管禁用账号（禁止登录；后台同步一并暂停）
	Disabled bool `gorm:"default:false;index;comment:账号禁用禁止登录"`

	// —— C 端个人订阅（Plus/Pro）——
	// SubTier 订阅档位：plus|pro；空=未订阅
	SubTier string `gorm:"size:16;default:'';index;comment:订阅档 plus|pro"`
	// SubExpireAt 订阅到期时间；nil=长期/未订阅（以 SubTier 为准）
	SubExpireAt *time.Time `gorm:"index;comment:订阅到期"`
	// SubSource 订阅来源：payfm|manager
	SubSource string `gorm:"size:16;default:'';comment:订阅来源 payfm|manager"`
	// SubPendingTier 排队档位：当前档到期后生效（仅另一档可排队）；空=无排队
	SubPendingTier string `gorm:"size:16;default:'';comment:排队档 plus|pro"`
	// SubPendingDays 排队档剩余/购买天数（Pro 到期晋升时 sub_expire_at = now + 该值）
	SubPendingDays int `gorm:"default:0;comment:排队档天数"`
	// SubPendingSource 排队档来源：payfm|manager
	SubPendingSource string `gorm:"size:16;default:'';comment:排队档来源 payfm|manager"`
	// SubReminded 到期提醒标记：0=未提醒 3=已发3天提醒 1=已发1天提醒（续费/晋升后重置）
	SubReminded int `gorm:"default:0;comment:到期提醒 0/3/1"`
	// AIDailyEnabled 个人 AI 日报开关（仅 Pro 生效；默认关）
	// column 显式指定：避免 NamingStrategy 把 AIDaily 拆成 a_i_daily → a_idaily_enabled
	AIDailyEnabled bool `gorm:"column:ai_daily_enabled;default:false;comment:AI日报开关(Pro)"`
}
