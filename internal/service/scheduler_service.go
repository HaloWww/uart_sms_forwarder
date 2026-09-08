package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

var (
	ErrScheduledTaskSIMRequired = errors.New("计划任务必须指定 SIM")
	ErrScheduledTaskBusy        = errors.New("计划任务正在执行或修改")
)

// SchedulerService 定时任务调度服务（包含任务管理功能）
type SchedulerService struct {
	logger        *zap.Logger
	cron          *cron.Cron
	repo          *repo.ScheduledTaskRepo
	serialManager *SerialManager
	taskLocks     sync.Map // map[taskID]*sync.Mutex；保留锁对象以保证同 ID 始终串行
}

// NewSchedulerService 创建定时任务服务实例
func NewSchedulerService(
	logger *zap.Logger,
	db *gorm.DB,
	serialManager *SerialManager,
) *SchedulerService {
	return &SchedulerService{
		logger:        logger,
		repo:          repo.NewScheduledTaskRepo(db),
		serialManager: serialManager,
	}
}

// ==================== 任务管理方法 ====================

// GetAll 获取所有定时任务
func (s *SchedulerService) GetAll(ctx context.Context) ([]models.ScheduledTask, error) {
	return s.repo.FindAll(ctx)
}

// GetAllEnabled 获取所有启用的定时任务
func (s *SchedulerService) GetAllEnabled(ctx context.Context) ([]models.ScheduledTask, error) {
	return s.repo.FindAllEnabled(ctx)
}

// GetById 根据ID获取定时任务
func (s *SchedulerService) GetById(ctx context.Context, id string) (*models.ScheduledTask, error) {
	task, err := s.repo.FindById(ctx, id)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *SchedulerService) tryLockTask(id string) (*sync.Mutex, error) {
	value, _ := s.taskLocks.LoadOrStore(id, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	if !lock.TryLock() {
		return nil, ErrScheduledTaskBusy
	}
	return lock, nil
}

// Create 创建定时任务
func (s *SchedulerService) Create(ctx context.Context, task *models.ScheduledTask) error {
	task.SIMID = strings.TrimSpace(task.SIMID)
	if task.SIMID == "" || task.SIMID == UnassignedSIMID {
		return ErrScheduledTaskSIMRequired
	}
	if err := ValidateSMSDestination(task.PhoneNumber); err != nil {
		return err
	}
	if err := ValidateSMSContent(task.Content); err != nil {
		return err
	}
	if err := s.serialManager.ValidateSIM(ctx, task.SIMID); err != nil {
		return err
	}
	task.DeviceID = ""
	now := time.Now().UnixMilli()
	task.ID = uuid.New().String()
	task.CreatedAt = now
	task.UpdatedAt = now
	return s.repo.Create(ctx, task)
}

// Update 更新定时任务
func (s *SchedulerService) Update(ctx context.Context, task *models.ScheduledTask) error {
	lock, err := s.tryLockTask(task.ID)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	task.SIMID = strings.TrimSpace(task.SIMID)
	if task.SIMID == "" || task.SIMID == UnassignedSIMID {
		return ErrScheduledTaskSIMRequired
	}
	if err := ValidateSMSDestination(task.PhoneNumber); err != nil {
		return err
	}
	if err := ValidateSMSContent(task.Content); err != nil {
		return err
	}
	if err := s.serialManager.ValidateSIM(ctx, task.SIMID); err != nil {
		return err
	}
	existingTask, err := s.GetById(ctx, task.ID)
	if err != nil {
		return err
	}
	existingTask.Name = task.Name
	existingTask.SIMID = task.SIMID
	existingTask.Enabled = task.Enabled
	existingTask.IntervalDays = task.IntervalDays
	existingTask.PhoneNumber = task.PhoneNumber
	existingTask.Content = task.Content
	existingTask.UpdatedAt = time.Now().UnixMilli()

	if err := s.repo.UpdateDefinition(ctx, existingTask); err != nil {
		return err
	}
	*task = *existingTask
	return nil
}

// Delete 删除定时任务
func (s *SchedulerService) Delete(ctx context.Context, id string) error {
	lock, err := s.tryLockTask(id)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return s.repo.DeleteById(ctx, id)
}

// TriggerTask 立即触发执行指定的任务
func (s *SchedulerService) TriggerTask(ctx context.Context, id string) error {
	_, err := s.TriggerTaskWithRequestID(ctx, id, uuid.NewString())
	return err
}

// TriggerTaskWithRequestID 使用浏览器生成并持久化的 UUID 触发一次执行。
// 浏览器响应丢失后重放同一 ID，只会读取第一次执行的状态。
func (s *SchedulerService) TriggerTaskWithRequestID(
	ctx context.Context,
	id string,
	requestID string,
) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(requestID))
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrSMSRequestIDInvalid, requestID)
	}
	messageID, err := s.executeTaskWithRequestID(id, false, parsed.String())
	if err != nil {
		return messageID, fmt.Errorf("执行任务失败: %w", err)
	}
	return messageID, nil
}

// ==================== 调度相关方法 ====================

// Start 启动定时任务服务
func (s *SchedulerService) Start(ctx context.Context) error {
	s.cron = cron.New()

	// 添加每天执行一次的检查任务（每天早上8点执行）
	_, err := s.cron.AddFunc("0 8 * * *", func() {
		s.logger.Info("开始检查定时任务")
		if err := s.checkAndExecuteTasks(); err != nil {
			s.logger.Error("检查并执行定时任务失败", zap.Error(err))
		}
	})
	if err != nil {
		return fmt.Errorf("添加检查任务失败: %w", err)
	}

	// 启动 cron
	s.cron.Start()

	s.logger.Info("定时任务服务启动成功")
	return nil
}

// checkAndExecuteTasks 检查并执行满足条件的任务
func (s *SchedulerService) checkAndExecuteTasks() error {
	ctx := context.Background()

	// 获取所有启用的任务
	tasks, err := s.GetAllEnabled(ctx)
	if err != nil {
		s.logger.Error("获取启用的定时任务失败", zap.Error(err))
		return err
	}

	now := time.Now()
	for _, task := range tasks {
		// 检查是否需要执行
		if s.shouldExecuteTask(task, now) {
			s.logger.Info("任务满足执行条件",
				zap.String("id", task.ID),
				zap.String("name", task.Name),
				zap.Int("intervalDays", task.IntervalDays))

			if err := s.executeTask(task.ID, true); err != nil {
				s.logger.Error("执行定时任务失败",
					zap.String("id", task.ID),
					zap.String("name", task.Name),
					zap.Error(err))
			}
		}
	}

	return nil
}

// shouldExecuteTask 判断任务是否应该执行
func (s *SchedulerService) shouldExecuteTask(task models.ScheduledTask, now time.Time) bool {
	// 如果从未执行过，则执行
	if task.LastRunAt <= 0 {
		return true
	}

	// 计算距离上次执行的天数
	lastRun := time.UnixMilli(task.LastRunAt)
	daysSinceLastRun := int(now.Sub(lastRun).Hours() / 24)

	// 如果上次执行失败，1天后就可以重试
	if task.LastRunStatus == models.LastRunStatusFailed {
		return daysSinceLastRun >= 1
	}

	// 如果满足间隔天数条件，则执行
	return daysSinceLastRun >= task.IntervalDays
}

// executeTask 执行任务。定时扫描使用 scheduled=true，会在拿到任务锁后重新读取
// 并再次判断 enabled/周期，防止使用扫描阶段的旧 SIM 绑定发送。
func (s *SchedulerService) executeTask(taskID string, scheduled bool) error {
	_, err := s.executeTaskWithRequestID(taskID, scheduled, "")
	return err
}

func (s *SchedulerService) executeTaskWithRequestID(
	taskID string,
	scheduled bool,
	requestID string,
) (string, error) {
	lock, err := s.tryLockTask(taskID)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()

	ctx := context.Background()
	current, err := s.GetById(ctx, taskID)
	if err != nil {
		return "", err
	}
	if requestID != "" {
		messageID, known, replayErr := s.replayScheduledRequest(ctx, current.ID, requestID)
		if known || replayErr != nil {
			return messageID, replayErr
		}
	}
	if scheduled && (!current.Enabled || !s.shouldExecuteTask(*current, time.Now())) {
		return "", nil
	}
	task := *current
	s.logger.Info("执行定时任务",
		zap.String("id", task.ID),
		zap.String("name", task.Name),
		zap.String("sim_id", task.SIMID),
		zap.Int("content_length", len(task.Content)))

	task.SIMID = strings.TrimSpace(task.SIMID)
	if task.SIMID == "" || task.SIMID == UnassignedSIMID {
		s.logger.Error("计划任务没有可用的 SIM 身份，已阻止发送",
			zap.String("id", task.ID), zap.String("legacy_device_id", task.DeviceID))
		return "", ErrScheduledTaskSIMRequired
	}

	// 先持久化 task↔message 关联，再下发串口命令。设备即使立即回包，
	// UpdateLastRunStatusByMsgId 也一定能命中本次任务。
	msgID := requestID
	if msgID == "" {
		msgID = uuid.NewString()
	}
	if err := s.repo.ClaimExecution(ctx, task.ID, msgID, time.Now().UnixMilli(), scheduled); err != nil {
		if errors.Is(err, repo.ErrScheduledTaskExecutionPending) {
			return msgID, ErrScheduledTaskBusy
		}
		return msgID, fmt.Errorf("预登记计划任务执行失败: %w", err)
	}

	// SendSMS 会统一处理飞行模式唤醒、等待网络注册及原手动状态恢复。
	_, err = s.serialManager.sendScheduledSMSWithRequestID(
		task.ID, task.SIMID, task.PhoneNumber, task.Content, msgID,
	)
	if err != nil {
		s.logger.Error("定时任务发送短信失败",
			zap.String("id", task.ID),
			zap.String("name", task.Name),
			zap.Error(err))
		if errors.Is(err, ErrSMSSubmissionAmbiguous) {
			// SerialService 已将消息与任务标记为 ambiguous；保留预登记，
			// 设备若稍后回包仍可更新最终结果。
			return msgID, err
		}
		// 未成功写出完整命令时恢复执行前状态；若回滚也失败，预登记的
		// ambiguous 状态会保守地阻止快速自动重试。
		if restoreErr := s.repo.RestoreExecution(ctx, task.ID, msgID, task); restoreErr != nil {
			return msgID, fmt.Errorf("%w；回滚执行登记失败: %v", err, restoreErr)
		}
		return msgID, err
	}
	s.logger.Info("定时任务短信已提交，等待设备最终回执",
		zap.String("id", task.ID),
		zap.String("name", task.Name),
		zap.String("request_id", msgID))

	return msgID, nil
}

// replayScheduledRequest 依赖短信记录上的永久 task 归属，而不是任务当前的
// LastMsgId。这样任务以后执行新一轮后，旧请求仍不会把 LastMsgId 回滚或重复发送。
func (s *SchedulerService) replayScheduledRequest(
	ctx context.Context,
	taskID string,
	requestID string,
) (messageID string, known bool, err error) {
	if s.serialManager == nil || s.serialManager.textMsgService == nil {
		return "", false, nil
	}
	message, err := s.serialManager.textMsgService.Get(ctx, requestID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return requestID, true, err
	}
	if message.Type != models.MessageTypeOutgoing || message.ScheduledTaskID != taskID {
		return requestID, true, fmt.Errorf("%w: %s", ErrSMSRequestConflict, requestID)
	}
	switch message.Status {
	case models.MessageStatusSending, models.MessageStatusSent:
		return message.ID, true, nil
	case models.MessageStatusAmbiguous:
		return message.ID, true, ErrSMSSubmissionAmbiguous
	case models.MessageStatusFailed:
		return message.ID, true, fmt.Errorf("%w: %s", ErrSMSRequestPreviouslyFailed, message.SendError)
	default:
		return message.ID, true, fmt.Errorf("%w: 非法短信状态 %q", ErrSMSRequestConflict, message.Status)
	}
}

func (s *SchedulerService) UpdateLastRunStatusByMsgId(ctx context.Context, msgId string, status models.LastRunStatus) error {
	return s.repo.UpdateLastRunStatusByMsgId(ctx, msgId, status)
}
