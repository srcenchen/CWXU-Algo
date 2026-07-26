package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/registry"
)

// orgMembersCacheTTL 组织成员列表进程内短缓存：热力/排行等聚合接口每次请求都要成员列表，
// 60s 级延迟可接受，避免高频打 user 服务
const orgMembersCacheTTL = 60 * time.Second

type orgMembersCacheEntry struct {
	ids   []int64
	orgID uint
	at    time.Time
}

var (
	orgMembersCacheMu sync.Mutex
	// key = 请求 orgID（0 表示回落公共域，user 服务解析确定）
	orgMembersCache = map[uint]orgMembersCacheEntry{}
	// 公共域 orgID 值缓存（isPublicOrgID 不再拉全量成员）
	publicOrgIDCached uint
	publicOrgIDAt     time.Time
)

// fetchOrgMemberIDs 通过 user 服务取组织成员（带 60s 进程内缓存）
func fetchOrgMemberIDs(ctx context.Context, reg *registry.Registrar, orgID uint) ([]int64, uint, bool, error) {
	if reg == nil {
		return nil, 0, false, fmt.Errorf("registry nil")
	}
	if orgID == 0 {
		if pd := auth.GetCurrentUser(ctx); pd != nil && pd.OrgID > 0 {
			orgID = pd.OrgID
		}
	}
	orgMembersCacheMu.Lock()
	if e, ok := orgMembersCache[orgID]; ok && time.Since(e.at) < orgMembersCacheTTL {
		ids, resolved := e.ids, e.orgID
		orgMembersCacheMu.Unlock()
		return ids, resolved, false, nil
	}
	orgMembersCacheMu.Unlock()

	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		return nil, orgID, false, err
	}
	res, err := client.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{OrgId: int64(orgID)})
	if err != nil {
		return nil, orgID, false, err
	}
	ids := res.GetUserIds()
	if ids == nil {
		ids = []int64{}
	}
	resolved := uint(res.GetOrgId())
	orgMembersCacheMu.Lock()
	orgMembersCache[orgID] = orgMembersCacheEntry{ids: ids, orgID: resolved, at: time.Now()}
	if orgID == 0 {
		// 顺带记住公共域 orgID（orgID=0 时 user 服务回落公共域）
		publicOrgIDCached = resolved
		publicOrgIDAt = time.Now()
	}
	orgMembersCacheMu.Unlock()
	return ids, resolved, false, nil
}

// isPublicOrgID orgID=0 或等于公共域 id 时视为公共域（全站聚合）。
// 公共域 orgID 走进程内缓存比较，不再每次拉全量成员。
func isPublicOrgID(ctx context.Context, reg *registry.Registrar, orgID uint) bool {
	if orgID == 0 {
		return true
	}
	if reg == nil {
		return true
	}
	orgMembersCacheMu.Lock()
	if publicOrgIDCached > 0 && time.Since(publicOrgIDAt) < orgMembersCacheTTL {
		pub := publicOrgIDCached
		orgMembersCacheMu.Unlock()
		return pub == orgID
	}
	orgMembersCacheMu.Unlock()

	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		return true
	}
	pub, err := client.GetUserIdsByOrg(ctx, &profile.GetUserIdsByOrgReq{OrgId: 0})
	if err != nil {
		return true
	}
	orgMembersCacheMu.Lock()
	publicOrgIDCached = uint(pub.GetOrgId())
	publicOrgIDAt = time.Now()
	orgMembersCacheMu.Unlock()
	return uint(pub.GetOrgId()) == orgID
}

// fetchDisplayNames 批量取当前组织（或指定 org）内展示名
func fetchDisplayNames(ctx context.Context, reg *registry.Registrar, userIDs []int64) map[int64]string {
	out := map[int64]string{}
	if reg == nil || len(userIDs) == 0 {
		return out
	}
	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}
	client, err := userrpc.ProfileClient(reg)
	if err != nil {
		return out
	}
	res, err := client.GetByIds(ctx, &profile.GetByIdsReq{UserIds: userIDs, OrgId: orgID})
	if err != nil || res == nil {
		return out
	}
	for _, p := range res.Profiles {
		if p.Name != "" {
			out[p.UserId] = p.Name
		}
	}
	return out
}
