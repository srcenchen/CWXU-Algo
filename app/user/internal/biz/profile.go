// Package biz 的 ProfileUseCase：列表 / 按组 ID 的薄封装；复杂逻辑在 service/profile.go
//（刻意保留，避免半吊子下沉）。
package biz

import (
	"context"
	"cwxu-algo/app/user/internal/data/dal"
	"cwxu-algo/app/user/internal/data/model"
)

// ProfileUseCase 列表/按组 ID 等薄封装；复杂资料逻辑在 service/profile.go（刻意保留本层）。
type ProfileUseCase struct {
	profileDal *dal.ProfileDal
}

func NewProfileUseCase(profileDal *dal.ProfileDal) *ProfileUseCase {
	return &ProfileUseCase{
		profileDal: profileDal,
	}
}

func (uc *ProfileUseCase) GetList(ctx context.Context, pageSize, pageNum int64, keyword string, dormantOnly bool, inactiveDays int) ([]model.User, int64, error) {
	return uc.profileDal.GetList(ctx, pageSize, pageNum, keyword, dormantOnly, inactiveDays)
}

func (uc *ProfileUseCase) GetUserIdsByGroup(ctx context.Context, groupId int64) ([]int64, error) {
	return uc.profileDal.GetUserIdsByGroup(ctx, groupId)
}

func (uc *ProfileUseCase) GetByIds(ctx context.Context, userIds []int64) ([]dal.UserProfile, error) {
	return uc.profileDal.GetByIds(ctx, userIds)
}
