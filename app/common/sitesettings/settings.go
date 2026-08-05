package sitesettings

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/mail"
	secretutil "cwxu-algo/app/common/utils/secret"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	RedisKey = "site:runtime_config:v1"
	// RedisTTL 跨服务共享 SMTP/AI 配置缓存。user 服务定时刷新 + 管理员更新即时覆盖。
	// 过短会导致 core_data/agent 在 user 未回填时读不到 SMTP，邮件静默跳过。
	RedisTTL = 24 * time.Hour
)

// Runtime 跨服务共享的运行时配置（可 JSON 缓存到 Redis）
type Runtime struct {
	SiteTitle         string `json:"siteTitle"`
	SMTPHost          string `json:"smtpHost"`
	SMTPPort          int    `json:"smtpPort"`
	SMTPUsername      string `json:"smtpUsername"`
	SMTPPassword      string `json:"smtpPassword"`
	SMTPFrom          string `json:"smtpFrom"`
	AgentModel        string `json:"agentModel"`
	AgentSecret       string `json:"agentSecret"`
	AiAnalyzeEndpoint string `json:"aiAnalyzeEndpoint"`
	AiAnalyzeModel    string `json:"aiAnalyzeModel"`
	AiAnalyzeSecret   string `json:"aiAnalyzeSecret"`
	OjLuoguUsername   string `json:"ojLuoguUsername"`
	OjLuoguPassword   string `json:"ojLuoguPassword"`
	OjQojUsername     string `json:"ojQojUsername"`
	OjQojPassword     string `json:"ojQojPassword"`
}

// Row 与 site_configs 表对齐（轻量，避免依赖 user/internal）
type Row struct {
	ID                uint   `gorm:"primaryKey"`
	SiteTitle         string `gorm:"column:site_title"`
	SMTPHost          string `gorm:"column:smtp_host"`
	SMTPPort          int    `gorm:"column:smtp_port"`
	SMTPUsername      string `gorm:"column:smtp_username"`
	SMTPPassword      string `gorm:"column:smtp_password"`
	SMTPFrom          string `gorm:"column:smtp_from"`
	AgentModel        string `gorm:"column:agent_model"`
	AgentSecret       string `gorm:"column:agent_secret"`
	AiAnalyzeEndpoint string `gorm:"column:ai_analyze_endpoint"`
	AiAnalyzeModel    string `gorm:"column:ai_analyze_model"`
	AiAnalyzeSecret   string `gorm:"column:ai_analyze_secret"`
	OjLuoguUsername   string `gorm:"column:oj_luogu_username"`
	OjLuoguPassword   string `gorm:"column:oj_luogu_password"`
	OjQojUsername     string `gorm:"column:oj_qoj_username"`
	OjQojPassword     string `gorm:"column:oj_qoj_password"`
}

func (Row) TableName() string { return "site_configs" }

func (r *Row) ToRuntime() *Runtime {
	if r == nil {
		return &Runtime{}
	}
	port := r.SMTPPort
	if port <= 0 {
		port = 465
	}
	title := strings.TrimSpace(r.SiteTitle)
	if title == "" {
		title = "GoAlgo"
	}
	decrypt := func(value string) string {
		plain, err := secretutil.Decrypt(value)
		if err != nil {
			return ""
		}
		return plain
	}
	return &Runtime{
		SiteTitle:         title,
		SMTPHost:          strings.TrimSpace(r.SMTPHost),
		SMTPPort:          port,
		SMTPUsername:      strings.TrimSpace(r.SMTPUsername),
		SMTPPassword:      decrypt(r.SMTPPassword),
		SMTPFrom:          strings.TrimSpace(r.SMTPFrom),
		AgentModel:        strings.TrimSpace(r.AgentModel),
		AgentSecret:       decrypt(r.AgentSecret),
		AiAnalyzeEndpoint: strings.TrimSpace(r.AiAnalyzeEndpoint),
		AiAnalyzeModel:    strings.TrimSpace(r.AiAnalyzeModel),
		AiAnalyzeSecret:   decrypt(r.AiAnalyzeSecret),
		OjLuoguUsername:   strings.TrimSpace(r.OjLuoguUsername),
		OjLuoguPassword:   decrypt(r.OjLuoguPassword),
		OjQojUsername:     strings.TrimSpace(r.OjQojUsername),
		OjQojPassword:     decrypt(r.OjQojPassword),
	}
}

// LoadFromDB 读 id=1
func LoadFromDB(db *gorm.DB) (*Runtime, error) {
	if db == nil {
		return &Runtime{}, nil
	}
	var row Row
	if err := db.First(&row, 1).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return &Runtime{SiteTitle: "GoAlgo"}, nil
		}
		return nil, err
	}
	return row.ToRuntime(), nil
}

// HasSMTP 是否具备可用的 SMTP host（密码等由 MailSender 再校验）
func (rt *Runtime) HasSMTP() bool {
	return rt != nil && strings.TrimSpace(rt.SMTPHost) != ""
}

// PublishRedis 写入 Redis 缓存（user 服务启动 / 管理员更新 / 定时刷新的显式发布）。
// 空配置也会写入，以便管理员清空 SMTP 后立即生效。
// 注意：core_data/agent 的 Load 自动回填路径不得对「空 Runtime」调用本函数（见 Load）。
func PublishRedis(ctx context.Context, rdb *redis.Client, rt *Runtime) error {
	if rdb == nil || rt == nil {
		return nil
	}
	b, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, RedisKey, b, RedisTTL).Err()
}

// worthCaching 空 Runtime 不得回写 Redis（agent/core_data 误用错误库时会制造毒缓存）
func (rt *Runtime) worthCaching() bool {
	if rt == nil {
		return false
	}
	if strings.TrimSpace(rt.SMTPHost) != "" {
		return true
	}
	if strings.TrimSpace(rt.AiAnalyzeEndpoint) != "" {
		return true
	}
	if strings.TrimSpace(rt.AgentModel) != "" || strings.TrimSpace(rt.AgentSecret) != "" {
		return true
	}
	if strings.TrimSpace(rt.OjLuoguUsername) != "" || strings.TrimSpace(rt.OjQojUsername) != "" {
		return true
	}
	return false
}

// LoadFromRedis
func LoadFromRedis(ctx context.Context, rdb *redis.Client) (*Runtime, error) {
	if rdb == nil {
		return nil, redis.Nil
	}
	b, err := rdb.Get(ctx, RedisKey).Bytes()
	if err != nil {
		return nil, err
	}
	var rt Runtime
	if err := json.Unmarshal(b, &rt); err != nil {
		return nil, err
	}
	// 历史毒缓存（空 SMTP 且无其它业务字段）：当 miss，迫使走 DB / 等待 user 回填
	if !rt.worthCaching() {
		return nil, redis.Nil
	}
	return &rt, nil
}

// Load 优先 Redis，失败再读 DB；仅当 DB 配置有意义时才回填 Redis。
// 注意：core_data / agent 的 DB 没有 site_configs，应传 db=nil，只读 Redis。
func Load(ctx context.Context, rdb *redis.Client, db *gorm.DB) *Runtime {
	if rt, err := LoadFromRedis(ctx, rdb); err == nil && rt != nil {
		return rt
	}
	if db == nil {
		return &Runtime{SiteTitle: "GoAlgo"}
	}
	rt, err := LoadFromDB(db)
	if err != nil || rt == nil {
		return &Runtime{SiteTitle: "GoAlgo"}
	}
	if rt.worthCaching() {
		if err := PublishRedis(ctx, rdb, rt); err != nil {
			log.Warnf("sitesettings: PublishRedis after LoadFromDB: %v", err)
		}
	}
	return rt
}

// LoadPreferDB 以 DB 为准（user 服务内）；有效配置才写 Redis
func LoadPreferDB(ctx context.Context, db *gorm.DB, rdb *redis.Client) *Runtime {
	rt, err := LoadFromDB(db)
	if err != nil || rt == nil {
		return &Runtime{SiteTitle: "GoAlgo"}
	}
	if rt.worthCaching() {
		if err := PublishRedis(ctx, rdb, rt); err != nil {
			log.Warnf("sitesettings: PublishRedis after LoadPreferDB: %v", err)
		}
	}
	return rt
}

func (rt *Runtime) SMTPConf() *conf.SMTP {
	if rt == nil {
		return &conf.SMTP{}
	}
	port := rt.SMTPPort
	if port <= 0 {
		port = 465
	}
	return &conf.SMTP{
		Host:     rt.SMTPHost,
		Port:     int32(port),
		Username: rt.SMTPUsername,
		Password: rt.SMTPPassword,
		From:     rt.SMTPFrom,
	}
}

func (rt *Runtime) MailSender() *mail.Sender {
	return mail.NewSender(rt.SMTPConf())
}

func (rt *Runtime) AgentConf() *conf.Agent {
	if rt == nil {
		return &conf.Agent{}
	}
	return &conf.Agent{Model: rt.AgentModel, Secret: rt.AgentSecret}
}

func (rt *Runtime) AiAnalyzeConf() *conf.AiAnalyze {
	if rt == nil {
		return &conf.AiAnalyze{}
	}
	return &conf.AiAnalyze{
		Endpoint: rt.AiAnalyzeEndpoint,
		Model:    rt.AiAnalyzeModel,
		Secret:   rt.AiAnalyzeSecret,
	}
}

// MergeFallback 用 yaml 兜底填空字段（仅迁移期）
func (rt *Runtime) MergeFallback(smtp *conf.SMTP, agent *conf.Agent, ai *conf.AiAnalyze) *Runtime {
	if rt == nil {
		rt = &Runtime{SiteTitle: "GoAlgo"}
	}
	if smtp != nil {
		if rt.SMTPHost == "" {
			rt.SMTPHost = smtp.Host
		}
		if rt.SMTPPort <= 0 && smtp.Port > 0 {
			rt.SMTPPort = int(smtp.Port)
		}
		if rt.SMTPUsername == "" {
			rt.SMTPUsername = smtp.Username
		}
		if rt.SMTPPassword == "" {
			rt.SMTPPassword = smtp.Password
		}
		if rt.SMTPFrom == "" {
			rt.SMTPFrom = smtp.From
		}
	}
	if agent != nil {
		if rt.AgentModel == "" {
			rt.AgentModel = agent.Model
		}
		if rt.AgentSecret == "" {
			rt.AgentSecret = agent.Secret
		}
	}
	if ai != nil {
		if rt.AiAnalyzeEndpoint == "" {
			rt.AiAnalyzeEndpoint = ai.Endpoint
		}
		if rt.AiAnalyzeModel == "" {
			rt.AiAnalyzeModel = ai.Model
		}
		if rt.AiAnalyzeSecret == "" {
			rt.AiAnalyzeSecret = ai.Secret
		}
	}
	return rt
}

func MaskSecret(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "••••••••"
}
