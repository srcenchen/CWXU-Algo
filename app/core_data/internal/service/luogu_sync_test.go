package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	"cwxu-algo/app/common/event"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type collectingLuoguProfilePublisher struct {
	mu        sync.Mutex
	events    []event.UserProfileEvent
	onPublish func(event.UserProfileEvent) error
}

func (p *collectingLuoguProfilePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *collectingLuoguProfilePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	var ev event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &ev); err != nil {
		return err
	}
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
	if p.onPublish != nil {
		return p.onPublish(ev)
	}
	return nil
}

func (p *collectingLuoguProfilePublisher) snapshot() []event.UserProfileEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.UserProfileEvent(nil), p.events...)
}

type luoguTestHeader struct{ h http.Header }

func (h *luoguTestHeader) Get(key string) string      { return h.h.Get(key) }
func (h *luoguTestHeader) Set(key, value string)      { h.h.Set(key, value) }
func (h *luoguTestHeader) Add(key, value string)      { h.h.Add(key, value) }
func (h *luoguTestHeader) Keys() []string             { return nil }
func (h *luoguTestHeader) Values(key string) []string { return h.h.Values(key) }

type luoguTestTransport struct{ header *luoguTestHeader }

func (t *luoguTestTransport) Kind() transport.Kind            { return transport.KindHTTP }
func (t *luoguTestTransport) Endpoint() string                { return "http://core.test" }
func (t *luoguTestTransport) Operation() string               { return "/api.core.v1.spider.Spider/StartLuoguSync" }
func (t *luoguTestTransport) RequestHeader() transport.Header { return t.header }
func (t *luoguTestTransport) ReplyHeader() transport.Header   { return t.header }

func luoguHeaderContext(key, value string) context.Context {
	h := http.Header{}
	h.Set(key, value)
	return transport.NewServerContext(context.Background(), &luoguTestTransport{header: &luoguTestHeader{h: h}})
}

type fakeLuoguValidator struct {
	identity luoguPluginIdentity
	err      error
}

type beforeLuoguSessionLockHook struct {
	once sync.Once
	fn   func()
}

type failLuoguCheckpointOnceHook struct {
	once sync.Once
}

type failLuoguCacheAndCheckpointOnceHook struct {
	mu          sync.Mutex
	cacheFailed bool
}

type commitLuoguCheckpointThenFailOnceHook struct {
	once sync.Once
}

type failLuoguGenerationHook struct{}

type failLuoguFinalizeGetOnceHook struct {
	once sync.Once
}

type failLuoguTailCommandOnceHook struct {
	once    sync.Once
	command string
	key     string
}

type cancelledFinishLockHook struct {
	once sync.Once
	run  func()
}

type platformLockAttemptHook struct {
	key           string
	attempts      atomic.Int32
	secondAttempt chan struct{}
	secondOnce    sync.Once
}

type afterLuoguGenerationBumpHook struct {
	key  string
	once sync.Once
	run  func()
}

func (h *afterLuoguGenerationBumpHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *afterLuoguGenerationBumpHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err != nil || cmd.Name() != "eval" {
			return err
		}
		for _, arg := range cmd.Args()[1:] {
			if fmt.Sprint(arg) == h.key {
				h.once.Do(h.run)
				break
			}
		}
		return nil
	}
}
func (h *afterLuoguGenerationBumpHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *platformLockAttemptHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *platformLockAttemptHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" && len(cmd.Args()) > 1 && fmt.Sprint(cmd.Args()[1]) == h.key {
			if h.attempts.Add(1) == 2 {
				h.secondOnce.Do(func() { close(h.secondAttempt) })
			}
		}
		return next(ctx, cmd)
	}
}
func (h *platformLockAttemptHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *cancelledFinishLockHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *cancelledFinishLockHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "eval" && len(cmd.Args()) > 1 {
			script := fmt.Sprint(cmd.Args()[1])
			if strings.Contains(script, `local next = redis.call("INCR", KEYS[1])`) && strings.Contains(script, `redis.call("DEL", KEYS[2])`) {
				h.once.Do(h.run)
			}
		}
		return next(ctx, cmd)
	}
}
func (h *cancelledFinishLockHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failLuoguFinalizeGetOnceHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failLuoguFinalizeGetOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && strings.HasPrefix(fmt.Sprint(cmd.Args()[1]), "luogu:sync:active:") {
			failed := false
			h.once.Do(func() { failed = true })
			if failed {
				return errors.New("injected Luogu finalize failure")
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failLuoguFinalizeGetOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failLuoguTailCommandOnceHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failLuoguTailCommandOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		matches := cmd.Name() == h.command
		if matches {
			matches = false
			for _, arg := range cmd.Args()[1:] {
				if fmt.Sprint(arg) == h.key {
					matches = true
					break
				}
			}
		}
		if matches {
			failed := false
			h.once.Do(func() { failed = true })
			if failed {
				return errors.New("injected Luogu tail command failure")
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failLuoguTailCommandOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (failLuoguGenerationHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (failLuoguGenerationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && strings.HasPrefix(cmd.Args()[1].(string), "spider:gen:") {
			return errors.New("generation unavailable")
		}
		return next(ctx, cmd)
	}
}
func (failLuoguGenerationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failLuoguCheckpointOnceHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failLuoguCheckpointOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		isCheckpoint := cmd.Name() == "eval"
		if isCheckpoint {
			isCheckpoint = false
			for _, arg := range cmd.Args() {
				if strings.HasPrefix(fmt.Sprint(arg), "luogu:sync:session:") {
					isCheckpoint = true
					break
				}
			}
		}
		if isCheckpoint {
			failed := false
			h.once.Do(func() { failed = true })
			if failed {
				return errors.New("injected session checkpoint failure")
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failLuoguCheckpointOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		isCheckpoint := false
		for _, cmd := range cmds {
			if cmd.Name() == "hset" && len(cmd.Args()) > 1 && strings.HasPrefix(cmd.Args()[1].(string), "luogu:sync:session:") {
				isCheckpoint = true
				break
			}
		}
		if isCheckpoint {
			failed := false
			h.once.Do(func() { failed = true })
			if failed {
				return errors.New("injected session checkpoint failure")
			}
		}
		return next(ctx, cmds)
	}
}

func (h *failLuoguCacheAndCheckpointOnceHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}
func (h *failLuoguCacheAndCheckpointOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "incr" && len(cmd.Args()) > 1 && fmt.Sprint(cmd.Args()[1]) == "statistic:user:7:ver" {
			h.mu.Lock()
			fail := !h.cacheFailed
			h.cacheFailed = true
			h.mu.Unlock()
			if fail {
				return errors.New("injected cache invalidation failure")
			}
		}
		return next(ctx, cmd)
	}
}
func (h *failLuoguCacheAndCheckpointOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		isCacheInvalidation := false
		for _, cmd := range cmds {
			if cmd.Name() == "incr" && len(cmd.Args()) > 1 && fmt.Sprint(cmd.Args()[1]) == "statistic:user:7:ver" {
				isCacheInvalidation = true
				break
			}
		}
		if isCacheInvalidation {
			h.mu.Lock()
			fail := !h.cacheFailed
			h.cacheFailed = true
			h.mu.Unlock()
			if fail {
				return errors.New("injected cache invalidation pipeline failure")
			}
		}
		return next(ctx, cmds)
	}
}

func (h *commitLuoguCheckpointThenFailOnceHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}
func (h *commitLuoguCheckpointThenFailOnceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		isCheckpoint := cmd.Name() == "eval"
		if isCheckpoint {
			isCheckpoint = false
			for _, arg := range cmd.Args() {
				if strings.HasPrefix(fmt.Sprint(arg), "luogu:sync:session:") {
					isCheckpoint = true
					break
				}
			}
		}
		err := next(ctx, cmd)
		if err != nil || !isCheckpoint {
			return err
		}
		failed := false
		h.once.Do(func() { failed = true })
		if failed {
			return errors.New("injected ambiguous checkpoint result")
		}
		return nil
	}
}
func (h *commitLuoguCheckpointThenFailOnceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *beforeLuoguSessionLockHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *beforeLuoguSessionLockHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" && len(cmd.Args()) > 1 && strings.HasPrefix(cmd.Args()[1].(string), "luogu:sync:lock:") {
			h.once.Do(h.fn)
		}
		return next(ctx, cmd)
	}
}
func (h *beforeLuoguSessionLockHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (v *fakeLuoguValidator) ValidateLuoguPluginToken(context.Context, string) (luoguPluginIdentity, error) {
	return v.identity, v.err
}

type fakeLuoguImporter struct {
	mu            sync.Mutex
	calls         int
	inserted      int64
	err           error
	completed     []string
	postProcessed int
	profileErr    error
	profileCalls  int
	entered       chan struct{}
	release       <-chan struct{}
	enterOnce     sync.Once
	auditDB       *gorm.DB
}

func (i *fakeLuoguImporter) StartClientSyncAudit(_ context.Context, start bizservice.ClientSyncAuditStart) error {
	if i.auditDB == nil {
		return nil
	}
	return i.auditDB.Create(&model.ClientSyncAudit{SessionID: start.SessionID, AuthorizationID: start.AuthorizationID, UserID: start.UserID, Platform: start.Platform, OJUID: start.OJUID, ClientKind: start.ClientKind, ClientVersion: start.ClientVersion, Status: "running", StartedAt: start.StartedAt, UpdatedAt: start.StartedAt}).Error
}
func (i *fakeLuoguImporter) UpdateClientSyncAudit(_ context.Context, p bizservice.ClientSyncAuditProgress) error {
	return nil
}
func (i *fakeLuoguImporter) TerminateClientSyncAudit(_ context.Context, sessionID, status, reason, code, message string, at time.Time) error {
	if i.auditDB == nil {
		return nil
	}
	return i.auditDB.Model(&model.ClientSyncAudit{}).Where("session_id = ? AND terminal_at IS NULL", sessionID).Updates(map[string]interface{}{"status": status, "completion_reason": reason, "error_code": code, "error_message": message, "terminal_at": at, "retention_until": at.Add(7 * 24 * time.Hour), "updated_at": at}).Error
}

type fakeLuoguSessionFinalizingImporter struct {
	*fakeLuoguImporter
	mu    sync.Mutex
	err   error
	calls int
}

func (i *fakeLuoguSessionFinalizingImporter) MarkClientSyncSessionTerminated(context.Context, string, time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	return i.err
}

func (i *fakeLuoguImporter) ForcePublishUserProfile(int64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.profileCalls++
	return i.profileErr
}

func (i *fakeLuoguImporter) ForcePublishMaintenanceUserProfile(_ int64, _ string) error {
	return i.ForcePublishUserProfile(0)
}

func (i *fakeLuoguImporter) RelayAbilityMaintenanceTargets(context.Context, *model.AbilityMaintenancePending) error {
	return i.ForcePublishUserProfile(0)
}

func (i *fakeLuoguImporter) ImportSubmitLogs(context.Context, int64, string, int64, []model.SubmitLog) (bizservice.SubmitImportResult, error) {
	i.mu.Lock()
	i.calls++
	inserted, err := i.inserted, i.err
	entered, release := i.entered, i.release
	i.mu.Unlock()
	if entered != nil {
		i.enterOnce.Do(func() { close(entered) })
	}
	if release != nil {
		<-release
	}
	return bizservice.SubmitImportResult{Inserted: inserted}, err
}

func (i *fakeLuoguImporter) CompleteClientSync(_ context.Context, _ int64, _ string, _ int64, head string, _ time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.completed = append(i.completed, head)
	return nil
}

func (i *fakeLuoguImporter) ScheduleSubmitPostProcess(int64) {
	i.mu.Lock()
	i.postProcessed++
	i.mu.Unlock()
}

type fakeLuoguClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeLuoguClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLuoguClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newLuoguSyncServiceTest(t *testing.T) (*SpiderService, *gorm.DB, *redis.Client, *fakeLuoguClock, *fakeLuoguImporter) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.Platform{}, &model.SubmitLog{}, &model.ContestLog{}, &model.DailyUserStat{},
		&model.UserACProblem{}, &model.UserACProblemDay{}, &model.UserTagACSnapshot{}, &model.SpiderRepairState{}, &model.ContestUserProblem{},
		&model.ProblemTag{}, &model.AbilityMaintenancePending{}, &model.AbilityMaintenanceTarget{},
	); err != nil {
		t.Fatal(err)
	}
	if err := model.InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Platform{UserID: 7, Platform: "LuoGu", Username: "2245873"}).Error; err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := rdb.Set(context.Background(), task.GenerationKey(7, "LuoGu"), 2, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	clock := &fakeLuoguClock{now: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)}
	importer := &fakeLuoguImporter{inserted: 1}
	svc := &SpiderService{
		db: db, rdb: rdb,
		luoguTokenValidator: &fakeLuoguValidator{identity: luoguPluginIdentity{
			AuthorizationID: 11, UserID: 7, LuoguUID: "2245873", ClientKind: "userscript", ClientVersion: "1.0.0",
		}},
		luoguImporter: importer,
		luoguClock:    clock,
	}
	return svc, db, rdb, clock, importer
}

func useDurableLuoguProfileRelay(svc *SpiderService, db *gorm.DB, rdb *redis.Client) *collectingLuoguProfilePublisher {
	publisher := &collectingLuoguProfilePublisher{}
	profileTask := task.NewUserProfileTaskWithPublisher(publisher, rdb)
	data := &coredata.Data{DB: db, RDB: rdb}
	problemUC := bizservice.NewProblemUseCase(data, nil, nil, nil, profileTask, nil)
	svc.luoguImporter = bizservice.NewSpiderUseCase(data, problemUC, nil)
	return publisher
}

func TestRemoveInvalidLuoguBindingPreservesProfileUntilReplacementCrawl(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	if err := db.AutoMigrate(
		&model.Problem{}, &model.ProblemTag{}, &model.UserTagAC{}, &model.UserProblemStatus{},
		&model.AbilityModelState{}, &model.ProblemAbilityStat{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	remaining := model.Problem{Platform: "CodeForces", ExternalID: "1A", Title: "A", Tags: model.StringArray{"dp"}}
	if err := db.Create(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: remaining.ID, Tag: "dp"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserACProblem{
		{UserID: 7, ProblemKey: "e:LuoGu:P1000", Platform: "LuoGu", FirstACAt: now},
		{UserID: 7, ProblemKey: fmt.Sprintf("p:%d", remaining.ID), Platform: "CodeForces", FirstACAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagAC{UserID: 7, Tag: "legacy", Count: 99, Weight: 99}).Error; err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"problem:user_profile:s7:u7:latest":        "old-latest",
		"problem:user_profile:s7:u7:m1:eold:g0:u0": "old-exact",
		"user_profile:fp:7":                        "old-fingerprint",
	} {
		if err := rdb.Set(ctx, key, value, time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}
	publisher := useDurableLuoguProfileRelay(svc, db, rdb)

	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	var rows []model.UserTagAC
	if err := db.Where("user_id = ?", 7).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Tag != "legacy" || rows[0].Count != 99 {
		t.Fatalf("existing canonical AC aggregate was not preserved: %+v", rows)
	}
	if keys := rdb.Keys(ctx, "problem:user_profile:s*:u7:*").Val(); len(keys) != 0 {
		t.Fatalf("stale profile caches survived invalid binding removal: %v", keys)
	}
	if rdb.Exists(ctx, "user_profile:fp:7").Val() != 0 {
		t.Fatal("stale profile fingerprint survived invalid binding removal")
	}
	if events := publisher.snapshot(); len(events) != 0 {
		t.Fatalf("invalid binding cleanup rebuilt profile before replacement crawl: %+v", events)
	}
}

func TestLuoguCleanupClearsDueDeliveredTargetWithoutRebuild(t *testing.T) {
	svc, db, _, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()

	intentID := "luogu-due-intent"
	pending := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(7), OperationID: intentID, Revision: 1,
		Phase: "tail_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
		Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityMaintenanceTarget{
		IntentID: intentID, UserID: 7, Revision: 1, State: "delivered",
		NextRetryAt: time.Now().Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").Delete(&model.Platform{}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err != nil {
		t.Fatal(err)
	}
	var parentCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&parentCount).Error; err != nil || parentCount != 0 {
		t.Fatalf("cleanup parent count=%d err=%v", parentCount, err)
	}
	var targetCount int64
	if err := db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", intentID).Count(&targetCount).Error; err != nil || targetCount != 0 {
		t.Fatalf("cleanup target count=%d err=%v", targetCount, err)
	}
}

func TestLuoguCleanupRelayRejectsMissingTarget(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	publisher := &collectingLuoguProfilePublisher{}
	profileTask := task.NewUserProfileTaskWithPublisher(publisher, rdb)
	data := &coredata.Data{DB: db, RDB: rdb}
	problemUC := bizservice.NewProblemUseCase(data, nil, nil, nil, profileTask, nil)
	svc.luoguImporter = bizservice.NewSpiderUseCase(data, problemUC, nil)

	pending := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(7), OperationID: "luogu-missing-target", Revision: 1,
		Phase: "tail_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
		Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.publishAndClearLuoguCleanup(ctx, &pending); err == nil {
		t.Fatal("relay accepted cleanup parent without its target")
	}
	var parentCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&parentCount).Error; err != nil || parentCount != 1 {
		t.Fatalf("missing target relay removed parent count=%d err=%v", parentCount, err)
	}
}

func TestLuoguCleanupRelayRejectsStaleOrAmbiguousState(t *testing.T) {
	tests := []struct {
		name          string
		mutateParent  func(*model.AbilityMaintenancePending, *model.AbilityMaintenancePending)
		targetUserIDs []int64
	}{
		{
			name: "stale revision",
			mutateParent: func(stored, request *model.AbilityMaintenancePending) {
				stored.Revision = 2
				request.Revision = 1
			},
			targetUserIDs: []int64{7},
		},
		{
			name: "wrong operation",
			mutateParent: func(stored, request *model.AbilityMaintenancePending) {
				stored.Operation = "problem"
				request.Operation = stored.Operation
			},
			targetUserIDs: []int64{7},
		},
		{
			name: "wrong phase",
			mutateParent: func(stored, request *model.AbilityMaintenancePending) {
				stored.Phase = "fence_finalized"
				request.Phase = stored.Phase
			},
			targetUserIDs: []int64{7},
		},
		{name: "wrong user", targetUserIDs: []int64{8}},
		{name: "second target", targetUserIDs: []int64{7, 8}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
			publisher := useDurableLuoguProfileRelay(svc, db, rdb)
			stored := model.AbilityMaintenancePending{
				Scope: luoguCleanupScope(7), OperationID: "luogu-relay-" + strings.ReplaceAll(tc.name, " ", "-"), Revision: 1,
				Phase: "tail_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
				Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
			}
			request := stored
			if tc.mutateParent != nil {
				tc.mutateParent(&stored, &request)
			}
			if err := db.Create(&stored).Error; err != nil {
				t.Fatal(err)
			}
			for _, userID := range tc.targetUserIDs {
				if err := db.Create(&model.AbilityMaintenanceTarget{IntentID: stored.OperationID, UserID: userID, Revision: 1, State: "outbox_ready"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.publishAndClearLuoguCleanup(context.Background(), &request); err == nil {
				t.Fatal("relay accepted stale or ambiguous durable state")
			}
			if events := publisher.snapshot(); len(events) != 0 {
				t.Fatalf("rejected relay still published events=%+v", events)
			}
			var parentCount, targetCount int64
			if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", stored.Scope).Count(&parentCount).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", stored.OperationID).Count(&targetCount).Error; err != nil {
				t.Fatal(err)
			}
			if parentCount != 1 || targetCount != int64(len(tc.targetUserIDs)) {
				t.Fatalf("rejected relay mutated durable state parent=%d targets=%d", parentCount, targetCount)
			}
		})
	}
}

func TestLuoguCleanupConsumerConfirmationRaceCompletesOnlyAfterConsumption(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	publisher := &collectingLuoguProfilePublisher{}
	profileTask := task.NewUserProfileTaskWithPublisher(publisher, rdb)
	data := &coredata.Data{DB: db, RDB: rdb}
	problemUC := bizservice.NewProblemUseCase(data, nil, nil, nil, profileTask, nil)
	publisher.onPublish = func(ev event.UserProfileEvent) error {
		return problemUC.ConfirmAbilityMaintenanceTarget(ctx, ev.IntentID, ev.UserId)
	}
	svc.luoguImporter = bizservice.NewSpiderUseCase(data, problemUC, nil)

	intentID := "luogu-confirm-race"
	pending := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(7), OperationID: intentID, Revision: 1,
		Phase: "tail_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
		Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityMaintenanceTarget{IntentID: intentID, UserID: 7, Revision: 1, State: "outbox_ready"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").Delete(&model.Platform{}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err != nil {
		t.Fatal(err)
	}
	if events := publisher.snapshot(); len(events) != 0 {
		t.Fatalf("confirmation race rebuilt profile=%+v", events)
	}
	var parentCount, targetCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&parentCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", intentID).Count(&targetCount).Error; err != nil {
		t.Fatal(err)
	}
	if parentCount != 0 || targetCount != 0 {
		t.Fatalf("consumed cleanup was not completed parent=%d target=%d", parentCount, targetCount)
	}
}

func TestLuoguCleanupTailSkipsReplacementBindingSessionAndCrawlerKeys(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	pending := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(7), OperationID: "luogu-replacement-tail", Revision: 1,
		Phase: "fence_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
		Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 7, Revision: 1, State: "outbox_ready"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": 99, "username": "998877",
	}).Error; err != nil {
		t.Fatal(err)
	}
	keys := []string{
		luoguSyncActiveKey(7, "2245873"),
		"spider:pending:7:LuoGu",
		"spider:inflight:7:LuoGu",
	}
	for _, key := range keys {
		if err := rdb.Set(ctx, key, "replacement", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}

	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "998877"); err != nil || !removed {
		t.Fatalf("replacement cleanup removed=%v err=%v", removed, err)
	}
	for _, key := range keys {
		if value := rdb.Get(ctx, key).Val(); value != "replacement" {
			t.Fatalf("replacement key %s was cleared: %q", key, value)
		}
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("replacement cleanup left durable intent count=%d", pendingCount)
	}
}

func TestLuoguCleanupTailWaitsForConcurrentSetAndPreservesCommittedReplacement(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").Delete(&model.Platform{}).Error; err != nil {
		t.Fatal(err)
	}
	cleanup := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(7), OperationID: "luogu-concurrent-set-tail", Revision: 1,
		Phase: "fence_finalized", Operation: "luogu_cleanup", UserID: 7, Platform: "LuoGu",
		Payload: `{"bindingId":1,"username":"legacy-name","authorizedUid":"2245873"}`,
	}
	if err := db.Create(&cleanup).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityMaintenanceTarget{IntentID: cleanup.OperationID, UserID: 7, Revision: 1, State: "outbox_ready"}).Error; err != nil {
		t.Fatal(err)
	}
	setPending, err := prepareSpiderMaintenancePending(ctx, db, spiderSetMaintenanceScope(7, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":7,"platform":"LuoGu","username":"998877"}`)
	if err != nil {
		t.Fatal(err)
	}
	svc.spider = task.NewSpiderTask(nil, rdb, db)

	setFactsEntered := make(chan struct{})
	releaseSetFacts := make(chan struct{})
	tailBindingQuery := make(chan struct{})
	releaseTailQuery := make(chan struct{})
	var releaseSetOnce, releaseTailOnce sync.Once
	releaseSet := func() { releaseSetOnce.Do(func() { close(releaseSetFacts) }) }
	releaseTail := func() { releaseTailOnce.Do(func() { close(releaseTailQuery) }) }
	t.Cleanup(releaseSet)
	t.Cleanup(releaseTail)
	var setFactsOnce, tailQueryOnce sync.Once
	if err := db.Callback().Create().Before("gorm:create").Register("test:block_concurrent_set_facts", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "platforms" {
			return
		}
		setFactsOnce.Do(func() { close(setFactsEntered) })
		<-releaseSetFacts
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:block_concurrent_tail_binding_read", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "platforms" {
			return
		}
		tailQueryOnce.Do(func() { close(tailBindingQuery) })
		<-releaseTailQuery
	}); err != nil {
		t.Fatal(err)
	}
	secondAttempt := make(chan struct{})
	rdb.AddHook(&platformLockAttemptHook{key: "spider:writelock:7:LuoGu", secondAttempt: secondAttempt})
	wait := func(label string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", label)
		}
	}
	setDone := make(chan error, 1)
	go func() { setDone <- svc.executeSetSpiderMaintenance(ctx, setPending) }()
	wait("Set facts transaction", setFactsEntered)
	tailDone := make(chan error, 1)
	go func() {
		_, tailErr := svc.finalizeLuoguCleanupTailPersisted(ctx, &cleanup, luoguCleanupPayload{BindingID: 1, Username: "legacy-name", AuthorizedUID: "2245873"})
		tailDone <- tailErr
	}()
	wait("tail platform-lock contention", secondAttempt)
	select {
	case <-tailBindingQuery:
		t.Fatal("cleanup tail reached binding query before concurrent Set released the platform lock")
	default:
	}
	releaseSet()
	wait("tail binding query", tailBindingQuery)
	select {
	case err := <-setDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Set completion")
	}
	replacementKeys := []string{
		luoguSyncActiveKey(7, "2245873"),
		"spider:pending:7:LuoGu",
		"spider:inflight:7:LuoGu",
	}
	for _, key := range replacementKeys {
		if err := rdb.Set(ctx, key, "replacement", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}
	releaseTail()
	select {
	case err := <-tailDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cleanup tail")
	}
	var binding model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.Username != "998877" {
		t.Fatalf("concurrent Set binding=%q want 998877", binding.Username)
	}
	for _, key := range replacementKeys {
		if value := rdb.Get(ctx, key).Val(); value != "replacement" {
			t.Fatalf("cleanup tail cleared committed replacement key %s: %q", key, value)
		}
	}
	var current model.AbilityMaintenancePending
	if err := db.Where("scope = ?", cleanup.Scope).First(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Phase != "tail_finalized" {
		t.Fatalf("cleanup phase=%q want tail_finalized", current.Phase)
	}
}

func TestLuoguCleanupTailReplaysAfterCleanupBeforePhaseCommit(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	for _, key := range []string{
		"core:submit_log:user:7",
		"spider:pending:7:LuoGu",
		"spider:inflight:7:LuoGu",
	} {
		if err := rdb.Set(ctx, key, "stale", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}
	var failOnce sync.Once
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_luogu_tail_phase_once", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_pending" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || updates["phase"] != "tail_finalized" {
			return
		}
		fail := false
		failOnce.Do(func() { fail = true })
		if fail {
			tx.AddError(errors.New("injected tail phase commit failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err == nil || removed {
		t.Fatalf("tail phase failure did not interrupt cleanup: removed=%v err=%v", removed, err)
	}
	var pending model.AbilityMaintenancePending
	if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Phase != "fence_finalized" {
		t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
	}
	for _, key := range []string{"core:submit_log:user:7", "spider:pending:7:LuoGu", "spider:inflight:7:LuoGu"} {
		if rdb.Exists(ctx, key).Val() != 0 {
			t.Fatalf("tail cleanup did not run before phase failure: key=%s", key)
		}
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("tail replay removed=%v err=%v", removed, err)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("replayed cleanup left durable intent count=%d", pendingCount)
	}
}

func TestLuoguCleanupGenerationBumpUsesCurrentProfileOwner(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	takeoverRDB := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	t.Cleanup(func() { _ = takeoverRDB.Close() })
	generationKey := task.GenerationKey(7, "LuoGu")
	var takeoverErr error
	var startedGeneration int64
	rdb.AddHook(&afterProfileValidationHook{run: func() {
		if err := takeoverRDB.Del(ctx, "problem:user_profile:generation:user:7:lease").Err(); err != nil {
			takeoverErr = err
			return
		}
		pending, err := loadLuoguCleanupPending(ctx, db, 7)
		if err != nil {
			takeoverErr = err
			return
		}
		ownerB, err := bizservice.BeginUserProfileInvalidationForIntent(ctx, takeoverRDB, 7, pending.OperationID)
		if err != nil {
			takeoverErr = err
			return
		}
		if err := claimLuoguCleanupPending(ctx, db, pending, ownerB.Owner); err != nil {
			takeoverErr = err
			return
		}
		startedGeneration, takeoverErr = bizservice.BumpUserProfileOwnedGeneration(ctx, takeoverRDB, 7, ownerB, generationKey, 7*24*time.Hour)
		if takeoverErr == nil {
			takeoverErr = bizservice.FinishUserProfileInvalidation(ctx, takeoverRDB, 7, ownerB)
		}
	}})
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err == nil || removed {
		t.Fatalf("lost owner continued Luogu cleanup removed=%v err=%v", removed, err)
	}
	if takeoverErr != nil || startedGeneration == 0 {
		t.Fatalf("takeover failed generation=%d err=%v", startedGeneration, takeoverErr)
	}
	storedGeneration, err := rdb.Get(ctx, generationKey).Int64()
	if err != nil || storedGeneration != startedGeneration {
		t.Fatalf("lost owner bumped takeover Luogu generation stored=%d started=%d err=%v", storedGeneration, startedGeneration, err)
	}
}

func TestRemoveInvalidLuoguBindingIgnoresProfilePublisherUntilReplacementCrawl(t *testing.T) {
	svc, db, _, _, importer := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	if err := db.AutoMigrate(
		&model.Problem{}, &model.ProblemTag{}, &model.UserTagAC{}, &model.UserProblemStatus{},
		&model.AbilityModelState{}, &model.ProblemAbilityStat{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := model.Problem{Platform: "CodeForces", ExternalID: "2A", Title: "A", Tags: model.StringArray{"graph"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "graph"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 7, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	importer.profileErr = errors.New("forced profile publish failure")
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err != nil || !removed {
		t.Fatalf("cleanup should not publish a profile before replacement crawl removed=%v err=%v", removed, err)
	}
	var bindingCount, pendingCount int64
	_ = db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&bindingCount).Error
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&pendingCount).Error
	if bindingCount != 0 || pendingCount != 0 {
		t.Fatalf("cleanup did not clear durable intent: binding=%d pending=%d", bindingCount, pendingCount)
	}
	if events := importer.profileCalls; events != 0 {
		t.Fatalf("profile publisher was called during cleanup: %d", events)
	}
}

func prepareInvalidLuoguCleanupRecoveryTest(t *testing.T) (*SpiderService, *gorm.DB, *redis.Client) {
	t.Helper()
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(
		&model.Problem{}, &model.ProblemTag{}, &model.UserTagAC{}, &model.UserProblemStatus{},
		&model.AbilityModelState{}, &model.ProblemAbilityStat{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	return svc, db, rdb
}

func TestRemoveInvalidLuoguBindingDoesNotRebuildDuringCleanup(t *testing.T) {
	svc, db, _ := prepareInvalidLuoguCleanupRecoveryTest(t)
	var failOnce sync.Once
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail_luogu_rebuild_once", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "ability_model_state" {
			fail := false
			failOnce.Do(func() { fail = true })
			if fail {
				tx.AddError(errors.New("injected Luogu rebuild failure"))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873"); err != nil || !removed {
		t.Fatalf("cleanup unexpectedly rebuilt profile removed=%v err=%v", removed, err)
	}
	var bindingCount, pendingCount int64
	_ = db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&bindingCount).Error
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&pendingCount).Error
	if bindingCount != 0 || pendingCount != 0 {
		t.Fatalf("cleanup did not clear durable intent: binding=%d pending=%d", bindingCount, pendingCount)
	}
}

func TestRemoveInvalidLuoguBindingFinalizeFailureRecoversWithoutBinding(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	rdb.AddHook(&failLuoguFinalizeGetOnceHook{})
	if removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873"); err == nil || removed {
		t.Fatalf("first cleanup removed=%v err=%v", removed, err)
	}
	var bindingCount int64
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&bindingCount).Error; err != nil {
		t.Fatal(err)
	}
	if bindingCount != 0 {
		t.Fatalf("finalize failure restored deleted binding count=%d", bindingCount)
	}
	var pending model.AbilityMaintenancePending
	if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Phase != "fence_finalized" {
		t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
	}
	removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("no-binding finalize recovery removed=%v err=%v", removed, err)
	}
}

func TestRemoveInvalidLuoguBindingDoesNotFinalizeTailWhenSessionFinalizerFails(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "2245873").Error; err != nil {
		t.Fatal(err)
	}
	started := startLuoguTestSession(t, svc)
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	baseImporter := svc.luoguImporter.(*fakeLuoguImporter)
	finalizer := &fakeLuoguSessionFinalizingImporter{fakeLuoguImporter: baseImporter, err: errors.New("injected session finalizer failure")}
	svc.luoguImporter = finalizer

	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err == nil || removed {
		t.Fatalf("session finalizer failure advanced cleanup: removed=%v err=%v", removed, err)
	}
	var pending model.AbilityMaintenancePending
	if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Phase != "fence_finalized" {
		t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
	}
	if active := rdb.Get(ctx, luoguSyncActiveKey(7, "2245873")).Val(); active != started.SessionId {
		t.Fatalf("failed finalizer lost retryable session: active=%q want=%q", active, started.SessionId)
	}

	finalizer.mu.Lock()
	finalizer.err = nil
	finalizer.mu.Unlock()
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("session finalizer recovery removed=%v err=%v", removed, err)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("successful cleanup left durable intent count=%d", pendingCount)
	}
	if rdb.Exists(ctx, luoguSyncActiveKey(7, "2245873"), luoguSyncSessionKey(started.SessionId)).Val() != 0 {
		t.Fatal("recovered session finalization left Redis session keys")
	}
}

func TestRemoveInvalidLuoguBindingDoesNotFinalizeTailWhenCacheInvalidationFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		key     string
	}{
		{name: "delete", command: "del", key: "core:submit_log:user:7"},
		{name: "increment", command: "incr", key: "statistic:user:7:ver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
			rdb.AddHook(&failLuoguTailCommandOnceHook{command: tc.command, key: tc.key})
			if removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873"); err == nil || removed {
				t.Fatalf("tail command failure advanced cleanup: removed=%v err=%v", removed, err)
			}
			var pending model.AbilityMaintenancePending
			if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
				t.Fatal(err)
			}
			if pending.Phase != "fence_finalized" {
				t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
			}
			removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873")
			if err != nil || !removed {
				t.Fatalf("tail command recovery removed=%v err=%v", removed, err)
			}
			var remaining int64
			if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&remaining).Error; err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("successful cleanup left durable intent count=%d", remaining)
			}
		})
	}
}

func TestRemoveInvalidLuoguBindingClearsStaleActiveSessionReference(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	activeKey := luoguSyncActiveKey(7, "2245873")
	if err := rdb.Set(ctx, activeKey, "missing-session", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("stale active session blocked cleanup: removed=%v err=%v", removed, err)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", luoguCleanupScope(7)).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("successful cleanup left durable intent count=%d", pendingCount)
	}
	if rdb.Exists(ctx, activeKey).Val() != 0 {
		t.Fatal("stale active session reference survived cleanup")
	}
}

func TestRemoveInvalidLuoguBindingRetriesTransientSessionLoadFailure(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "2245873").Error; err != nil {
		t.Fatal(err)
	}
	started := startLuoguTestSession(t, svc)
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	rdb.AddHook(&failLuoguTailCommandOnceHook{command: "hgetall", key: luoguSyncSessionKey(started.SessionId)})
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err == nil || removed {
		t.Fatalf("transient session load failure advanced cleanup: removed=%v err=%v", removed, err)
	}
	var pending model.AbilityMaintenancePending
	if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Phase != "fence_finalized" {
		t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
	}
	if active := rdb.Get(ctx, luoguSyncActiveKey(7, "2245873")).Val(); active != started.SessionId {
		t.Fatalf("transient load failure lost retryable session: active=%q want=%q", active, started.SessionId)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("session load recovery removed=%v err=%v", removed, err)
	}
}

func TestRemoveInvalidLuoguBindingRetriesSessionRedisTerminationFailure(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "2245873").Error; err != nil {
		t.Fatal(err)
	}
	started := startLuoguTestSession(t, svc)
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "legacy-name").Error; err != nil {
		t.Fatal(err)
	}
	rdb.AddHook(&failLuoguTailCommandOnceHook{command: "eval", key: luoguSyncSessionKey(started.SessionId)})
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873"); err == nil || removed {
		t.Fatalf("session Redis termination failure advanced cleanup: removed=%v err=%v", removed, err)
	}
	var pending model.AbilityMaintenancePending
	if err := db.Where("scope = ?", luoguCleanupScope(7)).First(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending.Phase != "fence_finalized" {
		t.Fatalf("pending phase=%q want fence_finalized", pending.Phase)
	}
	if active := rdb.Get(ctx, luoguSyncActiveKey(7, "2245873")).Val(); active != started.SessionId {
		t.Fatalf("termination failure lost retryable session: active=%q want=%q", active, started.SessionId)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("session termination recovery removed=%v err=%v", removed, err)
	}
}

func TestRemoveInvalidLuoguBindingCancelsOldIntentAfterValidReplacement(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	replacementID := old.Id + 1000
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": replacementID, "username": "998877",
	}).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "998877")
	if err != nil || removed {
		t.Fatalf("replacement cancellation removed=%v err=%v", removed, err)
	}
	var current model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Id != replacementID || current.Username != "998877" {
		t.Fatalf("new binding was touched: %+v", current)
	}
	var pendingCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if pendingCount != 0 || targetCount != 0 {
		t.Fatalf("cancel tail pending=%d target=%d", pendingCount, targetCount)
	}
	generation, parseErr := strconv.ParseInt(rdb.Get(ctx, "problem:user_profile:generation:user:7").Val(), 10, 64)
	if parseErr != nil || generation%2 != 0 {
		t.Fatalf("cancelled intent left unsafe generation=%d err=%v", generation, parseErr)
	}
}

func TestRemoveInvalidLuoguBindingRecoversPersistedCancelledIntent(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	pending.LeaseOwner = "dead-owner"
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "lease_owner": pending.LeaseOwner, "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	replacementID := old.Id + 2000
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": replacementID, "username": "887766",
	}).Error; err != nil {
		t.Fatal(err)
	}
	keys := []string{
		luoguSyncActiveKey(7, "887766"),
		"spider:pending:7:LuoGu",
		"spider:inflight:7:LuoGu",
	}
	for _, key := range keys {
		if err := rdb.Set(ctx, key, "replacement", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "887766")
	if err != nil || removed {
		t.Fatalf("cancel recovery removed=%v err=%v", removed, err)
	}
	var current model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Id != replacementID || current.Username != "887766" {
		t.Fatalf("cancel recovery changed replacement: %+v", current)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error; err != nil || pendingCount != 0 {
		t.Fatalf("cancel recovery pending=%d err=%v", pendingCount, err)
	}
	for _, key := range keys {
		if value := rdb.Get(ctx, key).Val(); value != "replacement" {
			t.Fatalf("cancelled cleanup cleared replacement key %s: %q", key, value)
		}
	}
}

func TestRemoveInvalidLuoguBindingResumesCleanupWhenCancelledReplacementDisappears(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").Delete(&model.Platform{}).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("cancelled cleanup did not resume after replacement disappeared: removed=%v err=%v", removed, err)
	}
	var parentCount, targetCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&parentCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error; err != nil {
		t.Fatal(err)
	}
	if parentCount != 0 || targetCount != 0 {
		t.Fatalf("resumed cleanup left durable delivery state parent=%d target=%d", parentCount, targetCount)
	}
	generationKey := "problem:user_profile:generation:user:7"
	generation, parseErr := strconv.ParseInt(rdb.Get(ctx, generationKey).Val(), 10, 64)
	if parseErr != nil || generation%2 != 0 {
		t.Fatalf("resumed cleanup left unsafe generation=%d err=%v", generation, parseErr)
	}
	if rdb.Exists(ctx, generationKey+":lease", generationKey+":current_intent").Val() != 0 {
		t.Fatal("resumed cleanup left profile invalidation ownership keys")
	}
}

func TestRemoveInvalidLuoguBindingResumesCleanupWhenCancelledReplacementIsInvalid(t *testing.T) {
	svc, db, _ := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": old.Id + 3500, "username": "still-invalid",
	}).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "2245873")
	if err != nil || !removed {
		t.Fatalf("invalid replacement cleanup removed=%v err=%v", removed, err)
	}
	var bindingCount int64
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&bindingCount).Error; err != nil {
		t.Fatal(err)
	}
	if bindingCount != 0 {
		t.Fatalf("invalid replacement binding survived count=%d", bindingCount)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error; err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("invalid replacement cleanup left durable intent count=%d", pendingCount)
	}
}

func TestRemoveInvalidLuoguBindingAbandonsFenceWhenCancelledRevalidationFails(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": old.Id + 3600, "username": "887766",
	}).Error; err != nil {
		t.Fatal(err)
	}
	var failOnce sync.Once
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail_cancelled_revalidation_once", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "platforms" {
			return
		}
		fail := false
		failOnce.Do(func() { fail = true })
		if fail {
			tx.AddError(errors.New("injected cancelled replacement query failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "887766"); err == nil || removed {
		t.Fatalf("cancelled revalidation failure removed=%v err=%v", removed, err)
	}
	profileGenerationKey := "problem:user_profile:generation:user:7"
	profileGeneration, parseErr := strconv.ParseInt(rdb.Get(ctx, profileGenerationKey).Val(), 10, 64)
	if parseErr != nil || profileGeneration%2 != 1 {
		t.Fatalf("cancelled revalidation failure lost retryable odd generation=%d err=%v", profileGeneration, parseErr)
	}
	if rdb.Exists(ctx, profileGenerationKey+":lease").Val() != 0 {
		t.Fatal("cancelled revalidation failure left the old owner lease")
	}
	if intent := rdb.Get(ctx, profileGenerationKey+":current_intent").Val(); intent != pending.OperationID {
		t.Fatalf("cancelled revalidation failure current intent=%q want=%q", intent, pending.OperationID)
	}
	retryToken, err := bizservice.BeginUserProfileInvalidationForIntent(ctx, rdb, 7, pending.OperationID)
	if err != nil {
		t.Fatalf("cancelled revalidation failure did not release fence for retry: %v", err)
	}
	if retryToken.Generation != uint64(profileGeneration) {
		t.Fatalf("retry generation=%d want takeover of %d", retryToken.Generation, profileGeneration)
	}
	if err := bizservice.FinishUserProfileInvalidation(ctx, rdb, 7, retryToken); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveInvalidLuoguBindingPreservesReplacementRestoredDuringCancelledCleanup(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").Delete(&model.Platform{}).Error; err != nil {
		t.Fatal(err)
	}
	generationKey := task.GenerationKey(7, "LuoGu")
	var restoreErr error
	rdb.AddHook(&afterLuoguGenerationBumpHook{key: generationKey, run: func() {
		restoreErr = db.Create(&model.Platform{Id: old.Id + 4000, UserID: 7, Platform: "LuoGu", Username: "776655"}).Error
	}})
	removed, err := svc.removeInvalidLuoguBinding(ctx, 7, "776655")
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err != nil || removed {
		t.Fatalf("restored replacement cancellation removed=%v err=%v", removed, err)
	}
	var replacement model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if replacement.Username != "776655" {
		t.Fatalf("restored replacement was changed: %+v", replacement)
	}
	var parentCount, targetCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&parentCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error; err != nil {
		t.Fatal(err)
	}
	if parentCount != 0 || targetCount != 0 {
		t.Fatalf("restored replacement left cancelled intent parent=%d target=%d", parentCount, targetCount)
	}
	profileGeneration, parseErr := strconv.ParseInt(rdb.Get(ctx, "problem:user_profile:generation:user:7").Val(), 10, 64)
	if parseErr != nil || profileGeneration%2 != 0 {
		t.Fatalf("restored replacement left unsafe profile generation=%d err=%v", profileGeneration, parseErr)
	}
}

func TestRemoveInvalidLuoguBindingKeepsCancelledPlatformLockThroughFinishAndClear(t *testing.T) {
	svc, db, rdb := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var old model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&old).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, old, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
		"phase": "cancelled", "revision": gorm.Expr("revision + 1"),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Updates(map[string]interface{}{
		"id": old.Id + 3000, "username": "887766",
	}).Error; err != nil {
		t.Fatal(err)
	}
	observer := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	t.Cleanup(func() { _ = observer.Close() })
	var rivalAcquired atomic.Bool
	var probes atomic.Int32
	probeLock := func() {
		probes.Add(1)
		ok, lockErr := observer.SetNX(ctx, "spider:writelock:7:LuoGu", "rival", time.Minute).Result()
		if lockErr != nil {
			t.Errorf("observe cancelled lock: %v", lockErr)
			return
		}
		if ok {
			rivalAcquired.Store(true)
			_ = observer.Del(ctx, "spider:writelock:7:LuoGu").Err()
		}
	}
	rdb.AddHook(&cancelledFinishLockHook{run: probeLock})
	var clearProbe sync.Once
	if err := db.Callback().Delete().After("gorm:delete").Register("test:observe_cancelled_parent_clear_lock", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_pending" {
			return
		}
		clearProbe.Do(probeLock)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.removeInvalidLuoguBinding(ctx, 7, "887766"); err != nil {
		t.Fatal(err)
	}
	if rivalAcquired.Load() {
		t.Fatal("cancelled cleanup released platform lock before finish/clear")
	}
	if probes.Load() < 2 {
		t.Fatalf("cancelled cleanup lock probes=%d want finish and parent-clear probes", probes.Load())
	}
}

func TestLuoguCleanupTargetDeleteRequiresStrictCAS(t *testing.T) {
	svc, db, _ := prepareInvalidLuoguCleanupRecoveryTest(t)
	ctx := context.Background()
	var binding model.Platform
	if err := db.Where("user_id = ? AND platform = ?", 7, "LuoGu").First(&binding).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareLuoguCleanupPending(ctx, db, binding, "2245873")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER keep_luogu_target BEFORE DELETE ON ability_maintenance_targets
		WHEN OLD.intent_id = '` + pending.OperationID + `' BEGIN SELECT RAISE(IGNORE); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.clearLuoguCleanupIntent(ctx, pending); err == nil || !strings.Contains(err.Error(), "target owner changed") {
		t.Fatalf("non-CAS target delete err=%v", err)
	}
	var pendingCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if pendingCount != 1 || targetCount != 1 {
		t.Fatalf("failed target CAS partially cleared pending=%d target=%d", pendingCount, targetCount)
	}
}

func TestLuoguCleanupStageRejectsStaleTargetRevisionAndState(t *testing.T) {
	svc, db, _ := prepareInvalidLuoguCleanupRecoveryTest(t)
	var injected atomic.Bool
	if err := db.Callback().Update().Before("gorm:update").Register("test:takeover_luogu_target_before_stage", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_targets" || !injected.CompareAndSwap(false, true) {
			return
		}
		if err := db.Model(&model.AbilityMaintenanceTarget{}).
			Where("user_id = ?", 7).
			Updates(map[string]interface{}{"state": "claimed", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeInvalidLuoguBinding(context.Background(), 7, "2245873")
	if err == nil || removed {
		t.Fatalf("stale Luogu target stage removed=%v err=%v", removed, err)
	}
	var pending model.AbilityMaintenancePending
	if err := db.First(&pending, "scope = ?", luoguCleanupScope(7)).Error; err != nil {
		t.Fatalf("stale stage lost pending: %v", err)
	}
	var target model.AbilityMaintenanceTarget
	if err := db.First(&target, "intent_id = ? AND user_id = ?", pending.OperationID, int64(7)).Error; err != nil {
		t.Fatal(err)
	}
	if target.State != "claimed" || target.Revision != 2 {
		t.Fatalf("stale stage overwrote target owner: %+v", target)
	}
}

func luoguReason(err error) string {
	if err == nil {
		return ""
	}
	return kratoserrors.FromError(err).Reason
}

func startLuoguTestSession(t *testing.T, svc *SpiderService) *spiderpb.StartLuoguSyncRes {
	return startLuoguTestSessionWithRequestID(t, svc, strings.Repeat("a", 43))
}

func startLuoguTestSessionWithRequestID(t *testing.T, svc *SpiderService, requestID string) *spiderpb.StartLuoguSyncRes {
	t.Helper()
	res, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: requestID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLuoguSyncStartReplayAfterArbitraryDelayKeepsIssuedToken(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	first := startLuoguTestSession(t, svc)
	clock.Sleep(10 * time.Minute)

	replayed := startLuoguTestSession(t, svc)
	if replayed.SessionId != first.SessionId || replayed.SessionToken != first.SessionToken {
		t.Fatalf("start replay changed issuance: first=%+v replayed=%+v", first, replayed)
	}
	if _, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, first.SessionToken), &spiderpb.LuoguSyncStatusReq{}); err != nil {
		t.Fatalf("issued token was invalidated by replay: %v", err)
	}
}

func TestLuoguSyncStartCannotChangeTokenWhilePageIsInFlight(t *testing.T) {
	svc, _, _, clock, importer := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	clock.Sleep(10 * time.Minute)
	entered := make(chan struct{})
	release := make(chan struct{})
	importer.entered, importer.release = entered, release
	pageDone := make(chan error, 1)
	go func() {
		_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
			LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
		})
		pageDone <- err
	}()
	<-entered

	replayed := startLuoguTestSession(t, svc)
	if replayed.SessionToken != started.SessionToken {
		t.Fatalf("in-flight page token changed: started=%s replayed=%s", started.SessionToken, replayed.SessionToken)
	}
	close(release)
	if err := <-pageDone; err != nil {
		t.Fatalf("page failed after idempotent start: %v", err)
	}
}

func TestLuoguSyncStartRecoveryCooldownAndTTL(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	first := startLuoguTestSession(t, svc)
	if first.Resumed || first.NextPage != 1 || first.PageDelayMs != 500 || first.SessionToken == "" {
		t.Fatalf("first=%+v", first)
	}
	ttl, err := rdb.TTL(context.Background(), luoguSyncSessionKey(first.SessionId)).Result()
	if err != nil || ttl < 29*time.Minute || ttl > 30*time.Minute {
		t.Fatalf("ttl=%v err=%v", ttl, err)
	}

	resumed := startLuoguTestSession(t, svc)
	if !resumed.Resumed || resumed.SessionId != first.SessionId || resumed.SessionToken != first.SessionToken {
		t.Fatalf("resumed=%+v first=%+v", resumed, first)
	}
	status, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, resumed.SessionToken), &spiderpb.LuoguSyncStatusReq{})
	if err != nil || status.SessionId != first.SessionId || status.NextPage != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	_, err = svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, resumed.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("b", 43)})
	if luoguReason(err) != "SYNC_COOLDOWN" || kratoserrors.FromError(err).Code != http.StatusTooManyRequests {
		t.Fatalf("cooldown err=%v", err)
	}
}

func TestLuoguSyncInvalidStartDoesNotConsumeCooldown(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	now := time.Now().UTC()
	for _, value := range []interface{}{
		&model.SubmitLog{UserID: 7, Platform: "LuoGu", SubmitID: "100", Time: now},
		&model.ContestLog{UserID: 7, Platform: "LuoGu", ContestId: "1", Time: now},
		&model.ContestUserProblem{UserID: 7, Platform: "LuoGu", ContestID: "1", ExternalID: "P1000"},
		&model.DailyUserStat{UserID: 7, Platform: "LuoGu", Day: now, SubmitCnt: 1},
		&model.UserACProblem{UserID: 7, Platform: "LuoGu", ProblemKey: "e:LuoGu:P1000", FirstACAt: now},
		&model.UserACProblemDay{UserID: 7, Platform: "LuoGu", ProblemKey: "e:LuoGu:P1000", Day: now},
		&model.SpiderRepairState{UserID: 7, Platform: "LuoGu", RepairKey: "test", Version: 1, CompletedAt: now},
		&model.SubmitLog{UserID: 7, Platform: "CodeForces", SubmitID: "200", Time: now},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "not-a-uid").Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("a", 43)})
	if luoguReason(err) != "LUOGU_BINDING_INVALID_REMOVED" {
		t.Fatalf("err=%v", err)
	}
	if n, _ := rdb.Exists(context.Background(), luoguSyncCooldownKey(7, "2245873")).Result(); n != 0 {
		t.Fatal("invalid start consumed cooldown")
	}
	if generation, err := task.CurrentGeneration(context.Background(), rdb, 7, "LuoGu"); err != nil || generation != 3 {
		t.Fatalf("generation=%d err=%v", generation, err)
	}
	for _, value := range []interface{}{
		&model.Platform{}, &model.SubmitLog{}, &model.ContestLog{}, &model.DailyUserStat{},
		&model.UserACProblem{}, &model.UserACProblemDay{}, &model.SpiderRepairState{}, &model.ContestUserProblem{},
	} {
		var count int64
		if err := db.Model(value).Where("user_id = ? AND platform = ?", 7, "LuoGu").Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("uncleared %T count=%d err=%v", value, count, err)
		}
	}
	var otherCount int64
	if err := db.Model(&model.SubmitLog{}).Where("user_id = ? AND platform = ?", 7, "CodeForces").Count(&otherCount).Error; err != nil || otherCount != 1 {
		t.Fatalf("other platform count=%d err=%v", otherCount, err)
	}
}

func TestLuoguSyncInfersFirstBrowserCheckpointFromExistingSubmits(t *testing.T) {
	svc, db, _, clock, _ := newLuoguSyncServiceTest(t)
	for index, id := range []string{"9", "100", "not-numeric"} {
		if err := db.Create(&model.SubmitLog{
			UserID: 7, Platform: "LuoGu", SubmitID: id,
			Time: clock.Now().Add(time.Duration(index) * time.Minute),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	started := startLuoguTestSession(t, svc)
	state, err := svc.loadLuoguSessionByID(context.Background(), started.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if state.OldCheckpoint != "100" {
		t.Fatalf("old checkpoint=%q want 100", state.OldCheckpoint)
	}
	records := make([]*spiderpb.LuoguSyncRecord, 0, 20)
	for id := 119; id >= 100; id-- {
		records = append(records, luoguRecord(strconv.Itoa(id), clock.Now().Unix()))
	}
	res, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 200, PerPage: 20, Records: records,
	})
	if err != nil || !res.Done || res.CompletionReason != "checkpoint" || res.ProcessedPages != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestLuoguSyncAcceptsCurrentLuoguRecordEnums(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	record := luoguRecord("100", clock.Now().Unix())
	record.Status = 21
	record.Problem.Difficulty = 8
	res, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 1, PerPage: 20, Records: []*spiderpb.LuoguSyncRecord{record},
	})
	if err != nil || !res.Done {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestLuoguSyncGenerationFailureDoesNotConsumeCooldown(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	rdb.AddHook(failLuoguGenerationHook{})
	_, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("a", 43),
	})
	if luoguReason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("err=%v reason=%q", err, luoguReason(err))
	}
	if exists, _ := rdb.Exists(context.Background(), luoguSyncCooldownKey(7, "2245873")).Result(); exists != 0 {
		t.Fatal("generation failure consumed cooldown")
	}
}

func TestLuoguSyncStartKeepsAuthorizationAfterClientUpgrade(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	res, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "2.0.0", RequestId: strings.Repeat("a", 43),
	})
	if err != nil || res == nil || res.SessionToken == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if exists, _ := rdb.Exists(context.Background(), luoguSyncCooldownKey(7, "2245873")).Result(); exists != 1 {
		t.Fatal("upgraded client did not create a session")
	}
}

func TestLuoguSyncConcurrentStartCreatesOneSession(t *testing.T) {
	svc, _, _, _, _ := newLuoguSyncServiceTest(t)
	type result struct {
		res *spiderpb.StartLuoguSyncRes
		err error
	}
	out := make(chan result, 2)
	for i := range 2 {
		go func(requestID string) {
			res, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: requestID})
			out <- result{res: res, err: err}
		}(strings.Repeat(string(rune('a'+i)), 43))
	}
	a, b := <-out, <-out
	results := []result{a, b}
	var winner *spiderpb.StartLuoguSyncRes
	losers := 0
	for _, got := range results {
		if got.err == nil {
			if winner != nil {
				t.Fatalf("two successful concurrent starts: a=%+v/%v b=%+v/%v", a.res, a.err, b.res, b.err)
			}
			winner = got.res
			continue
		}
		if luoguReason(got.err) != "SYNC_IN_PROGRESS" {
			t.Fatalf("unexpected loser: res=%+v err=%v", got.res, got.err)
		}
		losers++
	}
	if winner == nil || losers != 1 || winner.Resumed {
		t.Fatalf("winner=%+v losers=%d", winner, losers)
	}
	if _, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, winner.SessionToken), &spiderpb.LuoguSyncStatusReq{}); err != nil {
		t.Fatalf("winning token was rotated before use: %v", err)
	}
}

func TestLuoguSyncChecksDuplicateBindingRevocationAndGeneration(t *testing.T) {
	t.Run("duplicate binding", func(t *testing.T) {
		svc, db, _, _, _ := newLuoguSyncServiceTest(t)
		if err := db.Create(&model.Platform{UserID: 8, Platform: "LuoGu", Username: "2245873"}).Error; err != nil {
			t.Fatal(err)
		}
		_, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("a", 43)})
		if luoguReason(err) != "LUOGU_UID_ALREADY_BOUND" {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("revoked on status", func(t *testing.T) {
		svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
		started := startLuoguTestSession(t, svc)
		if err := rdb.Set(context.Background(), "luogu:plugin:authorization:revoked:11", "1", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
		_, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.LuoguSyncStatusReq{})
		if luoguReason(err) != "GOALGO_CONNECT_REQUIRED" {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("generation on page", func(t *testing.T) {
		svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
		started := startLuoguTestSession(t, svc)
		if err := rdb.Incr(context.Background(), task.GenerationKey(7, "LuoGu")).Err(); err != nil {
			t.Fatal(err)
		}
		_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20})
		if luoguReason(err) != "SESSION_EXPIRED" {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestTerminateLuoguSessionOnlyDeletesOwnedActivePointer(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	old := startLuoguTestSession(t, svc)
	oldState, err := svc.loadLuoguSessionByID(context.Background(), old.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Del(context.Background(), luoguSyncActiveKey(7, "2245873"), luoguSyncCooldownKey(7, "2245873")).Err(); err != nil {
		t.Fatal(err)
	}
	newer := startLuoguTestSessionWithRequestID(t, svc, strings.Repeat("b", 43))

	svc.terminateLuoguSession(context.Background(), oldState)
	active, err := rdb.Get(context.Background(), luoguSyncActiveKey(7, "2245873")).Result()
	if err != nil || active != newer.SessionId {
		t.Fatalf("active=%q want=%q err=%v", active, newer.SessionId, err)
	}
}

func TestCompletedLuoguSessionOnlyDeletesOwnedActivePointer(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	state, err := svc.loadLuoguSessionByID(context.Background(), started.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), luoguSyncActiveKey(7, "2245873"), "newer-session", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	state.Done = true
	if err := svc.storeLuoguSession(context.Background(), state, true); err != nil {
		t.Fatal(err)
	}
	active, err := rdb.Get(context.Background(), luoguSyncActiveKey(7, "2245873")).Result()
	if err != nil || active != "newer-session" {
		t.Fatalf("active=%q err=%v", active, err)
	}
}

func TestLuoguSyncPageRechecksRevocationInsideSessionLock(t *testing.T) {
	svc, _, rdb, _, importer := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	rdb.AddHook(&beforeLuoguSessionLockHook{fn: func() {
		if err := rdb.Set(context.Background(), "luogu:plugin:authorization:revoked:11", "1", time.Hour).Err(); err != nil {
			t.Errorf("set revoke marker: %v", err)
		}
	}})

	_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if luoguReason(err) != "GOALGO_CONNECT_REQUIRED" {
		t.Fatalf("err=%v", err)
	}
	if importer.calls != 0 {
		t.Fatalf("revoked request imported %d pages", importer.calls)
	}
}

func TestLuoguSyncPageRechecksAllAuthorizationStateInsideSessionLock(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		mutate func(*testing.T, *gorm.DB, *redis.Client, *spiderpb.StartLuoguSyncRes)
	}{
		{
			name: "rotated token", reason: "SESSION_EXPIRED",
			mutate: func(t *testing.T, _ *gorm.DB, rdb *redis.Client, started *spiderpb.StartLuoguSyncRes) {
				oldHash := hashLuoguSessionToken(started.SessionToken)
				newHash := hashLuoguSessionToken("replacement-session-token")
				if err := rdb.Del(context.Background(), luoguSyncTokenKey(oldHash)).Err(); err != nil {
					t.Fatal(err)
				}
				if err := rdb.HSet(context.Background(), luoguSyncSessionKey(started.SessionId), "token_hash", newHash).Err(); err != nil {
					t.Fatal(err)
				}
				if err := rdb.Set(context.Background(), luoguSyncTokenKey(newHash), started.SessionId, time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed binding", reason: "LUOGU_UID_MISMATCH",
			mutate: func(t *testing.T, db *gorm.DB, _ *redis.Client, _ *spiderpb.StartLuoguSyncRes) {
				if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "999").Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed generation", reason: "SESSION_EXPIRED",
			mutate: func(t *testing.T, _ *gorm.DB, rdb *redis.Client, _ *spiderpb.StartLuoguSyncRes) {
				if err := rdb.Incr(context.Background(), task.GenerationKey(7, "LuoGu")).Err(); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc, db, rdb, _, importer := newLuoguSyncServiceTest(t)
			started := startLuoguTestSession(t, svc)
			rdb.AddHook(&beforeLuoguSessionLockHook{fn: func() { test.mutate(t, db, rdb, started) }})
			_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
				LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
			})
			if luoguReason(err) != test.reason {
				t.Fatalf("err=%v reason=%q", err, luoguReason(err))
			}
			if importer.calls != 0 {
				t.Fatalf("invalidated request imported %d pages", importer.calls)
			}
		})
	}
}

func TestLuoguSyncDefersPostProcessUntilCompletionOrTermination(t *testing.T) {
	svc, _, rdb, clock, importer := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	records := make([]*spiderpb.LuoguSyncRecord, 0, 20)
	for id := 0; id < 20; id++ {
		records = append(records, luoguRecord(strconv.Itoa(80_000-id), clock.Now().Unix()))
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 21, PerPage: 20, Records: records,
	}); err != nil {
		t.Fatal(err)
	}
	if importer.postProcessed != 0 {
		t.Fatalf("postprocess ran per-page: %d", importer.postProcessed)
	}
	if err := rdb.Set(context.Background(), "luogu:plugin:authorization:revoked:11", "1", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.LuoguSyncStatusReq{}); luoguReason(err) != "GOALGO_CONNECT_REQUIRED" {
		t.Fatalf("err=%v", err)
	}
	if importer.postProcessed != 1 {
		t.Fatalf("termination postprocess=%d want=1", importer.postProcessed)
	}
}

func TestLuoguSyncMapsImporterGenerationRaceToSessionExpired(t *testing.T) {
	svc, _, rdb, _, importer := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	importer.err = kratoserrors.Conflict("SYNC_BINDING_CHANGED", "binding changed")

	_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if luoguReason(err) != "SESSION_EXPIRED" {
		t.Fatalf("err=%v reason=%q", err, luoguReason(err))
	}
	if exists, _ := rdb.Exists(context.Background(), luoguSyncSessionKey(started.SessionId)).Result(); exists != 0 {
		t.Fatal("stale generation session remained active")
	}
}

func luoguRecord(id string, at int64) *spiderpb.LuoguSyncRecord {
	return &spiderpb.LuoguSyncRecord{
		SubmitId: id, SubmitTime: at, Status: 12, Language: 28,
		Problem: &spiderpb.LuoguSyncProblem{Pid: "P1001", Title: "A+B Problem", Difficulty: 1},
	}
}

func TestLuoguSyncPageCheckpointIdempotencyAndThrottle(t *testing.T) {
	svc, db, _, clock, importer := newLuoguSyncServiceTest(t)
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("client_sync_head_submit_id", "90").Error; err != nil {
		t.Fatal(err)
	}
	started := startLuoguTestSession(t, svc)
	req := &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 21, PerPage: 20,
		Records: []*spiderpb.LuoguSyncRecord{luoguRecord("100", clock.Now().Unix())},
	}
	for id := 99; id >= 81; id-- {
		req.Records = append(req.Records, luoguRecord(time.Unix(int64(id), 0).Format("05"), clock.Now().Unix()))
	}
	first, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req)
	if err != nil || first.Done || first.NextPage != 2 || first.PageInserted != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	retry, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req)
	if err != nil || retry.String() != first.String() || importer.calls != 1 {
		t.Fatalf("retry=%+v err=%v calls=%d", retry, err, importer.calls)
	}
	before := clock.Now()
	second := &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 2, RemoteCount: 21, PerPage: 20, Records: []*spiderpb.LuoguSyncRecord{luoguRecord("90", clock.Now().Unix())}}
	done, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), second)
	if err != nil || !done.Done || !done.Connected || done.CompletionReason != "checkpoint" {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	if clock.Now().Sub(before) != 500*time.Millisecond {
		t.Fatalf("server throttle advanced=%v", clock.Now().Sub(before))
	}
	if len(importer.completed) != 1 || importer.completed[0] != "100" {
		t.Fatalf("completed=%v", importer.completed)
	}
	changedAfterDone := &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 2, RemoteCount: 21, PerPage: 20, Records: []*spiderpb.LuoguSyncRecord{luoguRecord("89", clock.Now().Unix())}}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), changedAfterDone); luoguReason(err) != "SESSION_EXPIRED" {
		t.Fatalf("changed completed page err=%v", err)
	}
}

func TestLuoguSyncRemoteEndAndRestartLimit(t *testing.T) {
	t.Run("empty remote end", func(t *testing.T) {
		svc, _, _, _, importer := newLuoguSyncServiceTest(t)
		started := startLuoguTestSession(t, svc)
		res, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20})
		if err != nil || !res.Done || !res.Connected || res.CompletionReason != "remote_end" || len(importer.completed) != 1 || importer.completed[0] != "" {
			t.Fatalf("res=%+v completed=%v err=%v", res, importer.completed, err)
		}
	})
	t.Run("record count changes", func(t *testing.T) {
		svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
		started := startLuoguTestSession(t, svc)
		page := func(count int32) (*spiderpb.UploadLuoguSyncPageRes, error) {
			records := []*spiderpb.LuoguSyncRecord{luoguRecord("100", clock.Now().Unix())}
			for len(records) < 20 {
				records = append(records, luoguRecord(time.Unix(int64(100-len(records)), 0).Format("05"), clock.Now().Unix()))
			}
			return svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: count, PerPage: 20, Records: records})
		}
		if _, err := page(21); err != nil {
			t.Fatal(err)
		}
		for n := int32(22); n <= 24; n++ {
			res, err := page(n)
			if err != nil || !res.Restart || res.NextPage != 1 {
				t.Fatalf("n=%d res=%+v err=%v", n, res, err)
			}
		}
		_, err := page(25)
		if luoguReason(err) != "LUOGU_RECORDS_CHANGED" {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLuoguSyncRejectsOutOfOrderPageBeforeRestartDetection(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	page := func(number, count int32) *spiderpb.UploadLuoguSyncPageReq {
		records := make([]*spiderpb.LuoguSyncRecord, 0, 10)
		for id := int32(0); id < 10; id++ {
			records = append(records, luoguRecord(strconv.FormatInt(10_000+int64(number*100-id), 10), clock.Now().Unix()))
		}
		return &spiderpb.UploadLuoguSyncPageReq{
			LuoguUid: "2245873", Page: number, RemoteCount: count, PerPage: 10, Records: records,
		}
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), page(1, 30)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), page(3, 31)); luoguReason(err) != "LUOGU_LAYOUT_CHANGED" {
		t.Fatalf("err=%v", err)
	}
	state, err := svc.loadLuoguSessionByID(context.Background(), started.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if state.Restarts != 0 || state.ExpectedPage != 2 {
		t.Fatalf("restarts=%d expectedPage=%d", state.Restarts, state.ExpectedPage)
	}
}

func TestLuoguSyncRestartsWhenPerPageChanges(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	records := func(count int, base int) []*spiderpb.LuoguSyncRecord {
		out := make([]*spiderpb.LuoguSyncRecord, 0, count)
		for index := 0; index < count; index++ {
			out = append(out, luoguRecord(strconv.Itoa(base-index), clock.Now().Unix()))
		}
		return out
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 30, PerPage: 10, Records: records(10, 50_000),
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 2, RemoteCount: 30, PerPage: 15, Records: records(15, 49_000),
	})
	if err != nil || !restarted.Restart || restarted.NextPage != 1 {
		t.Fatalf("restarted=%+v err=%v", restarted, err)
	}
}

func TestLuoguSyncStatusKeepsRemotePerPage(t *testing.T) {
	svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 3, PerPage: 2,
		Records: []*spiderpb.LuoguSyncRecord{luoguRecord("3", clock.Now().Unix()), luoguRecord("2", clock.Now().Unix())},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.LuoguSyncStatusReq{})
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalPages != 2 {
		t.Fatalf("totalPages=%d want=2", status.TotalPages)
	}
}

func TestLuoguSyncStatusRestoresCompletedResponse(t *testing.T) {
	svc, _, _, _, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	completed, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if err != nil || !completed.Done {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	status, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.LuoguSyncStatusReq{})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Done || !status.Connected || status.CompletionReason != "remote_end" || status.Inserted != completed.Inserted || status.ProcessedPages != completed.ProcessedPages {
		t.Fatalf("status=%+v completed=%+v", status, completed)
	}
}

func TestLuoguSyncCreatesAndCompletesSessionAudit(t *testing.T) {
	svc, db, rdb, _, _ := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(
		&model.ClientSyncAudit{}, &model.ClientSyncPageReceipt{}, &model.ClientSyncPostProcessJob{},
	); err != nil {
		t.Fatal(err)
	}
	svc.luoguImporter = bizservice.NewSpiderUseCase(&coredata.Data{DB: db, RDB: rdb}, nil, nil)
	started := startLuoguTestSession(t, svc)
	var running model.ClientSyncAudit
	if err := db.First(&running, "session_id = ?", started.SessionId).Error; err != nil {
		t.Fatal(err)
	}
	if running.Status != "running" || running.AuthorizationID != 11 || running.UserID != 7 || running.OJUID != "2245873" || running.ClientVersion != "1.0.0" {
		t.Fatalf("running audit = %+v", running)
	}
	completed, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20})
	if err != nil || !completed.Done {
		t.Fatalf("completion=%+v err=%v", completed, err)
	}
	var audit model.ClientSyncAudit
	if err := db.First(&audit, "session_id = ?", started.SessionId).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Status != "completed" || audit.CompletionReason != "remote_end" || audit.ProcessedPages != 1 || audit.TerminalAt == nil || !audit.RetentionUntil.Equal(audit.TerminalAt.Add(7*24*time.Hour)) {
		t.Fatalf("completed audit = %+v", audit)
	}
}

func TestLuoguSyncPurgeRemovesIndexedSessionKeys(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	ctx := context.Background()
	if err := rdb.SAdd(ctx, luoguSyncUserSessionsKey(7), "session-to-delete").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, luoguSyncUserUIDsKey(7), "2245873").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, luoguSyncActiveKey(7, "2245873"), "session-to-delete", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HSet(ctx, luoguSyncSessionKey("session-to-delete"), "user_id", 7, "token_hash", "session-hash", "authorization_id", 11, "luogu_uid", "2245873", "request_id_hash", "request-hash").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, luoguSyncTokenKey("session-hash"), "session-to-delete", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, luoguSyncIssuanceKey(11, "request-hash"), "session-to-delete", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, luoguSyncCooldownKey(7, "2245873"), "1", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, luoguSyncLockKey("session-to-delete"), "lock", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.purgeLuoguSyncRedis(ctx, 7); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{luoguSyncUserSessionsKey(7), luoguSyncUserUIDsKey(7), luoguSyncActiveKey(7, "2245873"), luoguSyncSessionKey("session-to-delete"), luoguSyncTokenKey("session-hash"), luoguSyncIssuanceKey(11, "request-hash"), luoguSyncCooldownKey(7, "2245873"), luoguSyncLockKey("session-to-delete")} {
		if rdb.Exists(ctx, key).Val() != 0 {
			t.Fatalf("key still exists: %s", key)
		}
	}
}

func TestExpiredLuoguSyncAuditRejectsPageBeforeImport(t *testing.T) {
	svc, db, _, _, importer := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	started := startLuoguTestSession(t, svc)
	now := time.Now().UTC()
	if err := db.Create(&model.ClientSyncAudit{SessionID: started.SessionId, AuthorizationID: 11, UserID: 7, Platform: "luogu", OJUID: "2245873", ClientKind: "userscript", ClientVersion: "1.0.0", Status: "expired", StartedAt: now, UpdatedAt: now, TerminalAt: &now, RetentionUntil: now.Add(7 * 24 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20})
	if luoguReason(err) != "SESSION_EXPIRED" {
		t.Fatalf("expired page reason = %q err=%v", luoguReason(err), err)
	}
	if importer.calls != 0 {
		t.Fatalf("expired page imported records: calls=%d", importer.calls)
	}
}

func TestLuoguSyncImporterInternalErrorWritesFailedAudit(t *testing.T) {
	svc, db, _, _, importer := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(&model.ClientSyncAudit{}); err != nil {
		t.Fatal(err)
	}
	importer.auditDB = db
	started := startLuoguTestSession(t, svc)
	importer.err = errors.New("database unavailable")
	state, err := svc.loadLuoguSessionByID(context.Background(), started.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.mapLuoguImporterError(context.Background(), state, errors.New("database unavailable"))
	var audit model.ClientSyncAudit
	if err := db.First(&audit, "session_id = ?", started.SessionId).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Status != "failed" || audit.ErrorMessage == "" {
		t.Fatalf("internal importer error was not audited: %+v", audit)
	}
}

func TestLuoguSyncPageRecoversAfterDatabaseCommitAndRedisCheckpointFailure(t *testing.T) {
	svc, db, rdb, clock, _ := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}, &model.ClientSyncPageReceipt{}, &model.ClientSyncPostProcessJob{},
	); err != nil {
		t.Fatal(err)
	}
	svc.luoguImporter = bizservice.NewSpiderUseCase(&coredata.Data{DB: db, RDB: rdb}, nil, nil)
	started := startLuoguTestSession(t, svc)
	rdb.AddHook(&failLuoguCheckpointOnceHook{})
	req := &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 1, PerPage: 20,
		Records: []*spiderpb.LuoguSyncRecord{luoguRecord("70001", clock.Now().Unix())},
	}

	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req); luoguReason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("first err=%v", err)
	}
	recovered, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Done || recovered.PageInserted != 1 || recovered.Inserted != 1 || recovered.ProcessedPages != 1 {
		t.Fatalf("recovered=%+v", recovered)
	}
	var submitCount int64
	if err := db.Model(&model.SubmitLog{}).Where("platform = ? AND submit_id = ?", "LuoGu", "70001").Count(&submitCount).Error; err != nil {
		t.Fatal(err)
	}
	var daily model.DailyUserStat
	if err := db.First(&daily, "user_id = ? AND platform = ?", 7, "LuoGu").Error; err != nil {
		t.Fatal(err)
	}
	if submitCount != 1 || daily.SubmitCnt != 1 || daily.AcCnt != 1 {
		t.Fatalf("submitCount=%d daily=%+v", submitCount, daily)
	}
}

func TestLuoguSyncChangedPageAfterCommittedReceiptUsesDeclaredRestart(t *testing.T) {
	svc, db, rdb, clock, _ := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}, &model.ClientSyncPageReceipt{}, &model.ClientSyncPostProcessJob{},
	); err != nil {
		t.Fatal(err)
	}
	svc.luoguImporter = bizservice.NewSpiderUseCase(&coredata.Data{DB: db, RDB: rdb}, nil, nil)
	started := startLuoguTestSession(t, svc)
	rdb.AddHook(&failLuoguCheckpointOnceHook{})
	req := &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 2, PerPage: 1,
		Records: []*spiderpb.LuoguSyncRecord{luoguRecord("71001", clock.Now().Unix())},
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req); luoguReason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("first err=%v", err)
	}
	changed := *req
	changed.Records = []*spiderpb.LuoguSyncRecord{luoguRecord("71002", clock.Now().Unix())}
	restarted, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &changed)
	if err != nil {
		t.Fatalf("changed receipt leaked internal error: %v reason=%q", err, luoguReason(err))
	}
	if !restarted.Restart || restarted.NextPage != 1 {
		t.Fatalf("changed committed receipt did not restart: %+v", restarted)
	}
}

func TestLuoguSyncReceiptReplayRepairsFailedCacheInvalidation(t *testing.T) {
	svc, db, rdb, clock, _ := newLuoguSyncServiceTest(t)
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}, &model.ClientSyncPageReceipt{}, &model.ClientSyncPostProcessJob{},
	); err != nil {
		t.Fatal(err)
	}
	svc.luoguImporter = bizservice.NewSpiderUseCase(&coredata.Data{DB: db, RDB: rdb}, nil, nil)
	started := startLuoguTestSession(t, svc)
	hook := &failLuoguCacheAndCheckpointOnceHook{}
	rdb.AddHook(hook)
	req := &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 1, PerPage: 20,
		Records: []*spiderpb.LuoguSyncRecord{luoguRecord("72001", clock.Now().Unix())},
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req); luoguReason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("first err=%v", err)
	}
	if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req); err != nil {
		t.Fatal(err)
	}
	version, err := rdb.Get(context.Background(), "statistic:user:7:ver").Int64()
	if err != nil || version < 1 {
		t.Fatalf("receipt replay did not repair cache version: version=%d err=%v", version, err)
	}
}

func TestLuoguSyncCompletionCheckpointAndActiveDeleteAreAtomic(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	rdb.AddHook(&commitLuoguCheckpointThenFailOnceHook{})
	_, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if luoguReason(err) != "SYNC_UNAVAILABLE" {
		t.Fatalf("ambiguous completion err=%v", err)
	}
	active, getErr := rdb.Get(context.Background(), luoguSyncActiveKey(7, "2245873")).Result()
	if getErr != redis.Nil {
		t.Fatalf("completed session remained active: active=%q err=%v", active, getErr)
	}
}

func TestLuoguSyncStartRecognizesLegacyCompletedActive(t *testing.T) {
	svc, _, rdb, clock, _ := newLuoguSyncServiceTest(t)
	started := startLuoguTestSession(t, svc)
	completed, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), &spiderpb.UploadLuoguSyncPageReq{
		LuoguUid: "2245873", Page: 1, RemoteCount: 0, PerPage: 20,
	})
	if err != nil || !completed.Done {
		t.Fatalf("complete=%+v err=%v", completed, err)
	}
	if err := rdb.Set(context.Background(), luoguSyncActiveKey(7, "2245873"), started.SessionId, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	clock.Sleep(10 * time.Minute)
	replayed := startLuoguTestSession(t, svc)
	if replayed.SessionId != started.SessionId || replayed.SessionToken != started.SessionToken {
		t.Fatalf("completed issuance was rotated: started=%+v replayed=%+v", started, replayed)
	}
	if n, err := rdb.Exists(context.Background(), luoguSyncActiveKey(7, "2245873")).Result(); err != nil || n != 0 {
		t.Fatalf("completed active pointer not cleaned: n=%d err=%v", n, err)
	}
	status, err := svc.LuoguSyncStatus(luoguHeaderContext(luoguSyncSessionHeader, replayed.SessionToken), &spiderpb.LuoguSyncStatusReq{})
	if err != nil || !status.Done || !status.Connected {
		t.Fatalf("completed issuance was not recoverable: status=%+v err=%v", status, err)
	}
}

func TestLuoguCleanupRecoveryRotatesFailedOldestBatch(t *testing.T) {
	svc, db, _, _, _ := newLuoguSyncServiceTest(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for i := 0; i < 51; i++ {
		createdAt := base.Add(time.Duration(i) * time.Second)
		pending := model.AbilityMaintenancePending{
			Scope: fmt.Sprintf("luogu-cleanup:starvation:%02d", i), OperationID: fmt.Sprintf("luogu-starvation-%02d", i),
			Revision: 1, Phase: "intent", Operation: "luogu_cleanup", Payload: "{", CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := db.Create(&pending).Error; err != nil {
			t.Fatal(err)
		}
	}
	firstAttempt := time.Now()
	svc.recoverPendingLuoguCleanups(context.Background())
	var first, late model.AbilityMaintenancePending
	if err := db.First(&first, "scope = ?", "luogu-cleanup:starvation:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&late, "scope = ?", "luogu-cleanup:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if first.UpdatedAt.Before(firstAttempt) {
		t.Fatalf("first failed attempt was not rotated: %v", first.UpdatedAt)
	}
	if !late.UpdatedAt.Equal(base.Add(50 * time.Second)) {
		t.Fatalf("late intent entered first batch: %v", late.UpdatedAt)
	}
	secondAttempt := time.Now()
	svc.recoverPendingLuoguCleanups(context.Background())
	if err := db.First(&late, "scope = ?", "luogu-cleanup:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if late.UpdatedAt.Before(secondAttempt) {
		t.Fatalf("late intent remained starved: %v", late.UpdatedAt)
	}
}

func TestLuoguSyncRejectsInvalidPages(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*spiderpb.UploadLuoguSyncPageReq)
	}{
		{name: "wrong uid", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.LuoguUid = "8" }},
		{name: "out of order", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Page = 2 }},
		{name: "over twenty", mut: func(r *spiderpb.UploadLuoguSyncPageReq) {
			r.Records = append(r.Records, luoguRecord("999", r.Records[0].SubmitTime))
		}},
		{name: "duplicate id", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Records[1].SubmitId = r.Records[0].SubmitId }},
		{name: "future time", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Records[0].SubmitTime += 3600 }},
		{name: "bad status", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Records[0].Status = 99 }},
		{name: "bad language", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Records[0].Language = 99 }},
		{name: "long title", mut: func(r *spiderpb.UploadLuoguSyncPageReq) { r.Records[0].Problem.Title = string(make([]byte, 513)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _, clock, _ := newLuoguSyncServiceTest(t)
			started := startLuoguTestSession(t, svc)
			req := &spiderpb.UploadLuoguSyncPageReq{LuoguUid: "2245873", Page: 1, RemoteCount: 20, PerPage: 20}
			for id := 1; id <= 20; id++ {
				req.Records = append(req.Records, luoguRecord(time.Unix(int64(id), 0).Format("05"), clock.Now().Unix()))
			}
			tt.mut(req)
			if _, err := svc.UploadLuoguSyncPage(luoguHeaderContext(luoguSyncSessionHeader, started.SessionToken), req); luoguReason(err) != "LUOGU_LAYOUT_CHANGED" && luoguReason(err) != "LUOGU_UID_MISMATCH" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
