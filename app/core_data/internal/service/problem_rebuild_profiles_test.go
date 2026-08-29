package service

import (
	"context"
	"errors"
	"testing"

	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/streadway/amqp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type serviceRebuildRefresher struct {
	err   error
	calls int
}

func (r *serviceRebuildRefresher) Refresh(context.Context, task.AbilityStatsRefreshMode) (uint64, error) {
	r.calls++
	return 0, r.err
}

func (r *serviceRebuildRefresher) RefreshForMaintenance(context.Context, task.AbilityStatsMaintenanceTransition) (uint64, error) {
	r.calls++
	return 0, r.err
}

type serviceRebuildPublisher struct{}

func (*serviceRebuildPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (*serviceRebuildPublisher) Publish(string, string, bool, bool, amqp.Publishing) error {
	return nil
}

func TestProblemServiceRebuildAllProfilesReturnsRefreshErrorUnchanged(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.UserACProblem{}, &model.AbilityMaintenancePending{}, &model.AbilityMaintenanceTarget{}); err != nil {
		t.Fatal(err)
	}
	refreshErr := errors.New("refresh failed unchanged")
	refresher := &serviceRebuildRefresher{err: refreshErr}
	profileTask := task.NewUserProfileTaskWithPublisher(&serviceRebuildPublisher{}, nil)
	uc := bizservice.NewProblemUseCase(&coredata.Data{DB: db}, nil, nil, nil, profileTask, refresher)
	svc := &ProblemService{uc: uc}
	token := spiderExtraAdminToken(t, 901, true)
	ctx := luoguHeaderContext("Authorization", "Bearer "+token)

	candidates, published, unauthorized, gotErr := svc.RebuildAllProfiles(ctx)

	if !errors.Is(gotErr, refreshErr) || candidates != 0 || published != 0 || unauthorized {
		t.Fatalf("candidates=%d published=%d unauthorized=%v err=%v", candidates, published, unauthorized, gotErr)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls=%d", refresher.calls)
	}
}
