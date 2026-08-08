package service

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	agenttool "cwxu-algo/app/agent/internal/agent/tool"
	"cwxu-algo/app/agent/internal/agent/tool/core_data"
	"cwxu-algo/app/common/mail"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
)

// 训练报告并发控制：AI + 数据聚合都很重（2c4g），全进程最多同时跑 2 个任务；
// 超出的任务保持 pending 排队。每组织在途任务数另设上限，防止单组织刷爆队列。
var trainingReportSem = make(chan struct{}, 2)

const maxActiveTrainingJobsPerOrg = 2

// StartTrainingReportParams 启动参数
type StartTrainingReportParams struct {
	OrgID     int64
	GroupID   int64
	SquadID   int64
	StartDate string
	EndDate   string
	UseAI     bool
	CreatedBy int64
	Source    string // manual | weekly
}

// GetTrainingReportJob 供 HTTP 层查询
func (uc *SummaryUseCase) GetTrainingReportJob(ctx context.Context, jobID string) (*TrainingReportJob, error) {
	return uc.getJob(ctx, jobID)
}

// ListTrainingReportJobs 供 HTTP 层列表
func (uc *SummaryUseCase) ListTrainingReportJobs(ctx context.Context, orgID, limit int64) ([]*TrainingReportJob, error) {
	return uc.listJobs(ctx, orgID, limit)
}

// StartTrainingReport 创建异步任务并后台执行，立即返回 jobId
func (uc *SummaryUseCase) StartTrainingReport(ctx context.Context, p StartTrainingReportParams) (string, error) {
	if p.OrgID <= 0 {
		return "", fmt.Errorf("缺少组织 id")
	}
	start, end, err := ParseDateRange(p.StartDate, p.EndDate)
	if err != nil {
		return "", err
	}
	if err := ValidateAIDateRange(start, end, p.UseAI); err != nil {
		return "", err
	}
	if err := ensureReportDir(); err != nil {
		return "", fmt.Errorf("创建报告目录失败: %w", err)
	}
	// 同组织同区间同分组防连点：进行中的任务直接复用
	if existing := uc.findActiveTrainingJob(ctx, p); existing != "" {
		return existing, nil
	}
	// 每组织在途任务上限：超限直接报错，提示稍后再试
	if n := uc.countActiveTrainingJobs(ctx, p.OrgID); n >= maxActiveTrainingJobsPerOrg {
		return "", fmt.Errorf("当前组织已有 %d 个报告任务在排队/生成中，请等其完成后再试", n)
	}
	jobID := newJobID()
	now := time.Now()
	job := &TrainingReportJob{
		JobID:     jobID,
		Status:    ReportStatusPending,
		Progress:  0,
		Message:   "排队中",
		StartDate: p.StartDate,
		EndDate:   p.EndDate,
		GroupID:   p.GroupID,
		SquadID:   p.SquadID,
		UseAI:     p.UseAI,
		OrgID:     p.OrgID,
		CreatedBy: p.CreatedBy,
		CreatedAt: now.Unix(),
		Source:    p.Source,
	}
	if job.Source == "" {
		job.Source = "manual"
	}
	if err := uc.saveJob(ctx, job, true); err != nil {
		return "", fmt.Errorf("保存任务失败: %w", err)
	}
	go uc.runTrainingReportJob(jobID)
	return jobID, nil
}

// findActiveTrainingJob 返回同参数仍在排队/运行的 jobId
func (uc *SummaryUseCase) findActiveTrainingJob(ctx context.Context, p StartTrainingReportParams) string {
	jobs, err := uc.listJobs(ctx, p.OrgID, 20)
	if err != nil || len(jobs) == 0 {
		return ""
	}
	src := p.Source
	if src == "" {
		src = "manual"
	}
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if j.Status != ReportStatusPending && j.Status != ReportStatusRunning {
			continue
		}
		if j.StartDate == p.StartDate && j.EndDate == p.EndDate &&
			j.GroupID == p.GroupID && j.SquadID == p.SquadID && j.UseAI == p.UseAI {
			js := j.Source
			if js == "" {
				js = "manual"
			}
			if js == src {
				return j.JobID
			}
		}
	}
	return ""
}

// countActiveTrainingJobs 组织当前排队/运行中的任务数
func (uc *SummaryUseCase) countActiveTrainingJobs(ctx context.Context, orgID int64) int {
	jobs, err := uc.listJobs(ctx, orgID, 20)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if j.Status == ReportStatusPending || j.Status == ReportStatusRunning {
			n++
		}
	}
	return n
}

func (uc *SummaryUseCase) runTrainingReportJob(jobID string) {
	// 全局并发限流：拿不到名额时排队（任务保持 pending「排队中」），10 分钟超时从拿到名额起算
	trainingReportSem <- struct{}{}
	defer func() { <-trainingReportSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			uc.failJob(ctx, jobID, fmt.Sprintf("panic: %v", r))
		}
	}()

	_ = uc.updateJob(ctx, jobID, func(j *TrainingReportJob) {
		j.Status = ReportStatusRunning
		j.Progress = 5
		j.Message = "加载数据中"
	})

	job, err := uc.getJob(ctx, jobID)
	if err != nil || job == nil {
		log.Errorf("training report job missing %s: %v", jobID, err)
		return
	}

	start, end, err := ParseDateRange(job.StartDate, job.EndDate)
	if err != nil {
		uc.failJob(ctx, jobID, err.Error())
		return
	}

	data, err := uc.LoadTrainingReportData(ctx, job.OrgID, job.GroupID, job.SquadID, job.CreatedBy, start, end)
	if err != nil {
		uc.failJob(ctx, jobID, "加载数据失败: "+err.Error())
		return
	}

	_ = uc.updateJob(ctx, jobID, func(j *TrainingReportJob) {
		j.Progress = 40
		j.Message = "生成报告中"
	})

	brand := uc.brandTitle(ctx)
	mode := DetailModeFromSource(job.Source)
	var html string
	if job.UseAI {
		html, err = uc.generateTrainingReportAI(ctx, data, mode)
		if err != nil {
			log.Warnf("training report AI failed job=%s: %v; fallback rule template", jobID, err)
			html = RenderRuleTemplateHTML(data, brand, mode)
			_ = uc.updateJob(ctx, jobID, func(j *TrainingReportJob) {
				j.Message = "AI 不可用，已用规则模板"
			})
		}
	} else {
		html = RenderRuleTemplateHTML(data, brand, mode)
	}
	html = stripCodeFence(html)
	if strings.TrimSpace(html) == "" {
		uc.failJob(ctx, jobID, "报告内容为空")
		return
	}

	htmlPath := jobHTMLPath(jobID)
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		uc.failJob(ctx, jobID, "写入 HTML 失败: "+err.Error())
		return
	}

	finished := time.Now()
	expires := finished.Add(reportDownloadTTL)
	fileName := fmt.Sprintf("training-report-%s-%s.html", job.StartDate, job.EndDate)

	// 就地更新本地 job 再落盘，保证 notify 使用含 ExpiresAt/FileName 的快照
	job.Status = ReportStatusDone
	job.Progress = 100
	job.Message = "已完成"
	job.FinishedAt = finished.Unix()
	job.ExpiresAt = expires.Unix()
	job.HTMLPath = htmlPath
	job.FileName = fileName
	if err := uc.saveJob(ctx, job, false); err != nil {
		uc.failJob(ctx, jobID, "保存完成状态失败: "+err.Error())
		return
	}

	// 再读一次，确保与 Redis 一致后再发邮件
	if fresh, err := uc.getJob(ctx, jobID); err == nil && fresh != nil {
		job = fresh
	}

	if err := uc.notifyTrainingReportDone(ctx, data, job, html); err != nil {
		log.Warnf("training report notify job=%s: %v", jobID, err)
	}
	log.Infof("training report done job=%s org=%d", jobID, job.OrgID)
}

func (uc *SummaryUseCase) failJob(_ context.Context, jobID, detail string) {
	// 调用方 ctx 可能已超时/取消：用独立短超时写 Redis，保证任务能落 failed 状态
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = uc.updateJob(wctx, jobID, func(j *TrainingReportJob) {
		j.Status = ReportStatusFailed
		j.Progress = 100
		j.Message = "失败"
		j.ErrorDetail = detail
		j.FinishedAt = time.Now().Unix()
	})
	log.Errorf("training report failed job=%s: %s", jobID, detail)
}

func (uc *SummaryUseCase) generateTrainingReportAI(ctx context.Context, data *TrainingReportData, mode string) (string, error) {
	if uc.chat == nil {
		return "", fmt.Errorf("chat 未初始化")
	}
	// 预置 JSON 已含全量统计：LLM 只输出评价文案参数，数字由模板渲染（防幻觉/格式乱）
	msgs := []*model.ChatCompletionMessage{
		{
			Role: model.ChatMessageRoleSystem,
			Content: &model.ChatCompletionMessageContent{
				StringValue: volcengine.String(trainingReportSystemPrompt(mode)),
			},
		},
		{
			Role: model.ChatMessageRoleUser,
			Content: &model.ChatCompletionMessageContent{
				StringValue: volcengine.String(trainingReportUserPrompt(data, mode)),
			},
		},
	}

	raw, err := uc.chat.Complete(ctx, msgs)
	if err != nil {
		return "", err
	}
	comment, cerr := ParseAIReportComment(raw)
	if cerr == nil {
		return RenderTemplateHTMLWithComment(data, uc.brandTitle(ctx), comment, mode), nil
	}
	log.Warnf("training report AI output invalid: %v; retry strict", cerr)

	// 严格重试：强调只输出 JSON
	retryMsgs := append(msgs, &model.ChatCompletionMessage{
		Role: model.ChatMessageRoleUser,
		Content: &model.ChatCompletionMessageContent{
			StringValue: volcengine.String("【重试】上次输出无法解析（" + cerr.Error() + "）。只输出 JSON 对象 {\"headline\":\"...\",\"highlights\":[],\"issues\":[],\"suggestions\":[]}，不要任何其它文字。"),
		},
	})
	raw2, err2 := uc.chat.Complete(ctx, retryMsgs)
	if err2 != nil {
		return "", fmt.Errorf("AI 重试失败: %w（首次校验: %v）", err2, cerr)
	}
	comment2, cerr2 := ParseAIReportComment(raw2)
	if cerr2 != nil {
		return "", fmt.Errorf("AI 输出校验失败: %v；重试仍失败: %v", cerr, cerr2)
	}
	return RenderTemplateHTMLWithComment(data, uc.brandTitle(ctx), comment2, mode), nil
}

// BuildNotifyEmail 纯函数：构造通知邮件主题/正文/附件名（可单测，不依赖 SMTP）。
// 正文为完整报告 HTML；页脚注入在 </body> 前，禁止拼到 </html> 外。
func BuildNotifyEmail(job *TrainingReportJob, brand, htmlDoc string) (subject, body, attachName string) {
	if brand == "" {
		brand = "GoAlgo"
	}
	start, end, id := "", "", ""
	var expiresAt int64
	if job != nil {
		start, end, id = job.StartDate, job.EndDate, job.JobID
		expiresAt = job.ExpiresAt
		attachName = strings.TrimSpace(job.FileName)
	}
	if attachName == "" {
		attachName = "training-report.html"
	}
	subject = fmt.Sprintf("【%s 训练报告】%s ~ %s 已生成", brand, start, end)
	expStr := "—"
	if expiresAt > 0 {
		expStr = time.Unix(expiresAt, 0).Format("2006-01-02 15:04")
	}
	footer := fmt.Sprintf(
		`<div style="padding:12px 16px;font-size:12px;color:%s;border-top:1px solid %s;background:%s;">任务 %s · 下载有效期至 %s（24 小时）· 产物为 HTML 报告（邮件正文与附件均为同一份）</div>`,
		mail.ColorMutedFg, mail.ColorBorder, mail.ColorCard,
		html.EscapeString(id), html.EscapeString(expStr),
	)

	body = strings.TrimSpace(htmlDoc)
	if body == "" {
		// 无报告正文：短通知卡片
		inner := fmt.Sprintf(
			`<p style="margin:0 0 12px;">您好，您发起的训练报告（%s ~ %s）已生成完成。</p>
<p style="margin:0 0 12px;">下载有效期 24 小时，请尽快在组织管理页下载 HTML，或查看本邮件附件。</p>
<p style="margin:0;font-size:12px;color:#737373;">任务 ID：%s</p>`,
			html.EscapeString(start), html.EscapeString(end), html.EscapeString(id),
		)
		body = mail.Wrap(mail.LayoutOpts{Brand: brand, Title: "训练报告已生成"}, inner)
		body = mail.InjectBeforeBodyClose(body, footer)
		return subject, body, attachName
	}
	// 确保完整文档 + 合法注入 footer
	if !mail.IsFullHTMLDocument(body) {
		body = mail.EnsureDocument(body)
	}
	body = mail.InjectBeforeBodyClose(body, footer)
	return subject, body, attachName
}

func (uc *SummaryUseCase) notifyTrainingReportDone(ctx context.Context, data *TrainingReportData, job *TrainingReportJob, html string) error {
	email := ""
	if data != nil {
		email = data.InitiatorEmail
	}
	if email == "" && job != nil && job.CreatedBy > 0 {
		email = uc.userContactEmail(job.CreatedBy)
	}
	if email == "" {
		return fmt.Errorf("发起人未绑定邮箱")
	}
	brand := uc.brandTitle(ctx)
	subject, body, attachName := BuildNotifyEmail(job, brand, html)

	rt := uc.runtime(ctx)
	sender := mail.NewSender(rt.SMTPConf())
	if !sender.Configured() {
		return fmt.Errorf("SMTP 未配置")
	}
	var atts []mail.Attachment
	if job != nil && job.HTMLPath != "" {
		if _, err := os.Stat(job.HTMLPath); err == nil {
			atts = append(atts, mail.Attachment{
				Filename:    attachName,
				Path:        job.HTMLPath,
				ContentType: "text/html; charset=utf-8",
			})
		}
	}
	return sender.SendWithAttachments(email, subject, body, atts)
}

// GenerateTrainingReportSync 同步生成（周报管道复用）：返回 HTML，并可选落盘 job
func (uc *SummaryUseCase) GenerateTrainingReportSync(ctx context.Context, p StartTrainingReportParams) (html string, data *TrainingReportData, err error) {
	start, end, err := ParseDateRange(p.StartDate, p.EndDate)
	if err != nil {
		return "", nil, err
	}
	if err := ValidateAIDateRange(start, end, p.UseAI); err != nil {
		return "", nil, err
	}
	data, err = uc.LoadTrainingReportData(ctx, p.OrgID, p.GroupID, p.SquadID, p.CreatedBy, start, end)
	if err != nil {
		return "", nil, err
	}
	brand := uc.brandTitle(ctx)
	mode := DetailModeFromSource(p.Source)
	if p.UseAI {
		html, err = uc.generateTrainingReportAI(ctx, data, mode)
		if err != nil {
			log.Warnf("weekly training AI fallback: %v", err)
			html = RenderRuleTemplateHTML(data, brand, mode)
			err = nil
		}
	} else {
		html = RenderRuleTemplateHTML(data, brand, mode)
	}
	html = stripCodeFence(html)
	return html, data, nil
}

// DomainAgentTools 训练/周报 AI 可用的域数据工具集。
// toolCtx 必须是 ContextWithElevatedAgent 结果，Bearer 会注入每个工具的 gRPC 调用。
func DomainAgentTools(reg *registry.Registrar, orgID uint, toolCtx context.Context) []agenttool.AgentToolFactory {
	if reg == nil {
		return nil
	}
	_ = orgID // org 已写入 elevated JWT claims
	ctx := toolCtx
	return []agenttool.AgentToolFactory{
		core_data.NewStatisticPeriod(reg, ctx),
		core_data.NewSubmitCnt(reg, ctx),
		core_data.NewSubmitLog(reg, ctx),
		core_data.NewGetProfileById(reg, ctx),
		core_data.NewRankTool(reg, ctx),
		core_data.NewHeatmapTool(reg, ctx),
		core_data.NewOrgMembersTool(reg, ctx),
		core_data.NewGroupMembersTool(reg, ctx),
		core_data.NewLastSubmitTool(reg, ctx),
		core_data.NewPeriodACTool(reg, ctx),
		core_data.NewProblemTagsTool(reg, ctx),
		// 组织博客 / 提交动态 / 比赛（列表·排行·详细榜）
		core_data.NewOrgBlogsTool(reg, ctx),
		core_data.NewOrgSubmitFeedTool(reg, ctx),
		core_data.NewContestListTool(reg, ctx),
		core_data.NewContestRankingTool(reg, ctx),
		core_data.NewContestBoardTool(reg, ctx),
		core_data.NewContestHistoryTool(reg, ctx),
	}
}

// DailyAgentTools 个人日报 AI 工具：标签 + 提交 + 热力 + 个人比赛。
func DailyAgentTools(reg *registry.Registrar, toolCtx context.Context) []agenttool.AgentToolFactory {
	if reg == nil {
		return nil
	}
	ctx := toolCtx
	return []agenttool.AgentToolFactory{
		core_data.NewProblemTagsTool(reg, ctx),
		core_data.NewSubmitLog(reg, ctx),
		core_data.NewHeatmapTool(reg, ctx),
		core_data.NewPeriodACTool(reg, ctx),
		core_data.NewContestHistoryTool(reg, ctx),
		core_data.NewContestListTool(reg, ctx),
		core_data.NewContestRankingTool(reg, ctx),
	}
}

// ToolAuthContexts 取出工具携带的 elevated context（测试用）
func ToolAuthContexts(tools []agenttool.AgentToolFactory) []context.Context {
	out := make([]context.Context, 0, len(tools))
	for _, t := range tools {
		type authC interface{ AuthContext() context.Context }
		if a, ok := t.(authC); ok {
			out = append(out, a.AuthContext())
		}
	}
	return out
}

// ResolveArtifactAbs 校验并返回可读 HTML 文件
func ResolveArtifactAbs(job *TrainingReportJob) (abs string, contentType string, name string, err error) {
	if job == nil {
		return "", "", "", fmt.Errorf("job nil")
	}
	if !job.IsDownloadable(time.Now()) {
		return "", "", "", fmt.Errorf("报告不存在或已过期")
	}
	if job.HTMLPath == "" {
		return "", "", "", fmt.Errorf("无可用 HTML 文件")
	}
	abs = job.HTMLPath
	contentType = "text/html; charset=utf-8"
	name = job.FileName
	if name == "" {
		name = filepath.Base(abs)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".html") {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".html"
	}
	abs = filepath.Clean(abs)
	base := filepath.Clean(reportDir())
	rel, err := filepath.Rel(base, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", "", fmt.Errorf("非法路径")
	}
	if _, err := os.Stat(abs); err != nil {
		return "", "", "", fmt.Errorf("文件不存在")
	}
	return abs, contentType, name, nil
}
