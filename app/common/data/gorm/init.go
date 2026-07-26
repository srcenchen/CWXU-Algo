package gorm

import (
	"cwxu-algo/app/common/conf"
	"os"
	"strconv"
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
)

// InitGorm 初始化GORM 连接数据库。
// 仅在各服务启动期由 wire 生成代码调用；改为返回 error 会改变导出签名并
// 波及全部服务的 wire 依赖图，而数据库连不上时启动本就应终止，故保留 panic（fail-fast）。
func InitGorm(conf *conf.Data) *gorm.DB {
	var db *gorm.DB
	var err error
	switch conf.Database.Driver {
	case "postgres":
		db, err = gorm.Open(postgres.Open(conf.Database.Source), &gorm.Config{
			// 生产默认 Warn，减少 2c 上日志 CPU
			Logger: logger.Default.LogMode(logger.Warn),
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
