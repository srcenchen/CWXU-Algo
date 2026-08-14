package model

import "time"

// SupportEvent 客户中心 webhook 回调幂等记录（event_id 唯一）。
// 工单数据不本地缓存，本表只做「事件已接收」去重标记 + 原始 payload 留存。
type SupportEvent struct {
	ID        uint   `gorm:"primaryKey"`
	EventID   string `gorm:"column:event_id;size:64;uniqueIndex;not null;comment:客户中心事件 id（幂等唯一键）"`
	EventType string `gorm:"column:event_type;size:64;comment:事件类型"`
	ProductID string `gorm:"column:product_id;size:64;comment:产品 UUID"`
	Payload   string `gorm:"column:payload;type:text;comment:原始 body JSON"`
	Processed bool   `gorm:"column:processed;not null;default:false;comment:是否已处理"`
	CreatedAt time.Time
}

func (SupportEvent) TableName() string {
	return "support_events"
}
