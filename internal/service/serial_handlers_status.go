package service

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"
)

type StatusData struct {
	DeviceID                string `json:"device_id"`
	DeviceName              string `json:"device_name"`
	SIMID                   string `json:"sim_id"`
	IMEI                    string `json:"imei"`
	MUID                    string `json:"muid"`
	SIMSlot                 int    `json:"sim_slot"`
	IdentityValid           bool   `json:"identity_valid"`
	IdentityRevision        uint64 `json:"identity_revision"`
	IdentityState           string `json:"identity_state"`
	FlymodeOwnerICCID       string `json:"flymode_owner_iccid"`
	FlymodeSource           string `json:"flymode_source"`
	ManualRestoreOwnerICCID string `json:"manual_restore_owner_iccid"`
	Flymode                 bool   `json:"flymode"` // 设备当前是否为飞行模式
	Type                    string `json:"type"`    // 消息类型
	Version                 string `json:"version"` // Lua 脚本版本
	Mobile                  struct {
		IsRegistered bool    `json:"is_registered"`
		IsRoaming    bool    `json:"is_roaming"`
		Flymode      bool    `json:"flymode"`
		Iccid        string  `json:"iccid"`
		Imei         string  `json:"imei"`
		SignalDesc   string  `json:"signal_desc"`
		SignalLevel  int     `json:"signal_level"`
		SimReady     bool    `json:"sim_ready"`
		Rssi         int     `json:"rssi"`
		Csq          int     `json:"csq"`      // CSQ 信号强度 (0-31)
		Rsrp         int     `json:"rsrp"`     // 参考信号接收功率 (-44 到 -140)
		Rsrq         float64 `json:"rsrq"`     // 参考信号发送功率 (-3 到 -19.5)
		Imsi         string  `json:"imsi"`     // SIM 卡 IMSI
		Number       string  `json:"number"`   // 手机号
		Operator     string  `json:"operator"` // 运营商名称
		Uptime       int64   `json:"uptime"`   // 模块开机时长，单位为秒
	} `json:"mobile"`
	Timestamp   int64  `json:"timestamp"`
	MemKb       int    `json:"mem_kb"`
	PortName    string `json:"port_name"` // 串口名称
	Connected   bool   `json:"connected"` // 连接状态
	statusEpoch uint64
}

func (s *SerialService) handleStatusResponse(msg *ParsedMessage) {
	if !s.statusAccepting.Load() {
		return
	}
	statusEpoch := s.statusEpoch.Load()
	var statusData StatusData
	if err := json.Unmarshal([]byte(msg.JSON), &statusData); err != nil {
		s.logger.Error("JSON解析失败", zap.Error(err), zap.String("data", msg.JSON))
		return
	}
	statusData.DeviceID = s.deviceID
	statusData.DeviceName = s.deviceName
	statusData.Mobile.Iccid = normalizeIdentityValue(statusData.Mobile.Iccid)
	statusData.Mobile.Imsi = normalizeIdentityValue(statusData.Mobile.Imsi)
	statusData.Mobile.Imei = normalizeIdentityValue(statusData.Mobile.Imei)
	statusData.IMEI = normalizeIdentityValue(statusData.Mobile.Imei)
	statusData.FlymodeOwnerICCID = normalizeIdentityValue(statusData.FlymodeOwnerICCID)
	statusData.ManualRestoreOwnerICCID = normalizeIdentityValue(statusData.ManualRestoreOwnerICCID)
	statusData.FlymodeSource = normalizeFlymodeSource(statusData.FlymodeSource)
	// sim_id 只允许由本次经过稳定性确认的 ICCID 在主机端生成，绝不信任
	// 设备上报的旧值，避免身份失效期间残留路由继续可用。
	statusData.SIMID = ""
	if statusData.Mobile.SimReady && statusData.IdentityValid {
		statusData.SIMID = makeSIMID(statusData.Mobile.Iccid)
	}
	statusData.PortName, statusData.Connected = s.getConnectionInfo()
	imsi := statusData.Mobile.Imsi
	if len(imsi) > 5 {
		plmn := imsi[:5]
		statusData.Mobile.Operator = func() string {
			if v, ok := OperData[plmn]; ok {
				return v
			}
			return plmn
		}()
	}
	// 解析期间如果发生断线、重启或 SIM 变化，此回包属于上一代状态，不能入缓存。
	s.statusMu.Lock()
	if !s.statusAccepting.Load() || statusEpoch != s.statusEpoch.Load() {
		s.statusMu.Unlock()
		return
	}
	// SIM event 可能因断连或队列压力丢失。如果 status_response 自身已经从
	// 一个路由身份切换到另一个，就把当前回包原子地作为新代际的首个快照；
	// 不能先调用 invalidateDeviceStatus，否则会把刚收到的新状态一起删除。
	identityChanged := false
	if previous, ok := s.deviceCache.Get(CacheKeyDeviceStatus); ok &&
		previous.statusEpoch == statusEpoch {
		previousICCID := verifiedStatusICCID(previous)
		currentICCID := verifiedStatusICCID(&statusData)
		identityChanged = previousICCID != currentICCID &&
			(previousICCID != "" || currentICCID != "")
		if identityChanged {
			statusEpoch = s.statusEpoch.Add(1)
			// 必须先更新时间，再公开新卡状态；否则自动飞行监控可能在
			// 两者之间观察到“新 SIM + 旧卡空闲时间”并立即进入飞行模式。
			s.recordSMSActivity()
		}
	}
	s.flyMode.Store(statusData.Mobile.Flymode)
	statusData.Flymode = statusData.Mobile.Flymode
	if statusData.Mobile.Flymode {
		s.setFlymodeOwner(makeSIMID(statusData.FlymodeOwnerICCID))
		s.autoFlymodeActive.Store(
			statusData.FlymodeOwnerICCID != "" &&
				statusData.FlymodeSource == flymodeProtocolSourceAutomatic,
		)
	} else {
		s.setFlymodeOwner("")
		s.autoFlymodeActive.Store(false)
	}
	statusData.statusEpoch = statusEpoch
	s.deviceCache.Set(CacheKeyDeviceStatus, &statusData, CacheTTL)
	s.statusMu.Unlock()
	observer := s.getStatusObserver()
	if observer != nil && s.statusAccepting.Load() && statusEpoch == s.statusEpoch.Load() {
		observer(&statusData)
	}
	s.recoverManualFlymodeIntent(&statusData)
	s.logger.Debug("设备状态缓存已更新")
}

func verifiedStatusICCID(status *StatusData) string {
	if status == nil || !status.IdentityValid || !status.Mobile.SimReady {
		return ""
	}
	return normalizeIdentityValue(status.Mobile.Iccid)
}

func normalizeFlymodeSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case flymodeProtocolSourceAutomatic:
		return flymodeProtocolSourceAutomatic
	case flymodeProtocolSourceManual:
		return flymodeProtocolSourceManual
	default:
		return ""
	}
}

func (s *SerialService) handleSystemReady(msg *ParsedMessage) {
	if message, ok := msg.Payload["message"].(string); ok {
		s.logger.Info("系统就绪", zap.String("message", message))
	}
	// 模块重启后，重启前的身份和脚本版本一律失效。
	s.resetPhysicalConnectionState()
	s.invalidateDeviceStatus(true)
	go s.RequestCacheUpdate()
}

func (s *SerialService) handleHeartbeat(msg *ParsedMessage) {
	timestamp, _ := msg.Payload["timestamp"].(float64)
	memoryUsage, _ := msg.Payload["memory_usage"].(float64)
	bufferSize, _ := msg.Payload["buffer_size"].(float64)

	s.logger.Debug("设备心跳",
		zap.Int64("timestamp", int64(timestamp)),
		zap.Float64("memory_usage", memoryUsage),
		zap.Int("buffer_size", int(bufferSize)))
}

func (s *SerialService) handleCellularControlResponse(msg *ParsedMessage) {
	s.logger.Debug("收到蜂窝网络控制响应", zap.Any("data", msg.Payload))
}

func (s *SerialService) handlePhoneNumberResponse(msg *ParsedMessage) {
	s.logger.Debug("收到电话号码响应", zap.Any("data", msg.Payload))
}

func (s *SerialService) handleCommandResponse(msg *ParsedMessage) {
	action, _ := msg.Payload["action"].(string)
	result, _ := msg.Payload["result"].(string)
	responseError, _ := msg.Payload["error"].(string)
	requestID, _ := msg.Payload["request_id"].(string)
	if action != "" {
		s.logger.Info("命令响应",
			zap.String("action", action), zap.String("result", result), zap.String("request_id", requestID))
	}
	if requestID != "" {
		if value, ok := s.pendingControls.Load(requestID); ok {
			if responseCh, ok := value.(chan controlCommandResponse); ok {
				select {
				case responseCh <- controlCommandResponse{Action: action, Result: result, Error: responseError}:
				default:
				}
			}
		}
		// 带 request_id 的控制响应由同步调用方提交或回滚状态。这里不得
		// 抢先修改，避免“设备拒绝 -> 调用方稍后仍乐观提交”的反向竞态。
		return
	}
	if result == "ok" {
		return
	}
	// 控制命令由设备端现场身份校验拒绝时，撤销主机的乐观状态并重新同步。
	switch action {
	case "set_flymode":
		if enabled, _ := msg.Payload["enabled"].(bool); enabled {
			s.flyMode.Store(false)
			s.setFlymodeOwner("")
		}
		s.invalidateDeviceStatus(true)
		go s.RequestCacheUpdate()
	case "reboot_mcu":
		s.invalidateDeviceStatus(true)
		go s.RequestCacheUpdate()
	}
}

func (s *SerialService) handleSIMEvent(msg *ParsedMessage) {
	status, _ := msg.Payload["status"].(string)
	s.logger.Info("SIM卡事件", zap.String("status", status))
	status = strings.ToUpper(strings.TrimSpace(status))
	reportedICCID, _ := msg.Payload["iccid"].(string)
	reportedICCID = normalizeIdentityValue(reportedICCID)
	reportedValid, _ := msg.Payload["identity_valid"].(bool)
	current, _ := s.GetStatus()
	sameVerifiedSIM := reportedValid && reportedICCID != "" && current.IdentityValid &&
		current.Mobile.SimReady && reportedICCID == current.Mobile.Iccid

	// RDY/NORDY/SIM_PIN 可能代表拔卡或换卡，必须立即撤销旧路由。
	// GET_NUMBER 等非关键元数据事件如果仍带有与当前缓存一致的可信
	// ICCID，只刷新展示数据，不推进连接代际或取消手动飞行模式恢复。
	critical := status == "RDY" || status == "NORDY" || status == "SIM_PIN"
	identityChanged := reportedValid && reportedICCID != "" &&
		reportedICCID != normalizeIdentityValue(current.Mobile.Iccid)
	if critical || identityChanged {
		// 物理 SIM 变化后为新卡重新计算完整空闲周期，不能继承上一张卡的计时。
		s.recordSMSActivity()
	}
	if critical || !sameVerifiedSIM {
		s.invalidateDeviceStatus(true)
	}
	go s.RequestCacheUpdate()
}

func (s *SerialService) handleWarningMessage(msg *ParsedMessage) {
	if warnMsg, ok := msg.Payload["msg"].(string); ok {
		s.logger.Warn("设备警告", zap.String("message", warnMsg))
	}
}

func (s *SerialService) handleErrorMessage(msg *ParsedMessage) {
	if errMsg, ok := msg.Payload["msg"].(string); ok {
		s.logger.Error("设备错误", zap.String("message", errMsg))
	}
}
