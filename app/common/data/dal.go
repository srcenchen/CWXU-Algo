package data

import (
	"context"
	"cwxu-algo/app/common/utils"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// cacheSF 防止缓存击穿：同一 key 并发 miss 只回源一次
var cacheSF singleflight.Group

// DefaultCacheTTL 个人统计默认 TTL（1w 日活：靠 user ver 失效，不宜过长占内存）
const DefaultCacheTTL = 6 * time.Hour

// OrgStatsCacheTTL 组织/全站统计较短 TTL，配合全局 ver 节流
const OrgStatsCacheTTL = 3 * time.Minute

// GetCacheDal 通用带Redis缓存的Dal查询操作层
//
// 泛型参数:
//   - T any 一般要传入一个model结构体
//
// 参数:
//   - ctx context 上下文
//   - rdb *redis.Client Redis客户端
//   - cacheKey string Redis缓存键
//   - dbFunc 数据库兜底策略查询
//
// 返回值:
//   - *T 返回的查询结果
//   - bool 是否命中缓存
//   - error 错误信息
func GetCacheDal[T any](
	ctx context.Context,
	rdb *redis.Client,
	cacheKey string,
	dbFunc func(data *T) error,
) (*T, bool, error) {
	return GetCacheDalTTL[T](ctx, rdb, cacheKey, DefaultCacheTTL, dbFunc)
}

// GetCacheDalTTL 同 GetCacheDal，可指定 TTL
func GetCacheDalTTL[T any](
	ctx context.Context,
	rdb *redis.Client,
	cacheKey string,
	ttl time.Duration,
	dbFunc func(data *T) error,
) (*T, bool, error) {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}

	// 尝试去查 Redis
	res := rdb.Get(ctx, cacheKey)
	rVal, err := res.Result()
	if err == nil {
		var data T
		if err := utils.GobDecoder([]byte(rVal), &data); err != nil {
			return nil, false, fmt.Errorf("缓存解析出错 %s", err.Error())
		}
		return &data, true, nil
	}

	// miss：singleflight 合并并发回源
	v, err, _ := cacheSF.Do(cacheKey, func() (interface{}, error) {
		// 回源 ctx 与首个调用者解耦：首个调用者取消不应拖死同 key 的等待者。
		// 注意 dbFunc 不接收 ctx，其内部若捕获了调用方 ctx 无法在此解耦。
		sfCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// double-check：其它协程可能已写入
		if rVal, err := rdb.Get(sfCtx, cacheKey).Result(); err == nil {
			var data T
			if err := utils.GobDecoder([]byte(rVal), &data); err != nil {
				return nil, fmt.Errorf("缓存解析出错 %s", err.Error())
			}
			return &data, nil
		}

		var data T
		if err := dbFunc(&data); err != nil {
			return nil, err
		}
		b, err := utils.GobEncoder(&data)
		if err != nil {
			return nil, errors.New("gob编码失败")
		}
		_ = rdb.Set(sfCtx, cacheKey, b, ttl).Err()
		return &data, nil
	})
	if err != nil {
		return nil, false, err
	}
	data, ok := v.(*T)
	if !ok {
		return nil, false, errors.New("cache type assert failed")
	}
	return data, false, nil
}

// UpdateCacheDal 通用带Redis缓存的Dal更新操作层
//
// 参数:
//   - ctx context 上下文
//   - rdb *redis.Client Redis客户端
//   - cacheKey string Redis缓存键
//   - dbFunc 数据库更新策略
//
// 返回值:
//   - error 错误信息
func UpdateCacheDal(
	ctx context.Context,
	rdb *redis.Client,
	cacheKey string,
	dbFunc func() error,
) error {
	err := dbFunc()
	if err != nil {
		return err
	}
	// 更新成功后把Redis缓存删了
	_ = rdb.Del(ctx, cacheKey)
	return nil
}
