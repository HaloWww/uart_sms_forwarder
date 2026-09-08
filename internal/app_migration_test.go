package internal

import (
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type legacyTextMessage struct {
	ID       string `gorm:"primaryKey"`
	DeviceID string
	Content  string
}

func (legacyTextMessage) TableName() string { return "text_messages" }

type legacyScheduledTask struct {
	ID       string `gorm:"primaryKey"`
	DeviceID string
	Name     string
	Enabled  bool
}

func (legacyScheduledTask) TableName() string { return "scheduled_tasks" }

func TestSIMMigrationKeepsAmbiguousLegacyRowsUnassigned(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueMigrationTestSQLiteDSN("app_migration_test")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&legacyTextMessage{}, &legacyScheduledTask{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyTextMessage{ID: "message-1", DeviceID: "default", Content: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyScheduledTask{ID: "task-1", DeviceID: "default", Name: "legacy", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}

	if err := autoMigrate(db); err != nil {
		t.Fatal(err)
	}
	if err := disableUnboundScheduledTasks(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.TextMessage{
		ID: "interrupted-send", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := markInterruptedSendingMessages(db); err != nil {
		t.Fatal(err)
	}

	var message models.TextMessage
	if err := db.First(&message, "id = ?", "message-1").Error; err != nil {
		t.Fatal(err)
	}
	if message.SIMID != "" {
		t.Fatalf("legacy message was ambiguously assigned to %q", message.SIMID)
	}
	var task models.ScheduledTask
	if err := db.First(&task, "id = ?", "task-1").Error; err != nil {
		t.Fatal(err)
	}
	if task.SIMID != "" {
		t.Fatalf("legacy scheduled task was ambiguously assigned to %q", task.SIMID)
	}
	if task.Enabled {
		t.Fatal("unbound legacy scheduled task was not disabled")
	}
	var interrupted models.TextMessage
	if err := db.First(&interrupted, "id = ?", "interrupted-send").Error; err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != models.MessageStatusAmbiguous || !interrupted.Ambiguous ||
		interrupted.SendError != "process_restarted_before_result" {
		t.Fatalf("interrupted send was not recovered conservatively: %+v", interrupted)
	}
}

func TestReconcileScheduledTaskResultsAfterRestart(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueMigrationTestSQLiteDSN("task_result_reconcile")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.TextMessage{}, &models.ScheduledTask{}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id         string
		msgStatus  *models.MessageStatus
		wantStatus models.LastRunStatus
	}{
		{id: "missing", wantStatus: models.LastRunStatusFailed},
		{id: "sent", msgStatus: messageStatusPointer(models.MessageStatusSent), wantStatus: models.LastRunStatusSuccess},
		{id: "failed", msgStatus: messageStatusPointer(models.MessageStatusFailed), wantStatus: models.LastRunStatusFailed},
		{id: "ambiguous", msgStatus: messageStatusPointer(models.MessageStatusAmbiguous), wantStatus: models.LastRunStatusAmbiguous},
	}
	for _, test := range cases {
		task := models.ScheduledTask{
			ID: "task-" + test.id, LastMsgId: "msg-" + test.id,
			LastRunStatus: models.LastRunStatusAmbiguous,
		}
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		if test.msgStatus != nil {
			if err := db.Create(&models.TextMessage{
				ID: task.LastMsgId, Type: models.MessageTypeOutgoing, Status: *test.msgStatus,
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := reconcileScheduledTaskResults(db); err != nil {
		t.Fatal(err)
	}
	for _, test := range cases {
		var task models.ScheduledTask
		if err := db.First(&task, "id = ?", "task-"+test.id).Error; err != nil {
			t.Fatal(err)
		}
		if task.LastRunStatus != test.wantStatus {
			t.Errorf("%s status=%q, want %q", test.id, task.LastRunStatus, test.wantStatus)
		}
	}
}

func messageStatusPointer(status models.MessageStatus) *models.MessageStatus {
	return &status
}

func uniqueMigrationTestSQLiteDSN(name string) string {
	return "file:" + name + "-" + uuid.NewString() + "?mode=memory&cache=shared"
}
