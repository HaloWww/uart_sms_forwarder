package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

const (
	autoFlymodeCheckInterval  = 10 * time.Second
	cellularReadyTimeout      = 45 * time.Second
	cellularReadyPollInterval = 2 * time.Second
	manualFlymodeRestoreDelay = 30 * time.Second
	manualFlymodeRetryDelay   = 5 * time.Second
)

type flymodeChangeSource string

const (
	flymodeChangeAutomatic flymodeChangeSource = "自动"
	flymodeChangeManual    flymodeChangeSource = "手动"
)

const (
	flymodeProtocolSourceAutomatic = "automatic"
	flymodeProtocolSourceManual    = "manual"
)

func (source flymodeChangeSource) protocolValue() string {
	if source == flymodeChangeAutomatic {
		return flymodeProtocolSourceAutomatic
	}
	return flymodeProtocolSourceManual
}

// StartAutoFlymodeMonitor 启动短信空闲监控。服务启动或配置刚启用时，
// 都会从当前时间开始计算完整的空闲周期。
func (s *SerialService) StartAutoFlymodeMonitor(ctx context.Context) {
	ticker := time.NewTicker(autoFlymodeCheckInterval)
	defer ticker.Stop()

	wasEnabled := false
	s.evaluateAutoFlymode(ctx, &wasEnabled)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("自动飞行模式监控已停止")
			return
		case <-ticker.C:
			s.evaluateAutoFlymode(ctx, &wasEnabled)
		}
	}
}

func (s *SerialService) evaluateAutoFlymode(ctx context.Context, wasEnabled *bool) {
	config, err := s.propertyService.GetAutoFlymodeConfig(ctx)
	if err != nil {
		s.logger.Error("读取自动飞行模式配置失败，本轮跳过", zap.Error(err))
		return
	}

	if !config.Enabled {
		// Lua 会持久保存飞行模式来源。即使主机是在设备已经自动进入飞行
		// 模式后才启动，也必须按当前可信状态执行“关闭自动飞行模式”。
		s.smsSendMu.Lock()
		if s.autoFlymodeActive.Load() && s.FlyMode() {
			if err := s.setFlymode(false, flymodeChangeAutomatic, "自动飞行模式配置已停用", nil); err != nil {
				s.smsSendMu.Unlock()
				s.logger.Error("关闭自动飞行模式后退出飞行模式失败", zap.Error(err))
				return
			}
			s.autoFlymodeActive.Store(false)
			s.recordSMSActivity()
			s.smsSendMu.Unlock()
			s.logger.Info("自动飞行模式已关闭，设备已退出自动开启的飞行模式")
		} else {
			s.smsSendMu.Unlock()
		}
		*wasEnabled = false
		return
	}

	if !*wasEnabled {
		*wasEnabled = true
		s.recordSMSActivity()
		s.logger.Info("自动飞行模式已启用",
			zap.Int64("idle_timeout_hours", config.IdleTimeoutHours))
		return
	}

	_, connected := s.getConnectionInfo()
	if !connected || s.FlyMode() || s.smsOperationRunning.Load() || s.hasPendingSMS() {
		return
	}

	lastActivity := time.UnixMilli(s.lastSMSActivityAt.Load())
	if !isAutoFlymodeDue(time.Now(), lastActivity, config.IdleTimeoutHours) {
		return
	}

	// 与短信提交和手动控制共用同一把锁，并在锁内重做所有判定，
	// 避免“刚检查为空闲，随后短信开始发送”的 check-then-act 竞态。
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()
	_, connected = s.getConnectionInfo()
	if !connected || s.FlyMode() || s.smsOperationRunning.Load() || s.hasPendingSMS() {
		return
	}
	lastActivity = time.UnixMilli(s.lastSMSActivityAt.Load())
	if !isAutoFlymodeDue(time.Now(), lastActivity, config.IdleTimeoutHours) {
		return
	}
	status, _ := s.GetStatus()
	identity := identityFromStatus(status)
	if err := s.ensureSIMIdentity(identity); err != nil {
		s.logger.Debug("SIM 身份尚未确认，跳过自动进入飞行模式", zap.Error(err))
		return
	}

	if err := s.setFlymode(
		true,
		flymodeChangeAutomatic,
		fmt.Sprintf("短信已空闲 %d 小时", config.IdleTimeoutHours),
		&identity,
	); err != nil {
		s.logger.Error("自动进入飞行模式失败", zap.Error(err))
		return
	}
	s.autoFlymodeActive.Store(true)
	s.logger.Info("短信空闲时间达到阈值，已自动进入飞行模式",
		zap.Int64("idle_timeout_hours", config.IdleTimeoutHours),
		zap.Time("last_sms_activity_at", lastActivity))
}

func isAutoFlymodeDue(now, lastActivity time.Time, idleTimeoutHours int64) bool {
	return !now.Before(lastActivity.Add(time.Duration(idleTimeoutHours) * time.Hour))
}

func (s *SerialService) recordSMSActivity() {
	s.lastSMSActivityAt.Store(time.Now().UnixMilli())
}

func (s *SerialService) notifyFlymodeChanged(source flymodeChangeSource, enabled bool, reason string) {
	if s.propertyService == nil || s.notifier == nil {
		return
	}

	stateLabel := "关闭"
	if enabled {
		stateLabel = "开启"
	}

	content := fmt.Sprintf("飞行模式已%s", stateLabel)
	if reason != "" {
		content += "\n原因: " + reason
	}
	deviceStatus, _ := s.GetStatus()
	identity := identityFromStatus(deviceStatus)

	go s.sendNotificationMessage(context.Background(), NotificationMessage{
		Type:       "flymode",
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
		SIMID:      identity.SIMID,
		ICCID:      identity.ICCID,
		IMSI:       identity.IMSI,
		IMEI:       identity.IMEI,
		From:       string(source),
		Content:    content,
		Timestamp:  time.Now().Unix(),
	})
}

// prepareNetworkForSMS 在自动或手动飞行模式下临时恢复蜂窝网络。
// 返回 true 表示原状态来自用户手动设置，短信完成后需要恢复飞行模式。
func (s *SerialService) prepareNetworkForSMS(ctx context.Context, expected SIMIdentity) (bool, uint64, error) {
	if !s.FlyMode() {
		return false, 0, nil
	}

	wasAutomatic := s.autoFlymodeActive.Load()
	manualFlymodeGen := s.manualFlymodeGen.Load()
	reason := "发送短信前临时恢复蜂窝网络"
	if wasAutomatic {
		reason += "（原飞行模式由自动策略开启）"
	} else {
		reason += "（原飞行模式由用户手动开启）"
	}
	if err := s.setFlymode(false, flymodeChangeAutomatic, reason, &expected); err != nil {
		return false, 0, fmt.Errorf("发送短信前退出飞行模式失败: %w", err)
	}
	s.autoFlymodeActive.Store(false)
	s.recordSMSActivity()

	s.logger.Info("发送短信前已退出飞行模式，等待移动网络注册",
		zap.Bool("was_automatic", wasAutomatic))
	if err := s.waitForCellularReady(ctx); err != nil {
		if !wasAutomatic {
			s.restoreManualFlymode(s.newManualFlymodeRestoreToken(expected, manualFlymodeGen))
		}
		return false, 0, err
	}

	return !wasAutomatic, manualFlymodeGen, nil
}

func (s *SerialService) waitForCellularReady(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, cellularReadyTimeout)
	defer cancel()

	// 丢弃飞行模式前的旧状态，确保等待的是退出飞行模式后的新回包。
	s.deviceCache.Delete(CacheKeyDeviceStatus)
	s.RequestCacheUpdate()

	ticker := time.NewTicker(cellularReadyPollInterval)
	defer ticker.Stop()

	for {
		if status, ok := s.deviceCache.Get(CacheKeyDeviceStatus); ok && status.Mobile.IsRegistered {
			s.logger.Info("移动网络注册成功，可以发送短信")
			return nil
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("退出飞行模式后等待移动网络注册超时")
		case <-ticker.C:
			s.RequestCacheUpdate()
		}
	}
}

type manualFlymodeRestoreToken struct {
	manualGeneration uint64
	statusEpoch      uint64
	expected         SIMIdentity
}

func (s *SerialService) newManualFlymodeRestoreToken(
	expected SIMIdentity,
	manualGeneration uint64,
) manualFlymodeRestoreToken {
	return manualFlymodeRestoreToken{
		manualGeneration: manualGeneration,
		statusEpoch:      s.statusEpoch.Load(),
		expected:         expected,
	}
}

func (s *SerialService) canRestoreManualFlymode(token manualFlymodeRestoreToken) error {
	if s.manualFlymodeGen.Load() != token.manualGeneration {
		return fmt.Errorf("手动飞行模式设置已经变化")
	}
	if s.statusEpoch.Load() != token.statusEpoch {
		return fmt.Errorf("SIM 或物理连接代际已经变化")
	}
	if err := s.ensureSIMIdentity(token.expected); err != nil {
		return err
	}
	if s.statusEpoch.Load() != token.statusEpoch {
		return fmt.Errorf("SIM 或物理连接在恢复前发生变化")
	}
	return nil
}

func (s *SerialService) hasPendingSMS() bool {
	pending := false
	s.pendingSMSTimers.Range(func(_, _ any) bool {
		pending = true
		return false
	})
	return pending
}

func (s *SerialService) restoreManualFlymode(token manualFlymodeRestoreToken) {
	if !s.manualRestoreActive.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.manualRestoreActive.Store(false)
		delay := manualFlymodeRestoreDelay
		for {
			time.Sleep(delay)
			retry, err := s.tryRestoreManualFlymode(token)
			if err != nil {
				s.logger.Info("SIM 或连接状态已变化，跳过旧飞行模式恢复", zap.Error(err))
				return
			}
			if retry {
				delay = manualFlymodeRetryDelay
				continue
			}
			s.autoFlymodeActive.Store(false)
			s.logger.Info("已恢复用户手动设置的飞行模式")
			return
		}
	}()
}

// tryRestoreManualFlymode 同步执行一次恢复尝试。retry=true 表示身份仍可信，
// 但还有短信等待设备最终结果，调用方应稍后重试。
func (s *SerialService) tryRestoreManualFlymode(token manualFlymodeRestoreToken) (retry bool, err error) {
	// 与新短信的接纳串行化：检查无在途短信到写出飞行模式命令之间，
	// 不允许另一条短信插入，避免恢复动作打断设备端 sendLong。
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()

	if err := s.canRestoreManualFlymode(token); err != nil {
		return false, err
	}
	if s.hasPendingSMS() {
		return true, nil
	}
	// 恢复窗口从最后一条短信活动重新计算，而不是只从第一条唤醒开始计时。
	if time.Since(time.UnixMilli(s.lastSMSActivityAt.Load())) < manualFlymodeRestoreDelay {
		return true, nil
	}
	if err := s.setFlymode(
		true,
		flymodeChangeManual,
		"短信发送完成后恢复用户设置",
		&token.expected,
	); err != nil {
		if errors.Is(err, ErrSMSOperationPending) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// recoverManualFlymodeIntent 从 Lua 状态恢复“为了发短信而临时退出手动飞行模式”
// 的意图。该字段属于当前模块会话，主机重启不会丢失；恢复前仍会再次核对 ICCID。
func (s *SerialService) recoverManualFlymodeIntent(status *StatusData) {
	if status == nil || status.Flymode || status.ManualRestoreOwnerICCID == "" ||
		!status.Connected || !status.Mobile.SimReady || !status.IdentityValid {
		return
	}
	expected := identityFromStatus(status)
	if expected.ICCID != status.ManualRestoreOwnerICCID ||
		expected.SIMID != makeSIMID(status.ManualRestoreOwnerICCID) {
		s.logger.Warn("忽略与当前 SIM 不一致的手动飞行模式恢复意图",
			zap.String("restore_iccid", status.ManualRestoreOwnerICCID),
			zap.String("current_iccid", expected.ICCID))
		return
	}
	s.restoreManualFlymode(
		s.newManualFlymodeRestoreToken(expected, s.manualFlymodeGen.Load()),
	)
}

func (s *SerialService) restoreManualFlymodeAfterResult(msgID string) {
	value, ok := s.restoreFlymodeByMsg.LoadAndDelete(msgID)
	if !ok {
		return
	}
	token, ok := value.(manualFlymodeRestoreToken)
	if !ok {
		s.logger.Error("恢复飞行模式状态无效", zap.String("request_id", msgID))
		return
	}
	s.restoreManualFlymode(token)
}
