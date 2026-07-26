package data

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/gorm/clause"
)

const visitTTL = 72 * time.Hour
const visitThrottle = 30 * time.Second
const visitIPMaxMembers = 2000 // 单日 IP 集合上限，防内存膨胀

var visitLoc *time.Location

func init() {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	visitLoc = loc
}

func visitDayKey(t time.Time) string {
	return t.In(visitLoc).Format("20060102")
}

func visitDayDate(t time.Time) time.Time {
	tt := t.In(visitLoc)
	return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, visitLoc)
}

// VisitRecord 一次访问上报结果
type VisitRecord struct {
	Counted bool
	Day     string // yyyy-MM-dd
}

// RecordVisit 记录访问：PV 节流；DAU 按 userId 精确集合；UV/IP 精确集合
func (d *Data) RecordVisit(ctx context.Context, userID uint, visitorID, clientIP, path string) (*VisitRecord, error) {
	if d == nil || d.RDB == nil {
		return &VisitRecord{}, nil
	}
	now := time.Now()
	day := visitDayKey(now)
	path = normalizeVisitPath(path)
	ts := now.Unix()

	throttleID := visitorThrottleID(userID, visitorID, clientIP)
	thKey := fmt.Sprintf("visit:th:%s:%s:%s", day, throttleID, path)
	ok, err := d.RDB.SetNX(ctx, thKey, "1", visitThrottle).Result()
	if err != nil {
		return nil, err
	}
	out := &VisitRecord{Day: now.In(visitLoc).Format("2006-01-02")}
	if !ok {
		_ = d.touchUniques(ctx, day, userID, visitorID, clientIP, path, ts)
		return out, nil
	}
	out.Counted = true

	pipe := d.RDB.Pipeline()
	pvKey := fmt.Sprintf("visit:pv:%s", day)
	pipe.Incr(ctx, pvKey)
	pipe.Expire(ctx, pvKey, visitTTL)

	if path != "" {
		pKey := fmt.Sprintf("visit:path:%s", day)
		pipe.HIncrBy(ctx, pKey, path, 1)
		pipe.Expire(ctx, pKey, visitTTL)
	}
	if userID > 0 {
		dauKey := fmt.Sprintf("visit:dau:%s", day)
		pipe.SAdd(ctx, dauKey, strconv.FormatUint(uint64(userID), 10))
		pipe.Expire(ctx, dauKey, visitTTL)
		month := now.In(visitLoc).Format("200601")
		mauKey := fmt.Sprintf("visit:mau:%s", month)
		pipe.SAdd(ctx, mauKey, strconv.FormatUint(uint64(userID), 10))
		pipe.Expire(ctx, mauKey, 40*24*time.Hour)
	}
	// 精确 UV 集合（visitor / user / ip）
	if m := uvMember(userID, visitorID, clientIP); m != "" {
		uvKey := fmt.Sprintf("visit:uvset:%s", day)
		pipe.SAdd(ctx, uvKey, m)
		pipe.Expire(ctx, uvKey, visitTTL)
	}
	// 精确独立 IP + 每 IP 的 PV / 最近路径
	if clientIP != "" {
		ipSet := fmt.Sprintf("visit:ipset:%s", day)
		pipe.SAdd(ctx, ipSet, clientIP)
		pipe.Expire(ctx, ipSet, visitTTL)
		ipPV := fmt.Sprintf("visit:ippv:%s", day)
		pipe.HIncrBy(ctx, ipPV, clientIP, 1)
		pipe.Expire(ctx, ipPV, visitTTL)
		ipMeta := fmt.Sprintf("visit:ipmeta:%s", day)
		// value: lastPath|unix
		pipe.HSet(ctx, ipMeta, clientIP, fmt.Sprintf("%s|%d", path, ts))
		pipe.Expire(ctx, ipMeta, visitTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	// 控制 IP 集合规模（极少见：超大量 IP 时裁剪最旧不处理，仅防异常）
	_ = d.trimIPSet(ctx, day)
	return out, nil
}

func (d *Data) touchUniques(ctx context.Context, day string, userID uint, visitorID, clientIP, path string, ts int64) error {
	pipe := d.RDB.Pipeline()
	if userID > 0 {
		dauKey := fmt.Sprintf("visit:dau:%s", day)
		pipe.SAdd(ctx, dauKey, strconv.FormatUint(uint64(userID), 10))
		pipe.Expire(ctx, dauKey, visitTTL)
		month := time.Now().In(visitLoc).Format("200601")
		mauKey := fmt.Sprintf("visit:mau:%s", month)
		pipe.SAdd(ctx, mauKey, strconv.FormatUint(uint64(userID), 10))
		pipe.Expire(ctx, mauKey, 40*24*time.Hour)
	}
	if m := uvMember(userID, visitorID, clientIP); m != "" {
		uvKey := fmt.Sprintf("visit:uvset:%s", day)
		pipe.SAdd(ctx, uvKey, m)
		pipe.Expire(ctx, uvKey, visitTTL)
	}
	if clientIP != "" {
		ipSet := fmt.Sprintf("visit:ipset:%s", day)
		pipe.SAdd(ctx, ipSet, clientIP)
		pipe.Expire(ctx, ipSet, visitTTL)
		ipMeta := fmt.Sprintf("visit:ipmeta:%s", day)
		pipe.HSet(ctx, ipMeta, clientIP, fmt.Sprintf("%s|%d", path, ts))
		pipe.Expire(ctx, ipMeta, visitTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (d *Data) trimIPSet(ctx context.Context, day string) error {
	ipSet := fmt.Sprintf("visit:ipset:%s", day)
	n, err := d.RDB.SCard(ctx, ipSet).Result()
	if err != nil || n <= visitIPMaxMembers {
		return err
	}
	// 超出时随机剔除一批（极端情况）
	drop := n - visitIPMaxMembers
	if drop > 200 {
		drop = 200
	}
	members, err := d.RDB.SRandMemberN(ctx, ipSet, drop).Result()
	if err != nil || len(members) == 0 {
		return err
	}
	args := make([]interface{}, len(members))
	for i, m := range members {
		args[i] = m
	}
	return d.RDB.SRem(ctx, ipSet, args...).Err()
}

// DayVisitStat 单日指标
type DayVisitStat struct {
	Date      string
	PV        int64
	DAU       int64
	UV        int64
	UniqueIP  int64
}

// PathVisitStat 页面 PV
type PathVisitStat struct {
	Path     string
	Category string
	PV       int64
	Share    float64
}

// CategoryVisitStat 服务/模块汇总
type CategoryVisitStat struct {
	Category string
	PV       int64
	Share    float64
}

// IPVisitItem 独立 IP 明细
type IPVisitItem struct {
	IP       string
	PV       int64
	LastPath string
	LastSeen int64
}

// GetDayVisitStat 优先 Redis，否则 PG
func (d *Data) GetDayVisitStat(ctx context.Context, day time.Time) DayVisitStat {
	key := visitDayKey(day)
	dateStr := day.In(visitLoc).Format("2006-01-02")
	st := DayVisitStat{Date: dateStr}

	if d.RDB != nil {
		if pv, err := d.RDB.Get(ctx, "visit:pv:"+key).Int64(); err == nil {
			st.PV = pv
		}
		if n, err := d.RDB.SCard(ctx, "visit:dau:"+key).Result(); err == nil {
			st.DAU = n
		}
		// 新键 uvset；兼容旧 pf
		if n, err := d.RDB.SCard(ctx, "visit:uvset:"+key).Result(); err == nil && n > 0 {
			st.UV = n
		} else if n, err := d.RDB.PFCount(ctx, "visit:uv:"+key).Result(); err == nil {
			st.UV = n
		}
		if n, err := d.RDB.SCard(ctx, "visit:ipset:"+key).Result(); err == nil {
			st.UniqueIP = n
		} else if n, err := d.RDB.PFCount(ctx, "visit:ip:"+key).Result(); err == nil {
			st.UniqueIP = n
		}
		if st.PV > 0 || st.DAU > 0 || st.UV > 0 || st.UniqueIP > 0 {
			return st
		}
		if visitDayKey(time.Now()) == key {
			return st
		}
	}

	var row model.SiteVisitDaily
	dayDate := visitDayDate(day)
	if d.DB != nil {
		if err := d.DB.WithContext(ctx).Where("day = ?", dayDate).First(&row).Error; err == nil {
			st.PV = row.PV
			st.DAU = row.DAU
			st.UV = row.UV
			st.UniqueIP = row.UniqueIP
		}
	}
	return st
}

// ListTopPaths 今日热门页面
func (d *Data) ListTopPaths(ctx context.Context, day time.Time, limit int) []PathVisitStat {
	if limit < 1 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	key := visitDayKey(day)
	if d.RDB == nil {
		return nil
	}
	m, err := d.RDB.HGetAll(ctx, "visit:path:"+key).Result()
	if err != nil || len(m) == 0 {
		return nil
	}
	type kv struct {
		path string
		pv   int64
	}
	list := make([]kv, 0, len(m))
	var total int64
	for p, v := range m {
		n, _ := strconv.ParseInt(v, 10, 64)
		if n <= 0 {
			continue
		}
		list = append(list, kv{path: p, pv: n})
		total += n
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].pv == list[j].pv {
			return list[i].path < list[j].path
		}
		return list[i].pv > list[j].pv
	})
	if len(list) > limit {
		list = list[:limit]
	}
	out := make([]PathVisitStat, 0, len(list))
	for _, it := range list {
		share := 0.0
		if total > 0 {
			share = float64(it.pv) * 100 / float64(total)
		}
		out = append(out, PathVisitStat{
			Path:     it.path,
			Category: categorizePath(it.path),
			PV:       it.pv,
			Share:    share,
		})
	}
	return out
}

// ListCategoryStats 按服务/模块汇总今日 PV
func (d *Data) ListCategoryStats(ctx context.Context, day time.Time) []CategoryVisitStat {
	key := visitDayKey(day)
	if d.RDB == nil {
		return nil
	}
	m, err := d.RDB.HGetAll(ctx, "visit:path:"+key).Result()
	if err != nil || len(m) == 0 {
		return nil
	}
	agg := map[string]int64{}
	var total int64
	for p, v := range m {
		n, _ := strconv.ParseInt(v, 10, 64)
		if n <= 0 {
			continue
		}
		cat := categorizePath(p)
		agg[cat] += n
		total += n
	}
	type kv struct {
		cat string
		pv  int64
	}
	list := make([]kv, 0, len(agg))
	for c, n := range agg {
		list = append(list, kv{cat: c, pv: n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].pv == list[j].pv {
			return list[i].cat < list[j].cat
		}
		return list[i].pv > list[j].pv
	})
	out := make([]CategoryVisitStat, 0, len(list))
	for _, it := range list {
		share := 0.0
		if total > 0 {
			share = float64(it.pv) * 100 / float64(total)
		}
		out = append(out, CategoryVisitStat{Category: it.cat, PV: it.pv, Share: share})
	}
	return out
}

// ListIPItems 今日独立 IP 明细
func (d *Data) ListIPItems(ctx context.Context, day time.Time, limit int) []IPVisitItem {
	if limit < 1 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	key := visitDayKey(day)
	if d.RDB == nil {
		return nil
	}
	ips, err := d.RDB.SMembers(ctx, "visit:ipset:"+key).Result()
	if err != nil || len(ips) == 0 {
		return nil
	}
	pvMap, _ := d.RDB.HGetAll(ctx, "visit:ippv:"+key).Result()
	metaMap, _ := d.RDB.HGetAll(ctx, "visit:ipmeta:"+key).Result()
	out := make([]IPVisitItem, 0, len(ips))
	for _, ip := range ips {
		item := IPVisitItem{IP: ip}
		if v, ok := pvMap[ip]; ok {
			item.PV, _ = strconv.ParseInt(v, 10, 64)
		}
		if meta, ok := metaMap[ip]; ok {
			parts := strings.SplitN(meta, "|", 2)
			if len(parts) >= 1 {
				item.LastPath = parts[0]
			}
			if len(parts) == 2 {
				item.LastSeen, _ = strconv.ParseInt(parts[1], 10, 64)
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PV == out[j].PV {
			return out[i].IP < out[j].IP
		}
		return out[i].PV > out[j].PV
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// FlushVisitDay Redis → PG 固化某日
func (d *Data) FlushVisitDay(ctx context.Context, day time.Time) error {
	if d == nil || d.DB == nil || d.RDB == nil {
		return nil
	}
	st := d.GetDayVisitStat(ctx, day)
	if st.PV == 0 && st.DAU == 0 && st.UV == 0 && st.UniqueIP == 0 {
		return nil
	}
	row := model.SiteVisitDaily{
		Day:      visitDayDate(day),
		PV:       st.PV,
		DAU:      st.DAU,
		UV:       st.UV,
		UniqueIP: st.UniqueIP,
	}
	return d.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "day"}},
		DoUpdates: clause.AssignmentColumns([]string{"pv", "dau", "uv", "unique_ip", "updated_at"}),
	}).Create(&row).Error
}

// CountRegisteredUsers 注册用户总数
func (d *Data) CountRegisteredUsers(ctx context.Context) int64 {
	if d == nil || d.DB == nil {
		return 0
	}
	var n int64
	_ = d.DB.WithContext(ctx).Model(&model.User{}).Count(&n)
	return n
}

// CountRegistrationsByDay 近 days 天（含今天）每日新注册用户数，key 为 yyyy-MM-dd（Asia/Shanghai）
func (d *Data) CountRegistrationsByDay(ctx context.Context, days int) map[string]int64 {
	out := make(map[string]int64)
	if d == nil || d.DB == nil {
		return out
	}
	if days < 1 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	now := time.Now().In(visitLoc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, visitLoc).
		AddDate(0, 0, -(days - 1))

	type row struct {
		Day string
		Cnt int64
	}
	var rows []row
	// 按上海时区自然日聚合；兼容 timestamptz / timestamp
	err := d.DB.WithContext(ctx).Raw(`
		SELECT to_char((created_at AT TIME ZONE 'Asia/Shanghai'), 'YYYY-MM-DD') AS day,
		       COUNT(*)::bigint AS cnt
		FROM users
		WHERE created_at >= ?
		GROUP BY 1
	`, start).Scan(&rows).Error
	if err != nil {
		// 回退：拉时间戳后在内存按日统计
		var created []time.Time
		if e := d.DB.WithContext(ctx).Model(&model.User{}).
			Where("created_at >= ?", start).
			Pluck("created_at", &created).Error; e != nil {
			return out
		}
		for _, t := range created {
			k := t.In(visitLoc).Format("2006-01-02")
			out[k]++
		}
		return out
	}
	for _, r := range rows {
		if r.Day != "" {
			out[r.Day] = r.Cnt
		}
	}
	return out
}

// CountMAU 当月活跃用户
func (d *Data) CountMAU(ctx context.Context) int64 {
	if d == nil || d.RDB == nil {
		return 0
	}
	month := time.Now().In(visitLoc).Format("200601")
	n, err := d.RDB.SCard(ctx, "visit:mau:"+month).Result()
	if err != nil {
		return 0
	}
	return n
}

// ListVisitSeries 近 days 天（含今天）：PG 按 day BETWEEN 一次查出后内存补零，
// 缺行（通常只有今天）才回落 Redis 单日查询。
func (d *Data) ListVisitSeries(ctx context.Context, days int) []DayVisitStat {
	if days < 1 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	_ = d.FlushVisitDay(ctx, time.Now().AddDate(0, 0, -1))

	now := time.Now()
	byDay := map[string]model.SiteVisitDaily{}
	if d.DB != nil {
		start := visitDayDate(now.AddDate(0, 0, -(days - 1)))
		end := visitDayDate(now)
		var rows []model.SiteVisitDaily
		if err := d.DB.WithContext(ctx).
			Where("day BETWEEN ? AND ?", start, end).
			Find(&rows).Error; err == nil {
			for _, r := range rows {
				byDay[r.Day.In(visitLoc).Format("2006-01-02")] = r
			}
		}
	}

	out := make([]DayVisitStat, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i)
		dateStr := day.In(visitLoc).Format("2006-01-02")
		if row, ok := byDay[dateStr]; ok {
			out = append(out, DayVisitStat{
				Date:     dateStr,
				PV:       row.PV,
				DAU:      row.DAU,
				UV:       row.UV,
				UniqueIP: row.UniqueIP,
			})
			continue
		}
		// 无落库行：今天走 Redis 实时值，历史缺日补零（GetDayVisitStat 查不到时即为零值）
		out = append(out, d.GetDayVisitStat(ctx, day))
	}
	return out
}

// categorizePath 将前端路由归类为服务/模块（审核与运营可读）
func categorizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "首页"
	}
	switch {
	case strings.HasPrefix(p, "/admin"):
		return "管理后台"
	case strings.HasPrefix(p, "/question-bank"), strings.HasPrefix(p, "/problem"):
		return "题库"
	case strings.HasPrefix(p, "/contest"):
		return "比赛"
	case strings.HasPrefix(p, "/bulletin"):
		return "公告"
	case strings.HasPrefix(p, "/all-activities"):
		return "动态"
	case strings.HasPrefix(p, "/profile"), strings.HasPrefix(p, "/change-profile"):
		return "个人中心"
	case strings.HasPrefix(p, "/tools"), strings.HasPrefix(p, "/p/"):
		return "工具"
	case strings.HasPrefix(p, "/org"):
		return "组织"
	case strings.HasPrefix(p, "/about"):
		return "关于"
	case strings.HasPrefix(p, "/login"), strings.HasPrefix(p, "/register"), strings.HasPrefix(p, "/forgot-password"), strings.HasPrefix(p, "/change-password"):
		return "账号"
	default:
		return "其他"
	}
}

func visitorThrottleID(userID uint, visitorID, clientIP string) string {
	if userID > 0 {
		return "u:" + strconv.FormatUint(uint64(userID), 10)
	}
	if v := strings.TrimSpace(visitorID); v != "" {
		if len(v) > 64 {
			v = v[:64]
		}
		return "v:" + v
	}
	if clientIP != "" {
		return "ip:" + clientIP
	}
	return "anon"
}

func uvMember(userID uint, visitorID, clientIP string) string {
	if userID > 0 {
		return "u:" + strconv.FormatUint(uint64(userID), 10)
	}
	if v := strings.TrimSpace(visitorID); v != "" {
		if len(v) > 64 {
			v = v[:64]
		}
		return "v:" + v
	}
	if clientIP != "" {
		return "ip:" + clientIP
	}
	return ""
}

func normalizeVisitPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 200 {
		p = p[:200]
	}
	if strings.Contains(p, "..") {
		return "/"
	}
	return p
}
