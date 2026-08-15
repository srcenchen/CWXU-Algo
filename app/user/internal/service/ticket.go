package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	ticketpb "cwxu-algo/api/user/v1/ticket"
	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/user/internal/external/supportcenter"

	"gorm.io/gorm"
)

// TicketService 工单服务：透传客户中心 API（数据不本地缓存，客户中心是唯一事实来源）。
// 实现 proto：api/user/v1/ticket/ticket.proto（TicketServiceHTTPServer）。
type TicketService struct {
	sc *supportcenter.Client
	db *gorm.DB
}

// NewTicketService 创建工单服务；cfg 缺失 / 配置不完整时 sc 为 nil（handler 返回「支持中心未配置」）。
func NewTicketService(cfg *conf.SupportCenter, db *gorm.DB) *TicketService {
	sc, err := supportcenter.New(cfg)
	if err != nil {
		sc = nil
	}
	return &TicketService{sc: sc, db: db}
}

// authAndClient 统一前置校验：登录态 + 原始 JWT + 客户中心已配置。
// 返回 (错误响应, 是否可直接返回)。第三个返回值是用户 token。
func (s *TicketService) authAndClient(ctx context.Context) (ok bool, userToken string) {
	if pd := auth.GetCurrentUser(ctx); pd == nil || pd.UserID == 0 {
		return false, ""
	}
	return true, auth.RawToken(ctx)
}

// errMessage 客户中心错误 → 用户可读 message（截断 ≤200 字符）
func errMessage(err error) string {
	if ae, ok := err.(*supportcenter.APIError); ok {
		msg := strings.TrimSpace(ae.Message)
		if msg == "" {
			msg = "客户中心处理失败"
		}
		return truncateRunes(msg, 200)
	}
	return "支持中心暂时不可用，请稍后再试"
}

func truncateRunes(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// flexUnix 时间字段：支持 RFC3339 字符串 / 数字（秒）。
func flexUnix(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	return 0
}

func parseTicket(raw json.RawMessage) *ticketpb.Ticket {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	get := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	getInt := func(k string) int64 {
		var n int64
		if rawV, ok := m[k]; ok {
			var f float64
			if err := json.Unmarshal(rawV, &f); err == nil {
				n = int64(f)
			}
		}
		return n
	}
	return &ticketpb.Ticket{
		Id:             get("id"),
		TicketNumber:   getInt("ticket_number"),
		Title:          get("title"),
		Status:         get("status"),
		Priority:       get("priority"),
		AwaitingActor:  get("awaiting_actor"),
		LatestMessageAt: flexUnix(m["latest_message_at"]),
		CreatedAt:      flexUnix(m["created_at"]),
		UpdatedAt:      flexUnix(m["updated_at"]),
	}
}

func parseMessage(raw json.RawMessage) *ticketpb.TicketMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	get := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	seq := int64(0)
	if rawV, ok := m["sequence_no"]; ok {
		var f float64
		if err := json.Unmarshal(rawV, &f); err == nil {
			seq = int64(f)
		}
	}
	return &ticketpb.TicketMessage{
		Id:         get("id"),
		SequenceNo: seq,
		SenderType: get("sender_type"),
		ContentType: get("content_type"),
		Content:    get("content"),
		SentAt:     flexUnix(m["sent_at"]),
	}
}

// GetCurrent 当前活跃工单（无活跃工单 → success:true, ticket=nil，前端进入 QA 问答）
func (s *TicketService) GetCurrent(ctx context.Context, req *ticketpb.GetCurrentReq) (*ticketpb.GetCurrentRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.GetCurrentRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.GetCurrentRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.GetCurrentRes{Success: false, Message: "请先登录"}, nil
	} else {
		resp, err := s.sc.GetCurrent(ctx, token)
		if err != nil {
			if ae, ok := err.(*supportcenter.APIError); ok && ae.StatusCode == 404 {
				// 无活跃工单（40400）→ success:true, ticket=nil
				return &ticketpb.GetCurrentRes{Success: true, Message: "ok"}, nil
			}
			return &ticketpb.GetCurrentRes{Success: false, Message: errMessage(err)}, nil
		}
		return &ticketpb.GetCurrentRes{Success: true, Message: "ok", Ticket: parseTicket(resp.Data)}, nil
	}
}

// AiAnswer 智能问答（透传；不创建工单、不持久化 QA 对话）。
func (s *TicketService) AiAnswer(ctx context.Context, req *ticketpb.AiAnswerReq) (*ticketpb.AiAnswerRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.AiAnswerRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.AiAnswerRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.AiAnswerRes{Success: false, Message: "请先登录"}, nil
	} else {
		question := strings.TrimSpace(req.GetQuestion())
		if question == "" || len([]rune(question)) > 2000 {
			return &ticketpb.AiAnswerRes{Success: false, Message: "问题不能为空且不能超过 2000 个字符"}, nil
		}
		resp, err := s.sc.AiAnswer(ctx, token, question)
		if err != nil {
			return &ticketpb.AiAnswerRes{Success: false, Message: errMessage(err)}, nil
		}
		var d struct {
			Answered   bool   `json:"answered"`
			Answer     string `json:"answer"`
			Mode       string `json:"mode"`
			References []struct {
				ArticleID string  `json:"article_id"`
				Title     string  `json:"title"`
				Question  string  `json:"question"`
				Content   string  `json:"content"`
				Score     float64 `json:"score"`
			} `json:"references"`
		}
		if err := json.Unmarshal(resp.Data, &d); err != nil {
			return &ticketpb.AiAnswerRes{Success: false, Message: "客户中心响应异常"}, nil
		}
		out := &ticketpb.AiAnswerRes{Success: true, Message: "ok", Answered: d.Answered, Answer: d.Answer, Mode: d.Mode}
		for _, r := range d.References {
			out.References = append(out.References, &ticketpb.AiAnswerReference{
				ArticleId: r.ArticleID,
				Title:     r.Title,
				Question:  r.Question,
				Content:   r.Content,
				Score:     r.Score,
			})
		}
		return out, nil
	}
}

// List 工单列表（cursor 分页）
func (s *TicketService) List(ctx context.Context, req *ticketpb.ListTicketsReq) (*ticketpb.ListTicketsRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.ListTicketsRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.ListTicketsRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.ListTicketsRes{Success: false, Message: "请先登录"}, nil
	} else {
		resp, err := s.sc.List(ctx, token, req.GetStatus(), req.GetLimit(), req.GetCursor())
		if err != nil {
			return &ticketpb.ListTicketsRes{Success: false, Message: errMessage(err)}, nil
		}
		var d struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(resp.Data, &d); err != nil {
			return &ticketpb.ListTicketsRes{Success: false, Message: "客户中心响应异常"}, nil
		}
		out := &ticketpb.ListTicketsRes{Success: true, Message: "ok", NextCursor: d.NextCursor}
		for _, it := range d.Items {
			if t := parseTicket(it); t != nil {
				out.List = append(out.List, t)
			}
		}
		return out, nil
	}
}

// Get 工单详情
func (s *TicketService) Get(ctx context.Context, req *ticketpb.GetTicketReq) (*ticketpb.GetTicketRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.GetTicketRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.GetTicketRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.GetTicketRes{Success: false, Message: "请先登录"}, nil
	} else {
		if strings.TrimSpace(req.GetTicketId()) == "" {
			return &ticketpb.GetTicketRes{Success: false, Message: "缺少工单 ID"}, nil
		}
		resp, err := s.sc.Get(ctx, token, req.GetTicketId())
		if err != nil {
			return &ticketpb.GetTicketRes{Success: false, Message: errMessage(err)}, nil
		}
		return &ticketpb.GetTicketRes{Success: true, Message: "ok", Ticket: parseTicket(resp.Data)}, nil
	}
}

// GetMessages 消息列表（after_sequence 增量）
func (s *TicketService) GetMessages(ctx context.Context, req *ticketpb.GetMessagesReq) (*ticketpb.GetMessagesRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.GetMessagesRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.GetMessagesRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.GetMessagesRes{Success: false, Message: "请先登录"}, nil
	} else {
		resp, err := s.sc.GetMessages(ctx, token, req.GetTicketId(), req.GetAfterSequence(), req.GetLimit())
		if err != nil {
			return &ticketpb.GetMessagesRes{Success: false, Message: errMessage(err)}, nil
		}
		var d struct {
			Items             []json.RawMessage `json:"items"`
			NextAfterSequence int64             `json:"next_after_sequence"`
		}
		if err := json.Unmarshal(resp.Data, &d); err != nil {
			return &ticketpb.GetMessagesRes{Success: false, Message: "客户中心响应异常"}, nil
		}
		out := &ticketpb.GetMessagesRes{Success: true, Message: "ok", NextAfterSequence: d.NextAfterSequence}
		for _, it := range d.Items {
			if m := parseMessage(it); m != nil {
				out.List = append(out.List, m)
			}
		}
		return out, nil
	}
}

// Create 创建工单；409/40900（已有进行中的工单）→ success:false + 返回已有工单 id
func (s *TicketService) Create(ctx context.Context, req *ticketpb.CreateTicketReq) (*ticketpb.CreateTicketRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.CreateTicketRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.CreateTicketRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.CreateTicketRes{Success: false, Message: "请先登录"}, nil
	} else {
		title := strings.TrimSpace(req.GetTitle())
		content := strings.TrimSpace(req.GetContent())
		if title == "" || content == "" {
			return &ticketpb.CreateTicketRes{Success: false, Message: "标题与内容不能为空"}, nil
		}
		if len([]rune(title)) > 500 {
			return &ticketpb.CreateTicketRes{Success: false, Message: "标题不能超过 500 个字符"}, nil
		}
		if len([]rune(content)) > 10000 {
			return &ticketpb.CreateTicketRes{Success: false, Message: "内容不能超过 10000 个字符"}, nil
		}
		resp, err := s.sc.Create(ctx, token, title, content)
		if err != nil {
			if ae, ok := err.(*supportcenter.APIError); ok && ae.StatusCode == 409 && ae.Code == 40900 {
				// 已有进行中的工单：携带已有工单 id（data.ticket_id），前端跳详情
				out := &ticketpb.CreateTicketRes{Success: false, Message: "已有进行中的工单", Ticket: &ticketpb.Ticket{}}
				if len(ae.Data) > 0 {
					var d struct {
						TicketID string `json:"ticket_id"`
					}
					if json.Unmarshal(ae.Data, &d) == nil {
						out.Ticket.Id = d.TicketID
					}
				}
				return out, nil
			}
			return &ticketpb.CreateTicketRes{Success: false, Message: errMessage(err)}, nil
		}
		var d struct {
			Ticket  json.RawMessage `json:"ticket"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(resp.Data, &d); err != nil {
			return &ticketpb.CreateTicketRes{Success: false, Message: "客户中心响应异常"}, nil
		}
		return &ticketpb.CreateTicketRes{
			Success:     true,
			Message:     "工单已创建",
			Ticket:      parseTicket(d.Ticket),
			MessageInfo: parseMessage(d.Message),
		}, nil
	}
}

// CreateMessage 补充消息
func (s *TicketService) CreateMessage(ctx context.Context, req *ticketpb.CreateMessageReq) (*ticketpb.CreateMessageRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.CreateMessageRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.CreateMessageRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.CreateMessageRes{Success: false, Message: "请先登录"}, nil
	} else {
		content := strings.TrimSpace(req.GetContent())
		if content == "" {
			return &ticketpb.CreateMessageRes{Success: false, Message: "消息内容不能为空"}, nil
		}
		resp, err := s.sc.CreateMessage(ctx, token, req.GetTicketId(), content)
		if err != nil {
			return &ticketpb.CreateMessageRes{Success: false, Message: errMessage(err)}, nil
		}
		return &ticketpb.CreateMessageRes{Success: true, Message: "已发送", MessageInfo: parseMessage(resp.Data)}, nil
	}
}

// PatchStatus 用户修改工单状态（成功后再拉详情填充返回）
func (s *TicketService) PatchStatus(ctx context.Context, req *ticketpb.PatchStatusReq) (*ticketpb.PatchStatusRes, error) {
	if ok, token := s.authAndClient(ctx); !ok {
		return &ticketpb.PatchStatusRes{Success: false, Message: "请先登录"}, nil
	} else if s.sc == nil {
		return &ticketpb.PatchStatusRes{Success: false, Message: "支持中心未配置，请稍后再试"}, nil
	} else if token == "" {
		return &ticketpb.PatchStatusRes{Success: false, Message: "请先登录"}, nil
	} else {
		status := strings.TrimSpace(req.GetStatus())
		if status != "resolved" && status != "closed" {
			return &ticketpb.PatchStatusRes{Success: false, Message: "仅支持标记为已解决或关闭"}, nil
		}
		if _, err := s.sc.PatchStatus(ctx, token, req.GetTicketId(), status, req.GetReason()); err != nil {
			return &ticketpb.PatchStatusRes{Success: false, Message: errMessage(err)}, nil
		}
		// PATCH 成功响应不带 data，补拉详情供前端刷新
		if resp, err := s.sc.Get(ctx, token, req.GetTicketId()); err == nil {
			return &ticketpb.PatchStatusRes{Success: true, Message: "已更新", Ticket: parseTicket(resp.Data)}, nil
		}
		return &ticketpb.PatchStatusRes{Success: true, Message: "已更新"}, nil
	}
}
