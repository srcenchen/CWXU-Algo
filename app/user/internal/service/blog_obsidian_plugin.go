package service

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

const (
	obsidianPluginMetaID     = 1
	obsidianPluginDefaultCDN = "https://zhiyuansofts.cn/obsidian/goalgo-blog"
	// 发布脚本与后端共享；可用环境变量 CWXU_OBSIDIAN_PUBLISH_TOKEN 覆盖
	obsidianPluginPublishTokenEnv = "CWXU_OBSIDIAN_PUBLISH_TOKEN"
	obsidianPluginPublishTokenDef = "goalgo-obsidian-publish-2026"
)

type obsidianPluginView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	MinAppVersion string `json:"minAppVersion,omitempty"`
	Notes         string `json:"notes,omitempty"`
	ReleasedAt    int64  `json:"releasedAt,omitempty"`
	// DownloadBase 云存储该版本目录（无尾 /），客户端从此拉 main.js / manifest.json / styles.css
	DownloadBase string `json:"downloadBase"`
}

type obsidianPluginPublishReq struct {
	Version       string `json:"version"`
	MinAppVersion string `json:"minAppVersion"`
	Notes         string `json:"notes"`
	ReleasedAt    int64  `json:"releasedAt"`
	DownloadBase  string `json:"downloadBase"`
}

func obsidianPublishTokenOK(r *http.Request) bool {
	want := strings.TrimSpace(os.Getenv(obsidianPluginPublishTokenEnv))
	if want == "" {
		want = obsidianPluginPublishTokenDef
	}
	got := strings.TrimSpace(r.Header.Get("X-Plugin-Publish-Token"))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-Goalgo-Publish-Token"))
	}
	return got != "" && got == want
}

func defaultObsidianDownloadBase(version string) string {
	v := strings.Trim(strings.TrimSpace(version), "/")
	if v == "" {
		return ""
	}
	return strings.TrimRight(obsidianPluginDefaultCDN, "/") + "/" + v
}

func (s *BlogService) loadObsidianPluginMeta() (*model.ObsidianPluginMeta, error) {
	var row model.ObsidianPluginMeta
	err := s.db.Where("id = ?", obsidianPluginMetaID).First(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func metaToView(row *model.ObsidianPluginMeta) obsidianPluginView {
	base := strings.TrimRight(strings.TrimSpace(row.DownloadBase), "/")
	if base == "" && row.Version != "" {
		base = defaultObsidianDownloadBase(row.Version)
	}
	return obsidianPluginView{
		ID:            "goalgo-blog",
		Name:          "GoAlgo Blog",
		Version:       strings.TrimSpace(row.Version),
		MinAppVersion: strings.TrimSpace(row.MinAppVersion),
		Notes:         row.Notes,
		ReleasedAt:    row.ReleasedAt,
		DownloadBase:  base,
	}
}

// GET /v1/user/blog/obsidian-plugin/latest — 公开：插件检查更新
func (s *BlogService) handleObsidianPluginLatest(ctx khttp.Context) error {
	row, err := s.loadObsidianPluginMeta()
	if err != nil || row == nil || strings.TrimSpace(row.Version) == "" {
		writeJSON(ctx.Response(), 404, map[string]interface{}{
			"code":    1,
			"message": "暂无插件版本信息",
		})
		return nil
	}
	view := metaToView(row)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code": 0,
		"data": view,
	})
	return nil
}

// POST /v1/user/blog/obsidian-plugin/publish — 发布脚本 / 站管登记当前版本
func (s *BlogService) handleObsidianPluginPublish(ctx khttp.Context) error {
	tokenOK := obsidianPublishTokenOK(ctx.Request())
	adminOK := blogIsSiteAdmin(ctx)
	if !tokenOK && !adminOK {
		writeJSON(ctx.Response(), 403, map[string]interface{}{
			"code":    1,
			"message": "需要发布令牌或站管权限",
		})
		return nil
	}

	var req obsidianPluginPublishReq
	if err := json.NewDecoder(ctx.Request().Body).Decode(&req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code":    1,
			"message": "参数错误",
		})
		return nil
	}
	version := strings.TrimSpace(req.Version)
	if version == "" ||
		strings.ContainsAny(version, "/\\") ||
		strings.Contains(version, "..") ||
		strings.Contains(version, " ") {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code":    1,
			"message": "version 无效",
		})
		return nil
	}
	// 简单 semver 形态校验
	if strings.Count(version, ".") < 1 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{
			"code":    1,
			"message": "version 应为 semver",
		})
		return nil
	}

	minApp := strings.TrimSpace(req.MinAppVersion)
	if minApp == "" {
		minApp = "1.4.0"
	}
	base := strings.TrimRight(strings.TrimSpace(req.DownloadBase), "/")
	if base == "" {
		base = defaultObsidianDownloadBase(version)
	}
	released := req.ReleasedAt
	if released <= 0 {
		released = time.Now().Unix()
	}

	row := model.ObsidianPluginMeta{
		ID:            obsidianPluginMetaID,
		Version:       version,
		MinAppVersion: minApp,
		Notes:         strings.TrimSpace(req.Notes),
		ReleasedAt:    released,
		DownloadBase:  base,
	}
	// upsert id=1
	var existing model.ObsidianPluginMeta
	if err := s.db.Where("id = ?", obsidianPluginMetaID).First(&existing).Error; err != nil {
		if err := s.db.Create(&row).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{
				"code":    1,
				"message": "保存失败",
			})
			return nil
		}
	} else {
		if err := s.db.Model(&existing).Updates(map[string]interface{}{
			"version":         row.Version,
			"min_app_version": row.MinAppVersion,
			"notes":           row.Notes,
			"released_at":     row.ReleasedAt,
			"download_base":   row.DownloadBase,
		}).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{
				"code":    1,
				"message": "更新失败",
			})
			return nil
		}
		row = existing
		row.Version = version
		row.MinAppVersion = minApp
		row.Notes = strings.TrimSpace(req.Notes)
		row.ReleasedAt = released
		row.DownloadBase = base
	}

	view := metaToView(&row)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"code":    0,
		"message": "ok",
		"data":    view,
	})
	return nil
}
