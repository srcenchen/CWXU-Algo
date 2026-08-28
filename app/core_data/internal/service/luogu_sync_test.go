package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
	entered       chan struct{}
	release       <-chan struct{}
	enterOnce     sync.Once
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
	if err := db.AutoMigrate(&model.Platform{}); err != nil {
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
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "not-a-uid").Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("a", 43)})
	if luoguReason(err) != "LUOGU_UID_MISMATCH" {
		t.Fatalf("err=%v", err)
	}
	if n, _ := rdb.Exists(context.Background(), luoguSyncCooldownKey(7, "2245873")).Result(); n != 0 {
		t.Fatal("invalid start consumed cooldown")
	}
	if err := db.Model(&model.Platform{}).Where("user_id = ? AND platform = ?", 7, "LuoGu").Update("username", "2245873").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{ClientKind: "userscript", ClientVersion: "1.0.0", RequestId: strings.Repeat("a", 43)}); err != nil {
		t.Fatal(err)
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

func TestLuoguSyncStartRejectsMismatchedClientVersion(t *testing.T) {
	svc, _, rdb, _, _ := newLuoguSyncServiceTest(t)
	_, err := svc.StartLuoguSync(luoguHeaderContext(luoguPluginTokenHeader, "device-token"), &spiderpb.StartLuoguSyncReq{
		ClientKind: "userscript", ClientVersion: "2.0.0", RequestId: strings.Repeat("a", 43),
	})
	if luoguReason(err) != "GOALGO_CONNECT_REQUIRED" {
		t.Fatalf("err=%v", err)
	}
	if exists, _ := rdb.Exists(context.Background(), luoguSyncCooldownKey(7, "2245873")).Result(); exists != 0 {
		t.Fatal("client version mismatch consumed cooldown")
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
