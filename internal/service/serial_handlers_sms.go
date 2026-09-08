package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// IncomingSMS 接收的短信消息结构
type IncomingSMS struct {
	MessageID        string `json:"message_id"`
	ICCID            string `json:"iccid"`
	IMSI             string `json:"imsi"`
	IMEI             string `json:"imei"`
	MUID             string `json:"muid"`
	SIMSlot          int    `json:"sim_slot"`
	IdentityValid    bool   `json:"identity_valid"`
	IdentityRevision uint64 `json:"identity_revision"`
	IdentityError    string `json:"identity_error"`
	Timestamp        int64  `json:"timestamp"`
	From             string `json:"from"`
	Content          string `json:"content"`
	Type             string `json:"type"`
}

func (r IncomingSMS) String() string {
	timestamp := time.Unix(r.Timestamp, 0)
	message := fmt.Sprintf(`%s
----
来自: %s
%s
`,
		r.Content,
		r.From,
		timestamp.Format(time.DateTime),
	)
	return message
}

// handleIncomingSMS 处理接收到的短信
func (s *SerialService) handleIncomingSMS(msg *ParsedMessage) {
	var sms IncomingSMS
	if err := json.Unmarshal([]byte(msg.JSON), &sms); err != nil {
		s.logger.Error("短信消息解析失败", zap.Error(err))
		return
	}

	s.logger.Info("收到新短信",
		zap.String("from", sms.From),
		zap.Int("content_length", len(sms.Content)),
		zap.Int64("timestamp", sms.Timestamp))
	s.recordSMSActivity()

	// 保存短信记录
	ctx := context.Background()
	if !sms.IdentityValid {
		// 未经设备端稳定性确认的 ICCID 不能用于业务归属；保留硬件身份供人工审计。
		sms.ICCID = ""
		sms.IMSI = ""
	}
	identity := SIMIdentity{
		ICCID:    normalizeIdentityValue(sms.ICCID),
		IMSI:     normalizeIdentityValue(sms.IMSI),
		IMEI:     normalizeIdentityValue(sms.IMEI),
		MUID:     normalizeIdentityValue(sms.MUID),
		SIMSlot:  sms.SIMSlot,
		Revision: sms.IdentityRevision,
		Verified: sms.IdentityValid,
	}
	identity.SIMID = makeSIMID(identity.ICCID)
	routeError := strings.TrimSpace(sms.IdentityError)
	identityConflict := false
	routeValidator, _ := s.getSIMRouteCallbacks()
	if identity.Verified && identity.SIMID != "" && routeValidator != nil {
		if err := routeValidator(s.deviceID, identity); err != nil {
			// 入站快照是在 SMS_INC 发生时冻结的历史事实。之后换卡或重连
			// 不能用“当前拓扑”抹掉其归属；只有当前明确存在重复 ICCID
			// 声明时增加冲突审计标记，仍按冻结 ICCID 归档。
			if errors.Is(err, ErrSIMConflict) {
				routeError = err.Error()
				identityConflict = true
				s.logger.Warn("入站短信的冻结 SIM 身份当前存在重复声明",
					zap.String("device_id", s.deviceID), zap.Error(err))
			}
		}
	}
	recordID := uuid.NewString()
	sourceID := recordID
	if sms.MessageID != "" {
		// 去重键只使用模块硬件身份和设备生成的 message_id，不能依赖会随
		// 换卡/拓扑判断变化的 SIMID，否则同一帧重放可能落成两条记录。
		dedupeScope := identity.MUID
		if dedupeScope == "" {
			dedupeScope = identity.IMEI
		}
		if dedupeScope == "" {
			dedupeScope = s.deviceID
		}
		recordID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(dedupeScope+":"+sms.MessageID)).String()
		sourceID = sms.MessageID
	}
	record := &models.TextMessage{
		ID:               recordID,
		DeviceID:         s.deviceID,
		DeviceName:       s.deviceName,
		SIMID:            identity.SIMID,
		ICCID:            identity.ICCID,
		IMSI:             identity.IMSI,
		IMEI:             identity.IMEI,
		MUID:             identity.MUID,
		SIMSlot:          identity.SIMSlot,
		IdentityRevision: identity.Revision,
		IdentityConflict: identityConflict,
		SendError:        routeError,
		SourceID:         sourceID,
		From:             sms.From,
		To:               "", // 接收方是本机
		Content:          sms.Content,
		Type:             models.MessageTypeIncoming,
		Status:           models.MessageStatusReceived,
		CreatedAt:        time.Now().UnixMilli(),
	}
	if sms.MessageID != "" {
		if _, err := s.textMsgService.Get(ctx, recordID); err == nil {
			s.ackIncomingSMS(sms.MessageID)
			return
		}
	}

	if err := s.textMsgService.Save(ctx, record); err != nil {
		s.logger.Error("保存短信记录失败", zap.Error(err))
		return
	}
	if sms.MessageID != "" {
		s.ackIncomingSMS(sms.MessageID)
	}

	// 异步发送通知
	go s.sendNotification(ctx, sms)
}

func (s *SerialService) ackIncomingSMS(messageID string) {
	if err := s.sendJSONCommand(map[string]any{"action": "ack_sms", "message_id": messageID}); err != nil {
		s.logger.Warn("发送短信接收确认失败", zap.String("message_id", messageID), zap.Error(err))
	}
}

// sendNotification 发送通知
func (s *SerialService) sendNotification(ctx context.Context, sms IncomingSMS) {
	// 转换为通用通知消息
	msg := NotificationMessage{
		Type:       "sms",
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
		SIMID:      makeSIMID(sms.ICCID),
		ICCID:      normalizeIdentityValue(sms.ICCID),
		IMSI:       normalizeIdentityValue(sms.IMSI),
		IMEI:       normalizeIdentityValue(sms.IMEI),
		From:       sms.From,
		Content:    sms.Content,
		Timestamp:  sms.Timestamp,
	}

	s.sendNotificationMessage(ctx, msg)
}

// sendNotificationMessage 发送通用通知消息
func (s *SerialService) sendNotificationMessage(ctx context.Context, msg NotificationMessage) {
	if s.propertyService == nil || s.notifier == nil {
		return
	}
	// 获取通知渠道配置
	channels, err := s.propertyService.GetNotificationChannelConfigs(ctx)
	if err != nil {
		s.logger.Error("获取通知渠道配置失败", zap.Error(err))
		return
	}

	// 格式化消息
	message := msg.String()

	// 发送到所有启用的渠道
	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}

		var sendErr error
		switch channel.Type {
		case "dingtalk":
			sendErr = s.notifier.SendDingTalkByConfig(ctx, channel.Config, message)
		case "wecom":
			sendErr = s.notifier.SendWeComByConfig(ctx, channel.Config, message)
		case "feishu":
			sendErr = s.notifier.SendFeishuByConfig(ctx, channel.Config, message)
		case "webhook":
			sendErr = s.notifier.SendWebhookByConfig(ctx, channel.Config, msg)
		case "email":
			sendErr = s.notifier.SendEmail(ctx, channel.Config, msg)
		case "telegram":
			sendErr = s.notifier.sendTelegramByConfig(ctx, channel.Config, message)
		}

		if sendErr != nil {
			s.logger.Error("发送通知失败",
				zap.String("type", channel.Type),
				zap.Error(sendErr))
		} else {
			s.logger.Info("通知发送成功", zap.String("type", channel.Type))
		}
	}
}

// handleSMSSendResult 处理短信发送结果
func (s *SerialService) handleSMSSendResult(msg *ParsedMessage) {
	success, _ := msg.Payload["success"].(bool)
	to, _ := msg.Payload["to"].(string)
	requestID, _ := msg.Payload["request_id"].(string)
	actualICCID, _ := msg.Payload["iccid"].(string)
	actualIMSI, _ := msg.Payload["imsi"].(string)
	actualIMEI, _ := msg.Payload["imei"].(string)
	identityValid, hasIdentityValid := msg.Payload["identity_valid"].(bool)
	submitted, hasSubmitted := msg.Payload["submitted"].(bool)
	ambiguous, hasAmbiguous := msg.Payload["ambiguous"].(bool)
	resultError, _ := msg.Payload["error"].(string)

	if requestID == "" {
		s.logger.Warn("收到短信发送结果但缺少 request_id", zap.Any("msg", msg.Payload))
		return
	}

	ctx := context.Background()
	actualICCID = normalizeIdentityValue(actualICCID)
	actualIMSI = normalizeIdentityValue(actualIMSI)
	actualIMEI = normalizeIdentityValue(actualIMEI)
	record, recordErr := s.textMsgService.Get(ctx, requestID)
	if recordErr == nil {
		to = record.To
	}
	// 1.3 以前的设备回执没有 submitted/identity_valid/ambiguous/ICCID，
	// 无法证明短信未提交或由目标卡发送。升级边界上只能保留“不确定”，绝不能
	// 把启动时已保护为 ambiguous 的记录降为 failed 后诱导人工或任务重试。
	completeProtocolEvidence := hasSubmitted && hasIdentityValid && hasAmbiguous
	if !completeProtocolEvidence {
		success = false
		ambiguous = true
		if recordErr == nil {
			submitted = record.Submitted
		}
		if resultError == "" {
			resultError = "legacy_or_incomplete_device_result"
		}
	}
	if success {
		if recordErr != nil {
			success = false
			if resultError == "" {
				resultError = "message_record_unavailable"
			}
			s.logger.Error("无法读取待发送记录，不能验证发送结果",
				zap.String("request_id", requestID), zap.Error(recordErr))
		} else if !submitted || !identityValid || record.ICCID == "" || actualICCID != record.ICCID ||
			record.SIMID != makeSIMID(actualICCID) {
			success = false
			if resultError == "" {
				resultError = "sim_identity_verification_failed"
			}
			s.logger.Error("发送结果 SIM 身份不一致，按失败处理",
				zap.String("request_id", requestID), zap.String("expected_iccid", record.ICCID),
				zap.String("actual_iccid", actualICCID))
		}
	}
	if ambiguous {
		success = false
		if resultError == "" {
			resultError = "device_reported_ambiguous_result"
		}
	}
	// 一旦设备已把短信提交给基带，却无法给出可信的最终成功回执，就不能
	// 当作普通失败自动重试，否则可能造成重复短信。
	ambiguous = ambiguous || (submitted && !success)
	var status models.MessageStatus
	var lastRunStatus models.LastRunStatus
	if success {
		status = models.MessageStatusSent
		lastRunStatus = models.LastRunStatusSuccess
	} else {
		if ambiguous {
			status = models.MessageStatusAmbiguous
			lastRunStatus = models.LastRunStatusAmbiguous
		} else {
			status = models.MessageStatusFailed
			lastRunStatus = models.LastRunStatusFailed
		}
	}

	updated, err := s.textMsgService.UpdateSendResultById(
		ctx, requestID, status, submitted, ambiguous, resultError,
	)
	if err != nil {
		s.logger.Error("更新短信发送结果失败",
			zap.String("request_id", requestID),
			zap.Error(err))
		// 设备通常只发送一次结果。持久化瞬时失败时重新保留一个保守超时，
		// 至少把 sending 收口为 ambiguous，不能删掉最后的恢复路径。
		s.stopSMSSendTimeout(requestID)
		s.startSMSSendTimeout(requestID)
		return
	}
	s.stopSMSSendTimeout(requestID)
	if !updated {
		// 短信结果和计划任务状态是两次独立持久化。上一次
		// 回执可能已经把短信推进到终态，却在更新任务时遇到
		// 瞬时错误。重复/迟到回执虽然不能再改写短信，仍应以
		// 已持久化的短信终态幂等补账。
		s.reconcileScheduledTaskStatus(ctx, requestID)
		s.logger.Debug("忽略重复或迟到的短信发送结果", zap.String("request_id", requestID))
		s.restoreManualFlymodeAfterResult(requestID)
		return
	}

	if success {
		s.recordSMSActivity()
		s.logger.Info("短信发送成功",
			zap.String("to", to),
			zap.String("request_id", requestID))
	} else {
		s.logger.Warn("短信发送未成功确认",
			zap.String("to", to),
			zap.String("request_id", requestID),
			zap.Bool("submitted", submitted),
			zap.Bool("ambiguous", ambiguous))
		failureIdentity := SIMIdentity{
			SIMID: makeSIMID(actualICCID), ICCID: actualICCID,
			IMSI: actualIMSI, IMEI: actualIMEI,
		}
		if recordErr == nil {
			failureIdentity = SIMIdentity{
				SIMID: record.SIMID, ICCID: record.ICCID,
				IMSI: record.IMSI, IMEI: record.IMEI,
			}
		}
		failureContent := fmt.Sprintf("短信发送失败: %s", to)
		if ambiguous {
			failureContent = fmt.Sprintf("短信发送状态不确定（可能已经发送，请勿立即重试）: %s", to)
		}
		if resultError != "" {
			failureContent += "\n原因: " + resultError
		}
		if recordErr == nil && actualICCID != "" && actualICCID != record.ICCID {
			failureContent += "\n设备返回 ICCID: " + actualICCID
		}
		go s.sendNotificationMessage(context.Background(), NotificationMessage{
			Type:       "sms",
			DeviceID:   s.deviceID,
			DeviceName: s.deviceName,
			SIMID:      failureIdentity.SIMID,
			ICCID:      failureIdentity.ICCID,
			IMSI:       failureIdentity.IMSI,
			IMEI:       failureIdentity.IMEI,
			From:       "UART 短信转发器",
			Content:    failureContent,
			Timestamp:  time.Now().Unix(),
		})
	}

	s.updateScheduledTaskStatus(ctx, requestID, lastRunStatus)
	s.restoreManualFlymodeAfterResult(requestID)
}

func (s *SerialService) updateScheduledTaskStatus(ctx context.Context, msgID string, status models.LastRunStatus) {
	updater := s.getScheduledTaskStatusUpdater()
	if updater == nil {
		return
	}
	if err := updater(ctx, msgID, status); err != nil {
		s.logger.Error("更新定时任务状态失败",
			zap.String("request_id", msgID),
			zap.Error(err))
		s.scheduleScheduledTaskStatusRetry(msgID)
	} else {
		s.stopScheduledTaskStatusRetry(msgID)
	}
}

const (
	defaultScheduledTaskRetryInitial = time.Second
	defaultScheduledTaskRetryMax     = time.Minute
)

func (s *SerialService) scheduledTaskRetryDelays() (time.Duration, time.Duration) {
	initial := s.scheduledTaskRetryInitial
	if initial <= 0 {
		initial = defaultScheduledTaskRetryInitial
	}
	maximum := s.scheduledTaskRetryMax
	if maximum <= 0 {
		maximum = defaultScheduledTaskRetryMax
	}
	if maximum < initial {
		maximum = initial
	}
	return initial, maximum
}

type scheduledTaskStatusRetry struct {
	stop chan struct{}
}

// scheduleScheduledTaskStatusRetry 为同一 messageId 去重地启动后台对账。
// 短信终态已经持久化，所以每次都重新读取数据库；旧消息即使迟到，
// ScheduledTaskRepo 的 last_msg_id CAS 也不会覆盖任务的新一轮执行。
func (s *SerialService) scheduleScheduledTaskStatusRetry(msgID string) {
	if s.getScheduledTaskStatusUpdater() == nil || s.textMsgService == nil || msgID == "" {
		return
	}
	retry := &scheduledTaskStatusRetry{stop: make(chan struct{})}
	if _, loaded := s.scheduledTaskStatusRetries.LoadOrStore(msgID, retry); loaded {
		return
	}

	go func(current *scheduledTaskStatusRetry) {
		defer s.scheduledTaskStatusRetries.CompareAndDelete(msgID, current)
		delay, maximum := s.scheduledTaskRetryDelays()
		for {
			timer := time.NewTimer(delay)
			select {
			case <-current.stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
			if err := s.reconcileScheduledTaskStatusOnce(context.Background(), msgID); err == nil {
				return
			} else {
				s.logger.Error("重试计划任务状态对账失败",
					zap.String("request_id", msgID), zap.Duration("retry_after", delay), zap.Error(err))
			}
			if delay < maximum {
				delay *= 2
				if delay > maximum {
					delay = maximum
				}
			}
		}
	}(retry)
}

// stopScheduledTaskStatusRetry 让已经由重复回执等同步路径补账成功的消息
// 立即停止等待中的后台重试。先从 Map 移除再关闭 channel，避免旧 goroutine
// 退出时误删同一 messageId 后来建立的新重试。
func (s *SerialService) stopScheduledTaskStatusRetry(msgID string) {
	value, ok := s.scheduledTaskStatusRetries.LoadAndDelete(msgID)
	if !ok {
		return
	}
	close(value.(*scheduledTaskStatusRetry).stop)
}

// reconcileScheduledTaskStatus 不信任当前回执里的结果，而是以数据库中
// 已经收口的短信状态为准补齐计划任务。
func (s *SerialService) reconcileScheduledTaskStatus(ctx context.Context, msgID string) {
	if err := s.reconcileScheduledTaskStatusOnce(ctx, msgID); err != nil {
		s.logger.Error("读取短信终态补齐计划任务失败",
			zap.String("request_id", msgID), zap.Error(err))
		s.scheduleScheduledTaskStatusRetry(msgID)
	}
}

func (s *SerialService) reconcileScheduledTaskStatusOnce(ctx context.Context, msgID string) error {
	updater := s.getScheduledTaskStatusUpdater()
	if updater == nil || s.textMsgService == nil {
		return nil
	}
	record, err := s.textMsgService.Get(ctx, msgID)
	if err != nil {
		// 用户删除仍未决的 task 后，终态短信即可被正常删除。此时已经没有
		// 可补账对象，后台循环必须收口，不能永远重试一个不存在的证据。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	var status models.LastRunStatus
	switch record.Status {
	case models.MessageStatusSent:
		status = models.LastRunStatusSuccess
	case models.MessageStatusFailed:
		status = models.LastRunStatusFailed
	case models.MessageStatusAmbiguous:
		status = models.LastRunStatusAmbiguous
	default:
		// 正常调用只会发生在短信已经持久化终态之后。若数据库暂时仍能
		// 看到中间态，继续退避等待，不能把它误当作对账成功。
		return fmt.Errorf("短信 %s 尚未进入可对账终态: %s", msgID, record.Status)
	}
	if err := updater(ctx, msgID, status); err != nil {
		return err
	}
	s.stopScheduledTaskStatusRetry(msgID)
	return nil
}
