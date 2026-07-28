package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"cwxu-algo/api/core/v1/problem"
	"cwxu-algo/api/user/v1/profile"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
	biz "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
)

type ProblemService struct {
	problem.UnimplementedProblemServer
	uc  *biz.ProblemUseCase
	reg *registry.Registrar
}

func NewProblemService(uc *biz.ProblemUseCase, reg *discovery.Register) *ProblemService {
	return &ProblemService{uc: uc, reg: &reg.Reg}
}

func (s *ProblemService) fetchUserNames(ctx context.Context, userIDs []int64) map[int64]string {
	out := map[int64]string{}
	if len(userIDs) == 0 {
		return out
	}
	client, err := userrpc.ProfileClient(s.reg)
	if err != nil {
		log.Errorf("problem userRPC: %v", err)
		return out
	}
	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}
	res, err := client.GetByIds(ctx, &profile.GetByIdsReq{UserIds: userIDs, OrgId: orgID})
	if err != nil {
		log.Errorf("problem GetByIds: %v", err)
		return out
	}
	for _, p := range res.Profiles {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.Username)
		}
		out[p.UserId] = name
	}
	return out
}

// attachContributors 填充审核通过的内容贡献者（多人全列）。
func (s *ProblemService) attachContributors(ctx context.Context, info *problem.ProblemInfo) {
	if info == nil || info.Id == 0 {
		return
	}
	uids, err := s.uc.ListProblemContributors(uint(info.Id))
	if err != nil || len(uids) == 0 {
		return
	}
	ids := make([]int64, 0, len(uids))
	for _, id := range uids {
		ids = append(ids, int64(id))
	}
	profiles := s.fetchUserProfiles(ctx, ids)
	out := make([]*problem.ProblemContributor, 0, len(uids))
	for _, id := range uids {
		p := profiles[int64(id)]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSpace(p.Username)
		}
		if name == "" {
			name = fmt.Sprintf("用户%d", id)
		}
		out = append(out, &problem.ProblemContributor{
			UserId:   uint32(id),
			Name:     name,
			Username: strings.TrimSpace(p.Username),
			Avatar:   strings.TrimSpace(p.Avatar),
		})
	}
	info.Contributors = out
}

// cleanDisplayTitle 列表/详情展示用：去掉 AtCoder 页头夹带的 Editorial 与空白行
func cleanDisplayTitle(title string) string {
	title = strings.ReplaceAll(title, "\r", "\n")
	for _, line := range strings.Split(title, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.EqualFold(line, "Editorial") || strings.EqualFold(line, "解説") {
			continue
		}
		for _, suf := range []string{"Editorial", "解説"} {
			if i := strings.LastIndex(line, suf); i > 0 {
				line = strings.TrimSpace(line[:i])
			}
		}
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			return line
		}
	}
	return strings.Join(strings.Fields(title), " ")
}

func (s *ProblemService) toInfo(p *model.Problem, userStatus string) *problem.ProblemInfo {
	if p == nil {
		return nil
	}
	tags := []string(p.Tags)
	if tags == nil {
		tags = []string{}
	}
	sols := make([]*problem.SolutionMeta, 0, len(p.SolutionsMeta))
	for _, sol := range p.SolutionsMeta {
		sols = append(sols, &problem.SolutionMeta{
			Name:             sol.Name,
			TimeComplexity:   sol.TimeComplexity,
			SpaceComplexity:  sol.SpaceComplexity,
			BriefExplanation: sol.BriefExplanation,
		})
	}
	var last int64
	if p.LastSubmittedAt != nil {
		last = p.LastSubmittedAt.Unix()
	}
	return &problem.ProblemInfo{
		Id:              uint32(p.ID),
		Platform:        p.Platform,
		ExternalId:      p.ExternalID,
		Title:           cleanDisplayTitle(p.Title),
		Url:             p.URL,
		ContentMd:       p.ContentMD,
		ProblemType:     p.ProblemType,
		Tags:            tags,
		Solutions:       sols,
		Difficulty:      p.Difficulty,
		Status:          p.Status,
		ErrorMsg:        p.ErrorMsg,
		LastSubmittedAt: last,
		UserStatus:      userStatus,
	}
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *ProblemService) ListTags(ctx context.Context, req *problem.ListTagsReq) (*problem.ListTagsRes, error) {
	rows, err := s.uc.ListTags(int(req.Limit))
	if err != nil {
		return nil, errors.InternalServer("list tags failed", "service unavailable")
	}
	data := make([]*problem.TagCount, 0, len(rows))
	for _, r := range rows {
		data = append(data, &problem.TagCount{Tag: r.Tag, Count: r.Count})
	}
	return &problem.ListTagsRes{Code: 0, Message: "success", Data: data}, nil
}

func (s *ProblemService) Hot(ctx context.Context, req *problem.HotProblemReq) (*problem.HotProblemRes, error) {
	page := req.Page
	if page <= 0 {
		page = 1
	}
	ps := req.PageSize
	if ps <= 0 {
		ps = 20
	}
	rows, total, days, err := s.uc.ListHot(page, ps, int(req.Days))
	if err != nil {
		return nil, errors.InternalServer("hot list failed", "service unavailable")
	}
	data := make([]*problem.HotProblemItem, 0, len(rows))
	for i := range rows {
		info := s.toInfo(&rows[i].Problem, "")
		info.ContentMd = ""
		// 热度榜用窗口内最近提交时间覆盖题库字段，便于前端展示
		var last int64
		if !rows[i].LastTime.IsZero() {
			last = rows[i].LastTime.Unix()
			info.LastSubmittedAt = last
		}
		data = append(data, &problem.HotProblemItem{
			Problem:         info,
			SubmitCount:     rows[i].SubmitCount,
			SolverCount:     rows[i].SolverCount,
			AcCount:         rows[i].AcCount,
			Score:           rows[i].Score,
			LastSubmittedAt: last,
		})
	}
	return &problem.HotProblemRes{
		Code:     0,
		Message:  "success",
		Data:     data,
		Total:    total,
		Page:     page,
		PageSize: ps,
		Days:     int32(days),
	}, nil
}

func (s *ProblemService) List(ctx context.Context, req *problem.ListProblemReq) (*problem.ListProblemRes, error) {
	var followingIDs []int64
	if req.FollowingOnly {
		viewer := auth.GetCurrentUserId(ctx)
		if viewer == 0 {
			return &problem.ListProblemRes{Code: 0, Message: "success", Data: nil, Total: 0, Page: 1, PageSize: req.PageSize}, nil
		}
		followingIDs = fetchFollowingIDs(ctx, s.reg, int64(viewer))
		if followingIDs == nil {
			followingIDs = []int64{}
		}
	}
	list, statusMap, total, err := s.uc.List(biz.ListProblemFilter{
		Page:         req.Page,
		PageSize:     req.PageSize,
		Sort:         req.Sort,
		Platforms:    splitCSV(req.Platforms),
		Tags:         splitCSV(req.Tags),
		UserStatus:   req.UserStatus,
		UserID:       req.UserId,
		Keyword:      req.Keyword,
		Difficulty:   req.Difficulty,
		FollowingIDs: followingIDs,
	})
	if err != nil {
		return nil, errors.InternalServer("list failed", "service unavailable")
	}
	data := make([]*problem.ProblemInfo, 0, len(list))
	for i := range list {
		us := statusMap[list[i].ID]
		if us == "" {
			us = "NONE"
		}
		// list 不返回完整 content
		info := s.toInfo(&list[i], us)
		info.ContentMd = ""
		data = append(data, info)
	}
	page := req.Page
	if page <= 0 {
		page = 1
	}
	ps := req.PageSize
	if ps <= 0 {
		ps = 20
	}
	return &problem.ListProblemRes{
		Code:     0,
		Message:  "success",
		Data:     data,
		Total:    total,
		Page:     page,
		PageSize: ps,
	}, nil
}

func (s *ProblemService) Get(ctx context.Context, req *problem.GetProblemReq) (*problem.GetProblemRes, error) {
	p, err := s.uc.Get(uint(req.Id))
	if err != nil {
		return &problem.GetProblemRes{Code: 1, Message: "题目不存在"}, nil
	}
	info := s.toInfo(p, "")
	s.attachContributors(ctx, info)
	return &problem.GetProblemRes{
		Code:    0,
		Message: "success",
		Data:    info,
	}, nil
}

func (s *ProblemService) RelatedContests(ctx context.Context, req *problem.RelatedContestsReq) (*problem.RelatedContestsRes, error) {
	if req == nil || req.ProblemId == 0 {
		return &problem.RelatedContestsRes{Code: 1, Message: "缺少题目"}, nil
	}
	list, err := s.uc.ListRelatedContests(uint(req.ProblemId))
	if err != nil {
		return &problem.RelatedContestsRes{Code: 1, Message: "查询失败，请稍后重试"}, nil
	}
	data := make([]*problem.ProblemRelatedContest, 0, len(list))
	for _, c := range list {
		data = append(data, &problem.ProblemRelatedContest{
			Platform:     c.Platform,
			ContestId:    c.ContestID,
			Label:        c.Label,
			ContestName:  c.ContestName,
			ContestLogId: uint32(c.ContestLogID),
			ContestTime:  c.ContestTime,
			ProblemTitle: c.ProblemTitle,
			ContestUrl:   c.ContestURL,
		})
	}
	return &problem.RelatedContestsRes{
		Code:    0,
		Message: "success",
		Data:    data,
	}, nil
}

func (s *ProblemService) ListSubmissions(ctx context.Context, req *problem.ListSubmissionsReq) (*problem.ListSubmissionsRes, error) {
	var followingIDs []int64
	if req.FollowingOnly {
		viewer := auth.GetCurrentUserId(ctx)
		if viewer == 0 {
			return &problem.ListSubmissionsRes{Code: 0, Message: "success", Data: nil, Total: 0}, nil
		}
		followingIDs = fetchFollowingIDs(ctx, s.reg, int64(viewer))
		if followingIDs == nil {
			followingIDs = []int64{}
		}
	}
	list, total, err := s.uc.ListSubmissions(uint(req.ProblemId), req.UserId, req.Page, req.PageSize, followingIDs, req.Status)
	if err != nil {
		return nil, errors.InternalServer("query failed", "service unavailable")
	}
	ids := make([]int64, 0, len(list))
	seen := map[int64]bool{}
	for _, v := range list {
		if v.UserID != 0 && !seen[v.UserID] {
			seen[v.UserID] = true
			ids = append(ids, v.UserID)
		}
	}
	names := s.fetchUserNames(ctx, ids)
	data := make([]*problem.SubmissionInfo, 0, len(list))
	for _, v := range list {
		name := names[v.UserID]
		if name == "" {
			name = ""
		}
		data = append(data, &problem.SubmissionInfo{
			Id:       uint32(v.ID),
			UserId:   v.UserID,
			Platform: v.Platform,
			SubmitId: model.NormalizeSubmitID(v.Platform, v.SubmitID),
			Lang:     v.Lang,
			Status:   v.Status,
			Time:     v.Time.Unix(),
			Contest:  v.Contest,
			UserName: name,
		})
	}
	return &problem.ListSubmissionsRes{
		Code:    0,
		Message: "success",
		Data:    data,
		Total:   total,
	}, nil
}

func (s *ProblemService) FollowingStatus(ctx context.Context, req *problem.FollowingStatusReq) (*problem.FollowingStatusRes, error) {
	viewer := auth.GetCurrentUserId(ctx)
	if viewer == 0 {
		return &problem.FollowingStatusRes{Code: 1, Message: "请先登录"}, nil
	}
	if req.ProblemId == 0 {
		return &problem.FollowingStatusRes{Code: 1, Message: "缺少题目 id"}, nil
	}
	following := fetchFollowingIDs(ctx, s.reg, int64(viewer))
	if len(following) == 0 {
		return &problem.FollowingStatusRes{Code: 0, Message: "success", Data: nil}, nil
	}
	rows, err := s.uc.FollowingProblemStatus(uint(req.ProblemId), following)
	if err != nil {
		return nil, errors.InternalServer("query failed", "service unavailable")
	}
	// 批量用户资料（含 username）
	profiles := s.fetchUserProfiles(ctx, following)
	data := make([]*problem.FollowingStatusItem, 0, len(rows))
	for _, r := range rows {
		p := profiles[r.UserID]
		name := p.Name
		if name == "" {
			name = p.Username
		}
		if name == "" {
			name = fmt.Sprintf("用户%d", r.UserID)
		}
		data = append(data, &problem.FollowingStatusItem{
			UserId:   r.UserID,
			Username: p.Username,
			Name:     name,
			Avatar:   p.Avatar,
			Status:   r.Status,
		})
	}
	// 排序：AC → TRIED → NONE，同组按名
	sort.SliceStable(data, func(i, j int) bool {
		ri, rj := rankFollowStatus(data[i].Status), rankFollowStatus(data[j].Status)
		if ri != rj {
			return ri > rj
		}
		return data[i].Name < data[j].Name
	})
	return &problem.FollowingStatusRes{Code: 0, Message: "success", Data: data}, nil
}

func rankFollowStatus(s string) int {
	switch strings.ToUpper(s) {
	case "AC":
		return 2
	case "TRIED":
		return 1
	default:
		return 0
	}
}

type userProfileBrief struct {
	Username string
	Name     string
	Avatar   string
}

func (s *ProblemService) fetchUserProfiles(ctx context.Context, userIDs []int64) map[int64]userProfileBrief {
	out := map[int64]userProfileBrief{}
	if len(userIDs) == 0 {
		return out
	}
	client, err := userrpc.ProfileClient(s.reg)
	if err != nil {
		return out
	}
	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}
	res, err := client.GetByIds(ctx, &profile.GetByIdsReq{UserIds: userIDs, OrgId: orgID})
	if err != nil {
		return out
	}
	for _, p := range res.GetProfiles() {
		out[p.UserId] = userProfileBrief{
			Username: p.Username,
			Name:     p.Name,
			Avatar:   p.Avatar,
		}
	}
	return out
}

func (s *ProblemService) UserProfile(ctx context.Context, req *problem.UserProfileReq) (*problem.UserProfileRes, error) {
	if req.UserId <= 0 {
		return &problem.UserProfileRes{Code: 1, Message: "user_id 无效"}, nil
	}
	radar, plats, diffs, totalAC, err := s.uc.UserProfile(req.UserId)
	if err != nil {
		return nil, errors.InternalServer("profile failed", "service unavailable")
	}
	r := make([]*problem.TagScore, 0, len(radar))
	for _, v := range radar {
		r = append(r, &problem.TagScore{Tag: v.Tag, Score: v.Score, AcCount: v.ACCount})
	}
	p := make([]*problem.NamedCount, 0, len(plats))
	for _, v := range plats {
		p = append(p, &problem.NamedCount{Name: v.Name, Count: v.Count})
	}
	d := make([]*problem.NamedCount, 0, len(diffs))
	for _, v := range diffs {
		d = append(d, &problem.NamedCount{Name: v.Name, Count: v.Count})
	}
	return &problem.UserProfileRes{
		Code:         0,
		Message:      "success",
		Radar:        r,
		Platforms:    p,
		Difficulties: d,
		TotalAc:      totalAC,
	}, nil
}

func toFailedProto(list []model.Problem) []*problem.FailedProblem {
	ff := make([]*problem.FailedProblem, 0, len(list))
	for _, f := range list {
		title := cleanDisplayTitle(f.Title)
		if title == "" {
			title = f.ExternalID
		}
		if title == "" {
			title = f.Status
		}
		ff = append(ff, &problem.FailedProblem{
			Id:            uint32(f.ID),
			Platform:      f.Platform,
			ExternalId:    f.ExternalID,
			Title:         title,
			ErrorMsg:      firstNonEmptyTitle(f.ErrorMsg, f.Status),
			UpdatedAt:     f.UpdatedAt.Unix(),
			Status:        f.Status,
			FetchAttempts: int32(f.FetchAttempts),
		})
	}
	return ff
}

func firstNonEmptyTitle(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func (s *ProblemService) Progress(ctx context.Context, req *problem.ProgressReq) (*problem.ProgressRes, error) {
	// 具备训练报告/管理端统计权限者可查看进度；运维写操作仍需题库运维权限
	if !auth.HasPerm(ctx, rbac.PermOrgReportView) {
		return &problem.ProgressRes{Code: 1, Message: "权限不足"}, nil
	}
	snap, err := s.uc.Progress()
	if err != nil {
		// 不因 MQ 等附属信息失败而整页不可用
		log.Errorf("problem progress: %v", err)
		snap = biz.ProgressSnapshot{}
	}
	pi := make([]*problem.ProgressItem, 0, len(snap.Items))
	for _, v := range snap.Items {
		pi = append(pi, &problem.ProgressItem{Status: v.Status, Count: v.Count})
	}
	jobs := make([]*problem.ActiveJob, 0, len(snap.ActiveJobs))
	for _, j := range snap.ActiveJobs {
		t := cleanDisplayTitle(j.Title)
		if t == "" {
			t = j.ExternalID
		}
		jobs = append(jobs, &problem.ActiveJob{
			ProblemId:  uint32(j.ProblemID),
			Platform:   j.Platform,
			ExternalId: j.ExternalID,
			Title:      t,
			Stage:      j.Stage,
			StartedAt:  j.StartedAt.Unix(),
		})
	}
	qs := make([]*problem.QueueStatus, 0, len(snap.Queues))
	for _, q := range snap.Queues {
		qs = append(qs, &problem.QueueStatus{
			Name:        q.Name,
			Messages:    q.Messages,
			Consumers:   q.Consumers,
			Concurrency: q.Concurrency,
		})
	}
	return &problem.ProgressRes{
		Code:             0,
		Message:          "success",
		Items:            pi,
		RecentFailed:     toFailedProto(snap.Failed),
		Total:            snap.Total,
		Paused:           snap.Paused,
		ActiveJobs:       jobs,
		Queues:           qs,
		InProgress:       toFailedProto(snap.InProgress),
		FetchPaused:      snap.FetchPaused,
		AnalyzePaused:    snap.AnalyzePaused,
		RecentFailedPerm: toFailedProto(snap.FailedPerm),
	}, nil
}

func (s *ProblemService) Backfill(ctx context.Context, req *problem.BackfillReq) (*problem.BackfillRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.BackfillRes{Code: 1, Message: "仅管理员可触发回填"}, nil
	}
	// 后台执行：避免 gateway/core HTTP 超时导致前端 500，而任务其实还在跑
	if ok, running := s.uc.TryStartAdminOp("backfill"); !ok {
		return &problem.BackfillRes{Code: 1, Message: "已有任务在执行：" + running + "，请稍后再试"}, nil
	}
	limit := 0
	if req != nil {
		limit = int(req.Limit)
	}
	go func() {
		defer s.uc.FinishAdminOp()
		scanned, bound, created, enqueued, enqFetch, enqAnalyze, err := s.uc.Backfill(limit)
		if err != nil {
			log.Errorf("Backfill background failed: %v", err)
			return
		}
		log.Infof("Backfill background done scanned=%d bound=%d created=%d enq=%d fetch=%d analyze=%d",
			scanned, bound, created, enqueued, enqFetch, enqAnalyze)
	}()
	return &problem.BackfillRes{
		Code:    0,
		Message: "已开始后台补全近 6 个月提交，请稍后刷新进度查看待处理数量",
	}, nil
}

func (s *ProblemService) ResetQueues(ctx context.Context, req *problem.ResetQueuesReq) (*problem.ResetQueuesRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.ResetQueuesRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	if ok, running := s.uc.TryStartAdminOp("reset-queues"); !ok {
		return &problem.ResetQueuesRes{Code: 1, Message: "已有任务在执行：" + running + "，请稍后再试"}, nil
	}
	go func() {
		defer s.uc.FinishAdminOp()
		pf, pa, ef, ea, err := s.uc.ResetQueues()
		if err != nil {
			log.Errorf("ResetQueues background failed: %v", err)
			return
		}
		log.Infof("ResetQueues background done purged_fetch=%d purged_analyze=%d enq_fetch=%d enq_analyze=%d",
			pf, pa, ef, ea)
	}()
	return &problem.ResetQueuesRes{
		Code:    0,
		Message: "已开始后台重置队列并按 DB 重灌，请稍后刷新进度",
	}, nil
}

func (s *ProblemService) EmergencyStop(ctx context.Context, req *problem.EmergencyStopReq) (*problem.EmergencyStopRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.EmergencyStopRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	pf, pa, err := s.uc.EmergencyStop()
	if err != nil {
		return nil, errors.InternalServer("emergency stop failed", "service unavailable")
	}
	return &problem.EmergencyStopRes{
		Code:          0,
		Message:       "已暂停 AI 分析（队列保留；清队列请用重置队列）",
		PurgedFetch:   int64(pf),
		PurgedAnalyze: int64(pa),
	}, nil
}

func (s *ProblemService) ResetAll(ctx context.Context, req *problem.ResetAllReq) (*problem.ResetAllRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.ResetAllRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	requeue := true
	if req != nil && req.RequeueSet {
		requeue = req.Requeue
	}
	reset, enqueued, pf, pa, err := s.uc.ResetAll(requeue)
	if err != nil {
		return nil, errors.InternalServer("reset failed", "service unavailable")
	}
	return &problem.ResetAllRes{
		Code:          0,
		Message:       "已重置 AI 分析（题面保留）",
		Reset_:        int64(reset),
		Enqueued:      int64(enqueued),
		PurgedFetch:   int64(pf),
		PurgedAnalyze: int64(pa),
	}, nil
}

func (s *ProblemService) Resume(ctx context.Context, req *problem.ResumeReq) (*problem.ResumeRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.ResumeRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	s.uc.Resume()
	return &problem.ResumeRes{Code: 0, Message: "AI 分析已恢复"}, nil
}

func (s *ProblemService) ClearRecentFailed(ctx context.Context, req *problem.ClearRecentFailedReq) (*problem.ClearRecentFailedRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.ClearRecentFailedRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	cleared, err := s.uc.ClearRecentFailed()
	if err != nil {
		return &problem.ClearRecentFailedRes{Code: 1, Message: "清空失败，请稍后重试"}, nil
	}
	msg := "已停止这些题目的自动重试"
	if cleared == 0 {
		msg = "没有需要清空的近期失败"
	} else {
		msg = fmt.Sprintf("已停止 %d 道题的自动重试，它们不再出现在近期失败列表", cleared)
	}
	return &problem.ClearRecentFailedRes{
		Code:    0,
		Message: msg,
		Cleared: cleared,
	}, nil
}

// RegisterProblemExtraRoutes 题库运维手写路由（不经 proto）。
func RegisterProblemExtraRoutes(srv *khttp.Server, s *ProblemService) {
	if srv == nil || s == nil {
		return
	}
	r := srv.Route("/")
	// 全量修复 QOJ 标题被识别为「QOJ.ac」的脏数据
	r.POST("/v1/core/problem/repair-qoj-titles", s.handleRepairQOJTitles)
}

// handleRepairQOJTitles body: { "limit"?: number, "refetch"?: bool }
// 默认 limit=0（全量）、refetch=false（优先 content_md 一级标题，不打 QOJ）。
// refetch=true 时对仍无好标题的题访问 qoj.ac 拉官方题头（较慢）。
func (s *ProblemService) handleRepairQOJTitles(ctx khttp.Context) error {
	write := func(code int, body map[string]interface{}) {
		ctx.Response().Header().Set("Content-Type", "application/json; charset=utf-8")
		ctx.Response().WriteHeader(code)
		_ = json.NewEncoder(ctx.Response()).Encode(body)
	}
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		write(403, map[string]interface{}{"code": 1, "message": "仅管理员可操作"})
		return nil
	}
	if ok, running := s.uc.TryStartAdminOp("repair-qoj-titles"); !ok {
		write(409, map[string]interface{}{"code": 1, "message": "已有任务在执行：" + running + "，请稍后再试"})
		return nil
	}
	var req struct {
		Limit   int  `json:"limit"`
		Refetch bool `json:"refetch"`
	}
	body, _ := io.ReadAll(ctx.Request().Body)
	_ = json.Unmarshal(body, &req)
	go func() {
		defer s.uc.FinishAdminOp()
		scanned, fixed, failed, skipped, err := s.uc.RepairQOJBrandTitles(req.Limit, req.Refetch)
		if err != nil {
			log.Errorf("RepairQOJBrandTitles failed: %v", err)
			return
		}
		log.Infof("RepairQOJBrandTitles done scanned=%d fixed=%d failed=%d skipped=%d", scanned, fixed, failed, skipped)
	}()
	msg := "已开始后台修复 QOJ 题目标题（优先用已有题面一级标题）"
	if req.Refetch {
		msg = "已开始后台修复 QOJ 题目标题（含访问 qoj.ac 补全）"
	}
	write(200, map[string]interface{}{"code": 0, "message": msg})
	return nil
}

func (s *ProblemService) ClearNowCoderContent(ctx context.Context, req *problem.ClearNowCoderContentReq) (*problem.ClearNowCoderContentRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.ClearNowCoderContentRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	if ok, running := s.uc.TryStartAdminOp("clear-nowcoder-content"); !ok {
		return &problem.ClearNowCoderContentRes{Code: 1, Message: "已有任务在执行：" + running + "，请稍后再试"}, nil
	}
	requeue := true
	if req != nil && req.RequeueSet {
		requeue = req.Requeue
	}
	// 恢复爬取，避免清空后队列不消费
	if s.uc != nil {
		s.uc.ResumeFetch()
	}
	go func() {
		defer s.uc.FinishAdminOp()
		cleared, enqueued, err := s.uc.ClearNowCoderContentAndRefetch(requeue)
		if err != nil {
			log.Errorf("ClearNowCoderContent failed: %v", err)
			return
		}
		log.Infof("ClearNowCoderContent done cleared=%d enqueued=%d", cleared, enqueued)
	}()
	msg := "已开始清空全部牛客题面（保留标签与 AI 分析）"
	if requeue {
		msg += "，并强制重新拉取题面"
	}
	return &problem.ClearNowCoderContentRes{
		Code:    0,
		Message: msg,
	}, nil
}

func (s *ProblemService) RetryFailed(ctx context.Context, req *problem.RetryFailedReq) (*problem.RetryFailedRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.RetryFailedRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	if s.uc != nil {
		s.uc.ResumeAnalyze()
		s.uc.ResumeFetch()
	}
	if ok, running := s.uc.TryStartAdminOp("retry-failed"); !ok {
		return &problem.RetryFailedRes{Code: 1, Message: "已有任务在执行：" + running + "，请稍后再试"}, nil
	}
	limit := 0
	includePermanent := false
	if req != nil {
		limit = int(req.Limit)
		includePermanent = req.IncludePermanent
	}
	go func() {
		defer s.uc.FinishAdminOp()
		scanned, enqueued, blacklisted, err := s.uc.RetryFailed(limit, includePermanent)
		if err != nil {
			log.Errorf("RetryFailed background failed: %v", err)
			return
		}
		log.Infof("RetryFailed background done includePermanent=%v scanned=%d enqueued=%d blacklisted=%d",
			includePermanent, scanned, enqueued, blacklisted)
	}()
	msg := "已开始后台重试近 6 月失败题，请稍后刷新进度"
	if includePermanent {
		msg = "已开始后台重试近 6 月永久失败（可恢复项），请稍后刷新进度"
	}
	return &problem.RetryFailedRes{
		Code:    0,
		Message: msg,
	}, nil
}

func (s *ProblemService) ToggleAnalyze(ctx context.Context, req *problem.TogglePipelineReq) (*problem.TogglePipelineRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.TogglePipelineRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	pause := true
	if req != nil && req.PauseSet {
		pause = req.Pause
	} else {
		// 翻转
		pause = !s.uc.ProgressPausedAnalyze()
	}
	if pause {
		n, err := s.uc.PauseAnalyze()
		if err != nil {
			return nil, errors.InternalServer("pause analyze failed", "service unavailable")
		}
		return &problem.TogglePipelineRes{Code: 0, Message: "AI 分析已暂停（队列保留）", Paused: true, Purged: int64(n)}, nil
	}
	s.uc.ResumeAnalyze()
	return &problem.TogglePipelineRes{Code: 0, Message: "AI 分析已恢复", Paused: false}, nil
}

func (s *ProblemService) ToggleFetch(ctx context.Context, req *problem.TogglePipelineReq) (*problem.TogglePipelineRes, error) {
	if !auth.HasPerm(ctx, rbac.PermSiteProblemOps) {
		return &problem.TogglePipelineRes{Code: 1, Message: "仅管理员可操作"}, nil
	}
	pause := true
	if req != nil && req.PauseSet {
		pause = req.Pause
	} else {
		pause = !s.uc.ProgressPausedFetch()
	}
	if pause {
		n, err := s.uc.PauseFetch()
		if err != nil {
			return nil, errors.InternalServer("pause fetch failed", "service unavailable")
		}
		return &problem.TogglePipelineRes{Code: 0, Message: "题面爬取已暂停（队列保留）", Paused: true, Purged: int64(n)}, nil
	}
	s.uc.ResumeFetch()
	return &problem.TogglePipelineRes{Code: 0, Message: "题面爬取已恢复", Paused: false}, nil
}

func (s *ProblemService) AdminUpdate(ctx context.Context, req *problem.AdminUpdateProblemReq) (*problem.AdminUpdateProblemRes, error) {
	// 与 ProposeEdit 特权路径一致：写审核记录并 auto-approve（站管/资源审核员）
	if !auth.HasPerm(ctx, rbac.PermContentProblemReview) {
		return &problem.AdminUpdateProblemRes{Code: 1, Message: "仅站点管理员或资源审核员可直接修改"}, nil
	}
	if req == nil || req.Id == 0 {
		return &problem.AdminUpdateProblemRes{Code: 1, Message: "题目 id 无效"}, nil
	}
	if !req.UpdateTags && !req.UpdateContent && strings.TrimSpace(req.Title) == "" && !req.UpdateDifficulty {
		return &problem.AdminUpdateProblemRes{Code: 1, Message: "没有需要修改的内容"}, nil
	}
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &problem.AdminUpdateProblemRes{Code: 1, Message: "请先登录"}, nil
	}
	_, err := s.uc.ProposeProblemEdit(
		uid, uint(req.Id),
		req.UpdateTags, req.Tags,
		req.UpdateContent, req.ContentMd,
		req.Title, "管理员/审核员直接修改",
		req.UpdateDifficulty, req.Difficulty,
		true, // auto-approve
	)
	if err != nil {
		return &problem.AdminUpdateProblemRes{Code: 1, Message: err.Error()}, nil
	}
	p, err := s.uc.Get(uint(req.Id))
	if err != nil {
		return &problem.AdminUpdateProblemRes{Code: 0, Message: "已保存并记入审核记录"}, nil
	}
	return &problem.AdminUpdateProblemRes{
		Code:    0,
		Message: "已保存并记入审核记录",
		Data:    s.toInfo(p, ""),
	}, nil
}

func (s *ProblemService) ProposeEdit(ctx context.Context, req *problem.ProposeProblemEditReq) (*problem.ProposeProblemEditRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &problem.ProposeProblemEditRes{Code: 1, Message: "请先登录"}, nil
	}
	if req == nil || req.ProblemId == 0 {
		return &problem.ProposeProblemEditRes{Code: 1, Message: "题目 id 无效"}, nil
	}
	// 站管 / 资源审核员：仍写 problem_edit_requests，创建后立即 auto-approve（「已通过」有记录、贡献统计计入）
	autoApprove := auth.HasPerm(ctx, rbac.PermContentProblemReview)
	id, err := s.uc.ProposeProblemEdit(
		uid, uint(req.ProblemId),
		req.UpdateTags, req.Tags,
		req.UpdateContent, req.ContentMd,
		req.Title, req.Note,
		req.UpdateDifficulty, req.Difficulty,
		autoApprove,
	)
	if err != nil {
		return &problem.ProposeProblemEditRes{Code: 1, Message: err.Error()}, nil
	}
	if autoApprove {
		return &problem.ProposeProblemEditRes{
			Code:      0,
			Message:   "已保存并记入审核记录",
			RequestId: uint32(id),
		}, nil
	}
	return &problem.ProposeProblemEditRes{
		Code:      0,
		Message:   "已提交，等待审核",
		RequestId: uint32(id),
	}, nil
}

func (s *ProblemService) toEditInfo(r *model.ProblemEditRequest, p *model.Problem, userName string) *problem.ProblemEditInfo {
	if r == nil {
		return nil
	}
	info := &problem.ProblemEditInfo{
		Id:                 uint32(r.ID),
		ProblemId:          uint32(r.ProblemID),
		UserId:             uint32(r.UserID),
		UserName:           userName,
		HasTags:            r.HasTags,
		HasContent:         r.HasContent,
		HasDifficulty:      r.HasDifficulty,
		ProposedTags:       []string(r.ProposedTags),
		ProposedContentMd:  r.ProposedContentMD,
		ProposedTitle:      r.ProposedTitle,
		ProposedDifficulty: r.ProposedDifficulty,
		Note:               r.Note,
		Status:             r.Status,
		ReviewNote:         r.ReviewNote,
		CreatedAt:          r.CreatedAt.Unix(),
		UpdatedAt:          r.UpdatedAt.Unix(),
	}
	if r.ReviewerID != nil {
		info.ReviewerId = uint32(*r.ReviewerID)
	}
	if info.ProposedTags == nil {
		info.ProposedTags = []string{}
	}
	if p != nil {
		info.Platform = p.Platform
		info.ExternalId = p.ExternalID
		info.ProblemTitle = cleanDisplayTitle(p.Title)
		info.CurrentTags = []string(p.Tags)
		if info.CurrentTags == nil {
			info.CurrentTags = []string{}
		}
		info.CurrentContentMd = p.ContentMD
		info.CurrentTitle = cleanDisplayTitle(p.Title)
		info.CurrentDifficulty = p.Difficulty
	}
	return info
}

func (s *ProblemService) ListEditRequests(ctx context.Context, req *problem.ListProblemEditReq) (*problem.ListProblemEditRes, error) {
	if !auth.HasPerm(ctx, rbac.PermContentProblemReview) {
		return &problem.ListProblemEditRes{Code: 1, Message: "仅站点管理员或资源审核员可查看审核列表"}, nil
	}
	page, ps := int64(1), int64(20)
	status := ""
	if req != nil {
		page = req.Page
		ps = req.PageSize
		status = req.Status
	}
	list, total, probMap, err := s.uc.ListProblemEditRequests(page, ps, status)
	if err != nil {
		return nil, errors.InternalServer("list edit requests failed", "service unavailable")
	}
	uids := make([]int64, 0, len(list))
	seen := map[int64]bool{}
	for _, r := range list {
		uid := int64(r.UserID)
		if uid > 0 && !seen[uid] {
			seen[uid] = true
			uids = append(uids, uid)
		}
	}
	names := s.fetchUserNames(ctx, uids)
	data := make([]*problem.ProblemEditInfo, 0, len(list))
	for i := range list {
		r := &list[i]
		name := names[int64(r.UserID)]
		data = append(data, s.toEditInfo(r, probMap[r.ProblemID], name))
	}
	if page <= 0 {
		page = 1
	}
	if ps <= 0 {
		ps = 20
	}
	return &problem.ListProblemEditRes{
		Code:     0,
		Message:  "success",
		Data:     data,
		Total:    total,
		Page:     page,
		PageSize: ps,
	}, nil
}

func (s *ProblemService) ReviewEdit(ctx context.Context, req *problem.ReviewProblemEditReq) (*problem.ReviewProblemEditRes, error) {
	if !auth.HasPerm(ctx, rbac.PermContentProblemReview) {
		return &problem.ReviewProblemEditRes{Code: 1, Message: "仅站点管理员或资源审核员可审核"}, nil
	}
	if req == nil || req.Id == 0 {
		return &problem.ReviewProblemEditRes{Code: 1, Message: "申请 id 无效"}, nil
	}
	uid := auth.GetCurrentUserId(ctx)
	if err := s.uc.ReviewProblemEdit(uint(req.Id), uid, req.Approve, req.ReviewNote); err != nil {
		return &problem.ReviewProblemEditRes{Code: 1, Message: err.Error()}, nil
	}
	msg := "已驳回"
	if req.Approve {
		msg = "已通过并应用修改"
	}
	return &problem.ReviewProblemEditRes{Code: 0, Message: msg}, nil
}

func (s *ProblemService) MyPendingEdit(ctx context.Context, req *problem.MyPendingEditReq) (*problem.MyPendingEditRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &problem.MyPendingEditRes{Code: 1, Message: "请先登录"}, nil
	}
	if req == nil || req.ProblemId == 0 {
		return &problem.MyPendingEditRes{Code: 1, Message: "题目 id 无效"}, nil
	}
	r, err := s.uc.MyPendingProblemEdit(uid, uint(req.ProblemId))
	if err != nil {
		return nil, errors.InternalServer("query failed", "service unavailable")
	}
	if r == nil {
		return &problem.MyPendingEditRes{Code: 0, Message: "success", HasPending: false}, nil
	}
	p, _ := s.uc.Get(r.ProblemID)
	return &problem.MyPendingEditRes{
		Code:       0,
		Message:    "success",
		HasPending: true,
		Data:       s.toEditInfo(r, p, ""),
	}, nil
}
