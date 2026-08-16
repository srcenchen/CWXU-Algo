package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	backuppb "cwxu-algo/api/user/v1/site/backup"
	"cwxu-algo/app/common/backup"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

const (
	maxBackupUploadBytes = 2 << 30 // 2 GiB
	// multipart 解析的内存驻留上限：超过部分自动落盘临时文件，避免整包吃进内存
	backupUploadMemoryBytes = 32 << 20 // 32 MiB
	// 导出/导入任务结束后保留 10 分钟（含 zip），超时自动删库记录与磁盘文件
	backupJobRetention = 10 * time.Minute
	backupCleanupEvery = 1 * time.Minute
	maxListedJobs      = 30
)

// RegisterBackupRoutes 站点数据备份/恢复中仍需手写的路由（自定义路由以支持大文件与长下载）：
// import（multipart 大文件上传）、jobs/{id}/download（zip 流式下载）、jobs/{id}（删除）。
// export / jobs / jobs/{id} 查询已迁移为 proto 服务（BackupService，见本文件）。
func RegisterBackupRoutes(srv *khttp.Server, d *data.Data) {
	if d == nil {
		return
	}
	_ = os.MkdirAll(backup.BackupDir(), 0o755)
	go startBackupCleanupLoop(d)

	r := srv.Route("/")

	r.POST("/v1/user/site/backup/import", func(ctx khttp.Context) error {
		if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要站点备份权限",
			})
		}
		req := ctx.Request()
		// 总量限制交给 MaxBytesReader（2GiB）；ParseMultipartForm 仅限内存驻留 32MB，
		// 其余由 multipart 自动落盘临时文件
		req.Body = http.MaxBytesReader(ctx.Response(), req.Body, maxBackupUploadBytes)
		if err := req.ParseMultipartForm(backupUploadMemoryBytes); err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "解析表单失败或文件过大（最大 2GB）",
			})
		}
		confirm := strings.TrimSpace(req.FormValue("confirm"))
		if confirm != backup.ConfirmToken {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "请确认后输入 RESTORE 再导入",
			})
		}
		if busy, _ := hasActiveJob(d, model.BackupKindImport); busy {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"code": 1, "message": "已有导入任务进行中，请稍后再试",
			})
		}
		file, hdr, err := req.FormFile("file")
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "缺少 file 字段（zip 备份包）",
			})
		}
		defer file.Close()

		pd := auth.GetCurrentUser(ctx)
		uid := uint(0)
		if pd != nil {
			uid = pd.UserID
		}
		job := model.BackupJob{
			Kind:      model.BackupKindImport,
			Status:    model.BackupStatusPending,
			Scopes:    `["all"]`,
			Progress:  0,
			Message:   "上传完成，排队中",
			CreatedBy: uid,
		}
		if err := d.DB.Create(&job).Error; err != nil {
			// 部分唯一索引（kind + pending/running）兜底并发：同类任务同时只允许一个
			if isUniqueViolation(err) {
				return ctx.JSON(http.StatusConflict, map[string]interface{}{
					"code": 1, "message": "已有导入任务进行中，请稍后再试",
				})
			}
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "创建任务失败",
			})
		}

		// save upload under backups/imports/
		importDir := filepath.Join(backup.BackupDir(), "imports")
		_ = os.MkdirAll(importDir, 0o755)
		relName := fmt.Sprintf("import_%d_%s.zip", job.ID, time.Now().Format("20060102_150405"))
		absPath := filepath.Join(importDir, relName)
		out, err := os.Create(absPath)
		if err != nil {
			failJob(d, job.ID, "无法保存上传文件")
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "保存上传失败",
			})
		}
		n, copyErr := io.Copy(out, io.LimitReader(file, maxBackupUploadBytes+1))
		_ = out.Close()
		if copyErr != nil || n > maxBackupUploadBytes {
			_ = os.Remove(absPath)
			failJob(d, job.ID, "读取上传文件失败或超过 2GB")
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "读取上传文件失败或超过 2GB",
			})
		}
		rel := filepath.ToSlash(filepath.Join("imports", relName))
		_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
			"file_path": rel,
			"file_size": n,
			"message":   fmt.Sprintf("已接收 %s（%d 字节）", hdr.Filename, n),
		})
		go runImportJob(d, job.ID)
		return ctx.JSON(http.StatusOK, map[string]interface{}{
			"code": 0, "message": "导入任务已创建", "jobId": job.ID,
		})
	})

	r.GET("/v1/user/site/backup/jobs/{id}/download", func(ctx khttp.Context) error {
		if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要站点备份权限",
			})
		}
		id, _ := strconv.ParseUint(ctx.Vars().Get("id"), 10, 64)
		var job model.BackupJob
		if err := d.DB.First(&job, id).Error; err != nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "任务不存在",
			})
		}
		if job.Kind != model.BackupKindExport || job.Status != model.BackupStatusDone || job.FilePath == "" {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "该任务没有可下载的备份包",
			})
		}
		abs := filepath.Join(backup.BackupDir(), filepath.FromSlash(job.FilePath))
		abs = filepath.Clean(abs)
		base := filepath.Clean(backup.BackupDir())
		if rel, err := filepath.Rel(base, abs); err != nil || strings.HasPrefix(rel, "..") {
			return ctx.JSON(http.StatusBadRequest, map[string]interface{}{
				"code": 1, "message": "非法文件路径",
			})
		}
		f, err := os.Open(abs)
		if err != nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "备份文件不存在或已清理",
			})
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"code": 1, "message": "读取文件失败",
			})
		}
		w := ctx.Response()
		name := filepath.Base(abs)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, ctx.Request(), name, st.ModTime(), f)
		return nil
	})

	r.DELETE("/v1/user/site/backup/jobs/{id}", func(ctx khttp.Context) error {
		if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
			return ctx.JSON(http.StatusForbidden, map[string]interface{}{
				"code": 1, "message": "需要站点备份权限",
			})
		}
		id, _ := strconv.ParseUint(ctx.Vars().Get("id"), 10, 64)
		var job model.BackupJob
		if err := d.DB.First(&job, id).Error; err != nil {
			return ctx.JSON(http.StatusNotFound, map[string]interface{}{
				"code": 1, "message": "任务不存在",
			})
		}
		if job.Status == model.BackupStatusRunning || job.Status == model.BackupStatusPending {
			return ctx.JSON(http.StatusConflict, map[string]interface{}{
				"code": 1, "message": "进行中的任务不可删除",
			})
		}
		if job.FilePath != "" {
			abs := filepath.Join(backup.BackupDir(), filepath.FromSlash(job.FilePath))
			_ = os.Remove(abs)
		}
		_ = d.DB.Delete(&model.BackupJob{}, id).Error
		return ctx.JSON(http.StatusOK, map[string]interface{}{
			"code": 0, "message": "已删除",
		})
	})
}

// BackupService 站点数据备份任务（proto：api/user/v1/site/backup/backup.proto）。
// 持有 *data.Data（与手写路由 RegisterBackupRoutes 同源）。
type BackupService struct {
	d *data.Data
}

func NewBackupService(d *data.Data) *BackupService {
	return &BackupService{d: d}
}

// Export 创建全量/按 scope 导出任务（后台异步执行）。
func (s *BackupService) Export(ctx context.Context, req *backuppb.ExportReq) (*backuppb.ExportRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
		return &backuppb.ExportRes{Code: 1, Message: "需要站点备份权限"}, nil
	}
	scopes, err := backup.NormalizeScopes(req.Scopes)
	if err != nil {
		return &backuppb.ExportRes{Code: 1, Message: err.Error()}, nil
	}
	if busy, _ := hasActiveJob(s.d, model.BackupKindExport); busy {
		return &backuppb.ExportRes{Code: 1, Message: "已有导出任务进行中，请稍后再试"}, nil
	}
	pd := auth.GetCurrentUser(ctx)
	uid := uint(0)
	if pd != nil {
		uid = pd.UserID
	}
	scopesJSON, _ := json.Marshal(scopes)
	job := model.BackupJob{
		Kind:      model.BackupKindExport,
		Status:    model.BackupStatusPending,
		Scopes:    string(scopesJSON),
		Progress:  0,
		Message:   "排队中",
		CreatedBy: uid,
	}
	if err := s.d.DB.WithContext(ctx).Create(&job).Error; err != nil {
		// 部分唯一索引（kind + pending/running）兜底并发：同类任务同时只允许一个
		if isUniqueViolation(err) {
			return &backuppb.ExportRes{Code: 1, Message: "已有导出任务进行中，请稍后再试"}, nil
		}
		return &backuppb.ExportRes{Code: 1, Message: "创建任务失败"}, nil
	}
	go runExportJob(s.d, job.ID)
	return &backuppb.ExportRes{Code: 0, Message: "导出任务已创建", JobId: int64(job.ID)}, nil
}

// ListJobs 最近备份任务列表。
func (s *BackupService) ListJobs(ctx context.Context, req *backuppb.ListJobsReq) (*backuppb.ListJobsRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
		return &backuppb.ListJobsRes{Code: 1, Message: "需要站点备份权限"}, nil
	}
	var jobs []model.BackupJob
	_ = s.d.DB.WithContext(ctx).Order("id DESC").Limit(maxListedJobs).Find(&jobs).Error
	list := make([]*backuppb.JobInfo, 0, len(jobs))
	for i := range jobs {
		list = append(list, jobToInfo(jobs[i]))
	}
	return &backuppb.ListJobsRes{Code: 0, Message: "ok", Jobs: list}, nil
}

// GetJob 单个任务状态。
func (s *BackupService) GetJob(ctx context.Context, req *backuppb.GetJobReq) (*backuppb.GetJobRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteBackup) {
		return &backuppb.GetJobRes{Code: 1, Message: "需要站点备份权限"}, nil
	}
	if req.Id == 0 {
		return &backuppb.GetJobRes{Code: 1, Message: "无效任务 id"}, nil
	}
	var job model.BackupJob
	if err := s.d.DB.WithContext(ctx).First(&job, req.Id).Error; err != nil {
		return &backuppb.GetJobRes{Code: 1, Message: "任务不存在"}, nil
	}
	return &backuppb.GetJobRes{Code: 0, Message: "ok", Job: jobToInfo(job)}, nil
}

func jobToInfo(j model.BackupJob) *backuppb.JobInfo {
	var scopes []string
	_ = json.Unmarshal([]byte(j.Scopes), &scopes)
	info := &backuppb.JobInfo{
		Id:          int64(j.ID),
		Kind:        j.Kind,
		Status:      j.Status,
		Scopes:      scopes,
		Progress:    int32(j.Progress),
		Message:     j.Message,
		FileSize:    j.FileSize,
		CreatedBy:   int64(j.CreatedBy),
		ErrorDetail: j.ErrorDetail,
		CreatedAt:   j.CreatedAt.UTC().Format(time.RFC3339),
		Downloadable: j.Kind == model.BackupKindExport &&
			j.Status == model.BackupStatusDone && j.FilePath != "",
	}
	if j.StartedAt != nil {
		info.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if j.FinishedAt != nil {
		info.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return info
}

func hasActiveJob(d *data.Data, kind string) (bool, error) {
	var n int64
	err := d.DB.Model(&model.BackupJob{}).
		Where("kind = ? AND status IN ?", kind, []string{model.BackupStatusPending, model.BackupStatusRunning}).
		Count(&n).Error
	return n > 0, err
}

func failJob(d *data.Data, id uint, msg string) {
	now := time.Now()
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":       model.BackupStatusFailed,
		"message":      msg,
		"error_detail": msg,
		"finished_at":  now,
		"progress":     0,
	})
	scheduleBackupJobExpiry(d, id)
}

func updateJobProgress(d *data.Data, id uint, pct int, msg string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 99 {
		pct = 99
	}
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"progress": pct,
		"message":  msg,
		"status":   model.BackupStatusRunning,
	})
}

func runExportJob(d *data.Data, id uint) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("backup export panic job=%d: %v", id, r)
			failJob(d, id, fmt.Sprintf("导出异常: %v", r))
		}
	}()
	now := time.Now()
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":     model.BackupStatusRunning,
		"started_at": now,
		"message":    "开始导出",
		"progress":   1,
	})

	var job model.BackupJob
	if err := d.DB.First(&job, id).Error; err != nil {
		return
	}
	var scopes []string
	_ = json.Unmarshal([]byte(job.Scopes), &scopes)

	work := filepath.Join(backup.BackupDir(), "work", fmt.Sprintf("export_%d", id))
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		failJob(d, id, "创建工作目录失败")
		return
	}
	defer os.RemoveAll(work)

	_, err := backup.Export(backup.ExportOptions{
		DBs:    backup.DBs{User: d.DB, Core: d.CoreDB},
		Dir:    work,
		Scopes: scopes,
		Progress: func(pct int, msg string) {
			updateJobProgress(d, id, pct, msg)
		},
	})
	if err != nil {
		failJob(d, id, err.Error())
		return
	}

	updateJobProgress(d, id, 96, "压缩备份包 …")
	zipName := fmt.Sprintf("goalgo-backup-%s-%d.zip", time.Now().Format("20060102-150405"), id)
	rel := filepath.ToSlash(filepath.Join("exports", zipName))
	absZip := filepath.Join(backup.BackupDir(), "exports", zipName)
	if err := backup.ZipDir(work, absZip); err != nil {
		failJob(d, id, "压缩失败: "+err.Error())
		return
	}
	st, _ := os.Stat(absZip)
	var size int64
	if st != nil {
		size = st.Size()
	}
	fin := time.Now()
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":      model.BackupStatusDone,
		"progress":    100,
		"message":     "导出完成，请在 10 分钟内下载",
		"file_path":   rel,
		"file_size":   size,
		"finished_at": fin,
	})
	log.Infof("backup export done job=%d size=%d", id, size)
	scheduleBackupJobExpiry(d, id)
}

func runImportJob(d *data.Data, id uint) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("backup import panic job=%d: %v", id, r)
			failJob(d, id, fmt.Sprintf("导入异常: %v（可能已部分恢复）", r))
		}
	}()
	now := time.Now()
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":     model.BackupStatusRunning,
		"started_at": now,
		"message":    "开始导入",
		"progress":   1,
	})

	var job model.BackupJob
	if err := d.DB.First(&job, id).Error; err != nil {
		return
	}
	if job.FilePath == "" {
		failJob(d, id, "缺少上传文件")
		return
	}
	absZip := filepath.Join(backup.BackupDir(), filepath.FromSlash(job.FilePath))
	work := filepath.Join(backup.BackupDir(), "work", fmt.Sprintf("import_%d", id))
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		failJob(d, id, "创建工作目录失败")
		return
	}
	defer os.RemoveAll(work)

	updateJobProgress(d, id, 5, "解压备份包 …")
	if err := backup.UnzipTo(absZip, work); err != nil {
		failJob(d, id, "解压失败: "+err.Error())
		return
	}

	_, err := backup.Import(backup.ImportOptions{
		DBs: backup.DBs{User: d.DB, Core: d.CoreDB},
		Dir: work,
		LegacyConfigKey: d.LegacyConfigKey,
		Progress: func(pct int, msg string) {
			updateJobProgress(d, id, pct, msg)
		},
	})
	if err != nil {
		failJob(d, id, err.Error())
		return
	}

	// refresh site settings redis
	data.PublishSiteSettings(d)

	fin := time.Now()
	_ = d.DB.Model(&model.BackupJob{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":      model.BackupStatusDone,
		"progress":    100,
		"message":     "导入完成，建议刷新页面",
		"finished_at": fin,
	})
	log.Infof("backup import done job=%d", id)
	scheduleBackupJobExpiry(d, id)
}

// scheduleBackupJobExpiry 任务结束后延迟删除 zip 与任务记录。
func scheduleBackupJobExpiry(d *data.Data, id uint) {
	if d == nil || id == 0 {
		return
	}
	time.AfterFunc(backupJobRetention, func() {
		purgeBackupJob(d, id)
	})
}

func startBackupCleanupLoop(d *data.Data) {
	// 启动立即扫一遍（覆盖进程重启期间已到期的任务），再按分钟轮询兜底
	cleanupExpiredBackups(d)
	ticker := time.NewTicker(backupCleanupEvery)
	defer ticker.Stop()
	for range ticker.C {
		cleanupExpiredBackups(d)
	}
}

func cleanupExpiredBackups(d *data.Data) {
	if d == nil || d.DB == nil {
		return
	}
	cutoff := time.Now().Add(-backupJobRetention)
	var old []model.BackupJob
	// 以结束时间为准；无 finished_at 的失败/历史任务用 updated_at
	if err := d.DB.Where(
		`status IN ? AND (
			(finished_at IS NOT NULL AND finished_at < ?) OR
			(finished_at IS NULL AND updated_at < ?)
		)`,
		[]string{model.BackupStatusDone, model.BackupStatusFailed},
		cutoff, cutoff,
	).Find(&old).Error; err != nil {
		return
	}
	for _, j := range old {
		purgeBackupJob(d, j.ID)
	}
}

func purgeBackupJob(d *data.Data, id uint) {
	if d == nil || d.DB == nil || id == 0 {
		return
	}
	var j model.BackupJob
	if err := d.DB.First(&j, id).Error; err != nil {
		return
	}
	// 仍在进行中的任务不删
	if j.Status == model.BackupStatusPending || j.Status == model.BackupStatusRunning {
		return
	}
	// 若尚未到期则跳过（AfterFunc 与轮询竞态时以 finished_at 为准）
	ref := j.UpdatedAt
	if j.FinishedAt != nil {
		ref = *j.FinishedAt
	}
	if time.Since(ref) < backupJobRetention {
		return
	}
	if j.FilePath != "" {
		_ = os.Remove(filepath.Join(backup.BackupDir(), filepath.FromSlash(j.FilePath)))
	}
	if err := d.DB.Delete(&model.BackupJob{}, j.ID).Error; err != nil {
		log.Warnf("backup purge job=%d: %v", id, err)
		return
	}
	log.Infof("backup purged job=%d kind=%s", id, j.Kind)
}
