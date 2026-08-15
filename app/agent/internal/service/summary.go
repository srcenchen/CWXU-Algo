package service

import (
	"context"
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
)

type SummaryService struct {
	rabbitMQ *event.RabbitMQ
	uc       *biz.SummaryUseCase
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

func NewSummaryService(data *data.Data, rabbitMQ *event.RabbitMQ, uc *biz.SummaryUseCase) *SummaryService {
	// 队列声明放启动期一次性做；失败仅告警（consumer 侧 DeclareOnMissing 兜底）
	if rabbitMQ != nil {
		if _, err := rabbitMQ.QueueDeclare(event.SummaryMailQueue, true, false, false, false, nil); err != nil {
			log.Warnf("NewSummaryService: QueueDeclare %s: %v", event.SummaryMailQueue, err)
		}
	}
	return &SummaryService{
		rabbitMQ: rabbitMQ,
		uc:       uc,
	}
}
