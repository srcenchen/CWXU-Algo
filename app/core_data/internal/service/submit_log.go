package service

import (
	"context"
	"cwxu-algo/api/core/v1/submit_log"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/redis/go-redis/v9"
	grpc2 "google.golang.org/grpc"
	"gorm.io/gorm"
)

// orgFeedFirstPageCursor 首屏游标（与 dal 默认最新一致）
const orgFeedFirstPageCursor int64 = -1
const orgFeedCacheTTL = 60 * time.Second

// feedCacheItem gob 友好（避免直接缓存 proto 消息）
type feedCacheItem struct {
	Id        uint32
	UserId    int64
	Platform  string
	SubmitId  string
	Contest   string
	Problem   string
	Lang      string
	Status    string
	Time      int64
	ProblemId uint32
}

type SubmitLogService struct {
	submit_log.UnimplementedSubmitServer
	sbDal *dal.SpiderDal
	db    *gorm.DB
	rdb   *redis.Client
	reg   *registry.Registrar
}

func (s SubmitLogService) userRPC() (*grpc2.ClientConn, error) {
	return userrpc.Conn(s.reg)
}

func (s SubmitLogService) GetSubmitLog(ctx context.Context, req *submit_log.GetSubmitLogReq) (*submit_log.GetSubmitLogRes, error) {
	// 多取一些，过滤掉力扣合成记录后仍尽量凑满 limit；不足则继续向更早时间翻页
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	var memberIDs []int64
	var resolvedOrg uint
	queryUserID := req.UserId
	// following_only：仅关注用户（与组织聚合流组合）
	// userId=-1 或 following_only 时走 scoped 查询
	if req.UserId == -1 || req.FollowingOnly {
		queryUserID = -1
		// 组织聚合动态：仅当前组织成员
		// 注意：followingOnly 与具体 userId>0 互斥语义由调用方保证；
		// 若同时传 followingOnly + userId>0，以 following 组织流为准（忽略单用户 id）。
		ids, orgID, _, err := ResolveOrgMemberIDs(ctx, s.reg, 0, false)
		if err != nil {
			log.Warnf("org members for submit feed: %v", err)
			ids = []int64{}
		}
		resolvedOrg = orgID
		// 公共域：剔除关闭「公共域动态」的用户（隐私仅在公共域生效）
		if isPublicOrgContext(ctx, s.reg, resolvedOrg) {
			ids = filterPublicFeedUserIDs(ctx, s.reg, ids)
		}
		if req.FollowingOnly {
			viewer := auth.GetCurrentUserId(ctx)
			if viewer == 0 {
				return &submit_log.GetSubmitLogRes{Data: nil}, nil
			}
			following := fetchFollowingIDs(ctx, s.reg, int64(viewer))
			ids = intersectIDs(ids, following)
		}
		memberIDs = ids
	}

	// 组织聚合首屏短缓存（不含 following）
	cacheOrgFeed := req.UserId == -1 && !req.FollowingOnly &&
		(req.Cursor == orgFeedFirstPageCursor || req.Cursor == 0) &&
		limit <= 50 && s.rdb != nil && resolvedOrg > 0
	var feedCacheKey string
	if cacheOrgFeed {
		ver := "0"
		if v, e := s.rdb.Get(ctx, "core:submit_feed:global:ver").Result(); e == nil && v != "" {
			ver = v
		}
		feedCacheKey = fmt.Sprintf("core:submit_feed:org:%d:v%s:lim%d", resolvedOrg, ver, limit)
		if b, e := s.rdb.Get(ctx, feedCacheKey).Bytes(); e == nil && len(b) > 0 {
			var cached []feedCacheItem
			if utils.GobDecoder(b, &cached) == nil {
				r := make([]*submit_log.SubmitLog, 0, len(cached))
				for _, it := range cached {
					r = append(r, &submit_log.SubmitLog{
						Id: it.Id, UserId: it.UserId, Platform: it.Platform, SubmitId: it.SubmitId,
						Contest: it.Contest, Problem: it.Problem, Lang: it.Lang, Status: it.Status,
						Time: it.Time, ProblemId: it.ProblemId,
					})
				}
				nameMap := s.fetchUserNames(ctx, r)
				metaMap := s.fetchProblemMeta(ctx, r)
				for _, item := range r {
					if n, ok := nameMap[item.UserId]; ok {
						item.UserName = n
					}
					if item.ProblemId > 0 {
						if m, ok := metaMap[item.ProblemId]; ok {
							item.ProblemTitle = m.Title
							if len(m.Tags) > 0 {
								item.ProblemTags = m.Tags
							}
							item.ProblemDifficulty = m.Difficulty
						}
					}
				}
				return &submit_log.GetSubmitLogRes{Data: r}, nil
			}
		}
	}

	r := make([]*submit_log.SubmitLog, 0, limit)
	cursor := req.Cursor
	if cursor == 0 && queryUserID == -1 {
		cursor = orgFeedFirstPageCursor
	}
	// 最多多轮回源，避免合成记录占比高时只吐半页导致前端误判「没有更多」
	const maxRounds = 6
	for round := 0; round < maxRounds && int64(len(r)) < limit; round++ {
		need := limit - int64(len(r))
		fetchLimit := need * 3
		if fetchLimit < 30 {
			fetchLimit = 30
		}
		d, err := s.sbDal.GetByUserIdScoped(ctx, queryUserID, cursor, fetchLimit, memberIDs)
		if err != nil {
			return nil, errors.InternalServer("内部服务器错误", "服务暂时不可用")
		}
		if len(d) == 0 {
			break
		}
		for _, v := range d {
			// 力扣合成 / UOJ AC 列表合成不进动态；力扣最近通过 lc-prob-* 仍展示
			if model.IsSyntheticSubmitForFeed(v.Platform, v.SubmitID) {
				continue
			}
			var problemID uint32
			if v.ProblemID != nil {
				problemID = uint32(*v.ProblemID)
			}
			r = append(r, &submit_log.SubmitLog{
				Id:        uint32(v.ID),
				UserId:    v.UserID,
				Platform:  v.Platform,
				SubmitId:  model.NormalizeSubmitID(v.Platform, v.SubmitID),
				Contest:   v.Contest,
				Problem:   v.Problem,
				Lang:      v.Lang,
				Status:    v.Status,
				Time:      v.Time.Unix(),
				ProblemId: problemID,
			})
			if int64(len(r)) >= limit {
				break
			}
		}
		// 游标推进到本批最旧一条（含被过滤的），继续向更早翻
		cursor = d[len(d)-1].Time.Unix()
		if int64(len(d)) < fetchLimit {
			// DB 已耗尽
			break
		}
	}

	if feedCacheKey != "" {
		items := make([]feedCacheItem, 0, len(r))
		for _, it := range r {
			items = append(items, feedCacheItem{
				Id: it.Id, UserId: it.UserId, Platform: it.Platform, SubmitId: it.SubmitId,
				Contest: it.Contest, Problem: it.Problem, Lang: it.Lang, Status: it.Status,
				Time: it.Time, ProblemId: it.ProblemId,
			})
		}
		if b, e := utils.GobEncoder(items); e == nil {
			_ = s.rdb.Set(ctx, feedCacheKey, b, orgFeedCacheTTL).Err()
		}
	}

	// 一次 RPC + 一次 SQL，补齐展示字段，避免前端 N+1
	nameMap := s.fetchUserNames(ctx, r)
	metaMap := s.fetchProblemMeta(ctx, r)
	for _, item := range r {
		if n, ok := nameMap[item.UserId]; ok {
			item.UserName = n
		}
		if item.ProblemId > 0 {
			if m, ok := metaMap[item.ProblemId]; ok {
				item.ProblemTitle = m.Title
				if len(m.Tags) > 0 {
					item.ProblemTags = m.Tags
				}
				item.ProblemDifficulty = m.Difficulty
			}
		}
	}

	return &submit_log.GetSubmitLogRes{
		Data: r,
	}, nil
}

// fetchUserNames 批量获取用户展示名（user 服务 GetByIds）
func (s SubmitLogService) fetchUserNames(ctx context.Context, logs []*submit_log.SubmitLog) map[int64]string {
	result := map[int64]string{}
	if len(logs) == 0 {
		return result
	}
	idSet := map[int64]struct{}{}
	for _, v := range logs {
		if v.UserId != 0 {
			idSet[v.UserId] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return result
	}
	userIds := make([]int64, 0, len(idSet))
	for id := range idSet {
		userIds = append(userIds, id)
	}

	client, err := userrpc.ProfileClient(s.reg)
	if err != nil {
		log.Errorf("submit_log userRPC: %v", err)
		return result
	}

	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}
	res, err := client.GetByIds(ctx, &profile.GetByIdsReq{UserIds: userIds, OrgId: orgID})
	if err != nil {
		log.Errorf("submit_log GetByIds: %v", err)
		return result
	}
	for _, p := range res.Profiles {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.Username)
		}
		// 仍无展示名时留空，由前端用中性文案，不把内部 userId 当名字
		result[p.UserId] = name
	}
	return result
}

type problemMeta struct {
	Title      string
	Tags       []string
	Difficulty string
}

// fetchProblemMeta 批量取题库标题、标签与难度（本库 problems）
func (s SubmitLogService) fetchProblemMeta(ctx context.Context, logs []*submit_log.SubmitLog) map[uint32]problemMeta {
	result := map[uint32]problemMeta{}
	if len(logs) == 0 || s.db == nil {
		return result
	}
	idSet := map[uint32]struct{}{}
	for _, v := range logs {
		if v.ProblemId > 0 {
			idSet[v.ProblemId] = struct{}{}
		}
	}
	if len(idSet) == 0 {
		return result
	}
	ids := make([]uint32, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	var rows []struct {
		ID         uint              `gorm:"column:id"`
		Title      string            `gorm:"column:title"`
		Tags       model.StringArray `gorm:"column:tags"`
		Difficulty string            `gorm:"column:difficulty"`
	}
	if err := s.db.WithContext(ctx).
		Table("problems").
		Select("id, title, tags, difficulty").
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		log.Errorf("submit_log fetchProblemMeta: %v", err)
		return result
	}
	for _, row := range rows {
		tags := []string(row.Tags)
		if tags == nil {
			tags = []string{}
		}
		// 最多展示 6 个，避免动态列表过宽
		if len(tags) > 6 {
			tags = tags[:6]
		}
		result[uint32(row.ID)] = problemMeta{
			Title:      row.Title,
			Tags:       tags,
			Difficulty: row.Difficulty,
		}
	}
	return result
}

func (s SubmitLogService) LastSubmitTime(ctx context.Context, req *submit_log.LastSubmitTimeReq) (*submit_log.LastSubmitTimeRes, error) {
	var d []model.SubmitLog
	timesMap := make(map[int64]int64)
	pipe := s.rdb.Pipeline()
	keys := make([]string, 0)
	for _, v := range req.UserIds {
		keys = append(keys, fmt.Sprintf("user:%d:lastSubmitTime", v))
	}
	// 到缓存查
	rVal, _ := s.rdb.MGet(ctx, keys...).Result()
	missUser := make([]int64, 0)
	for i, v := range rVal {
		if v == nil {
			missUser = append(missUser, req.UserIds[i])
			continue
		}
		in, ok := v.(string)
		if !ok {
			continue
		}
		val, _ := strconv.ParseInt(in, 10, 64)
		timesMap[req.UserIds[i]] = val
	}
	// 回源
	if len(missUser) > 0 {
		err := s.db.
			Table("submit_logs").
			Select("DISTINCT ON (user_id) user_id, time").
			Where("user_id IN ?", missUser).
			Order("user_id, time DESC").
			Scan(&d).Error
		if err != nil {
			return nil, errors.InternalServer("内部错误", "数据库查询错误")
		}
		for _, v := range d {
			timesMap[v.UserID] = v.Time.Unix()
			// 塞入缓存
			pipe.Set(ctx, fmt.Sprintf("user:%d:lastSubmitTime", v.UserID), v.Time.Unix(), 1*time.Hour)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			log.Errorf("LastSubmitTime: pipeline exec failed: %v", err)
		}
	}
	encoded, err := utils.GobEncoder(timesMap)
	if err != nil {
		return nil, errors.InternalServer("内部错误", "编码错误")
	}
	return &submit_log.LastSubmitTimeRes{TimeMap: encoded}, nil
}

func NewSubmitLogService(sbDal *dal.SpiderDal, data *data.Data, reg *discovery.Register) *SubmitLogService {
	return &SubmitLogService{
		sbDal: sbDal,
		db:    data.DB,
		rdb:   data.RDB,
		reg:   &reg.Reg,
	}
}
