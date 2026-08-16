package service

import "github.com/google/wire"

// ProviderSet is service providers.
var ProviderSet = wire.NewSet(NewSubmitLogService, NewSpiderService, NewStatistic, NewContestLogService, NewBulletinService, NewProblemService, NewEmergencyService, NewContestCalendarService, NewCommunityService, NewProblemsetService, NewHealthService, NewBackupService)
