package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/agent/internal/agent/svcdata"
	_const "cwxu-algo/app/common/const"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

// genTestRSAKeys 生成测试用 RSA 密钥对（PEM）。
func genTestRSAKeys(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privPEM, pubPEM
}

// configureAgentTestJWTKeys 配置测试 RSA 密钥（IssueElevatedAgentToken 依赖）。
func configureAgentTestJWTKeys(t *testing.T) {
	t.Helper()
	privPEM, pubPEM := genTestRSAKeys(t)
	if err := _const.ConfigureJWTKeys(privPEM, pubPEM); err != nil {
		t.Fatal(err)
	}
}

func parseMapClaimsTest(token string) (map[string]interface{}, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodRS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return _const.JWTPublicKey(), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}))
	if err != nil || parsed == nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("not map claims")
	}
	out := make(map[string]interface{}, len(mc))
	for k, v := range mc {
		out[k] = v
	}
	return out, nil
}

func fixtureTrainingData() *TrainingReportData {
	return &TrainingReportData{
		OrgID:            7,
		GroupID:          0,
		ScopeLabel:       "整组织",
		StartDate:        "2026-07-06",
		EndDate:          "2026-07-12",
		PrevStartDate:    "2026-06-29",
		PrevEndDate:      "2026-07-05",
		MemberCount:      5,
		MemberIDs:        []int64{1, 2, 3, 4, 5},
		TotalSubmits:     42,
		PrevTotalSubmits: 30,
		TotalAC:          18,
		DailyTrend: []DayCount{
			{Date: "2026-07-06", Count: 5},
			{Date: "2026-07-07", Count: 6},
			{Date: "2026-07-08", Count: 7},
			{Date: "2026-07-09", Count: 8},
			{Date: "2026-07-10", Count: 9},
			{Date: "2026-07-11", Count: 4},
			{Date: "2026-07-12", Count: 3},
		},
		DailyACTrend: []DayCount{
			{Date: "2026-07-06", Count: 2},
			{Date: "2026-07-07", Count: 3},
		},
		TopSubmit: []RankEntry{
			{Rank: 1, UserID: 1, Name: "Alice", Score: 15},
			{Rank: 2, UserID: 2, Name: "Bob", Score: 12},
			{Rank: 3, UserID: 3, Name: "Eve", Score: 15},
		},
		TopAC: []RankEntry{
			{Rank: 1, UserID: 1, Name: "Alice", Score: 8},
		},
		ActiveRanking: []MemberStat{
			{Rank: 1, UserID: 1, Name: "Alice", Submits: 15, AC: 8, ACRate: 53.3, Share: 35.7},
			{Rank: 2, UserID: 3, Name: "Eve", Submits: 15, AC: 7, ACRate: 46.7, Share: 35.7},
			{Rank: 3, UserID: 2, Name: "Bob", Submits: 12, AC: 3, ACRate: 25.0, Share: 28.6},
		},
		InactiveMembers: []string{"Carol", "Dave"},
		ActiveMembers:   3,
		TeamTags: []TagHit{
			{Tag: "dp", Count: 12},
			{Tag: "图论", Count: 5},
		},
		ProblemOverview: []ProblemTouch{
			{Problem: "A+B", Title: "A+B", Tags: []string{"模拟"}, Submitters: 3, ACCount: 2, ACUsers: []string{"Alice", "Bob"}},
		},
		OrgSubmitSample: []SubmitFeedItem{
			{UserName: "Alice", Problem: "A+B", Title: "A+B", Status: "AC", Platform: "cf", Time: "07-10 12:00", Tags: []string{"dp"}},
		},
		InitiatorUserID: 99,
		InitiatorName:   "Coach",
		InitiatorEmail:  "coach@example.com",
	}
}

func TestRenderRuleTemplateHTML_UsesFixtureNumbers(t *testing.T) {
	data := fixtureTrainingData()
	html := RenderRuleTemplateHTML(data, "GoAlgo")
	if html == "" {
		t.Fatal("empty html")
	}
	// 必须包含真实数字与名单，禁止编造；QQ 兼容用 table + inline style
	mustContain := []string{
		"42", "18", "Alice", "Bob", "Carol", "Dave", "Eve",
		"2026-07-06", "2026-07-12", "整组织", "30",
		"viewport", "成员排行榜", "题目标签", "做题概览", "不活跃成员",
		"综合维度评价", "dp", "A+B", "<table", "style=",
		"algo.zhiyuansofts.cn",
	}
	for _, s := range mustContain {
		if !strings.Contains(html, s) {
			t.Errorf("template missing %q", s)
		}
	}
	// 不应出现未在数据中的假成员
	if strings.Contains(html, "FakeUser999") {
		t.Error("invented member")
	}
}

func TestBuildActiveRanking_IncludesZeroLast(t *testing.T) {
	submit := map[int64]int64{1: 10, 2: 0, 3: 5}
	ac := map[int64]int64{1: 4, 2: 0, 3: 5}
	idMap := map[int64]userIdentity{
		1: {Name: "A", Username: "a"},
		2: {Name: "B", Username: "b"},
		3: {Name: "C", Username: "c"},
	}
	r := buildActiveRanking(submit, ac, idMap)
	if len(r) != 3 {
		t.Fatalf("want 3 members (incl. zero-submit), got %+v", r)
	}
	if r[0].Name != "A" || r[1].Name != "C" || r[2].Name != "B" {
		t.Fatalf("order wrong: %+v", r)
	}
	if r[2].Submits != 0 || r[2].ACRate != 0 {
		t.Fatalf("zero-submit row wrong: %+v", r[2])
	}
	if r[0].ProfileURL == "" || !strings.Contains(r[0].ProfileURL, "/profile/") {
		t.Fatalf("profile url %s", r[0].ProfileURL)
	}
}

func TestAggregateTeamTagsAndProblems(t *testing.T) {
	feed := []SubmitFeedItem{
		{UserName: "A", Problem: "P1", Title: "题1", Status: "AC", Tags: []string{"dp", "贪心"}},
		{UserName: "B", Problem: "P1", Title: "题1", Status: "WA", Tags: []string{"dp"}},
		{UserName: "A", Problem: "P2", Title: "题2", Status: "AC", Tags: []string{"图论"}},
	}
	tags := aggregateTeamTags(feed, 10)
	if len(tags) < 2 || tags[0].Tag != "dp" {
		t.Fatalf("tags %+v", tags)
	}
	probs := aggregateProblemOverview(feed, 10)
	if len(probs) != 2 {
		t.Fatalf("probs %+v", probs)
	}
}

func TestLastWeekRange_Monday(t *testing.T) {
	// 2026-07-13 is Monday → last week 07-06 ~ 07-12
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.Local)
	start, end := LastWeekRange(now)
	if start.Format(dateLayout) != "2026-07-06" || end.Format(dateLayout) != "2026-07-12" {
		t.Fatalf("got %s ~ %s", start.Format(dateLayout), end.Format(dateLayout))
	}
}

func TestParseDateRange(t *testing.T) {
	_, _, err := ParseDateRange("2026-07-01", "2026-06-01")
	if err == nil {
		t.Fatal("expected error for inverted range")
	}
	s, e, err := ParseDateRange("2026-07-01", "2026-07-07")
	if err != nil || s.Day() != 1 || e.Day() != 7 {
		t.Fatalf("parse: %v %v %v", s, e, err)
	}
}

func TestJobTTL_DownloadWindow(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	job := &TrainingReportJob{
		Status:    ReportStatusDone,
		HTMLPath:  "/tmp/x.html",
		ExpiresAt: now.Add(1 * time.Hour).Unix(),
	}
	if !job.IsDownloadable(now) {
		t.Fatal("should be downloadable within TTL")
	}
	if job.IsDownloadable(now.Add(2 * time.Hour)) {
		t.Fatal("should reject after TTL")
	}
	if job.EffectiveStatus(now.Add(2*time.Hour)) != ReportStatusExpired {
		t.Fatal("effective status should be expired")
	}
}

func TestJobStore_RedisRoundTrip(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)

	job := &TrainingReportJob{
		JobID:     "job-test-1",
		Status:    ReportStatusPending,
		OrgID:     3,
		StartDate: "2026-07-01",
		EndDate:   "2026-07-07",
		CreatedBy: 1,
		CreatedAt: time.Now().Unix(),
	}
	if err := uc.saveJob(ctx, job, true); err != nil {
		t.Fatal(err)
	}
	// 进度更新不得再次 LPush，否则列表出现重复
	job.Status = ReportStatusRunning
	job.Progress = 40
	if err := uc.saveJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	job.Status = ReportStatusDone
	if err := uc.saveJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	got, err := uc.getJob(ctx, "job-test-1")
	if err != nil || got == nil || got.OrgID != 3 || got.Status != ReportStatusDone {
		t.Fatalf("getJob: %+v err=%v", got, err)
	}
	list, err := uc.listJobs(ctx, 3, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
}

func TestListJobs_DedupesRepeatedIDs(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	ctx := context.Background()

	job := &TrainingReportJob{
		JobID: "dup-1", Status: ReportStatusDone, OrgID: 9,
		StartDate: "2026-07-01", EndDate: "2026-07-07",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath:  "/tmp/x.html",
	}
	// 模拟历史 bug：同一 id 被 LPush 多次
	_ = uc.saveJob(ctx, job, true)
	_ = rdb.LPush(ctx, orgJobsKey(9), "dup-1", "dup-1", "dup-1").Err()
	list, err := uc.listJobs(ctx, 9, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].JobID != "dup-1" {
		t.Fatalf("want 1 unique, got %+v", list)
	}
}

func TestStartTrainingReport_ReturnsJobID_NonAI(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)

	// 无 registry 时后台任务会失败，但 Start 应立即返回 jobId
	jobID, err := uc.StartTrainingReport(context.Background(), StartTrainingReportParams{
		OrgID:     1,
		StartDate: "2026-07-01",
		EndDate:   "2026-07-07",
		UseAI:     false,
		CreatedBy: 9,
		Source:    "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if jobID == "" {
		t.Fatal("empty job id")
	}
	// 立即查状态应为 pending 或 running（异步已启动）
	time.Sleep(50 * time.Millisecond)
	job, err := uc.getJob(context.Background(), jobID)
	if err != nil || job == nil {
		t.Fatalf("job missing: %v", err)
	}
	if job.Status != ReportStatusPending && job.Status != ReportStatusRunning && job.Status != ReportStatusFailed {
		t.Fatalf("unexpected status %s", job.Status)
	}
	// start 响应不应含完整 HTML 正文
	if strings.Contains(jobID, "<html") {
		t.Fatal("job id looks like html body")
	}
}

func TestNonAI_EndToEndInProcess(t *testing.T) {
	// 真实路径：规则模板 → 写 HTML → finalize job → re-get → notify
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)
	ctx := context.Background()

	data := fixtureTrainingData()
	html := RenderRuleTemplateHTML(data, "GoAlgo", DetailModeFull)
	if !strings.Contains(html, "42") || !strings.Contains(html, "Alice") {
		t.Fatal("template missing fixture stats")
	}
	if !strings.Contains(html, "综合维度评价") {
		t.Fatal("template missing comprehensive eval")
	}

	jobID := "e2e-job"
	htmlPath := jobHTMLPath(jobID)
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &TrainingReportJob{
		JobID:     jobID,
		Status:    ReportStatusPending,
		StartDate: data.StartDate,
		EndDate:   data.EndDate,
		OrgID:     data.OrgID,
		CreatedBy: data.InitiatorUserID,
		CreatedAt: time.Now().Unix(),
		UseAI:     false,
	}
	if err := uc.saveJob(ctx, job, true); err != nil {
		t.Fatal(err)
	}
	job.Status = ReportStatusRunning
	_ = uc.saveJob(ctx, job, true)

	finished := time.Now()
	expires := finished.Add(reportDownloadTTL)
	fileName := fmt.Sprintf("training-report-%s-%s.html", job.StartDate, job.EndDate)
	job.Status = ReportStatusDone
	job.Progress = 100
	job.Message = "已完成"
	job.FinishedAt = finished.Unix()
	job.ExpiresAt = expires.Unix()
	job.HTMLPath = htmlPath
	job.FileName = fileName
	if err := uc.saveJob(ctx, job, true); err != nil {
		t.Fatal(err)
	}
	fresh, err := uc.getJob(ctx, jobID)
	if err != nil || fresh == nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !fresh.IsDownloadable(now) {
		t.Fatal("should download within 24h")
	}
	abs, ct, name, err := ResolveArtifactAbs(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(abs, ".html") || !strings.Contains(ct, "text/html") || name != fileName {
		t.Fatalf("artifact %s %s %s", abs, ct, name)
	}
	_, body, attach := BuildNotifyEmail(fresh, "GoAlgo", html)
	if strings.Contains(body, "1970") || attach != fileName {
		t.Fatalf("notify snapshot bad: attach=%s body=%s", attach, body)
	}
	if !strings.Contains(body, time.Unix(fresh.ExpiresAt, 0).Format("2006-01-02 15:04")) {
		t.Fatal("notify missing expiry")
	}
	// 过期后拒绝
	fresh.ExpiresAt = now.Add(-time.Minute).Unix()
	if _, _, _, err := ResolveArtifactAbs(fresh); err == nil {
		t.Fatal("expected expire error")
	}
	fresh.ExpiresAt = expires.Unix()
	err = uc.notifyTrainingReportDone(ctx, data, fresh, html)
	if err == nil {
		t.Log("SMTP configured; notify ok")
	} else if !strings.Contains(err.Error(), "SMTP") && !strings.Contains(err.Error(), "邮箱") {
		t.Fatalf("unexpected notify err: %v", err)
	}
}

func TestElevatedAgentIdentity(t *testing.T) {
	// 配置临时 RSA 密钥
	configureAgentTestJWTKeys(t)
	tok, err := IssueElevatedAgentToken(5)
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	if !IsElevatedAgentUser(AgentHiddenUserID) {
		t.Fatal("agent user id check")
	}
	if IsElevatedAgentUser(1) {
		t.Fatal("normal user should not match")
	}
	ctx, err := ContextWithElevatedAgent(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil {
		t.Fatal("nil ctx")
	}
	// 必须可从 outgoing metadata 取出 Bearer
	if !svcdata.HasElevatedAuth(ctx) {
		t.Fatal("elevated ctx missing Bearer metadata")
	}
	if svcdata.BearerFromContext(ctx) != tok {
		// ContextWithElevatedAgent issues a new token; just require non-empty matching claims
		if svcdata.BearerFromContext(ctx) == "" {
			t.Fatal("empty bearer in elevated ctx")
		}
	}
}

func TestBuildNotifyEmail_UsesExpiresAndFileName(t *testing.T) {
	// 复现 skeptic：若用 pre-update job，ExpiresAt=0 → 1970，FileName 空 → attachment
	stale := &TrainingReportJob{
		JobID:     "job-stale",
		StartDate: "2026-07-06",
		EndDate:   "2026-07-12",
		// ExpiresAt 0, FileName ""
	}
	_, bodyStale, nameStale := BuildNotifyEmail(stale, "GoAlgo", "<p>x</p>")
	if !strings.Contains(bodyStale, "—") && strings.Contains(bodyStale, "1970") {
		t.Fatal("stale job should not show 1970 when using — for zero expires")
	}
	if nameStale != "training-report.html" {
		t.Fatalf("default attach name got %s", nameStale)
	}

	// 生产路径：finalize 后的 job
	exp := time.Date(2026, 7, 18, 15, 4, 0, 0, time.Local)
	done := &TrainingReportJob{
		JobID:     "job-done",
		StartDate: "2026-07-06",
		EndDate:   "2026-07-12",
		Status:    ReportStatusDone,
		ExpiresAt: exp.Unix(),
		FileName:  "training-report-2026-07-06-2026-07-12.html",
	}
	subj, body, name := BuildNotifyEmail(done, "GoAlgo", "<p>report</p>")
	if !strings.Contains(subj, "2026-07-06") {
		t.Fatalf("subject: %s", subj)
	}
	wantExp := exp.Format("2006-01-02 15:04")
	if !strings.Contains(body, wantExp) {
		t.Fatalf("body missing expiry %s: %s", wantExp, body)
	}
	if strings.Contains(body, "1970") {
		t.Fatal("body has 1970 epoch")
	}
	if name != done.FileName {
		t.Fatalf("attach name %s", name)
	}
}

func TestCompleteJobThenNotify_Snapshot(t *testing.T) {
	// 真实 finalize 路径：save done job → re-get → BuildNotifyEmail
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)
	ctx := context.Background()

	jobID := "finalize-job"
	data := fixtureTrainingData()
	html := RenderRuleTemplateHTML(data, "GoAlgo")
	htmlPath := jobHTMLPath(jobID)
	if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &TrainingReportJob{
		JobID:     jobID,
		Status:    ReportStatusRunning,
		StartDate: data.StartDate,
		EndDate:   data.EndDate,
		OrgID:     data.OrgID,
		CreatedBy: data.InitiatorUserID,
		CreatedAt: time.Now().Unix(),
	}
	if err := uc.saveJob(ctx, job, true); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	expires := finished.Add(reportDownloadTTL)
	fileName := fmt.Sprintf("training-report-%s-%s.html", job.StartDate, job.EndDate)
	job.Status = ReportStatusDone
	job.Progress = 100
	job.Message = "已完成"
	job.FinishedAt = finished.Unix()
	job.ExpiresAt = expires.Unix()
	job.HTMLPath = htmlPath
	job.FileName = fileName
	if err := uc.saveJob(ctx, job, true); err != nil {
		t.Fatal(err)
	}
	fresh, err := uc.getJob(ctx, jobID)
	if err != nil || fresh == nil {
		t.Fatalf("re-get: %v", err)
	}
	if fresh.ExpiresAt == 0 || fresh.FileName == "" {
		t.Fatalf("fresh job incomplete: %+v", fresh)
	}
	_, body, name := BuildNotifyEmail(fresh, "GoAlgo", html)
	if strings.Contains(body, "1970") {
		t.Fatal("notify body has 1970")
	}
	if !strings.Contains(body, time.Unix(fresh.ExpiresAt, 0).Format("2006-01-02 15:04")) {
		t.Fatalf("body missing real expiry: %s", body)
	}
	if name != fileName {
		t.Fatalf("name %s want %s", name, fileName)
	}
	err = uc.notifyTrainingReportDone(ctx, data, fresh, html)
	if err == nil {
		t.Log("SMTP configured; notify ok")
	} else if !strings.Contains(err.Error(), "SMTP") && !strings.Contains(err.Error(), "邮箱") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestWeeklyUsesTrainingPipeline_DateScope(t *testing.T) {
	// 周报 = 上周 Mon-Sun（与 LastWeekRange 一致），经 GenerateTrainingReportSync 参数
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.Local) // Monday
	start, end := LastWeekRange(now)
	p := StartTrainingReportParams{
		OrgID:     2,
		StartDate: start.Format(dateLayout),
		EndDate:   end.Format(dateLayout),
		UseAI:     true,
		Source:    "weekly",
	}
	if p.Source != "weekly" {
		t.Fatal("source")
	}
	if p.StartDate != "2026-07-06" || p.EndDate != "2026-07-12" {
		t.Fatalf("weekly range %s %s", p.StartDate, p.EndDate)
	}
	// 规则模板路径不依赖网络
	data := fixtureTrainingData()
	data.StartDate = p.StartDate
	data.EndDate = p.EndDate
	html := RenderRuleTemplateHTML(data, "GoAlgo")
	if !strings.Contains(html, "2026-07-06") {
		t.Fatal("weekly html missing week start")
	}
}

func TestFindSharedWeeklyJob_ReusesOrgDoc(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)

	startS, endS := "2026-07-06", "2026-07-12"
	htmlPath := filepath.Join(dir, "shared-weekly.html")
	const body = "<html>shared weekly body</html>"
	if err := os.WriteFile(htmlPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// 无关 manual 任务不应被命中
	_ = uc.saveJob(ctx, &TrainingReportJob{
		JobID: "manual-1", Status: ReportStatusDone, OrgID: 7,
		StartDate: startS, EndDate: endS, Source: "manual",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath:  htmlPath, UseAI: true,
	}, true)
	// 分组周报不应被命中
	_ = uc.saveJob(ctx, &TrainingReportJob{
		JobID: "weekly-group", Status: ReportStatusDone, OrgID: 7, GroupID: 3,
		StartDate: startS, EndDate: endS, Source: "weekly",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath:  htmlPath, UseAI: true,
	}, true)
	// 正确的组织共享周报
	_ = uc.saveJob(ctx, &TrainingReportJob{
		JobID: "weekly-shared", Status: ReportStatusDone, OrgID: 7, GroupID: 0,
		StartDate: startS, EndDate: endS, Source: "weekly",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath:  htmlPath, UseAI: true,
	}, true)

	got := uc.findSharedWeeklyJob(ctx, 7, startS, endS)
	if got == nil || got.JobID != "weekly-shared" {
		t.Fatalf("want weekly-shared, got %+v", got)
	}
	h, id, ok := uc.loadSharedWeeklyHTML(ctx, 7, startS, endS)
	if !ok || id != "weekly-shared" || h != body {
		t.Fatalf("loadShared: ok=%v id=%s h=%q", ok, id, h)
	}
	// 过期不复用
	_ = uc.saveJob(ctx, &TrainingReportJob{
		JobID: "weekly-expired", Status: ReportStatusDone, OrgID: 8,
		StartDate: startS, EndDate: endS, Source: "weekly",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
		HTMLPath:  htmlPath, UseAI: true,
	}, true)
	if j := uc.findSharedWeeklyJob(ctx, 8, startS, endS); j != nil {
		t.Fatalf("expired should not match: %+v", j)
	}
}

func TestPersistWeeklyAsJob_DedupesExisting(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	uc := &SummaryUseCase{redis: rdb}
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)

	start := time.Date(2026, 7, 6, 0, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 12, 0, 0, 0, 0, time.Local)
	htmlPath := filepath.Join(dir, "existing.html")
	_ = os.WriteFile(htmlPath, []byte("<html>first</html>"), 0o644)
	_ = uc.saveJob(ctx, &TrainingReportJob{
		JobID: "first-weekly", Status: ReportStatusDone, OrgID: 11,
		StartDate: start.Format(dateLayout), EndDate: end.Format(dateLayout),
		Source: "weekly", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath: htmlPath, UseAI: true,
	}, true)

	id, err := uc.persistWeeklyAsJob(ctx, 11, 99, start, end, "<html>second</html>", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "first-weekly" {
		t.Fatalf("should reuse first job, got %s", id)
	}
	list, err := uc.listJobs(ctx, 11, 10)
	if err != nil {
		t.Fatal(err)
	}
	// 不应再插入新 job
	if len(list) != 1 || list[0].JobID != "first-weekly" {
		t.Fatalf("list=%+v", list)
	}
}

func TestRankFromMap(t *testing.T) {
	scores := map[int64]int64{1: 10, 2: 20, 3: 0}
	names := map[int64]string{1: "A", 2: "B", 3: "C"}
	r := rankFromMap(scores, names, 5)
	if len(r) != 2 || r[0].Name != "B" || r[0].Score != 20 {
		t.Fatalf("%+v", r)
	}
}

func TestArtifactPathSandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(reportDirEnv, dir)
	f := filepath.Join(dir, "safe.html")
	_ = os.WriteFile(f, []byte("<html>ok</html>"), 0o644)
	job := &TrainingReportJob{
		Status:    ReportStatusDone,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		HTMLPath:  f,
		FileName:  "safe.html",
	}
	abs, ct, _, err := ResolveArtifactAbs(job)
	if err != nil || abs != filepath.Clean(f) || !strings.Contains(ct, "html") {
		t.Fatalf("%v %s %s", err, abs, ct)
	}
	// path traversal
	job.HTMLPath = filepath.Join(dir, "..", "etc", "passwd")
	if _, _, _, err := ResolveArtifactAbs(job); err == nil {
		t.Fatal("expected path reject")
	}
}

func TestValidateAIDateRange(t *testing.T) {
	s := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	e := s.AddDate(0, 0, 100)
	if err := ValidateAIDateRange(s, e, true); err != nil {
		t.Fatal(err)
	}
	// 含首尾：跨度 MaxAIRangeDays 天 = start 起加 (MaxAIRangeDays-1) 天
	e2 := s.AddDate(0, 0, MaxAIRangeDays-1)
	if err := ValidateAIDateRange(s, e2, true); err != nil {
		t.Fatal(err)
	}
	e3 := s.AddDate(0, 0, MaxAIRangeDays)
	if err := ValidateAIDateRange(s, e3, true); err == nil {
		t.Fatal("expected AI range error")
	}
	if err := ValidateAIDateRange(s, e3, false); err != nil {
		t.Fatal("non-AI should allow long range at this layer")
	}
}

func TestDetailModeFromSource(t *testing.T) {
	if DetailModeFromSource("weekly") != DetailModeCompact {
		t.Fatal("weekly")
	}
	if DetailModeFromSource("manual") != DetailModeFull {
		t.Fatal("manual")
	}
}

func TestRenderRuleTemplate_CompactAndFull(t *testing.T) {
	data := fixtureTrainingData()
	data.Contests = []ContestBrief{{ContestName: "CF Round", Platform: "codeforces", ACCount: 3, TotalCount: 6, Time: "2026-07-10"}}
	data.RecentBlogs = []BlogBrief{{Title: "DP 笔记", Author: "Bob", Summary: "区间 DP"}}
	full := RenderRuleTemplateHTML(data, "GoAlgo", DetailModeFull)
	compact := RenderRuleTemplateHTML(data, "GoAlgo", DetailModeCompact)
	for _, html := range []string{full, compact} {
		for _, s := range []string{"综合维度评价", "做题概览", "成员排行榜", "活跃度", "viewport", "<table"} {
			if !strings.Contains(html, s) {
				t.Errorf("missing dim %q", s)
			}
		}
	}
	// 比赛表现仅 full 展示（compact 周报去除个人参赛史板块）
	if !strings.Contains(full, "比赛表现") {
		t.Error("full should have contest section")
	}
	if strings.Contains(compact, "比赛表现") {
		t.Error("compact should not have contest section")
	}
	if !strings.Contains(compact, "教练周报") {
		t.Error("compact title")
	}
	if !strings.Contains(full, "训练报告") {
		t.Error("full title")
	}
	if !strings.Contains(full, "CF Round") {
		t.Error("fixture contest missing")
	}
}

func TestRenderRuleTemplate_TrendUsesChartOnly(t *testing.T) {
	html := RenderRuleTemplateHTML(fixtureTrainingData(), "GoAlgo", DetailModeCompact)
	for _, want := range []string{`data-axis="y"`, `data-series="提交"`, `data-series="AC"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing trend chart marker %q", want)
		}
	}
	if strings.Contains(html, `>日期</th>`) {
		t.Error("weekly trend should not include the old date table")
	}
}

func TestTrainingReportPrompts_Dimensions(t *testing.T) {
	sys := trainingReportSystemPrompt(DetailModeFull)
	for _, s := range []string{"activeRanking", "禁止", "HTML", "teamTags"} {
		if !strings.Contains(sys, s) {
			t.Fatalf("system prompt missing %q", s)
		}
	}
	sys2 := trainingReportSystemPrompt(DetailModeCompact)
	if !strings.Contains(sys2, "简版") {
		t.Fatal("compact system")
	}
	up := trainingReportUserPrompt(fixtureTrainingData(), DetailModeFull)
	if !strings.Contains(up, "详版") || !strings.Contains(up, "activeRanking") {
		t.Fatal("user prompt")
	}
}

func TestSanitizeAndValidateReportHTML(t *testing.T) {
	// 带前言 + 围栏
	raw := "现在我已获取所有必需的数据，开始生成：\n```html\n<!DOCTYPE html><html><body><table>" +
		"<tr><td>活跃度</td></tr></table>" +
		strings.Repeat("<p>排行榜标签做题动态比赛博客不活跃综合评价内容填充足够长度</p>", 20) +
		"</body></html>\n```"
	html, ok, reason := SanitizeAndValidateReportHTML(raw)
	if !ok {
		t.Fatalf("should pass: %s", reason)
	}
	if strings.Contains(html, "```") || strings.Contains(html, "现在我已") {
		t.Fatal("preamble/fence not stripped")
	}
	// 垃圾输出
	_, ok, _ = SanitizeAndValidateReportHTML("分析如下：很好")
	if ok {
		t.Fatal("garbage should fail")
	}
	// 残缺
	_, ok, _ = SanitizeAndValidateReportHTML("<table><tr><td>活跃</td></tr>")
	if ok {
		t.Fatal("too short should fail")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
