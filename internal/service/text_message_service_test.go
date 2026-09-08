package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestMessageQueriesRequireSIMScope(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "message_scope_required")
	ctx := context.Background()
	if err := db.Create(&models.TextMessage{ID: "one", SIMID: "iccid:one", Content: "keep"}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := messageService.GetStats(ctx, MessageScope{}); !errors.Is(err, ErrSIMIdentityRequired) {
		t.Fatalf("GetStats() err=%v, want ErrSIMIdentityRequired", err)
	}
	if err := messageService.Clear(ctx, MessageScope{}); !errors.Is(err, ErrSIMIdentityRequired) {
		t.Fatalf("Clear() err=%v, want ErrSIMIdentityRequired", err)
	}
	var count int64
	if err := db.Model(&models.TextMessage{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("unsafe clear changed data: count=%d err=%v", count, err)
	}
}

func TestMessageScopeSeparatesSIMAndLegacyData(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "message_scope_separation")
	ctx := context.Background()
	rows := []models.TextMessage{
		{ID: "one", SIMID: "iccid:one", Content: "one"},
		{ID: "two", SIMID: "iccid:two", Content: "two"},
		{ID: "legacy", SIMID: "", Content: "legacy"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	stats, err := messageService.GetStats(ctx, MessageScope{SIMID: "iccid:one"})
	if err != nil || stats.TotalCount != 1 {
		t.Fatalf("SIM stats=%+v err=%v", stats, err)
	}
	legacyStats, err := messageService.GetStats(ctx, MessageScope{SIMID: UnassignedSIMID})
	if err != nil || legacyStats.TotalCount != 1 {
		t.Fatalf("legacy stats=%+v err=%v", legacyStats, err)
	}
	if err := messageService.Delete(ctx, "one", MessageScope{SIMID: "iccid:two"}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("cross-SIM Delete() err=%v, want record not found", err)
	}
	if err := db.First(&models.TextMessage{}, "id = ?", "one").Error; err != nil {
		t.Fatalf("cross-SIM delete removed message: %v", err)
	}
}

func TestSendTimeoutCannotOverwriteFinalResult(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "message_timeout_cas")
	ctx := context.Background()
	rows := []models.TextMessage{
		{ID: "pending", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending},
		{ID: "complete", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := messageService.MarkSendResultTimeout(ctx, "pending")
	if err != nil || !updated {
		t.Fatalf("pending timeout updated=%v err=%v", updated, err)
	}
	updated, err = messageService.MarkSendResultTimeout(ctx, "complete")
	if err != nil || updated {
		t.Fatalf("completed timeout updated=%v err=%v", updated, err)
	}
	complete, err := messageService.Get(ctx, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != models.MessageStatusSent || complete.Ambiguous {
		t.Fatalf("timeout overwrote final result: %+v", complete)
	}
}

func TestSendResultCannotOverwriteTerminalState(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "message_result_cas")
	ctx := context.Background()
	rows := []models.TextMessage{
		{ID: "complete", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent, Submitted: true},
		{ID: "uncertain", Type: models.MessageTypeOutgoing, Status: models.MessageStatusAmbiguous, Ambiguous: true},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	updated, err := messageService.UpdateSendResultById(
		ctx, "complete", models.MessageStatusAmbiguous, false, true, "serial_write_uncertain",
	)
	if err != nil || updated {
		t.Fatalf("terminal rewrite updated=%v err=%v", updated, err)
	}
	updated, err = messageService.UpdateSendResultById(
		ctx, "uncertain", models.MessageStatusSent, true, false, "",
	)
	if err != nil || !updated {
		t.Fatalf("ambiguous resolution updated=%v err=%v", updated, err)
	}

	complete, err := messageService.Get(ctx, "complete")
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != models.MessageStatusSent || complete.Ambiguous {
		t.Fatalf("terminal result was overwritten: %+v", complete)
	}
}

func TestPendingOutgoingEvidenceCannotBeDeleted(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "message_pending_delete_guard")
	ctx := context.Background()
	scope := MessageScope{SIMID: "iccid:one"}
	rows := []models.TextMessage{
		{ID: "sending", SIMID: scope.SIMID, To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending},
		{ID: "ambiguous", SIMID: scope.SIMID, To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusAmbiguous, Ambiguous: true},
		{ID: "sent-awaiting-task", SIMID: scope.SIMID, To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent},
		{ID: "sent", SIMID: scope.SIMID, To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent},
		{ID: "incoming", SIMID: scope.SIMID, From: "10086", Type: models.MessageTypeIncoming, Status: models.MessageStatusReceived},
		{ID: "other-sim", SIMID: "iccid:two", To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSent},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ScheduledTask{
		ID: "task-awaiting-message-reconcile", LastMsgId: "sent-awaiting-task",
		LastRunStatus: models.LastRunStatusAmbiguous,
	}).Error; err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"sending", "ambiguous", "sent-awaiting-task"} {
		if err := messageService.Delete(ctx, id, scope); !errors.Is(err, ErrSMSOperationPending) {
			t.Fatalf("Delete(%q) err=%v, want ErrSMSOperationPending", id, err)
		}
	}
	if err := messageService.DeleteConversation(ctx, scope, "10086"); !errors.Is(err, ErrSMSOperationPending) {
		t.Fatalf("DeleteConversation() err=%v, want ErrSMSOperationPending", err)
	}
	if err := messageService.Clear(ctx, scope); !errors.Is(err, ErrSMSOperationPending) {
		t.Fatalf("Clear() err=%v, want ErrSMSOperationPending", err)
	}

	var count int64
	if err := db.Model(&models.TextMessage{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != int64(len(rows)) {
		t.Fatalf("guarded deletion removed evidence: count=%d, want %d", count, len(rows))
	}

	if err := db.Model(&models.TextMessage{}).
		Where("id IN ?", []string{"sending", "ambiguous"}).
		Updates(map[string]any{"status": models.MessageStatusFailed, "ambiguous": false}).Error; err != nil {
		t.Fatal(err)
	}
	if err := messageService.DeleteConversation(ctx, scope, "10086"); !errors.Is(err, ErrSMSOperationPending) {
		t.Fatalf("DeleteConversation() with unresolved task evidence err=%v, want ErrSMSOperationPending", err)
	}
	if err := messageService.Clear(ctx, scope); !errors.Is(err, ErrSMSOperationPending) {
		t.Fatalf("Clear() with unresolved task evidence err=%v, want ErrSMSOperationPending", err)
	}
	if err := db.Model(&models.ScheduledTask{}).
		Where("id = ?", "task-awaiting-message-reconcile").
		Update("last_run_status", models.LastRunStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	if err := messageService.DeleteConversation(ctx, scope, "10086"); err != nil {
		t.Fatalf("DeleteConversation() after terminal results: %v", err)
	}
	var remaining []models.TextMessage
	if err := db.Order("id").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "other-sim" {
		t.Fatalf("conversation delete crossed SIM scope or left terminal rows: %+v", remaining)
	}
}

func newTestTextMessageService(t *testing.T, name string) (*TextMessageService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN(name)), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.TextMessage{}, &models.ScheduledTask{}); err != nil {
		t.Fatal(err)
	}
	return NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db)), db
}
