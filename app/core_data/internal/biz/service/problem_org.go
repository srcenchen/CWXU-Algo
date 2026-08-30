package service

import (
	"context"
	"fmt"
	"time"

	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/log"
)

// pipelineUserCacheTTL 题面流水线资格用户缓存
const pipelineUserCacheTTL = 2 * time.Minute

// problemHasFetchSubmitter 近窗是否有「题面爬取资格」用户提交
func (uc *ProblemUseCase) problemHasFetchSubmitter(problemID uint) bool {
	return uc.problemHasPipelineSubmitter(problemID, "fetch")
}

// problemHasAISubmitter 近窗是否有「题面 AI 资格」用户提交
func (uc *ProblemUseCase) problemHasAISubmitter(problemID uint) bool {
	return uc.problemHasPipelineSubmitter(problemID, "ai")
}

// userHasAIEligibility 指定用户是否具备题面 AI 资格（默认非公共域组织 + 个人覆盖）
func (uc *ProblemUseCase) userHasAIEligibility(userID uint) bool {
	if userID == 0 {
		return false
	}
	users, ok := uc.pipelineUserIDs("ai")
	if !ok {
		// 名单不可用时保守放行，与 problemHasPipelineSubmitter 一致
		log.Warnf("userHasAIEligibility: list unavailable, allow user=%d", userID)
		return true
	}
	_, yes := users[int64(userID)]
	return yes
}

func (uc *ProblemUseCase) UserCanFetchProblem(ctx context.Context, userID uint) bool {
	if userID == 0 {
		return false
	}
	users, ok := uc.pipelineUserIDs("fetch")
	if !ok {
		return false
	}
	_, yes := users[int64(userID)]
	return yes
}

// problemHasOrgSubmitter 兼容旧调用：等价于 AI 资格（题面 AI 闸门）
func (uc *ProblemUseCase) problemHasOrgSubmitter(problemID uint) bool {
	return uc.problemHasAISubmitter(problemID)
}

func (uc *ProblemUseCase) problemHasPipelineSubmitter(problemID uint, kind string) bool {
	ids := uc.pipelineSubmitterIDs(problemID, kind)
	return len(ids) > 0
}

// pipelineSubmitterIDs 近窗有指定流水线资格用户提交的 userId 列表（去重，最多 50）。
// 名单不可用时返回 nil（调用方保守放行）；无资格提交返回空切片。
func (uc *ProblemUseCase) pipelineSubmitterIDs(problemID uint, kind string) []int64 {
	if problemID == 0 {
		return nil
	}
	users, ok := uc.pipelineUserIDs(kind)
	if !ok {
		// 拉不到名单时保守放行，避免 user 短暂故障拖死流水线
		log.Warnf("pipelineSubmitterIDs(%s): list unavailable, allow id=%d", kind, problemID)
		return nil
	}
	if len(users) == 0 {
		return []int64{}
	}
	ids := make([]int64, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	cutoff := time.Now().Add(-backfillWindow)
	var uids []int64
	err := uc.data.DB.Model(&model.SubmitLog{}).
		Where("problem_id = ?", problemID).
		Where("time IS NOT NULL AND time >= ?", cutoff).
		Where("user_id IN ?", ids).
		Distinct().
		Limit(50).
		Pluck("user_id", &uids).Error
	if err != nil {
		log.Warnf("pipelineSubmitterIDs(%s) query id=%d: %v", kind, problemID, err)
		return nil
	}
	return uids
}

// shouldEnqueueFetch 是否入队爬题面：近窗有爬取资格用户提交
func (uc *ProblemUseCase) shouldEnqueueFetch(problemID uint) bool {
	return uc.problemHasFetchSubmitter(problemID)
}

// recentPipelineProblemSet 近窗有指定流水线资格用户提交的 problem_id 集合（批量，避免 N+1）
// ok=false：名单不可用（调用方应保守放行或降级逐题检查）
func (uc *ProblemUseCase) recentPipelineProblemSet(kind string, cutoff time.Time) (map[uint]struct{}, bool) {
	users, ok := uc.pipelineUserIDs(kind)
	if !ok {
		return nil, false
	}
	out := make(map[uint]struct{})
	if len(users) == 0 {
		return out, true
	}
	ids := make([]int64, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	var pids []uint
	err := uc.data.DB.Model(&model.SubmitLog{}).
		Select("DISTINCT problem_id").
		Where("problem_id IS NOT NULL AND problem_id > 0").
		Where("time IS NOT NULL AND time >= ?", cutoff).
		Where("user_id IN ?", ids).
		Pluck("problem_id", &pids).Error
	if err != nil {
		log.Warnf("recentPipelineProblemSet(%s): %v", kind, err)
		return nil, false
	}
	for _, id := range pids {
		if id > 0 {
			out[id] = struct{}{}
		}
	}
	return out, true
}

// pipelineUserIDSlice 资格用户 id 列表（有序无关）
func (uc *ProblemUseCase) pipelineUserIDSlice(kind string) ([]int64, bool) {
	users, ok := uc.pipelineUserIDs(kind)
	if !ok {
		return nil, false
	}
	ids := make([]int64, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	return ids, true
}

func (uc *ProblemUseCase) pipelineUserIDs(kind string) (map[int64]struct{}, bool) {
	uc.orgUsersMu.Lock()
	defer uc.orgUsersMu.Unlock()
	if uc.pipelineUsersCache != nil && time.Since(uc.pipelineUsersAt) < pipelineUserCacheTTL {
		if kind == "ai" {
			return uc.pipelineUsersCache.ai, true
		}
		return uc.pipelineUsersCache.fetch, true
	}
	fetchIDs, aiIDs, err := uc.fetchPipelineUserIDs()
	if err != nil {
		log.Warnf("fetchPipelineUserIDs: %v", err)
		if uc.pipelineUsersCache != nil {
			if kind == "ai" {
				return uc.pipelineUsersCache.ai, true
			}
			return uc.pipelineUsersCache.fetch, true
		}
		// 回退旧缓存字段
		if uc.orgUsersCache != nil {
			return uc.orgUsersCache, true
		}
		return nil, false
	}
	fetchM := make(map[int64]struct{}, len(fetchIDs))
	for _, id := range fetchIDs {
		fetchM[id] = struct{}{}
	}
	aiM := make(map[int64]struct{}, len(aiIDs))
	for _, id := range aiIDs {
		aiM[id] = struct{}{}
	}
	uc.pipelineUsersCache = &pipelineUserSets{fetch: fetchM, ai: aiM}
	uc.pipelineUsersAt = time.Now()
	// 兼容旧字段
	uc.orgUsersCache = fetchM
	uc.orgUsersAt = uc.pipelineUsersAt
	if kind == "ai" {
		return aiM, true
	}
	return fetchM, true
}

// nonPublicOrgUserIDs 兼容：返回爬取资格集合
func (uc *ProblemUseCase) nonPublicOrgUserIDs() (map[int64]struct{}, bool) {
	return uc.pipelineUserIDs("fetch")
}

type pipelineUserSets struct {
	fetch map[int64]struct{}
	ai    map[int64]struct{}
}

func (uc *ProblemUseCase) fetchPipelineUserIDs() (fetchIDs, aiIDs []int64, err error) {
	if uc.reg == nil {
		return nil, nil, fmt.Errorf("registry nil")
	}
	client, err := userrpc.ProfileClient(uc.reg)
	if err != nil {
		return nil, nil, err
	}
	res, err := client.GetNonPublicOrgUserIds(context.Background(), &profile.GetNonPublicOrgUserIdsReq{})
	if err != nil {
		return nil, nil, err
	}
	fetchIDs = res.GetFetchUserIds()
	if len(fetchIDs) == 0 {
		// 旧 user 服务未返回 fetchUserIds 时回落 userIds
		fetchIDs = res.GetUserIds()
	}
	aiIDs = res.GetAiUserIds()
	if len(aiIDs) == 0 {
		// 旧服务无 ai 列表：与爬取共用
		aiIDs = fetchIDs
	}
	if fetchIDs == nil {
		fetchIDs = []int64{}
	}
	if aiIDs == nil {
		aiIDs = []int64{}
	}
	return fetchIDs, aiIDs, nil
}

func (uc *ProblemUseCase) fetchNonPublicOrgUserIDs() ([]int64, error) {
	fetch, _, err := uc.fetchPipelineUserIDs()
	return fetch, err
}
