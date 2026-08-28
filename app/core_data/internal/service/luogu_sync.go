package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	pluginpb "cwxu-algo/api/user/v1/plugin"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spider/platform"
	"cwxu-algo/app/core_data/internal/userrpc"
	"cwxu-algo/app/core_data/task"

	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	luoguPluginTokenHeader = "X-GoAlgo-Plugin-Token"
	luoguSyncSessionHeader = "X-GoAlgo-Sync-Session"
	luoguSyncScope         = "luogu.sync"
	luoguSyncSessionTTL    = 30 * time.Minute
	luoguSyncCooldown      = 5 * time.Minute
	luoguSyncPageDelay     = 500 * time.Millisecond
	luoguSyncMaxRestarts   = 3
)

var (
	luoguUIDPattern       = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	luoguSubmitIDPattern  = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	luoguProblemID        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	luoguRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type luoguPluginIdentity struct {
	AuthorizationID uint64
	UserID          int64
	LuoguUID        string
	ClientKind      string
	ClientVersion   string
}

type luoguTokenValidator interface {
	ValidateLuoguPluginToken(context.Context, string) (luoguPluginIdentity, error)
}

type luoguSubmitImporter interface {
	ImportSubmitLogs(context.Context, int64, string, int64, []model.SubmitLog) (bizservice.SubmitImportResult, error)
	CompleteClientSync(context.Context, int64, string, int64, string, time.Time) error
	ScheduleSubmitPostProcess(int64)
}

type luoguClientPageImporter interface {
	ImportClientSyncPage(context.Context, int64, string, int64, []model.SubmitLog, bizservice.ClientSyncPageImport) (bizservice.ClientSyncPageImportResult, error)
}

type luoguSessionFinalizer interface {
	MarkClientSyncSessionTerminated(context.Context, string, time.Time) error
}

type luoguSyncClock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type realLuoguSyncClock struct{}

func (realLuoguSyncClock) Now() time.Time        { return time.Now() }
func (realLuoguSyncClock) Sleep(d time.Duration) { time.Sleep(d) }

type grpcLuoguTokenValidator struct{ service *SpiderService }

func (v *grpcLuoguTokenValidator) ValidateLuoguPluginToken(ctx context.Context, token string) (luoguPluginIdentity, error) {
	if v == nil || v.service == nil || v.service.reg == nil {
		return luoguPluginIdentity{}, kratoserrors.ServiceUnavailable("GOALGO_CONNECT_REQUIRED", "设备授权校验暂不可用")
	}
	client, err := userrpc.LuoguPluginClient(&v.service.reg.Reg)
	if err != nil {
		return luoguPluginIdentity{}, kratoserrors.ServiceUnavailable("GOALGO_CONNECT_REQUIRED", "设备授权校验暂不可用")
	}
	res, err := client.ValidateLuoguPluginToken(ctx, &pluginpb.ValidateLuoguPluginTokenReq{Token: token, Scope: luoguSyncScope})
	if err != nil {
		reason := kratoserrors.Reason(err)
		if reason == "TOKEN_REVOKED" {
			return luoguPluginIdentity{}, kratoserrors.Unauthorized("GOALGO_CONNECT_REQUIRED", "设备授权已撤销")
		}
		return luoguPluginIdentity{}, err
	}
	return luoguPluginIdentity{
		AuthorizationID: res.AuthorizationId,
		UserID:          int64(res.UserId),
		LuoguUID:        res.LuoguUid,
		ClientKind:      res.ClientKind,
		ClientVersion:   res.ClientVersion,
	}, nil
}

type luoguSession struct {
	ID              string
	TokenHash       string
	AuthorizationID uint64
	UserID          int64
	LuoguUID        string
	ClientKind      string
	RequestIDHash   string
	Generation      int64
	ExpectedPage    int32
	FirstSubmitID   string
	RemoteCount     int32
	PerPage         int32
	OldCheckpoint   string
	Inserted        int64
	ProcessedPages  int32
	Restarts        int32
	LastPage        int32
	LastPageDigest  string
	LastResponse    string
	LastProcessedMS int64
	ExpiresAt       int64
	NextAvailableAt int64
	Done            bool
}

func luoguSyncSessionKey(id string) string { return "luogu:sync:session:" + id }
func luoguSyncTokenKey(hash string) string { return "luogu:sync:token:" + hash }
func luoguSyncActiveKey(userID int64, uid string) string {
	return fmt.Sprintf("luogu:sync:active:%d:%s", userID, uid)
}
func luoguSyncCooldownKey(userID int64, uid string) string {
	return fmt.Sprintf("luogu:sync:cooldown:%d:%s", userID, uid)
}
func luoguSyncIssuanceKey(authorizationID uint64, requestIDHash string) string {
	return fmt.Sprintf("luogu:sync:issuance:%d:%s", authorizationID, requestIDHash)
}
func luoguSyncLockKey(id string) string { return "luogu:sync:lock:" + id }

var luoguStartScript = redis.NewScript(`
local issued = redis.call("GET", KEYS[5])
if issued then
	local issuedSession = ARGV[1] .. issued
	if redis.call("EXISTS", issuedSession) == 1 and
		redis.call("HGET", issuedSession, "request_id_hash") == ARGV[14] and
		redis.call("HGET", issuedSession, "authorization_id") == ARGV[6] then
		if redis.call("HGET", issuedSession, "done") == "1" and redis.call("GET", KEYS[1]) == issued then
			redis.call("DEL", KEYS[1])
		end
		return {"REPLAY", issued}
	end
	redis.call("DEL", KEYS[5])
end
local active = redis.call("GET", KEYS[1])
if active then
	local activeSession = ARGV[1] .. active
	if redis.call("EXISTS", activeSession) == 1 then
		if redis.call("HGET", activeSession, "done") == "1" then
			redis.call("DEL", KEYS[1])
		else
			return {"BUSY", active}
		end
	else
		redis.call("DEL", KEYS[1])
	end
end
local cooldown = redis.call("GET", KEYS[2])
if cooldown then
  return {"COOLDOWN", cooldown}
end
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[3])
redis.call("HSET", KEYS[3],
  "id", ARGV[4], "token_hash", ARGV[5], "authorization_id", ARGV[6],
  "user_id", ARGV[7], "luogu_uid", ARGV[8], "client_kind", ARGV[9],
	"request_id_hash", ARGV[14],
  "generation", ARGV[10], "expected_page", "1", "first_submit_id", "",
  "remote_count", "-1", "per_page", "0", "old_checkpoint", ARGV[11], "inserted", "0",
  "processed_pages", "0", "restarts", "0", "last_page", "0",
  "last_page_digest", "", "last_response", "", "last_processed_ms", "0",
  "expires_at", ARGV[12], "next_available_at", ARGV[2], "done", "0")
redis.call("PEXPIRE", KEYS[3], ARGV[13])
redis.call("SET", KEYS[1], ARGV[4], "PX", ARGV[13])
redis.call("SET", KEYS[4], ARGV[4], "PX", ARGV[13])
redis.call("SET", KEYS[5], ARGV[4], "PX", ARGV[13])
return {"NEW", ARGV[4]}
`)

var luoguSessionUnlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0
`)

var luoguActiveDeleteScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0
`)

var luoguTerminateScript = redis.NewScript(`
if redis.call("GET", KEYS[3]) == ARGV[1] then redis.call("DEL", KEYS[3]) end
return redis.call("DEL", KEYS[1], KEYS[2], KEYS[4], KEYS[5])
`)

var luoguStoreSessionScript = redis.NewScript(`
for i = 4, #ARGV, 2 do
	redis.call("HSET", KEYS[1], ARGV[i], ARGV[i + 1])
end
redis.call("PEXPIRE", KEYS[1], ARGV[1])
redis.call("PEXPIRE", KEYS[2], ARGV[1])
redis.call("PEXPIRE", KEYS[4], ARGV[1])
if ARGV[2] == "1" then
	if redis.call("GET", KEYS[3]) == ARGV[3] then redis.call("DEL", KEYS[3]) end
elseif redis.call("GET", KEYS[3]) == ARGV[3] then
	redis.call("PEXPIRE", KEYS[3], ARGV[1])
end
return 1
`)

func (s *SpiderService) StartLuoguSync(ctx context.Context, req *spiderpb.StartLuoguSyncReq) (*spiderpb.StartLuoguSyncRes, error) {
	if s == nil || s.rdb == nil || s.db == nil {
		return nil, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	token := requestHeader(ctx, luoguPluginTokenHeader)
	if token == "" {
		return nil, kratoserrors.Unauthorized("GOALGO_CONNECT_REQUIRED", "请先连接 GoAlgo")
	}
	validator := s.luoguTokenValidator
	if validator == nil {
		validator = &grpcLuoguTokenValidator{service: s}
	}
	identity, err := validator.ValidateLuoguPluginToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := validateLuoguStartRequest(req, identity); err != nil {
		return nil, err
	}
	binding, generation, err := s.validateLuoguBinding(ctx, identity.UserID, identity.LuoguUID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(binding.ClientSyncHeadSubmitID) == "" {
		binding.ClientSyncHeadSubmitID, err = s.inferLuoguCheckpoint(ctx, identity.UserID)
		if err != nil {
			return nil, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
		}
	}

	now := s.luoguNow()
	expiresAt := now.Add(luoguSyncSessionTTL)
	nextAvailableAt := now.Add(luoguSyncCooldown)
	sessionID, err := secureLuoguRandom(16)
	if err != nil {
		return nil, kratoserrors.InternalServer("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	requestID := strings.TrimSpace(req.RequestId)
	requestIDHash := hashLuoguRequestID(requestID)
	sessionToken := deriveLuoguSessionToken(token, identity.AuthorizationID, requestID, sessionID)
	tokenHash := hashLuoguSessionToken(sessionToken)
	activeKey := luoguSyncActiveKey(identity.UserID, identity.LuoguUID)
	result, err := luoguStartScript.Run(ctx, s.rdb,
		[]string{activeKey, luoguSyncCooldownKey(identity.UserID, identity.LuoguUID), luoguSyncSessionKey(sessionID), luoguSyncTokenKey(tokenHash), luoguSyncIssuanceKey(identity.AuthorizationID, requestIDHash)},
		"luogu:sync:session:", nextAvailableAt.Unix(), luoguSyncCooldown.Milliseconds(), sessionID, tokenHash,
		identity.AuthorizationID, identity.UserID, identity.LuoguUID, identity.ClientKind, generation,
		binding.ClientSyncHeadSubmitID, expiresAt.Unix(), luoguSyncSessionTTL.Milliseconds(), requestIDHash,
	).Slice()
	if err != nil || len(result) != 2 {
		return nil, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	mode := fmt.Sprint(result[0])
	if mode == "COOLDOWN" {
		until, _ := strconv.ParseInt(fmt.Sprint(result[1]), 10, 64)
		return nil, luoguCooldownError(now, until)
	}
	if mode == "BUSY" {
		return nil, kratoserrors.Conflict("SYNC_IN_PROGRESS", "同步会话正在启动")
	}
	if mode == "REPLAY" {
		sessionID = fmt.Sprint(result[1])
		state, loadErr := s.loadLuoguSessionByID(ctx, sessionID)
		if loadErr != nil {
			return nil, loadErr
		}
		// Binding was validated before touching the active session. Generation
		// mismatch terminates the stale session instead of reviving it.
		if state.UserID != identity.UserID || state.LuoguUID != identity.LuoguUID || state.Generation != generation ||
			state.AuthorizationID != identity.AuthorizationID || state.RequestIDHash != requestIDHash {
			s.terminateLuoguSession(ctx, state)
			return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
		}
		sessionToken = deriveLuoguSessionToken(token, identity.AuthorizationID, requestID, sessionID)
		if state.TokenHash != hashLuoguSessionToken(sessionToken) {
			return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
		}
		return luoguStartResponse(state, sessionToken, true), nil
	}
	state, err := s.loadLuoguSessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return luoguStartResponse(state, sessionToken, false), nil
}

func (s *SpiderService) LuoguSyncStatus(ctx context.Context, _ *spiderpb.LuoguSyncStatusReq) (*spiderpb.LuoguSyncStatusRes, error) {
	state, err := s.authorizeLuoguSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.refreshLuoguSessionTTL(ctx, state); err != nil {
		return nil, err
	}
	response := &spiderpb.LuoguSyncStatusRes{
		SessionId: state.ID, NextPage: state.ExpectedPage, Inserted: state.Inserted,
		ProcessedPages: state.ProcessedPages, TotalPages: totalLuoguPages(state.RemoteCount, state.PerPage),
		ExpiresAt: state.ExpiresAt, NextAvailableAt: state.NextAvailableAt, Done: state.Done,
	}
	if state.Done && state.LastResponse != "" {
		var completed spiderpb.UploadLuoguSyncPageRes
		if json.Unmarshal([]byte(state.LastResponse), &completed) == nil {
			response.Connected = completed.Connected
			response.CompletionReason = completed.CompletionReason
		}
	}
	return response, nil
}

func (s *SpiderService) UploadLuoguSyncPage(ctx context.Context, req *spiderpb.UploadLuoguSyncPageReq) (*spiderpb.UploadLuoguSyncPageRes, error) {
	state, err := s.authorizeLuoguSession(ctx)
	if err != nil {
		return nil, err
	}
	unlock, locked := s.tryLuoguSessionLock(ctx, state.ID)
	if !locked {
		return nil, kratoserrors.Conflict("SYNC_IN_PROGRESS", "同步页正在处理")
	}
	defer unlock()
	authorizedTokenHash := state.TokenHash
	// Reload after lock acquisition so concurrent handlers cannot use stale
	// expectedPage/digest state.
	state, err = s.loadLuoguSessionByID(ctx, state.ID)
	if err != nil {
		return nil, err
	}
	if err := s.validateLuoguSessionState(ctx, state, authorizedTokenHash); err != nil {
		return nil, err
	}
	if err := validateLuoguPage(req, state, s.luoguNow()); err != nil {
		return nil, err
	}
	digest, err := digestLuoguPage(req)
	if err != nil {
		return nil, kratoserrors.BadRequest("LUOGU_LAYOUT_CHANGED", "同步页内容无效")
	}
	if req.Page == state.LastPage && digest == state.LastPageDigest && state.LastResponse != "" {
		var previous spiderpb.UploadLuoguSyncPageRes
		if json.Unmarshal([]byte(state.LastResponse), &previous) == nil {
			return &previous, nil
		}
	}
	if state.Done {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已完成")
	}
	// Only the expected page, or the immediately previous page being retried
	// with changed content, may affect restart state. Arbitrary out-of-order
	// pages must never consume the restart budget.
	if req.Page != state.ExpectedPage && req.Page != state.LastPage {
		return nil, kratoserrors.BadRequest("LUOGU_LAYOUT_CHANGED", "同步页顺序无效")
	}

	remoteChanged := state.RemoteCount >= 0 && state.RemoteCount != req.RemoteCount
	perPageChanged := state.PerPage > 0 && state.PerPage != req.PerPage
	samePageChanged := req.Page == state.LastPage && state.LastPage > 0 && digest != state.LastPageDigest
	if remoteChanged || perPageChanged || samePageChanged {
		return s.restartLuoguScan(ctx, state, req.RemoteCount, req.PerPage)
	}
	if req.Page != state.ExpectedPage {
		return nil, kratoserrors.BadRequest("LUOGU_LAYOUT_CHANGED", "同步页顺序无效")
	}
	if state.RemoteCount < 0 {
		state.RemoteCount = req.RemoteCount
	}
	state.PerPage = req.PerPage
	if state.LastProcessedMS > 0 {
		notBefore := time.UnixMilli(state.LastProcessedMS).Add(luoguSyncPageDelay)
		if wait := notBefore.Sub(s.luoguNow()); wait > 0 {
			s.luoguSleep(wait)
		}
	}

	logs := make([]model.SubmitLog, 0, len(req.Records))
	for _, raw := range req.Records {
		record, convErr := luoguProtoRecord(raw)
		if convErr != nil {
			return nil, convErr
		}
		logs = append(logs, platform.LuoGuRecordToSubmitLog(state.UserID, record))
	}
	importer := s.luoguImporter
	if importer == nil {
		return nil, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	firstSubmitID := state.FirstSubmitID
	if firstSubmitID == "" && req.Page == 1 && len(req.Records) > 0 {
		firstSubmitID = req.Records[0].SubmitId
	}
	connected, done, reason := false, false, ""
	if state.OldCheckpoint != "" && pageContainsLuoguSubmit(req.Records, state.OldCheckpoint) {
		connected, done, reason = true, true, "checkpoint"
	} else if req.Page >= totalLuoguPages(req.RemoteCount, req.PerPage) {
		connected, done, reason = true, true, "remote_end"
	}
	var pageInserted int64
	if receiptImporter, ok := importer.(luoguClientPageImporter); ok {
		pageResult, importErr := receiptImporter.ImportClientSyncPage(ctx, state.UserID, spiderregistry.LuoGu, state.Generation, logs, bizservice.ClientSyncPageImport{
			SessionID: state.ID, Restart: state.Restarts, Page: req.Page, Digest: digest,
			FirstSubmitID: firstSubmitID, RemoteCount: req.RemoteCount, PerPage: req.PerPage,
			InsertedBefore: state.Inserted, ProcessedPagesBefore: state.ProcessedPages,
			CompletionReason: reason, NextAvailableAt: state.NextAvailableAt,
			CompletedAt: s.luoguNow(), ExpiresAt: s.luoguNow().Add(luoguSyncSessionTTL),
		})
		if importErr != nil {
			return nil, s.mapLuoguImporterError(ctx, state, importErr)
		}
		pageInserted = pageResult.PageInserted
		state.FirstSubmitID, state.RemoteCount, state.PerPage = pageResult.FirstSubmitID, pageResult.RemoteCount, pageResult.PerPage
		state.Inserted, state.ProcessedPages, state.ExpectedPage = pageResult.Inserted, pageResult.ProcessedPages, pageResult.NextPage
		reason = pageResult.CompletionReason
		done, connected = reason != "", reason != ""
		if !pageResult.DigestMatched {
			state.LastPage, state.LastPageDigest, state.LastResponse = pageResult.NextPage-1, "", ""
			return s.restartLuoguScan(ctx, state, req.RemoteCount, req.PerPage)
		}
	} else {
		imported, importErr := importer.ImportSubmitLogs(ctx, state.UserID, spiderregistry.LuoGu, state.Generation, logs)
		if importErr != nil {
			return nil, s.mapLuoguImporterError(ctx, state, importErr)
		}
		pageInserted = imported.Inserted
		state.FirstSubmitID = firstSubmitID
		state.Inserted += imported.Inserted
		state.ProcessedPages++
		state.ExpectedPage++
		if done {
			if err := importer.CompleteClientSync(ctx, state.UserID, spiderregistry.LuoGu, state.Generation, state.FirstSubmitID, s.luoguNow()); err != nil {
				return nil, s.mapLuoguImporterError(ctx, state, err)
			}
		}
	}
	state.LastProcessedMS = s.luoguNow().UnixMilli()
	response := &spiderpb.UploadLuoguSyncPageRes{
		Connected: connected, Done: done, CompletionReason: reason,
		NextPage: state.ExpectedPage, PageInserted: pageInserted, Inserted: state.Inserted,
		ProcessedPages: state.ProcessedPages, TotalPages: totalLuoguPages(req.RemoteCount, req.PerPage),
		NextAvailableAt: state.NextAvailableAt,
	}
	if done {
		state.Done = true
		if state.Inserted > 0 {
			if _, durable := importer.(luoguClientPageImporter); !durable {
				importer.ScheduleSubmitPostProcess(state.UserID)
			}
		}
	}
	encoded, _ := json.Marshal(response)
	state.LastPage, state.LastPageDigest, state.LastResponse = req.Page, digest, string(encoded)
	if err := s.storeLuoguSession(ctx, state, done); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *SpiderService) mapLuoguImporterError(ctx context.Context, state *luoguSession, err error) error {
	if kratoserrors.Reason(err) == "SYNC_BINDING_CHANGED" {
		s.terminateLuoguSession(ctx, state)
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	if kratoserrors.Reason(err) == "LUOGU_RECORDS_CHANGED" {
		s.terminateLuoguSession(ctx, state)
	}
	return err
}

func validateLuoguStartRequest(req *spiderpb.StartLuoguSyncReq, identity luoguPluginIdentity) error {
	if req == nil || req.ClientKind != "userscript" || req.ClientKind != identity.ClientKind ||
		!luoguRequestIDPattern.MatchString(strings.TrimSpace(req.RequestId)) {
		return kratoserrors.BadRequest("GOALGO_CONNECT_REQUIRED", "客户端授权不匹配")
	}
	version := strings.TrimSpace(req.ClientVersion)
	if version == "" || len(version) > 64 || identity.UserID <= 0 || identity.AuthorizationID == 0 || !luoguUIDPattern.MatchString(identity.LuoguUID) {
		return kratoserrors.BadRequest("GOALGO_CONNECT_REQUIRED", "客户端授权无效")
	}
	return nil
}

func (s *SpiderService) validateLuoguBinding(ctx context.Context, userID int64, uid string) (model.Platform, int64, error) {
	var binding model.Platform
	err := s.db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&binding).Error
	if err != nil {
		return binding, 0, kratoserrors.Conflict("LUOGU_UID_MISMATCH", "当前洛谷账号与 GoAlgo 绑定不一致")
	}
	boundUID := strings.TrimSpace(binding.Username)
	if !luoguUIDPattern.MatchString(boundUID) {
		removed, err := s.removeInvalidLuoguBinding(ctx, userID, uid)
		if err != nil {
			log.Errorf("remove invalid Luogu binding user=%d: %v", userID, err)
			return binding, 0, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "绑定异常，自动清理失败，请稍后重试")
		}
		if !removed {
			return binding, 0, kratoserrors.Conflict("SYNC_IN_PROGRESS", "洛谷绑定正在更新，请稍后重试")
		}
		return binding, 0, kratoserrors.Conflict("LUOGU_BINDING_INVALID_REMOVED", "原洛谷绑定无效，相关同步数据已清理，请重新绑定")
	}
	if boundUID != uid {
		return binding, 0, kratoserrors.Conflict("LUOGU_UID_MISMATCH", "当前洛谷账号与 GoAlgo 绑定不一致")
	}
	var owners int64
	if err := s.db.WithContext(ctx).Model(&model.Platform{}).
		Where("platform = ? AND username = ? AND user_id <> ?", spiderregistry.LuoGu, uid, userID).
		Count(&owners).Error; err != nil {
		return binding, 0, kratoserrors.InternalServer("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	if owners > 0 {
		return binding, 0, kratoserrors.Conflict("LUOGU_UID_ALREADY_BOUND", "该洛谷账号已被其他用户绑定")
	}
	generation, err := task.CurrentGeneration(ctx, s.rdb, userID, spiderregistry.LuoGu)
	if err != nil {
		return binding, 0, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	return binding, generation, nil
}

func (s *SpiderService) inferLuoguCheckpoint(ctx context.Context, userID int64) (string, error) {
	rows, err := s.db.WithContext(ctx).Model(&model.SubmitLog{}).
		Select("submit_id").Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).Rows()
	if err != nil {
		return "", err
	}
	defer rows.Close()
	best := ""
	for rows.Next() {
		var submitID string
		if err := rows.Scan(&submitID); err != nil {
			return "", err
		}
		submitID = strings.TrimSpace(submitID)
		if !luoguSubmitIDPattern.MatchString(submitID) {
			continue
		}
		if best == "" || len(submitID) > len(best) || (len(submitID) == len(best) && submitID > best) {
			best = submitID
		}
	}
	return best, rows.Err()
}

func (s *SpiderService) removeInvalidLuoguBinding(ctx context.Context, userID int64, authorizedUID string) (bool, error) {
	unlock, locked := trySpiderPlatformWriteLock(ctx, s.rdb, userID, spiderregistry.LuoGu)
	if !locked {
		return false, fmt.Errorf("acquire LuoGu write lock")
	}
	defer unlock()
	var lockedBinding model.Platform
	if err := s.db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&lockedBinding).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, err
	}
	if luoguUIDPattern.MatchString(strings.TrimSpace(lockedBinding.Username)) {
		return false, nil
	}
	if _, err := s.rdb.Incr(ctx, task.GenerationKey(userID, spiderregistry.LuoGu)).Result(); err != nil {
		return false, err
	}
	_ = s.rdb.Expire(ctx, task.GenerationKey(userID, spiderregistry.LuoGu), 7*24*time.Hour).Err()
	removed := false
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.Platform
		if err := tx.Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&current).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil
			}
			return err
		}
		if luoguUIDPattern.MatchString(strings.TrimSpace(current.Username)) {
			return nil
		}
		removed = true
		return deleteSpiderPlatformData(ctx, tx, userID, spiderregistry.LuoGu)
	}); err != nil {
		return false, err
	}
	if !removed {
		return false, nil
	}
	if sessionID, err := s.rdb.Get(ctx, luoguSyncActiveKey(userID, authorizedUID)).Result(); err == nil {
		if state, loadErr := s.loadLuoguSessionByID(ctx, sessionID); loadErr == nil {
			s.terminateLuoguSession(ctx, state)
		}
	} else if err != redis.Nil {
		return false, err
	}
	_ = s.rdb.Del(ctx,
		fmt.Sprintf("core:submit_log:user:%d", userID),
		fmt.Sprintf("user:%d:lastSubmitTime", userID),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", userID),
		fmt.Sprintf("spider:pending:%d:%s", userID, spiderregistry.LuoGu),
		fmt.Sprintf("spider:inflight:%d:%s", userID, spiderregistry.LuoGu),
	).Err()
	_ = s.rdb.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", userID)).Err()
	_ = s.rdb.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", userID)).Err()
	_ = s.rdb.Incr(ctx, "statistic:period:global:ver").Err()
	return true, nil
}

func validateLuoguPage(req *spiderpb.UploadLuoguSyncPageReq, state *luoguSession, now time.Time) error {
	bad := func() error { return kratoserrors.BadRequest("LUOGU_LAYOUT_CHANGED", "同步页内容无效") }
	if req == nil || state == nil {
		return bad()
	}
	if req.LuoguUid != state.LuoguUID {
		return kratoserrors.Conflict("LUOGU_UID_MISMATCH", "当前洛谷账号与 GoAlgo 绑定不一致")
	}
	if req.Page <= 0 || req.RemoteCount < 0 || req.RemoteCount > 10_000_000 || req.PerPage <= 0 || req.PerPage > 20 || len(req.Records) > 20 {
		return bad()
	}
	total := totalLuoguPages(req.RemoteCount, req.PerPage)
	if req.RemoteCount == 0 {
		if req.Page != 1 || len(req.Records) != 0 {
			return bad()
		}
	} else {
		if req.Page > total {
			return bad()
		}
		remaining := int(req.RemoteCount - (req.Page-1)*req.PerPage)
		expected := int(req.PerPage)
		if remaining < expected {
			expected = remaining
		}
		if len(req.Records) != expected || expected <= 0 {
			return bad()
		}
	}
	seen := make(map[string]struct{}, len(req.Records))
	minTime := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	maxTime := now.Add(5 * time.Minute).Unix()
	for _, record := range req.Records {
		if record == nil || !luoguSubmitIDPattern.MatchString(record.SubmitId) || record.SubmitTime < minTime || record.SubmitTime > maxTime ||
			!validLuoguStatus(record.Status) || record.Language < 1 || record.Language > 34 || record.Problem == nil ||
			!luoguProblemID.MatchString(record.Problem.Pid) || len(record.Problem.Title) > 512 || record.Problem.Difficulty < 0 || record.Problem.Difficulty > 8 {
			return bad()
		}
		if _, ok := seen[record.SubmitId]; ok {
			return bad()
		}
		seen[record.SubmitId] = struct{}{}
	}
	return nil
}

func validLuoguStatus(status int32) bool {
	return (status >= 0 && status <= 14) || status == -1 || status == 21 || status == 22 || status == 23
}

func luoguProtoRecord(raw *spiderpb.LuoguSyncRecord) (platform.Record, error) {
	var record platform.Record
	id, err := strconv.ParseInt(raw.SubmitId, 10, 64)
	if err != nil {
		return record, kratoserrors.BadRequest("LUOGU_LAYOUT_CHANGED", "同步页内容无效")
	}
	record.ID, record.SubmitTime, record.Status, record.Language = id, raw.SubmitTime, int(raw.Status), int(raw.Language)
	record.Problem.Pid, record.Problem.Title, record.Problem.Difficulty = raw.Problem.Pid, raw.Problem.Title, int(raw.Problem.Difficulty)
	return record, nil
}

func (s *SpiderService) authorizeLuoguSession(ctx context.Context) (*luoguSession, error) {
	token := requestHeader(ctx, luoguSyncSessionHeader)
	if token == "" {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	hash := hashLuoguSessionToken(token)
	id, err := s.rdb.Get(ctx, luoguSyncTokenKey(hash)).Result()
	if err != nil || id == "" {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	state, err := s.loadLuoguSessionByID(ctx, id)
	if err != nil {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	if err := s.validateLuoguSessionState(ctx, state, hash); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *SpiderService) validateLuoguSessionState(ctx context.Context, state *luoguSession, tokenHash string) error {
	if state == nil || state.TokenHash != tokenHash {
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	id, err := s.rdb.Get(ctx, luoguSyncTokenKey(tokenHash)).Result()
	if err != nil || id != state.ID {
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	revoked, err := s.rdb.Exists(ctx, fmt.Sprintf("luogu:plugin:authorization:revoked:%d", state.AuthorizationID)).Result()
	if err != nil {
		return kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	if revoked > 0 {
		s.terminateLuoguSession(ctx, state)
		return kratoserrors.Unauthorized("GOALGO_CONNECT_REQUIRED", "设备授权已撤销")
	}
	_, generation, bindErr := s.validateLuoguBinding(ctx, state.UserID, state.LuoguUID)
	if bindErr != nil {
		if kratoserrors.Reason(bindErr) != "SYNC_UNAVAILABLE" {
			s.terminateLuoguSession(ctx, state)
		}
		return bindErr
	}
	if generation != state.Generation {
		s.terminateLuoguSession(ctx, state)
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	return nil
}

func (s *SpiderService) loadLuoguSessionByID(ctx context.Context, id string) (*luoguSession, error) {
	values, err := s.rdb.HGetAll(ctx, luoguSyncSessionKey(id)).Result()
	if err != nil || len(values) == 0 {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	state := &luoguSession{ID: id}
	state.TokenHash = values["token_hash"]
	state.AuthorizationID = parseUint64(values["authorization_id"])
	state.UserID = parseInt64(values["user_id"])
	state.LuoguUID, state.ClientKind, state.RequestIDHash = values["luogu_uid"], values["client_kind"], values["request_id_hash"]
	state.Generation = parseInt64(values["generation"])
	state.ExpectedPage = int32(parseInt64(values["expected_page"]))
	state.FirstSubmitID, state.OldCheckpoint = values["first_submit_id"], values["old_checkpoint"]
	state.RemoteCount = int32(parseInt64(values["remote_count"]))
	state.PerPage = int32(parseInt64(values["per_page"]))
	state.Inserted = parseInt64(values["inserted"])
	state.ProcessedPages = int32(parseInt64(values["processed_pages"]))
	state.Restarts = int32(parseInt64(values["restarts"]))
	state.LastPage = int32(parseInt64(values["last_page"]))
	state.LastPageDigest, state.LastResponse = values["last_page_digest"], values["last_response"]
	state.LastProcessedMS = parseInt64(values["last_processed_ms"])
	state.ExpiresAt, state.NextAvailableAt = parseInt64(values["expires_at"]), parseInt64(values["next_available_at"])
	state.Done = values["done"] == "1"
	if state.ID == "" || state.TokenHash == "" || state.RequestIDHash == "" || state.UserID <= 0 || state.ExpectedPage <= 0 {
		return nil, kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	return state, nil
}

func (s *SpiderService) storeLuoguSession(ctx context.Context, state *luoguSession, completed bool) error {
	now := s.luoguNow()
	state.ExpiresAt = now.Add(luoguSyncSessionTTL).Unix()
	fields := map[string]interface{}{
		"expected_page": state.ExpectedPage, "first_submit_id": state.FirstSubmitID,
		"remote_count": state.RemoteCount, "per_page": state.PerPage, "inserted": state.Inserted, "processed_pages": state.ProcessedPages,
		"restarts": state.Restarts, "last_page": state.LastPage, "last_page_digest": state.LastPageDigest,
		"last_response": state.LastResponse, "last_processed_ms": state.LastProcessedMS,
		"expires_at": state.ExpiresAt, "done": boolInt(state.Done),
	}
	args := []interface{}{luoguSyncSessionTTL.Milliseconds(), boolInt(completed), state.ID}
	for key, value := range fields {
		args = append(args, key, value)
	}
	if err := luoguStoreSessionScript.Run(ctx, s.rdb, []string{
		luoguSyncSessionKey(state.ID), luoguSyncTokenKey(state.TokenHash),
		luoguSyncActiveKey(state.UserID, state.LuoguUID), luoguSyncIssuanceKey(state.AuthorizationID, state.RequestIDHash),
	}, args...).Err(); err != nil {
		return kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	return nil
}

func (s *SpiderService) refreshLuoguSessionTTL(ctx context.Context, state *luoguSession) error {
	state.ExpiresAt = s.luoguNow().Add(luoguSyncSessionTTL).Unix()
	_, err := s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, luoguSyncSessionKey(state.ID), "expires_at", state.ExpiresAt)
		pipe.Expire(ctx, luoguSyncSessionKey(state.ID), luoguSyncSessionTTL)
		pipe.Expire(ctx, luoguSyncTokenKey(state.TokenHash), luoguSyncSessionTTL)
		pipe.Expire(ctx, luoguSyncIssuanceKey(state.AuthorizationID, state.RequestIDHash), luoguSyncSessionTTL)
		if !state.Done {
			pipe.Expire(ctx, luoguSyncActiveKey(state.UserID, state.LuoguUID), luoguSyncSessionTTL)
		}
		return nil
	})
	if err != nil {
		return kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	return nil
}

func (s *SpiderService) restartLuoguScan(ctx context.Context, state *luoguSession, remoteCount, perPage int32) (*spiderpb.UploadLuoguSyncPageRes, error) {
	state.Restarts++
	if state.Restarts > luoguSyncMaxRestarts {
		s.terminateLuoguSession(ctx, state)
		return nil, kratoserrors.Conflict("LUOGU_RECORDS_CHANGED", "洛谷记录持续变化，请稍后重试")
	}
	state.ExpectedPage, state.FirstSubmitID, state.RemoteCount, state.PerPage, state.ProcessedPages = 1, "", remoteCount, perPage, 0
	state.LastPage, state.LastPageDigest, state.LastResponse, state.LastProcessedMS = 0, "", "", s.luoguNow().UnixMilli()
	response := &spiderpb.UploadLuoguSyncPageRes{
		NextPage: 1, Restart: true, Inserted: state.Inserted, ProcessedPages: 0,
		TotalPages: totalLuoguPages(remoteCount, perPage), NextAvailableAt: state.NextAvailableAt,
	}
	if err := s.storeLuoguSession(ctx, state, false); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *SpiderService) terminateLuoguSession(ctx context.Context, state *luoguSession) {
	if s == nil || s.rdb == nil || state == nil {
		return
	}
	if finalizer, ok := s.luoguImporter.(luoguSessionFinalizer); ok {
		markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := finalizer.MarkClientSyncSessionTerminated(markCtx, state.ID, s.luoguNow()); err != nil {
			log.Errorf("client-sync mark termination session=%s user=%d: %v", state.ID, state.UserID, err)
		}
		cancel()
	} else if state.Inserted > 0 && !state.Done && s.luoguImporter != nil {
		s.luoguImporter.ScheduleSubmitPostProcess(state.UserID)
	}
	_ = luoguTerminateScript.Run(ctx, s.rdb, []string{
		luoguSyncSessionKey(state.ID), luoguSyncTokenKey(state.TokenHash),
		luoguSyncActiveKey(state.UserID, state.LuoguUID), luoguSyncLockKey(state.ID),
		luoguSyncIssuanceKey(state.AuthorizationID, state.RequestIDHash),
	}, state.ID).Err()
}

func (s *SpiderService) tryLuoguSessionLock(ctx context.Context, id string) (func(), bool) {
	token, err := secureLuoguRandom(16)
	if err != nil {
		return func() {}, false
	}
	key := luoguSyncLockKey(id)
	ok, err := s.rdb.SetNX(ctx, key, token, 2*time.Minute).Result()
	if err != nil || !ok {
		return func() {}, false
	}
	return func() { _ = luoguSessionUnlockScript.Run(context.Background(), s.rdb, []string{key}, token).Err() }, true
}

func luoguStartResponse(state *luoguSession, token string, resumed bool) *spiderpb.StartLuoguSyncRes {
	return &spiderpb.StartLuoguSyncRes{
		SessionId: state.ID, SessionToken: token, Resumed: resumed, NextPage: state.ExpectedPage,
		PageDelayMs: int32(luoguSyncPageDelay.Milliseconds()), ExpiresAt: state.ExpiresAt,
		NextAvailableAt: state.NextAvailableAt,
	}
}

func luoguCooldownError(now time.Time, until int64) error {
	retry := until - now.Unix()
	if retry < 1 {
		retry = 1
	}
	return kratoserrors.New(http.StatusTooManyRequests, "SYNC_COOLDOWN", "同步冷却中，请稍后再试").WithMetadata(map[string]string{
		"code": "SYNC_COOLDOWN", "nextAvailableAt": strconv.FormatInt(until, 10), "retryAfterSeconds": strconv.FormatInt(retry, 10),
	})
}

func requestHeader(ctx context.Context, name string) string {
	if tr, ok := transport.FromServerContext(ctx); ok {
		return strings.TrimSpace(tr.RequestHeader().Get(name))
	}
	return ""
}

func secureLuoguRandom(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashLuoguSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashLuoguRequestID(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(sum[:])
}

func deriveLuoguSessionToken(deviceToken string, authorizationID uint64, requestID, sessionID string) string {
	mac := hmac.New(sha256.New, []byte(deviceToken))
	_, _ = fmt.Fprintf(mac, "goalgo-luogu-sync-session-v1\x00%d\x00%s\x00%s", authorizationID, requestID, sessionID)
	return hex.EncodeToString(mac.Sum(nil))
}

func digestLuoguPage(req *spiderpb.UploadLuoguSyncPageReq) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func totalLuoguPages(count, perPage int32) int32 {
	if count <= 0 {
		return 0
	}
	if perPage <= 0 {
		perPage = 20
	}
	return (count + perPage - 1) / perPage
}

func pageContainsLuoguSubmit(records []*spiderpb.LuoguSyncRecord, id string) bool {
	for _, record := range records {
		if record != nil && record.SubmitId == id {
			return true
		}
	}
	return false
}

func (s *SpiderService) luoguNow() time.Time {
	if s.luoguClock != nil {
		return s.luoguClock.Now()
	}
	return time.Now()
}

func (s *SpiderService) luoguSleep(d time.Duration) {
	if s.luoguClock != nil {
		s.luoguClock.Sleep(d)
		return
	}
	time.Sleep(d)
}

func parseInt64(value string) int64   { n, _ := strconv.ParseInt(value, 10, 64); return n }
func parseUint64(value string) uint64 { n, _ := strconv.ParseUint(value, 10, 64); return n }
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
