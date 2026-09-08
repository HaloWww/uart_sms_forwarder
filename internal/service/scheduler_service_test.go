package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestScheduledTaskBindsKnownOfflineSIM(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_known_offline")
	const simID = "iccid:8986000000000000001"
	if err := db.Create(&models.SIMProfile{ID: simID, ICCID: "8986000000000000001"}).Error; err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	task := &models.ScheduledTask{
		DeviceID: "legacy-device", SIMID: simID, Name: "保号", Enabled: true,
		IntervalDays: 30, PhoneNumber: "10086", Content: "test",
	}
	if err := scheduler.Create(context.Background(), task); err != nil {
		t.Fatalf("Create() rejected known offline SIM: %v", err)
	}
	if task.DeviceID != "" {
		t.Fatalf("new task retained physical DeviceID %q", task.DeviceID)
	}
}

func TestScheduledTaskRejectsMissingOrUnknownSIM(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_reject_identity")
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	base := models.ScheduledTask{Name: "保号", IntervalDays: 30, PhoneNumber: "10086", Content: "test"}

	missing := base
	if err := scheduler.Create(context.Background(), &missing); !errors.Is(err, ErrScheduledTaskSIMRequired) {
		t.Fatalf("Create(missing) err=%v", err)
	}
	unknown := base
	unknown.SIMID = "iccid:missing"
	if err := scheduler.Create(context.Background(), &unknown); !errors.Is(err, ErrSIMUnknown) {
		t.Fatalf("Create(unknown) err=%v", err)
	}
}

func TestScheduledExecutionReloadsDisabledTaskBeforeSending(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_reload_disabled")
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	task := models.ScheduledTask{
		ID: "disabled-before-execution", SIMID: "iccid:any", Name: "已停用",
		Enabled: false, IntervalDays: 1, PhoneNumber: "10086", Content: "test",
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.executeTask(task.ID, true); err != nil {
		t.Fatal(err)
	}
	var stored models.ScheduledTask
	if err := db.First(&stored, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastMsgId != "" || stored.LastRunAt != 0 {
		t.Fatalf("disabled task was claimed for execution: %+v", stored)
	}
}

func TestScheduledTaskRejectsConcurrentMutationOrTrigger(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_task_lock")
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	lock, err := scheduler.tryLockTask("busy-task")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	if err := scheduler.TriggerTask(context.Background(), "busy-task"); !errors.Is(err, ErrScheduledTaskBusy) {
		t.Fatalf("TriggerTask() err=%v, want ErrScheduledTaskBusy", err)
	}
	if err := scheduler.Delete(context.Background(), "busy-task"); !errors.Is(err, ErrScheduledTaskBusy) {
		t.Fatalf("Delete() err=%v, want ErrScheduledTaskBusy", err)
	}
	if err := scheduler.Update(context.Background(), &models.ScheduledTask{ID: "busy-task"}); !errors.Is(err, ErrScheduledTaskBusy) {
		t.Fatalf("Update() err=%v, want ErrScheduledTaskBusy", err)
	}
}

func TestScheduledTaskClaimExistsBeforeFastResult(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_fast_result")
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	task := models.ScheduledTask{ID: "fast-result", Name: "快速回执"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.repo.ClaimExecution(context.Background(), task.ID, "message-1", 1234); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.UpdateLastRunStatusByMsgId(context.Background(), "message-1", models.LastRunStatusFailed); err != nil {
		t.Fatal(err)
	}
	stored, err := scheduler.GetById(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastMsgId != "message-1" || stored.LastRunAt != 1234 || stored.LastRunStatus != models.LastRunStatusFailed {
		t.Fatalf("fast result did not update claimed task: %+v", stored)
	}
}

func TestScheduledExecutionUsesIdempotencyClaimBeforeUART(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_idempotency_claim")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	manager, err := NewSerialManager(
		zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, messageService, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	serialService := manager.services["default"]
	setTestSIM(serialService, "8986000000000000001")
	port := &capturingSerialPort{}
	serialService.port = port

	task := &models.ScheduledTask{
		ID: "idempotent-scheduled-task", SIMID: "iccid:8986000000000000001",
		Name: "保号", Enabled: true, IntervalDays: 30, PhoneNumber: "10086", Content: "test",
	}
	if err := db.Create(task).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	if err := scheduler.executeTask(task.ID, false); err != nil {
		t.Fatalf("executeTask() error = %v", err)
	}

	var storedTask models.ScheduledTask
	if err := db.First(&storedTask, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedTask.LastMsgId == "" {
		t.Fatal("scheduled execution did not persist its request ID")
	}
	var storedMessage models.TextMessage
	if err := db.First(&storedMessage, "id = ?", storedTask.LastMsgId).Error; err != nil {
		t.Fatal(err)
	}
	if storedMessage.DeviceID != "default" || storedMessage.SIMID != task.SIMID {
		t.Fatalf("scheduled request claim was not completed: %+v", storedMessage)
	}
	frame := port.String()
	if !strings.Contains(frame, `"request_id":"`+storedTask.LastMsgId+`"`) ||
		!strings.Contains(frame, `"action":"send_sms"`) {
		t.Fatalf("scheduled execution UART frame = %q", frame)
	}
	serialService.stopSMSSendTimeout(storedTask.LastMsgId)
}

func TestScheduledTaskClaimBlocksPendingExecution(t *testing.T) {
	for _, test := range []struct {
		name          string
		dbName        string
		messageStatus *models.MessageStatus
		wantBusy      bool
	}{
		{name: "claim before message save", dbName: "missing", wantBusy: true},
		{name: "message sending", dbName: "sending", messageStatus: messageStatusPtr(models.MessageStatusSending), wantBusy: true},
		{name: "message ambiguous", dbName: "ambiguous", messageStatus: messageStatusPtr(models.MessageStatusAmbiguous), wantBusy: true},
		{name: "message sent", dbName: "sent", messageStatus: messageStatusPtr(models.MessageStatusSent)},
		{name: "message failed", dbName: "failed", messageStatus: messageStatusPtr(models.MessageStatusFailed)},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newSchedulerTestDB(t, "scheduler_pending_claim_"+test.dbName)
			taskRepo := repo.NewScheduledTaskRepo(db)
			ctx := context.Background()
			task := models.ScheduledTask{ID: "task", Name: test.name}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := taskRepo.ClaimExecution(ctx, task.ID, "first-message", 1000); err != nil {
				t.Fatal(err)
			}
			if test.messageStatus != nil {
				message := models.TextMessage{
					ID: "first-message", Type: models.MessageTypeOutgoing, Status: *test.messageStatus,
				}
				if err := db.Create(&message).Error; err != nil {
					t.Fatal(err)
				}
			}

			err := taskRepo.ClaimExecution(ctx, task.ID, "second-message", 2000)
			if test.wantBusy {
				if !errors.Is(err, repo.ErrScheduledTaskExecutionPending) {
					t.Fatalf("ClaimExecution() err=%v, want pending execution", err)
				}
			} else if err != nil {
				t.Fatalf("ClaimExecution() err=%v", err)
			}

			var stored models.ScheduledTask
			if err := db.First(&stored, "id = ?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			wantMsgID, wantRunAt := "second-message", int64(2000)
			if test.wantBusy {
				wantMsgID, wantRunAt = "first-message", 1000
			}
			if stored.LastMsgId != wantMsgID || stored.LastRunAt != wantRunAt {
				t.Fatalf("claim state=%+v, want msg=%q runAt=%d", stored, wantMsgID, wantRunAt)
			}
		})
	}
}

func TestScheduledTaskPendingExecutionReturnsBusy(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		name := "manual"
		if scheduled {
			name = "scheduled"
		}
		t.Run(name, func(t *testing.T) {
			db := newSchedulerTestDB(t, "scheduler_pending_"+name)
			scheduler := &SchedulerService{
				logger: zap.NewNop(), repo: repo.NewScheduledTaskRepo(db),
			}
			task := models.ScheduledTask{
				ID: "task", SIMID: "iccid:8986000000000000001", Name: name,
				Enabled: true, IntervalDays: 1, PhoneNumber: "10086", Content: "test",
			}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := scheduler.repo.ClaimExecution(context.Background(), task.ID, "pending-message", 1000); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.TextMessage{
				ID: "pending-message", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := scheduler.executeTask(task.ID, scheduled); !errors.Is(err, ErrScheduledTaskBusy) {
				t.Fatalf("executeTask() err=%v, want ErrScheduledTaskBusy", err)
			}
			stored, err := scheduler.GetById(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.LastMsgId != "pending-message" || stored.LastRunAt != 1000 {
				t.Fatalf("pending claim was overwritten: %+v", stored)
			}
		})
	}
}

func TestScheduledClaimAllowsTerminalAmbiguousAtNormalInterval(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_terminal_ambiguous_next_interval")
	taskRepo := repo.NewScheduledTaskRepo(db)
	ctx := context.Background()
	task := models.ScheduledTask{ID: "task", Name: "next interval"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	firstRun := time.Now().AddDate(0, 0, -30).UnixMilli()
	if err := taskRepo.ClaimExecution(ctx, task.ID, "first-message", firstRun); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.TextMessage{
		ID: "first-message", Type: models.MessageTypeOutgoing, Status: models.MessageStatusAmbiguous,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := taskRepo.ClaimExecution(
		ctx, task.ID, "second-message", time.Now().UnixMilli(), true,
	); err != nil {
		t.Fatalf("normal scheduled interval could not advance terminal ambiguous attempt: %v", err)
	}
}

func messageStatusPtr(status models.MessageStatus) *models.MessageStatus {
	return &status
}

func TestScheduledTaskLateResultWinsTimeout(t *testing.T) {
	for _, test := range []struct {
		name               string
		messageStatus      models.MessageStatus
		taskStatus         models.LastRunStatus
		submitted          bool
		competingTaskState models.LastRunStatus
	}{
		{
			name: "late success", messageStatus: models.MessageStatusSent,
			taskStatus: models.LastRunStatusSuccess, submitted: true,
			competingTaskState: models.LastRunStatusFailed,
		},
		{
			name: "late failure", messageStatus: models.MessageStatusFailed,
			taskStatus:         models.LastRunStatusFailed,
			competingTaskState: models.LastRunStatusSuccess,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newSchedulerTestDB(t, "scheduler_late_result_"+string(test.messageStatus))
			scheduler := &SchedulerService{repo: repo.NewScheduledTaskRepo(db)}
			messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
			ctx := context.Background()
			msgID := "message-" + string(test.messageStatus)
			task := models.ScheduledTask{ID: "task-" + string(test.messageStatus), Name: test.name}
			message := models.TextMessage{
				ID: msgID, Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
			}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&message).Error; err != nil {
				t.Fatal(err)
			}
			if err := scheduler.repo.ClaimExecution(ctx, task.ID, msgID, 1234); err != nil {
				t.Fatal(err)
			}

			// 复现最坏顺序：超时线程已把短信改为 ambiguous，但尚未
			// 更新任务；此时迟到回执完成了短信和任务的终态更新。
			updated, err := messageService.MarkSendResultTimeout(ctx, msgID)
			if err != nil || !updated {
				t.Fatalf("MarkSendResultTimeout() updated=%v err=%v", updated, err)
			}
			updated, err = messageService.UpdateSendResultById(
				ctx, msgID, test.messageStatus, test.submitted, false, "",
			)
			if err != nil || !updated {
				t.Fatalf("UpdateSendResultById() updated=%v err=%v", updated, err)
			}
			if err := scheduler.UpdateLastRunStatusByMsgId(ctx, msgID, test.taskStatus); err != nil {
				t.Fatal(err)
			}

			// 超时线程随后的 ambiguous 写入、以及冲突的过期终态都必须失效。
			if err := scheduler.UpdateLastRunStatusByMsgId(ctx, msgID, models.LastRunStatusAmbiguous); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.UpdateLastRunStatusByMsgId(ctx, msgID, test.competingTaskState); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.repo.RestoreExecution(ctx, task.ID, msgID, task); err != nil {
				t.Fatalf("stale RestoreExecution() err=%v", err)
			}

			storedMessage, err := messageService.Get(ctx, msgID)
			if err != nil {
				t.Fatal(err)
			}
			if storedMessage.Status != test.messageStatus || storedMessage.Ambiguous {
				t.Fatalf("message terminal state was overwritten: %+v", storedMessage)
			}
			storedTask, err := scheduler.GetById(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedTask.LastRunStatus != test.taskStatus {
				t.Fatalf("task terminal state=%q, want %q", storedTask.LastRunStatus, test.taskStatus)
			}
		})
	}
}

func TestScheduledTaskStaleResultCannotOverwriteNewExecution(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_stale_result")
	scheduler := &SchedulerService{repo: repo.NewScheduledTaskRepo(db)}
	ctx := context.Background()
	task := models.ScheduledTask{ID: "task", Name: "new execution"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.repo.ClaimExecution(ctx, task.ID, "old-message", 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.TextMessage{
		ID: "old-message", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := scheduler.repo.ClaimExecution(ctx, task.ID, "new-message", 2000); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.UpdateLastRunStatusByMsgId(ctx, "old-message", models.LastRunStatusSuccess); err != nil {
		t.Fatal(err)
	}

	stored, err := scheduler.GetById(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastMsgId != "new-message" || stored.LastRunAt != 2000 ||
		stored.LastRunStatus != models.LastRunStatusAmbiguous {
		t.Fatalf("stale result overwrote new execution: %+v", stored)
	}
}

func TestAmbiguousScheduledTaskDoesNotUseFastFailureRetry(t *testing.T) {
	scheduler := &SchedulerService{}
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	task := models.ScheduledTask{
		IntervalDays:  30,
		LastRunAt:     now.AddDate(0, 0, -1).UnixMilli(),
		LastRunStatus: models.LastRunStatusAmbiguous,
	}
	if scheduler.shouldExecuteTask(task, now) {
		t.Fatal("ambiguous result was treated as a retryable failure")
	}
	task.LastRunAt = now.AddDate(0, 0, -30).UnixMilli()
	if !scheduler.shouldExecuteTask(task, now) {
		t.Fatal("ambiguous result did not become due after its normal interval")
	}
}

func TestManualScheduledTriggerRequestIDIsDurableAcrossLaterRuns(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_manual_request_replay")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	manager, err := NewSerialManager(
		zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, messageService, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	serialService := manager.services["default"]
	setTestSIM(serialService, "8986000000000000001")
	port := &capturingSerialPort{}
	serialService.port = port
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	task := &models.ScheduledTask{
		ID: "durable-trigger-task", SIMID: "iccid:8986000000000000001",
		Name: "保号", Enabled: true, IntervalDays: 30, PhoneNumber: "10086", Content: "test",
	}
	if err := db.Create(task).Error; err != nil {
		t.Fatal(err)
	}
	firstID := "11111111-1111-4111-8111-111111111111"
	secondID := "22222222-2222-4222-8222-222222222222"
	if got, err := scheduler.TriggerTaskWithRequestID(context.Background(), task.ID, firstID); err != nil || got != firstID {
		t.Fatalf("first trigger id=%q err=%v", got, err)
	}
	serialService.stopSMSSendTimeout(firstID)
	if updated, err := messageService.UpdateSendResultById(
		context.Background(), firstID, models.MessageStatusSent, true, false, "",
	); err != nil || !updated {
		t.Fatalf("finish first message updated=%v err=%v", updated, err)
	}
	if err := scheduler.UpdateLastRunStatusByMsgId(
		context.Background(), firstID, models.LastRunStatusSuccess,
	); err != nil {
		t.Fatal(err)
	}

	if got, err := scheduler.TriggerTaskWithRequestID(context.Background(), task.ID, secondID); err != nil || got != secondID {
		t.Fatalf("second trigger id=%q err=%v", got, err)
	}
	serialService.stopSMSSendTimeout(secondID)
	if got, err := scheduler.TriggerTaskWithRequestID(context.Background(), task.ID, firstID); err != nil || got != firstID {
		t.Fatalf("old request replay id=%q err=%v", got, err)
	}

	if count := strings.Count(port.String(), `"action":"send_sms"`); count != 2 {
		t.Fatalf("UART send count=%d, want 2; frame=%q", count, port.String())
	}
	var storedTask models.ScheduledTask
	if err := db.First(&storedTask, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedTask.LastMsgId != secondID {
		t.Fatalf("old replay replaced current task result: %+v", storedTask)
	}
	var firstMessage models.TextMessage
	if err := db.First(&firstMessage, "id = ?", firstID).Error; err != nil {
		t.Fatal(err)
	}
	if firstMessage.ScheduledTaskID != task.ID {
		t.Fatalf("message lost permanent task binding: %+v", firstMessage)
	}
}

func TestManualScheduledTriggerRejectsRequestIDOwnedByAnotherTask(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduler_cross_task_request_conflict")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	manager, err := NewSerialManager(
		zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, messageService, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	serialService := manager.services["default"]
	setTestSIM(serialService, "8986000000000000001")
	port := &capturingSerialPort{}
	serialService.port = port
	scheduler := NewSchedulerService(zap.NewNop(), db, manager)
	first := models.ScheduledTask{
		ID: "first-task", SIMID: "iccid:8986000000000000001", Name: "first",
		Enabled: true, IntervalDays: 30, PhoneNumber: "10086", Content: "test",
	}
	second := first
	second.ID = "second-task"
	second.Name = "second"
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	requestID := "33333333-3333-4333-8333-333333333333"
	if _, err := scheduler.TriggerTaskWithRequestID(context.Background(), first.ID, requestID); err != nil {
		t.Fatal(err)
	}
	serialService.stopSMSSendTimeout(requestID)
	if _, err := scheduler.TriggerTaskWithRequestID(context.Background(), second.ID, requestID); !errors.Is(err, ErrSMSRequestConflict) {
		t.Fatalf("cross-task replay error=%v, want ErrSMSRequestConflict", err)
	}
	if count := strings.Count(port.String(), `"action":"send_sms"`); count != 1 {
		t.Fatalf("UART send count=%d, want 1; frame=%q", count, port.String())
	}
	var storedSecond models.ScheduledTask
	if err := db.First(&storedSecond, "id = ?", second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedSecond.LastMsgId != "" {
		t.Fatalf("conflicting request claimed second task: %+v", storedSecond)
	}
}

func TestManualScheduledTriggerRejectsInvalidRequestID(t *testing.T) {
	scheduler := &SchedulerService{}
	if _, err := scheduler.TriggerTaskWithRequestID(context.Background(), "task", "not-a-uuid"); !errors.Is(err, ErrSMSRequestIDInvalid) {
		t.Fatalf("invalid request ID error=%v", err)
	}
}

func newSchedulerTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN(name)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.ScheduledTask{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	return db
}
