package dal

import (
	"math"
	"testing"
)

func TestAbilityDifficultyProfileNormalizesEnglishAndChinese(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantKey     string
		wantPrior   float64
		wantQuality float64
	}{
		{name: "easy english", input: "easy", wantKey: "easy", wantPrior: 0.65, wantQuality: 0.8},
		{name: "easy chinese", input: "简单", wantKey: "easy", wantPrior: 0.65, wantQuality: 0.8},
		{name: "medium english", input: " Medium ", wantKey: "medium", wantPrior: 0.35, wantQuality: 1},
		{name: "medium chinese", input: "中等", wantKey: "medium", wantPrior: 0.35, wantQuality: 1},
		{name: "hard english", input: "HARD", wantKey: "hard", wantPrior: 0.2, wantQuality: 1.3},
		{name: "hard chinese", input: "困难", wantKey: "hard", wantPrior: 0.2, wantQuality: 1.3},
		{name: "easy alias", input: "入门", wantKey: "easy", wantPrior: 0.65, wantQuality: 0.8},
		{name: "medium alias", input: "中级", wantKey: "medium", wantPrior: 0.35, wantQuality: 1},
		{name: "hard alias", input: "高级", wantKey: "hard", wantPrior: 0.2, wantQuality: 1.3},
		{name: "unknown", input: "", wantKey: "unknown", wantPrior: 0.45, wantQuality: 0.9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DifficultyAbilityProfile(tt.input)
			if got.Key != tt.wantKey || got.Prior != tt.wantPrior || got.Quality != tt.wantQuality {
				t.Fatalf("profile(%q) = %+v, want key=%q prior=%v quality=%v", tt.input, got, tt.wantKey, tt.wantPrior, tt.wantQuality)
			}
		})
	}
}

func TestProblemHardnessUsesDifficultyPriorWithoutSamples(t *testing.T) {
	if got := ProblemHardness("中等", 0, 0, 0, 0); got != 1 {
		t.Fatalf("no-sample medium hardness = %v, want 1", got)
	}
}

func TestProblemHardnessShrinksSmallProblemSampleToGroupPrior(t *testing.T) {
	got := ProblemHardness("medium", 35, 100, 3, 20)
	want := 1.1404856476
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("hardness = %.12f, want %.12f", got, want)
	}
}

func TestSolveEffortImmediateCompleteACIsOne(t *testing.T) {
	if got := SolveEffort(1, 0, true); got != 1 {
		t.Fatalf("immediate AC effort = %v, want 1", got)
	}
}

func TestSolveEffortPenalizesAttemptsAndTime(t *testing.T) {
	got := SolveEffort(4, 120, true)
	want := 0.7261513374
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("four-attempt two-hour effort = %.12f, want %.12f", got, want)
	}
	if SolveEffort(4, 120, true) >= SolveEffort(1, 0, true) {
		t.Fatal("more attempts and time should reduce complete-history effort")
	}
	if SolveEffort(4, 0, true) >= SolveEffort(1, 0, true) {
		t.Fatal("more attempts should reduce complete-history effort")
	}
	if SolveEffort(1, 120, true) >= SolveEffort(1, 0, true) {
		t.Fatal("more elapsed time should reduce complete-history effort")
	}
}

func TestSolveEffortDoesNotTreatACOnlyAsImmediateComplete(t *testing.T) {
	got := SolveEffort(1, 0, false)
	want := 0.78
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("AC-only effort = %.12f, want %.12f", got, want)
	}
}

func TestSolveEffortObservedFailureAndMissingProcess(t *testing.T) {
	if got := SolveEffort(0, 0, false); got != 0.78 {
		t.Fatalf("missing process effort = %v, want 0.78", got)
	}
	if got := SolveEffort(4, 120, false); got >= 0.78 {
		t.Fatalf("truncated history with failures should not exceed neutral effort: %v", got)
	}
	const wantIncomplete = 0.747229701085376
	if got := SolveEffort(4, 120, false); math.Abs(got-wantIncomplete) > 1e-9 {
		t.Fatalf("truncated history effort = %.15f, want %.15f (fixed rho=0.6)", got, wantIncomplete)
	}
}

func TestSolveEffortInvalidTimesFallBackToNeutral(t *testing.T) {
	for _, minutes := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if got := SolveEffort(1, minutes, true); got != 0.78 {
			t.Errorf("invalid minutes %v effort = %v, want 0.78", minutes, got)
		}
	}
}

func TestSolveEffortCompletenessIsBooleanAndControlsFallback(t *testing.T) {
	complete := SolveEffort(4, 120, true)
	truncated := SolveEffort(4, 120, false)
	if complete >= truncated {
		t.Fatalf("complete effort=%v should preserve observed penalty and be below truncated neutral blend=%v", complete, truncated)
	}
	if got := SolveEffort(1, 120, false); got != 0.78 {
		t.Fatalf("incomplete one-observation effort = %v, want 0.78", got)
	}
}

func TestProblemHardnessRejectsImpossibleOrNegativeCounts(t *testing.T) {
	for _, tc := range [][4]float64{
		{-1, 100, 3, 20},
		{35, -100, 3, 20},
		{35, 100, -1, 20},
		{35, 100, 21, 20},
		{101, 100, 3, 20},
		{math.NaN(), 100, 3, 20},
		{35, math.Inf(1), 3, 20},
		{35, 100, 3, math.NaN()},
		{35, 100, 3, math.Inf(1)},
	} {
		if got := ProblemHardness("medium", tc[0], tc[1], tc[2], tc[3]); got != 1 {
			t.Errorf("invalid counts %+v hardness = %v, want static medium quality 1", tc, got)
		}
	}
}

func TestProblemMasteryQualityCombinesHardnessAndEffort(t *testing.T) {
	if got := ProblemMasteryQuality(1, 1); got != 0.8 {
		t.Fatalf("one-shot medium quality = %v, want 0.8", got)
	}
	want := 0.6602467497
	got := ProblemMasteryQuality(1, 0.7261513374)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("four-attempt medium quality = %.12f, want %.12f", got, want)
	}
	wantACOnly := 0.6892023484
	gotACOnly := ProblemMasteryQuality(1, 0.78)
	if math.Abs(gotACOnly-wantACOnly) > 1e-9 {
		t.Fatalf("AC-only medium quality = %.12f, want %.12f", gotACOnly, wantACOnly)
	}
}

func TestProblemMasteryQualityAndTagAbilityScoreStayBounded(t *testing.T) {
	for _, tc := range []struct {
		h, e float64
	}{
		{math.Inf(1), math.Inf(1)},
		{math.NaN(), math.NaN()},
		{-math.Inf(1), -math.Inf(1)},
	} {
		got := ProblemMasteryQuality(tc.h, tc.e)
		if got < 0.15 || got > 0.98 || math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("quality(%v,%v) = %v, outside [0.15,0.98]", tc.h, tc.e, got)
		}
	}
	for _, tc := range [][2]float64{{0, 0}, {math.NaN(), 1}, {math.Inf(1), 1}} {
		got := TagAbilityScore(tc[0], 0)
		if got < 0 || got > 100 || math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("score(%v,0) = %v, outside [0,100]", tc[0], got)
		}
	}
	if got := TagAbilityScore(math.Inf(1), 500); got < 0 || got > 100 {
		t.Fatalf("large invalid quality sum score = %v, outside [0,100]", got)
	}
	if got := TagAbilityScore(0.8, -1); got < 0 || got > 100 {
		t.Fatalf("negative count score = %v, outside [0,100]", got)
	}
}

func TestTagAbilityScoreInvalidQualitySumUsesNeutralQuality(t *testing.T) {
	const count = 10
	want := TagAbilityScore(0.55*count, count)
	for _, qualitySum := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0.15*count - 0.01, 0.98*count + 0.01} {
		if got := TagAbilityScore(qualitySum, count); got != want {
			t.Errorf("invalid quality sum %v score = %v, want neutral score %v", qualitySum, got, want)
		}
	}
}

func TestTagAbilityScoreAcceptsAccumulatedBoundaryRounding(t *testing.T) {
	const count = 100
	qualitySum := 0.0
	for i := 0; i < count; i++ {
		qualitySum += abilityMaxQuality
	}
	if qualitySum <= abilityMaxQuality*count {
		t.Fatalf("test fixture must expose positive accumulation drift: sum=%0.17g bound=%0.17g", qualitySum, abilityMaxQuality*count)
	}
	want := TagAbilityScore(abilityMaxQuality*count, count)
	if got := TagAbilityScore(qualitySum, count); got != want {
		t.Fatalf("boundary accumulation drift must clamp to the valid maximum: got=%v want=%v", got, want)
	}
}

func TestTagAbilityScoreUsesQualityAndBreadthMonotonically(t *testing.T) {
	if got := TagAbilityScore(0.8*500, 500); math.Abs(got-75.405) > 1e-3 {
		t.Fatalf("500 medium-quality problems score = %.3f, want about 75.405", got)
	}
	if TagAbilityScore(0.8*10, 10) <= TagAbilityScore(0.8*5, 5) {
		t.Fatal("adding equal-quality solved problems should increase score")
	}
	if TagAbilityScore(0.9, 1) <= TagAbilityScore(0.5, 1) {
		t.Fatal("higher mastery quality should increase score")
	}
	if TagAbilityScore(0.98, 1) <= TagAbilityScore(0.30, 2) {
		t.Fatal("a tiny count difference must not dominate a large quality difference")
	}
	lowBreadth := TagAbilityScore(0.65*40, 40)
	highBreadth := TagAbilityScore(0.65*160, 160)
	if highBreadth-lowBreadth < 14 {
		t.Fatalf("breadth confidence should visibly separate mature tags: low=%.3f high=%.3f gap=%.3f", lowBreadth, highBreadth, highBreadth-lowBreadth)
	}
}
