package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/go-orz/cache"
	"github.com/google/uuid"
	"github.com/jpillora/backoff"
	"go.bug.st/serial"
	"go.uber.org/zap"
)

const (
	// 缓存键
	CacheKeyDeviceStatus = "device_status"
	// 缓存刷新间隔
	CacheRefreshInterval = 10 * time.Second
	// 缓存过期时间
	CacheTTL = 5 * time.Minute
	// 物理控制命令必须收到同一 request_id 的设备确认后才提交主机状态。
	controlCommandResponseTimeout = 5 * time.Second
	// serial.Open 没有 context API；隔离后最多等待这一时长，端点回收也可
	// 通过父 context 立即终止当前 worker。
	serialConnectWindow = 5 * time.Second
	// 16 KiB 在 115200 baud 下正常写出远小于此值；超时意味着驱动或
	// 设备异常，必须摘除句柄并重连，不能永久占住全局路由租约。
	defaultSerialWriteWindow = 5 * time.Second
	// 与 main.lua 的 max_uart_recv_buffer_size 保持一致。
	MaxUARTCommandFrameBytes = 16 * 1024
)

var (
	ErrSMSSubmissionAmbiguous = errors.New("短信命令可能已写入设备，发送状态不确定")
	ErrSMSOperationPending    = errors.New("仍有短信等待设备最终结果")
	ErrControlCommandRejected = errors.New("设备拒绝执行控制命令")
	ErrControlCommandTimeout  = errors.New("等待设备确认控制命令超时")
)

type controlCommandResponse struct {
	Action string
	Result string
	Error  string
}

type ScheduledTaskStatusUpdater func(ctx context.Context, msgID string, status models.LastRunStatus) error

// SerialService 串口管理服务
type SerialService struct {
	logger                     *zap.Logger
	config                     config.SerialConfig
	deviceID                   string
	deviceName                 string
	port                       serial.Port
	textMsgService             *TextMessageService
	notifier                   *Notifier
	propertyService            *PropertyService
	handlers                   map[string]messageHandler
	callbacksMu                sync.RWMutex
	statusObserver             func(*StatusData)
	simRouteValidator          func(deviceID string, identity SIMIdentity) error
	routeWriteGuard            func(operation func() error) error
	scheduledTaskStatusUpdater ScheduledTaskStatusUpdater
	requireDiscoveryHandshake  bool
	skipNextDiscoveryHandshake bool
	wg                         sync.WaitGroup
	// 设备信息缓存
	deviceCache cache.Cache[string, *StatusData]
	// 连接状态管理
	mu        sync.RWMutex
	portName  string // 当前使用的串口名称
	connected bool   // 连接状态
	portMu    sync.RWMutex
	writeMu   sync.Mutex // 只串行化写操作；关闭端口绝不能等待这把锁
	portEpoch uint64
	smsSendMu sync.Mutex
	writeWait time.Duration
	// 每次串口重连、模块重启或 SIM 变化都会推进状态代际。业务路由只接受
	// 当前代际的新状态，防止上一台模块/上一张卡的缓存被新连接复用。
	statusEpoch     atomic.Uint64
	statusAccepting atomic.Bool
	statusMu        sync.Mutex
	// Lua 版本属于物理连接而不是 SIM 身份。飞行模式会暂时失效 SIM
	// 状态，但不能因此把已确认的脚本版本误判成“过旧”。
	scriptVersionMu sync.RWMutex
	scriptVersion   string

	// 设备的飞行模式查询永远返回 false，无奈只能在应用层处理
	flyMode           atomic.Bool
	flymodeOwnerMu    sync.RWMutex
	flymodeOwnerSIMID string
	// 自动飞行模式运行状态
	lastSMSActivityAt   atomic.Int64
	autoFlymodeActive   atomic.Bool
	smsOperationRunning atomic.Bool
	manualFlymodeGen    atomic.Uint64
	manualRestoreActive atomic.Bool
	restoreFlymodeByMsg sync.Map
	pendingSMSTimers    sync.Map
	pendingControls     sync.Map
	// 计划任务终态写入失败时，每个 messageId 最多启动一个后台对账循环。
	scheduledTaskStatusRetries sync.Map
	scheduledTaskRetryInitial  time.Duration
	scheduledTaskRetryMax      time.Duration
}

// NewSerialService 创建串口服务实例
func NewSerialService(
	logger *zap.Logger,
	config config.SerialConfig,
	deviceID string,
	deviceName string,
	textMsgService *TextMessageService,
	notifier *Notifier,
	propertyService *PropertyService,
) *SerialService {
	service := &SerialService{
		logger:          logger,
		config:          config,
		deviceID:        deviceID,
		deviceName:      deviceName,
		portName:        config.Port,
		textMsgService:  textMsgService,
		notifier:        notifier,
		propertyService: propertyService,
		deviceCache:     cache.New[string, *StatusData](CacheTTL),
		writeWait:       defaultSerialWriteWindow,
	}
	service.lastSMSActivityAt.Store(time.Now().UnixMilli())
	service.statusAccepting.Store(true)
	service.initMessageHandlers()
	return service
}

func (s *SerialService) DeviceID() string   { return s.deviceID }
func (s *SerialService) DeviceName() string { return s.deviceName }

func (s *SerialService) SetStatusObserver(observer func(*StatusData)) {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()
	s.statusObserver = observer
}

func (s *SerialService) SetSIMRouteValidator(
	validator func(deviceID string, identity SIMIdentity) error,
) {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()
	s.simRouteValidator = validator
}

func (s *SerialService) SetRouteWriteGuard(guard func(operation func() error) error) {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()
	s.routeWriteGuard = guard
}

func (s *SerialService) SetScheduledTaskStatusUpdater(updater ScheduledTaskStatusUpdater) {
	s.callbacksMu.Lock()
	defer s.callbacksMu.Unlock()
	s.scheduledTaskStatusUpdater = updater
}

func (s *SerialService) getStatusObserver() func(*StatusData) {
	s.callbacksMu.RLock()
	defer s.callbacksMu.RUnlock()
	return s.statusObserver
}

func (s *SerialService) getSIMRouteCallbacks() (
	func(deviceID string, identity SIMIdentity) error,
	func(operation func() error) error,
) {
	s.callbacksMu.RLock()
	defer s.callbacksMu.RUnlock()
	return s.simRouteValidator, s.routeWriteGuard
}

func (s *SerialService) getScheduledTaskStatusUpdater() ScheduledTaskStatusUpdater {
	s.callbacksMu.RLock()
	defer s.callbacksMu.RUnlock()
	return s.scheduledTaskStatusUpdater
}

func (s *SerialService) withRouteWriteGuard(operation func() error) error {
	_, guard := s.getSIMRouteCallbacks()
	if guard != nil {
		return guard(operation)
	}
	return operation()
}

// Start 启动串口服务（使用 backoff 重连机制）。ctx 取消时会关闭当前
// 串口并终止重连，供自动发现协调器安全回收已拔出的端点。
func (s *SerialService) Start(ctx context.Context) {

	// 启动主循环
	b := &backoff.Backoff{
		Min:    5 * time.Second,
		Max:    1 * time.Minute,
		Factor: 2,
		Jitter: true,
	}

	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx, b.Reset)
		if ctx.Err() != nil {
			return
		}

		// 连接失败或断开，使用 backoff 重试
		if err != nil {
			s.setConnected(false)
			s.resetPhysicalConnectionState()
			s.invalidateDeviceStatus(false)
			retryAfter := b.Duration()
			s.logger.Warn("串口连接异常，将重试",
				zap.Error(err),
				zap.Duration("retry_after", retryAfter))
			timer := time.NewTimer(retryAfter)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}
}

// setConnected 设置连接状态
func (s *SerialService) setConnected(connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
}

// setPortName 设置串口名称
func (s *SerialService) setPortName(portName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portName = portName
}

// getConnectionInfo 获取连接信息
func (s *SerialService) getConnectionInfo() (portName string, connected bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.portName, s.connected
}

// invalidateDeviceStatus 原子地撤销当前状态代际。acceptNew 为 true 时，后续
// 新回包可建立下一代状态；为 false 时（例如重启等待中）先拒绝所有状态。
func (s *SerialService) invalidateDeviceStatus(acceptNew bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.statusAccepting.Store(false)
	s.statusEpoch.Add(1)
	s.deviceCache.Delete(CacheKeyDeviceStatus)
	s.statusAccepting.Store(acceptNew)
}

func (s *SerialService) rememberScriptVersion(version string) string {
	version = strings.TrimSpace(version)
	s.scriptVersionMu.Lock()
	if version != "" {
		s.scriptVersion = version
	}
	remembered := s.scriptVersion
	s.scriptVersionMu.Unlock()
	return remembered
}

func (s *SerialService) clearScriptVersion() {
	s.scriptVersionMu.Lock()
	s.scriptVersion = ""
	s.scriptVersionMu.Unlock()
}

func (s *SerialService) setFlymodeOwner(simID string) {
	s.flymodeOwnerMu.Lock()
	s.flymodeOwnerSIMID = strings.TrimSpace(simID)
	s.flymodeOwnerMu.Unlock()
}

func (s *SerialService) FlymodeOwnerSIMID() string {
	s.flymodeOwnerMu.RLock()
	defer s.flymodeOwnerMu.RUnlock()
	return s.flymodeOwnerSIMID
}

// resetPhysicalConnectionState 撤销只对当前物理连接有效的控制状态。
// 同一个串口重连后可能已是另一台模块，不能继承旧飞行模式所有者或恢复定时器。
func (s *SerialService) resetPhysicalConnectionState() {
	s.flyMode.Store(false)
	s.autoFlymodeActive.Store(false)
	s.setFlymodeOwner("")
	s.clearScriptVersion()
	s.manualFlymodeGen.Add(1)
	s.recordSMSActivity()
}

// runOnce 执行一次连接尝试
func (s *SerialService) runOnce(ctx context.Context, resetBackoff func()) error {
	// 获取串口列表
	ports, err := serial.GetPortsList()
	if err != nil {
		return fmt.Errorf("获取串口列表失败: %w", err)
	}

	if len(ports) == 0 {
		return fmt.Errorf("未发现可用串口")
	}

	s.logger.Debug("发现可用串口", zap.Strings("ports", ports))

	// 确定使用的串口
	var selectedPort string
	if s.config.Port != "" {
		// 使用配置的串口
		selectedPort = s.config.Port
		s.logger.Info("使用配置的串口", zap.String("port", selectedPort))
	} else {
		// 自动检测
		s.logger.Info("开始自动检测串口...")
		selectedPort, err = s.autoDetectPort(ctx, ports)
		if err != nil {
			return fmt.Errorf("自动检测串口失败: %w", err)
		}
		s.logger.Info("自动检测到可用串口", zap.String("port", selectedPort))
	}

	// 自动发现的端点每次重连都重新验证项目握手，避免同一串口号后来
	// 被其他硬件占用时把命令发送给错误设备。静态旧配置保持兼容。
	if s.requireDiscoveryHandshake {
		if s.skipNextDiscoveryHandshake {
			s.skipNextDiscoveryHandshake = false
		} else {
			if _, err := probeAir780Port(ctx, selectedPort); err != nil {
				return fmt.Errorf("Air780 重连握手失败: %w", err)
			}
		}
	}

	// 连接串口
	if err := s.connectSerial(ctx, selectedPort); err != nil {
		return fmt.Errorf("连接串口失败: %w", err)
	}

	// 同名串口可能已经对应另一台模块；先撤销上一代缓存，再标记新连接在线。
	s.setConnected(false)
	s.resetPhysicalConnectionState()
	s.invalidateDeviceStatus(true)
	// 设置连接状态和串口名称
	s.setPortName(selectedPort)
	s.setConnected(true)

	// 重置 backoff（连接成功）
	resetBackoff()

	s.logger.Info("串口连接成功", zap.String("port", selectedPort))

	// 为本次连接创建独立的 context，用于管理连接的生命周期
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel() // 确保退出时取消 context
	closeOnCancelDone := make(chan struct{})
	go func() {
		select {
		case <-connCtx.Done():
			s.closeSerial()
		case <-closeOnCancelDone:
		}
	}()
	defer close(closeOnCancelDone)

	// 启动监听 goroutine
	s.wg.Add(1)
	go s.listenSerialData(connCtx, connCancel)

	// 启动定时更新缓存的 goroutine
	s.wg.Add(1)
	go s.periodicCacheUpdate(connCtx)

	// 新版 Lua 支持短信接收确认；旧版会忽略为 unknown command，仍可继续工作。
	if err := s.sendJSONCommand(map[string]string{"action": "enable_sms_ack"}); err != nil {
		s.logger.Warn("启用短信接收确认失败", zap.Error(err))
	}
	// 首次立即发送缓存更新请求
	go s.RequestCacheUpdate()

	// 等待连接断开
	s.wg.Wait()

	// 连接已断开，更新状态
	s.setConnected(false)
	s.resetPhysicalConnectionState()
	s.invalidateDeviceStatus(false)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("串口连接已断开")
}

// connectSerial 连接串口
func (s *SerialService) connectSerial(ctx context.Context, portName string) error {
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	}

	connectCtx, cancel := context.WithTimeout(ctx, serialConnectWindow)
	defer cancel()
	port, err := openSerialPortWithContext(connectCtx, portName, mode)
	if err != nil {
		return err
	}

	s.installSerialPort(port)
	return nil
}

// autoDetectPort 自动检测可用串口
func (s *SerialService) autoDetectPort(ctx context.Context, ports []string) (string, error) {
	for _, portName := range ports {
		s.logger.Debug("测试串口", zap.String("port", portName))
		if _, err := probeAir780Port(ctx, portName); err == nil {
			s.logger.Debug("检测到可用串口", zap.String("port", portName))
			return portName, nil
		} else {
			s.logger.Debug("串口未通过 Air780 握手", zap.String("port", portName), zap.Error(err))
		}
	}

	return "", fmt.Errorf("未检测到可用串口")
}

// listenSerialData 监听串口数据（在独立 goroutine 中运行）
func (s *SerialService) listenSerialData(connCtx context.Context, connCancel context.CancelFunc) {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("串口监听 goroutine panic", zap.Any("recover", r))
		}
		s.closeSerial()
		// 取消连接 context，通知其他 goroutine 连接已断开
		connCancel()
	}()

	port, _ := s.serialPortSnapshot()
	if port == nil {
		return
	}
	reader := bufio.NewReader(port)

	for {
		select {
		case <-connCtx.Done():
			s.logger.Info("串口监听停止")
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					// EOF 可能表示设备断开
					s.logger.Warn("串口读取 EOF，设备可能已断开")
					return
				}
				// 检查 context 是否已取消
				if connCtx.Err() != nil {
					return
				}
				// 其他错误，可能是设备断开或硬件错误
				s.logger.Error("读取串口数据错误，退出监听", zap.Error(err))
				return
			}

			s.processReceivedData(strings.TrimSpace(line))
		}
	}
}

// periodicCacheUpdate 定时更新缓存
func (s *SerialService) periodicCacheUpdate(connCtx context.Context) {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("定时更新缓存 goroutine panic", zap.Any("recover", r))
		}
	}()

	ticker := time.NewTicker(CacheRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-connCtx.Done():
			s.logger.Info("停止定时更新缓存")
			return
		case <-ticker.C:
			s.RequestCacheUpdate()
		}
	}
}

// RequestCacheUpdate 请求更新缓存（只发送命令，不等待响应）
func (s *SerialService) RequestCacheUpdate() {
	s.logger.Debug("发送缓存更新请求")

	// 发送获取设备状态命令（包含移动网络信息）
	if err := s.sendJSONCommand(map[string]string{"action": "get_status"}); err != nil {
		s.logger.Error("发送设备状态请求失败", zap.Error(err))
	}
}

// processReceivedData 处理接收到的数据
func (s *SerialService) processReceivedData(data string) {
	msg, err := parseSMSFrame(data)
	if err != nil {
		if errors.Is(err, errNotSMSFrame) {
			return
		}
		if errors.Is(err, errMissingType) {
			s.logger.Warn("消息类型缺失", zap.String("data", data))
			return
		}
		s.logger.Error("解析串口消息失败", zap.Error(err), zap.String("data", data))
		return
	}

	s.logger.Debug("收到串口消息", zap.String("type", msg.Type))
	s.routeMessage(msg)
}

// SendSMS 发送短信。expected 绑定目标 ICCID，避免换卡后通过错误 SIM 发送。
func (s *SerialService) SendSMS(expected SIMIdentity, to, content, requestID string) (string, error) {
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()

	s.smsOperationRunning.Store(true)
	defer s.smsOperationRunning.Store(false)
	if !s.FlyMode() {
		if err := s.ensureSIMIdentity(expected); err != nil {
			return "", err
		}
	}

	restoreManualFlymode, manualFlymodeGen, err := s.prepareNetworkForSMS(context.Background(), expected)
	if err != nil {
		return "", err
	}
	if err := s.ensureSIMIdentity(expected); err != nil {
		if restoreManualFlymode {
			s.restoreManualFlymode(s.newManualFlymodeRestoreToken(expected, manualFlymodeGen))
		}
		return "", err
	}
	routeValidator, routeWriteGuard := s.getSIMRouteCallbacks()
	if routeValidator != nil {
		if err := routeValidator(s.deviceID, expected); err != nil {
			if restoreManualFlymode {
				s.restoreManualFlymode(s.newManualFlymodeRestoreToken(expected, manualFlymodeGen))
			}
			return "", err
		}
	}

	// 先保存发送记录，状态为 "sending"
	ctx := context.Background()
	msgID := strings.TrimSpace(requestID)
	if msgID == "" {
		msgID = uuid.NewString()
	}
	msg := &models.TextMessage{
		ID:               msgID,
		DeviceID:         s.deviceID,
		DeviceName:       s.deviceName,
		SIMID:            expected.SIMID,
		ICCID:            expected.ICCID,
		IMSI:             expected.IMSI,
		IMEI:             expected.IMEI,
		MUID:             expected.MUID,
		SIMSlot:          expected.SIMSlot,
		IdentityRevision: expected.Revision,
		From:             "", // 发送方是本机
		To:               to,
		Content:          content,
		Type:             models.MessageTypeOutgoing,
		Status:           models.MessageStatusSending, // 初始状态为发送中
		CreatedAt:        time.Now().UnixMilli(),
	}

	if err := s.textMsgService.PrepareClaimedOutgoingRequest(ctx, msg); err != nil {
		s.logger.Error("补全短信发送记录失败", zap.Error(err))
		if restoreManualFlymode {
			s.restoreManualFlymode(s.newManualFlymodeRestoreToken(expected, manualFlymodeGen))
		}
		return "", err
	}
	if restoreManualFlymode {
		s.restoreFlymodeByMsg.Store(
			msgID,
			s.newManualFlymodeRestoreToken(expected, manualFlymodeGen),
		)
	}

	// 发送命令，使用消息 ID 作为 request_id
	cmd := map[string]any{
		"action":         "send_sms",
		"to":             to,
		"content":        content,
		"request_id":     msgID,
		"expected_iccid": expected.ICCID,
	}

	// 最终身份校验和 UART 写入共用 manager 的路由读租约；自动发现要修改
	// 拓扑时取得写租约，因此不能插入二者之间。超时仍必须先于写入登记，
	// 避免设备极快返回结果造成回执竞态。
	validateAndWrite := func() error {
		if routeValidator != nil {
			if err := routeValidator(s.deviceID, expected); err != nil {
				return err
			}
		}
		s.startSMSSendTimeout(msgID)
		return s.sendJSONCommand(cmd)
	}
	var writeErr error
	if routeWriteGuard != nil {
		writeErr = routeWriteGuard(validateAndWrite)
	} else {
		writeErr = validateAndWrite()
	}
	if writeErr != nil {
		err := writeErr
		if writeMayHaveReachedDevice(err) {
			// 串口驱动可能在已经写出部分甚至全部字节后同时返回错误。此时
			// 设备有机会完成发送，必须保留超时和计划任务登记，禁止按普通失败重试。
			updated, updateErr := s.textMsgService.UpdateSendResultById(
				ctx, msgID, models.MessageStatusAmbiguous, false, true, "serial_write_uncertain",
			)
			if updateErr != nil {
				s.logger.Error("记录串口不确定写入失败", zap.String("request_id", msgID), zap.Error(updateErr))
			}
			if updated {
				s.updateScheduledTaskStatus(ctx, msgID, models.LastRunStatusAmbiguous)
			} else if updateErr == nil {
				s.reconcileScheduledTaskStatus(ctx, msgID)
			}
			s.recordSMSActivity()
			return msgID, fmt.Errorf("%w: %v", ErrSMSSubmissionAmbiguous, err)
		}
		s.stopSMSSendTimeout(msgID)
		s.logger.Error("发送短信命令失败", zap.Error(err))
		_, _ = s.textMsgService.UpdateSendResultById(
			ctx, msgID, models.MessageStatusFailed, false, false, "serial_write_failed",
		)
		s.restoreFlymodeByMsg.Delete(msgID)
		if restoreManualFlymode {
			s.restoreManualFlymode(s.newManualFlymodeRestoreToken(expected, manualFlymodeGen))
		}
		return "", err
	}
	s.recordSMSActivity()

	s.logger.Info("发送短信命令成功", zap.String("to", to), zap.String("request_id", msgID))

	return msgID, nil
}

func (s *SerialService) ensureSIMIdentity(expected SIMIdentity) error {
	if expected.SIMID == "" || expected.ICCID == "" {
		return ErrSIMIdentityRequired
	}
	status, _ := s.GetStatus()
	actual := identityFromStatus(status)
	if !status.Connected || !status.Mobile.SimReady || !actual.Verified {
		return fmt.Errorf("%w: 期望 %s", ErrSIMOffline, expected.SIMID)
	}
	if actual.SIMID != expected.SIMID || actual.ICCID != expected.ICCID {
		return fmt.Errorf("%w: 期望 %s，实际 %s", ErrSIMMismatch, expected.SIMID, actual.SIMID)
	}
	if !supportsSIMIdentityProtocol(status.Version) {
		return fmt.Errorf("%w: 当前 %q，最低要求 %s", ErrSIMProtocolOutdated, status.Version, minimumSIMIdentityProtocolVersion)
	}
	return nil
}

// waitForExpectedSIMIdentity 用于退出飞行模式后的物理控制确认。只有重新读取到
// 目标 ICCID 才返回成功；若模块里已经是另一张卡，则立即失败关闭。
func (s *SerialService) waitForExpectedSIMIdentity(ctx context.Context, expected SIMIdentity) error {
	waitCtx, cancel := context.WithTimeout(ctx, cellularReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(cellularReadyPollInterval)
	defer ticker.Stop()

	for {
		err := s.ensureSIMIdentity(expected)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrSIMMismatch) || errors.Is(err, ErrSIMProtocolOutdated) {
			return err
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%w: 等待目标 %s 身份确认超时", ErrSIMOffline, expected.SIMID)
		case <-ticker.C:
			s.RequestCacheUpdate()
		}
	}
}

const (
	smsSendResultTimeout    = 2 * time.Minute
	smsSendResultRetryDelay = 5 * time.Second
)

func (s *SerialService) startSMSSendTimeout(msgID string) {
	s.scheduleSMSSendTimeout(msgID, smsSendResultTimeout)
}

func (s *SerialService) scheduleSMSSendTimeout(msgID string, delay time.Duration) {
	timer := time.AfterFunc(delay, func() {
		s.pendingSMSTimers.Delete(msgID)
		s.handleSMSSendTimeout(msgID)
	})
	s.pendingSMSTimers.Store(msgID, timer)
}

func (s *SerialService) handleSMSSendTimeout(msgID string) {
	ctx := context.Background()
	updated, err := s.textMsgService.MarkSendResultTimeout(ctx, msgID)
	if err != nil {
		s.logger.Error("短信发送结果超时，更新状态失败", zap.String("request_id", msgID), zap.Error(err))
		// 数据库瞬时故障不能让消息永久留在 sending。短延迟
		// 重试期间保留手动飞行模式恢复令牌，直到结果真正落库。
		s.scheduleSMSSendTimeout(msgID, smsSendResultRetryDelay)
		return
	}
	if updated {
		s.updateScheduledTaskStatus(ctx, msgID, models.LastRunStatusAmbiguous)
	} else if err == nil {
		// 超时回调可能晚于设备回执。若短信已是终态，利用这次
		// CAS 未命中的机会补齐上一次未成功持久化的任务结果。
		s.reconcileScheduledTaskStatus(ctx, msgID)
	}
	s.restoreManualFlymodeAfterResult(msgID)
	if updated {
		s.logger.Warn("等待短信发送结果超时", zap.String("request_id", msgID), zap.String("device_id", s.deviceID))
	}
}

func (s *SerialService) stopSMSSendTimeout(msgID string) {
	if value, ok := s.pendingSMSTimers.LoadAndDelete(msgID); ok {
		if timer, ok := value.(*time.Timer); ok {
			timer.Stop()
		}
	}
}

// GetStatus 获取设备状态（从缓存读取，包含 mobile 信息和串口连接状态）
func (s *SerialService) GetStatus() (*StatusData, error) {
	// 获取连接信息
	portName, connected := s.getConnectionInfo()

	// 从缓存读取
	if status, ok := s.deviceCache.Get(CacheKeyDeviceStatus); ok {
		if !s.statusAccepting.Load() || status.statusEpoch != s.statusEpoch.Load() {
			return &StatusData{
				DeviceID: s.deviceID, DeviceName: s.deviceName,
				PortName: portName, Connected: connected,
			}, nil
		}
		// 缓存中的状态只读，返回副本供本次请求补充连接信息。
		snapshot := *status
		// 更新串口连接信息
		snapshot.PortName = portName
		snapshot.Connected = connected
		snapshot.DeviceID = s.deviceID
		snapshot.DeviceName = s.deviceName

		// 更新飞行模式状态
		snapshot.Flymode = s.FlyMode()
		return &snapshot, nil
	}

	// 缓存未命中，但仍然返回连接状态
	status := &StatusData{
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
		PortName:   portName,
		Connected:  connected,
		Version:    s.rememberScriptVersion(""),
	}
	return status, nil
}

func (s *SerialService) FlyMode() bool {
	// 返回当前飞行模式状态
	return s.flyMode.Load()
}

// SetFlymode 设置飞行模式。启用时必须绑定当前 verified SIM，避免换卡竞态下
// 把另一张卡所在的模块置入飞行模式；退出后由上层再次验证目标 ICCID。
func (s *SerialService) SetFlymode(expected SIMIdentity, enabled bool) error {
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()

	if err := s.setFlymode(enabled, flymodeChangeManual, "", &expected); err != nil {
		return err
	}
	// 公开方法代表用户或其他业务手动设置，不再视为自动逻辑持有。
	s.autoFlymodeActive.Store(false)
	s.manualFlymodeGen.Add(1)
	if !enabled {
		s.recordSMSActivity()
	}
	return nil
}

func (s *SerialService) setFlymode(
	enabled bool,
	source flymodeChangeSource,
	reason string,
	expected *SIMIdentity,
) error {
	return s.withRouteWriteGuard(func() error {
		return s.setFlymodeWithRoute(enabled, source, reason, expected)
	})
}

func (s *SerialService) setFlymodeWithRoute(
	enabled bool,
	source flymodeChangeSource,
	reason string,
	expected *SIMIdentity,
) error {
	if enabled {
		if expected == nil || expected.SIMID == "" || expected.ICCID == "" {
			return ErrSIMIdentityRequired
		}
		wantedAutomatic := source == flymodeChangeAutomatic
		sameOwnerFlight := s.FlyMode() && s.FlymodeOwnerSIMID() == expected.SIMID
		if sameOwnerFlight &&
			s.autoFlymodeActive.Load() == wantedAutomatic {
			return nil
		}
		if s.hasPendingSMS() {
			return ErrSMSOperationPending
		}
		// 飞行模式中 ICCID 暂不可读；相同 owner 只是在 automatic/manual
		// 之间转移策略所有权，由 Lua 对已保存 owner 再核对，不重复切换基带。
		if !sameOwnerFlight {
			routeValidator, _ := s.getSIMRouteCallbacks()
			if routeValidator != nil {
				if err := routeValidator(s.deviceID, *expected); err != nil {
					return err
				}
			}
			if err := s.ensureSIMIdentity(*expected); err != nil {
				return err
			}
		}
	}
	if !enabled && expected != nil && s.FlyMode() {
		owner := s.FlymodeOwnerSIMID()
		if owner != "" && owner != expected.SIMID {
			return fmt.Errorf("%w: 期望 %s，飞行模式所有者 %s", ErrSIMMismatch, expected.SIMID, owner)
		}
	}
	cmd := map[string]any{
		"action":  "set_flymode",
		"enabled": enabled,
		"source":  source.protocolValue(),
	}
	if !enabled && source == flymodeChangeAutomatic && expected != nil &&
		s.FlyMode() && !s.autoFlymodeActive.Load() {
		// 从用户手动飞行模式临时唤醒发送短信。设备保留 owner 意图，
		// 即使主机在结果返回前重启，也能在重新握手后恢复。
		cmd["preserve_manual_owner"] = true
	}
	if expected != nil && expected.ICCID != "" {
		cmd["expected_iccid"] = expected.ICCID
	}
	if err := s.sendControlCommand(cmd); err != nil {
		// 设备明确表示短信仍在执行时，控制动作没有发生，当前身份代际仍然可信。
		// 保留它才能让手动飞行模式恢复逻辑稍后安全重试。
		if !errors.Is(err, ErrSMSOperationPending) {
			s.invalidateDeviceStatus(true)
			s.RequestCacheUpdate()
		}
		return err
	}
	// 更新飞行模式状态
	s.flyMode.Store(enabled)
	if enabled {
		s.setFlymodeOwner(expected.SIMID)
		s.autoFlymodeActive.Store(source == flymodeChangeAutomatic)
	} else {
		s.setFlymodeOwner("")
		s.autoFlymodeActive.Store(false)
	}
	s.notifyFlymodeChanged(source, enabled, reason)
	return nil
}

// RebootMcu 重启模块。按 SIM 调用时 expected 非空，并在主机和 Lua 两端复核；
// 按 deviceId 的显式物理维护操作可传 nil。
func (s *SerialService) RebootMcu(expected *SIMIdentity) error {
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()
	return s.withRouteWriteGuard(func() error {
		return s.rebootMcuWithRoute(expected)
	})
}

func (s *SerialService) rebootMcuWithRoute(expected *SIMIdentity) error {
	if s.hasPendingSMS() {
		return ErrSMSOperationPending
	}
	cmd := map[string]any{"action": "reboot_mcu"}
	if expected != nil {
		routeValidator, _ := s.getSIMRouteCallbacks()
		if routeValidator != nil {
			if err := routeValidator(s.deviceID, *expected); err != nil {
				return err
			}
		}
		if err := s.ensureSIMIdentity(*expected); err != nil {
			return err
		}
		cmd["expected_iccid"] = expected.ICCID
	}
	if err := s.sendControlCommand(cmd); err != nil {
		if !errors.Is(err, ErrSMSOperationPending) {
			s.invalidateDeviceStatus(true)
			s.RequestCacheUpdate()
		}
		return err
	}
	// 从命令写出起禁止复用重启前的身份，等待 system_ready 或新连接。
	s.invalidateDeviceStatus(false)
	s.resetPhysicalConnectionState()
	s.recordSMSActivity()
	return nil
}

func (s *SerialService) sendControlCommand(cmd map[string]any) error {
	requestID := uuid.NewString()
	cmd["request_id"] = requestID
	responseCh := make(chan controlCommandResponse, 1)
	s.pendingControls.Store(requestID, responseCh)
	defer s.pendingControls.Delete(requestID)

	if err := s.sendJSONCommand(cmd); err != nil {
		return err
	}
	timer := time.NewTimer(controlCommandResponseTimeout)
	defer timer.Stop()

	select {
	case response := <-responseCh:
		action, _ := cmd["action"].(string)
		if response.Action != action {
			return fmt.Errorf("%w: 响应动作 %q 与请求 %q 不一致", ErrControlCommandRejected, response.Action, action)
		}
		if response.Result == "ok" {
			return nil
		}
		switch response.Error {
		case "sim_identity_mismatch":
			return fmt.Errorf("%w: %s", ErrSIMMismatch, response.Error)
		case "sim_identity_unavailable":
			return fmt.Errorf("%w: %s", ErrSIMOffline, response.Error)
		case "expected_iccid_required":
			return fmt.Errorf("%w: %s", ErrSIMIdentityRequired, response.Error)
		case "sms_operation_pending":
			return fmt.Errorf("%w: %s", ErrSMSOperationPending, response.Error)
		default:
			return fmt.Errorf("%w: %s", ErrControlCommandRejected, response.Error)
		}
	case <-timer.C:
		return fmt.Errorf("%w: request_id=%s", ErrControlCommandTimeout, requestID)
	}
}

// sendJSONCommand 发送JSON命令到设备
func (s *SerialService) sendJSONCommand(cmd any) error {
	message, jsonData, err := buildCommandMessage(cmd)
	if err != nil {
		return err
	}
	if len(message) > MaxUARTCommandFrameBytes {
		return fmt.Errorf("串口命令帧过大: %d > %d", len(message), MaxUARTCommandFrameBytes)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	port, epoch := s.serialPortSnapshot()
	if port == nil {
		return fmt.Errorf("串口未连接")
	}

	resultCh := make(chan error, 1)
	go func() { resultCh <- writeAll(port, message) }()
	timer := time.NewTimer(s.writeWait)
	defer timer.Stop()
	select {
	case err = <-resultCh:
		if err != nil {
			s.closeSerialEpoch(epoch)
			return fmt.Errorf("串口写入失败: %w", err)
		}
	case <-timer.C:
		// Write 已交给驱动，超时无法证明设备没有收到数据，因此沿用
		// writeProgressError 的“不确定提交”语义。
		s.closeSerialEpoch(epoch)
		err = wrapWriteProgressError(0, context.DeadlineExceeded)
		return fmt.Errorf("串口写入超时: %w", err)
	}
	s.logger.Debug("串口命令已发送", zap.Int("payload_bytes", len(jsonData)))

	return nil
}

func (s *SerialService) installSerialPort(port serial.Port) {
	s.portMu.Lock()
	previous := s.port
	s.port = port
	s.portEpoch++
	s.portMu.Unlock()
	if previous != nil {
		s.closeDetachedSerialPort(previous)
	}
}

func (s *SerialService) serialPortSnapshot() (serial.Port, uint64) {
	s.portMu.RLock()
	defer s.portMu.RUnlock()
	return s.port, s.portEpoch
}

// closeSerial 先原子摘除句柄，再异步 Close。它不等待 writeMu，因此即使
// 驱动的 Write 卡住，取消连接仍能发起 Close 并让路由/发现流程继续。
func (s *SerialService) closeSerial() {
	s.portMu.Lock()
	port := s.port
	if port == nil {
		s.portMu.Unlock()
		return
	}
	s.port = nil
	s.portEpoch++
	s.portMu.Unlock()
	s.setConnected(false)
	s.closeDetachedSerialPort(port)
}

func (s *SerialService) closeSerialEpoch(epoch uint64) {
	s.portMu.Lock()
	if s.port == nil || s.portEpoch != epoch {
		s.portMu.Unlock()
		return
	}
	port := s.port
	s.port = nil
	s.portEpoch++
	s.portMu.Unlock()
	s.setConnected(false)
	s.closeDetachedSerialPort(port)
}

func (s *SerialService) closeDetachedSerialPort(port serial.Port) {
	go func() {
		if err := port.Close(); err != nil {
			s.logger.Debug("关闭串口失败", zap.Error(err))
		}
	}()
}

func writeAll(writer io.Writer, data []byte) error {
	written := 0
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return wrapWriteProgressError(written, io.ErrShortWrite)
		}
		written += n
		data = data[n:]
		if err != nil {
			return wrapWriteProgressError(written, err)
		}
		if n == 0 {
			return wrapWriteProgressError(written, io.ErrShortWrite)
		}
	}
	return nil
}

type writeProgressError struct {
	written int
	err     error
}

func (e *writeProgressError) Error() string { return e.err.Error() }
func (e *writeProgressError) Unwrap() error { return e.err }

func wrapWriteProgressError(written int, err error) error {
	return &writeProgressError{written: written, err: err}
}

func writeMayHaveReachedDevice(err error) bool {
	var progressErr *writeProgressError
	// 一旦调用底层 Write，驱动返回的 n/err 组合不足以证明设备没有收到
	// 数据，因此统一按不确定处理；只有尚未调用 Write 的错误才可确定失败。
	return errors.As(err, &progressErr)
}
