package service

import (
	"context"
	"time"

	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
)

// pipelineSweepHour 每日资格回落监测时刻（服务器时区）
const pipelineSweepHour = 3

// startPipelineEligibilitySweep 每日定时监测：订阅到期 / 资格取消后，个人题面爬取/AI
// 开关自动回落（跟随剩余组织资格）。同时兜底清理任何残留的资格镜像覆盖。
// 每日固定小时跑一次（对齐「到期回落是每天定时监测」的需求）。
// 返回 stop 函数，进程退出时调用。
func startPipelineEligibilitySweep(d *data.Data) func() {
	if d == nil || d.DB == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	go func() {
		ctx := context.Background()
		sweep := func() {
			pd := dal.NewProfileDalRaw(d.DB, d.RDB)
			n, err := pd.SyncProblemPipelineOverridesBatch(ctx, nil)
			if err != nil {
				log.Warnf("pipeline eligibility sweep: %v", err)
			} else if n > 0 {
				log.Infof("pipeline eligibility sweep updated %d users", n)
			}
		}
		// 启动先跑一次（覆盖进程重启期间已到期的用户），再按日轮询
		sweep()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case t := <-ticker.C:
				if t.Hour() == pipelineSweepHour {
					sweep()
				}
			}
		}
	}()
	return func() { close(stopCh) }
}
