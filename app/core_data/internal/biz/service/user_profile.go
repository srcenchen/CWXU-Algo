package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/loadgate"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// 画像缓存：model+evidence 精确失效 + latest 兜底。
const (
	userProfileCacheSchema   = "9"
	userProfileLatestTTL     = 30 * 24 * time.Hour
	userProfileExactTTL      = 7 * 24 * time.Hour
	userProfileEmptyProofTTL = 6 * time.Hour
	// userProfileFpKey 用户 AC 指纹缓存：数据未变化时跳过重建，削 3h 整点画像风暴
	userProfileFpPref           = "user_profile:fp:"
	profileGlobalGenerationKey  = "problem:user_profile:generation:global"
	profileUserGenerationPref   = "problem:user_profile:generation:user:"
	profileInvalidationLeaseTTL = 30 * time.Minute
)

var (
	profileBeginInvalidationScript = redis.NewScript(`
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
local intent = redis.call("GET", KEYS[3])
local lease = redis.call("GET", KEYS[2])
if current % 2 == 1 then
  if intent ~= ARGV[3] then return -2 end
  if lease == ARGV[1] then
    redis.call("PEXPIRE", KEYS[2], ARGV[2])
    return current
  end
  if lease then return -1 end
  if not redis.call("SET", KEYS[2], ARGV[1], "NX", "PX", ARGV[2]) then return -1 end
  return current
end
if intent or lease then return -3 end
if redis.call("GET", KEYS[1]) == "9223372036854775807" then return -4 end
local next = redis.call("INCR", KEYS[1])
redis.call("SET", KEYS[3], ARGV[3])
redis.call("SET", KEYS[2], ARGV[1], "PX", ARGV[2])
return next`)
	profileValidateInvalidationScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then return 0 end
if redis.call("GET", KEYS[3]) ~= ARGV[4] then return 0 end
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current ~= tonumber(ARGV[2]) or current % 2 ~= 1 then return 0 end
redis.call("PEXPIRE", KEYS[2], ARGV[3])
return 1`)
	profileFinishInvalidationScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then return 0 end
if redis.call("GET", KEYS[3]) ~= ARGV[3] then return 0 end
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current ~= tonumber(ARGV[2]) or current % 2 ~= 1 then return 0 end
local next = redis.call("INCR", KEYS[1])
redis.call("DEL", KEYS[2])
redis.call("DEL", KEYS[3])
return next`)
	profileAbandonInvalidationScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then return 0 end
if redis.call("GET", KEYS[3]) ~= ARGV[3] then return 0 end
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current ~= tonumber(ARGV[2]) or current % 2 ~= 1 then return 0 end
redis.call("DEL", KEYS[2])
return 1`)
	profileDeleteOwnedScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then return -1 end
if redis.call("GET", KEYS[3]) ~= ARGV[3] then return -1 end
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current ~= tonumber(ARGV[2]) or current % 2 ~= 1 then return -1 end
redis.call("PEXPIRE", KEYS[2], ARGV[4])
if #KEYS <= 3 then return 0 end
local doomed = {}
for i = 4, #KEYS do doomed[#doomed + 1] = KEYS[i] end
return redis.call("DEL", unpack(doomed))`)
	profileIncrementGenerationOwnedScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then return -1 end
if redis.call("GET", KEYS[3]) ~= ARGV[3] then return -1 end
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current ~= tonumber(ARGV[2]) or current % 2 ~= 1 then return -1 end
redis.call("PEXPIRE", KEYS[2], ARGV[4])
local next = redis.call("INCR", KEYS[4])
redis.call("PEXPIRE", KEYS[4], ARGV[5])
return next`)
	profilePublishScript = redis.NewScript(`
local global = tonumber(redis.call("GET", KEYS[1]) or "0")
local user = tonumber(redis.call("GET", KEYS[2]) or "0")
if global ~= tonumber(ARGV[1]) or user ~= tonumber(ARGV[2]) or global % 2 == 1 or user % 2 == 1 then
  return 0
end
redis.call("SET", KEYS[3], ARGV[3], "PX", ARGV[4])
redis.call("SET", KEYS[4], ARGV[3], "PX", ARGV[5])
redis.call("SET", KEYS[5], ARGV[6])
return 1`)
	profileReadScript = redis.NewScript(`
local global = tonumber(redis.call("GET", KEYS[1]) or "0")
local user = tonumber(redis.call("GET", KEYS[2]) or "0")
if global ~= tonumber(ARGV[1]) or user ~= tonumber(ARGV[2]) or global % 2 == 1 or user % 2 == 1 then
  return false
end
return redis.call("GET", KEYS[3])`)
)

type profileCacheGeneration struct {
	Global uint64
	User   uint64
}

// ProfileInvalidationToken uniquely owns one odd generation lease. The owner
// prevents concurrent callers from completing each other's invalidation.
type ProfileInvalidationToken struct {
	Generation uint64
	IntentID   string
	Owner      string
	lease      *profileInvalidationLease
}

type profileInvalidationLease struct {
	rdb      *redis.Client
	key      string
	ttl      time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	errMu    sync.Mutex
	err      error
}

func (l *profileInvalidationLease) setError(err error) {
	if l == nil || err == nil {
		return
	}
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
		l.cancel()
	}
	l.errMu.Unlock()
}

func (l *profileInvalidationLease) leaseError() error {
	if l == nil {
		return nil
	}
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

func (token ProfileInvalidationToken) Context() context.Context {
	if token.lease == nil {
		return context.Background()
	}
	return token.lease.ctx
}

func (token ProfileInvalidationToken) stopHeartbeat() {
	if token.lease == nil {
		return
	}
	token.lease.stopOnce.Do(func() { close(token.lease.stop) })
	<-token.lease.done
}

func profileUserGenerationKey(userID int64) string {
	return fmt.Sprintf("%s%d", profileUserGenerationPref, userID)
}

func readProfileCacheGeneration(ctx context.Context, rdb *redis.Client, userID int64) (profileCacheGeneration, error) {
	if rdb == nil {
		return profileCacheGeneration{}, nil
	}
	values, err := rdb.MGet(ctx, profileGlobalGenerationKey, profileUserGenerationKey(userID)).Result()
	if err != nil {
		return profileCacheGeneration{}, err
	}
	parse := func(value interface{}) (uint64, error) {
		if value == nil {
			return 0, nil
		}
		var parsed uint64
		if _, err := fmt.Sscan(fmt.Sprint(value), &parsed); err != nil {
			return 0, err
		}
		return parsed, nil
	}
	global, err := parse(values[0])
	if err != nil {
		return profileCacheGeneration{}, err
	}
	user, err := parse(values[1])
	if err != nil {
		return profileCacheGeneration{}, err
	}
	gen := profileCacheGeneration{Global: global, User: user}
	if gen.Global%2 == 1 || gen.User%2 == 1 {
		return gen, fmt.Errorf("user profile cache invalidation in progress")
	}
	return gen, nil
}

func beginProfileInvalidation(ctx context.Context, rdb *redis.Client, key string) (ProfileInvalidationToken, error) {
	return beginProfileInvalidationForIntentWithTTL(ctx, rdb, key, uuid.NewString(), profileInvalidationLeaseTTL)
}

func beginProfileInvalidationForIntentWithTTL(ctx context.Context, rdb *redis.Client, key, intentID string, ttl time.Duration) (ProfileInvalidationToken, error) {
	return beginProfileInvalidationForAttemptWithTTL(ctx, rdb, key, intentID, uuid.NewString(), ttl)
}

func beginProfileInvalidationForAttemptWithTTL(ctx context.Context, rdb *redis.Client, key, intentID, ownerID string, ttl time.Duration) (ProfileInvalidationToken, error) {
	token := ProfileInvalidationToken{IntentID: intentID, Owner: ownerID}
	if strings.TrimSpace(intentID) == "" {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid profile invalidation intent")
	}
	if strings.TrimSpace(ownerID) == "" {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid profile invalidation owner")
	}
	if rdb == nil {
		return token, nil
	}
	if ttl <= 0 {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid profile invalidation lease TTL")
	}
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileBeginInvalidationScript.Run(ctx, rdb, []string{key, key + ":lease", key + ":current_intent"}, leaseValue, ttl.Milliseconds(), token.IntentID).Int64()
	if err != nil {
		return ProfileInvalidationToken{}, err
	}
	if value < 0 {
		if value == -2 {
			return ProfileInvalidationToken{}, fmt.Errorf("profile invalidation intent changed")
		}
		if value == -3 {
			return ProfileInvalidationToken{}, fmt.Errorf("profile invalidation state corrupt")
		}
		if value == -4 {
			return ProfileInvalidationToken{}, fmt.Errorf("profile invalidation generation exhausted")
		}
		return ProfileInvalidationToken{}, fmt.Errorf("profile invalidation already in progress")
	}
	token.Generation = uint64(value)
	leaseCtx, cancel := context.WithCancel(ctx)
	token.lease = &profileInvalidationLease{
		rdb: rdb, key: key, ttl: ttl, ctx: leaseCtx, cancel: cancel,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go token.runHeartbeat()
	return token, nil
}

func (token ProfileInvalidationToken) runHeartbeat() {
	lease := token.lease
	if lease == nil {
		return
	}
	defer close(lease.done)
	interval := lease.ttl / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-lease.stop:
			return
		case <-lease.ctx.Done():
			if err := lease.ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
				lease.setError(err)
			}
			return
		case <-ticker.C:
			if err := validateProfileInvalidation(lease.ctx, lease.rdb, lease.key, token); err != nil {
				lease.setError(err)
				return
			}
		}
	}
}

func validateProfileInvalidation(ctx context.Context, rdb *redis.Client, key string, token ProfileInvalidationToken) error {
	if rdb == nil {
		return nil
	}
	if token.lease != nil {
		if err := token.lease.leaseError(); err != nil {
			return err
		}
	}
	ttl := profileInvalidationLeaseTTL
	if token.lease != nil {
		ttl = token.lease.ttl
	}
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileValidateInvalidationScript.Run(ctx, rdb, []string{key, key + ":lease", key + ":current_intent"}, leaseValue, token.Generation, ttl.Milliseconds(), token.IntentID).Int64()
	if err != nil {
		return err
	}
	if value != 1 {
		return fmt.Errorf("profile invalidation ownership changed")
	}
	return nil
}

func incrementGenerationOwned(ctx context.Context, rdb *redis.Client, fenceKey string, token ProfileInvalidationToken, targetGenerationKey string, targetTTL time.Duration) (int64, error) {
	if rdb == nil {
		return 0, fmt.Errorf("owned generation increment: redis unavailable")
	}
	if strings.TrimSpace(fenceKey) == "" || strings.TrimSpace(targetGenerationKey) == "" || targetTTL <= 0 {
		return 0, fmt.Errorf("owned generation increment: invalid target")
	}
	if token.lease != nil {
		if err := token.lease.leaseError(); err != nil {
			return 0, err
		}
	}
	leaseTTL := profileInvalidationLeaseTTL
	if token.lease != nil {
		leaseTTL = token.lease.ttl
	}
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileIncrementGenerationOwnedScript.Run(ctx, rdb, []string{
		fenceKey, fenceKey + ":lease", fenceKey + ":current_intent", targetGenerationKey,
	}, leaseValue, token.Generation, token.IntentID, leaseTTL.Milliseconds(), targetTTL.Milliseconds()).Int64()
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("profile invalidation ownership changed")
	}
	return value, nil
}

// BumpUserProfileOwnedGeneration increments a dependent generation only while
// token still owns the user's odd profile invalidation fence.
func BumpUserProfileOwnedGeneration(ctx context.Context, rdb *redis.Client, userID int64, token ProfileInvalidationToken, targetGenerationKey string, targetTTL time.Duration) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("invalid user_id")
	}
	return incrementGenerationOwned(ctx, rdb, profileUserGenerationKey(userID), token, targetGenerationKey, targetTTL)
}

func finishProfileInvalidation(ctx context.Context, rdb *redis.Client, key string, token ProfileInvalidationToken) error {
	if rdb == nil {
		return nil
	}
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileFinishInvalidationScript.Run(ctx, rdb, []string{key, key + ":lease", key + ":current_intent"}, leaseValue, token.Generation, token.IntentID).Int64()
	if err != nil {
		return err
	}
	if value != int64(token.Generation+1) {
		return fmt.Errorf("profile invalidation generation changed")
	}
	return nil
}

func abandonProfileInvalidation(ctx context.Context, rdb *redis.Client, key string, token ProfileInvalidationToken) error {
	if rdb == nil {
		return nil
	}
	token.stopHeartbeat()
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileAbandonInvalidationScript.Run(ctx, rdb, []string{key, key + ":lease", key + ":current_intent"}, leaseValue, token.Generation, token.IntentID).Int64()
	if err != nil {
		return err
	}
	if value != 1 {
		return fmt.Errorf("profile invalidation ownership changed")
	}
	return nil
}

func deleteProfileKeysOwned(ctx context.Context, rdb *redis.Client, fenceKey string, token ProfileInvalidationToken, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	ttl := profileInvalidationLeaseTTL
	if token.lease != nil {
		ttl = token.lease.ttl
	}
	scriptKeys := []string{fenceKey, fenceKey + ":lease", fenceKey + ":current_intent"}
	scriptKeys = append(scriptKeys, keys...)
	leaseValue := token.IntentID + "|" + token.Owner
	value, err := profileDeleteOwnedScript.Run(ctx, rdb, scriptKeys, leaseValue, token.Generation, token.IntentID, ttl.Milliseconds()).Int64()
	if err != nil {
		return err
	}
	if value < 0 {
		return fmt.Errorf("profile invalidation ownership changed")
	}
	return nil
}

func deleteProfileCachePattern(ctx context.Context, rdb *redis.Client, fenceKey string, token ProfileInvalidationToken, pattern string, validate func(context.Context) error) error {
	if rdb == nil {
		return nil
	}
	var cursor uint64
	for {
		if validate != nil {
			if err := validate(ctx); err != nil {
				return err
			}
		}
		keys, next, err := rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := deleteProfileKeysOwned(ctx, rdb, fenceKey, token, keys); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// BeginUserProfileInvalidation fences publishers before a targeted DB fact change.
func BeginUserProfileInvalidation(ctx context.Context, rdb *redis.Client, userID int64) (ProfileInvalidationToken, error) {
	if userID <= 0 {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid user_id")
	}
	return beginProfileInvalidation(ctx, rdb, profileUserGenerationKey(userID))
}

func beginUserProfileInvalidationWithTTL(ctx context.Context, rdb *redis.Client, userID int64, ttl time.Duration) (ProfileInvalidationToken, error) {
	if userID <= 0 {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid user_id")
	}
	return beginProfileInvalidationForIntentWithTTL(ctx, rdb, profileUserGenerationKey(userID), uuid.NewString(), ttl)
}

func beginUserProfileInvalidationForIntent(ctx context.Context, rdb *redis.Client, userID int64, intentID string) (ProfileInvalidationToken, error) {
	return beginProfileInvalidationForIntentWithTTL(ctx, rdb, profileUserGenerationKey(userID), intentID, profileInvalidationLeaseTTL)
}

func BeginUserProfileInvalidationForIntent(ctx context.Context, rdb *redis.Client, userID int64, intentID string) (ProfileInvalidationToken, error) {
	if userID <= 0 {
		return ProfileInvalidationToken{}, fmt.Errorf("invalid user_id")
	}
	return beginUserProfileInvalidationForIntent(ctx, rdb, userID, intentID)
}

func ValidateUserProfileInvalidation(ctx context.Context, rdb *redis.Client, userID int64, token ProfileInvalidationToken) error {
	return validateProfileInvalidation(ctx, rdb, profileUserGenerationKey(userID), token)
}

func beginGlobalProfileInvalidationForIntent(ctx context.Context, rdb *redis.Client, intentID string) (ProfileInvalidationToken, error) {
	return beginProfileInvalidationForIntentWithTTL(ctx, rdb, profileGlobalGenerationKey, intentID, profileInvalidationLeaseTTL)
}

func BeginGlobalProfileInvalidationForIntent(ctx context.Context, rdb *redis.Client, intentID string) (ProfileInvalidationToken, error) {
	return beginGlobalProfileInvalidationForIntent(ctx, rdb, intentID)
}

// FinishUserProfileInvalidation removes old identities before reopening readers/publishers.
func FinishUserProfileInvalidation(ctx context.Context, rdb *redis.Client, userID int64, token ProfileInvalidationToken) error {
	if rdb == nil {
		return nil
	}
	defer token.stopHeartbeat()
	key := profileUserGenerationKey(userID)
	validate := func(checkCtx context.Context) error { return validateProfileInvalidation(checkCtx, rdb, key, token) }
	if err := validate(ctx); err != nil {
		return err
	}
	if err := deleteProfileCachePattern(ctx, rdb, key, token, fmt.Sprintf("problem:user_profile:s*:u%d:*", userID), validate); err != nil {
		return err
	}
	if err := deleteProfileKeysOwned(ctx, rdb, key, token, []string{
		userProfileFpKey(userID),
		fmt.Sprintf("user_profile:pending:%d", userID),
		fmt.Sprintf("user:%d:profile", userID),
	}); err != nil {
		return err
	}
	return finishProfileInvalidation(ctx, rdb, key, token)
}

func AbandonUserProfileInvalidation(ctx context.Context, rdb *redis.Client, userID int64, token ProfileInvalidationToken) error {
	return abandonProfileInvalidation(ctx, rdb, profileUserGenerationKey(userID), token)
}

func BeginGlobalProfileInvalidation(ctx context.Context, rdb *redis.Client) (ProfileInvalidationToken, error) {
	return beginProfileInvalidation(ctx, rdb, profileGlobalGenerationKey)
}

func ValidateGlobalProfileInvalidation(ctx context.Context, rdb *redis.Client, token ProfileInvalidationToken) error {
	return validateProfileInvalidation(ctx, rdb, profileGlobalGenerationKey, token)
}

func FinishGlobalProfileInvalidation(ctx context.Context, rdb *redis.Client, token ProfileInvalidationToken) error {
	if rdb == nil {
		return nil
	}
	defer token.stopHeartbeat()
	validate := func(checkCtx context.Context) error {
		return validateProfileInvalidation(checkCtx, rdb, profileGlobalGenerationKey, token)
	}
	if err := validate(ctx); err != nil {
		return err
	}
	for _, pattern := range []string{"problem:user_profile:s*:u*:*", userProfileFpPref + "*", "user_profile:pending:*"} {
		if err := deleteProfileCachePattern(ctx, rdb, profileGlobalGenerationKey, token, pattern, validate); err != nil {
			return err
		}
	}
	return finishProfileInvalidation(ctx, rdb, profileGlobalGenerationKey, token)
}

func AbandonGlobalProfileInvalidation(ctx context.Context, rdb *redis.Client, token ProfileInvalidationToken) error {
	return abandonProfileInvalidation(ctx, rdb, profileGlobalGenerationKey, token)
}

func userProfileFpKey(userID int64) string {
	return fmt.Sprintf("%s%d", userProfileFpPref, userID)
}

func userProfileBuildVersion(modelVersion uint64, evidenceVersion string) string {
	return fmt.Sprintf("m%d:e%s", modelVersion, evidenceVersion)
}

// profileLightSF 只折叠 HTTP light read；light read 从不写缓存。
var profileLightSF singleflight.Group
var profileEmptyValidationSF singleflight.Group

var ErrUserProfileNotReady = errors.New("user profile aggregate is not ready")

type userProfileBuildState struct {
	mu              sync.Mutex
	forceGeneration atomic.Uint64
	refs            int
}

var (
	userProfileBuildStateRegistryMu sync.Mutex
	userProfileBuildStates          sync.Map
)

func profileBuildState(userID int64) *userProfileBuildState {
	userProfileBuildStateRegistryMu.Lock()
	defer userProfileBuildStateRegistryMu.Unlock()
	state, _ := userProfileBuildStates.LoadOrStore(userID, &userProfileBuildState{})
	buildState := state.(*userProfileBuildState)
	buildState.refs++
	return buildState
}

func releaseProfileBuildState(userID int64, state *userProfileBuildState) {
	state.mu.Unlock()
	userProfileBuildStateRegistryMu.Lock()
	defer userProfileBuildStateRegistryMu.Unlock()
	state.refs--
	if state.refs != 0 {
		return
	}
	if current, ok := userProfileBuildStates.Load(userID); ok && current == state {
		userProfileBuildStates.Delete(userID)
	}
}

// UserProfileSnapshot 可 gob 缓存的画像快照
type UserProfileSnapshot struct {
	SchemaVersion    string
	ModelVersion     uint64
	EvidenceVersion  string
	GlobalGeneration uint64
	UserGeneration   uint64
	// EmptyRadarVerified is set only by the authoritative MQ builder after it
	// has rebuilt the tag aggregate. Legacy empty snapshots remain false and
	// receive one source-data validation before being trusted.
	EmptyRadarVerified bool
	Radar              []struct {
		Tag     string
		Score   float64
		ACCount int64
	}
	Platforms []struct {
		Name  string
		Count int64
	}
	Difficulties []struct {
		Name  string
		Count int64
	}
	TotalAC int64
	BuiltAt int64 // unix sec
}

func userProfileExactCacheKey(userID int64, modelVersion uint64, evidenceVersion string) string {
	return userProfileExactCacheKeyForGeneration(userID, modelVersion, evidenceVersion, profileCacheGeneration{})
}

func userProfileExactCacheKeyForGeneration(userID int64, modelVersion uint64, evidenceVersion string, gen profileCacheGeneration) string {
	if evidenceVersion == "" {
		evidenceVersion = "0"
	}
	return fmt.Sprintf("problem:user_profile:s%s:u%d:m%d:e%s:g%d:u%d", userProfileCacheSchema, userID, modelVersion, evidenceVersion, gen.Global, gen.User)
}

func userProfileLatestKey(userID int64) string {
	return fmt.Sprintf("problem:user_profile:s%s:u%d:latest", userProfileCacheSchema, userID)
}

func (uc *ProblemUseCase) readProfileCache(ctx context.Context, key string) (*UserProfileSnapshot, bool) {
	if uc.data == nil || uc.data.RDB == nil || key == "" {
		return nil, false
	}
	b, err := uc.data.RDB.Get(ctx, key).Bytes()
	if err != nil || len(b) == 0 {
		return nil, false
	}
	var snap UserProfileSnapshot
	if err := utils.GobDecoder(b, &snap); err != nil {
		return nil, false
	}
	return &snap, true
}

func (uc *ProblemUseCase) readProfileCacheAtGeneration(ctx context.Context, userID int64, key string, gen profileCacheGeneration) (*UserProfileSnapshot, bool) {
	if uc.data == nil || uc.data.RDB == nil || key == "" {
		return nil, false
	}
	value, err := profileReadScript.Run(ctx, uc.data.RDB, []string{
		profileGlobalGenerationKey, profileUserGenerationKey(userID), key,
	}, gen.Global, gen.User).Result()
	if err != nil || value == nil {
		return nil, false
	}
	var b []byte
	switch typed := value.(type) {
	case string:
		b = []byte(typed)
	case []byte:
		b = typed
	default:
		return nil, false
	}
	if len(b) == 0 {
		return nil, false
	}
	var snap UserProfileSnapshot
	if err := utils.GobDecoder(b, &snap); err != nil {
		return nil, false
	}
	return &snap, true
}

func (uc *ProblemUseCase) writeProfileCache(ctx context.Context, userID int64, modelVersion uint64, evidenceVersion string, snap *UserProfileSnapshot) error {
	gen, err := readProfileCacheGeneration(ctx, uc.data.RDB, userID)
	if err != nil {
		return err
	}
	return uc.writeProfileCacheAtGeneration(ctx, userID, modelVersion, evidenceVersion, gen, snap)
}

func (uc *ProblemUseCase) writeProfileCacheAtGeneration(ctx context.Context, userID int64, modelVersion uint64, evidenceVersion string, gen profileCacheGeneration, snap *UserProfileSnapshot) error {
	if uc.data == nil || uc.data.RDB == nil || snap == nil || userID <= 0 {
		return nil
	}
	if snap.BuiltAt == 0 {
		snap.BuiltAt = time.Now().Unix()
	}
	snap.SchemaVersion = userProfileCacheSchema
	snap.ModelVersion = modelVersion
	snap.EvidenceVersion = evidenceVersion
	snap.GlobalGeneration = gen.Global
	snap.UserGeneration = gen.User
	b, err := utils.GobEncoder(snap)
	if err != nil {
		return fmt.Errorf("user_profile gob encode user=%d: %w", userID, err)
	}
	published, err := profilePublishScript.Run(ctx, uc.data.RDB, []string{
		profileGlobalGenerationKey, profileUserGenerationKey(userID),
		userProfileExactCacheKeyForGeneration(userID, modelVersion, evidenceVersion, gen),
		userProfileLatestKey(userID), userProfileFpKey(userID),
	}, gen.Global, gen.User, b, userProfileExactTTL.Milliseconds(), userProfileLatestTTL.Milliseconds(), userProfileBuildVersionAtGeneration(modelVersion, evidenceVersion, gen)).Int64()
	if err != nil {
		return fmt.Errorf("user_profile atomic cache publish user=%d: %w", userID, err)
	}
	if published != 1 {
		return fmt.Errorf("user profile generation changed during publish")
	}
	return nil
}

func (uc *ProblemUseCase) cacheBuiltProfile(ctx context.Context, userID int64, modelVersion uint64, evidenceVersion string, snap *UserProfileSnapshot) error {
	return uc.writeProfileCache(ctx, userID, modelVersion, evidenceVersion, snap)
}

func (uc *ProblemUseCase) cacheBuiltProfileAtGeneration(ctx context.Context, userID int64, modelVersion uint64, evidenceVersion string, gen profileCacheGeneration, snap *UserProfileSnapshot) error {
	return uc.writeProfileCacheAtGeneration(ctx, userID, modelVersion, evidenceVersion, gen, snap)
}

func userProfileBuildVersionAtGeneration(modelVersion uint64, evidenceVersion string, gen profileCacheGeneration) string {
	base := userProfileBuildVersion(modelVersion, evidenceVersion)
	if gen.Global == 0 && gen.User == 0 {
		return base
	}
	return fmt.Sprintf("%s:g%d:u%d", base, gen.Global, gen.User)
}

// EnqueueUserProfileRebuild 异步重建（绑平台/爬虫 / 每日 cron）
// 走 user_profile 队列，不阻塞 HTTP。
func (uc *ProblemUseCase) EnqueueUserProfileRebuild(userID int64) {
	if userID <= 0 || uc.profileTask == nil {
		return
	}
	_ = uc.profileTask.Do(userID)
}

func (uc *ProblemUseCase) enqueueUserProfileRebuildForce(userID int64) {
	if userID <= 0 || uc.profileTask == nil {
		return
	}
	_ = uc.profileTask.DoForce(userID)
}

// ForcePublishUserProfile queues a forced rebuild and reports MQ failure to
// synchronous maintenance callers.
func (uc *ProblemUseCase) ForcePublishUserProfile(userID int64) error {
	if userID <= 0 || uc.profileTask == nil {
		return fmt.Errorf("user profile publisher unavailable")
	}
	result := uc.profileTask.DoForce(userID)
	if result.Failed {
		return fmt.Errorf("force publish user profile %d failed", userID)
	}
	return nil
}

func (uc *ProblemUseCase) ForcePublishMaintenanceUserProfile(userID int64, intentID string) error {
	if userID <= 0 || intentID == "" || uc.profileTask == nil {
		return fmt.Errorf("user profile maintenance publisher unavailable")
	}
	if result := uc.profileTask.DoMaintenanceForce(userID, intentID); result.Failed {
		return fmt.Errorf("force publish maintenance profile %d failed", userID)
	}
	return nil
}

// BuildAndCacheUserProfile MQ consumer 用：先全量重建 user_tag_ac，再算画像写缓存。
// force=false 时按用户 AC 指纹跳过「数据未变化」的用户（3h 整点爬虫风暴主要削这里）。
// 重 JOIN 只在队列里跑；key 与 HTTP 轻量路径分离，避免抢到「未重建」的空结果。
func (uc *ProblemUseCase) BuildAndCacheUserProfile(userID int64, force bool) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user_id")
	}
	state := profileBuildState(userID)
	forceAtArrival := state.forceGeneration.Load()
	state.mu.Lock()
	defer releaseProfileBuildState(userID, state)
	if force && state.forceGeneration.Load() != forceAtArrival {
		return nil
	}
	_, err := func() (interface{}, error) {
		ctx := context.Background()
		generation, err := readProfileCacheGeneration(ctx, uc.data.RDB, userID)
		if err != nil {
			return nil, err
		}
		identity, err := dal.EnsureProfileCacheIdentityForBuild(ctx, uc.data.DB, userID)
		if err != nil {
			return nil, err
		}
		modelVersion := identity.ModelVersion
		evidenceVersion := identity.Evidence.String()
		buildVersion := userProfileBuildVersionAtGeneration(modelVersion, evidenceVersion, generation)
		if buildVersion != "" && !force && uc.data != nil && uc.data.RDB != nil {
			if last, e := uc.data.RDB.Get(ctx, userProfileFpKey(userID)).Result(); e == nil && last == buildVersion {
				log.Infof("user_profile skip unchanged user=%d version=%s", userID, buildVersion)
				return nil, nil
			}
		}
		// 系统过载时先退避，画像重 JOIN 让路给在线访问（最多等 30s）
		loadgate.Global().Wait(ctx, 30*time.Second)
		// 雷达预聚合从 user_ac_problems×problem_tags 重算，保证「做过有标签的题就一定有雷达」
		if err := dal.RebuildUserTagACForUserAtIdentity(ctx, uc.data.DB, userID, identity); err != nil {
			return nil, err
		}
		snap, computeErr := uc.computeUserProfileAtIdentity(userID, identity)
		identityAfter, err := dal.ReadProfileCacheIdentity(ctx, uc.data.DB, userID)
		if err != nil {
			return nil, err
		}
		if identityAfter != identity {
			return nil, fmt.Errorf(
				"user profile changed during build: model %d->%d evidence %q->%q",
				modelVersion, identityAfter.ModelVersion, evidenceVersion, identityAfter.Evidence.String(),
			)
		}
		if computeErr != nil {
			return nil, computeErr
		}
		snap.EmptyRadarVerified = len(snap.Radar) == 0
		if err := uc.cacheBuiltProfileAtGeneration(ctx, userID, modelVersion, evidenceVersion, generation, snap); err != nil {
			return nil, err
		}
		return snap, nil
	}()
	if err == nil && force {
		state.forceGeneration.Add(1)
	}
	return err
}

// buildUserProfileNow HTTP 冷启动：只读现有预聚合，不做重 JOIN，也不写 exact/latest。
// 所有正式快照只由 MQ builder 发布，避免 light read 覆盖并发重建结果。
func (uc *ProblemUseCase) buildUserProfileNow(userID int64, identity dal.ProfileCacheIdentity) (*UserProfileSnapshot, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user_id")
	}
	v, err, _ := profileLightSF.Do(fmt.Sprintf("up-light:%d:m%d:e%s", userID, identity.ModelVersion, identity.Evidence.String()), func() (interface{}, error) {
		return uc.computeUserProfileForLightReadAtIdentity(userID, identity)
	})
	if err != nil {
		return nil, err
	}
	snap, _ := v.(*UserProfileSnapshot)
	return snap, nil
}

func (uc *ProblemUseCase) enqueueInvalidEmptyProfileRebuild(ctx context.Context, userID int64, snap *UserProfileSnapshot) {
	if snap == nil || len(snap.Radar) != 0 || snap.EmptyRadarVerified || uc.data == nil || uc.data.DB == nil {
		return
	}
	proofKey := fmt.Sprintf(
		"problem:user_profile:s%s:u%d:empty-proof:m%d:e%s:g%d:u%d",
		userProfileCacheSchema, userID, snap.ModelVersion, snap.EvidenceVersion, snap.GlobalGeneration, snap.UserGeneration,
	)
	readProof := func() string {
		if uc.data.RDB == nil {
			return ""
		}
		proof, err := uc.data.RDB.Get(ctx, proofKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			log.Warnf("user_profile read empty radar proof user=%d: %v", userID, err)
			return ""
		}
		if proof == "tagged" || proof == "legitimate" {
			return proof
		}
		return ""
	}
	proof := readProof()
	if proof == "" {
		value, err, _ := profileEmptyValidationSF.Do(proofKey, func() (interface{}, error) {
			if cached := readProof(); cached != "" {
				return cached, nil
			}
			hasTaggedAC, err := dal.UserHasTaggedAC(ctx, uc.data.DB, userID)
			if err != nil {
				return "", err
			}
			result := "legitimate"
			if hasTaggedAC {
				result = "tagged"
			}
			if uc.data.RDB != nil {
				if err := uc.data.RDB.Set(ctx, proofKey, result, userProfileEmptyProofTTL).Err(); err != nil {
					log.Warnf("user_profile write empty radar proof user=%d: %v", userID, err)
				}
			}
			return result, nil
		})
		if err != nil {
			log.Warnf("user_profile validate empty radar user=%d: %v", userID, err)
			return
		}
		proof, _ = value.(string)
	}
	if proof == "tagged" {
		uc.enqueueUserProfileRebuildForce(userID)
	}
}

// UserProfile 读路径：缓存优先；HTTP 永不做 tag_ac 重 JOIN。
func (uc *ProblemUseCase) UserProfile(userID int64) (radar []struct {
	Tag     string
	Score   float64
	ACCount int64
}, platforms []struct {
	Name  string
	Count int64
}, difficulties []struct {
	Name  string
	Count int64
}, totalAC int64, err error) {
	ctx := context.Background()
	generation, generationErr := readProfileCacheGeneration(ctx, uc.data.RDB, userID)
	if generationErr != nil {
		err = generationErr
		return
	}
	identity, identityErr := dal.ReadProfileCacheIdentity(ctx, uc.data.DB, userID)
	if identityErr != nil {
		uc.enqueueUserProfileRebuildForce(userID)
		log.Warnf("user_profile identity unavailable, serving fallback user=%d: %v", userID, identityErr)
		snap, fallbackErr := uc.computeUserProfileFromPreaggregates(userID, dal.ProfileCacheIdentity{}, true)
		if fallbackErr != nil {
			err = fmt.Errorf("user profile identity user=%d: %w; fallback: %v", userID, identityErr, fallbackErr)
			return
		}
		return unpackProfile(snap)
	}
	modelVersion := identity.ModelVersion
	evidenceVersion := identity.Evidence.String()

	if snap, ok := uc.readProfileCacheAtGeneration(ctx, userID, userProfileExactCacheKeyForGeneration(userID, modelVersion, evidenceVersion, generation), generation); ok &&
		snap.SchemaVersion == userProfileCacheSchema && snap.ModelVersion == modelVersion && snap.EvidenceVersion == evidenceVersion &&
		snap.GlobalGeneration == generation.Global && snap.UserGeneration == generation.User {
		uc.enqueueInvalidEmptyProfileRebuild(ctx, userID, snap)
		return unpackProfile(snap)
	}
	// 精确 miss：证据和 generation 必须完全匹配；模型落后一版时允许
	// stale-while-revalidate，避免全局模型原子切换后队列尚未重建的用户雷达变空。
	// 旧模型快照只读返回，绝不会被写到 active model 的 exact/latest 身份下。
	if snap, ok := uc.readProfileCacheAtGeneration(ctx, userID, userProfileLatestKey(userID), generation); ok {
		latestReadable := snap.SchemaVersion == userProfileCacheSchema && snap.ModelVersion > 0 && snap.ModelVersion <= modelVersion && snap.EvidenceVersion == evidenceVersion &&
			snap.GlobalGeneration == generation.Global && snap.UserGeneration == generation.User
		if !latestReadable {
			log.Infof("user_profile reject stale latest user=%d snapshot model=%d evidence=%s", userID, snap.ModelVersion, snap.EvidenceVersion)
			uc.enqueueUserProfileRebuildForce(userID)
		} else {
			if snap.ModelVersion < modelVersion {
				log.Infof("user_profile serve stale model user=%d snapshot=%d active=%d", userID, snap.ModelVersion, modelVersion)
			}
			uc.enqueueUserProfileRebuildForce(userID)
			return unpackProfile(snap)
		}
	}

	// 从未处理：轻量即时算（读预聚合）；重 JOIN 交给队列
	start := time.Now()
	snap, e := uc.buildUserProfileNow(userID, identity)
	if e != nil {
		log.Errorf("user_profile on-demand user=%d: %v", userID, e)
		uc.EnqueueUserProfileRebuild(userID)
		err = e
		return
	}
	uc.enqueueUserProfileRebuildForce(userID)
	log.Infof("user_profile on-demand user=%d cost=%s", userID, time.Since(start).Round(time.Millisecond))
	return unpackProfile(snap)
}

func unpackProfile(snap *UserProfileSnapshot) (radar []struct {
	Tag     string
	Score   float64
	ACCount int64
}, platforms []struct {
	Name  string
	Count int64
}, difficulties []struct {
	Name  string
	Count int64
}, totalAC int64, err error) {
	if snap == nil {
		return
	}
	radar = snap.Radar
	platforms = snap.Platforms
	difficulties = snap.Difficulties
	totalAC = snap.TotalAC
	return
}

// computeUserProfile 雷达读 user_tag_ac；平台过题力扣走官方 acTotal；难度仍 JOIN 题库
func (uc *ProblemUseCase) computeUserProfile(userID int64) (*UserProfileSnapshot, error) {
	identity, err := dal.ReadProfileCacheIdentity(context.Background(), uc.data.DB, userID)
	if err != nil {
		return nil, err
	}
	return uc.computeUserProfileAtIdentity(userID, identity)
}

func (uc *ProblemUseCase) computeUserProfileAtIdentity(userID int64, identity dal.ProfileCacheIdentity) (*UserProfileSnapshot, error) {
	return uc.computeUserProfileFromPreaggregates(userID, identity, false)
}

// computeUserProfileForLightRead may display the latest readable tag aggregate
// while the active model's forced rebuild is still queued. The light path never
// writes exact/latest cache keys, so an older model cannot be published under
// the active model identity.
func (uc *ProblemUseCase) computeUserProfileForLightRead(userID int64) (*UserProfileSnapshot, error) {
	identity, err := dal.ReadProfileCacheIdentity(context.Background(), uc.data.DB, userID)
	if err != nil {
		return nil, err
	}
	return uc.computeUserProfileForLightReadAtIdentity(userID, identity)
}

func (uc *ProblemUseCase) computeUserProfileForLightReadAtIdentity(userID int64, identity dal.ProfileCacheIdentity) (*UserProfileSnapshot, error) {
	return uc.computeUserProfileFromPreaggregates(userID, identity, true)
}

func (uc *ProblemUseCase) computeUserProfileFromPreaggregates(userID int64, identity dal.ProfileCacheIdentity, allowStaleRadar bool) (*UserProfileSnapshot, error) {
	snap := &UserProfileSnapshot{BuiltAt: time.Now().Unix()}
	var computeErrs []error

	// 雷达：写时预聚合 user_tag_ac
	radarSnapshot, radarErr := dal.ListUserTagAbilitySnapshot(
		context.Background(), uc.data.DB, userID, identity, allowStaleRadar, int(^uint(0)>>1),
	)
	if radarErr != nil {
		err := radarErr
		log.Errorf("radar preagg user=%d: %v", userID, err)
		if !allowStaleRadar {
			computeErrs = append(computeErrs, fmt.Errorf("radar preagg: %w", err))
		}
	} else if !radarSnapshot.Ready {
		if !allowStaleRadar {
			computeErrs = append(computeErrs, ErrUserProfileNotReady)
		}
	} else {
		snap.ModelVersion = radarSnapshot.ModelVersion
		for _, t := range radarSnapshot.Rows {
			snap.Radar = append(snap.Radar, struct {
				Tag     string
				Score   float64
				ACCount int64
			}{Tag: t.Tag, Score: dal.TagAbilityScore(t.Weight, int(t.Count)), ACCount: t.Count})
		}
		sort.Slice(snap.Radar, func(i, j int) bool {
			if snap.Radar[i].Score != snap.Radar[j].Score {
				return snap.Radar[i].Score > snap.Radar[j].Score
			}
			if snap.Radar[i].ACCount != snap.Radar[j].ACCount {
				return snap.Radar[i].ACCount > snap.Radar[j].ACCount
			}
			return snap.Radar[i].Tag < snap.Radar[j].Tag
		})
	}

	// 平台过题：读 user_ac_problems；力扣优先官方 acTotal 合成键（e:LeetCode:ac-*）
	// 牛客统一为 NowCoder（不拆竞赛站 / Tracker）
	if plats, e := dal.ListUserPlatformAC(uc.data.DB, userID); e != nil {
		log.Errorf("platforms sql user=%d: %v", userID, e)
		computeErrs = append(computeErrs, fmt.Errorf("platforms: %w", e))
	} else {
		for _, p := range plats {
			snap.Platforms = append(snap.Platforms, struct {
				Name  string
				Count int64
			}{p.Name, p.Count})
		}
	}

	// 难度分布：从 user_ac_problems（按 user_id 索引）驱动，
	// p:/e: 分支分别走 problems 主键 / (platform, external_id) 唯一索引，
	// 替代整表 Seq Scan + OR 字符串拼接 JOIN（高 AC 用户不再拖慢画像）。
	type nc struct {
		Name  string
		Count int64
	}
	var diffs []nc
	difficultySQL := `
		SELECT name, COUNT(DISTINCT pid) AS count
		FROM (
			SELECT p.id AS pid, p.difficulty AS name
			FROM user_ac_problems u
			JOIN problems p ON p.id = NULLIF(substring(u.problem_key, 3), '')::bigint
			WHERE u.user_id = ?
			  AND u.problem_key LIKE 'p:%'
			  AND u.problem_key ~ '^p:[0-9]+$'
			  AND p.difficulty IS NOT NULL AND BTRIM(p.difficulty) <> ''
			  AND UPPER(BTRIM(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
			UNION ALL
			SELECT p.id, p.difficulty
			FROM user_ac_problems u
			JOIN problems p
			  ON p.platform = split_part(substring(u.problem_key, 3), ':', 1)
			 AND p.external_id = substring(substring(u.problem_key, 3) FROM position(':' IN substring(u.problem_key, 3)) + 1)
			 AND p.external_id IS NOT NULL AND btrim(p.external_id) <> ''
			WHERE u.user_id = ?
			  AND u.problem_key LIKE 'e:%'
			  AND p.difficulty IS NOT NULL AND BTRIM(p.difficulty) <> ''
			  AND UPPER(BTRIM(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
		) t
		GROUP BY name
	`
	if uc.data.DB.Dialector.Name() == "sqlite" {
		difficultySQL = `
			SELECT name, COUNT(DISTINCT pid) AS count
			FROM (
				SELECT p.id AS pid, p.difficulty AS name
				FROM user_ac_problems u
				JOIN problems p ON p.id = CAST(substr(u.problem_key, 3) AS INTEGER)
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'p:%'
				  AND substr(u.problem_key, 3) <> ''
				  AND substr(u.problem_key, 3) NOT GLOB '*[^0-9]*'
				  AND p.difficulty IS NOT NULL AND trim(p.difficulty) <> ''
				  AND upper(trim(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
				UNION ALL
				SELECT p.id, p.difficulty
				FROM user_ac_problems u
				JOIN problems p
				  ON p.platform = substr(substr(u.problem_key, 3), 1, instr(substr(u.problem_key, 3), ':') - 1)
				 AND p.external_id = substr(substr(u.problem_key, 3), instr(substr(u.problem_key, 3), ':') + 1)
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'e:%'
				  AND instr(substr(u.problem_key, 3), ':') > 0
				  AND p.external_id IS NOT NULL AND trim(p.external_id) <> ''
				  AND p.difficulty IS NOT NULL AND trim(p.difficulty) <> ''
				  AND upper(trim(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
			) t
			GROUP BY name
		`
	}
	if e := uc.data.DB.Raw(difficultySQL, userID, userID).Scan(&diffs).Error; e != nil {
		log.Errorf("difficulties sql user=%d: %v", userID, e)
		computeErrs = append(computeErrs, fmt.Errorf("difficulties: %w", e))
	}
	for _, d := range diffs {
		snap.Difficulties = append(snap.Difficulties, struct {
			Name  string
			Count int64
		}{d.Name, d.Count})
	}

	// 生涯 total 与 period.ac.total / 平台合计一致：力扣用官方合成键
	if n, e := dal.CountUserLifetimeAC(uc.data.DB, userID); e != nil {
		log.Errorf("totalAC sql user=%d: %v", userID, e)
		computeErrs = append(computeErrs, fmt.Errorf("total AC: %w", e))
	} else {
		snap.TotalAC = n
	}

	return snap, errors.Join(computeErrs...)
}
