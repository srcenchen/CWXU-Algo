package dal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const CurrentUserTagAbilityScoreVersion uint = 1

var userTagEmptyHealCursor atomic.Int64

// abilityLookupBatchSize keeps normalized tuple predicates below common SQL
// parameter limits while still avoiding one query per candidate.
const abilityLookupBatchSize = 200

var ErrUserTagAbilityModelChanged = errors.New("user tag ability model changed; retry rebuild")
var ErrUserTagAbilityIncomplete = errors.New("user tag ability facts incomplete; keep previous snapshot")
var ErrUserTagAbilityEvidenceChanged = errors.New("user tag ability evidence changed; retry rebuild")
var ErrUserTagAbilitySnapshotCorrupt = errors.New("user tag ability snapshot is inconsistent")

type UserTagAbility struct {
	Tag    string
	Count  int64
	Weight float64
}

// UserTagAbilitySnapshot makes the source model explicit so callers cannot
// treat an older snapshot as rows produced by the active model.
type UserTagAbilitySnapshot struct {
	Ready        bool
	ModelVersion uint64
	Evidence     ProfileEvidenceIdentity
	Rows         []UserTagAbility
}

// DifficultyWeight is retained only for legacy version-zero backfill data.
func DifficultyWeight(d string) float64 {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "简单", "easy", "入门":
		return 1
	case "中等", "medium", "中级":
		return 3
	case "困难", "hard", "高级":
		return 8
	default:
		return 2
	}
}

// The former incremental path writes a retired metric, so it must not mutate
// current Σx rows. A rebuild is the sole publisher for version one.
func IncUserTagAC(context.Context, *gorm.DB, int64, []string, float64) error { return nil }

func AdjustUserTagACForProblemTagsChange(ctx context.Context, db *gorm.DB, problemID uint, oldTags, newTags []string) error {
	if db == nil || problemID == 0 || sameStringSet(NormalizeTags(oldTags), NormalizeTags(newTags)) {
		return nil
	}
	ids, err := listUsersACProblem(ctx, db, problemID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := RebuildUserTagACForUser(ctx, db, id); err != nil {
			return err
		}
	}
	return nil
}

func listUsersACProblem(ctx context.Context, db *gorm.DB, problemID uint) ([]int64, error) {
	seen := map[int64]struct{}{}
	var out []int64
	add := func(ids []int64) {
		for _, id := range ids {
			if id > 0 {
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					out = append(out, id)
				}
			}
		}
	}
	var problem model.Problem
	if err := db.WithContext(ctx).First(&problem, problemID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, nil
		}
		return nil, err
	}
	external := abilityExternalKey{platform: normalizeAbilityPlatform(problem.Platform), external: strings.TrimSpace(problem.ExternalID)}
	var acRows []model.UserACProblem
	query := db.WithContext(ctx).Where("problem_key = ?", fmt.Sprintf("p:%d", problemID))
	if external.platform != "" && external.external != "" {
		query = db.WithContext(ctx).
			Where("problem_key = ? OR ("+abilityExternalKeySQL(db)+")", fmt.Sprintf("p:%d", problemID), external.platform, external.platform, external.external)
	}
	if err := query.Find(&acRows).Error; err != nil {
		return nil, err
	}
	for _, row := range acRows {
		if id, ok := strictProblemKeyID(row.ProblemKey); ok && id == problemID {
			add([]int64{row.UserID})
			continue
		}
		if key, ok := parseAbilityExternalKey(row.ProblemKey); ok && key == external {
			add([]int64{row.UserID})
		}
	}
	return out, nil
}

// ListUsersACProblem returns only users backed by canonical lifetime AC facts.
// Derived status rows are deliberately excluded so stale TRIED/AC status cannot widen fact edits.
func ListUsersACProblem(ctx context.Context, db *gorm.DB, problemID uint) ([]int64, error) {
	return listUsersACProblem(ctx, db, problemID)
}

func IncUserTagACForFirstProblemAC(context.Context, *gorm.DB, int64, uint) error { return nil }

func activeUserTagAbilityModelVersion(ctx context.Context, db *gorm.DB) (uint64, bool, error) {
	version, err := ActiveAbilityModelVersion(ctx, db)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return version, version > 0, nil
}

func ListUserTagAC(ctx context.Context, db *gorm.DB, userID int64, limit int) ([]UserTagAbility, error) {
	if db == nil || userID <= 0 {
		return nil, nil
	}
	identity, err := ReadProfileCacheIdentity(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	snapshot, err := ListUserTagAbilitySnapshot(ctx, db, userID, identity, false, limit)
	return snapshot.Rows, err
}

// ListStaleUserTagAC reads the newest usable current-score snapshot at or
// below the active model. It never relabels rows as active: ModelVersion is
// returned with the rows so the caller can keep stale data out of active
// cache keys while a rebuild runs in the background.
func ListStaleUserTagAC(ctx context.Context, db *gorm.DB, userID int64, limit int) (UserTagAbilitySnapshot, error) {
	if db == nil || userID <= 0 {
		return UserTagAbilitySnapshot{}, nil
	}
	identity, err := ReadProfileCacheIdentity(ctx, db, userID)
	if err != nil {
		return UserTagAbilitySnapshot{}, err
	}
	return ListUserTagAbilitySnapshot(ctx, db, userID, identity, true, limit)
}

// ListLatestUserTagACRows recovers the newest complete score rows when the
// snapshot header is temporarily unavailable. It never writes or relabels the
// rows; the caller must still enqueue a normal snapshot rebuild.
func ListLatestUserTagACRows(ctx context.Context, db *gorm.DB, userID int64, limit int) (UserTagAbilitySnapshot, error) {
	if db == nil || userID <= 0 {
		return UserTagAbilitySnapshot{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var version uint64
	if err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Where("user_id = ? AND count > 0", userID).
		Select("COALESCE(MAX(model_version), 0)").Scan(&version).Error; err != nil {
		return UserTagAbilitySnapshot{}, err
	}
	var scoreVersion uint
	if err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Where("user_id = ? AND model_version = ? AND count > 0", userID, version).
		Select("COALESCE(MAX(score_version), 0)").Scan(&scoreVersion).Error; err != nil {
		return UserTagAbilitySnapshot{}, err
	}
	var rows []UserTagAbility
	if err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Select("tag, count, weight").
		Where("user_id = ? AND score_version = ? AND model_version = ? AND count > 0", userID, scoreVersion, version).
		Find(&rows).Error; err != nil {
		return UserTagAbilitySnapshot{}, err
	}
	if len(rows) == 0 {
		return UserTagAbilitySnapshot{}, nil
	}
	sortUserTagAbilities(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return UserTagAbilitySnapshot{Ready: true, ModelVersion: version, Rows: rows}, nil
}

// ListUserTagAbilitySnapshot reads the publication header and all matching tag
// rows in one MVCC statement. This prevents a header from one rebuild being
// paired with rows from another, and represents a published zero-row radar as
// Ready=true.
func ListUserTagAbilitySnapshot(ctx context.Context, db *gorm.DB, userID int64, expected ProfileCacheIdentity, allowStaleModel bool, limit int) (UserTagAbilitySnapshot, error) {
	if db == nil || userID <= 0 || expected.ModelVersion == 0 {
		return UserTagAbilitySnapshot{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	type snapshotRow struct {
		ModelVersion uint64          `gorm:"column:model_version"`
		RowCount     int64           `gorm:"column:row_count"`
		Tag          sql.NullString  `gorm:"column:tag"`
		Count        sql.NullInt64   `gorm:"column:count"`
		Weight       sql.NullFloat64 `gorm:"column:weight"`
	}
	modelPredicate := "s.model_version = ?"
	if allowStaleModel {
		modelPredicate = "s.model_version > 0 AND s.model_version <= ?"
	}
	var records []snapshotRow
	err := db.WithContext(ctx).Raw(`SELECT s.model_version, s.row_count, t.tag, t.count, t.weight
		FROM user_tag_ac_snapshots s
		LEFT JOIN user_tag_ac t
		  ON t.user_id = s.user_id
		 AND t.score_version = s.score_version
		 AND t.model_version = s.model_version
		 AND t.count > 0
		WHERE s.user_id = ?
		  AND s.score_version = ?
		  AND s.evidence_dataset_revision = ?
		  AND s.evidence_user_revision = ?
		  AND `+modelPredicate,
		userID, CurrentUserTagAbilityScoreVersion,
		expected.Evidence.DatasetRevision, expected.Evidence.UserRevision,
		expected.ModelVersion,
	).Scan(&records).Error
	if err != nil {
		return UserTagAbilitySnapshot{}, err
	}
	if len(records) == 0 {
		return UserTagAbilitySnapshot{}, nil
	}
	snapshot := UserTagAbilitySnapshot{
		Ready: true, ModelVersion: records[0].ModelVersion, Evidence: expected.Evidence,
	}
	for _, record := range records {
		if record.ModelVersion != snapshot.ModelVersion || record.RowCount != records[0].RowCount {
			return UserTagAbilitySnapshot{}, ErrUserTagAbilitySnapshotCorrupt
		}
		if !record.Tag.Valid {
			continue
		}
		if !record.Count.Valid || !record.Weight.Valid {
			return UserTagAbilitySnapshot{}, ErrUserTagAbilitySnapshotCorrupt
		}
		snapshot.Rows = append(snapshot.Rows, UserTagAbility{Tag: record.Tag.String, Count: record.Count.Int64, Weight: record.Weight.Float64})
	}
	if int64(len(snapshot.Rows)) != records[0].RowCount {
		return UserTagAbilitySnapshot{}, ErrUserTagAbilitySnapshotCorrupt
	}
	sortUserTagAbilities(snapshot.Rows)
	if len(snapshot.Rows) > limit {
		snapshot.Rows = snapshot.Rows[:limit]
	}
	return snapshot, nil
}

func sortUserTagAbilities(rows []UserTagAbility) {
	sort.Slice(rows, func(i, j int) bool {
		si := TagAbilityScore(rows[i].Weight, int(rows[i].Count))
		sj := TagAbilityScore(rows[j].Weight, int(rows[j].Count))
		if si != sj {
			return si > sj
		}
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Tag < rows[j].Tag
	})
}

func CountUserTagAC(ctx context.Context, db *gorm.DB, userID int64) (int64, error) {
	if db == nil || userID <= 0 {
		return 0, nil
	}
	identity, err := ReadProfileCacheIdentity(ctx, db, userID)
	if err != nil {
		return 0, err
	}
	snapshot, err := ListUserTagAbilitySnapshot(ctx, db, userID, identity, false, int(^uint(0)>>1))
	return int64(len(snapshot.Rows)), err
}

// UserHasTaggedAC is source-data based; versioned cache rows never suppress a
// heal. It follows the same canonical p:/e: mapping as rebuild without a
// PostgreSQL-only string parser.
func UserHasTaggedAC(ctx context.Context, db *gorm.DB, userID int64) (bool, error) {
	if db == nil || userID <= 0 {
		return false, nil
	}
	var acRows []model.UserACProblem
	if err := db.WithContext(ctx).Where("user_id = ?", userID).Find(&acRows).Error; err != nil {
		return false, err
	}
	pIDs, extKeys := collectAbilityCandidateKeys(acRows)
	problems, err := loadAbilityProblems(ctx, db, pIDs, extKeys)
	if err != nil {
		return false, err
	}
	candidates, _ := resolveAbilityCandidates(acRows, problems, extKeys)
	if len(candidates) == 0 {
		return false, nil
	}
	ids := make([]uint, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	tagsByProblem, err := loadAbilityTags(ctx, db, ids)
	if err != nil {
		return false, err
	}
	return len(tagsByProblem) > 0, nil
}

func ListUserIDsWithACButEmptyTagAC(ctx context.Context, db *gorm.DB, limit int) ([]int64, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	type globalIdentity struct {
		ModelVersion    uint64 `gorm:"column:model_version"`
		DatasetRevision uint64 `gorm:"column:dataset_revision"`
		SchemaVersion   uint   `gorm:"column:schema_version"`
		Ready           bool   `gorm:"column:ready"`
	}
	var global globalIdentity
	result := db.WithContext(ctx).Raw(`SELECT a.active_version AS model_version,
		d.revision AS dataset_revision, d.schema_version, d.ready
		FROM ability_model_state a
		JOIN profile_evidence_dataset_state d ON d.id = ?
		WHERE a.id = ?`, 1, 1).Scan(&global)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 || global.ModelVersion == 0 || !global.Ready || global.SchemaVersion != CurrentProfileEvidenceSchemaVersion {
		return nil, errors.New("user tag ability heal identity is not ready")
	}
	// Probe current zero-row publications first with an independent, bounded
	// budget. Missing snapshots must not consume the whole maintenance batch,
	// while legitimate empty publications must never cause an unbounded scan or
	// more than one normal lookup batch of authoritative per-user validation.
	emptyProbeLimit := limit
	if emptyProbeLimit > abilityLookupBatchSize {
		emptyProbeLimit = abilityLookupBatchSize
	}
	invalidBudget := (emptyProbeLimit + 1) / 2
	loadEmptyWindow := func(afterUserID int64) ([]int64, error) {
		var window []int64
		err := db.WithContext(ctx).Raw(`
			SELECT DISTINCT u.user_id
			FROM user_ac_problems u
			JOIN user_tag_ac_snapshots s ON s.user_id = u.user_id
			LEFT JOIN user_profile_evidence_versions e ON e.user_id = u.user_id
			WHERE u.user_id > ?
				AND s.score_version = ? AND s.model_version = ?
				AND s.evidence_dataset_revision = ?
				AND s.evidence_user_revision = COALESCE(e.revision, 0)
				AND s.row_count = 0
			ORDER BY u.user_id
			LIMIT ?
		`, afterUserID, CurrentUserTagAbilityScoreVersion, global.ModelVersion, global.DatasetRevision, emptyProbeLimit).Scan(&window).Error
		return window, err
	}
	afterUserID := userTagEmptyHealCursor.Load()
	emptyIDs, err := loadEmptyWindow(afterUserID)
	if err != nil {
		return nil, err
	}
	if len(emptyIDs) == 0 && afterUserID != 0 {
		afterUserID = 0
		emptyIDs, err = loadEmptyWindow(afterUserID)
		if err != nil {
			return nil, err
		}
	}
	ids := make([]int64, 0, limit)
	for _, userID := range emptyIDs {
		hasTaggedAC, err := UserHasTaggedAC(ctx, db, userID)
		if err != nil {
			return nil, err
		}
		userTagEmptyHealCursor.Store(userID)
		if hasTaggedAC {
			ids = append(ids, userID)
			if len(ids) == invalidBudget {
				break
			}
		}
	}
	if len(emptyIDs) == 0 {
		userTagEmptyHealCursor.Store(0)
	}

	missingBudget := limit - len(ids)
	if missingBudget == 0 {
		return ids, nil
	}
	var missingIDs []int64
	err = db.WithContext(ctx).Raw(`
		SELECT DISTINCT u.user_id
		FROM user_ac_problems u
		LEFT JOIN user_profile_evidence_versions e ON e.user_id = u.user_id
		WHERE NOT EXISTS (
			SELECT 1 FROM user_tag_ac_snapshots s
			WHERE s.user_id = u.user_id
				AND s.score_version = ? AND s.model_version = ?
				AND s.evidence_dataset_revision = ?
				AND s.evidence_user_revision = COALESCE(e.revision, 0)
		)
		ORDER BY u.user_id
		LIMIT ?
	`, CurrentUserTagAbilityScoreVersion, global.ModelVersion, global.DatasetRevision, missingBudget).Scan(&missingIDs).Error
	if err != nil {
		return nil, err
	}
	ids = append(ids, missingIDs...)
	return ids, nil
}

type abilityCandidate struct {
	problem   model.Problem
	firstACAt time.Time
	platform  string
}

type abilityExternalKey struct {
	platform string
	external string
}

type abilitySubmit struct {
	id   uint
	isAC bool
	time time.Time
}

func strictProblemKeyID(key string) (uint, bool) {
	if !strings.HasPrefix(key, "p:") {
		return 0, false
	}
	v := strings.TrimPrefix(key, "p:")
	if v == "" {
		return 0, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 || uint64(uint(n)) != n {
		return 0, false
	}
	return uint(n), true
}

func parseAbilityExternalKey(key string) (abilityExternalKey, bool) {
	if !strings.HasPrefix(key, "e:") {
		return abilityExternalKey{}, false
	}
	rest := strings.TrimPrefix(key, "e:")
	i := strings.IndexByte(rest, ':')
	if i < 0 {
		return abilityExternalKey{}, false
	}
	result := abilityExternalKey{platform: normalizeAbilityPlatform(rest[:i]), external: strings.TrimSpace(rest[i+1:])}
	if result.platform == "" || result.external == "" || (result.platform == "leetcode" && strings.HasPrefix(strings.ToLower(result.external), "ac-")) {
		return abilityExternalKey{}, false
	}
	return result, true
}

func problemAbilityExternalKey(p model.Problem) (abilityExternalKey, bool) {
	key := abilityExternalKey{platform: normalizeAbilityPlatform(p.Platform), external: strings.TrimSpace(p.ExternalID)}
	return key, key.platform != "" && key.external != ""
}

func mergeAbilityCandidate(candidates map[uint]abilityCandidate, p model.Problem, at time.Time) {
	current, ok := candidates[p.ID]
	if !ok {
		if !abilityUsableTime(at) {
			at = time.Time{}
		}
		candidates[p.ID] = abilityCandidate{problem: p, firstACAt: at, platform: normalizeAbilityPlatform(p.Platform)}
		return
	}
	if abilityUsableTime(at) && (!abilityUsableTime(current.firstACAt) || at.Before(current.firstACAt)) {
		candidates[p.ID] = abilityCandidate{problem: p, firstACAt: at, platform: normalizeAbilityPlatform(p.Platform)}
	}
}

func abilityUsableTime(at time.Time) bool {
	return !at.IsZero() && at.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
}

func normalAbilityTerminal(l model.SubmitLog) bool {
	platform := normalizeAbilityPlatform(l.Platform)
	submitID := strings.TrimSpace(l.SubmitID)
	if submitID == "" || strings.TrimSpace(l.Status) == "" || model.IsPendingSubmitStatus(l.Status) {
		return false
	}
	lowerID := strings.ToLower(submitID)
	if (platform == "leetcode" && (strings.HasPrefix(lowerID, "lc-ac-") || strings.HasPrefix(lowerID, "lc-prob-"))) ||
		(platform == "uoj" && strings.HasPrefix(lowerID, "uoj-ac-")) {
		return false
	}
	return true
}

func RebuildUserTagACForUser(ctx context.Context, db *gorm.DB, userID int64) error {
	if db == nil || userID <= 0 {
		return nil
	}
	if !db.Migrator().HasTable(&model.UserTagAC{}) || !db.Migrator().HasTable(&model.ProblemTag{}) {
		return nil
	}
	identity, err := EnsureProfileCacheIdentityForBuild(ctx, db, userID)
	if err != nil {
		return err
	}
	return RebuildUserTagACForUserAtIdentity(ctx, db, userID, identity)
}

// RebuildUserTagACForUserAtIdentity computes outside the publication
// transaction, then atomically replaces rows only if the complete model and
// evidence identity is still current.
func RebuildUserTagACForUserAtIdentity(ctx context.Context, db *gorm.DB, userID int64, identity ProfileCacheIdentity) error {
	if db == nil || userID <= 0 || identity.ModelVersion == 0 {
		return errors.New("user tag ability: invalid rebuild identity")
	}
	modelVersion := identity.ModelVersion

	var acRows []model.UserACProblem
	if err := db.WithContext(ctx).Where("user_id = ?", userID).Find(&acRows).Error; err != nil {
		return err
	}
	pIDs, extKeys := collectAbilityCandidateKeys(acRows)
	problemByID, err := loadAbilityProblems(ctx, db, pIDs, nil)
	if err != nil {
		return err
	}
	lookupKeys := append([]abilityExternalKey(nil), extKeys...)
	for _, p := range problemByID {
		if key, ok := problemAbilityExternalKey(p); ok {
			lookupKeys = append(lookupKeys, key)
		}
	}
	if len(lookupKeys) > 0 {
		byExternal, err := loadAbilityProblems(ctx, db, nil, lookupKeys)
		if err != nil {
			return err
		}
		for id, p := range byExternal {
			problemByID[id] = p
		}
	}
	candidates, externalMatches := resolveAbilityCandidates(acRows, problemByID, extKeys)
	if len(candidates) == 0 {
		if len(acRows) > 0 && userHasPublishedTagRows(ctx, db, userID) {
			return ErrUserTagAbilityIncomplete
		}
		return replaceUserTagAbilityRows(ctx, db, userID, identity, nil)
	}

	ids := make([]uint, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	tagsByProblem, hardnessByProblem, err := loadAbilityFacts(ctx, db, ids, modelVersion)
	if err != nil {
		return err
	}
	missingFacts, taggedFacts := false, false
	for _, id := range ids {
		if len(tagsByProblem[id]) == 0 {
			missingFacts = true
		} else {
			taggedFacts = true
		}
	}
	// 题库绑定/AI 打标是异步的。只要本次结果不完整，就不能用部分
	// 结果覆盖已有聚合；等题面标签齐全后由绑定/每日任务重试。
	if missingFacts && (taggedFacts || userHasPublishedTagRows(ctx, db, userID)) {
		return ErrUserTagAbilityIncomplete
	}
	submitsByProblem, completePlatforms, backlogPlatforms, err := loadAbilityEvidence(ctx, db, userID, candidates, externalMatches)
	if err != nil {
		return err
	}

	aggregates := map[string]*model.UserTagAC{}
	for _, id := range ids {
		candidate := candidates[id]
		effort := personalAbilityEffort(submitsByProblem[id], candidate.platform, completePlatforms[candidate.platform], backlogPlatforms[candidate.platform])
		hardness := hardnessByProblem[id]
		if !finiteAbilityFloat(hardness) || hardness <= 0 {
			hardness = DifficultyAbilityProfile(candidate.problem.Difficulty).Quality
		}
		x := ProblemMasteryQuality(hardness, effort)
		seenTags := map[string]struct{}{}
		for _, tag := range tagsByProblem[id] {
			if _, seen := seenTags[tag]; seen {
				continue
			}
			seenTags[tag] = struct{}{}
			row := aggregates[tag]
			if row == nil {
				row = &model.UserTagAC{UserID: userID, Tag: tag, ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: modelVersion}
				aggregates[tag] = row
			}
			row.Count++
			row.Weight += x
		}
	}
	rows := make([]model.UserTagAC, 0, len(aggregates))
	for _, row := range aggregates {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Tag < rows[j].Tag })
	return replaceUserTagAbilityRows(ctx, db, userID, identity, rows)
}

func collectAbilityCandidateKeys(rows []model.UserACProblem) ([]uint, []abilityExternalKey) {
	seenP := map[uint]struct{}{}
	seenE := map[abilityExternalKey]struct{}{}
	var pIDs []uint
	var keys []abilityExternalKey
	for _, row := range rows {
		if id, ok := strictProblemKeyID(row.ProblemKey); ok {
			if _, exists := seenP[id]; !exists {
				seenP[id] = struct{}{}
				pIDs = append(pIDs, id)
			}
			continue
		}
		if key, ok := parseAbilityExternalKey(row.ProblemKey); ok {
			if _, exists := seenE[key]; !exists {
				seenE[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	return pIDs, keys
}

// abilityExternalKeySQL returns a dialect-compatible predicate that parses an
// e:<platform>:<external-id> key with the same case/whitespace semantics as
// parseAbilityExternalKey. Its literal prefix/delimiter conditions exactly
// imply the partial external-identity index. The caller supplies the normalized
// row platform, embedded platform, then external ID.
func abilityExternalKeySQL(db *gorm.DB) string {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return "LOWER(BTRIM(platform)) = ? AND LEFT(problem_key, 2) = 'e:' AND " +
			"POSITION(':' IN SUBSTRING(problem_key FROM 3)) > 0 AND " +
			postgresAbilityExternalEmbeddedPlatformSQL + " <> '' AND " + postgresAbilityExternalIDSQL + " <> '' AND " +
			"NOT (LOWER(" + postgresAbilityExternalEmbeddedPlatformSQL + ") = 'leetcode' AND LEFT(LOWER(" + postgresAbilityExternalIDSQL + "), 3) = 'ac-') AND " +
			"LOWER(" + postgresAbilityExternalEmbeddedPlatformSQL + ") = ? AND " + postgresAbilityExternalIDSQL + " = ?"
	}
	return "LOWER(TRIM(platform)) = ? AND SUBSTR(problem_key, 1, 2) = 'e:' AND " +
		"INSTR(SUBSTR(problem_key, 3), ':') > 0 AND " +
		portableAbilityExternalEmbeddedPlatformSQL + " <> '' AND " + portableAbilityExternalIDSQL + " <> '' AND " +
		"NOT (LOWER(" + portableAbilityExternalEmbeddedPlatformSQL + ") = 'leetcode' AND SUBSTR(LOWER(" + portableAbilityExternalIDSQL + "), 1, 3) = 'ac-') AND " +
		"LOWER(" + portableAbilityExternalEmbeddedPlatformSQL + ") = ? AND " + portableAbilityExternalIDSQL + " = ?"
}

const (
	postgresAbilityExternalEmbeddedPlatformSQL = "BTRIM(SUBSTRING(problem_key FROM 3 FOR GREATEST(POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1, 0)))"
	postgresAbilityExternalIDSQL               = "BTRIM(SUBSTRING(problem_key FROM 3 + POSITION(':' IN SUBSTRING(problem_key FROM 3))))"
	portableAbilityExternalEmbeddedPlatformSQL = "TRIM(SUBSTR(problem_key, 3, INSTR(SUBSTR(problem_key, 3), ':') - 1))"
	portableAbilityExternalIDSQL               = "TRIM(SUBSTR(problem_key, 3 + INSTR(SUBSTR(problem_key, 3), ':')))"
)

func uniqueAbilityExternalKeys(keys []abilityExternalKey) []abilityExternalKey {
	seen := make(map[abilityExternalKey]struct{}, len(keys))
	out := make([]abilityExternalKey, 0, len(keys))
	for _, key := range keys {
		if key.platform == "" || key.external == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

func abilityExternalPairsWhere(keys []abilityExternalKey) (string, []interface{}) {
	parts := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*2)
	for _, key := range keys {
		parts = append(parts, "(LOWER(TRIM(platform)) = ? AND TRIM(external_id) = ?)")
		args = append(args, key.platform, key.external)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func loadAbilityProblems(ctx context.Context, db *gorm.DB, pIDs []uint, extKeys []abilityExternalKey) (map[uint]model.Problem, error) {
	out := map[uint]model.Problem{}
	if len(pIDs) > 0 {
		for start := 0; start < len(pIDs); start += abilityLookupBatchSize {
			end := start + abilityLookupBatchSize
			if end > len(pIDs) {
				end = len(pIDs)
			}
			var rows []model.Problem
			if err := db.WithContext(ctx).Where("id IN ?", pIDs[start:end]).Find(&rows).Error; err != nil {
				return nil, err
			}
			for _, p := range rows {
				out[p.ID] = p
			}
		}
	}
	for start, keys := 0, uniqueAbilityExternalKeys(extKeys); start < len(keys); start += abilityLookupBatchSize {
		end := start + abilityLookupBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		where, args := abilityExternalPairsWhere(keys[start:end])
		var rows []model.Problem
		if err := db.WithContext(ctx).Where(where, args...).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, p := range rows {
			out[p.ID] = p
		}
	}
	return out, nil
}

func resolveAbilityCandidates(acRows []model.UserACProblem, problems map[uint]model.Problem, extKeys []abilityExternalKey) (map[uint]abilityCandidate, map[abilityExternalKey][]model.Problem) {
	candidates := map[uint]abilityCandidate{}
	for _, row := range acRows {
		if id, ok := strictProblemKeyID(row.ProblemKey); ok {
			if p, exists := problems[id]; exists {
				mergeAbilityCandidate(candidates, p, row.FirstACAt)
			}
		}
	}
	matches := map[abilityExternalKey][]model.Problem{}
	for _, p := range problems {
		if key, ok := problemAbilityExternalKey(p); ok {
			matches[key] = append(matches[key], p)
		}
	}
	for _, row := range acRows {
		key, ok := parseAbilityExternalKey(row.ProblemKey)
		if ok && len(matches[key]) == 1 {
			mergeAbilityCandidate(candidates, matches[key][0], row.FirstACAt)
		}
	}
	candidateMatches := map[abilityExternalKey][]model.Problem{}
	for _, candidate := range candidates {
		if key, ok := problemAbilityExternalKey(candidate.problem); ok {
			candidateMatches[key] = matches[key]
		}
	}
	return candidates, candidateMatches
}

func loadAbilityTags(ctx context.Context, db *gorm.DB, ids []uint) (map[uint][]string, error) {
	tagsByProblem := map[uint][]string{}
	for start := 0; start < len(ids); start += abilityLookupBatchSize {
		end := start + abilityLookupBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		var tags []model.ProblemTag
		if err := db.WithContext(ctx).Where("problem_id IN ?", ids[start:end]).Find(&tags).Error; err != nil {
			return nil, err
		}
		for _, row := range tags {
			if tag := strings.TrimSpace(row.Tag); tag != "" {
				tagsByProblem[row.ProblemID] = append(tagsByProblem[row.ProblemID], tag)
			}
		}
	}
	return tagsByProblem, nil
}

func loadAbilityFacts(ctx context.Context, db *gorm.DB, ids []uint, modelVersion uint64) (map[uint][]string, map[uint]float64, error) {
	tagsByProblem, err := loadAbilityTags(ctx, db, ids)
	if err != nil {
		return nil, nil, err
	}
	hardness := map[uint]float64{}
	for start := 0; start < len(ids); start += abilityLookupBatchSize {
		end := start + abilityLookupBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		var stats []model.ProblemAbilityStat
		if err := db.WithContext(ctx).Where("model_version = ? AND problem_id IN ?", modelVersion, ids[start:end]).Find(&stats).Error; err != nil {
			return nil, nil, err
		}
		for _, stat := range stats {
			hardness[stat.ProblemID] = stat.Hardness
		}
	}
	return tagsByProblem, hardness, nil
}

func userHasPublishedTagRows(ctx context.Context, db *gorm.DB, userID int64) bool {
	if db == nil || userID <= 0 {
		return false
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Where("user_id = ? AND count > 0", userID).Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

func loadAbilityEvidence(ctx context.Context, db *gorm.DB, userID int64, candidates map[uint]abilityCandidate, externalMatches map[abilityExternalKey][]model.Problem) (map[uint][]abilitySubmit, map[string]bool, map[string]bool, error) {
	logs := make([]model.SubmitLog, 0)
	backlog := map[string]bool{}
	ids := make([]uint, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += abilityLookupBatchSize {
		end := start + abilityLookupBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		var rows []model.SubmitLog
		if err := db.WithContext(ctx).Where("user_id = ? AND problem_id IN ?", userID, ids[start:end]).Find(&rows).Error; err != nil {
			return nil, nil, nil, err
		}
		logs = append(logs, rows...)
	}

	uniqueExternal := make([]abilityExternalKey, 0, len(externalMatches))
	hasLuoguCandidate := false
	for _, candidate := range candidates {
		if candidate.platform == "luogu" {
			hasLuoguCandidate = true
			break
		}
	}
	for key, matches := range externalMatches {
		if len(matches) == 1 {
			uniqueExternal = append(uniqueExternal, key)
		}
	}
	for start, keys := 0, uniqueAbilityExternalKeys(uniqueExternal); start < len(keys); start += abilityLookupBatchSize {
		end := start + abilityLookupBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		where, args := abilityExternalPairsWhere(keys[start:end])
		args = append([]interface{}{userID}, args...)
		var rows []model.SubmitLog
		if err := db.WithContext(ctx).
			Where("user_id = ? AND (problem_id IS NULL OR problem_id = 0) AND "+where, args...).
			Find(&rows).Error; err != nil {
			return nil, nil, nil, err
		}
		logs = append(logs, rows...)
	}
	if hasLuoguCandidate {
		var luoguBacklog int
		if err := db.WithContext(ctx).Raw(`
			SELECT 1
			FROM submit_logs s
			LEFT JOIN problems p ON p.id = s.problem_id
			WHERE s.user_id = ?
				AND LOWER(TRIM(s.platform)) = 'luogu'
				AND TRIM(COALESCE(s.submit_id, '')) <> ''
				AND TRIM(COALESCE(s.status, '')) <> ''
				AND TRIM(s.status) NOT IN ('正在评测', '评测中', '等待评测', '排队中')
				AND UPPER(TRIM(s.status)) NOT IN ('TESTING', 'PENDING', 'JUDGING', 'IN_QUEUE', 'IN QUEUE', 'WAITING', 'WJ', 'QUEUE')
				AND (s.problem_id IS NULL OR s.problem_id = 0 OR p.id IS NULL
					OR LOWER(TRIM(p.platform)) <> LOWER(TRIM(s.platform)))
				AND NOT EXISTS (
					SELECT 1
					FROM submit_logs canonical
					JOIN problems canonical_problem ON canonical_problem.id = canonical.problem_id
					WHERE canonical.user_id = s.user_id
						AND LOWER(TRIM(canonical.platform)) = LOWER(TRIM(s.platform))
						AND TRIM(canonical.submit_id) = TRIM(s.submit_id)
						AND canonical.problem_id IS NOT NULL AND canonical.problem_id <> 0
						AND LOWER(TRIM(canonical_problem.platform)) = LOWER(TRIM(canonical.platform))
						AND TRIM(COALESCE(canonical.submit_id, '')) <> ''
						AND TRIM(COALESCE(canonical.status, '')) <> ''
						AND TRIM(canonical.status) NOT IN ('正在评测', '评测中', '等待评测', '排队中')
						AND UPPER(TRIM(canonical.status)) NOT IN ('TESTING', 'PENDING', 'JUDGING', 'IN_QUEUE', 'IN QUEUE', 'WAITING', 'WJ', 'QUEUE')
				)
			LIMIT 1
		`, userID).Scan(&luoguBacklog).Error; err != nil {
			return nil, nil, nil, err
		}
		if luoguBacklog == 1 {
			// Keep the existing one-sided incomplete-history penalty without
			// materializing every unrelated unbound Luogu submission.
			backlog["luogu"] = true
		}
	}
	var platforms []model.Platform
	if err := db.WithContext(ctx).Where("user_id = ?", userID).Find(&platforms).Error; err != nil {
		return nil, nil, nil, err
	}
	completed := map[string]bool{}
	for _, p := range platforms {
		if p.ClientSyncCompletedAt != nil && normalizeAbilityPlatform(p.Platform) == "luogu" {
			completed["luogu"] = true
		}
	}
	dedup := map[string]model.SubmitLog{}
	for _, log := range logs {
		if !normalAbilityTerminal(log) {
			continue
		}
		key := normalizeAbilityPlatform(log.Platform) + "\\x00" + strings.TrimSpace(log.SubmitID)
		if old, exists := dedup[key]; !exists || abilitySubmitBefore(log, old) {
			dedup[key] = log
		}
	}
	byProblem := map[uint][]abilitySubmit{}
	for _, log := range dedup {
		platform := normalizeAbilityPlatform(log.Platform)
		if log.ProblemID == nil || *log.ProblemID == 0 {
			backlog[platform] = true
		}
		var id uint
		if log.ProblemID != nil && *log.ProblemID != 0 {
			id = *log.ProblemID
		} else if matches := externalMatches[abilityExternalKey{platform: platform, external: strings.TrimSpace(log.ExternalID)}]; len(matches) == 1 {
			id = matches[0].ID
		}
		candidate, ok := candidates[id]
		if !ok || candidate.platform != platform {
			continue
		}
		byProblem[id] = append(byProblem[id], abilitySubmit{id: log.ID, isAC: model.IsAcceptedStatus(log.Status), time: log.Time})
	}
	return byProblem, completed, backlog, nil
}

func abilitySubmitBefore(a, b model.SubmitLog) bool {
	if a.Time.Equal(b.Time) {
		return a.ID < b.ID
	}
	return a.Time.Before(b.Time)
}

func personalAbilityEffort(submits []abilitySubmit, platform string, completeAnchor, backlog bool) float64 {
	if len(submits) == 0 {
		return 0.78
	}
	sort.Slice(submits, func(i, j int) bool {
		if submits[i].time.Equal(submits[j].time) {
			return submits[i].id < submits[j].id
		}
		return submits[i].time.Before(submits[j].time)
	})
	firstAC := -1
	for i, submit := range submits {
		if submit.isAC {
			firstAC = i
			break
		}
	}
	if firstAC < 0 {
		return 0.78
	}
	sequence := submits[:firstAC+1]
	attempts := len(sequence)
	if attempts > 20 {
		attempts = 20
	}
	hasFailure, timingValid := false, true
	for _, submit := range sequence {
		if !abilityUsableTime(submit.time) {
			timingValid = false
		}
		if !submit.isAC {
			hasFailure = true
		}
	}
	minutes := 0.0
	if timingValid {
		minutes = sequence[len(sequence)-1].time.Sub(sequence[0].time).Minutes()
		if minutes < 0 {
			timingValid, minutes = false, 0
		}
	}
	if platform == "luogu" && completeAnchor && !backlog && timingValid {
		return SolveEffort(attempts, minutes, true)
	}
	if !hasFailure {
		return 0.78
	}
	return SolveEffort(attempts, minutes, false)
}

func replaceUserTagAbilityRows(ctx context.Context, db *gorm.DB, userID int64, expected ProfileCacheIdentity, rows []model.UserTagAC) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := lockProfileCacheIdentity(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := compareUserTagAbilityIdentity(current, expected); err != nil {
			return err
		}
		if len(rows) > 0 {
			for i := range rows {
				rows[i].UserID = userID
				rows[i].ScoreVersion = CurrentUserTagAbilityScoreVersion
				rows[i].ModelVersion = expected.ModelVersion
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "user_id"}, {Name: "tag"}},
				DoUpdates: clause.AssignmentColumns([]string{"count", "weight", "score_version", "model_version"}),
			}).CreateInBatches(&rows, 200).Error; err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			if err := tx.Where("user_id = ?", userID).Delete(&model.UserTagAC{}).Error; err != nil {
				return err
			}
		} else {
			tags := make([]string, 0, len(rows))
			for _, row := range rows {
				tags = append(tags, row.Tag)
			}
			if err := tx.Where("user_id = ? AND tag NOT IN ?", userID, tags).Delete(&model.UserTagAC{}).Error; err != nil {
				return err
			}
		}
		header := model.UserTagACSnapshot{
			UserID: userID, ScoreVersion: CurrentUserTagAbilityScoreVersion, ModelVersion: expected.ModelVersion,
			EvidenceDatasetRevision: expected.Evidence.DatasetRevision,
			EvidenceUserRevision:    expected.Evidence.UserRevision,
			RowCount:                int64(len(rows)),
			PublishedAt:             time.Now(),
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"score_version", "model_version", "evidence_dataset_revision",
				"evidence_user_revision", "row_count", "published_at",
			}),
		}).Create(&header).Error; err != nil {
			return err
		}
		current, err = lockProfileCacheIdentity(ctx, tx, userID)
		if err != nil {
			return err
		}
		return compareUserTagAbilityIdentity(current, expected)
	})
}

func compareUserTagAbilityIdentity(current, expected ProfileCacheIdentity) error {
	var errs []error
	if current.ModelVersion != expected.ModelVersion {
		errs = append(errs, ErrUserTagAbilityModelChanged)
	}
	if current.Evidence != expected.Evidence {
		errs = append(errs, ErrUserTagAbilityEvidenceChanged)
	}
	return errors.Join(errs...)
}

// lockActiveUserTagAbilityModelVersion locks the singleton state row used by
// the global publisher. PostgreSQL keeps this FOR SHARE lock to transaction
// commit: concurrent rebuilds remain compatible, while the publisher's FOR
// UPDATE must happen before this rebuild or wait until it is atomically done.
func lockActiveUserTagAbilityModelVersion(ctx context.Context, db *gorm.DB) (uint64, bool, error) {
	var state model.AbilityModelState
	err := db.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).First(&state, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return state.ActiveVersion, state.ActiveVersion > 0, nil
}
