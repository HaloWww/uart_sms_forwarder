package config

type AppConfig struct {
	JWT    JWTConfig         `json:"JWT"`
	Users  map[string]string `json:"Users"`  // 用户名 -> bcrypt加密的密码
	Serial SerialConfig      `json:"Serial"` // 串口配置
	OIDC   *OIDCConfig       `json:"OIDC"`   // OIDC配置（可选）
}

// JWTConfig JWT配置
type JWTConfig struct {
	Secret       string `json:"Secret"`
	ExpiresHours int    `json:"ExpiresHours"`
}

// SerialConfig 串口配置
type SerialConfig struct {
	// 默认不需要列出设备：当 Port 和 Devices 都为空时，系统会自动发现所有
	// 运行本项目 Lua 脚本的 Air780。Ports 只用于限制自动探测范围，不是设备身份。
	AutoDiscover *bool                `json:"AutoDiscover"`
	Ports        []string             `json:"Ports"`
	Port         string               `json:"Port"`    // 兼容旧版单设备配置
	Devices      []SerialDeviceConfig `json:"Devices"` // 兼容旧版静态多设备配置
}

// SerialDeviceConfig 描述一条 Air780 物理连接。ID 只标识连接配置；
// 短信和计划任务使用 ICCID 派生的 simId，不会因模块或串口变化而改变归属。
type SerialDeviceConfig struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Port    string `json:"Port"`
	Enabled *bool  `json:"Enabled"` // nil 表示启用，兼容未填写该字段的配置
}

func (c SerialDeviceConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// OIDCConfig OIDC认证配置
type OIDCConfig struct {
	Enabled      bool   `json:"Enabled"`      // 是否启用OIDC
	Issuer       string `json:"Issuer"`       // OIDC Provider的Issuer URL
	ClientID     string `json:"ClientID"`     // Client ID
	ClientSecret string `json:"ClientSecret"` // Client Secret
	RedirectURL  string `json:"RedirectURL"`  // 回调URL
}
