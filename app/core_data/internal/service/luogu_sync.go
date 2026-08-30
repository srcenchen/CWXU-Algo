package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	spiderpb "cwxu-algo/api/core/v1/spider"
	pluginpb "cwxu-algo/api/user/v1/plugin"
	profilepb "cwxu-algo/api/user/v1/profile"
	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	spiderregistry "cwxu-algo/app/core_data/internal/spider"
	"cwxu-algo/app/core_data/internal/spider/platform"
	"cwxu-algo/app/core_data/internal/userrpc"
	"cwxu-algo/app/core_data/task"

	kratoserrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

type luoguSyncAuditor interface {
	StartClientSyncAudit(context.Context, bizservice.ClientSyncAuditStart) error
	UpdateClientSyncAudit(context.Context, bizservice.ClientSyncAuditProgress) error
	TerminateClientSyncAudit(context.Context, string, string, string, string, string, time.Time) error
}

type luoguProfilePublisher interface {
	RelayAbilityMaintenanceTargets(context.Context, *model.AbilityMaintenancePending) error
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
func luoguSyncUserSessionsKey(userID int64) string {
	return fmt.Sprintf("luogu:sync:user-sessions:%d", userID)
}
func luoguSyncUserUIDsKey(userID int64) string {
	return fmt.Sprintf("luogu:sync:user-uids:%d", userID)
}

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
redis.call("SADD", KEYS[6], ARGV[4])
redis.call("PEXPIRE", KEYS[6], ARGV[13])
redis.call("SADD", KEYS[7], ARGV[8])
redis.call("PEXPIRE", KEYS[7], ARGV[13])
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
		[]string{activeKey, luoguSyncCooldownKey(identity.UserID, identity.LuoguUID), luoguSyncSessionKey(sessionID), luoguSyncTokenKey(tokenHash), luoguSyncIssuanceKey(identity.AuthorizationID, requestIDHash), luoguSyncUserSessionsKey(identity.UserID), luoguSyncUserUIDsKey(identity.UserID)},
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
		s.recordLuoguSyncAuditStart(ctx, state, req.ClientVersion, now)
		return luoguStartResponse(state, sessionToken, true), nil
	}
	state, err := s.loadLuoguSessionByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	s.recordLuoguSyncAuditStart(ctx, state, req.ClientVersion, now)
	return luoguStartResponse(state, sessionToken, false), nil
}

func (s *SpiderService) recordLuoguSyncAuditStart(ctx context.Context, state *luoguSession, clientVersion string, startedAt time.Time) {
	if state == nil {
		return
	}
	if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok {
		username := ""
		if s.reg != nil {
			if client, err := userrpc.ProfileClient(&s.reg.Reg); err == nil {
				if result, err := client.GetByIds(ctx, &profilepb.GetByIdsReq{UserIds: []int64{state.UserID}}); err == nil && len(result.Profiles) > 0 && result.Profiles[0] != nil {
					username = result.Profiles[0].Username
				}
			}
		}
		if auditErr := auditor.StartClientSyncAudit(ctx, bizservice.ClientSyncAuditStart{SessionID: state.ID, AuthorizationID: state.AuthorizationID, UserID: state.UserID, Username: username, Platform: "luogu", OJUID: state.LuoguUID, ClientKind: state.ClientKind, ClientVersion: clientVersion, StartedAt: startedAt}); auditErr != nil {
			log.Warnf("client-sync audit start session=%s: %v", state.ID, auditErr)
		}
	}
}

func (s *SpiderService) LuoguSyncStatus(ctx context.Context, _ *spiderpb.LuoguSyncStatusReq) (*spiderpb.LuoguSyncStatusRes, error) {
	state, err := s.authorizeLuoguSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.refreshLuoguSessionTTL(ctx, state); err != nil {
		return nil, err
	}
	if err := s.ensureLuoguSyncAuditActive(ctx, state); err != nil {
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
	if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok {
		if auditErr := auditor.UpdateClientSyncAudit(ctx, bizservice.ClientSyncAuditProgress{SessionID: state.ID, ProcessedPages: state.ProcessedPages, RemoteCount: state.RemoteCount, Inserted: state.Inserted, RestartCount: state.Restarts, UpdatedAt: s.luoguNow()}); auditErr != nil {
			log.Warnf("client-sync audit status session=%s: %v", state.ID, auditErr)
		}
		if state.Done && response.CompletionReason != "" {
			if auditErr := auditor.TerminateClientSyncAudit(ctx, state.ID, "completed", response.CompletionReason, "", "", s.luoguNow()); auditErr != nil {
				log.Warnf("client-sync audit completion session=%s: %v", state.ID, auditErr)
			}
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
	if err := s.ensureLuoguSyncAuditActive(ctx, state); err != nil {
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
			if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok {
				_ = auditor.UpdateClientSyncAudit(ctx, bizservice.ClientSyncAuditProgress{SessionID: state.ID, ProcessedPages: state.ProcessedPages, RemoteCount: state.RemoteCount, Inserted: state.Inserted, RestartCount: state.Restarts, UpdatedAt: s.luoguNow()})
				if previous.Done {
					_ = auditor.TerminateClientSyncAudit(ctx, state.ID, "completed", previous.CompletionReason, "", "", s.luoguNow())
				}
			}
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
		s.failLuoguSyncAudit(state, err)
		return nil, err
	}
	return response, nil
}

func (s *SpiderService) mapLuoguImporterError(ctx context.Context, state *luoguSession, err error) error {
	s.failLuoguSyncAudit(state, err)
	if kratoserrors.Reason(err) == "SYNC_BINDING_CHANGED" {
		s.terminateLuoguSession(ctx, state)
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已失效")
	}
	if kratoserrors.Reason(err) == "LUOGU_RECORDS_CHANGED" {
		s.terminateLuoguSession(ctx, state)
	}
	return err
}

func (s *SpiderService) failLuoguSyncAudit(state *luoguSession, err error) {
	if state == nil || err == nil {
		return
	}
	if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok {
		code := kratoserrors.Reason(err)
		if auditErr := auditor.TerminateClientSyncAudit(context.Background(), state.ID, "failed", "failed", code, kratoserrors.FromError(err).Message, s.luoguNow()); auditErr != nil {
			log.Warnf("client-sync audit failure session=%s: %v", state.ID, auditErr)
		}
	}
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
	if pending, pendingErr := loadLuoguCleanupPending(ctx, s.db, userID); pendingErr != nil {
		return binding, 0, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "绑定异常，自动清理失败，请稍后重试")
	} else if pending != nil {
		removed, recoverErr := s.removeInvalidLuoguBinding(ctx, userID, uid)
		if recoverErr != nil {
			log.Errorf("recover invalid Luogu binding user=%d: %v", userID, recoverErr)
			return binding, 0, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "绑定异常，自动清理失败，请稍后重试")
		}
		if removed {
			return binding, 0, kratoserrors.Conflict("LUOGU_BINDING_INVALID_REMOVED", "原洛谷绑定无效，相关同步数据已清理，请重新绑定")
		}
	}
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

func luoguCleanupScope(userID int64) string {
	return fmt.Sprintf("luogu-cleanup:%d:%s", userID, spiderregistry.LuoGu)
}

type luoguCleanupPayload struct {
	BindingID     int64  `json:"bindingId"`
	Username      string `json:"username"`
	AuthorizedUID string `json:"authorizedUid"`
}

func loadLuoguCleanupPending(ctx context.Context, db *gorm.DB, userID int64) (*model.AbilityMaintenancePending, error) {
	if db == nil || !db.Migrator().HasTable(&model.AbilityMaintenancePending{}) {
		return nil, nil
	}
	var pending model.AbilityMaintenancePending
	err := db.WithContext(ctx).Where("scope = ?", luoguCleanupScope(userID)).First(&pending).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pending, nil
}

func prepareLuoguCleanupPending(ctx context.Context, db *gorm.DB, binding model.Platform, authorizedUID string) (*model.AbilityMaintenancePending, error) {
	payload, err := json.Marshal(luoguCleanupPayload{BindingID: binding.Id, Username: binding.Username, AuthorizedUID: authorizedUID})
	if err != nil {
		return nil, err
	}
	pending := model.AbilityMaintenancePending{
		Scope: luoguCleanupScope(binding.UserID), OperationID: uuid.NewString(), Revision: 1,
		Phase: "intent", Operation: "luogu_cleanup", UserID: binding.UserID, Platform: spiderregistry.LuoGu, Payload: string(payload),
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "scope"}}, DoNothing: true}).Create(&pending)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: binding.UserID, Revision: 1, State: "pending"}
		return tx.Create(&target).Error
	})
	if err != nil {
		return nil, err
	}
	return loadLuoguCleanupPending(ctx, db, binding.UserID)
}

func claimLuoguCleanupPending(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, owner string) error {
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ?", pending.Scope, pending.OperationID, pending.Revision).
		Updates(map[string]interface{}{"lease_owner": owner, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("LuoGu cleanup owner changed")
	}
	pending.LeaseOwner = owner
	pending.Revision++
	return nil
}

func advanceLuoguCleanupPhase(ctx context.Context, db *gorm.DB, pending *model.AbilityMaintenancePending, phase string) error {
	res := db.WithContext(ctx).Model(&model.AbilityMaintenancePending{}).
		Where("scope = ? AND operation_id = ? AND revision = ? AND lease_owner = ?", pending.Scope, pending.OperationID, pending.Revision, pending.LeaseOwner).
		Updates(map[string]interface{}{"phase": phase, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("LuoGu cleanup phase owner changed")
	}
	pending.Phase = phase
	pending.Revision++
	return nil
}

func (s *SpiderService) publishAndClearLuoguCleanup(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	profilePublisher, ok := s.luoguImporter.(luoguProfilePublisher)
	if !ok || profilePublisher == nil {
		return fmt.Errorf("LuoGu binding cleanup: user profile publisher unavailable")
	}
	return profilePublisher.RelayAbilityMaintenanceTargets(ctx, pending)
}

func (s *SpiderService) clearLuoguCleanupIntent(ctx context.Context, pending *model.AbilityMaintenancePending) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var target model.AbilityMaintenanceTarget
		if err := tx.Where("intent_id = ? AND user_id = ?", pending.OperationID, pending.UserID).First(&target).Error; err != nil {
			return err
		}
		res := tx.Where("intent_id = ? AND user_id = ? AND revision = ? AND state = ?", target.IntentID, target.UserID, target.Revision, target.State).
			Delete(&model.AbilityMaintenanceTarget{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("LuoGu cleanup target owner changed")
		}
		res = tx.Where("scope = ? AND operation_id = ? AND revision = ?", pending.Scope, pending.OperationID, pending.Revision).
			Delete(&model.AbilityMaintenancePending{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("LuoGu cleanup complete owner changed")
		}
		return nil
	})
}

func (s *SpiderService) finalizeLuoguCleanupTail(ctx context.Context, userID int64, authorizedUID string) error {
	activeKey := luoguSyncActiveKey(userID, authorizedUID)
	if sessionID, err := s.rdb.Get(ctx, activeKey).Result(); err == nil {
		state, loadErr := s.loadLuoguSessionByID(ctx, sessionID)
		if loadErr != nil {
			if kratoserrors.Reason(loadErr) != "SESSION_EXPIRED" {
				return loadErr
			}
			if err := s.rdb.Del(ctx, activeKey).Err(); err != nil {
				return err
			}
		} else {
			if err := s.finalizeLuoguSessionForCleanup(ctx, state); err != nil {
				return err
			}
		}
	} else if err != redis.Nil {
		return err
	}
	if err := s.rdb.Del(ctx,
		fmt.Sprintf("core:submit_log:user:%d", userID),
		fmt.Sprintf("user:%d:lastSubmitTime", userID),
		"core:platforms:bound_users:v1",
		fmt.Sprintf("core:platforms:user:%d:v1", userID),
		fmt.Sprintf("spider:pending:%d:%s", userID, spiderregistry.LuoGu),
		fmt.Sprintf("spider:inflight:%d:%s", userID, spiderregistry.LuoGu),
	).Err(); err != nil {
		return err
	}
	if err := s.rdb.Incr(ctx, fmt.Sprintf("core:contest_log:user:%d:ver", userID)).Err(); err != nil {
		return err
	}
	if err := s.rdb.Incr(ctx, fmt.Sprintf("statistic:user:%d:ver", userID)).Err(); err != nil {
		return err
	}
	if err := s.rdb.Incr(ctx, "statistic:period:global:ver").Err(); err != nil {
		return err
	}
	return nil
}

// finalizeLuoguCleanupTailPersisted runs the destructive cache/session tail
// under a short platform lock, records its completion, then lets MQ relay run
// after the lock has been released. Any replacement binding makes the tail a
// durable no-op so an expired old owner cannot clear a new session.
func (s *SpiderService) finalizeLuoguCleanupTailPersisted(ctx context.Context, pending *model.AbilityMaintenancePending, payload luoguCleanupPayload) (*model.AbilityMaintenancePending, error) {
	tailUnlock, tailLocked := trySpiderPlatformWriteLock(ctx, s.rdb, pending.UserID, spiderregistry.LuoGu)
	if !tailLocked {
		return nil, fmt.Errorf("acquire LuoGu finalize lock")
	}
	defer tailUnlock()
	current, err := loadLuoguCleanupPending(ctx, s.db, pending.UserID)
	if err != nil || current == nil {
		return current, err
	}
	if current.OperationID != pending.OperationID {
		return nil, fmt.Errorf("LuoGu cleanup intent changed")
	}
	if current.Phase == "tail_finalized" {
		return current, nil
	}
	if current.Phase != "fence_finalized" {
		return nil, fmt.Errorf("LuoGu cleanup tail unexpected phase %q", current.Phase)
	}
	var binding model.Platform
	err = s.db.WithContext(ctx).Where("user_id = ? AND platform = ?", current.UserID, spiderregistry.LuoGu).First(&binding).Error
	if err == nil {
		// A fresh binding may already have its own active session and crawler
		// keys. Do not let this old cleanup touch any of them.
		return current, advanceLuoguCleanupPhase(ctx, s.db, current, "tail_finalized")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := s.finalizeLuoguCleanupTail(ctx, current.UserID, payload.AuthorizedUID); err != nil {
		return nil, err
	}
	if err := advanceLuoguCleanupPhase(ctx, s.db, current, "tail_finalized"); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *SpiderService) removeInvalidLuoguBinding(ctx context.Context, userID int64, authorizedUID string) (bool, error) {
	unlock, locked := trySpiderPlatformWriteLock(ctx, s.rdb, userID, spiderregistry.LuoGu)
	if !locked {
		return false, fmt.Errorf("acquire LuoGu write lock")
	}
	lockHeld := true
	releaseLock := func() {
		if lockHeld {
			unlock()
			lockHeld = false
		}
	}
	defer releaseLock()
	pending, err := loadLuoguCleanupPending(ctx, s.db, userID)
	if err != nil {
		return false, err
	}
	if pending == nil {
		var binding model.Platform
		if err := s.db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&binding).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return false, nil
			}
			return false, err
		}
		if luoguUIDPattern.MatchString(strings.TrimSpace(binding.Username)) {
			return false, nil
		}
		pending, err = prepareLuoguCleanupPending(ctx, s.db, binding, authorizedUID)
		if err != nil {
			return false, err
		}
	}
	var cleanupPayload luoguCleanupPayload
	if err := json.Unmarshal([]byte(pending.Payload), &cleanupPayload); err != nil {
		return false, err
	}
	// The platform lock only protects binding discovery/intent creation. The
	// durable intent and profile fence own the expensive rebuild/cache/MQ tail;
	// keeping the Redis write lock through that work risks TTL takeover.
	releaseLock()
	if pending.Phase == "fence_finalized" {
		pending, err = s.finalizeLuoguCleanupTailPersisted(ctx, pending, cleanupPayload)
		if err != nil {
			return false, err
		}
	}
	if pending.Phase == "tail_finalized" {
		// The replacement crawl has not necessarily happened yet. Finish the
		// cleanup intent without publishing a profile; the new binding's
		// platform-scoped marker will trigger the rebuild after its crawl.
		return true, s.clearLuoguCleanupIntent(ctx, pending)
	}
	profileToken, err := bizservice.BeginUserProfileInvalidationForIntent(ctx, s.rdb, userID, pending.OperationID)
	if err != nil {
		return false, err
	}
	if err := claimLuoguCleanupPending(ctx, s.db, pending, profileToken.Owner); err != nil {
		return false, errors.Join(err, bizservice.AbandonUserProfileInvalidation(context.Background(), s.rdb, userID, profileToken))
	}
	workCtx := profileToken.Context()
	abandon := func(cause error) error {
		return errors.Join(cause, bizservice.AbandonUserProfileInvalidation(context.Background(), s.rdb, userID, profileToken))
	}
	validateOwner := func() error {
		return bizservice.ValidateUserProfileInvalidation(workCtx, s.rdb, userID, profileToken)
	}
	if err := validateOwner(); err != nil {
		return false, abandon(err)
	}
	if pending.Phase == "intent" {
		if err := s.db.WithContext(workCtx).Transaction(func(tx *gorm.DB) error {
			var current model.Platform
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&current).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					return nil
				}
				return err
			}
			if (current.Id != cleanupPayload.BindingID || current.Username != cleanupPayload.Username) && luoguUIDPattern.MatchString(strings.TrimSpace(current.Username)) {
				return advanceLuoguCleanupPhase(workCtx, tx, pending, "cancelled")
			}
			return nil
		}); err != nil {
			return false, abandon(err)
		}
	}
	generationBumped := false
	if pending.Phase == "cancelled" {
		cancelUnlock, cancelLocked := trySpiderPlatformWriteLock(ctx, s.rdb, userID, spiderregistry.LuoGu)
		if !cancelLocked {
			return false, abandon(fmt.Errorf("acquire LuoGu cancelled lock"))
		}
		cancelLockHeld := true
		releaseCancelLock := func() {
			if cancelLockHeld {
				cancelUnlock()
				cancelLockHeld = false
			}
		}
		defer releaseCancelLock()
		current, loadErr := loadLuoguCleanupPending(ctx, s.db, userID)
		if loadErr != nil {
			return false, abandon(loadErr)
		}
		if current == nil || current.OperationID != pending.OperationID || current.Phase != "cancelled" {
			return false, abandon(fmt.Errorf("LuoGu cancelled cleanup intent changed"))
		}
		pending = current
		var replacement model.Platform
		replacementErr := s.db.WithContext(ctx).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&replacement).Error
		validReplacement := replacementErr == nil &&
			(replacement.Id != cleanupPayload.BindingID || replacement.Username != cleanupPayload.Username) &&
			luoguUIDPattern.MatchString(strings.TrimSpace(replacement.Username))
		if replacementErr != nil && !errors.Is(replacementErr, gorm.ErrRecordNotFound) {
			return false, abandon(replacementErr)
		}
		if validReplacement {
			if err := bizservice.FinishUserProfileInvalidation(workCtx, s.rdb, userID, profileToken); err != nil {
				return false, abandon(err)
			}
			if err := s.clearLuoguCleanupIntent(ctx, pending); err != nil {
				return false, err
			}
			return false, nil
		}

		// The replacement that justified cancellation disappeared or became
		// invalid. Resume the same durable cleanup instead of leaving a live odd
		// profile fence behind. Recheck inside the facts transaction so a valid
		// replacement can never be deleted, even if the DB was changed outside
		// the normal platform-lock path.
		if _, err := bizservice.BumpUserProfileOwnedGeneration(workCtx, s.rdb, userID, profileToken, task.GenerationKey(userID, spiderregistry.LuoGu), 7*24*time.Hour); err != nil {
			return false, abandon(err)
		}
		generationBumped = true
		replacementRestored := false
		if err := validateOwner(); err != nil {
			return false, abandon(err)
		}
		if err := s.db.WithContext(workCtx).Transaction(func(tx *gorm.DB) error {
			var latest model.Platform
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&latest).Error
			if err == nil {
				if (latest.Id != cleanupPayload.BindingID || latest.Username != cleanupPayload.Username) && luoguUIDPattern.MatchString(strings.TrimSpace(latest.Username)) {
					replacementRestored = true
					return nil
				}
				if err := deleteSpiderPlatformData(workCtx, tx, userID, spiderregistry.LuoGu); err != nil {
					return err
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return advanceLuoguCleanupPhase(workCtx, tx, pending, "facts")
		}); err != nil {
			return false, abandon(err)
		}
		if replacementRestored {
			if err := bizservice.FinishUserProfileInvalidation(workCtx, s.rdb, userID, profileToken); err != nil {
				return false, abandon(err)
			}
			if err := s.clearLuoguCleanupIntent(ctx, pending); err != nil {
				return false, err
			}
			return false, nil
		}
		releaseCancelLock()
	}
	if !generationBumped {
		if _, err := bizservice.BumpUserProfileOwnedGeneration(workCtx, s.rdb, userID, profileToken, task.GenerationKey(userID, spiderregistry.LuoGu), 7*24*time.Hour); err != nil {
			return false, abandon(err)
		}
	}
	removed := false
	if pending.Phase == "intent" {
		if err := validateOwner(); err != nil {
			return false, abandon(err)
		}
		if err := s.db.WithContext(workCtx).Transaction(func(tx *gorm.DB) error {
			var current model.Platform
			if err := tx.Where("user_id = ? AND platform = ?", userID, spiderregistry.LuoGu).First(&current).Error; err != nil {
				if err == gorm.ErrRecordNotFound {
					return advanceLuoguCleanupPhase(workCtx, tx, pending, "facts")
				}
				return err
			}
			if current.Id != cleanupPayload.BindingID || current.Username != cleanupPayload.Username {
				return fmt.Errorf("LuoGu cleanup binding identity changed")
			}
			removed = true
			if err := deleteSpiderPlatformData(workCtx, tx, userID, spiderregistry.LuoGu); err != nil {
				return err
			}
			return advanceLuoguCleanupPhase(workCtx, tx, pending, "facts")
		}); err != nil {
			return false, abandon(err)
		}
	} else {
		removed = true
	}
	if err := validateOwner(); err != nil {
		return false, abandon(err)
	}
	// 换绑清理阶段只删除旧 OJ 的源数据，保留已发布画像；新绑定的
	// 提交爬完后由 BindSubmitsAfterSpider 触发强制重构原子替换。
	if pending.Phase == "facts" {
		if err := validateOwner(); err != nil {
			return false, abandon(err)
		}
		var target model.AbilityMaintenanceTarget
		if err := s.db.WithContext(workCtx).Where("intent_id = ? AND user_id = ?", pending.OperationID, userID).First(&target).Error; err != nil {
			return false, abandon(err)
		}
		if err := validateOwner(); err != nil {
			return false, abandon(err)
		}
		if err := bizservice.AdvanceAbilityMaintenanceTarget(workCtx, s.db, pending, &target, "outbox_ready", "derived_ready", ""); err != nil {
			return false, abandon(err)
		}
	}
	if pending.Phase == "derived_ready" {
		if err := bizservice.FinishUserProfileInvalidation(workCtx, s.rdb, userID, profileToken); err != nil {
			return false, abandon(err)
		}
		if err := advanceLuoguCleanupPhase(ctx, s.db, pending, "fence_finalized"); err != nil {
			return false, err
		}
	}
	if pending.Phase == "fence_finalized" {
		pending, err = s.finalizeLuoguCleanupTailPersisted(ctx, pending, cleanupPayload)
		if err != nil {
			return false, err
		}
	}
	if pending.Phase == "tail_finalized" {
		// Invalid-binding cleanup must not rebuild the profile: the replacement
		// OJ has not been crawled yet. Keep the existing user_tag_ac rows and
		// finish only the durable cleanup intent; SetSpider marks the new OJ so
		// its post-crawl binding pass performs the forced rebuild.
		if err := s.clearLuoguCleanupIntent(ctx, pending); err != nil {
			return false, err
		}
	}
	return removed, nil
}

func (s *SpiderService) recoverPendingLuoguCleanups(ctx context.Context) {
	if s == nil || s.db == nil || s.rdb == nil || !s.db.Migrator().HasTable(&model.AbilityMaintenancePending{}) {
		return
	}
	pending, err := dal.LoadAbilityMaintenanceRecoveryBatch(ctx, s.db, []string{"luogu_cleanup"}, 50)
	if err != nil {
		log.Warnf("LuoGu cleanup recovery scan: %v", err)
		return
	}
	for i := range pending {
		claimed, touchErr := dal.TouchAbilityMaintenanceRecoveryAttempt(ctx, s.db, &pending[i], time.Now())
		if touchErr != nil {
			log.Warnf("LuoGu cleanup recovery touch intent=%s: %v", pending[i].OperationID, touchErr)
			continue
		}
		if !claimed {
			continue
		}
		var payload luoguCleanupPayload
		if err := json.Unmarshal([]byte(pending[i].Payload), &payload); err != nil {
			log.Warnf("LuoGu cleanup recovery payload intent=%s: %v", pending[i].OperationID, err)
			continue
		}
		if _, err := s.removeInvalidLuoguBinding(ctx, pending[i].UserID, payload.AuthorizedUID); err != nil {
			log.Warnf("LuoGu cleanup recovery user=%d intent=%s: %v", pending[i].UserID, pending[i].OperationID, err)
		}
	}
}

func (s *SpiderService) runLuoguCleanupRecovery() {
	s.recoverPendingLuoguCleanups(context.Background())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.recoverPendingLuoguCleanups(context.Background())
	}
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
	if s.db != nil && s.db.Migrator().HasTable(&model.ClientSyncAudit{}) {
		var audit model.ClientSyncAudit
		if err := s.db.WithContext(ctx).Select("status").First(&audit, "session_id = ?", state.ID).Error; err == nil && audit.Status == "expired" {
			return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已过期")
		}
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
	if err != nil {
		return nil, kratoserrors.ServiceUnavailable("SYNC_UNAVAILABLE", "同步服务暂不可用")
	}
	if len(values) == 0 {
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
		s.failLuoguSyncAudit(state, kratoserrors.Conflict("LUOGU_RECORDS_CHANGED", "洛谷记录持续变化，请稍后重试"))
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
	if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok {
		if auditErr := auditor.UpdateClientSyncAudit(ctx, bizservice.ClientSyncAuditProgress{SessionID: state.ID, ProcessedPages: 0, RemoteCount: remoteCount, Inserted: state.Inserted, RestartCount: state.Restarts, UpdatedAt: s.luoguNow()}); auditErr != nil {
			log.Warnf("client-sync audit restart session=%s: %v", state.ID, auditErr)
		}
	}
	return response, nil
}

func (s *SpiderService) markLuoguSessionTerminated(state *luoguSession) error {
	if s == nil || state == nil {
		return nil
	}
	if finalizer, ok := s.luoguImporter.(luoguSessionFinalizer); ok {
		markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := finalizer.MarkClientSyncSessionTerminated(markCtx, state.ID, s.luoguNow()); err != nil {
			return err
		}
	}
	if auditor, ok := s.luoguImporter.(luoguSyncAuditor); ok && !state.Done {
		if err := auditor.TerminateClientSyncAudit(context.Background(), state.ID, "terminated", "terminated", "SESSION_TERMINATED", "同步会话已终止", s.luoguNow()); err != nil {
			return err
		}
	}
	if state.Inserted > 0 && !state.Done && s.luoguImporter != nil {
		s.luoguImporter.ScheduleSubmitPostProcess(state.UserID)
	}
	return nil
}

func (s *SpiderService) deleteLuoguSessionKeys(ctx context.Context, state *luoguSession) error {
	if s == nil || s.rdb == nil || state == nil {
		return nil
	}
	err := luoguTerminateScript.Run(ctx, s.rdb, []string{
		luoguSyncSessionKey(state.ID), luoguSyncTokenKey(state.TokenHash),
		luoguSyncActiveKey(state.UserID, state.LuoguUID), luoguSyncLockKey(state.ID),
		luoguSyncIssuanceKey(state.AuthorizationID, state.RequestIDHash),
	}, state.ID).Err()
	_ = s.rdb.SRem(ctx, luoguSyncUserSessionsKey(state.UserID), state.ID).Err()
	return err
}

func (s *SpiderService) ensureLuoguSyncAuditActive(ctx context.Context, state *luoguSession) error {
	if s == nil || s.db == nil || state == nil || !s.db.Migrator().HasTable(&model.ClientSyncAudit{}) {
		return nil
	}
	now := s.luoguNow().UTC()
	updated := s.db.WithContext(ctx).Model(&model.ClientSyncAudit{}).
		Where("session_id = ? AND status = ? AND terminal_at IS NULL AND updated_at > ?", state.ID, "running", now.Add(-luoguSyncSessionTTL)).
		Updates(map[string]interface{}{"updated_at": now})
	if updated.Error != nil {
		log.Warnf("client-sync audit activity session=%s: %v", state.ID, updated.Error)
		return nil
	}
	if updated.RowsAffected == 1 {
		return nil
	}
	var audit model.ClientSyncAudit
	if err := s.db.WithContext(ctx).Select("status").First(&audit, "session_id = ?", state.ID).Error; err == nil && audit.Status == "expired" {
		return kratoserrors.Unauthorized("SESSION_EXPIRED", "同步会话已过期")
	}
	return nil
}

// purgeLuoguSyncRedis removes only sessions indexed for this user. The index
// avoids a Redis KEYS scan and also covers session-specific token/lock keys.
func (s *SpiderService) purgeLuoguSyncRedis(ctx context.Context, userID int64) (int, error) {
	if s == nil || s.rdb == nil || userID <= 0 {
		return 0, nil
	}
	index := luoguSyncUserSessionsKey(userID)
	sessionIDs, err := s.rdb.SMembers(ctx, index).Result()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, sessionID := range sessionIDs {
		values, readErr := s.rdb.HGetAll(ctx, luoguSyncSessionKey(sessionID)).Result()
		if readErr != nil {
			return deleted, readErr
		}
		keys := []string{luoguSyncSessionKey(sessionID), luoguSyncLockKey(sessionID)}
		if uid := parseInt64(values["user_id"]); uid == userID {
			keys = append(keys, luoguSyncActiveKey(userID, values["luogu_uid"]), luoguSyncCooldownKey(userID, values["luogu_uid"]))
		}
		if hash := values["token_hash"]; hash != "" {
			keys = append(keys, luoguSyncTokenKey(hash))
		}
		if authID := parseUint64(values["authorization_id"]); authID > 0 && values["request_id_hash"] != "" {
			keys = append(keys, luoguSyncIssuanceKey(authID, values["request_id_hash"]))
		}
		if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
			return deleted, err
		}
		_ = s.rdb.SRem(ctx, index, sessionID).Err()
		deleted++
	}
	if err := s.rdb.Del(ctx, index).Err(); err != nil {
		return deleted, err
	}
	uids, err := s.rdb.SMembers(ctx, luoguSyncUserUIDsKey(userID)).Result()
	if err != nil {
		return deleted, err
	}
	for _, uid := range uids {
		if err := s.rdb.Del(ctx, luoguSyncActiveKey(userID, uid), luoguSyncCooldownKey(userID, uid)).Err(); err != nil {
			return deleted, err
		}
	}
	if err := s.rdb.Del(ctx, luoguSyncUserUIDsKey(userID)).Err(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (s *SpiderService) finalizeLuoguSessionForCleanup(ctx context.Context, state *luoguSession) error {
	if err := s.markLuoguSessionTerminated(state); err != nil {
		return err
	}
	return s.deleteLuoguSessionKeys(ctx, state)
}

func (s *SpiderService) terminateLuoguSession(ctx context.Context, state *luoguSession) {
	if s == nil || s.rdb == nil || state == nil {
		return
	}
	if err := s.markLuoguSessionTerminated(state); err != nil {
		log.Errorf("client-sync mark termination session=%s user=%d: %v", state.ID, state.UserID, err)
	}
	if err := s.deleteLuoguSessionKeys(ctx, state); err != nil {
		log.Errorf("client-sync terminate Redis session=%s user=%d: %v", state.ID, state.UserID, err)
	}
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
