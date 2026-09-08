package models

// SIMProfile 是按 ICCID 建立的持久化 SIM 档案。连接位置会随换卡动态更新。
type SIMProfile struct {
	ID             string `gorm:"primaryKey" json:"simId"`
	Name           string `json:"name"`
	ICCID          string `gorm:"column:iccid;uniqueIndex" json:"iccid"`
	IMSI           string `gorm:"index" json:"imsi"`
	LastIMEI       string `gorm:"index" json:"lastImei"`
	LastDeviceID   string `json:"lastDeviceId"`
	LastDeviceName string `json:"lastDeviceName"`
	LastPort       string `json:"lastPort"`
	FirstSeenAt    int64  `json:"firstSeenAt"`
	LastSeenAt     int64  `json:"lastSeenAt"`
}

func (SIMProfile) TableName() string { return "sim_profiles" }
