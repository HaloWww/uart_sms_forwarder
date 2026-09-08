package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/go-orz/orz"
	"gorm.io/gorm"
)

var ErrScheduledTaskExecutionPending = errors.New("计划任务已有未决执行")

const scheduledTaskClaimRecoveryGrace = 5 * time.Minute

type ScheduledTaskRepo struct {
	orz.Repository[models.ScheduledTask, string]
	db *gorm.DB
}

func NewScheduledTaskRepo(db *gorm.DB) *ScheduledTaskRepo {
	return &ScheduledTaskRepo{
		Repository: orz.NewRepository[models.ScheduledTask, string](db),
		db:         db,
	}
}

// FindAllEnabled 查询所有启用的任务
func (r *ScheduledTaskRepo) FindAllEnabled(ctx context.Context) ([]models.ScheduledTask, error) {
	var tasks []models.ScheduledTask
	err := r.db.WithContext(ctx).Where("enabled = ?", true).Find(&tasks).Error
	return tasks, err
}

// FindAll 查询所有任务
func (r *ScheduledTaskRepo) FindAll(ctx context.Context) ([]models.ScheduledTask, error) {
	var tasks []models.ScheduledTask
	err := r.db.WithContext(ctx).Find(&tasks).Error
	return tasks, err
}

func (r *ScheduledTaskRepo) UpdateLastRunStatusByMsgId(ctx context.Context, msgId string, status models.LastRunStatus) error {
	switch status {
	case models.LastRunStatusAmbiguous, models.LastRunStatusSuccess, models.LastRunStatusFailed:
	default:
		return fmt.Errorf("不支持的计划任务运行状态: %q", status)
	}

	result := r.db.WithContext(ctx).Model(&models.ScheduledTask{}).
		Where("last_msg_id = ? AND last_run_status = ?", msgId, models.LastRunStatusAmbiguous).
		Update("last_run_status", status)
	if result.Error != nil {
		return result.Error
	}
	// ClaimExecution 会先把本次执行标为 ambiguous。仅允许从该中间态
	// 进入终态，可以同时防止：
	//   - 旧的超时回调在迟到回执后把 success/failed 改回 ambiguous；
	//   - 重复或冲突的终态回执互相覆盖。
	// 普通手工短信没有计划任务关联，匹配 0 行是正常情况。
	return nil
}

// UpdateDefinition 只更新用户可编辑字段，避免覆盖异步短信回执写入的运行状态。
func (r *ScheduledTaskRepo) UpdateDefinition(ctx context.Context, task *models.ScheduledTask) error {
	result := r.db.WithContext(ctx).Model(&models.ScheduledTask{}).Where("id = ?", task.ID).Updates(map[string]any{
		"name": task.Name, "sim_id": task.SIMID, "enabled": task.Enabled,
		"interval_days": task.IntervalDays, "phone_number": task.PhoneNumber,
		"content": task.Content, "updated_at": task.UpdatedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ClaimExecution 在串口下发前持久化关联，初始标为 ambiguous：进程若在下发或
// 等待回执期间退出，也不会把本次尝试当作可快速重试的普通失败。
func (r *ScheduledTaskRepo) ClaimExecution(
	ctx context.Context,
	id string,
	msgID string,
	runAt int64,
	allowTerminalAmbiguous ...bool,
) error {
	allowAmbiguous := len(allowTerminalAmbiguous) > 0 && allowTerminalAmbiguous[0]
	missingClaimStaleBefore := runAt - scheduledTaskClaimRecoveryGrace.Milliseconds()
	result := r.db.WithContext(ctx).Model(&models.ScheduledTask{}).
		Where(`id = ? AND (
			COALESCE(last_msg_id, '') = ''
			OR COALESCE(last_run_status, '') <> ?
			OR EXISTS (
				SELECT 1 FROM text_messages
				WHERE text_messages.id = scheduled_tasks.last_msg_id
				AND text_messages.status IN ?
			)
			OR (
				? = true
				AND EXISTS (
					SELECT 1 FROM text_messages
					WHERE text_messages.id = scheduled_tasks.last_msg_id
					AND text_messages.status = ?
				)
			)
			OR (
				NOT EXISTS (
					SELECT 1 FROM text_messages
					WHERE text_messages.id = scheduled_tasks.last_msg_id
				)
				AND last_run_at <= ?
			)
		)`, id, models.LastRunStatusAmbiguous, []models.MessageStatus{
			models.MessageStatusSent, models.MessageStatusFailed,
		}, allowAmbiguous, models.MessageStatusAmbiguous, missingClaimStaleBefore).
		Updates(map[string]any{
			"last_msg_id": msgID, "last_run_at": runAt, "last_run_status": models.LastRunStatusAmbiguous,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := r.db.WithContext(ctx).Model(&models.ScheduledTask{}).
			Where("id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return gorm.ErrRecordNotFound
		}
		return ErrScheduledTaskExecutionPending
	}
	return nil
}

// RestoreExecution 仅在关联仍指向本次 msgID 且处于 ambiguous 时回滚，
// 避免覆盖已经到达的设备回执。
func (r *ScheduledTaskRepo) RestoreExecution(
	ctx context.Context,
	id string,
	msgID string,
	previous models.ScheduledTask,
) error {
	result := r.db.WithContext(ctx).Model(&models.ScheduledTask{}).
		Where("id = ? AND last_msg_id = ? AND last_run_status = ?", id, msgID, models.LastRunStatusAmbiguous).
		Updates(map[string]any{
			"last_msg_id":     previous.LastMsgId,
			"last_run_at":     previous.LastRunAt,
			"last_run_status": previous.LastRunStatus,
		})
	if result.Error != nil {
		return result.Error
	}
	// 条件未命中表示回执或新一次执行已经推进了状态；这是一次
	// 已被更新状态取代的回滚，不应当作数据库错误。
	return nil
}
