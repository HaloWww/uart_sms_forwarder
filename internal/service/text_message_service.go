package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// TextMessageService 短信服务
type TextMessageService struct {
	repo   *repo.TextMessageRepo
	logger *zap.Logger
}

// NewTextMessageService 创建短信服务实例
func NewTextMessageService(logger *zap.Logger, repo *repo.TextMessageRepo) *TextMessageService {
	return &TextMessageService{
		repo:   repo,
		logger: logger,
	}
}

// Stats 统计信息
type Stats struct {
	TotalCount    int64 `json:"totalCount"`
	IncomingCount int64 `json:"incomingCount"`
	OutgoingCount int64 `json:"outgoingCount"`
	TodayCount    int64 `json:"todayCount"`
}

// Conversation 会话信息
type Conversation struct {
	DeviceID     string              `json:"deviceId"`
	Peer         string              `json:"peer"`         // 对方号码
	LastMessage  *models.TextMessage `json:"lastMessage"`  // 最后一条消息
	MessageCount int64               `json:"messageCount"` // 消息总数
	UnreadCount  int64               `json:"unreadCount"`  // 未读数量（暂时为0）
}

// Save 保存短信记录
func (s *TextMessageService) Save(ctx context.Context, msg *models.TextMessage) error {
	if err := s.repo.Save(ctx, msg); err != nil {
		s.logger.Error("保存短信记录失败", zap.Error(err), zap.String("id", msg.ID))
		return fmt.Errorf("保存短信记录失败: %w", err)
	}
	return nil
}

// Get 获取单条短信记录
func (s *TextMessageService) Get(ctx context.Context, id string) (*models.TextMessage, error) {
	msg, err := s.repo.FindById(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("短信记录不存在")
		}
		s.logger.Error("获取短信记录失败", zap.Error(err), zap.String("id", id))
		return nil, fmt.Errorf("获取短信记录失败: %w", err)
	}
	return &msg, nil
}

// Delete 删除单条短信记录
func (s *TextMessageService) Delete(ctx context.Context, id string) error {
	if err := s.repo.DeleteById(ctx, id); err != nil {
		s.logger.Error("删除短信记录失败", zap.Error(err), zap.String("id", id))
		return fmt.Errorf("删除短信记录失败: %w", err)
	}
	s.logger.Info("删除短信记录成功", zap.String("id", id))
	return nil
}

// Clear 清空所有短信记录
func (s *TextMessageService) Clear(ctx context.Context, deviceID string) error {
	db := s.repo.GetDB(ctx)
	query := db.Where("1 = 1")
	if deviceID != "" {
		query = query.Where("device_id = ?", deviceID)
	}
	if err := query.Delete(&models.TextMessage{}).Error; err != nil {
		s.logger.Error("清空短信记录失败", zap.Error(err))
		return fmt.Errorf("清空短信记录失败: %w", err)
	}
	s.logger.Info("清空短信记录成功")
	return nil
}

// GetStats 获取统计信息
func (s *TextMessageService) GetStats(ctx context.Context, deviceID string) (*Stats, error) {
	db := s.repo.GetDB(ctx)
	if deviceID != "" {
		db = db.Where("device_id = ?", deviceID)
	}

	stats := &Stats{}

	// 总数
	if err := db.Model(&models.TextMessage{}).Count(&stats.TotalCount).Error; err != nil {
		return nil, fmt.Errorf("统计总数失败: %w", err)
	}

	// 接收数量
	if err := db.Model(&models.TextMessage{}).Where("type = ?", "incoming").Count(&stats.IncomingCount).Error; err != nil {
		return nil, fmt.Errorf("统计接收数量失败: %w", err)
	}

	// 发送数量
	if err := db.Model(&models.TextMessage{}).Where("type = ?", "outgoing").Count(&stats.OutgoingCount).Error; err != nil {
		return nil, fmt.Errorf("统计发送数量失败: %w", err)
	}

	// 今日数量（按 created_at 字段）
	todayStart := time.Now().Truncate(24 * time.Hour).UnixMilli()
	if err := db.Model(&models.TextMessage{}).Where("created_at >= ?", todayStart).Count(&stats.TodayCount).Error; err != nil {
		return nil, fmt.Errorf("统计今日数量失败: %w", err)
	}

	return stats, nil
}

func (s *TextMessageService) UpdateStatusById(ctx context.Context, id string, status models.MessageStatus) error {
	return s.repo.UpdateColumnsById(ctx, id, map[string]interface{}{
		"status": status,
	})
}

// GetConversations 获取会话列表（按对方号码分组）
func (s *TextMessageService) GetConversations(ctx context.Context, deviceID string) ([]*Conversation, error) {
	db := s.repo.GetDB(ctx)
	if deviceID != "" {
		db = db.Where("device_id = ?", deviceID)
	}

	// 获取所有短信记录，按创建时间倒序
	var messages []models.TextMessage
	if err := db.Order("created_at DESC").Find(&messages).Error; err != nil {
		s.logger.Error("获取短信记录失败", zap.Error(err))
		return nil, fmt.Errorf("获取短信记录失败: %w", err)
	}

	// 按对方号码分组
	conversationMap := make(map[string]*Conversation)
	for i := range messages {
		msg := &messages[i]

		// 确定对方号码
		var peer string
		if msg.Type == models.MessageTypeIncoming {
			peer = msg.From
		} else {
			peer = msg.To
		}

		if peer == "" {
			continue
		}

		// 如果会话不存在，创建新会话
		key := msg.DeviceID + "\x00" + peer
		if _, exists := conversationMap[key]; !exists {
			conversationMap[key] = &Conversation{
				DeviceID:     msg.DeviceID,
				Peer:         peer,
				LastMessage:  msg,
				MessageCount: 0,
				UnreadCount:  0,
			}
		}

		// 更新消息数量
		conversationMap[key].MessageCount++

		// 更新最后一条消息（取最新的）
		if msg.CreatedAt > conversationMap[key].LastMessage.CreatedAt {
			conversationMap[key].LastMessage = msg
		}
	}

	// 转换为切片并按最后消息时间排序
	conversations := make([]*Conversation, 0, len(conversationMap))
	for _, conv := range conversationMap {
		conversations = append(conversations, conv)
	}

	// 按最后消息时间倒序排序
	for i := 0; i < len(conversations)-1; i++ {
		for j := i + 1; j < len(conversations); j++ {
			if conversations[i].LastMessage.CreatedAt < conversations[j].LastMessage.CreatedAt {
				conversations[i], conversations[j] = conversations[j], conversations[i]
			}
		}
	}

	return conversations, nil
}

// GetConversationMessages 获取指定会话的所有消息
func (s *TextMessageService) GetConversationMessages(ctx context.Context, deviceID, peer string) ([]models.TextMessage, error) {
	db := s.repo.GetDB(ctx)
	if deviceID != "" {
		db = db.Where("device_id = ?", deviceID)
	}

	var messages []models.TextMessage

	// 查询条件：(type=incoming AND from=peer) OR (type=outgoing AND to=peer)
	if err := db.Where("(type = ? AND \"from\" = ?) OR (type = ? AND \"to\" = ?)",
		models.MessageTypeIncoming, peer,
		models.MessageTypeOutgoing, peer,
	).Order("created_at ASC").Find(&messages).Error; err != nil {
		s.logger.Error("获取会话消息失败", zap.Error(err), zap.String("peer", peer))
		return nil, fmt.Errorf("获取会话消息失败: %w", err)
	}

	return messages, nil
}

// DeleteConversation 删除整个会话（与某个联系人的所有消息）
func (s *TextMessageService) DeleteConversation(ctx context.Context, deviceID, peer string) error {
	db := s.repo.GetDB(ctx)
	if deviceID != "" {
		db = db.Where("device_id = ?", deviceID)
	}

	// 删除条件：(type=incoming AND from=peer) OR (type=outgoing AND to=peer)
	result := db.Where("(type = ? AND \"from\" = ?) OR (type = ? AND \"to\" = ?)",
		models.MessageTypeIncoming, peer,
		models.MessageTypeOutgoing, peer,
	).Delete(&models.TextMessage{})

	if result.Error != nil {
		s.logger.Error("删除会话失败", zap.Error(result.Error), zap.String("peer", peer))
		return fmt.Errorf("删除会话失败: %w", result.Error)
	}

	s.logger.Info("删除会话成功", zap.String("peer", peer), zap.Int64("deleted_count", result.RowsAffected))
	return nil
}
