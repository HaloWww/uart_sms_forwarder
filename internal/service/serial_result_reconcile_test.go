package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"go.uber.org/zap"
)

func TestScheduledTaskStatusFailureRetriesFromPersistedMessageState(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduled_task_background_reconcile")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
	)
	serialService.scheduledTaskRetryInitial = 5 * time.Millisecond
	serialService.scheduledTaskRetryMax = 10 * time.Millisecond

	const requestID = "scheduled-task-background-message"
	if err := db.Create(&models.TextMessage{
		ID: requestID, Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent,
	}).Error; err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	retryDone := make(chan models.LastRunStatus, 1)
	serialService.SetScheduledTaskStatusUpdater(func(
		_ context.Context, _ string, status models.LastRunStatus,
	) error {
		if calls.Add(1) == 1 {
			return errors.New("injected transient task update error")
		}
		retryDone <- status
		return nil
	})

	// 故意传入与数据库终态相反的状态；后台重试必须重新读取 TextMessage，
	// 并且同一 messageId 的重复调度只能产生一个重试循环。
	serialService.updateScheduledTaskStatus(context.Background(), requestID, models.LastRunStatusFailed)
	serialService.scheduleScheduledTaskStatusRetry(requestID)

	select {
	case got := <-retryDone:
		if got != models.LastRunStatusSuccess {
			t.Fatalf("retry status=%q, want success from persisted sent message", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scheduled task status retry")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, active := serialService.scheduledTaskStatusRetries.Load(requestID); !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled task retry goroutine did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("scheduled task updater calls=%d, want one initial call and one deduplicated retry", got)
	}
}

func TestScheduledTaskStatusRetryStopsWhenEvidenceWasDeleted(t *testing.T) {
	db := newSchedulerTestDB(t, "scheduled_task_background_deleted_evidence")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
	)
	serialService.scheduledTaskRetryInitial = 20 * time.Millisecond
	serialService.scheduledTaskRetryMax = 20 * time.Millisecond

	const requestID = "deleted-scheduled-task-message"
	const simID = "iccid:8986000000000000001"
	if err := db.Create(&models.TextMessage{
		ID: requestID, SIMID: simID, To: "10086",
		Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	serialService.SetScheduledTaskStatusUpdater(func(
		_ context.Context, _ string, _ models.LastRunStatus,
	) error {
		calls.Add(1)
		return errors.New("injected transient task update error")
	})
	serialService.updateScheduledTaskStatus(context.Background(), requestID, models.LastRunStatusSuccess)
	if err := messageService.Delete(context.Background(), requestID, MessageScope{SIMID: simID}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		if _, active := serialService.scheduledTaskStatusRetries.Load(requestID); !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry did not stop after its message evidence was removed")
		}
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("scheduled task updater calls=%d, want only the initial failed call", got)
	}
}

func TestDuplicateResultReconcilesScheduledTaskAfterUpdaterFailure(t *testing.T) {
	for _, test := range []struct {
		name            string
		success         bool
		submitted       bool
		messageStatus   models.MessageStatus
		taskStatus      models.LastRunStatus
		requestIDSuffix string
	}{
		{
			name: "success", success: true, submitted: true,
			messageStatus: models.MessageStatusSent, taskStatus: models.LastRunStatusSuccess,
			requestIDSuffix: "success",
		},
		{
			name: "failure", success: false, submitted: false,
			messageStatus: models.MessageStatusFailed, taskStatus: models.LastRunStatusFailed,
			requestIDSuffix: "failure",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newSchedulerTestDB(t, "duplicate_result_reconcile_"+test.requestIDSuffix)
			messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
			taskRepo := repo.NewScheduledTaskRepo(db)
			serialService := NewSerialService(
				zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
			)
			ctx := context.Background()
			requestID := "message-" + test.requestIDSuffix
			task := models.ScheduledTask{ID: "task-" + test.requestIDSuffix, Name: test.name}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.TextMessage{
				ID: requestID, SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001",
				To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := taskRepo.ClaimExecution(ctx, task.ID, requestID, 1234); err != nil {
				t.Fatal(err)
			}

			updateCalls := 0
			serialService.SetScheduledTaskStatusUpdater(func(
				ctx context.Context, msgID string, status models.LastRunStatus,
			) error {
				updateCalls++
				if updateCalls == 1 {
					return errors.New("injected transient update error")
				}
				return taskRepo.UpdateLastRunStatusByMsgId(ctx, msgID, status)
			})
			result := &ParsedMessage{Payload: map[string]any{
				"request_id": requestID, "to": "10086",
				"success": test.success, "submitted": test.submitted, "ambiguous": false,
				"identity_valid": true, "iccid": "8986000000000000001",
			}}

			// 首次回执先完成短信终态，注入的瞬时错误使任务仍停在 ambiguous。
			serialService.handleSMSSendResult(result)
			storedMessage, err := messageService.Get(ctx, requestID)
			if err != nil {
				t.Fatal(err)
			}
			if storedMessage.Status != test.messageStatus {
				t.Fatalf("message status=%q, want %q", storedMessage.Status, test.messageStatus)
			}
			var storedTask models.ScheduledTask
			if err := db.First(&storedTask, "id = ?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if storedTask.LastRunStatus != models.LastRunStatusAmbiguous {
				t.Fatalf("task unexpectedly advanced after injected error: %q", storedTask.LastRunStatus)
			}

			// 重复回执的短信 CAS 必然不命中；应读取已持久化的
			// 终态幂等补账，而不是相信重放帧中的值。
			serialService.handleSMSSendResult(result)
			if err := db.First(&storedTask, "id = ?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if storedTask.LastRunStatus != test.taskStatus {
				t.Fatalf("reconciled task status=%q, want %q", storedTask.LastRunStatus, test.taskStatus)
			}
			if updateCalls != 2 {
				t.Fatalf("scheduled task updater calls=%d, want 2", updateCalls)
			}
		})
	}
}

func TestSMSSendTimeoutReconcilesExistingTerminalTaskState(t *testing.T) {
	for _, test := range []struct {
		name          string
		messageStatus models.MessageStatus
		taskStatus    models.LastRunStatus
	}{
		{name: "sent", messageStatus: models.MessageStatusSent, taskStatus: models.LastRunStatusSuccess},
		{name: "failed", messageStatus: models.MessageStatusFailed, taskStatus: models.LastRunStatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newSchedulerTestDB(t, "timeout_terminal_reconcile_"+test.name)
			messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
			taskRepo := repo.NewScheduledTaskRepo(db)
			serialService := NewSerialService(
				zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
			)
			ctx := context.Background()
			requestID := "timeout-" + test.name
			task := models.ScheduledTask{ID: "task-" + test.name, Name: test.name}
			if err := db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.TextMessage{
				ID: requestID, Type: models.MessageTypeOutgoing, Status: test.messageStatus,
			}).Error; err != nil {
				t.Fatal(err)
			}
			if err := taskRepo.ClaimExecution(ctx, task.ID, requestID, 1234); err != nil {
				t.Fatal(err)
			}
			serialService.SetScheduledTaskStatusUpdater(taskRepo.UpdateLastRunStatusByMsgId)

			// 复现回执已完成短信终态，但对应任务写入丢失后，
			// 超时线程迟到的顺序。超时 CAS 不命中时仍必须补账。
			serialService.handleSMSSendTimeout(requestID)

			var storedTask models.ScheduledTask
			if err := db.First(&storedTask, "id = ?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if storedTask.LastRunStatus != test.taskStatus {
				t.Fatalf("task status=%q, want %q", storedTask.LastRunStatus, test.taskStatus)
			}
			storedMessage, err := messageService.Get(ctx, requestID)
			if err != nil {
				t.Fatal(err)
			}
			if storedMessage.Status != test.messageStatus {
				t.Fatalf("timeout overwrote terminal message: %q", storedMessage.Status)
			}
		})
	}
}

func TestSMSSendTimeoutClosesSendingAsAmbiguous(t *testing.T) {
	db := newSchedulerTestDB(t, "timeout_closes_sending")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
	)
	const requestID = "timeout-pending"
	if err := db.Create(&models.TextMessage{
		ID: requestID, Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var gotStatus models.LastRunStatus
	serialService.SetScheduledTaskStatusUpdater(func(
		_ context.Context, _ string, status models.LastRunStatus,
	) error {
		gotStatus = status
		return nil
	})

	serialService.handleSMSSendTimeout(requestID)
	stored, err := messageService.Get(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.MessageStatusAmbiguous || !stored.Ambiguous || stored.SendError != "result_timeout" {
		t.Fatalf("timeout message state=%+v", stored)
	}
	if gotStatus != models.LastRunStatusAmbiguous {
		t.Fatalf("timeout task status=%q, want ambiguous", gotStatus)
	}
}

func TestSMSSendTimeoutDatabaseErrorSchedulesRetryWithoutRestoringFlymode(t *testing.T) {
	db := newSchedulerTestDB(t, "timeout_database_retry")
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "device", "Air780", messageService, nil, nil,
	)
	const requestID = "timeout-database-error"
	// 只需确认数据库失败时恢复令牌仍在；不会在本测试中
	// 真正执行恢复，因此哨兵值不需要构造完整设备状态。
	serialService.restoreFlymodeByMsg.Store(requestID, "restore-token")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	serialService.handleSMSSendTimeout(requestID)
	if _, ok := serialService.pendingSMSTimers.Load(requestID); !ok {
		t.Fatal("database failure did not schedule a timeout persistence retry")
	}
	if _, ok := serialService.restoreFlymodeByMsg.Load(requestID); !ok {
		t.Fatal("database failure restored flymode before message state was persisted")
	}
	serialService.stopSMSSendTimeout(requestID)
}
