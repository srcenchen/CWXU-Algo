// Package supportcenter 客户中心（工单系统）HTTP 客户端。
// 对接协议：透传用户当前 RS256 JWT（Authorization: Bearer）+ X-Product-ID；
// 写操作带 Idempotency-Key。响应统一 {code, message, data}；HTTP 非 2xx 或
// code != 0 视为错误。工单数据不本地缓存，客户中心是唯一事实来源。
package supportcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cwxu-algo/app/common/conf"

	"github.com/google/uuid"
)

// APIError 客户中心业务错误（HTTP 非 2xx 或 code != 0）。
type APIError struct {
	StatusCode int
	Code       int
	Message    string
	Reason     string // 如 OPEN_TICKET_EXISTS（可能缺失）
	Data       json.RawMessage
}

func (e *APIError) Error() string {
	return fmt.Sprintf("support center error: status=%d code=%d reason=%s message=%s",
		e.StatusCode, e.Code, e.Reason, e.Message)
}

// rawResp 客户中心统一响应 envelope 的 data 字段原样保留，由调用方按需解析。
type rawResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Client 客户中心客户端。
type Client struct {
	baseURL   string
	productID string
	signKey   []byte // 回调验签密钥（callback_secret），本轮客户端只透传、不参与请求签名
	hc        *http.Client
}

// New 创建客户端；base_url / product_id 任一为空返回错误（调用方按「支持中心未配置」处理）。
// sign_key 仅 webhook 验签用，此处允许为空（回调 handler 会单独校验）。
func New(cfg *conf.SupportCenter) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("support_center 未配置完整: 缺少 base_url/product_id")
	}
	baseURL := strings.TrimSpace(cfg.GetBaseUrl())
	productID := strings.TrimSpace(cfg.GetProductId())
	if baseURL == "" || productID == "" {
		return nil, fmt.Errorf("support_center 未配置完整: 缺少 base_url/product_id")
	}
	timeout := 5 * time.Second
	if cfg.GetTimeout() != nil {
		if d := cfg.GetTimeout().AsDuration(); d > 0 {
			timeout = d
		}
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		productID: productID,
		signKey:   []byte(strings.TrimSpace(cfg.GetSignKey())),
		hc:        &http.Client{Timeout: timeout},
	}, nil
}

// SignKey 返回回调验签密钥（供回调 handler 使用；未配置时返回 nil）。
func (c *Client) SignKey() []byte {
	if c == nil {
		return nil
	}
	return c.signKey
}

// ProductID 返回当前产品 UUID（供回调 handler 校验 product_id）。
func (c *Client) ProductID() string {
	if c == nil {
		return ""
	}
	return c.productID
}

// do 发送请求：透传用户 JWT，注入 X-Product-ID，写操作带 Idempotency-Key。
// 非 2xx 或 code != 0 返回 *APIError。
func (c *Client) do(ctx context.Context, userToken, method, path string, body any, idemKey string) (*rawResp, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("支持中心未配置")
	}
	if strings.TrimSpace(userToken) == "" {
		return nil, fmt.Errorf("缺少用户 token")
	}
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("support center 请求体编码失败: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("support center 请求构建失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(userToken))
	req.Header.Set("X-Product-ID", c.productID)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("support center 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("support center 响应读取失败: %w", err)
	}

	var out rawResp
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("support center 响应解析失败(status=%d): %w", resp.StatusCode, err)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.Code != 0 {
		ae := &APIError{StatusCode: resp.StatusCode, Code: out.Code, Message: strings.TrimSpace(out.Message)}
		if out.Code == 0 {
			ae.Code = resp.StatusCode // 非 2xx 但无业务 code 时回退 HTTP 状态码
		}
		ae.Data = out.Data
		// 尝试解析 data.reason（如 OPEN_TICKET_EXISTS）
		if len(out.Data) > 0 {
			var meta struct {
				Reason   string `json:"reason"`
				TicketID string `json:"ticket_id"`
			}
			if err := json.Unmarshal(out.Data, &meta); err == nil {
				ae.Reason = meta.Reason
				if ae.Reason == "" && meta.TicketID != "" {
					// 保留 ticket_id 供 service 层 40900 分支使用
					ae.Data = out.Data
				}
			}
		}
		return nil, ae
	}
	return &out, nil
}

// GetCurrent 当前活跃工单（无活跃工单时客户中心返回 404/40400 → *APIError）。
func (c *Client) GetCurrent(ctx context.Context, userToken string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodGet, "/api/v1/tickets/current", nil, "")
}

// AiAnswer 智能问答（不创建工单、不持久化对话；不要求 Idempotency-Key）。
func (c *Client) AiAnswer(ctx context.Context, userToken, question string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodPost, "/api/v1/tickets/ai/answer",
		map[string]string{"question": question}, "")
}

// List 工单列表。
func (c *Client) List(ctx context.Context, userToken, status string, limit int64, cursor string) (*rawResp, error) {
	q := []string{}
	if status != "" {
		q = append(q, "status="+status)
	}
	if limit > 0 {
		q = append(q, fmt.Sprintf("limit=%d", limit))
	}
	if cursor != "" {
		q = append(q, "cursor="+cursor)
	}
	path := "/api/v1/tickets"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	return c.do(ctx, userToken, http.MethodGet, path, nil, "")
}

// Get 工单详情。
func (c *Client) Get(ctx context.Context, userToken, ticketID string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodGet, "/api/v1/tickets/"+ticketID, nil, "")
}

// GetMessages 消息列表（增量：after_sequence 之后的 public 消息）。
func (c *Client) GetMessages(ctx context.Context, userToken, ticketID string, afterSequence, limit int64) (*rawResp, error) {
	q := []string{fmt.Sprintf("after_sequence=%d", afterSequence)}
	if limit > 0 {
		q = append(q, fmt.Sprintf("limit=%d", limit))
	}
	return c.do(ctx, userToken, http.MethodGet,
		"/api/v1/tickets/"+ticketID+"/messages?"+strings.Join(q, "&"), nil, "")
}

// Create 创建工单（写操作，带 Idempotency-Key）。
func (c *Client) Create(ctx context.Context, userToken, title, content string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodPost, "/api/v1/tickets",
		map[string]string{"title": title, "content": content}, uuid.NewString())
}

// CreateMessage 补充消息（写操作）。
func (c *Client) CreateMessage(ctx context.Context, userToken, ticketID, content string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodPost, "/api/v1/tickets/"+ticketID+"/messages",
		map[string]string{"content": content}, uuid.NewString())
}

// PatchStatus 用户修改工单状态（写操作；pending_agent|pending_customer → resolved|closed）。
func (c *Client) PatchStatus(ctx context.Context, userToken, ticketID, status, reason string) (*rawResp, error) {
	return c.do(ctx, userToken, http.MethodPatch, "/api/v1/tickets/"+ticketID+"/status",
		map[string]string{"status": status, "reason": reason}, uuid.NewString())
}
