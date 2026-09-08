package service

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
)

// IncomingCall 来电消息结构
type IncomingCall struct {
	Timestamp int64  `json:"timestamp"`
	From      string `json:"from"`
	Type      string `json:"type"`
}

// handleIncomingCall 处理来电通知
func (s *SerialService) handleIncomingCall(msg *ParsedMessage) {
	var call IncomingCall
	if err := json.Unmarshal([]byte(msg.JSON), &call); err != nil {
		s.logger.Error("来电消息解析失败", zap.Error(err))
		return
	}

	s.logger.Info("收到来电",
		zap.String("from", call.From),
		zap.Int64("timestamp", call.Timestamp))

	// 转换为通用通知消息并发送
	notifMsg := NotificationMessage{
		Type:       "call",
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
		From:       call.From,
		Content:    "", // 来电无内容
		Timestamp:  call.Timestamp,
	}

	go s.sendNotificationMessage(context.Background(), notifMsg)
}

// handleCallDisconnected 处理通话结束通知
func (s *SerialService) handleCallDisconnected(msg *ParsedMessage) {
	timestamp, _ := msg.Payload["timestamp"].(float64)

	s.logger.Info("通话已结束",
		zap.Int64("timestamp", int64(timestamp)))
}
