package main

import (
	"fmt"
	"time"

	gorminit "cwxu-algo/app/common/data/gorm"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func openDB(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gorminit.NewLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库：%w", err)
	}
	return db, nil
}

func timeNow() time.Time {
	return time.Now()
}
