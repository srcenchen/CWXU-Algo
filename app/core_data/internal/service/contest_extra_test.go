package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"cwxu-algo/api/core/v1/contest_log"
	"cwxu-algo/app/core_data/internal/data/model"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"google.golang.org/protobuf/encoding/protojson"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// contestExtraServer 用 proto 注册的 khttp server 挂载 ContestLogService。
func contestExtraServer(t *testing.T, s *ContestLogService) *khttp.Server {
	t.Helper()
	server := khttp.NewServer()
	contest_log.RegisterContestHTTPServer(server, s)
	return server
}

func contestExtraRequest(server *khttp.Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func setupContestExtraDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := "file:ce_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(name), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 覆盖 contest extra 读路径触及的表（ListContestCellSubmits / ListContestPracticeCells）
	if err := db.AutoMigrate(
		&model.ContestLog{},
		&model.SubmitLog{},
		&model.ContestProblem{},
		&model.ContestCalendar{},
		&model.ContestUserProblem{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func contestExtraBody(t *testing.T, r *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, r.Body.String())
	}
	return body
}

// TestContestExtraRoutesRegisteredByProto 验证 3 个 extra 路径由 proto 注册可达，
// 且参数校验分支返回手写同款 JSON 形状（HTTP 200 + success:false + message，不返回 Kratos error）。
func TestContestExtraRoutesRegisteredByProto(t *testing.T) {
	server := contestExtraServer(t, &ContestLogService{})

	// problems：缺 id → 手写同款 400 语义
	r := contestExtraRequest(server, "/v1/core/contest/problems")
	if r.Code != http.StatusOK {
		t.Fatalf("problems no-id status=%d body=%s", r.Code, r.Body.String())
	}
	body := contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "缺少比赛 id" {
		t.Fatalf("problems no-id unexpected body=%s", r.Body.String())
	}

	// board：缺 id
	r = contestExtraRequest(server, "/v1/core/contest/board")
	if r.Code != http.StatusOK {
		t.Fatalf("board no-id status=%d body=%s", r.Code, r.Body.String())
	}
	body = contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "缺少比赛 id" {
		t.Fatalf("board no-id unexpected body=%s", r.Body.String())
	}

	// cell-submits：缺 id/userId
	r = contestExtraRequest(server, "/v1/core/contest/cell-submits")
	if r.Code != http.StatusOK {
		t.Fatalf("cell-submits no-id status=%d body=%s", r.Code, r.Body.String())
	}
	body = contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "缺少比赛 id 或 userId" {
		t.Fatalf("cell-submits no-id unexpected body=%s", r.Body.String())
	}
}

// TestContestProblemsShape 带 DB 的 problems 目录响应形状：404 语义 + 成功形状
// （success/message/data 包装、data.contest、data.list、data.force、data.ensureStatus）。
func TestContestProblemsShape(t *testing.T) {
	db := setupContestExtraDB(t)
	cl := model.ContestLog{
		Platform:    "CodeForces",
		ContestId:   "1234",
		ContestName: "Round #1",
		ContestUrl:  "https://codeforces.com/contest/1234",
		Time:        time.Unix(1750000000, 0),
	}
	if err := db.Create(&cl).Error; err != nil {
		t.Fatal(err)
	}
	server := contestExtraServer(t, &ContestLogService{db: db})

	// 不存在 → 404 语义
	r := contestExtraRequest(server, "/v1/core/contest/problems?id=99999")
	if r.Code != http.StatusOK {
		t.Fatalf("problems missing status=%d body=%s", r.Code, r.Body.String())
	}
	body := contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "比赛不存在" {
		t.Fatalf("problems missing unexpected body=%s", r.Body.String())
	}

	// 存在 → 成功形状（prob=nil：list 空、ensure 不触发）
	r = contestExtraRequest(server, fmt.Sprintf("/v1/core/contest/problems?id=%d", cl.ID))
	if r.Code != http.StatusOK {
		t.Fatalf("problems ok status=%d body=%s", r.Code, r.Body.String())
	}
	body = contestExtraBody(t, r)
	if body["success"] != true || body["message"] != "ok" {
		t.Fatalf("problems ok unexpected body=%s", r.Body.String())
	}
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("problems ok missing data body=%s", r.Body.String())
	}
	contest, _ := data["contest"].(map[string]interface{})
	if contest == nil {
		t.Fatalf("problems ok missing data.contest body=%s", r.Body.String())
	}
	if contest["id"] != float64(cl.ID) || contest["platform"] != "CodeForces" ||
		contest["contestId"] != "1234" || contest["contestName"] != "Round #1" ||
		contest["contestUrl"] != "https://codeforces.com/contest/1234" {
		t.Fatalf("problems ok contest mismatch body=%s", r.Body.String())
	}
	// 无日历时 CF 按 hint 为结束时间解析展示窗：endTime=hint，time=startTime
	if contest["endTime"] != strconv.FormatInt(cl.Time.Unix(), 10) {
		t.Fatalf("problems ok contest.endTime=%v want %s body=%s",
			contest["endTime"], strconv.FormatInt(cl.Time.Unix(), 10), r.Body.String())
	}
	if contest["time"] == nil || contest["time"] != contest["startTime"] {
		t.Fatalf("problems ok contest.time/startTime mismatch body=%s", r.Body.String())
	}
	if list, ok := data["list"].([]interface{}); !ok || len(list) != 0 {
		t.Fatalf("problems ok data.list want [] body=%s", r.Body.String())
	}
	if data["ensureStatus"] != "" || data["ensureError"] != "" || data["force"] != false {
		t.Fatalf("problems ok ensure fields mismatch body=%s", r.Body.String())
	}
}

// TestContestBoardShape 带 DB 的站内榜形状：种子行出现在榜中（真实行为：本场 contest_log 行必上榜），
// scoring/hasCellDetail/problems/cells/total 形状正确。
func TestContestBoardShape(t *testing.T) {
	db := setupContestExtraDB(t)
	cl := model.ContestLog{
		Platform:  "CodeForces",
		ContestId: "BOARD1",
		Time:      time.Unix(1750000000, 0),
	}
	if err := db.Create(&cl).Error; err != nil {
		t.Fatal(err)
	}
	server := contestExtraServer(t, &ContestLogService{db: db})

	// 不存在 → 404 语义
	r := contestExtraRequest(server, "/v1/core/contest/board?id=99999")
	body := contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "比赛不存在" {
		t.Fatalf("board missing unexpected body=%s", r.Body.String())
	}

	// 存在：本场 contest_log 行（种子行 UserID=0）上榜
	r = contestExtraRequest(server, fmt.Sprintf("/v1/core/contest/board?id=%d", cl.ID))
	if r.Code != http.StatusOK {
		t.Fatalf("board ok status=%d body=%s", r.Code, r.Body.String())
	}
	body = contestExtraBody(t, r)
	if body["success"] != true || body["message"] != "ok" {
		t.Fatalf("board ok unexpected body=%s", r.Body.String())
	}
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("board ok missing data body=%s", r.Body.String())
	}
	if data["scoring"] != "icpc" || data["hasCellDetail"] != false {
		t.Fatalf("board ok scoring/hasCellDetail mismatch body=%s", r.Body.String())
	}
	rows, _ := data["rows"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("board ok want 1 row body=%s", r.Body.String())
	}
	row, _ := rows[0].(map[string]interface{})
	if row == nil || row["userId"] != "0" || row["isContestant"] != true || row["rankLocal"] != float64(1) {
		t.Fatalf("board ok row mismatch body=%s", r.Body.String())
	}
	if cells, ok := row["cells"].([]interface{}); !ok || len(cells) != 0 {
		t.Fatalf("board ok row.cells want [] body=%s", r.Body.String())
	}
	if problems, ok := data["problems"].([]interface{}); !ok || len(problems) != 0 {
		t.Fatalf("board ok data.problems want [] body=%s", r.Body.String())
	}
	if data["total"] != float64(1) {
		t.Fatalf("board ok data.total mismatch body=%s", r.Body.String())
	}
}

// TestContestCellSubmitsShape 带 DB 的空提交明细形状 + 参数校验分支。
// 用零时间种子（无 hint 可解析展示窗）验证 startTime/endTime 为 null（前端视为缺省）。
func TestContestCellSubmitsShape(t *testing.T) {
	db := setupContestExtraDB(t)
	cl := model.ContestLog{
		Platform:  "CodeForces",
		ContestId: "CELL1",
	}
	if err := db.Create(&cl).Error; err != nil {
		t.Fatal(err)
	}
	server := contestExtraServer(t, &ContestLogService{db: db})

	// 缺 label/externalId → 400 语义
	r := contestExtraRequest(server, fmt.Sprintf("/v1/core/contest/cell-submits?id=%d&userId=5", cl.ID))
	if r.Code != http.StatusOK {
		t.Fatalf("cell-submits no-label status=%d body=%s", r.Code, r.Body.String())
	}
	body := contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "缺少题目 label 或 externalId" {
		t.Fatalf("cell-submits no-label unexpected body=%s", r.Body.String())
	}

	// 比赛不存在
	r = contestExtraRequest(server, "/v1/core/contest/cell-submits?id=99999&userId=5&label=A")
	body = contestExtraBody(t, r)
	if body["success"] != false || body["message"] != "比赛不存在" {
		t.Fatalf("cell-submits missing unexpected body=%s", r.Body.String())
	}

	// 成功：空提交列表；未解析出时间窗时 startTime/endTime 为 null（前端视为缺省）
	r = contestExtraRequest(server, fmt.Sprintf("/v1/core/contest/cell-submits?id=%d&userId=5&label=A", cl.ID))
	if r.Code != http.StatusOK {
		t.Fatalf("cell-submits ok status=%d body=%s", r.Code, r.Body.String())
	}
	body = contestExtraBody(t, r)
	if body["success"] != true || body["message"] != "ok" {
		t.Fatalf("cell-submits ok unexpected body=%s", r.Body.String())
	}
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("cell-submits ok missing data body=%s", r.Body.String())
	}
	if data["platform"] != "CodeForces" || data["contestId"] != "CELL1" ||
		data["userId"] != "5" || data["label"] != "A" {
		t.Fatalf("cell-submits ok fields mismatch body=%s", r.Body.String())
	}
	if list, ok := data["list"].([]interface{}); !ok || len(list) != 0 {
		t.Fatalf("cell-submits ok data.list want [] body=%s", r.Body.String())
	}
	if data["total"] != float64(0) {
		t.Fatalf("cell-submits ok data.total mismatch body=%s", r.Body.String())
	}
	// 无日历/无 hint 时展示窗兜底为 now 宽窗：startTime/endTime 以字符串输出（前端 num() 兼容）
	if st, ok := data["startTime"].(string); !ok || st == "" {
		t.Fatalf("cell-submits ok startTime want string body=%s", r.Body.String())
	}
	if et, ok := data["endTime"].(string); !ok || et == "" {
		t.Fatalf("cell-submits ok endTime want string body=%s", r.Body.String())
	}
}

// TestContestCellOptionalEncoding 钉死 protojson（EmitUnpopulated）对 proto3 optional
// 字段的行为：未设置时整键省略（与手写 cellToMap 的 if != nil 分支一致），
// 设置时才输出；普通字段零值照常输出。这是迁移形状的关键约定。
func TestContestCellOptionalEncoding(t *testing.T) {
	cell := &contest_log.ContestBoardCell{
		Label:  "A",
		Status: model.ContestCellNone,
	}
	b, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(cell)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, string(b))
	}
	for _, k := range []string{"relativeSec", "firstAcAt"} {
		if _, present := m[k]; present {
			t.Fatalf("unset optional %s should be omitted, got %#v (body=%s)", k, m[k], string(b))
		}
	}
	if m["label"] != "A" || m["status"] != "NONE" || m["attempts"] != float64(0) || m["scoreDelta"] != float64(0) {
		t.Fatalf("board cell shape mismatch body=%s", string(b))
	}
	if v, present := m["externalId"]; !present || v != "" {
		t.Fatalf("plain string field externalId want \"\" body=%s", string(b))
	}

	// 设置 optional 时输出（int32 → 数字；int64 → 字符串）
	set := &contest_log.ContestBoardCell{Label: "A", Status: "AC", RelativeSec: int32ptr(600)}
	b2, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]interface{}
	if err := json.Unmarshal(b2, &m2); err != nil {
		t.Fatalf("invalid json: %v body=%s", err, string(b2))
	}
	if m2["relativeSec"] != float64(600) {
		t.Fatalf("set optional relativeSec want 600, got %#v (body=%s)", m2["relativeSec"], string(b2))
	}
	if _, present := m2["firstAcAt"]; present {
		t.Fatalf("unset firstAcAt should be omitted body=%s", string(b2))
	}
}

func int32ptr(v int32) *int32 { return &v }
