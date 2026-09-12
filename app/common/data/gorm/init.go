package gorm

import (
	"cwxu-algo/app/common/conf"
	stdlog "log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 默认按 2c4g 单机多进程共库设计：
// gateway/user/core_data/agent 各开池，合计不宜超过 PG max_connections 的 50%。
// 环境变量可覆盖：CWXU_DB_MAX_OPEN / CWXU_DB_MAX_IDLE
const (
	defaultMaxOpen = 8
	defaultMaxIdle = 3
	// defaultSlowThreshold 生产慢查询阈值。GORM 默认 200ms 在 2c4g 上过于敏感，
	// 会把正常统计查询打成 SLOW SQL 刷屏，这里放宽到 1s。
	defaultSlowThreshold = time.Second
)

// NewLogger 返回统一配置的 GORM 日志器。
//
// 生产曾因 logger.Default 的两个默认行为把 core_data 容器日志写到 55GB：
//   - IgnoreRecordNotFoundError=false：轮询类查询（如洛谷清理状态）未命中属正常，
//     却被按 Error 逐条打印（record not found）。
//   - SlowThreshold=200ms：2c4g 上大量正常查询被判为 SLOW SQL，按 Warn 刷屏。
//
// 环境变量：
//   - CWXU_GORM_LOG_LEVEL  silent|error|warn|info，默认 warn
//   - CWXU_GORM_SLOW_MS    慢查询阈值毫秒，0 关闭，默认 1000
func NewLogger() logger.Interface {
	return logger.New(
		stdlog.New(os.Stdout, "\r\n", stdlog.LstdFlags),
		logger.Config{
			SlowThreshold:             parseSlowThreshold(os.Getenv("CWXU_GORM_SLOW_MS")),
			LogLevel:                  parseLogLevel(os.Getenv("CWXU_GORM_LOG_LEVEL")),
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
}

func parseLogLevel(raw string) logger.LogLevel {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "silent":
		return logger.Silent
	case "error":
		return logger.Error
	case "info":
		return logger.Info
	default:
		return logger.Warn
	}
}

func parseSlowThreshold(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultSlowThreshold
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return defaultSlowThreshold
	}
	return time.Duration(ms) * time.Millisecond
}

// InitGorm 初始化GORM 连接数据库。
// 仅在各服务启动期由 wire 生成代码调用；改为返回 error 会改变导出签名并
// 波及全部服务的 wire 依赖图，而数据库连不上时启动本就应终止，故保留 panic（fail-fast）。
func InitGorm(conf *conf.Data) *gorm.DB {
	var db *gorm.DB
	var err error
	switch conf.Database.Driver {
	case "postgres":
		db, err = gorm.Open(postgres.Open(conf.Database.Source), &gorm.Config{
			// 生产默认 Warn，忽略 record not found，放宽慢查询阈值（见 NewLogger）
			Logger: NewLogger(),
			// 预编译语句：高频统计/列表查询降解析开销
			PrepareStmt: true,
		})
		if err != nil {
			panic("数据库：postgres数据库连接失败" + err.Error())
		}
	}
	if db == nil {
		panic("数据库：数据库连接失败")
	}
	sqlDB, err := db.DB()
	if err != nil {
		panic("数据库：获取连接池失败" + err.Error())
	}
	maxOpen := envInt("CWXU_DB_MAX_OPEN", defaultMaxOpen)
	maxIdle := envInt("CWXU_DB_MAX_IDLE", defaultMaxIdle)
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	// 短生命周期：2c4g 上避免连接占满与陈旧连接
	sqlDB.SetConnMaxLifetime(15 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	log.Infof("数据库：连接池 MaxOpen=%d MaxIdle=%d (2c4g 友好默认)", maxOpen, maxIdle)
	return db
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
