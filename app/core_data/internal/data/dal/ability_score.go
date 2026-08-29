package dal

import (
	"math"
	"strings"
)

const (
	abilityNeutralEffort = 0.78
	abilityMinQuality    = 0.15
	abilityMaxQuality    = 0.98
)

// DifficultyProfile contains the normalized difficulty priors used by the
// ability score. Prior is the static AC prior and Quality is the difficulty
// multiplier before the sample posterior adjustment.
type DifficultyProfile struct {
	Key     string
	Prior   float64
	Quality float64
}

// DifficultyAbilityProfile normalizes the difficulty values used by the
// different OJ adapters. Unknown and empty values use the unknown prior.
func DifficultyAbilityProfile(difficulty string) DifficultyProfile {
	switch strings.ToLower(strings.TrimSpace(difficulty)) {
	case "easy", "简单", "简单题", "入门":
		return DifficultyProfile{Key: "easy", Prior: 0.65, Quality: 0.8}
	case "medium", "中等", "中等题", "mid", "中级":
		return DifficultyProfile{Key: "medium", Prior: 0.35, Quality: 1}
	case "hard", "困难", "困难题", "高级":
		return DifficultyProfile{Key: "hard", Prior: 0.2, Quality: 1.3}
	default:
		return DifficultyProfile{Key: "unknown", Prior: 0.45, Quality: 0.9}
	}
}

func finiteAbilityFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func clampAbility(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abilityLogit(p float64) float64 {
	p = clampAbility(p, 0.02, 0.98)
	return math.Log(p / (1 - p))
}

// ProblemHardness calculates a problem's quality multiplier from its
// difficulty group and the problem's observed AC sample. Impossible counts
// are rejected; with no reliable sample the static difficulty value is
// returned directly.
func ProblemHardness(difficulty string, groupAC, groupAttempts, problemAC, problemAttempts float64) float64 {
	profile := DifficultyAbilityProfile(difficulty)
	if !finiteAbilityFloat(groupAC) || !finiteAbilityFloat(groupAttempts) ||
		!finiteAbilityFloat(problemAC) || !finiteAbilityFloat(problemAttempts) {
		return profile.Quality
	}
	if groupAttempts <= 0 || problemAttempts <= 0 || groupAC < 0 || problemAC < 0 ||
		groupAC > groupAttempts || problemAC > problemAttempts {
		return profile.Quality
	}

	muGroup := (groupAC + 200*profile.Prior) / (groupAttempts + 200)
	qProblem := (problemAC + 30*muGroup) / (problemAttempts + 30)
	hardness := profile.Quality * math.Exp(0.35*(abilityLogit(muGroup)-abilityLogit(qProblem)))
	if !finiteAbilityFloat(hardness) {
		return profile.Quality
	}
	return clampAbility(hardness, 0.65, 1.75)
}

// SolveEffort returns the process evidence for a first AC. minutes is the
// elapsed time from the first submission to that AC. If completeHistory is
// false, the observed history is trusted at a fixed 0.6 confidence. A
// non-complete history with no observed failure is neutral rather than being
// treated as an immediate successful solve.
func SolveEffort(attempts int, minutes float64, completeHistory bool) float64 {
	if attempts <= 0 || !finiteAbilityFloat(minutes) || minutes < 0 {
		return abilityNeutralEffort
	}
	if attempts > 20 {
		attempts = 20
	}
	minutes = math.Min(minutes, 365*24*60)

	fAttempt := math.Max(0.4, math.Pow(float64(attempts), -0.28))
	fTime := math.Max(0.6, math.Pow(1+minutes/30, -0.10))
	eObserved := math.Pow(fAttempt, 0.7) * math.Pow(fTime, 0.3)
	if completeHistory {
		return clampAbility(eObserved, 0, 1)
	}
	if attempts <= 1 {
		return abilityNeutralEffort
	}
	// Missing history must not turn a first observed AC into a reward. Only
	// the portion at or below neutral is trusted when coverage is incomplete.
	eObserved = math.Min(abilityNeutralEffort, eObserved)
	const incompleteHistoryConfidence = 0.6
	e := math.Pow(abilityNeutralEffort, 1-incompleteHistoryConfidence) *
		math.Pow(eObserved, incompleteHistoryConfidence)
	if !finiteAbilityFloat(e) {
		return abilityNeutralEffort
	}
	return clampAbility(e, 0, 1)
}

// ProblemMasteryQuality combines the problem quality and process evidence.
func ProblemMasteryQuality(hardness, effort float64) float64 {
	if !finiteAbilityFloat(hardness) || hardness <= 0 {
		hardness = 1
	}
	if !finiteAbilityFloat(effort) || effort <= 0 {
		effort = abilityNeutralEffort
	}
	hardness = clampAbility(hardness, 0.65, 1.75)
	effort = clampAbility(effort, 0, 1)
	quality := 0.8 * math.Pow(hardness, 0.4) * math.Pow(effort, 0.6)
	if !finiteAbilityFloat(quality) {
		quality = 0.8 * math.Pow(abilityNeutralEffort, 0.6)
	}
	return clampAbility(quality, abilityMinQuality, abilityMaxQuality)
}

// TagAbilityScore aggregates a tag's quality sum and de-duplicated AC count.
// Four virtual medium-quality observations provide the low-sample prior, while
// sqrt(n/(n+60)) is the evidence confidence. The slower saturation keeps mature
// tags distinguishable without letting tiny count differences dominate quality.
func TagAbilityScore(qualitySum float64, acCount int) float64 {
	if acCount <= 0 {
		return 0
	}
	c := float64(acCount)
	minQualitySum := abilityMinQuality * c
	maxQualitySum := abilityMaxQuality * c
	// Repeatedly adding a clamped boundary value can drift a few ULPs beyond
	// the mathematical bound. Treat that representation noise as valid, while
	// still rejecting materially impossible/corrupt aggregates.
	boundTolerance := 1e-9 * math.Max(1, c)
	if !finiteAbilityFloat(qualitySum) || qualitySum < minQualitySum-boundTolerance || qualitySum > maxQualitySum+boundTolerance {
		qualitySum = 0.55 * c
	} else {
		qualitySum = clampAbility(qualitySum, minQualitySum, maxQualitySum)
	}
	q := (4*0.55 + qualitySum) / (4 + c)
	score := 100 * q * math.Sqrt(c/(c+60))
	if !finiteAbilityFloat(score) {
		return 0
	}
	return clampAbility(math.Round(score*1000)/1000, 0, 100)
}
