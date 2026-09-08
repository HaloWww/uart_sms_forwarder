package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrSMSRequestConflict = errors.New("同一 requestId 已用于另一条短信")

const unresolvedScheduledEvidenceSQL = `EXISTS (
	SELECT 1 FROM scheduled_tasks
	WHERE scheduled_tasks.last_msg_id = text_messages.id
	AND scheduled_tasks.last_run_status = ?
)`

func unresolvedMessageEvidenceWhere() string {
	return `(type = ? AND status IN ?) OR ` + unresolvedScheduledEvidenceSQL
}

func excludeUnresolvedMessageEvidenceWhere() string {
	return `NOT (` + unresolvedMessageEvidenceWhere() + `)`
}

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
	SIMID        string              `json:"simId"`
	DeviceID     string              `json:"deviceId,omitempty"` // 旧记录的物理连接标识
	Peer         string              `json:"peer"`               // 对方号码
	LastMessage  *models.TextMessage `json:"lastMessage"`        // 最后一条消息
	MessageCount int64               `json:"messageCount"`       // 消息总数
	UnreadCount  int64               `json:"unreadCount"`        // 未读数量（暂时为0）
}

// MessageScope 只允许按稳定的 SIM 身份筛选，DeviceID 仅保留在记录中用于审计。
type MessageScope struct {
	SIMID string
}

func applyMessageScope(db *gorm.DB, scope MessageScope) (*gorm.DB, error) {
	simID := strings.TrimSpace(scope.SIMID)
	if simID == "" {
		return nil, ErrSIMIdentityRequired
	}
	if simID == UnassignedSIMID {
		return db.Where("sim_id = '' OR sim_id IS NULL"), nil
	}
	return db.Where("sim_id = ?", simID), nil
}

// Save 保存短信记录
func (s *TextMessageService) Save(ctx context.Context, msg *models.TextMessage) error {
	if err := s.repo.Save(ctx, msg); err != nil {
		s.logger.Error("保存短信记录失败", zap.Error(err), zap.String("id", msg.ID))
		return fmt.Errorf("保存短信记录失败: %w", err)
	}
	return nil
}

// ClaimOutgoingRequest 以短信 ID 作为幂等键原子占位。只有 claimed=true
// 的调用方可以继续操作串口；相同请求的重试直接返回原记录。
func (s *TextMessageService) ClaimOutgoingRequest(
	ctx context.Context,
	request *models.TextMessage,
) (existing *models.TextMessage, claimed bool, err error) {
	result := s.repo.GetDB(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(request)
	if result.Error != nil {
		return nil, false, fmt.Errorf("占用短信 requestId 失败: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return request, true, nil
	}

	stored, findErr := s.repo.FindById(ctx, request.ID)
	if findErr != nil {
		return nil, false, fmt.Errorf("读取已有短信请求失败: %w", findErr)
	}
	if stored.Type != models.MessageTypeOutgoing ||
		stored.ScheduledTaskID != request.ScheduledTaskID ||
		stored.SIMID != request.SIMID ||
		stored.To != request.To ||
		stored.Content != request.Content {
		return &stored, false, fmt.Errorf("%w: %s", ErrSMSRequestConflict, request.ID)
	}
	return &stored, false, nil
}

// PrepareClaimedOutgoingRequest 只补全已由 ClaimOutgoingRequest 占位的记录。
// 不使用 upsert，避免任何未获得首次执行权的路径意外覆盖原请求。
func (s *TextMessageService) PrepareClaimedOutgoingRequest(
	ctx context.Context,
	msg *models.TextMessage,
) error {
	result := s.repo.GetDB(ctx).Model(&models.TextMessage{}).
		Where("id = ? AND sim_id = ? AND type = ?", msg.ID, msg.SIMID, models.MessageTypeOutgoing).
		Where(map[string]any{"to": msg.To, "content": msg.Content}).
		Updates(map[string]any{
			"device_id": msg.DeviceID, "device_name": msg.DeviceName,
			"iccid": msg.ICCID, "imsi": msg.IMSI, "imei": msg.IMEI, "m_uid": msg.MUID,
			"sim_slot": msg.SIMSlot, "identity_revision": msg.IdentityRevision,
		})
	if result.Error != nil {
		return fmt.Errorf("补全短信请求信息失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%w: %s", ErrSMSRequestConflict, msg.ID)
	}
	return nil
}

// Get 获取单条短信记录
func (s *TextMessageService) Get(ctx context.Context, id string) (*models.TextMessage, error) {
	msg, err := s.repo.FindById(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("短信记录不存在: %w", gorm.ErrRecordNotFound)
		}
		s.logger.Error("获取短信记录失败", zap.Error(err), zap.String("id", id))
		return nil, fmt.Errorf("获取短信记录失败: %w", err)
	}
	return &msg, nil
}

// Delete 删除单条短信记录
func (s *TextMessageService) Delete(ctx context.Context, id string, scope MessageScope) error {
	db, err := applyMessageScope(s.repo.GetDB(ctx), scope)
	if err != nil {
		return err
	}
	pendingStatuses := []models.MessageStatus{
		models.MessageStatusSending, models.MessageStatusAmbiguous,
	}
	result := db.Where("id = ?", id).
		Where(excludeUnresolvedMessageEvidenceWhere(),
			models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
		Delete(&models.TextMessage{})
	if result.Error != nil {
		s.logger.Error("删除短信记录失败", zap.Error(result.Error), zap.String("id", id))
		return fmt.Errorf("删除短信记录失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		pendingDB, err := applyMessageScope(s.repo.GetDB(ctx), scope)
		if err != nil {
			return err
		}
		var pending int64
		if err := pendingDB.Model(&models.TextMessage{}).Where("id = ?", id).
			Where(unresolvedMessageEvidenceWhere(),
				models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return ErrSMSOperationPending
		}
		return gorm.ErrRecordNotFound
	}
	s.logger.Info("删除短信记录成功", zap.String("id", id))
	return nil
}

// Clear 清空所有短信记录
func (s *TextMessageService) Clear(ctx context.Context, scope MessageScope) error {
	pendingStatuses := []models.MessageStatus{
		models.MessageStatusSending, models.MessageStatusAmbiguous,
	}
	err := s.repo.GetDB(ctx).Transaction(func(tx *gorm.DB) error {
		countQuery, err := applyMessageScope(tx.Session(&gorm.Session{}), scope)
		if err != nil {
			return err
		}
		var pending int64
		if err := countQuery.Model(&models.TextMessage{}).
			Where(unresolvedMessageEvidenceWhere(),
				models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return ErrSMSOperationPending
		}
		// 即使在检查后出现并发发送，请求态记录也由 DELETE 条件保护。
		deleteQuery, err := applyMessageScope(tx.Session(&gorm.Session{}), scope)
		if err != nil {
			return err
		}
		return deleteQuery.Where(excludeUnresolvedMessageEvidenceWhere(),
			models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
			Delete(&models.TextMessage{}).Error
	})
	if err != nil {
		if errors.Is(err, ErrSMSOperationPending) {
			return err
		}
		s.logger.Error("清空短信记录失败", zap.Error(err))
		return fmt.Errorf("清空短信记录失败: %w", err)
	}
	s.logger.Info("清空短信记录成功")
	return nil
}

// GetStats 获取统计信息
func (s *TextMessageService) GetStats(ctx context.Context, scope MessageScope) (*Stats, error) {
	db, err := applyMessageScope(s.repo.GetDB(ctx), scope)
	if err != nil {
		return nil, err
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

// UpdateSendResultById 只允许 sending/ambiguous 推进到新结果。sent/failed 是
// 终态，重复回执或串口写入竞态不能再把它覆盖。
func (s *TextMessageService) UpdateSendResultById(
	ctx context.Context,
	id string,
	status models.MessageStatus,
	submitted bool,
	ambiguous bool,
	sendError string,
) (bool, error) {
	result := s.repo.GetDB(ctx).Model(&models.TextMessage{}).
		Where("id = ? AND status IN ?", id, []models.MessageStatus{
			models.MessageStatusSending, models.MessageStatusAmbiguous,
		}).Updates(map[string]any{
		"status": status, "submitted": submitted, "ambiguous": ambiguous, "send_error": sendError,
	})
	if result.Error != nil {
		return false, fmt.Errorf("更新短信发送结果失败: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// MarkSendResultTimeout 只允许把仍在 sending 的记录推进到 ambiguous。
// 如果设备回执已经先到，条件更新不会用超时结果覆盖最终 sent/failed 状态。
func (s *TextMessageService) MarkSendResultTimeout(ctx context.Context, id string) (bool, error) {
	result := s.repo.GetDB(ctx).Model(&models.TextMessage{}).
		Where("id = ? AND status = ?", id, models.MessageStatusSending).
		Updates(map[string]any{
			"status": models.MessageStatusAmbiguous, "ambiguous": true,
			"send_error": "result_timeout",
		})
	if result.Error != nil {
		return false, fmt.Errorf("更新短信发送超时失败: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// GetConversations 获取会话列表（按对方号码分组）
func (s *TextMessageService) GetConversations(ctx context.Context, scope MessageScope) ([]*Conversation, error) {
	db, err := applyMessageScope(s.repo.GetDB(ctx), scope)
	if err != nil {
		return nil, err
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
		identityKey := msg.SIMID
		if identityKey == "" {
			identityKey = UnassignedSIMID
			// API 使用显式占位身份，数据库仍保留空值表示无法可靠迁移。
			msg.SIMID = UnassignedSIMID
		}
		key := identityKey + "\x00" + peer
		if _, exists := conversationMap[key]; !exists {
			conversationMap[key] = &Conversation{
				SIMID:        identityKey,
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
func (s *TextMessageService) GetConversationMessages(ctx context.Context, scope MessageScope, peer string) ([]models.TextMessage, error) {
	db, err := applyMessageScope(s.repo.GetDB(ctx), scope)
	if err != nil {
		return nil, err
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
	if scope.SIMID == UnassignedSIMID {
		for i := range messages {
			messages[i].SIMID = UnassignedSIMID
		}
	}

	return messages, nil
}

// DeleteConversation 删除整个会话（与某个联系人的所有消息）
func (s *TextMessageService) DeleteConversation(ctx context.Context, scope MessageScope, peer string) error {
	pendingStatuses := []models.MessageStatus{
		models.MessageStatusSending, models.MessageStatusAmbiguous,
	}
	var deletedCount int64
	err := s.repo.GetDB(ctx).Transaction(func(tx *gorm.DB) error {
		countQuery, err := applyMessageScope(tx.Session(&gorm.Session{}), scope)
		if err != nil {
			return err
		}
		conversationWhere := "(type = ? AND \"from\" = ?) OR (type = ? AND \"to\" = ?)"
		var pending int64
		if err := countQuery.Where(
			conversationWhere,
			models.MessageTypeIncoming, peer, models.MessageTypeOutgoing, peer,
		).
			Model(&models.TextMessage{}).
			Where(unresolvedMessageEvidenceWhere(),
				models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return ErrSMSOperationPending
		}
		deleteQuery, err := applyMessageScope(tx.Session(&gorm.Session{}), scope)
		if err != nil {
			return err
		}
		result := deleteQuery.Where(
			conversationWhere,
			models.MessageTypeIncoming, peer, models.MessageTypeOutgoing, peer,
		).Where(excludeUnresolvedMessageEvidenceWhere(),
			models.MessageTypeOutgoing, pendingStatuses, models.LastRunStatusAmbiguous).
			Delete(&models.TextMessage{})
		deletedCount = result.RowsAffected
		return result.Error
	})
	if err != nil {
		if errors.Is(err, ErrSMSOperationPending) {
			return err
		}
		s.logger.Error("删除会话失败", zap.Error(err), zap.String("peer", peer))
		return fmt.Errorf("删除会话失败: %w", err)
	}

	s.logger.Info("删除会话成功", zap.String("peer", peer), zap.Int64("deleted_count", deletedCount))
	return nil
}
