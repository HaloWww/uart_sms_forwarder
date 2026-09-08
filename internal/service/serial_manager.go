package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.uber.org/zap"
)

// SerialManager 管理多台 Air780，并为旧版未指定 deviceId 的调用保留默认设备。
type SerialManager struct {
	logger    *zap.Logger
	services  map[string]*SerialService
	defaultID string
}

func NewSerialManager(
	logger *zap.Logger,
	serialConfig config.SerialConfig,
	textMsgService *TextMessageService,
	notifier *Notifier,
	propertyService *PropertyService,
) (*SerialManager, error) {
	devices := serialConfig.Devices
	if len(devices) == 0 {
		devices = []config.SerialDeviceConfig{{ID: "default", Name: "Air780", Port: serialConfig.Port}}
	}

	manager := &SerialManager{logger: logger, services: make(map[string]*SerialService)}
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

func (m *SerialManager) service(deviceID string) (*SerialService, error) {
	if deviceID == "" {
		deviceID = m.defaultID
	}
	service, ok := m.services[deviceID]
	if !ok {
		return nil, fmt.Errorf("设备不存在或未启用: %s", deviceID)
	}
	return service, nil
}

func (m *SerialManager) SendSMS(deviceID, to, content string) (string, error) {
	if strings.Contains(content, ":CMD_END") {
		return "", fmt.Errorf("短信内容包含串口协议保留标记 :CMD_END")
	}
	service, err := m.service(deviceID)
	if err != nil { return "", err }
	return service.SendSMS(to, content)
}

func (m *SerialManager) GetStatus(deviceID string) (*StatusData, error) {
	service, err := m.service(deviceID)
	if err != nil { return nil, err }
	return service.GetStatus()
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

func (m *SerialManager) SetFlymode(deviceID string, enabled bool) error {
	service, err := m.service(deviceID)
	if err != nil { return err }
	if err := service.SetFlymode(enabled); err != nil { return err }
	go service.RequestCacheUpdate()
	return nil
}

func (m *SerialManager) RebootMcu(deviceID string) error {
	service, err := m.service(deviceID)
	if err != nil { return err }
	if err := service.RebootMcu(); err != nil { return err }
	go service.RequestCacheUpdate()
	return nil
}
