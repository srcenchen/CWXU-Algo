package data

import (
	"context"

	"cwxu-algo/app/common/conf"
	redis2 "cwxu-algo/app/common/data/redis"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/sitesettings"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// ProviderSet is data providers.
var ProviderSet = wire.NewSet(NewData, NewDataRDB)

// NewDataDB 从 Data 中提取 DB
func NewDataDB(data *Data) *gorm.DB {
	return data.DB
}

// NewDataRDB 从 Data 中提取 RDB
func NewDataRDB(data *Data) *redis.Client {
	return data.RDB
}

// Data .
type Data struct {
	DB  *gorm.DB
	RDB *redis.Client
}

// NewData .
func NewData(c *conf.Data) (*Data, func(), error) {
	data := &Data{RDB: redis2.InitRedis(c)}
	mail.SetStatusReporter(func(ok bool, errMsg string) {
		st, msg := sitesettings.StatusFail, errMsg
		if ok {
			st, msg = sitesettings.StatusOK, ""
		}
		sitesettings.SetServiceStatus(context.Background(), data.RDB, sitesettings.ServiceSmtp, st, msg)
	})
	//migrateModels(data.DB)
	cleanup := func() {
		log.Info("closing the data resources")
		if data.DB != nil {
			if sql, err := data.DB.DB(); err == nil && sql != nil {
				_ = sql.Close()
			}
		}
		if data.RDB != nil {
			_ = data.RDB.Close()
		}
	}
	return data, cleanup, nil
}

//// migrateModels 合并
//func migrateModels(db *gorm.DB) {
//	err := db.AutoMigrate(&model.SubmitLog{}, &model.Platform{})
//	if err != nil {
//		panic("数据库：数据库自动合并失败")
//	}
//}
