package models

type LastRunStatus string

const (
	LastRunStatusUnknown   LastRunStatus = "unknown"
	LastRunStatusSuccess   LastRunStatus = "success"
	LastRunStatusFailed    LastRunStatus = "failed"
	LastRunStatusAmbiguous LastRunStatus = "ambiguous"
)

// ScheduledTask 定时任务
type ScheduledTask struct {
	ID           string `gorm:"primaryKey" json:"id"`                  // UUID
	DeviceID     string `gorm:"index" json:"deviceId,omitempty"`       // 旧版物理设备字段，仅保留审计/迁移，禁止作为发送回退
	SIMID        string `gorm:"index" json:"simId"`                    // 目标 SIM（由 ICCID 派生）；发送时必须填写
	Name         string `json:"name"`                                  // 任务名称
	Enabled      bool   `json:"enabled"`                               // 是否启用
	IntervalDays int    `json:"intervalDays"`                          // 执行间隔天数，例如 90 表示每90天执行一次
	PhoneNumber  string `json:"phoneNumber"`                           // 目标手机号
	Content      string `gorm:"type:text" json:"content"`              // 短信内容
	CreatedAt    int64  `json:"createdAt" gorm:"autoCreateTime:milli"` // 创建时间（时间戳毫秒）
	UpdatedAt    int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"` // 更新时间（时间戳毫秒）

	LastMsgId     string        `json:"lastMsgId"`     // 上次发送的短信ID
	LastRunAt     int64         `json:"lastRunAt"`     // 上次执行时间（时间戳毫秒）
	LastRunStatus LastRunStatus `json:"lastRunStatus"` // 上次执行状态
}

func (ScheduledTask) TableName() string {
	return "scheduled_tasks"
}
