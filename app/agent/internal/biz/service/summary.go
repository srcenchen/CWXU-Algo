package service

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	profile2 "cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/agent/internal/agent"
	"cwxu-algo/app/agent/internal/data"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/openaiclient"
	"cwxu-algo/app/common/sitesettings"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/redis/go-redis/v9"
)

type SummaryUseCase struct {
	chat  *agent.Chat
	reg   *registry.Registrar
	redis *redis.Client
}

func NewSummaryUseCase(chat *agent.Chat, reg *discovery.Register, d *data.Data) *SummaryUseCase {
	return &SummaryUseCase{
		chat:  chat,
		reg:   &reg.Reg,
		redis: d.RDB,
	}
}

func (uc *SummaryUseCase) runtime(ctx context.Context) *sitesettings.Runtime {
	rt := sitesettings.Load(ctx, uc.redis, nil)
	return rt
}

func (uc *SummaryUseCase) brandTitle(ctx context.Context) string {
	t := strings.TrimSpace(uc.runtime(ctx).SiteTitle)
	if t == "" {
		return "GoAlgo"
	}
	return t
}

// PersonalLastDay 仅发个人日报（周报见 WeeklyStaff）
func (uc *SummaryUseCase) PersonalLastDay(userId int64) error {
	if !uc.canSendDailyEmail(userId) {
		log.Infof("用户 %d 日报未开启或无组织授权，跳过", userId)
		return nil
	}

	lockKey := fmt.Sprintf("agent:lock:summary:daily:%d", userId)
	if !uc.tryAcquireLock(context.Background(), lockKey, openaiclient.LLMCallTimeout) {
		log.Infof("用户 %d 日报生成进行中，跳过", userId)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), openaiclient.LLMCallTimeout)
	defer cancel()

	data, err := uc.loadDailyReportData(ctx, userId)
	if err != nil {
		// 瞬时失败：释放锁，MQ 重试可立即重进，不被 3 分钟锁静默吞掉
		uc.releaseLock(lockKey)
		return err
	}

	msgs := []agent.Message{
		{Role: "system", Content: dailySystemPrompt(data.Name)},
		{Role: "user", Content: dailyUserPrompt(data)},
	}
	brand := uc.brandTitle(ctx)
	// LLM 只输出评价文案参数，HTML 由统一模板渲染（数字全来自数据层，防幻觉/格式乱）
	html := ""
	raw, err := uc.chat.Complete(ctx, msgs)
	if err != nil {
		log.Warnf("用户 %d 日报 AI 失败: %v；回退规则模板", userId, err)
		html = RenderDailyRuleHTML(data, brand)
	} else {
		comment, cerr := ParseAIReportComment(raw)
		if cerr != nil {
			log.Warnf("用户 %d 日报 AI 输出无效: %v；回退规则模板", userId, cerr)
			html = RenderDailyRuleHTML(data, brand)
		} else {
			html = RenderDailyHTMLWithComment(data, brand, comment)
		}
	}
	if strings.TrimSpace(html) == "" {
		html = RenderDailyRuleHTML(data, brand)
	}

	subject := fmt.Sprintf("【%s 日报】%s · %s", brand, formatCNDate(data.Yesterday), data.Name)
	if err := uc.sendHTMLEmail(data.Email, subject, html); err != nil {
		uc.releaseLock(lockKey)
		return fmt.Errorf("发送日报失败: %w", err)
	}
	log.Infof("用户 %d 日报已发送至 %s", userId, data.Email)
	return nil
}

// PersonalDailyRule 仅发规则模板常规日报（不调 LLM，无 AI 成本）。
// 数据加载与 AI 版共用 loadDailyReportData；门槛沿用 canSendDailyEmail。
// AI 日报（Pro 订阅 + 套餐开启 + 个人开关）走 PersonalLastDay，普通用户走本方法。
func (uc *SummaryUseCase) PersonalDailyRule(userId int64) error {
	if !uc.canSendDailyEmail(userId) {
		log.Infof("用户 %d 日报未开启或无组织授权，跳过", userId)
		return nil
	}

	lockKey := fmt.Sprintf("agent:lock:summary:daily:rule:%d", userId)
	if !uc.tryAcquireLock(context.Background(), lockKey, 3*time.Minute) {
		log.Infof("用户 %d 日报生成进行中，跳过", userId)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	data, err := uc.loadDailyReportData(ctx, userId)
	if err != nil {
		// 瞬时失败：释放锁，MQ 重试可立即重进
		uc.releaseLock(lockKey)
		return err
	}

	brand := uc.brandTitle(ctx)
	html := RenderDailyRuleHTML(data, brand)
	if strings.TrimSpace(html) == "" {
		uc.releaseLock(lockKey)
		return fmt.Errorf("用户 %d 日报模板渲染为空", userId)
	}

	subject := fmt.Sprintf("【%s 日报】%s · %s", brand, formatCNDate(data.Yesterday), data.Name)
	if err := uc.sendHTMLEmail(data.Email, subject, html); err != nil {
		uc.releaseLock(lockKey)
		return fmt.Errorf("发送日报失败: %w", err)
	}
	log.Infof("用户 %d 常规日报(规则模板)已发送至 %s", userId, data.Email)
	return nil
}

// WeeklyStaff 组织教练/队长/组织管理员周报 = 上周训练报告（共享训练报告管道）
// 同组织同周期只生成一份文档，其余 staff 复用后各自发邮件。
func (uc *SummaryUseCase) WeeklyStaff(userId int64) error {
	if !uc.canSendWeeklyEmail(userId) {
		log.Infof("用户 %d 周报未开启或无组织 staff 授权，跳过", userId)
		return nil
	}

	// 锁 TTL 覆盖最大工作时长，防止未完成时锁先过期导致并发重跑。
	lockKey := fmt.Sprintf("agent:lock:summary:weekly:%d", userId)
	locked, err := uc.tryAcquireWeeklyLock(context.Background(), lockKey, openaiclient.LLMCallTimeout)
	if err != nil {
		return err
	}
	if !locked {
		log.Infof("用户 %d 周报发送进行中，跳过", userId)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), openaiclient.LLMCallTimeout)
	defer cancel()

	orgIDs := uc.userStaffOrgIDs(ctx, userId)
	if len(orgIDs) == 0 {
		uc.releaseLock(lockKey)
		return fmt.Errorf("用户 %d 无 staff 组织，无法生成周报", userId)
	}
	weekStart, weekEnd := LastWeekRange(time.Now())
	email := uc.userContactEmail(userId)
	if email == "" {
		uc.releaseLock(lockKey)
		return fmt.Errorf("用户 %d 未绑定邮箱", userId)
	}
	var lastErr error
	sent := 0
	for _, orgID := range orgIDs {
		alreadySent, err := uc.weeklyAlreadySent(ctx, userId, orgID, weekStart, weekEnd)
		if err != nil {
			lastErr = err
			continue
		}
		if alreadySent {
			continue
		}
		html, jobID, reused, err := uc.ensureSharedWeeklyReport(ctx, orgID, userId, weekStart, weekEnd)
		if err != nil {
			lastErr = err
			log.Warnf("weekly training report org=%d user=%d: %v", orgID, userId, err)
			continue
		}
		subject := fmt.Sprintf("【%s 周报】%s-%s", uc.brandTitle(ctx), formatCNDate(weekStart.Format(dateLayout)), formatCNDate(weekEnd.Format(dateLayout)))
		if jobID != "" {
			footer := fmt.Sprintf(
				`<div style="padding:12px 16px;font-size:12px;color:%s;border-top:1px solid %s;background:%s;">本周报即上周训练报告（简版），任务 %s，可在组织管理下载 HTML（24 小时内）。</div>`,
				mail.ColorMutedFg, mail.ColorBorder, mail.ColorCard, jobID,
			)
			html = mail.InjectBeforeBodyClose(html, footer)
		}
		if err := uc.sendHTMLEmail(email, subject, html); err != nil {
			lastErr = err
			log.Warnf("weekly email org=%d user=%d: %v", orgID, userId, err)
			continue
		}
		if err := uc.markWeeklySent(ctx, userId, orgID, weekStart, weekEnd); err != nil {
			log.Warnf("weekly sent marker org=%d user=%d: %v", orgID, userId, err)
			lastErr = err
			continue
		}
		sent++
		shareTag := "generated"
		if reused {
			shareTag = "shared"
		}
		log.Infof("用户 %d 周报(训练报告)已发送至 %s org=%d range=%s~%s job=%s mode=%s",
			userId, email, orgID, weekStart.Format(dateLayout), weekEnd.Format(dateLayout), jobID, shareTag)
	}
	if lastErr != nil {
		uc.releaseLock(lockKey)
		return lastErr
	}
	if sent == 0 {
		// 所有组织均已有本周期发送记录，MQ 重试可正常结束。
		for _, orgID := range orgIDs {
			alreadySent, err := uc.weeklyAlreadySent(ctx, userId, orgID, weekStart, weekEnd)
			if err != nil || !alreadySent {
				uc.releaseLock(lockKey)
				if err != nil {
					return err
				}
				return fmt.Errorf("用户 %d 周报未发送任何组织", userId)
			}
		}
		return nil
	}
	return nil
}

func weeklySentKey(userID, orgID int64, start, end time.Time) string {
	return fmt.Sprintf("agent:summary:weekly:sent:%d:%d:%s:%s", userID, orgID, start.Format(dateLayout), end.Format(dateLayout))
}

func (uc *SummaryUseCase) tryAcquireWeeklyLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := uc.redis.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("获取周报锁失败: %w", err)
	}
	return ok, nil
}

func (uc *SummaryUseCase) weeklyAlreadySent(ctx context.Context, userID, orgID int64, start, end time.Time) (bool, error) {
	ok, err := uc.redis.Exists(ctx, weeklySentKey(userID, orgID, start, end)).Result()
	if err != nil {
		return false, fmt.Errorf("查询周报发送记录失败 user=%d org=%d: %w", userID, orgID, err)
	}
	return ok > 0, nil
}

func (uc *SummaryUseCase) markWeeklySent(ctx context.Context, userID, orgID int64, start, end time.Time) error {
	markerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return uc.redis.Set(markerCtx, weeklySentKey(userID, orgID, start, end), "1", 14*24*time.Hour).Err()
}

func (uc *SummaryUseCase) userStaffOrgIDs(ctx context.Context, userId int64) []int64 {
	conn, err := uc.dialUser(ctx)
	if err != nil {
		return nil
	}
	cli := profile2.NewProfileClient(conn)
	res, err := cli.GetStaffOrgIds(ctx, &profile2.GetStaffOrgIdsReq{UserId: userId})
	if err != nil || res == nil {
		log.Warnf("GetStaffOrgIds user=%d: %v", userId, err)
		return nil
	}
	return res.GetOrgIds()
}

// ensureSharedWeeklyReport 同组织同周期周报只生成一次：命中已完成 job 则复用 HTML。
// 并发时用组织级锁；等不到锁则轮询共享产物，超时再自行生成兜底。
func (uc *SummaryUseCase) ensureSharedWeeklyReport(ctx context.Context, orgID, userID int64, start, end time.Time) (html string, jobID string, reused bool, err error) {
	startS := start.Format(dateLayout)
	endS := end.Format(dateLayout)

	if h, id, ok := uc.loadSharedWeeklyHTML(ctx, orgID, startS, endS); ok {
		return h, id, true, nil
	}

	// 等锁上限大幅缩短：单 worker 不能被 2s 轮询占死 7 分钟。
	// 30s 内等不到共享产物就自行生成兜底（可能与持锁方并存一份，可接受且不丢消息）。
	lockKey := fmt.Sprintf("agent:lock:summary:weekly:org:%d:%s:%s", orgID, startS, endS)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return "", "", false, err
		}
		if h, id, ok := uc.loadSharedWeeklyHTML(ctx, orgID, startS, endS); ok {
			return h, id, true, nil
		}
		if uc.tryAcquireLock(ctx, lockKey, 8*time.Minute) {
			// 双检：抢到锁后可能已被其他实例写好
			if h, id, ok := uc.loadSharedWeeklyHTML(ctx, orgID, startS, endS); ok {
				return h, id, true, nil
			}
			html, data, genErr := uc.GenerateTrainingReportSync(ctx, StartTrainingReportParams{
				OrgID:     orgID,
				GroupID:   0,
				StartDate: startS,
				EndDate:   endS,
				UseAI:     true,
				CreatedBy: userID,
				Source:    "weekly",
			})
			if genErr != nil {
				return "", "", false, genErr
			}
			id, perr := uc.persistWeeklyAsJob(ctx, orgID, userID, start, end, html, data)
			if perr != nil {
				log.Warnf("weekly persist job user=%d org=%d: %v", userID, orgID, perr)
				// 落盘失败仍把 HTML 发给本教练，但不标记 shared（下次会再生成）
				return html, "", false, nil
			}
			return html, id, false, nil
		}
		// 其他 staff 正在生成：等待共享产物
		if time.Now().After(deadline) {
			log.Warnf("weekly shared wait timeout org=%d range=%s~%s user=%d, generate fallback", orgID, startS, endS, userID)
			html, data, genErr := uc.GenerateTrainingReportSync(ctx, StartTrainingReportParams{
				OrgID:     orgID,
				GroupID:   0,
				StartDate: startS,
				EndDate:   endS,
				UseAI:     true,
				CreatedBy: userID,
				Source:    "weekly",
			})
			if genErr != nil {
				return "", "", false, genErr
			}
			// 超时兜底仍尝试落盘（可能与另一份并存，可接受）
			id, perr := uc.persistWeeklyAsJob(ctx, orgID, userID, start, end, html, data)
			if perr != nil {
				log.Warnf("weekly persist job (fallback) user=%d org=%d: %v", userID, orgID, perr)
				return html, "", false, nil
			}
			return html, id, false, nil
		}
		select {
		case <-ctx.Done():
			return "", "", false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// loadSharedWeeklyHTML 读取同组织同周期已完成且文件仍在的周报 job
func (uc *SummaryUseCase) loadSharedWeeklyHTML(ctx context.Context, orgID int64, startDate, endDate string) (html string, jobID string, ok bool) {
	job := uc.findSharedWeeklyJob(ctx, orgID, startDate, endDate)
	if job == nil {
		return "", "", false
	}
	b, err := os.ReadFile(job.HTMLPath)
	if err != nil || len(b) == 0 {
		return "", "", false
	}
	return string(b), job.JobID, true
}

// findSharedWeeklyJob 查找 source=weekly、全组织、同日期区间的已完成任务
func (uc *SummaryUseCase) findSharedWeeklyJob(ctx context.Context, orgID int64, startDate, endDate string) *TrainingReportJob {
	jobs, err := uc.listJobs(ctx, orgID, 50)
	if err != nil || len(jobs) == 0 {
		return nil
	}
	now := time.Now()
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if j.Source != "weekly" {
			continue
		}
		if j.StartDate != startDate || j.EndDate != endDate {
			continue
		}
		if j.GroupID != 0 {
			continue
		}
		if !j.IsDownloadable(now) {
			continue
		}
		if j.HTMLPath == "" {
			continue
		}
		if _, err := os.Stat(j.HTMLPath); err != nil {
			continue
		}
		return j
	}
	return nil
}

// persistWeeklyAsJob 将周报产物写入训练报告 job，便于下载
func (uc *SummaryUseCase) persistWeeklyAsJob(ctx context.Context, orgID, userID int64, start, end time.Time, html string, data *TrainingReportData) (string, error) {
	if err := ensureReportDir(); err != nil {
		return "", err
	}
	// 再次防重：落盘前若已有共享 job 则直接复用（避免超时兜底双写）
	startS := start.Format(dateLayout)
	endS := end.Format(dateLayout)
	if existing := uc.findSharedWeeklyJob(ctx, orgID, startS, endS); existing != nil {
		return existing.JobID, nil
	}
	jobID := newJobID()
	now := time.Now()
	htmlPath := jobHTMLPath(jobID)
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		return "", err
	}
	_ = data // 保留参数签名，便于后续扩展元数据
	job := &TrainingReportJob{
		JobID:      jobID,
		Status:     ReportStatusDone,
		Progress:   100,
		Message:    "周报已生成（组织共享）",
		StartDate:  startS,
		EndDate:    endS,
		UseAI:      true,
		OrgID:      orgID,
		CreatedBy:  userID,
		CreatedAt:  now.Unix(),
		FinishedAt: now.Unix(),
		ExpiresAt:  now.Add(reportDownloadTTL).Unix(),
		HTMLPath:   htmlPath,
		FileName:   fmt.Sprintf("weekly-report-%s-%s.html", startS, endS),
		Source:     "weekly",
	}
	if err := uc.saveJob(ctx, job, true); err != nil {
		return "", err
	}
	return jobID, nil
}

// WeeklyReportForCoach 兼容旧调用名
func (uc *SummaryUseCase) WeeklyReportForCoach(coachUserId int64) error {
	return uc.WeeklyStaff(coachUserId)
}

func formatCNDate(ymd string) string {
	t, err := time.Parse(dateLayout, ymd)
	if err != nil {
		return ymd
	}
	return t.Format("1月2日")
}
