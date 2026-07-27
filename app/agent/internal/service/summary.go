package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"cwxu-algo/api/agent/v1/summary"
	biz "cwxu-algo/app/agent/internal/biz/service"
	"cwxu-algo/app/agent/internal/data"
	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
)

type SummaryService struct {
	rdb      *redis.Client
	rabbitMQ *event.RabbitMQ
	uc       *biz.SummaryUseCase
}

func (s SummaryService) GetRecentSummary(ctx context.Context, request *summary.GetSummaryRequest) (*summary.GetSummaryReply, error) {
	if request.UserId <= 0 || !auth.VerifySelfOrAbove(ctx, uint(request.UserId)) {
		return nil, errors.Forbidden("权限不足", "只能查看自己的 AI 总结")
	}
	key := fmt.Sprintf("agent:summary:%d:recent", request.UserId)
	val, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		// HTTP 路径异步入队：不 QueueDeclare（启动时已声明）、不同步等 confirm
		s.enqueueSummaryAsync(request.UserId, "PersonalRecent")
		return &summary.GetSummaryReply{
			Code: 1,
			Msg:  "嘿嘿，稍等稍等，您的 AI 分析报告马上就好(1-2min)",
			Resp: "",
		}, nil
	}
	if err != nil {
		return nil, errors.ServiceUnavailable("AI 总结暂不可用", "请稍后重试")
	}
	return &summary.GetSummaryReply{
		Code: 0,
		Msg:  "success",
		Resp: val,
	}, nil
}

func (s SummaryService) StartTrainingReport(ctx context.Context, req *summary.StartTrainingReportRequest) (*summary.StartTrainingReportReply, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return nil, errors.Unauthorized("未登录", "请先登录")
	}
	orgID := req.GetOrgId()
	if orgID <= 0 {
		orgID = int64(pd.OrgID)
	}
	if orgID <= 0 {
		return nil, errors.BadRequest("参数错误", "缺少组织 id")
	}
	// 细粒度权限：站管旁路；否则须在目标组织内具备训练报告权限
	if !auth.HasOrgPerm(ctx, uint(orgID), rbac.PermOrgReportView) {
		return nil, errors.Forbidden("权限不足", "无权导出该组织的训练报告")
	}
	if s.uc == nil {
		return nil, errors.ServiceUnavailable("服务未就绪", "训练报告服务不可用")
	}
	jobID, err := s.uc.StartTrainingReport(ctx, biz.StartTrainingReportParams{
		OrgID:     orgID,
		GroupID:   req.GetGroupId(),
		SquadID:   req.GetSquadId(),
		StartDate: req.GetStartDate(),
		EndDate:   req.GetEndDate(),
		UseAI:     req.GetUseAi(),
		CreatedBy: int64(pd.UserID),
		Source:    "manual",
	})
	if err != nil {
		return nil, errors.BadRequest("创建失败", err.Error())
	}
	return &summary.StartTrainingReportReply{
		Code:  0,
		Msg:   "任务已创建，后台生成中",
		JobId: jobID,
	}, nil
}

func (s SummaryService) GetTrainingReportJob(ctx context.Context, req *summary.GetTrainingReportJobRequest) (*summary.GetTrainingReportJobReply, error) {
	if s.uc == nil || req.GetJobId() == "" {
		return nil, errors.BadRequest("参数错误", "缺少 jobId")
	}
	job, err := s.uc.GetTrainingReportJob(ctx, req.GetJobId())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	if job == nil {
		return nil, errors.NotFound("不存在", "任务不存在或已清理")
	}
	// 细粒度权限：站管旁路；否则须在任务所属组织内具备训练报告权限
	if !auth.HasOrgPerm(ctx, uint(job.OrgID), rbac.PermOrgReportView) {
		return nil, errors.Forbidden("权限不足", "无权查看该组织任务")
	}
	return &summary.GetTrainingReportJobReply{
		Code: 0,
		Msg:  "ok",
		Job:  toProtoJob(job),
	}, nil
}

func (s SummaryService) ListTrainingReportJobs(ctx context.Context, req *summary.ListTrainingReportJobsRequest) (*summary.ListTrainingReportJobsReply, error) {
	pd := auth.GetCurrentUser(ctx)
	if pd == nil {
		return nil, errors.Unauthorized("未登录", "请先登录")
	}
	orgID := req.GetOrgId()
	if orgID <= 0 {
		orgID = int64(pd.OrgID)
	}
	// 细粒度权限：站管旁路；否则须在目标组织内具备训练报告权限
	if !auth.HasOrgPerm(ctx, uint(orgID), rbac.PermOrgReportView) {
		return nil, errors.Forbidden("权限不足", "无权查看该组织")
	}
	if s.uc == nil {
		return &summary.ListTrainingReportJobsReply{Code: 0, Msg: "ok"}, nil
	}
	jobs, err := s.uc.ListTrainingReportJobs(ctx, orgID, req.GetLimit())
	if err != nil {
		return nil, errors.InternalServer("内部错误", err.Error())
	}
	out := make([]*summary.TrainingReportJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toProtoJob(j))
	}
	return &summary.ListTrainingReportJobsReply{
		Code: 0,
		Msg:  "ok",
		Jobs: out,
	}, nil
}

func toProtoJob(j *biz.TrainingReportJob) *summary.TrainingReportJob {
	if j == nil {
		return nil
	}
	now := time.Now()
	st := j.EffectiveStatus(now)
	return &summary.TrainingReportJob{
		JobId:        j.JobID,
		Status:       st,
		Progress:     int64(j.Progress),
		Message:      j.Message,
		StartDate:    j.StartDate,
		EndDate:      j.EndDate,
		GroupId:      j.GroupID,
		SquadId:      j.SquadID,
		UseAi:        j.UseAI,
		OrgId:        j.OrgID,
		CreatedBy:    j.CreatedBy,
		CreatedAt:    j.CreatedAt,
		FinishedAt:   j.FinishedAt,
		ExpiresAt:    j.ExpiresAt,
		Downloadable: st == biz.ReportStatusDone && j.IsDownloadable(now),
		ErrorDetail:  j.ErrorDetail,
		FileName:     j.FileName,
	}
}

// RegisterTrainingReportDownload 文件下载（自定义路由）
func RegisterTrainingReportDownload(srv *khttp.Server, uc *biz.SummaryUseCase) {
	if srv == nil || uc == nil {
		return
	}
	r := srv.Route("/")
	r.GET("/v1/agent/training-report/download", func(ctx khttp.Context) error {
		jobID := ctx.Query().Get("jobId")
		if jobID == "" {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "缺少 jobId",
			})
		}
		job, err := uc.GetTrainingReportJob(ctx, jobID)
		if err != nil || job == nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "任务不存在",
			})
		}
		// 细粒度权限：站管旁路；否则须在任务所属组织内具备训练报告权限
		if !auth.HasOrgPerm(ctx, uint(job.OrgID), rbac.PermOrgReportView) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "无权下载该组织报告",
			})
		}
		abs, ct, name, err := biz.ResolveArtifactAbs(job)
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": err.Error(),
			})
		}
		f, err := os.Open(abs)
		if err != nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "文件不存在或已清理",
			})
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "读取失败",
			})
		}
		w := ctx.Response()
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		http.ServeContent(w, ctx.Request(), name, st.ModTime(), f)
		return nil
	})
}

// summaryPendingTTL 与 core_data 侧 SummaryTask 保持一致的去重窗口
const summaryPendingTTL = 20 * time.Minute

func summaryPendingKey(userId int64, typ string) string {
	return fmt.Sprintf("summary:pending:%s:%d", typ, userId)
}

// enqueueSummaryAsync HTTP 缓存 miss 路径的入队：Redis 去重后 PublishAsync，
// 绝不阻塞 HTTP（QueueDeclare 已在 NewSummaryService 启动时做过一次）。
func (s SummaryService) enqueueSummaryAsync(userId int64, typ string) {
	if s.rabbitMQ == nil {
		log.Errorf("enqueueSummaryAsync: mq not ready")
		return
	}
	if s.rdb != nil {
		ok, err := s.rdb.SetNX(context.Background(), summaryPendingKey(userId, typ), "1", summaryPendingTTL).Result()
		if err == nil && !ok {
			log.Debugf("enqueueSummaryAsync: dedup skip user=%d type=%s", userId, typ)
			return
		}
	}
	body, err := json.Marshal(event.SummaryEvent{UserId: userId, Type: typ})
	if err != nil {
		log.Errorf("enqueueSummaryAsync: json.Marshal failed: %v", err)
		return
	}
	s.rabbitMQ.PublishAsync("", "summary", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	})
}

func NewSummaryService(data *data.Data, rabbitMQ *event.RabbitMQ, uc *biz.SummaryUseCase) *SummaryService {
	// 队列声明放启动期一次性做；失败仅告警（consumer 侧 DeclareOnMissing 兜底）
	if rabbitMQ != nil {
		if _, err := rabbitMQ.QueueDeclare("summary", true, false, false, false, nil); err != nil {
			log.Warnf("NewSummaryService: QueueDeclare summary: %v", err)
		}
	}
	return &SummaryService{
		rdb:      data.RDB,
		rabbitMQ: rabbitMQ,
		uc:       uc,
	}
}
