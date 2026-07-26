package core_data

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"cwxu-algo/api/core/v1/contest_log"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	"github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	grpc2 "google.golang.org/grpc"
)

func dialCoreContest(reg *registry.Registrar, _ context.Context) (*grpc2.ClientConn, error) {
	if reg == nil {
		return nil, fmt.Errorf("registry 未配置")
	}
	// 复用共享长连接，避免每次工具调用 dial+Close
	return SharedGRPCConn(reg, "core-data")
}

// ---------- contest_list ----------

type ContestListTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewContestListTool(reg *registry.Registrar, ctxs ...context.Context) *ContestListTool {
	return &ContestListTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *ContestListTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *ContestListTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "contest_list",
			Description: "比赛列表。userId=-1（默认）取当前组织成员参赛场次；userId>0 取该用户。" +
				"返回含过题数 acCount、官方 rank、总题数。可按 timeFrom/timeTo（unix 秒）筛选。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userId":    map[string]interface{}{"type": "integer", "description": "-1 组织，>0 个人，默认 -1"},
					"limit":     map[string]interface{}{"type": "integer", "description": "默认 15，最大 20"},
					"offset":    map[string]interface{}{"type": "integer"},
					"platform":  map[string]interface{}{"type": "string"},
					"timeFrom":  map[string]interface{}{"type": "integer", "description": "unix 秒下界"},
					"timeTo":    map[string]interface{}{"type": "integer", "description": "unix 秒上界"},
					"keyword":   map[string]interface{}{"type": "string"},
				},
			},
		},
	}
}

func (c *ContestListTool) AiInterface(jsonStr string) string {
	var p struct {
		UserId   int64  `json:"userId"`
		Limit    int64  `json:"limit"`
		Offset   int64  `json:"offset"`
		Platform string `json:"platform"`
		TimeFrom int64  `json:"timeFrom"`
		TimeTo   int64  `json:"timeTo"`
		Keyword  string `json:"keyword"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.UserId == 0 {
		p.UserId = -1
	}
	if p.Limit <= 0 {
		p.Limit = 15
	}
	if p.Limit > 20 {
		p.Limit = 20
	}
	conn, err := dialCoreContest(c.reg, c.ctx)
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := contest_log.NewContestClient(conn)
	res, err := cli.GetContestList(toolRPCContext(c.ctx), &contest_log.GetContestListReq{
		UserId:   p.UserId,
		Limit:    p.Limit,
		Offset:   p.Offset,
		Platform: p.Platform,
		Keyword:  p.Keyword,
		TimeFrom: p.TimeFrom,
		TimeTo:   p.TimeTo,
	})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	type row struct {
		ID          uint32 `json:"id"`
		Platform    string `json:"platform"`
		ContestID   string `json:"contestId"`
		ContestName string `json:"contestName"`
		Rank        int32  `json:"rank"`
		ACCount     int32  `json:"acCount"`
		TotalCount  int32  `json:"totalCount"`
		Time        int64  `json:"time"`
		UserID      int64  `json:"userId,omitempty"`
		UserName    string `json:"userName,omitempty"`
	}
	out := make([]row, 0, len(res.GetData()))
	for _, v := range res.GetData() {
		if v == nil {
			continue
		}
		out = append(out, row{
			ID: v.Id, Platform: v.Platform, ContestID: v.ContestId, ContestName: v.ContestName,
			Rank: v.Rank, ACCount: v.AcCount, TotalCount: v.TotalCount, Time: v.Time,
			UserID: v.UserId, UserName: v.UserName,
		})
	}
	b, _ := json.Marshal(out)
	return fmt.Sprintf("比赛列表 total=%d: %s", res.GetTotal(), string(b))
}

// ---------- contest_ranking ----------

type ContestRankingTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewContestRankingTool(reg *registry.Registrar, ctxs ...context.Context) *ContestRankingTool {
	return &ContestRankingTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *ContestRankingTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *ContestRankingTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "contest_ranking",
			Description: "单场比赛的组织内排行榜（含过题数 acCount、总分 score、总题数 totalCount、名次 rank）。" +
				"contestId 为平台侧比赛 id；可选 groupId 过滤分组。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"contestId": map[string]interface{}{"type": "string", "description": "比赛 id"},
					"limit":     map[string]interface{}{"type": "integer", "description": "默认 20，最大 50"},
					"offset":    map[string]interface{}{"type": "integer"},
					"groupId":   map[string]interface{}{"type": "integer", "description": "可选组 id"},
				},
				"required": []string{"contestId"},
			},
		},
	}
}

func (c *ContestRankingTool) AiInterface(jsonStr string) string {
	var p struct {
		ContestId string `json:"contestId"`
		Limit     int64  `json:"limit"`
		Offset    int64  `json:"offset"`
		GroupId   *int64 `json:"groupId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.ContestId == "" {
		return "contestId 必填"
	}
	if p.Limit <= 0 {
		p.Limit = 20
	}
	if p.Limit > 50 {
		p.Limit = 50
	}
	conn, err := dialCoreContest(c.reg, c.ctx)
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := contest_log.NewContestClient(conn)
	req := &contest_log.GetContestRankingReq{
		ContestId: p.ContestId,
		Limit:     p.Limit,
		Offset:    p.Offset,
	}
	if p.GroupId != nil {
		req.GroupId = p.GroupId
	}
	res, err := cli.GetContestRanking(toolRPCContext(c.ctx), req)
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	type row struct {
		Rank       int64  `json:"rank"`
		UserID     int64  `json:"userId"`
		Name       string `json:"name"`
		Score      int32  `json:"score"`
		ACCount    int32  `json:"acCount"`
		TotalCount int32  `json:"totalCount"`
	}
	out := make([]row, 0, len(res.GetData()))
	for _, v := range res.GetData() {
		if v == nil {
			continue
		}
		out = append(out, row{
			Rank: v.Rank, UserID: v.UserId, Name: v.Name,
			Score: v.Score, ACCount: v.AcCount, TotalCount: v.TotalCount,
		})
	}
	meta := map[string]interface{}{}
	if ct := res.GetContest(); ct != nil {
		meta["contestName"] = ct.ContestName
		meta["platform"] = ct.Platform
		meta["contestId"] = ct.ContestId
	}
	meta["total"] = res.GetTotal()
	meta["rows"] = out
	b, _ := json.Marshal(meta)
	return fmt.Sprintf("比赛排行榜: %s", string(b))
}

// ---------- contest_history ----------

type ContestHistoryTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewContestHistoryTool(reg *registry.Registrar, ctxs ...context.Context) *ContestHistoryTool {
	return &ContestHistoryTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *ContestHistoryTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *ContestHistoryTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "contest_history",
			Description: "个人比赛历史（含 rank、acCount、totalCount）。写日报/点评个人比赛表现时使用。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"userId":   map[string]interface{}{"type": "integer", "description": "用户 id"},
					"limit":    map[string]interface{}{"type": "integer", "description": "默认 15，最大 30"},
					"cursor":   map[string]interface{}{"type": "integer", "description": "时间游标"},
					"platform": map[string]interface{}{"type": "string"},
				},
				"required": []string{"userId"},
			},
		},
	}
}

func (c *ContestHistoryTool) AiInterface(jsonStr string) string {
	var p struct {
		UserId   int64  `json:"userId"`
		Limit    int64  `json:"limit"`
		Cursor   int64  `json:"cursor"`
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.UserId <= 0 {
		return "userId 无效"
	}
	if p.Limit <= 0 {
		p.Limit = 15
	}
	if p.Limit > 30 {
		p.Limit = 30
	}
	conn, err := dialCoreContest(c.reg, c.ctx)
	if err != nil {
		return "连接失败: " + err.Error()
	}
	cli := contest_log.NewContestClient(conn)
	res, err := cli.GetUserContestHistory(toolRPCContext(c.ctx), &contest_log.GetUserContestHistoryReq{
		UserId:   p.UserId,
		Limit:    p.Limit,
		Cursor:   p.Cursor,
		Platform: p.Platform,
	})
	if err != nil {
		log.Error(err)
		return "查询失败: " + err.Error()
	}
	type row struct {
		Platform    string `json:"platform"`
		ContestID   string `json:"contestId"`
		ContestName string `json:"contestName"`
		Rank        int32  `json:"rank"`
		ACCount     int32  `json:"acCount"`
		TotalCount  int32  `json:"totalCount"`
		Time        int64  `json:"time"`
	}
	out := make([]row, 0, len(res.GetData()))
	for _, v := range res.GetData() {
		if v == nil {
			continue
		}
		out = append(out, row{
			Platform: v.Platform, ContestID: v.ContestId, ContestName: v.ContestName,
			Rank: v.Rank, ACCount: v.AcCount, TotalCount: v.TotalCount, Time: v.Time,
		})
	}
	b, _ := json.Marshal(out)
	return fmt.Sprintf("用户%d比赛历史(%d): %s", p.UserId, len(out), string(b))
}

// ---------- contest_board（详细格子榜，HTTP） ----------

type ContestBoardTool struct {
	reg *registry.Registrar
	ctx context.Context
}

func NewContestBoardTool(reg *registry.Registrar, ctxs ...context.Context) *ContestBoardTool {
	return &ContestBoardTool{reg: reg, ctx: firstCtx(ctxs...)}
}

func (c *ContestBoardTool) AuthContext() context.Context { return toolRPCContext(c.ctx) }

func (c *ContestBoardTool) Description() *model.Tool {
	return &model.Tool{
		Type: model.ToolTypeFunction,
		Function: &model.FunctionDefinition{
			Name: "contest_board",
			Description: "单场比赛详细排行榜（XCPCIO 风格格子榜，含每题过题情况）。" +
				"id 为 contest_logs 行 id（可从 contest_list 的 id 字段取得）。数据量较大，优先用 contest_ranking；仅需题级明细时调用。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":      map[string]interface{}{"type": "integer", "description": "contest_logs 行 id"},
					"groupId": map[string]interface{}{"type": "integer", "description": "可选组过滤"},
				},
				"required": []string{"id"},
			},
		},
	}
}

func (c *ContestBoardTool) AiInterface(jsonStr string) string {
	var p struct {
		ID      int64 `json:"id"`
		GroupId int64 `json:"groupId"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return "参数错误"
	}
	if p.ID <= 0 {
		return "id 无效"
	}
	q := url.Values{}
	q.Set("id", strconv.FormatInt(p.ID, 10))
	if p.GroupId > 0 {
		q.Set("groupId", strconv.FormatInt(p.GroupId, 10))
	}
	path := "/v1/core/contest/board?" + q.Encode()
	body, code, err := discoveryHTTPGet(c.ctx, c.reg, "core-data", path)
	if err != nil {
		return "连接失败: " + err.Error()
	}
	if code >= 400 {
		return fmt.Sprintf("查询失败 HTTP %d: %s", code, truncateStr(string(body), 300))
	}
	// 截断过大响应
	s := string(body)
	if len(s) > 12000 {
		s = s[:12000] + "...(truncated)"
	}
	return "详细比赛榜: " + s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
