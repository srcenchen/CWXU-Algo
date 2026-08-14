package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/common/notify"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm/clause"
)

// 客户中心回调协议常量（见 support-service/docs/external/callback-protocol.md）
const (
	supportEventVersion   = "1.0"
	supportEventMaxDrift  = 300 * time.Second
	supportEventTimeout   = 10 * time.Second
	supportEventTypeReply = "ticket.message.created"
	supportEventTypeState = "ticket.status.changed"
)

// ticketStatusCN 状态英文 → 中文文案（站内信 body）
func ticketStatusCN(status string) string {
	switch strings.TrimSpace(status) {
	case "pending_agent":
		return "待处理"
	case "pending_customer":
		return "待你回复"
	case "resolved":
		return "已解决"
	case "closed":
		return "已关闭"
	default:
		return status
	}
}

// NotifyEventsHTTP 客户中心 webhook 回调（原生路由，无 JWT）。
// 流程：验签（timestamp 窗 + HMAC-SHA256 恒定时间比较）→ envelope 校验 →
// event_id 幂等落库 → 按事件类型写站内信 → 204。
// 验签/协议错误 → 403（客户中心 dead-letter，不重试）；DB 错误 → 500（客户中心重试）。
func (s *TicketService) NotifyEventsHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	if r.Method != nethttp.MethodPost {
		w.Header().Set("Allow", nethttp.MethodPost)
		nethttp.Error(w, "method not allowed", nethttp.StatusMethodNotAllowed)
		return
	}

	// 1. 原始 body（保留字节用于验签）
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		nethttp.Error(w, "read body failed", nethttp.StatusBadRequest)
		return
	}

	// 2. 验签
	if s.sc == nil || len(s.sc.SignKey()) == 0 {
		nethttp.Error(w, "sign key not configured", nethttp.StatusForbidden)
		return
	}
	tsRaw := strings.TrimSpace(r.Header.Get("X-Support-Timestamp"))
	sigRaw := strings.TrimSpace(r.Header.Get("X-Support-Signature"))
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		nethttp.Error(w, "invalid timestamp", nethttp.StatusForbidden)
		return
	}
	if diff := time.Now().Unix() - ts; diff > int64(supportEventMaxDrift.Seconds()) || diff < -int64(supportEventMaxDrift.Seconds()) {
		nethttp.Error(w, "timestamp out of window", nethttp.StatusForbidden)
		return
	}
	// 签名格式 v1=<hex>
	const prefix = "v1="
	if !strings.HasPrefix(sigRaw, prefix) {
		nethttp.Error(w, "invalid signature format", nethttp.StatusForbidden)
		return
	}
	sigHex := strings.TrimSpace(strings.TrimPrefix(sigRaw, prefix))
	expect := hmacSHA256Hex(s.sc.SignKey(), tsRaw+"."+string(raw))
	if len(sigHex) != len(expect) || subtle.ConstantTimeCompare([]byte(strings.ToLower(sigHex)), []byte(expect)) != 1 {
		nethttp.Error(w, "signature mismatch", nethttp.StatusForbidden)
		return
	}

	// 3. envelope 校验
	var ev struct {
		EventID      string          `json:"event_id"`
		EventType    string          `json:"event_type"`
		EventVersion string          `json:"event_version"`
		ProductID    string          `json:"product_id"`
		Data         json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		nethttp.Error(w, "invalid envelope", nethttp.StatusForbidden)
		return
	}
	headerEventID := strings.TrimSpace(r.Header.Get("X-Support-Event-ID"))
	if ev.ProductID != s.sc.ProductID() || ev.EventVersion != supportEventVersion || ev.EventID == "" || ev.EventID != headerEventID {
		nethttp.Error(w, "envelope check failed", nethttp.StatusForbidden)
		return
	}

	// 4. 幂等：event_id 唯一键，冲突（已接收）→ 204
	now := time.Now()
	row := &model.SupportEvent{
		EventID:   ev.EventID,
		EventType: ev.EventType,
		ProductID: ev.ProductID,
		Payload:   string(raw),
		CreatedAt: now,
	}
	res := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}},
		DoNothing: true,
	}).Create(row)
	if res.Error != nil {
		log.Errorf("support event idempotency insert failed: %v", res.Error)
		nethttp.Error(w, "storage failed", nethttp.StatusInternalServerError)
		return
	}
	if res.RowsAffected == 0 {
		// 重复事件：已处理过，直接成功
		w.WriteHeader(nethttp.StatusNoContent)
		return
	}

	// 5. 事件分支 → 站内信
	switch ev.EventType {
	case supportEventTypeReply:
		s.handleMessageCreated(ev.Data)
	case supportEventTypeState:
		s.handleStatusChanged(ev.Data)
	default:
		log.Warnf("support event unknown type: %s event_id=%s", ev.EventType, ev.EventID)
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// locateUser 按 customer_external_user_id 定位用户；找不到返回 0（不创建用户）。
func (s *TicketService) locateUser(externalID string) uint {
	uid, err := strconv.ParseUint(strings.TrimSpace(externalID), 10, 32)
	if err != nil || uid == 0 {
		return 0
	}
	var u model.User
	if err := s.db.Select("id").First(&u, uint(uid)).Error; err != nil {
		return 0
	}
	return u.ID
}

func (s *TicketService) writeNotify(userID uint, ntype, title, body string, ticketID string, ticketNumber int64) {
	if userID == 0 {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"ticket_id":     ticketID,
		"ticket_number": ticketNumber,
	})
	if err := notify.Create(s.db, notify.Row{
		UserID:  userID,
		Type:    ntype,
		Title:   title,
		Body:    body,
		RefType: "ticket",
		RefID:   0, // uint 放不下 UUID，前端跳转读 Payload.ticket_id
		Payload: string(payload),
	}); err != nil {
		log.Errorf("support event notify write failed user=%d type=%s: %v", userID, ntype, err)
	}
}

// handleMessageCreated ticket.message.created：客服 public 回复
func (s *TicketService) handleMessageCreated(data json.RawMessage) {
	var d struct {
		Ticket struct {
			ID                    string `json:"id"`
			TicketNumber          int64  `json:"ticket_number"`
			Status                string `json:"status"`
			CustomerExternalUserID string `json:"customer_external_user_id"`
		} `json:"ticket"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		log.Warnf("support event message.created data parse failed: %v", err)
		return
	}
	userID := s.locateUser(d.Ticket.CustomerExternalUserID)
	if userID == 0 {
		log.Warnf("support event message.created: user not found external_id=%q ticket=%s", d.Ticket.CustomerExternalUserID, d.Ticket.ID)
		return
	}
	s.writeNotify(userID, notify.TypeTicketReply, "客服回复了你的工单",
		truncateRunes(strings.TrimSpace(d.Message.Content), 200), d.Ticket.ID, d.Ticket.TicketNumber)
}

// handleStatusChanged ticket.status.changed：工单状态变化
func (s *TicketService) handleStatusChanged(data json.RawMessage) {
	var d struct {
		Ticket struct {
			ID                    string `json:"id"`
			TicketNumber          int64  `json:"ticket_number"`
			ToStatus              string `json:"to_status"`
			CustomerExternalUserID string `json:"customer_external_user_id"`
		} `json:"ticket"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		log.Warnf("support event status.changed data parse failed: %v", err)
		return
	}
	userID := s.locateUser(d.Ticket.CustomerExternalUserID)
	if userID == 0 {
		log.Warnf("support event status.changed: user not found external_id=%q ticket=%s", d.Ticket.CustomerExternalUserID, d.Ticket.ID)
		return
	}
	body := "工单 #" + strconv.FormatInt(d.Ticket.TicketNumber, 10) + " 状态变更为 " + ticketStatusCN(d.Ticket.ToStatus)
	s.writeNotify(userID, notify.TypeTicketStatusChanged, "工单状态更新", body, d.Ticket.ID, d.Ticket.TicketNumber)
}

func hmacSHA256Hex(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
