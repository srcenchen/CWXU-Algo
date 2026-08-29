package task

import "github.com/google/wire"

var ProviderSet = wire.NewSet(
	NewSpiderTask,
	NewCronTask,
	NewSummaryTask,
	NewUserProfileTask,
	NewProblemAbilityStatsRefresher,
	wire.Bind(new(AbilityStatsRefresher), new(*ProblemAbilityStatsRefresher)),
)
