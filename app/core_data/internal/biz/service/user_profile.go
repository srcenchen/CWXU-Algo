package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/loadgate"

	"github.com/go-kratos/kratos/v2/log"
	"golang.org/x/sync/singleflight"
)

// 画像缓存：ver 精确失效 + latest 兜底（爬虫后仍可读旧画像，同时 MQ 刷新）
const (
	// s6：能力雷达改用难度加权题量 + 饱和锚定（非纯 count 归一化）
	userProfileCacheSchema = "6"
	userProfileLatestTTL   = 30 * 24 * time.Hour
	userProfileVerTTL      = 7 * 24 * time.Hour
	// userProfileFpKey 用户 AC 指纹缓存：数据未变化时跳过重建，削 3h 整点画像风暴
	userProfileFpPref = "user_profile:fp:"
	// profileRadarK 能力雷达饱和锚定：weight=K 时 score≈50（等价 10 道中等题）。
	// 避免纯题量归一（最强标签恒 100），奖励难度，且跨用户可比。
	profileRadarK = 30.0
)

func userProfileFpKey(userID int64) string {
	return fmt.Sprintf("%s%d", userProfileFpPref, userID)
}

// profileBuildSF 同一用户并发请求只算一次（HTTP 即时处理 + MQ consumer 共用）
var profileBuildSF singleflight.Group

// UserProfileSnapshot 可 gob 缓存的画像快照
type UserProfileSnapshot struct {
	Radar []struct {
		Tag     string
		Score   float64
		ACCount int64
	}
	Platforms []struct {
		Name  string
		Count int64
	}
	Difficulties []struct {
		Name  string
		Count int64
	}
	TotalAC int64
	BuiltAt int64 // unix sec
}

func userProfileVerKey(userID int64) string {
	return fmt.Sprintf("statistic:user:%d:ver", userID)
}

func userProfileCacheKey(userID int64, ver string) string {
	if ver == "" {
		ver = "0"
	}
	return fmt.Sprintf("problem:user_profile:s%s:u%d:v%s", userProfileCacheSchema, userID, ver)
}

func userProfileLatestKey(userID int64) string {
	return fmt.Sprintf("problem:user_profile:s%s:u%d:latest", userProfileCacheSchema, userID)
}

func (uc *ProblemUseCase) profileVer(ctx context.Context, userID int64) string {
	if uc.data == nil || uc.data.RDB == nil {
		return "0"
	}
	v, err := uc.data.RDB.Get(ctx, userProfileVerKey(userID)).Result()
	if err != nil || v == "" {
		return "0"
	}
	return v
}

func (uc *ProblemUseCase) readProfileCache(ctx context.Context, key string) (*UserProfileSnapshot, bool) {
	if uc.data == nil || uc.data.RDB == nil || key == "" {
		return nil, false
	}
	b, err := uc.data.RDB.Get(ctx, key).Bytes()
	if err != nil || len(b) == 0 {
		return nil, false
	}
	var snap UserProfileSnapshot
	if err := utils.GobDecoder(b, &snap); err != nil {
		return nil, false
	}
	return &snap, true
}

func (uc *ProblemUseCase) writeProfileCache(ctx context.Context, userID int64, snap *UserProfileSnapshot) {
	if uc.data == nil || uc.data.RDB == nil || snap == nil || userID <= 0 {
		return
	}
	if snap.BuiltAt == 0 {
		snap.BuiltAt = time.Now().Unix()
	}
	b, err := utils.GobEncoder(snap)
	if err != nil {
		log.Warnf("user_profile gob encode user=%d: %v", userID, err)
		return
	}
	ver := uc.profileVer(ctx, userID)
	_ = uc.data.RDB.Set(ctx, userProfileCacheKey(userID, ver), b, userProfileVerTTL).Err()
	_ = uc.data.RDB.Set(ctx, userProfileLatestKey(userID), b, userProfileLatestTTL).Err()
}

// EnqueueUserProfileRebuild 异步重建（绑平台/爬虫 / 空雷达补刷 / 每日 cron）
// 走 user_profile 队列，不阻塞 HTTP。
func (uc *ProblemUseCase) EnqueueUserProfileRebuild(userID int64) {
	if userID <= 0 || uc.profileTask == nil {
		return
	}
	_ = uc.profileTask.Do(userID)
}

// BuildAndCacheUserProfile MQ consumer 用：先全量重建 user_tag_ac，再算画像写缓存。
// force=false 时按用户 AC 指纹跳过「数据未变化」的用户（3h 整点爬虫风暴主要削这里）。
// 重 JOIN 只在队列里跑；key 与 HTTP 轻量路径分离，避免抢到「未重建」的空结果。
func (uc *ProblemUseCase) BuildAndCacheUserProfile(userID int64, force bool) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user_id")
	}
	_, err, _ := profileBuildSF.Do(fmt.Sprintf("up-rebuild:%d", userID), func() (interface{}, error) {
		ctx := context.Background()
		fp, fpErr := dal.UserACFingerprint(ctx, uc.data.DB, userID)
		if fpErr == nil && fp != "" && !force && uc.data != nil && uc.data.RDB != nil {
			if last, e := uc.data.RDB.Get(ctx, userProfileFpKey(userID)).Result(); e == nil && last == fp {
				log.Infof("user_profile skip unchanged user=%d fp=%s", userID, fp)
				return nil, nil
			}
		}
		// 系统过载时先退避，画像重 JOIN 让路给在线访问（最多等 30s）
		loadgate.Global().Wait(ctx, 30*time.Second)
		// 雷达预聚合从 user_ac_problems×problem_tags 重算，保证「做过有标签的题就一定有雷达」
		if e := dal.RebuildUserTagACForUser(ctx, uc.data.DB, userID); e != nil {
			log.Warnf("user_profile rebuild tag_ac user=%d: %v", userID, e)
			// 预聚合失败仍尝试用旧表算画像，避免整任务失败无限重试卡死
		}
		snap, e := uc.computeUserProfile(userID)
		if e != nil {
			return nil, e
		}
		uc.writeProfileCache(ctx, userID, snap)
		if fpErr == nil && fp != "" && uc.data != nil && uc.data.RDB != nil {
			_ = uc.data.RDB.Set(ctx, userProfileFpKey(userID), fp, 0).Err()
		}
		return snap, nil
	})
	return err
}

// buildUserProfileNow HTTP 冷启动：只读现有预聚合，不做重 JOIN（避免拖垮接口）。
// 若雷达为空且确有标签题，由 maybeEnqueueEmptyRadarHeal 入队后台补齐。
func (uc *ProblemUseCase) buildUserProfileNow(userID int64) (*UserProfileSnapshot, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user_id")
	}
	v, err, _ := profileBuildSF.Do(fmt.Sprintf("up-light:%d", userID), func() (interface{}, error) {
		snap, e := uc.computeUserProfile(userID)
		if e != nil {
			return nil, e
		}
		uc.writeProfileCache(context.Background(), userID, snap)
		return snap, nil
	})
	if err != nil {
		return nil, err
	}
	snap, _ := v.(*UserProfileSnapshot)
	return snap, nil
}

// maybeEnqueueEmptyRadarHeal 雷达为空且做过有标签的题 → 入队补刷（不挡响应）
func (uc *ProblemUseCase) maybeEnqueueEmptyRadarHeal(userID int64, snap *UserProfileSnapshot) {
	if userID <= 0 || snap == nil || len(snap.Radar) > 0 {
		return
	}
	go func(uid int64) {
		has, err := dal.UserHasTaggedAC(context.Background(), uc.data.DB, uid)
		if err != nil {
			log.Warnf("user_profile empty-radar check user=%d: %v", uid, err)
			return
		}
		if !has {
			return
		}
		log.Infof("user_profile empty-radar heal enqueue user=%d", uid)
		uc.EnqueueUserProfileRebuild(uid)
	}(userID)
}

// UserProfile 读路径：缓存优先；HTTP 永不做 tag_ac 重 JOIN；空雷达自动入队补刷
func (uc *ProblemUseCase) UserProfile(userID int64) (radar []struct {
	Tag     string
	Score   float64
	ACCount int64
}, platforms []struct {
	Name  string
	Count int64
}, difficulties []struct {
	Name  string
	Count int64
}, totalAC int64, err error) {
	ctx := context.Background()
	ver := uc.profileVer(ctx, userID)

	if snap, ok := uc.readProfileCache(ctx, userProfileCacheKey(userID, ver)); ok {
		uc.maybeEnqueueEmptyRadarHeal(userID, snap)
		return unpackProfile(snap)
	}
	// ver 失效：先返回 latest，并后台入队重算（不挡本次响应）
	if snap, ok := uc.readProfileCache(ctx, userProfileLatestKey(userID)); ok {
		uc.EnqueueUserProfileRebuild(userID)
		uc.maybeEnqueueEmptyRadarHeal(userID, snap)
		return unpackProfile(snap)
	}

	// 从未处理：轻量即时算（读预聚合）；重 JOIN 交给队列
	start := time.Now()
	snap, e := uc.buildUserProfileNow(userID)
	if e != nil {
		log.Errorf("user_profile on-demand user=%d: %v", userID, e)
		uc.EnqueueUserProfileRebuild(userID)
		err = e
		return
	}
	if uc.profileTask != nil {
		uc.profileTask.ClearPending(userID)
	}
	uc.maybeEnqueueEmptyRadarHeal(userID, snap)
	log.Infof("user_profile on-demand user=%d cost=%s", userID, time.Since(start).Round(time.Millisecond))
	return unpackProfile(snap)
}

func unpackProfile(snap *UserProfileSnapshot) (radar []struct {
	Tag     string
	Score   float64
	ACCount int64
}, platforms []struct {
	Name  string
	Count int64
}, difficulties []struct {
	Name  string
	Count int64
}, totalAC int64, err error) {
	if snap == nil {
		return
	}
	radar = snap.Radar
	platforms = snap.Platforms
	difficulties = snap.Difficulties
	totalAC = snap.TotalAC
	return
}

// computeUserProfile 雷达读 user_tag_ac；平台过题力扣走官方 acTotal；难度仍 JOIN 题库
func (uc *ProblemUseCase) computeUserProfile(userID int64) (*UserProfileSnapshot, error) {
	snap := &UserProfileSnapshot{BuiltAt: time.Now().Unix()}

	// 雷达：写时预聚合 user_tag_ac
	if tags, err := dal.ListUserTagAC(context.Background(), uc.data.DB, userID, 20); err != nil {
		log.Errorf("radar preagg user=%d: %v", userID, err)
	} else {
		for _, t := range tags {
			// 掌握度 = 饱和锚定：score = 100 × weight/(weight+K)。
			// weight 已按难度加权；单题/低难度不再虚高（1 道简单题 ≈ 3.2 分）。
			score := 0.0
			if t.Weight > 0 {
				score = math.Round(100*t.Weight/(t.Weight+profileRadarK)*1000) / 1000
			}
			snap.Radar = append(snap.Radar, struct {
				Tag     string
				Score   float64
				ACCount int64
			}{Tag: t.Tag, Score: score, ACCount: t.Count})
		}
	}

	// 平台过题：读 user_ac_problems；力扣优先官方 acTotal 合成键（e:LeetCode:ac-*）
	// 牛客统一为 NowCoder（不拆竞赛站 / Tracker）
	if plats, e := dal.ListUserPlatformAC(uc.data.DB, userID); e != nil {
		log.Errorf("platforms sql user=%d: %v", userID, e)
	} else {
		for _, p := range plats {
			snap.Platforms = append(snap.Platforms, struct {
				Name  string
				Count int64
			}{p.Name, p.Count})
		}
	}

	// 难度分布：从 user_ac_problems（按 user_id 索引）驱动，
	// p:/e: 分支分别走 problems 主键 / (platform, external_id) 唯一索引，
	// 替代整表 Seq Scan + OR 字符串拼接 JOIN（高 AC 用户不再拖慢画像）。
	type nc struct {
		Name  string
		Count int64
	}
	var diffs []nc
	if e := uc.data.DB.Raw(`
		SELECT name, COUNT(DISTINCT pid) AS count
		FROM (
			SELECT p.id AS pid, p.difficulty AS name
			FROM user_ac_problems u
			JOIN problems p ON p.id = NULLIF(substring(u.problem_key, 3), '')::bigint
			WHERE u.user_id = ?
			  AND u.problem_key LIKE 'p:%'
			  AND u.problem_key ~ '^p:[0-9]+$'
			  AND p.difficulty IS NOT NULL AND BTRIM(p.difficulty) <> ''
			  AND UPPER(BTRIM(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
			UNION ALL
			SELECT p.id, p.difficulty
			FROM user_ac_problems u
			JOIN problems p
			  ON p.platform = split_part(substring(u.problem_key, 3), ':', 1)
			 AND p.external_id = substring(substring(u.problem_key, 3) FROM position(':' IN substring(u.problem_key, 3)) + 1)
			 AND p.external_id IS NOT NULL AND btrim(p.external_id) <> ''
			WHERE u.user_id = ?
			  AND u.problem_key LIKE 'e:%'
			  AND p.difficulty IS NOT NULL AND BTRIM(p.difficulty) <> ''
			  AND UPPER(BTRIM(p.difficulty)) NOT IN ('UNKNOWN','NULL','NONE')
		) t
		GROUP BY name
	`, userID, userID).Scan(&diffs).Error; e != nil {
		log.Errorf("difficulties sql user=%d: %v", userID, e)
	}
	for _, d := range diffs {
		snap.Difficulties = append(snap.Difficulties, struct {
			Name  string
			Count int64
		}{d.Name, d.Count})
	}

	// 生涯 total 与 period.ac.total / 平台合计一致：力扣用官方合成键
	if n, e := dal.CountUserLifetimeAC(uc.data.DB, userID); e != nil {
		log.Errorf("totalAC sql user=%d: %v", userID, e)
	} else {
		snap.TotalAC = n
	}

	return snap, nil
}
