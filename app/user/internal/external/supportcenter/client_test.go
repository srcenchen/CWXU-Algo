package supportcenter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/common/conf"

	"google.golang.org/protobuf/types/known/durationpb"
)

func newTestCfg(baseURL, productID, signKey string) *conf.SupportCenter {
	return &conf.SupportCenter{
		BaseUrl:   baseURL,
		ProductId: productID,
		SignKey:   signKey,
		Timeout:   durationpb.New(2 * time.Second),
	}
}

// 未配置：base_url / product_id 任一为空返回错误
func TestNewUnconfigured(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil cfg should error")
	}
	if _, err := New(newTestCfg("", "pid", "")); err == nil {
		t.Fatal("empty base_url should error")
	}
	if _, err := New(newTestCfg("https://x", "", "")); err == nil {
		t.Fatal("empty product_id should error")
	}
	c, err := New(newTestCfg("https://x", "pid", ""))
	if err != nil {
		t.Fatalf("valid cfg should succeed: %v", err)
	}
	if c.ProductID() != "pid" {
		t.Fatalf("ProductID = %q", c.ProductID())
	}
}

// 假上游成功：创建工单解析出 ticket/message
func TestCreateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("X-Product-ID") != "pid" {
			t.Errorf("X-Product-ID = %q", r.Header.Get("X-Product-ID"))
		}
		if r.Header.Get("Authorization") != "Bearer u-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("Idempotency-Key missing on write op")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"code":0,"message":"created","data":{"ticket":{"id":"t1","ticket_number":10001,"title":"标题","status":"pending_agent"},"message":{"id":"m1","sequence_no":1,"content":"正文"}}}`))
	}))
	defer srv.Close()

	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Create(context.Background(), "u-token", "标题", "正文")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var d struct {
		Ticket struct {
			ID           string `json:"id"`
			TicketNumber int64  `json:"ticket_number"`
		} `json:"ticket"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if d.Ticket.ID != "t1" || d.Ticket.TicketNumber != 10001 || d.Message.ID != "m1" {
		t.Fatalf("unexpected data: %+v", d)
	}
}

// 409 + code 40900：OPEN_TICKET_EXISTS 透传，含 data.ticket_id
func TestCreateConflict40900(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":40900,"message":"每位用户仅允许一个活跃工单","data":{"reason":"OPEN_TICKET_EXISTS","ticket_id":"existing-1"}}`))
	}))
	defer srv.Close()

	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(context.Background(), "u-token", "t", "c")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.StatusCode != http.StatusConflict || ae.Code != 40900 || ae.Reason != "OPEN_TICKET_EXISTS" {
		t.Fatalf("unexpected APIError: %+v", ae)
	}
	if !strings.Contains(string(ae.Data), "existing-1") {
		t.Fatalf("data should carry ticket_id, got %s", ae.Data)
	}
}

// 列表：query 参数透传，data.items 原样返回
func TestListQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"items":[],"next_cursor":"c2"}}`))
	}))
	defer srv.Close()

	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.List(context.Background(), "u-token", "pending_agent", 20, "c1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotQuery != "status=pending_agent&limit=20&cursor=c1" {
		t.Fatalf("query = %q", gotQuery)
	}
	var d struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.NextCursor != "c2" {
		t.Fatalf("next_cursor = %q", d.NextCursor)
	}
}

// 网络错误：上游不可达返回错误而非 panic
func TestNetworkError(t *testing.T) {
	c, err := New(newTestCfg("http://127.0.0.1:1", "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "u-token", "t1"); err == nil {
		t.Fatal("network error should surface")
	}
}

// GetCurrent：路径 /current，无活跃工单时上游 404 → *APIError
func TestGetCurrent(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("GET should not carry Idempotency-Key")
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":40400,"message":"无活跃工单","data":{}}`))
	}))
	defer srv.Close()

	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetCurrent(context.Background(), "u-token")
	if gotPath != "/api/v1/tickets/current" {
		t.Fatalf("path = %q", gotPath)
	}
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.StatusCode != http.StatusNotFound || ae.Code != 40400 {
		t.Fatalf("unexpected APIError: %+v", ae)
	}
}

// AiAnswer：POST /ai/answer，body 带 question，不带 Idempotency-Key；响应透传
func TestAiAnswer(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("AI answer should not carry Idempotency-Key")
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"answered":true,"answer":"请先确认网络可用。","mode":"generated","references":[{"article_id":"a1","title":"排查指南","content":"步骤一…","score":0.92}]}}`))
	}))
	defer srv.Close()

	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.AiAnswer(context.Background(), "u-token", "登录失败怎么办")
	if err != nil {
		t.Fatalf("AiAnswer: %v", err)
	}
	if gotPath != "/api/v1/tickets/ai/answer" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, "登录失败怎么办") {
		t.Fatalf("body = %q", gotBody)
	}
	var d struct {
		Answered bool `json:"answered"`
		References []struct {
			ArticleID string `json:"article_id"`
			Title     string `json:"title"`
		} `json:"references"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		t.Fatal(err)
	}
	if !d.Answered || len(d.References) != 1 || d.References[0].Title != "排查指南" {
		t.Fatalf("unexpected data: %+v", d)
	}
}

// AiAnswer 慢上游（模拟 LLM 生成耗时数秒）：专用长超时客户端不应被默认短超时截断
func TestAiAnswerSlowUpstream(t *testing.T) {
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond) // 远超默认 5s 之外的用例用短延时；此处验证长超时客户端可用
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"answered":true,"answer":"慢答案","mode":"generated","references":[]}}`))
	}))
	defer srv.Close()

	// 默认配置 timeout 仅 200ms，但 AiAnswer 专用客户端为 60s，不应被截断
	c, err := New(newTestCfg(srv.URL, "pid", ""))
	if err != nil {
		t.Fatal(err)
	}
	c.hc.Timeout = 200 * time.Millisecond // 模拟极短默认超时
	resp, err := c.AiAnswer(context.Background(), "u-token", "慢问题")
	if err != nil {
		t.Fatalf("AiAnswer slow upstream should use long-timeout client: %v", err)
	}
	if el := time.Since(start); el < 1400*time.Millisecond {
		t.Fatalf("expected to wait for slow upstream, got %v", el)
	}
	var d struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Answer != "慢答案" {
		t.Fatalf("answer = %q", d.Answer)
	}
}
