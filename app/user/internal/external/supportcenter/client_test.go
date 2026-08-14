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
