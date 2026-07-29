package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
)

// orgScopeMembersCacheTTL 组织成员列表进程内短缓存（动态流/比赛列表每请求都要成员列表）。
// 30s：user 改成员后无法直调 core 失效，靠短 TTL 折中；见 InvalidateOrgMemberCache。
const orgScopeMembersCacheTTL = 30 * time.Second

// orgScopeMembersFailBackoff user 服务抖动时的重试间隔：期间直接用上一次成功值，
// 避免每个请求都去撞一次超时的 RPC（比赛列表等会因此整页变空 / 变慢）。
const orgScopeMembersFailBackoff = 15 * time.Second

type orgScopeMembersEntry struct {
	ids   []int64
	orgID uint
	at    time.Time
	// failAt 最近一次 RPC 失败时刻；ids/at 仍是上一次成功的值
	failAt time.Time
}

var (
	orgScopeMembersMu sync.Mutex
	// key = 请求 orgID（0 表示回落公共域）
	orgScopeMembersCache = map[uint]orgScopeMembersEntry{}
	// 公共域 orgID 值缓存（isPublicOrgContext 不再拉全量成员）
	orgScopePublicID   uint
	orgScopePublicIDAt time.Time
)

// InvalidateOrgMemberCache 清进程内组织成员缓存。
// orgID=0 时清空全部；否则只删对应 key。供成员变更后调用（当前跨服务无 pub/sub，Resolve 靠 30s TTL 折中）。
func InvalidateOrgMemberCache(orgID uint) {
	orgScopeMembersMu.Lock()
	defer orgScopeMembersMu.Unlock()
	if orgID == 0 {
		orgScopeMembersCache = map[uint]orgScopeMembersEntry{}
		orgScopePublicID = 0
		orgScopePublicIDAt = time.Time{}
		return
	}
	delete(orgScopeMembersCache, orgID)
}

// ResolveOrgMemberIDs 解析组织成员 userId 列表（带 30s 进程内缓存）。
// orgID=0 时用 JWT 当前组织；仍为 0 则 user 服务回落公共域。
// scopeSite=true 且具备全站统计权限：unrestricted=true 表示全站。
func ResolveOrgMemberIDs(ctx context.Context, reg *registry.Registrar, orgID uint, scopeSite bool) (userIDs []int64, resolvedOrg uint, unrestricted bool, err error) {
	if scopeSite && auth.HasPerm(ctx, rbac.PermSiteStatsRead) {
		return nil, 0, true, nil
	}
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
			orgID = pd.OrgID
		}
	}
	orgScopeMembersMu.Lock()
	cached, hasCached := orgScopeMembersCache[orgID]
	if hasCached && time.Since(cached.at) < orgScopeMembersCacheTTL {
		ids, resolved := cached.ids, cached.orgID
		orgScopeMembersMu.Unlock()
		return ids, resolved, false, nil
	}
	// 刚失败过且手上有旧值：先用旧值，别把请求堵在超时的 RPC 上
	if hasCached && cached.ids != nil && time.Since(cached.failAt) < orgScopeMembersFailBackoff {
		ids, resolved := cached.ids, cached.orgID
		orgScopeMembersMu.Unlock()
		return ids, resolved, false, nil
	}
	orgScopeMembersMu.Unlock()

	// staleFallback RPC 失败时回落上一次成功的成员列表（宁可稍旧，也不要整页空数据）
	staleFallback := func(err error) ([]int64, uint, bool, error) {
		orgScopeMembersMu.Lock()
		e, ok := orgScopeMembersCache[orgID]
		if ok {
			e.failAt = time.Now()
			orgScopeMembersCache[orgID] = e
		}
		orgScopeMembersMu.Unlock()
		if ok && e.ids != nil {
			log.Warnf("org members fallback to stale (org=%d, age=%s): %v", orgID, time.Since(e.at).Truncate(time.Second), err)
			return e.ids, e.orgID, false, nil
		}
		return nil, orgID, false, err
	}

	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		return staleFallback(err)
	}
	res, err := client.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{OrgId: int64(orgID)})
	if err != nil {
		log.Warnf("GetUserIdsByOrg: %v", err)
		return staleFallback(err)
	}
	ids := res.GetUserIds()
	if ids == nil {
		ids = []int64{}
	}
	resolved := uint(res.GetOrgId())
	orgScopeMembersMu.Lock()
	orgScopeMembersCache[orgID] = orgScopeMembersEntry{ids: ids, orgID: resolved, at: time.Now()}
	if orgID == 0 {
		// 顺带记住公共域 orgID（orgID=0 时 user 服务回落公共域）
		orgScopePublicID = resolved
		orgScopePublicIDAt = time.Now()
	}
	orgScopeMembersMu.Unlock()
	return ids, resolved, false, nil
}

// fetchFollowingIDs 某人关注的 userId 列表
func fetchFollowingIDs(ctx context.Context, reg *registry.Registrar, userID int64) []int64 {
	if reg == nil || userID <= 0 {
		return []int64{}
	}
	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		log.Warnf("following ids dial: %v", err)
		return []int64{}
	}
	res, err := client.GetFollowingIds(ctx, &profile.GetFollowingIdsReq{UserId: userID})
	if err != nil {
		log.Warnf("GetFollowingIds: %v", err)
		return []int64{}
	}
	ids := res.GetUserIds()
	if ids == nil {
		return []int64{}
	}
	return ids
}

// filterPublicFeedUserIDs 公共域动态：剔除关闭动态可见的用户。
// 调用失败时 fail-closed（返回空），避免隐私过滤失效时全量泄露。
func filterPublicFeedUserIDs(ctx context.Context, reg *registry.Registrar, userIDs []int64) []int64 {
	if reg == nil || len(userIDs) == 0 {
		return []int64{}
	}
	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		log.Warnf("filter public feed dial: %v", err)
		return []int64{}
	}
	res, err := client.FilterPublicFeedUserIds(ctx, &profile.FilterPublicFeedUserIdsReq{UserIds: userIDs})
	if err != nil {
		log.Warnf("FilterPublicFeedUserIds: %v", err)
		return []int64{}
	}
	ids := res.GetUserIds()
	if ids == nil {
		return []int64{}
	}
	return ids
}

// isPublicOrgContext orgID=0 或公共域 slug → 隐私生效。
// 公共域 orgID 走进程内缓存比较，不再每次拉全量成员。
func isPublicOrgContext(ctx context.Context, reg *registry.Registrar, orgID uint) bool {
	if orgID == 0 {
		return true
	}
	if reg == nil {
		return true
	}
	orgScopeMembersMu.Lock()
	if orgScopePublicID > 0 && time.Since(orgScopePublicIDAt) < orgScopeMembersCacheTTL {
		pub := orgScopePublicID
		orgScopeMembersMu.Unlock()
		return pub == orgID
	}
	orgScopeMembersMu.Unlock()

	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		return true
	}
	// public org：GetUserIdsByOrg(orgId=0) 回落公共域，对比 id
	pub, err := client.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{OrgId: 0})
	if err != nil {
		return true
	}
	orgScopeMembersMu.Lock()
	orgScopePublicID = uint(pub.GetOrgId())
	orgScopePublicIDAt = time.Now()
	orgScopeMembersMu.Unlock()
	return uint(pub.GetOrgId()) == orgID
}

func intersectIDs(a, b []int64) []int64 {
	if len(a) == 0 || len(b) == 0 {
		return []int64{}
	}
	set := make(map[int64]struct{}, len(b))
	for _, id := range b {
		set[id] = struct{}{}
	}
	out := make([]int64, 0, len(a))
	for _, id := range a {
		if _, ok := set[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// ResolveOrgMemberIDsFromConn 复用已有 user 连接
func ResolveOrgMemberIDsFromConn(ctx context.Context, client profile.ProfileClient, orgID uint, scopeSite bool) ([]int64, uint, bool, error) {
	if scopeSite && auth.HasPerm(ctx, rbac.PermSiteStatsRead) {
		return nil, 0, true, nil
	}
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
			orgID = pd.OrgID
		}
	}
	if client == nil {
		return nil, orgID, false, fmt.Errorf("profile client nil")
	}
	res, err := client.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{OrgId: int64(orgID)})
	if err != nil {
		return nil, orgID, false, err
	}
	ids := res.GetUserIds()
	if ids == nil {
		ids = []int64{}
	}
	return ids, uint(res.GetOrgId()), false, nil
}
