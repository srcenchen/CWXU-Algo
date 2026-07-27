package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	ReportStatusPending  = "pending"
	ReportStatusRunning  = "running"
	ReportStatusDone     = "done"
	ReportStatusFailed   = "failed"
	ReportStatusExpired  = "expired"

	reportJobTTL       = 48 * time.Hour // redis 保留略长于下载窗
	reportDownloadTTL  = 24 * time.Hour
	reportJobKeyPrefix = "agent:training_report:job:"
	reportOrgIndexKey  = "agent:training_report:org:%d:jobs"
	reportDirEnv       = "GOALGO_TRAINING_REPORT_DIR"
)

// TrainingReportJob 异步训练报告任务元数据（Redis）
type TrainingReportJob struct {
	JobID       string `json:"jobId"`
	Status      string `json:"status"`
	Progress    int    `json:"progress"`
	Message     string `json:"message"`
	StartDate   string `json:"startDate"`
	EndDate     string `json:"endDate"`
	GroupID     int64  `json:"groupId"`
	SquadID     int64  `json:"squadId"`
	UseAI       bool   `json:"useAi"`
	OrgID       int64  `json:"orgId"`
	CreatedBy   int64  `json:"createdBy"`
	CreatedAt   int64  `json:"createdAt"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
	ErrorDetail string `json:"errorDetail,omitempty"`
	HTMLPath string `json:"htmlPath,omitempty"`
	FileName string `json:"fileName,omitempty"`
	// Source: manual | weekly
	Source string `json:"source,omitempty"`
}

func reportDir() string {
	if v := strings.TrimSpace(os.Getenv(reportDirEnv)); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "goalgo-training-reports")
}

func ensureReportDir() error {
	return os.MkdirAll(reportDir(), 0o755)
}

func jobRedisKey(id string) string {
	return reportJobKeyPrefix + id
}

func orgJobsKey(orgID int64) string {
	return fmt.Sprintf(reportOrgIndexKey, orgID)
}

func newJobID() string {
	return uuid.NewString()
}

// saveJob 写入任务元数据。indexInOrgList=true 时才写入组织任务索引（仅创建时一次）。
// 注意：进度/完成更新必须 indexInOrgList=false，否则 LPush 会把同一 job 刷出多条。
func (uc *SummaryUseCase) saveJob(ctx context.Context, job *TrainingReportJob, indexInOrgList bool) error {
	if uc.redis == nil || job == nil || job.JobID == "" {
		return fmt.Errorf("redis or job invalid")
	}
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	pipe := uc.redis.Pipeline()
	pipe.Set(ctx, jobRedisKey(job.JobID), string(b), reportJobTTL)
	if indexInOrgList && job.OrgID > 0 {
		pipe.LPush(ctx, orgJobsKey(job.OrgID), job.JobID)
		pipe.LTrim(ctx, orgJobsKey(job.OrgID), 0, 49)
		pipe.Expire(ctx, orgJobsKey(job.OrgID), reportJobTTL)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (uc *SummaryUseCase) getJob(ctx context.Context, jobID string) (*TrainingReportJob, error) {
	if uc.redis == nil || jobID == "" {
		return nil, fmt.Errorf("invalid")
	}
	val, err := uc.redis.Get(ctx, jobRedisKey(jobID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var job TrainingReportJob
	if err := json.Unmarshal([]byte(val), &job); err != nil {
		return nil, err
	}
	// 过期判定
	if job.Status == ReportStatusDone && job.ExpiresAt > 0 && time.Now().Unix() > job.ExpiresAt {
		job.Status = ReportStatusExpired
	}
	return &job, nil
}

func (uc *SummaryUseCase) listJobs(ctx context.Context, orgID int64, limit int64) ([]*TrainingReportJob, error) {
	if uc.redis == nil || orgID <= 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	// 多取一些再去重：历史 bug 会把同一 jobId 重复 LPush
	ids, err := uc.redis.LRange(ctx, orgJobsKey(orgID), 0, 99).Result()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]*TrainingReportJob, 0, int(limit))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		j, err := uc.getJob(ctx, id)
		if err != nil || j == nil {
			continue
		}
		out = append(out, j)
		if int64(len(out)) >= limit {
			break
		}
	}
	return out, nil
}

func (uc *SummaryUseCase) updateJob(ctx context.Context, jobID string, mut func(*TrainingReportJob)) error {
	job, err := uc.getJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job not found")
	}
	// getJob may mark expired; restore done for in-place updates while still within TTL storage
	if job.Status == ReportStatusExpired {
		// still allow status field rewrite on disk copy
	}
	mut(job)
	return uc.saveJob(ctx, job, false)
}

func jobHTMLPath(jobID string) string {
	return filepath.Join(reportDir(), jobID) + ".html"
}

// IsDownloadable 是否仍在 24h 下载窗内
func (j *TrainingReportJob) IsDownloadable(now time.Time) bool {
	if j == nil || j.Status != ReportStatusDone {
		return false
	}
	if j.ExpiresAt <= 0 {
		return false
	}
	return now.Unix() <= j.ExpiresAt && j.HTMLPath != ""
}

// EffectiveStatus 含过期态
func (j *TrainingReportJob) EffectiveStatus(now time.Time) string {
	if j == nil {
		return ""
	}
	if j.Status == ReportStatusDone && j.ExpiresAt > 0 && now.Unix() > j.ExpiresAt {
		return ReportStatusExpired
	}
	return j.Status
}
