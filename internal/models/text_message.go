package models

type MessageType string

const (
	MessageTypeIncoming MessageType = "incoming" // 收到
	MessageTypeOutgoing MessageType = "outgoing" // 发送
)

type MessageStatus string

const (
	MessageStatusReceived  MessageStatus = "received"  // 接收成功
	MessageStatusSending   MessageStatus = "sending"   // 发送中
	MessageStatusSent      MessageStatus = "sent"      // 发送成功
	MessageStatusFailed    MessageStatus = "failed"    // 发送失败
	MessageStatusAmbiguous MessageStatus = "ambiguous" // 已提交但最终状态无法确认
)

// TextMessage 短信记录
type TextMessage struct {
	ID               string        `gorm:"primaryKey" json:"id"` // UUID
	ScheduledTaskID  string        `gorm:"index" json:"scheduledTaskId,omitempty"`
	DeviceID         string        `gorm:"index" json:"deviceId"`
	DeviceName       string        `json:"deviceName"`
	SIMID            string        `gorm:"index" json:"simId"`
	ICCID            string        `gorm:"column:iccid;index" json:"iccid"`
	IMSI             string        `json:"imsi"`
	IMEI             string        `gorm:"index" json:"imei"`
	MUID             string        `json:"muid"`
	SIMSlot          int           `json:"simSlot"`
	IdentityRevision uint64        `json:"identityRevision"`
	IdentityConflict bool          `json:"identityConflict"` // 冻结快照可信，但当前检测到重复 ICCID 声明
	Submitted        bool          `json:"submitted"`        // 设备是否已把短信提交给基带
	Ambiguous        bool          `json:"ambiguous"`        // 可能已发送，禁止按普通失败自动重试
	SendError        string        `json:"sendError"`
	SourceID         string        `gorm:"index" json:"-"`                        // 设备侧消息ID，用于接收去重
	From             string        `gorm:"index" json:"from"`                     // 发送方号码
	To               string        `gorm:"index" json:"to"`                       // 接收方号码
	Content          string        `gorm:"type:text" json:"content"`              // 短信内容
	Type             MessageType   `gorm:"index" json:"type"`                     // 消息类型：incoming（收到）、outgoing（发送）
	Status           MessageStatus `gorm:"index" json:"status"`                   // 状态：received、sending、sent、failed、ambiguous
	CreatedAt        int64         `json:"createdAt" gorm:"autoCreateTime:milli"` // 创建时间
	UpdatedAt        int64         `json:"updatedAt" gorm:"autoUpdateTime:milli"` // 更新时间
}

// TableName 指定表名
func (TextMessage) TableName() string {
	return "text_messages"
}
