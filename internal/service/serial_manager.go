package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

var (
	ErrSIMIdentityRequired  = errors.New("必须指定 SIM 身份")
	ErrSIMUnknown           = errors.New("目标 SIM 尚未登记")
	ErrSIMOffline           = errors.New("目标 SIM 当前离线或身份尚未确认")
	ErrSIMMismatch          = errors.New("当前模块内的 SIM 与目标不一致")
	ErrSIMConflict          = errors.New("检测到重复 SIM 身份，已阻止操作")
	ErrSIMTopologyUnsettled = errors.New("仍有已连接设备正在确认 SIM 身份")
	ErrSIMProtocolOutdated  = errors.New("Air780 Lua 脚本不支持安全的 SIM 身份校验")
)

const UnassignedSIMID = "legacy:unassigned"

// SerialManager 管理串口连接，并根据 ICCID 将业务请求动态路由到 SIM 当前所在的连接。
type SerialManager struct {
	logger         *zap.Logger
	db             *gorm.DB
	textMsgService *TextMessageService
	services       map[string]*SerialService
	defaultID      string
}

type SIMStatus struct {
	SIMID            string      `json:"simId"`
	Name             string      `json:"name"`
	ICCID            string      `json:"iccid"`
	IMSI             string      `json:"imsi"`
	Online           bool        `json:"online"`
	SendReady        bool        `json:"sendReady"`
	Conflict         bool        `json:"conflict"`
	ScriptCompatible bool        `json:"scriptCompatible"`
	LastIMEI         string      `json:"lastImei"`
	LastDeviceID     string      `json:"lastDeviceId"`
	LastDeviceName   string      `json:"lastDeviceName"`
	LastPort         string      `json:"lastPort"`
	FirstSeenAt      int64       `json:"firstSeenAt"`
	LastSeenAt       int64       `json:"lastSeenAt"`
	CurrentStatus    *StatusData `json:"currentStatus,omitempty"`
}

func NewSerialManager(
	logger *zap.Logger,
	db *gorm.DB,
	serialConfig config.SerialConfig,
	textMsgService *TextMessageService,
	notifier *Notifier,
	propertyService *PropertyService,
) (*SerialManager, error) {
	devices := serialConfig.Devices
	if len(devices) == 0 {
		devices = []config.SerialDeviceConfig{{ID: "default", Name: "Air780", Port: serialConfig.Port}}
	}

	manager := &SerialManager{
		logger: logger, db: db, textMsgService: textMsgService,
		services: make(map[string]*SerialService),
	}
	enabledCount := 0
	for _, device := range devices {
		if device.IsEnabled() {
			enabledCount++
		}
	}
	usedPorts := make(map[string]string)
	for _, device := range devices {
		if !device.IsEnabled() {
			continue
		}
		device.ID = strings.TrimSpace(device.ID)
		if device.ID == "" {
			return nil, fmt.Errorf("Air780 设备 ID 不能为空")
		}
		if _, exists := manager.services[device.ID]; exists {
			return nil, fmt.Errorf("Air780 设备 ID 重复: %s", device.ID)
		}
		if device.Name == "" {
			device.Name = device.ID
		}
		if enabledCount > 1 && strings.TrimSpace(device.Port) == "" {
			return nil, fmt.Errorf("多设备模式必须为设备 %s 指定串口", device.ID)
		}
		if otherID, exists := usedPorts[device.Port]; device.Port != "" && exists {
			return nil, fmt.Errorf("设备 %s 与 %s 使用了相同串口: %s", device.ID, otherID, device.Port)
		}
		if device.Port != "" {
			usedPorts[device.Port] = device.ID
		}
		service := NewSerialService(
			logger.With(zap.String("device_id", device.ID), zap.String("device_name", device.Name)),
			config.SerialConfig{Port: device.Port},
			device.ID,
			device.Name,
			textMsgService,
			notifier,
			propertyService,
		)
		service.SetStatusObserver(manager.observeStatus)
		service.SetSIMRouteValidator(manager.validateSIMRoute)
		manager.services[device.ID] = service
		if manager.defaultID == "" {
			manager.defaultID = device.ID
		}
	}
	if len(manager.services) == 0 {
		return nil, fmt.Errorf("至少需要启用一台 Air780 设备")
	}
	return manager, nil
}

func (m *SerialManager) Start(ctx context.Context) {
	for _, serialService := range m.services {
		service := serialService
		go service.Start()
		go service.StartAutoFlymodeMonitor(ctx)
	}
}

func (m *SerialManager) SetScheduledTaskStatusUpdater(updater ScheduledTaskStatusUpdater) {
	for _, service := range m.services {
		service.SetScheduledTaskStatusUpdater(updater)
	}
}

func (m *SerialManager) DefaultDevice() (string, string) {
	service := m.services[m.defaultID]
	return service.DeviceID(), service.DeviceName()
}

func (m *SerialManager) observeStatus(status *StatusData) {
	if m.db == nil || status == nil || status.SIMID == "" || !status.IdentityValid || !status.Mobile.SimReady {
		return
	}
	identity := identityFromStatus(status)
	if identity.ICCID == "" {
		return
	}
	now := time.Now().UnixMilli()
	name := "SIM " + identity.ICCID
	if len(identity.ICCID) > 6 {
		name = "SIM •" + identity.ICCID[len(identity.ICCID)-6:]
	}
	profile := models.SIMProfile{ID: identity.SIMID}
	if err := m.db.Where("id = ?", identity.SIMID).Attrs(models.SIMProfile{
		Name: name, ICCID: identity.ICCID, FirstSeenAt: now,
	}).FirstOrCreate(&profile).Error; err != nil {
		m.logger.Error("保存 SIM 档案失败", zap.String("sim_id", identity.SIMID), zap.Error(err))
		return
	}
	updates := map[string]any{
		"iccid": identity.ICCID, "imsi": identity.IMSI, "last_imei": identity.IMEI,
		"last_device_id": status.DeviceID, "last_device_name": status.DeviceName,
		"last_port": status.PortName, "last_seen_at": now,
	}
	if err := m.db.Model(&models.SIMProfile{}).Where("id = ?", identity.SIMID).Updates(updates).Error; err != nil {
		m.logger.Error("更新 SIM 档案失败", zap.String("sim_id", identity.SIMID), zap.Error(err))
	}
}

func (m *SerialManager) deviceService(deviceID string) (*SerialService, error) {
	if deviceID == "" {
		deviceID = m.defaultID
	}
	service, ok := m.services[deviceID]
	if !ok {
		return nil, fmt.Errorf("设备不存在或未启用: %s", deviceID)
	}
	return service, nil
}

func (m *SerialManager) resolveLiveSIM(simID string) (*SerialService, SIMIdentity, error) {
	simID = strings.TrimSpace(simID)
	if simID == "" || simID == UnassignedSIMID {
		return nil, SIMIdentity{}, ErrSIMIdentityRequired
	}
	type match struct {
		service  *SerialService
		identity SIMIdentity
	}
	matches := make([]match, 0, 1)
	claims := make(map[string]struct{})
	unsettledDevices := make([]string, 0)
	for _, serialService := range m.services {
		status, _ := serialService.GetStatus()
		if !status.Connected {
			continue
		}
		settled := false
		if status.SIMID == simID && status.Mobile.SimReady && status.IdentityValid {
			identity := identityFromStatus(status)
			if identity.ICCID != "" {
				matches = append(matches, match{service: serialService, identity: identity})
				claims[serialService.DeviceID()] = struct{}{}
			}
		}
		if status.Mobile.SimReady && status.IdentityValid && status.SIMID != "" {
			settled = true
		}
		// 飞行模式会隐藏 verified 身份，但 Lua 保存的 owner 仍是当前连接
		// 对该 SIM 的独占声明。verified+flight 或 flight+flight 都必须冲突关闭。
		if serialService.FlyMode() && serialService.FlymodeOwnerSIMID() == simID {
			claims[serialService.DeviceID()] = struct{}{}
		}
		if serialService.FlyMode() && serialService.FlymodeOwnerSIMID() != "" {
			settled = true
		}
		if settledEmptySIMStatus(status) {
			settled = true
		}
		if !settled {
			unsettledDevices = append(unsettledDevices, serialService.DeviceID())
		}
	}
	if len(matches) > 1 || len(claims) > 1 {
		return nil, SIMIdentity{}, fmt.Errorf("%w: %s", ErrSIMConflict, simID)
	}
	if len(matches) == 1 {
		if len(unsettledDevices) > 0 {
			return nil, SIMIdentity{}, fmt.Errorf(
				"%w: %s", ErrSIMTopologyUnsettled, strings.Join(unsettledDevices, ","),
			)
		}
		return matches[0].service, matches[0].identity, nil
	}
	if _, err := m.getSIMProfile(context.Background(), simID); err != nil {
		return nil, SIMIdentity{}, err
	}
	return nil, SIMIdentity{}, fmt.Errorf("%w: %s", ErrSIMOffline, simID)
}

func settledEmptySIMStatus(status *StatusData) bool {
	if status == nil || !supportsSIMIdentityProtocol(status.Version) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(status.IdentityState)) {
	case "no_sim", "no_active_slot", "pin_required":
		return true
	default:
		return false
	}
}

// validateSIMRoute 在真正写 UART 前和接收入站短信前做最后一次全局核对。
// 除目标连接必须仍持有 verified ICCID 外，其他已连接端点也必须完成身份收敛；
// 否则无法排除第二台设备稍后声明同一 ICCID。
func (m *SerialManager) validateSIMRoute(deviceID string, expected SIMIdentity) error {
	if expected.SIMID == "" || expected.ICCID == "" {
		return ErrSIMIdentityRequired
	}
	claims := make(map[string]struct{})
	targetVerified := false
	unsettledDevices := make([]string, 0)
	for _, serialService := range m.services {
		status, _ := serialService.GetStatus()
		if !status.Connected {
			continue
		}
		settled := false
		if status.Mobile.SimReady && status.IdentityValid && status.SIMID != "" {
			settled = true
			if status.SIMID == expected.SIMID &&
				normalizeIdentityValue(status.Mobile.Iccid) == expected.ICCID {
				claims[serialService.DeviceID()] = struct{}{}
				if serialService.DeviceID() == deviceID {
					targetVerified = true
				}
			}
		}
		if serialService.FlyMode() && serialService.FlymodeOwnerSIMID() != "" {
			settled = true
			if serialService.FlymodeOwnerSIMID() == expected.SIMID {
				claims[serialService.DeviceID()] = struct{}{}
			}
		}
		if settledEmptySIMStatus(status) {
			settled = true
		}
		if !settled {
			unsettledDevices = append(unsettledDevices, serialService.DeviceID())
		}
	}
	if len(claims) > 1 {
		return fmt.Errorf("%w: %s", ErrSIMConflict, expected.SIMID)
	}
	if !targetVerified {
		return fmt.Errorf("%w: %s", ErrSIMMismatch, expected.SIMID)
	}
	if len(unsettledDevices) > 0 {
		return fmt.Errorf("%w: %s", ErrSIMTopologyUnsettled, strings.Join(unsettledDevices, ","))
	}
	return nil
}

func (m *SerialManager) resolveSIM(simID string) (*SerialService, SIMIdentity, error) {
	serialService, identity, err := m.resolveLiveSIM(simID)
	if err == nil {
		return serialService, identity, nil
	}
	if !errors.Is(err, ErrSIMOffline) {
		return nil, SIMIdentity{}, err
	}
	simID = strings.TrimSpace(simID)

	// 飞行模式下 modem 暂时无法给出 verified 身份。Lua 在当前连接上冻结的
	// owner ICCID 才是路由依据；历史 DeviceID/串口只作审计，设备换口后不能反向限制 SIM。
	profile, err := m.getSIMProfile(context.Background(), simID)
	if err != nil {
		return nil, SIMIdentity{}, err
	}
	var ownerService *SerialService
	unsettledDevices := make([]string, 0)
	for _, candidate := range m.services {
		status, _ := candidate.GetStatus()
		if !status.Connected {
			continue
		}
		if candidate.FlyMode() && candidate.FlymodeOwnerSIMID() == simID {
			if ownerService != nil && ownerService != candidate {
				return nil, SIMIdentity{}, fmt.Errorf("%w: %s", ErrSIMConflict, simID)
			}
			ownerService = candidate
			continue
		}
		settled := status.Mobile.SimReady && status.IdentityValid && status.SIMID != ""
		settled = settled || (candidate.FlyMode() && candidate.FlymodeOwnerSIMID() != "")
		settled = settled || settledEmptySIMStatus(status)
		if !settled {
			unsettledDevices = append(unsettledDevices, candidate.DeviceID())
		}
	}
	if ownerService != nil {
		if len(unsettledDevices) > 0 {
			return nil, SIMIdentity{}, fmt.Errorf(
				"%w: %s", ErrSIMTopologyUnsettled, strings.Join(unsettledDevices, ","),
			)
		}
		return ownerService, SIMIdentity{
			SIMID: simID, ICCID: profile.ICCID, IMSI: profile.IMSI, IMEI: profile.LastIMEI,
		}, nil
	}
	return nil, SIMIdentity{}, fmt.Errorf("%w: %s", ErrSIMOffline, simID)
}

func (m *SerialManager) SendSMS(simID, to, content string) (string, error) {
	return m.sendSMSWithRequestContext(simID, to, content, "", "")
}

// SendSMSWithRequestID 使用调用方生成的 UUID 作为端到端幂等键。
func (m *SerialManager) SendSMSWithRequestID(simID, to, content, requestID string) (string, error) {
	return m.sendSMSWithRequestContext(simID, to, content, requestID, "")
}

// sendScheduledSMSWithRequestID 将请求永久绑定到计划任务。即使该任务以后
// 又执行了新一轮，旧 requestId 的重放也只能读取原结果，不能再次下发。
func (m *SerialManager) sendScheduledSMSWithRequestID(
	taskID, simID, to, content, requestID string,
) (string, error) {
	return m.sendSMSWithRequestContext(simID, to, content, requestID, taskID)
}

func (m *SerialManager) sendSMSWithRequestContext(
	simID, to, content, requestID, scheduledTaskID string,
) (string, error) {
	simID = strings.TrimSpace(simID)
	if simID == "" || simID == UnassignedSIMID {
		return "", ErrSIMIdentityRequired
	}
	to = strings.TrimSpace(to)
	if err := ValidateSMSDestination(to); err != nil {
		return "", err
	}
	if err := ValidateSMSContent(content); err != nil {
		return "", err
	}
	if strings.Contains(content, ":CMD_END") {
		return "", fmt.Errorf("短信内容包含串口协议保留标记 :CMD_END")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = uuid.NewString()
	} else {
		parsed, err := uuid.Parse(requestID)
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrSMSRequestIDInvalid, requestID)
		}
		requestID = parsed.String()
	}

	claimed := false
	if m.textMsgService != nil {
		request := &models.TextMessage{
			ID: requestID, ScheduledTaskID: strings.TrimSpace(scheduledTaskID),
			SIMID: simID, To: to, Content: content,
			Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
			CreatedAt: time.Now().UnixMilli(),
		}
		existing, wonClaim, err := m.textMsgService.ClaimOutgoingRequest(context.Background(), request)
		if err != nil {
			return requestID, err
		}
		if !wonClaim {
			switch existing.Status {
			case models.MessageStatusAmbiguous:
				return existing.ID, ErrSMSSubmissionAmbiguous
			case models.MessageStatusFailed:
				return existing.ID, fmt.Errorf(
					"%w: %s", ErrSMSRequestPreviouslyFailed, existing.SendError,
				)
			default:
				return existing.ID, nil
			}
		}
		claimed = true
	}

	serialService, identity, err := m.resolveSIM(simID)
	if err != nil {
		m.failClaimedSMS(requestID, claimed, "route_failed")
		return requestID, err
	}
	messageID, err := serialService.SendSMS(identity, to, content, requestID)
	if err != nil && !errors.Is(err, ErrSMSSubmissionAmbiguous) {
		m.failClaimedSMS(requestID, claimed, "submission_failed")
	}
	if messageID == "" && claimed {
		messageID = requestID
	}
	return messageID, err
}

func (m *SerialManager) failClaimedSMS(requestID string, claimed bool, sendError string) {
	if !claimed || m.textMsgService == nil {
		return
	}
	if _, err := m.textMsgService.UpdateSendResultById(
		context.Background(), requestID, models.MessageStatusFailed, false, false, sendError,
	); err != nil {
		m.logger.Error("更新幂等短信请求失败", zap.String("request_id", requestID), zap.Error(err))
	}
}

func (m *SerialManager) GetStatusBySIM(simID string) (*StatusData, error) {
	serialService, _, err := m.resolveSIM(simID)
	if err != nil {
		return nil, err
	}
	return serialService.GetStatus()
}

func (m *SerialManager) GetDeviceStatus(deviceID string) (*StatusData, error) {
	serialService, err := m.deviceService(deviceID)
	if err != nil {
		return nil, err
	}
	return serialService.GetStatus()
}

func (m *SerialManager) GetStatuses() []*StatusData {
	statuses := make([]*StatusData, 0, len(m.services))
	for _, service := range m.services {
		status, _ := service.GetStatus()
		statuses = append(statuses, status)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].DeviceID < statuses[j].DeviceID })
	return statuses
}

func (m *SerialManager) GetSIMs(ctx context.Context) ([]SIMStatus, error) {
	profiles := make([]models.SIMProfile, 0)
	if m.db != nil {
		if err := m.db.WithContext(ctx).Order("last_seen_at DESC").Find(&profiles).Error; err != nil {
			return nil, err
		}
	}
	live := make(map[string][]*StatusData)
	claims := make(map[string]map[string]struct{})
	statuses := m.GetStatuses()
	topologySettled := true
	for _, status := range statuses {
		if status.Connected && status.SIMID != "" && status.IdentityValid && status.Mobile.SimReady {
			live[status.SIMID] = append(live[status.SIMID], status)
			if claims[status.SIMID] == nil {
				claims[status.SIMID] = make(map[string]struct{})
			}
			claims[status.SIMID][status.DeviceID] = struct{}{}
		}
		if !status.Connected {
			continue
		}
		if serialService, ok := m.services[status.DeviceID]; ok && serialService.FlyMode() {
			owner := serialService.FlymodeOwnerSIMID()
			if owner != "" {
				if claims[owner] == nil {
					claims[owner] = make(map[string]struct{})
				}
				claims[owner][status.DeviceID] = struct{}{}
			}
		}
		serialService := m.services[status.DeviceID]
		settled := status.Mobile.SimReady && status.IdentityValid && status.SIMID != ""
		settled = settled || (serialService != nil && serialService.FlyMode() &&
			serialService.FlymodeOwnerSIMID() != "")
		settled = settled || settledEmptySIMStatus(status)
		if !settled {
			topologySettled = false
		}
	}
	result := make([]SIMStatus, 0, len(profiles))
	for _, profile := range profiles {
		matches := live[profile.ID]
		conflict := len(matches) > 1 || len(claims[profile.ID]) > 1
		item := SIMStatus{
			SIMID: profile.ID, Name: profile.Name, ICCID: profile.ICCID, IMSI: profile.IMSI,
			LastIMEI: profile.LastIMEI, LastDeviceID: profile.LastDeviceID,
			LastDeviceName: profile.LastDeviceName, LastPort: profile.LastPort,
			FirstSeenAt: profile.FirstSeenAt, LastSeenAt: profile.LastSeenAt,
			Online: len(matches) == 1 && !conflict, Conflict: conflict,
		}
		if len(matches) == 1 && !conflict {
			item.CurrentStatus = matches[0]
			item.ScriptCompatible = supportsSIMIdentityProtocol(matches[0].Version)
			item.SendReady = matches[0].Mobile.SimReady && item.ScriptCompatible && topologySettled
		} else if len(matches) == 0 && !conflict {
			// 飞行模式会隐藏 verified 身份；使用当前连接由 Lua 回报的唯一 owner，
			// 不依赖 SIM 历史所在的 DeviceID/串口。
			if len(claims[profile.ID]) == 1 {
				for deviceID := range claims[profile.ID] {
					serialService, ok := m.services[deviceID]
					if !ok || !serialService.FlyMode() ||
						serialService.FlymodeOwnerSIMID() != profile.ID {
						continue
					}
					status, _ := serialService.GetStatus()
					if status.Connected && status.SIMID == "" {
						item.Online = true
						item.CurrentStatus = status
						item.ScriptCompatible = supportsSIMIdentityProtocol(status.Version)
						item.SendReady = item.ScriptCompatible && topologySettled
					}
				}
			}
		}
		result = append(result, item)
		delete(live, profile.ID)
		delete(claims, profile.ID)
	}
	for simID, matches := range live {
		if len(matches) == 0 {
			continue
		}
		identity := identityFromStatus(matches[0])
		name := "SIM " + identity.ICCID
		if len(identity.ICCID) > 6 {
			name = "SIM •" + identity.ICCID[len(identity.ICCID)-6:]
		}
		conflict := len(matches) > 1 || len(claims[simID]) > 1
		item := SIMStatus{SIMID: simID, Name: name, ICCID: identity.ICCID, IMSI: identity.IMSI,
			Online: len(matches) == 1 && !conflict, Conflict: conflict}
		if len(matches) == 1 && !conflict {
			item.CurrentStatus = matches[0]
			item.ScriptCompatible = supportsSIMIdentityProtocol(matches[0].Version)
			item.SendReady = matches[0].Mobile.SimReady && item.ScriptCompatible && topologySettled
		}
		result = append(result, item)
	}
	if m.db != nil {
		var unassignedCount int64
		if err := m.db.WithContext(ctx).Model(&models.TextMessage{}).
			Where("sim_id = '' OR sim_id IS NULL").Count(&unassignedCount).Error; err != nil {
			return nil, err
		}
		if unassignedCount > 0 {
			result = append(result, SIMStatus{
				SIMID: UnassignedSIMID,
				Name:  "未分配短信",
			})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Online != result[j].Online {
			return result[i].Online
		}
		if result[i].LastSeenAt != result[j].LastSeenAt {
			return result[i].LastSeenAt > result[j].LastSeenAt
		}
		return result[i].SIMID < result[j].SIMID
	})
	return result, nil
}

// ValidateSIM 确认 simId 来自曾经读取到的 ICCID。允许已知但当前离线的 SIM，
// 以便未插卡时仍能编辑计划任务；真正发送时仍由 resolveSIM 严格校验在线状态。
func (m *SerialManager) ValidateSIM(ctx context.Context, simID string) error {
	simID = strings.TrimSpace(simID)
	if simID == "" || simID == UnassignedSIMID {
		return ErrSIMIdentityRequired
	}
	for _, status := range m.GetStatuses() {
		if status.SIMID == simID && status.Mobile.SimReady && status.IdentityValid {
			return nil
		}
	}
	_, err := m.getSIMProfile(ctx, simID)
	return err
}

func (m *SerialManager) getSIMProfile(ctx context.Context, simID string) (*models.SIMProfile, error) {
	if m.db == nil {
		return nil, fmt.Errorf("%w: %s", ErrSIMUnknown, simID)
	}
	var profile models.SIMProfile
	if err := m.db.WithContext(ctx).Where("id = ?", simID).First(&profile).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrSIMUnknown, simID)
		}
		return nil, err
	}
	return &profile, nil
}

func (m *SerialManager) SetFlymode(simID string, enabled bool) error {
	serialService, identity, err := m.resolveSIM(simID)
	if err != nil {
		return err
	}
	if err := serialService.SetFlymode(identity, enabled); err != nil {
		return err
	}
	serialService.invalidateDeviceStatus(true)
	serialService.RequestCacheUpdate()
	if !enabled {
		return serialService.waitForExpectedSIMIdentity(context.Background(), identity)
	}
	return nil
}

func (m *SerialManager) RebootMcu(simID, deviceID string) error {
	var serialService *SerialService
	var expected *SIMIdentity
	var err error
	if simID != "" {
		// 重启是物理操作且无法在重启后复核；只允许当前 verified 路由，
		// 不能使用飞行模式下的历史承载关系猜测设备。
		var identity SIMIdentity
		serialService, identity, err = m.resolveLiveSIM(simID)
		expected = &identity
	} else {
		serialService, err = m.deviceService(deviceID)
	}
	if err != nil {
		return err
	}
	if err := serialService.RebootMcu(expected); err != nil {
		return err
	}
	return nil
}
