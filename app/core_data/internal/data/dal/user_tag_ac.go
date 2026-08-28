package dal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const CurrentUserTagAbilityScoreVersion uint = 1

var ErrUserTagAbilityModelChanged = errors.New("user tag ability model changed; retry rebuild")

type UserTagAbility struct {
	Tag    string
	Count  int64
	Weight float64
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
	var statusIDs []int64
	if err := db.WithContext(ctx).Model(&model.UserProblemStatus{}).
		Where("problem_id = ? AND status = ?", problemID, model.UserProblemStatusAC).
		Pluck("user_id", &statusIDs).Error; err != nil {
		return nil, err
	}
	add(statusIDs)
	var problem model.Problem
	if err := db.WithContext(ctx).First(&problem, problemID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, nil
		}
		return nil, err
	}
	var acRows []model.UserACProblem
	if err := db.WithContext(ctx).Where("problem_key = ? OR problem_key LIKE ?", fmt.Sprintf("p:%d", problemID), "e:%").Find(&acRows).Error; err != nil {
		return nil, err
	}
	external := abilityExternalKey{platform: normalizeAbilityPlatform(problem.Platform), external: strings.TrimSpace(problem.ExternalID)}
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
	if limit <= 0 {
		limit = 20
	}
	version, ready, err := activeUserTagAbilityModelVersion(ctx, db)
	if err != nil || !ready {
		return nil, err
	}
	var rows []UserTagAbility
	if err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Select("tag, count, weight").
		Where("user_id = ? AND count > 0 AND score_version = ? AND model_version = ?", userID, CurrentUserTagAbilityScoreVersion, version).
		Find(&rows).Error; err != nil {
		return nil, err
	}
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
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func CountUserTagAC(ctx context.Context, db *gorm.DB, userID int64) (int64, error) {
	if db == nil || userID <= 0 {
		return 0, nil
	}
	version, ready, err := activeUserTagAbilityModelVersion(ctx, db)
	if err != nil || !ready {
		return 0, err
	}
	var n int64
	err = db.WithContext(ctx).Model(&model.UserTagAC{}).
		Where("user_id = ? AND count > 0 AND score_version = ? AND model_version = ?", userID, CurrentUserTagAbilityScoreVersion, version).
		Count(&n).Error
	return n, err
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
	var n int64
	if err := db.WithContext(ctx).Model(&model.ProblemTag{}).Where("problem_id IN ?", ids).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

func ListUserIDsWithACButEmptyTagAC(ctx context.Context, db *gorm.DB, limit int) ([]int64, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	version, ready, err := activeUserTagAbilityModelVersion(ctx, db)
	if err != nil || !ready {
		return nil, err
	}
	var ids []int64
	err = db.WithContext(ctx).Raw(`
		SELECT DISTINCT u.user_id
		FROM user_ac_problems u
		WHERE NOT EXISTS (
			SELECT 1 FROM user_tag_ac t
			WHERE t.user_id = u.user_id AND t.count > 0
				AND t.score_version = ? AND t.model_version = ?
		)
		ORDER BY u.user_id
		LIMIT ?
	`, CurrentUserTagAbilityScoreVersion, version, limit).Scan(&ids).Error
	return ids, err
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
	modelVersion, ready, err := activeUserTagAbilityModelVersion(ctx, db)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("user tag ability: active model version is not published")
	}

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
		return replaceUserTagAbilityRows(ctx, db, userID, modelVersion, nil)
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
	return replaceUserTagAbilityRows(ctx, db, userID, modelVersion, rows)
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

func loadAbilityProblems(ctx context.Context, db *gorm.DB, pIDs []uint, extKeys []abilityExternalKey) (map[uint]model.Problem, error) {
	out := map[uint]model.Problem{}
	if len(pIDs) > 0 {
		var rows []model.Problem
		if err := db.WithContext(ctx).Where("id IN ?", pIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, p := range rows {
			out[p.ID] = p
		}
	}
	if len(extKeys) > 0 {
		seen := map[string]struct{}{}
		var externalIDs []string
		for _, key := range extKeys {
			if _, ok := seen[key.external]; !ok {
				seen[key.external] = struct{}{}
				externalIDs = append(externalIDs, key.external)
			}
		}
		var rows []model.Problem
		if err := db.WithContext(ctx).Where("TRIM(external_id) IN ?", externalIDs).Find(&rows).Error; err != nil {
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

func loadAbilityFacts(ctx context.Context, db *gorm.DB, ids []uint, modelVersion uint64) (map[uint][]string, map[uint]float64, error) {
	var tags []model.ProblemTag
	if err := db.WithContext(ctx).Where("problem_id IN ?", ids).Find(&tags).Error; err != nil {
		return nil, nil, err
	}
	tagsByProblem := map[uint][]string{}
	for _, row := range tags {
		if tag := strings.TrimSpace(row.Tag); tag != "" {
			tagsByProblem[row.ProblemID] = append(tagsByProblem[row.ProblemID], tag)
		}
	}
	var stats []model.ProblemAbilityStat
	if err := db.WithContext(ctx).Where("model_version = ? AND problem_id IN ?", modelVersion, ids).Find(&stats).Error; err != nil {
		return nil, nil, err
	}
	hardness := map[uint]float64{}
	for _, stat := range stats {
		hardness[stat.ProblemID] = stat.Hardness
	}
	return tagsByProblem, hardness, nil
}

func loadAbilityEvidence(ctx context.Context, db *gorm.DB, userID int64, candidates map[uint]abilityCandidate, externalMatches map[abilityExternalKey][]model.Problem) (map[uint][]abilitySubmit, map[string]bool, map[string]bool, error) {
	var logs []model.SubmitLog
	if err := db.WithContext(ctx).Where("user_id = ?", userID).Find(&logs).Error; err != nil {
		return nil, nil, nil, err
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
	backlog := map[string]bool{}
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
		byProblem[id] = append(byProblem[id], abilitySubmit{id: log.ID, isAC: log.IsAC || model.IsAcceptedStatus(log.Status), time: log.Time})
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

func replaceUserTagAbilityRows(ctx context.Context, db *gorm.DB, userID int64, modelVersion uint64, rows []model.UserTagAC) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, ready, err := lockActiveUserTagAbilityModelVersion(ctx, tx)
		if err != nil {
			return err
		}
		if !ready || current != modelVersion {
			return ErrUserTagAbilityModelChanged
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.UserTagAC{}).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(&rows, 200).Error; err != nil {
				return err
			}
		}
		current, ready, err = lockActiveUserTagAbilityModelVersion(ctx, tx)
		if err != nil {
			return err
		}
		if !ready || current != modelVersion {
			return ErrUserTagAbilityModelChanged
		}
		return nil
	})
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
