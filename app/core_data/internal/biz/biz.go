package biz

import (
	"cwxu-algo/app/core_data/internal/biz/service"

	"github.com/google/wire"
)

// ProviderSet is biz providers.
var ProviderSet = wire.NewSet(
	service.NewSpiderUseCase,
	service.NewConsumer,
	service.NewStatisticUseCase,
	service.NewProblemUseCase,
	service.NewProblemTagger,
	service.NewProblemFetchConsumer,
	service.NewProblemAnalyzeConsumer,
	service.NewUserProfileConsumer,
	service.NewMailConsumer,
)
