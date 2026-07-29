package dal

import (
	"cwxu-algo/app/common/utils/sqllike"
	"fmt"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	calspider "cwxu-algo/app/core_data/internal/spider/calendar"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ContestCalendarDal struct {
	db *gorm.DB
}

func NewContestCalendarDal(d *data.Data) *ContestCalendarDal {
	return &ContestCalendarDal{db: d.DB}
}

// NewContestCalendarDalDB 供 cron 等仅持有 *gorm.DB 的场景
func NewContestCalendarDalDB(db *gorm.DB) *ContestCalendarDal {
	return &ContestCalendarDal{db: db}
}

// UpsertItems 按 (platform, external_id) 写入/更新
func (d *ContestCalendarDal) UpsertItems(items []calspider.Item) (int, error) {
	if d.db == nil || len(items) == 0 {
		return 0, nil
	}
	rows := make([]model.ContestCalendar, 0, len(items))
	now := time.Now()
	for _, it := range items {
		if it.Platform == "" || it.ExternalID == "" {
			continue
		}
		// 与参赛记录/爬虫 platform 对齐（AtCoder 而非 atcoder），避免站内榜查日历 miss
		plat := calspider.NormalizePlatform(it.Platform)
		platName := strings.TrimSpace(it.PlatformName)
		if platName == "" {
			platName = plat
		}
		rows = append(rows, model.ContestCalendar{
			Platform:     plat,
			PlatformName: platName,
			ExternalID:   it.ExternalID,
			Name:         it.Name,
			URL:          it.URL,
			StartTime:    it.StartTime,
			EndTime:      it.EndTime,
			Source:       it.Source,
			IconURL:      it.IconURL,
			UpdatedAt:    now,
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	err := d.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "platform"}, {Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"platform_name", "name", "url", "start_time", "end_time", "source", "icon_url", "updated_at",
		}),
	}).CreateInBatches(&rows, 50).Error
	if err != nil {
		return 0, err
	}
	// 历史脏数据：platform 小写 atcoder 等，与爬虫 AtCoder 并存时优先规范名
	_ = d.NormalizeLegacyPlatformNames()
	return len(rows), nil
}

// legacyPlatformPairs cpolar 小写 id → 爬虫规范名
var legacyPlatformPairs = []struct{ from, to string }{
	{"atcoder", "AtCoder"},
	{"codeforces", "CodeForces"},
	{"nowcoder", "NowCoder"},
	{"leetcode", "LeetCode"},
	{"luogu", "LuoGu"},
	{"qoj", "QOJ"},
}

// NormalizeLegacyPlatformNames 将日历 platform 从 cpolar 小写 id 迁到爬虫常量名。
// 若规范名行已存在则删掉小写重复行。同步修正订阅表 platform 字段。
func (d *ContestCalendarDal) NormalizeLegacyPlatformNames() error {
	if d.db == nil {
		return nil
	}
	for _, p := range legacyPlatformPairs {
		// 无冲突：直接改名
		if err := d.db.Exec(`
			UPDATE contest_calendars AS c
			SET platform = ?
			WHERE platform = ?
			  AND NOT EXISTS (
			    SELECT 1 FROM contest_calendars x
			    WHERE x.platform = ? AND x.external_id = c.external_id
			  )`, p.to, p.from, p.to).Error; err != nil {
			return err
		}
		// 有冲突：丢掉小写重复
		if err := d.db.Where("platform = ?", p.from).Delete(&model.ContestCalendar{}).Error; err != nil {
			return err
		}
		// 订阅表：platform 级 / contest 级均对齐规范名（通知 byPlatform 按规范名匹配）
		if err := d.db.Exec(`
			UPDATE contest_calendar_subs
			SET platform = ?
			WHERE platform = ?`, p.to, p.from).Error; err != nil {
			return err
		}
	}
	return nil
}

// CleanupEnded 删除 end_time 早于 keepBeforeUnix 的赛程（默认保留刚结束 7 天）
func (d *ContestCalendarDal) CleanupEnded(keepBeforeUnix int64) (int64, error) {
	res := d.db.Where("end_time < ?", keepBeforeUnix).Delete(&model.ContestCalendar{})
	return res.RowsAffected, res.Error
}

type CalendarListQuery struct {
	Platform string
	Keyword  string
	Status   string // upcoming | ongoing | ended | all
	TimeFrom int64  // 开赛时间下界（unix 秒，含）；0=不限
	TimeTo   int64  // 开赛时间上界（unix 秒，含）；0=不限
	Limit    int
	Offset   int
}

func (d *ContestCalendarDal) List(q CalendarListQuery) ([]model.ContestCalendar, int64, error) {
	now := time.Now().Unix()
	db := d.db.Model(&model.ContestCalendar{})
	if p := strings.TrimSpace(q.Platform); p != "" {
		// 与入库一致：AtCoder / CodeForces …，禁止 ToLower（会与规范名失配）
		db = db.Where("platform = ?", calspider.NormalizePlatform(p))
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		db = db.Where("name ILIKE ?", sqllike.Pattern(kw))
	}
	switch strings.ToLower(strings.TrimSpace(q.Status)) {
	case "ongoing":
		db = db.Where("start_time <= ? AND end_time > ?", now, now)
	case "ended":
		db = db.Where("end_time <= ?", now)
	case "all":
		// no status time filter
	default: // upcoming
		db = db.Where("start_time > ?", now)
	}
	// 自定义时间窗：按开赛时间 start_time 过滤（与 status 叠加）
	if q.TimeFrom > 0 {
		db = db.Where("start_time >= ?", q.TimeFrom)
	}
	if q.TimeTo > 0 {
		db = db.Where("start_time <= ?", q.TimeTo)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	var list []model.ContestCalendar
	// 已结束 / 全部：新近在前；即将开始 / 进行中：开赛近的在前
	order := "start_time ASC"
	st := strings.ToLower(strings.TrimSpace(q.Status))
	if st == "ended" || st == "all" {
		order = "start_time DESC"
	}
	err := db.Order(order).Limit(limit).Offset(offset).Find(&list).Error
	return list, total, err
}

type PlatformStat struct {
	Platform     string
	PlatformName string
	IconURL      string
	Count        int64
}

func (d *ContestCalendarDal) ListPlatforms(upcomingOnly bool) ([]PlatformStat, error) {
	now := time.Now().Unix()
	db := d.db.Model(&model.ContestCalendar{})
	if upcomingOnly {
		db = db.Where("start_time > ?", now)
	}
	type row struct {
		Platform     string
		PlatformName string
		IconURL      string
		Count        int64
	}
	var rows []row
	err := db.Select("platform, MAX(platform_name) as platform_name, MAX(icon_url) as icon_url, COUNT(*) as count").
		Group("platform").
		Order("platform ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]PlatformStat, 0, len(rows))
	for _, r := range rows {
		out = append(out, PlatformStat{
			Platform:     r.Platform,
			PlatformName: r.PlatformName,
			IconURL:      r.IconURL,
			Count:        r.Count,
		})
	}
	return out, nil
}

func (d *ContestCalendarDal) GetByID(id uint) (*model.ContestCalendar, error) {
	var m model.ContestCalendar
	err := d.db.First(&m, id).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (d *ContestCalendarDal) ListSubsByUser(userID int64) ([]model.ContestCalendarSub, error) {
	var list []model.ContestCalendarSub
	err := d.db.Where("user_id = ?", userID).Order("scope ASC, platform ASC, calendar_id ASC").Find(&list).Error
	return list, err
}

// UpsertSub 创建或更新订阅。
// created=true 表示新建；prevEnabled / prevAdvance 为更新前状态（新建时 false / 0）。
func (d *ContestCalendarDal) UpsertSub(sub *model.ContestCalendarSub) (created bool, prevEnabled bool, prevAdvance int, err error) {
	if sub == nil {
		return false, false, 0, fmt.Errorf("nil sub")
	}
	// 用唯一键查找后更新或创建
	var existing model.ContestCalendarSub
	err = d.db.Where(
		"user_id = ? AND scope = ? AND platform = ? AND calendar_id = ?",
		sub.UserID, sub.Scope, sub.Platform, sub.CalendarID,
	).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		if e := d.db.Create(sub).Error; e != nil {
			return false, false, 0, e
		}
		return true, false, 0, nil
	}
	if err != nil {
		return false, false, 0, err
	}
	prevEnabled = existing.Enabled
	prevAdvance = existing.AdvanceMinutes
	existing.AdvanceMinutes = sub.AdvanceMinutes
	existing.Enabled = sub.Enabled
	existing.Platform = sub.Platform
	if e := d.db.Save(&existing).Error; e != nil {
		return false, prevEnabled, prevAdvance, e
	}
	return false, prevEnabled, prevAdvance, nil
}

func (d *ContestCalendarDal) DeleteSub(userID int64, scope, platform string, calendarID uint) error {
	q := d.db.Where("user_id = ? AND scope = ?", userID, scope)
	if scope == model.CalScopePlatform {
		q = q.Where("platform = ?", platform)
	} else {
		q = q.Where("calendar_id = ?", calendarID)
	}
	return q.Delete(&model.ContestCalendarSub{}).Error
}

// ListEnabledSubs 通知扫描用（enabled=true）
func (d *ContestCalendarDal) ListEnabledSubs() ([]model.ContestCalendarSub, error) {
	var list []model.ContestCalendarSub
	err := d.db.Where("enabled = ?", true).Find(&list).Error
	return list, err
}

// ListMutedContestSubs 本场静默：scope=contest 且 enabled=false，覆盖平台订阅
func (d *ContestCalendarDal) ListMutedContestSubs() ([]model.ContestCalendarSub, error) {
	var list []model.ContestCalendarSub
	err := d.db.Where("scope = ? AND enabled = ? AND calendar_id > 0", model.CalScopeContest, false).
		Find(&list).Error
	return list, err
}

// ListUpcomingInWindow start_time in (now, now+maxAdvance]
func (d *ContestCalendarDal) ListUpcomingInWindow(nowUnix, maxAdvanceSec int64) ([]model.ContestCalendar, error) {
	var list []model.ContestCalendar
	err := d.db.Where("start_time > ? AND start_time <= ?", nowUnix, nowUnix+maxAdvanceSec).
		Order("start_time ASC").
		Find(&list).Error
	return list, err
}

func (d *ContestCalendarDal) HasNotifyLog(userID int64, calendarID uint, advance int) (bool, error) {
	var n int64
	err := d.db.Model(&model.ContestCalendarNotifyLog{}).
		Where("user_id = ? AND calendar_id = ? AND advance_minutes = ?", userID, calendarID, advance).
		Count(&n).Error
	return n > 0, err
}

func (d *ContestCalendarDal) CreateNotifyLog(userID int64, calendarID uint, advance int) error {
	return d.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ContestCalendarNotifyLog{
		UserID:         userID,
		CalendarID:     calendarID,
		AdvanceMinutes: advance,
		SentAt:         time.Now(),
	}).Error
}

// TryClaimNotifyLog 原子占坑：插入成功则本进程负责发送；冲突则已发过/他人在发。
// 发送失败时调用 DeleteNotifyLog 释放，供后续 cron 重试。
func (d *ContestCalendarDal) TryClaimNotifyLog(userID int64, calendarID uint, advance int) (claimed bool, err error) {
	res := d.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ContestCalendarNotifyLog{
		UserID:         userID,
		CalendarID:     calendarID,
		AdvanceMinutes: advance,
		SentAt:         time.Now(),
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeleteNotifyLog 释放发送占坑（SMTP 失败时允许下次重试）
func (d *ContestCalendarDal) DeleteNotifyLog(userID int64, calendarID uint, advance int) error {
	return d.db.Where(
		"user_id = ? AND calendar_id = ? AND advance_minutes = ?",
		userID, calendarID, advance,
	).Delete(&model.ContestCalendarNotifyLog{}).Error
}

// CleanupNotifyLogs 清理很久以前的日志
func (d *ContestCalendarDal) CleanupNotifyLogs(before time.Time) (int64, error) {
	res := d.db.Where("sent_at < ?", before).Delete(&model.ContestCalendarNotifyLog{})
	return res.RowsAffected, res.Error
}
