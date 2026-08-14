package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"cwxu-algo/app/common/conf"
	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/user/internal/data/model"
	"cwxu-algo/app/user/internal/external/supportcenter"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"google.golang.org/protobuf/types/known/durationpb"
)

const testSignKey = "test-callback-secret-0123456789abcdef"
const testProductID = "11111111-2222-3333-4444-555555555555"

func newCallbackService(t *testing.T) *TicketService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:cb_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.SupportEvent{}, &model.Notification{}, &model.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Create(&model.User{ID: 5, Username: "ticket-user"}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	sc, err := supportcenter.New(&conf.SupportCenter{
		BaseUrl:   "http://127.0.0.1:1",
		ProductId: testProductID,
		SignKey:   testSignKey,
		Timeout:   durationpb.New(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &TicketService{sc: sc, db: db}
}

// signedEvent 构造带合法签名的回调请求。
func signedEvent(t *testing.T, ts int64, body []byte, eventID string) *http.Request {
	t.Helper()
	sig := hmacSHA256Hex([]byte(testSignKey), strconv.FormatInt(ts, 10)+"."+string(body))
	req := httptest.NewRequest(http.MethodPost, "/v1/support/events", bytes.NewReader(body))
	req.Header.Set("X-Support-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Support-Signature", "v1="+sig)
	req.Header.Set("X-Support-Event-ID", eventID)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func messageCreatedBody(eventID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"event_id":      eventID,
		"event_type":    "ticket.message.created",
		"event_version": "1.0",
		"product_id":    testProductID,
		"occurred_at":   time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"ticket": map[string]any{
				"id":                       "ticket-uuid-1",
				"ticket_number":            10001,
				"status":                   "pending_customer",
				"customer_external_user_id": "5",
			},
			"message": map[string]any{
				"id":           "msg-uuid-1",
				"sequence_no":  2,
				"sender_type":  "support_agent",
				"content_type": "text",
				"content":      "您好，请重新登录后再试。",
				"sent_at":      time.Now().UTC().Format(time.RFC3339),
			},
		},
	})
	return body
}

func statusChangedBody(eventID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"event_id":      eventID,
		"event_type":    "ticket.status.changed",
		"event_version": "1.0",
		"product_id":    testProductID,
		"occurred_at":   time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"ticket": map[string]any{
				"id":                       "ticket-uuid-2",
				"ticket_number":            10002,
				"from_status":              "pending_agent",
				"to_status":                "resolved",
				"customer_external_user_id": "5",
				"updated_at":               time.Now().UTC().Format(time.RFC3339),
			},
			"reason": "问题已解决",
		},
	})
	return body
}

// 合法 message.created 回调：204 + 事件落库 + 站内信写入
func TestNotifyEventsValidReply(t *testing.T) {
	s := newCallbackService(t)
	body := messageCreatedBody("evt-1")
	req := signedEvent(t, time.Now().Unix(), body, "evt-1")
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	var evs []model.SupportEvent
	if err := s.db.Find(&evs).Error; err != nil || len(evs) != 1 {
		t.Fatalf("support_events rows = %d err=%v", len(evs), err)
	}
	if evs[0].EventID != "evt-1" || !evs[0].Processed == false {
		t.Fatalf("event row mismatch: %+v", evs[0])
	}
	var ns []model.Notification
	if err := s.db.Find(&ns).Error; err != nil || len(ns) != 1 {
		t.Fatalf("notifications rows = %d err=%v", len(ns), err)
	}
	if ns[0].UserID != 5 || ns[0].Type != notify.TypeTicketReply || ns[0].RefType != "ticket" {
		t.Fatalf("notification mismatch: %+v", ns[0])
	}
	if ns[0].Title != "客服回复了你的工单" || ns[0].Body != "您好，请重新登录后再试。" {
		t.Fatalf("notification copy mismatch: %+v", ns[0])
	}
}

// 幂等：同 event_id 重复投递 → 第二次 204 且不重复写事件/通知
func TestNotifyEventsIdempotent(t *testing.T) {
	s := newCallbackService(t)
	body := messageCreatedBody("evt-idem")
	ts := time.Now().Unix()
	for i := 0; i < 2; i++ {
		req := signedEvent(t, ts, body, "evt-idem")
		rec := httptest.NewRecorder()
		s.NotifyEventsHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d status = %d, want 204", i+1, rec.Code)
		}
	}
	var evs []model.SupportEvent
	_ = s.db.Find(&evs).Error
	if len(evs) != 1 {
		t.Fatalf("support_events rows = %d, want 1", len(evs))
	}
	var ns []model.Notification
	_ = s.db.Find(&ns).Error
	if len(ns) != 1 {
		t.Fatalf("notifications rows = %d, want 1", len(ns))
	}
}

// 状态变更事件 → ticket_status_changed 通知（中文状态文案）
func TestNotifyEventsStatusChanged(t *testing.T) {
	s := newCallbackService(t)
	body := statusChangedBody("evt-status")
	req := signedEvent(t, time.Now().Unix(), body, "evt-status")
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	var ns []model.Notification
	if err := s.db.Find(&ns).Error; err != nil || len(ns) != 1 {
		t.Fatalf("notifications rows = %d err=%v", len(ns), err)
	}
	if ns[0].Type != notify.TypeTicketStatusChanged {
		t.Fatalf("type = %s, want ticket_status_changed", ns[0].Type)
	}
	if ns[0].Body != "工单 #10002 状态变更为 已解决" {
		t.Fatalf("body = %q", ns[0].Body)
	}
}

// 篡改 body（与签名时不一致）→ 403
func TestNotifyEventsTamperedBody(t *testing.T) {
	s := newCallbackService(t)
	body := messageCreatedBody("evt-tamper")
	req := signedEvent(t, time.Now().Unix(), body, "evt-tamper")
	// 篡改：请求体与签名输入不一致
	req.Body = http.NoBody
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var evs []model.SupportEvent
	_ = s.db.Find(&evs).Error
	if len(evs) != 0 {
		t.Fatalf("support_events rows = %d, want 0", len(evs))
	}
}

// 过期 timestamp（>5 分钟）→ 403
func TestNotifyEventsExpiredTimestamp(t *testing.T) {
	s := newCallbackService(t)
	body := messageCreatedBody("evt-expired")
	old := time.Now().Add(-10 * time.Minute).Unix()
	req := signedEvent(t, old, body, "evt-expired")
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// 错误 product_id → 403
func TestNotifyEventsWrongProduct(t *testing.T) {
	s := newCallbackService(t)
	body, _ := json.Marshal(map[string]any{
		"event_id":      "evt-wrong-product",
		"event_type":    "ticket.message.created",
		"event_version": "1.0",
		"product_id":    "other-product-id",
		"data":          map[string]any{},
	})
	req := signedEvent(t, time.Now().Unix(), body, "evt-wrong-product")
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// 无 sign_key（未配置）→ 403
func TestNotifyEventsNoSignKey(t *testing.T) {
	db, _ := gorm.Open(sqlite.Open("file:cb_nokey_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	_ = db.AutoMigrate(&model.SupportEvent{}, &model.Notification{})
	sc, err := supportcenter.New(&conf.SupportCenter{
		BaseUrl: "http://127.0.0.1:1", ProductId: testProductID, SignKey: "", Timeout: durationpb.New(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &TicketService{sc: sc, db: db}
	body := messageCreatedBody("evt-nokey")
	req := signedEvent(t, time.Now().Unix(), body, "evt-nokey")
	rec := httptest.NewRecorder()
	s.NotifyEventsHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
