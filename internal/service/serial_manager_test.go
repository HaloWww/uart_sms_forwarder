package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"go.uber.org/zap"
)

func TestSerialManagerLegacyConfigCreatesDefaultDevice(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, name := manager.DefaultDevice()
	if id != "default" || name != "Air780" {
		t.Fatalf("DefaultDevice() = %q, %q", id, name)
	}
	status, err := manager.GetDeviceStatus("")
	if err != nil {
		t.Fatal(err)
	}
	if status.DeviceID != "default" || status.PortName != "COM9" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestSerialManagerRejectsDuplicateDeviceIDs(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "same", Name: "one"},
		{ID: "same", Name: "two"},
	}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate device ID error")
	}
}

func TestSerialManagerRejectsProtocolMarkerInSMS(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SendSMS("", "10086", "bad :CMD_END content"); err == nil {
		t.Fatal("expected reserved marker error")
	}
}

func TestSerialManagerRequiresUniqueExplicitPortsForMultipleDevices(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Port: "COM9"},
		{ID: "two", Port: "COM9"},
	}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate port error")
	}
}

func TestSerialManagerListsMultipleDevices(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Name: "主卡", Port: "COM8"},
		{ID: "two", Name: "备用卡", Port: "COM9"},
	}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	statuses := manager.GetStatuses()
	if len(statuses) != 2 || statuses[0].DeviceID != "one" || statuses[1].DeviceID != "two" {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
}

func TestSerialManagerRoutesByICCIDAfterCardsSwapPorts(t *testing.T) {
	manager := newTwoDeviceManager(t)
	setTestSIM(manager.services["one"], "8986000000000000001")
	setTestSIM(manager.services["two"], "8986000000000000002")

	serviceOne, _, err := manager.resolveSIM("iccid:8986000000000000001")
	if err != nil || serviceOne.DeviceID() != "one" {
		t.Fatalf("before swap resolved to %v, err=%v", serviceOne, err)
	}

	setTestSIM(manager.services["one"], "8986000000000000002")
	setTestSIM(manager.services["two"], "8986000000000000001")
	serviceTwo, identity, err := manager.resolveSIM("iccid:8986000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if serviceTwo.DeviceID() != "two" || identity.ICCID != "8986000000000000001" {
		t.Fatalf("after swap resolved to device=%s identity=%+v", serviceTwo.DeviceID(), identity)
	}
}

func TestSerialManagerFailsClosedOnDuplicateICCID(t *testing.T) {
	manager := newTwoDeviceManager(t)
	setTestSIM(manager.services["one"], "8986000000000000001")
	setTestSIM(manager.services["two"], "8986000000000000001")

	if _, _, err := manager.resolveSIM("iccid:8986000000000000001"); !errors.Is(err, ErrSIMConflict) {
		t.Fatalf("resolveSIM() err=%v, want ErrSIMConflict", err)
	}
}

func TestSerialManagerFailsClosedOnVerifiedAndFlymodeClaims(t *testing.T) {
	manager := newTwoDeviceManager(t)
	simID := "iccid:8986000000000000001"
	setTestSIM(manager.services["one"], "8986000000000000001")
	setTestFlymodeClaim(manager.services["two"], simID)

	if _, _, err := manager.resolveSIM(simID); !errors.Is(err, ErrSIMConflict) {
		t.Fatalf("verified+flymode resolveSIM() err=%v, want ErrSIMConflict", err)
	}
}

func TestSerialManagerFailsClosedOnTwoFlymodeClaims(t *testing.T) {
	manager := newTwoDeviceManager(t)
	simID := "iccid:8986000000000000001"
	setTestFlymodeClaim(manager.services["one"], simID)
	setTestFlymodeClaim(manager.services["two"], simID)

	if _, _, err := manager.resolveSIM(simID); !errors.Is(err, ErrSIMConflict) {
		t.Fatalf("flymode+flymode resolveSIM() err=%v, want ErrSIMConflict", err)
	}
}

func TestSerialManagerAllowsVerifiedSIMWhenAnotherDeviceHasNoActiveSlot(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "no_active_slot_does_not_block_send")
	manager, err := NewSerialManager(
		zap.NewNop(), db, config.SerialConfig{Devices: []config.SerialDeviceConfig{
			{ID: "one", Name: "有卡设备", Port: "COM8"},
			{ID: "two", Name: "无卡设备", Port: "COM9"},
		}}, messageService, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	const iccid = "8986000000000000001"
	const simID = "iccid:" + iccid
	const requestID = "cf880a84-c627-45d6-b332-389e8c7654b4"
	setTestSIM(manager.services["one"], iccid)
	setTestEmptySIMState(manager.services["two"], "no_active_slot")
	targetPort := &capturingSerialPort{}
	manager.services["one"].port = targetPort

	messageID, err := manager.SendSMSWithRequestID(simID, "10086", "test", requestID)
	if err != nil {
		t.Fatalf("SendSMSWithRequestID() error = %v", err)
	}
	if messageID != requestID {
		t.Fatalf("messageID = %q, want %q", messageID, requestID)
	}
	if frame := targetPort.String(); !strings.Contains(frame, `"expected_iccid":"`+iccid+`"`) {
		t.Fatalf("verified SIM was not sent through the target device: %q", frame)
	}
	manager.services["one"].stopSMSSendTimeout(requestID)
}

func TestSerialManagerRejectsUnknownSIM(t *testing.T) {
	manager := newTwoDeviceManager(t)
	if _, _, err := manager.resolveSIM("iccid:missing"); !errors.Is(err, ErrSIMUnknown) {
		t.Fatalf("resolveSIM() err=%v, want ErrSIMUnknown", err)
	}
}

func TestSerialManagerRejectsReadOnlyLegacyIdentity(t *testing.T) {
	manager := newTwoDeviceManager(t)
	if _, _, err := manager.resolveSIM(UnassignedSIMID); !errors.Is(err, ErrSIMIdentityRequired) {
		t.Fatalf("resolveSIM(legacy) err=%v, want ErrSIMIdentityRequired", err)
	}
}

func TestSerialManagerSendSMSIsIdempotentBeforeUART(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "manual_sms_idempotency")
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

	const requestID = "f851ccee-2c10-4ebb-9992-3532dc5f31b8"
	const simID = "iccid:8986000000000000001"
	messageID, err := manager.SendSMSWithRequestID(simID, "10086", "same message", requestID)
	if err != nil {
		t.Fatalf("first SendSMSWithRequestID() error = %v", err)
	}
	if messageID != requestID {
		t.Fatalf("first messageID = %q, want %q", messageID, requestID)
	}
	firstFrame := port.String()
	if !strings.Contains(firstFrame, `"action":"send_sms"`) {
		t.Fatalf("first request did not write an SMS frame: %q", firstFrame)
	}

	retryID, err := manager.SendSMSWithRequestID(simID, "10086", "same message", requestID)
	if err != nil {
		t.Fatalf("same request retry error = %v", err)
	}
	if retryID != requestID {
		t.Fatalf("retry messageID = %q, want %q", retryID, requestID)
	}
	if got := port.String(); got != firstFrame {
		t.Fatalf("same request retry wrote UART again: before=%q after=%q", firstFrame, got)
	}

	if _, err := manager.SendSMSWithRequestID(simID, "10086", "changed message", requestID); !errors.Is(err, ErrSMSRequestConflict) {
		t.Fatalf("changed request error = %v, want ErrSMSRequestConflict", err)
	}
	if got := port.String(); got != firstFrame {
		t.Fatalf("conflicting request wrote UART: before=%q after=%q", firstFrame, got)
	}

	var count int64
	if err := db.Model(&models.TextMessage{}).Where("id = ?", requestID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored message count = %d, want 1", count)
	}
	var stored models.TextMessage
	if err := db.First(&stored, "id = ?", requestID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DeviceID != "default" || stored.ICCID != "8986000000000000001" {
		t.Fatalf("claimed message identity was not completed: %+v", stored)
	}
	serialService.stopSMSSendTimeout(requestID)
}

func TestSerialManagerRejectsInvalidSMSRequestID(t *testing.T) {
	manager := newTwoDeviceManager(t)
	if _, err := manager.SendSMSWithRequestID(
		"iccid:8986000000000000001", "10086", "test", "not-a-uuid",
	); !errors.Is(err, ErrSMSRequestIDInvalid) {
		t.Fatalf("SendSMSWithRequestID() error = %v, want ErrSMSRequestIDInvalid", err)
	}
}

func TestSerialManagerReplaysFailedAndAmbiguousRequestsWithoutUART(t *testing.T) {
	messageService, db := newTestTextMessageService(t, "manual_sms_replay_status")
	if err := db.AutoMigrate(&models.SIMProfile{}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(
		zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, messageService, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	const simID = "iccid:8986000000000000001"
	const failedID = "aed5766f-a147-4f15-9477-b3d0dd225bb2"
	if _, err := manager.SendSMSWithRequestID(simID, "10086", "failed request", failedID); !errors.Is(err, ErrSIMUnknown) {
		t.Fatalf("initial offline request error = %v, want ErrSIMUnknown", err)
	}
	serialService := manager.services["default"]
	setTestSIM(serialService, "8986000000000000001")
	port := &capturingSerialPort{}
	serialService.port = port
	messageID, err := manager.SendSMSWithRequestID(simID, "10086", "failed request", failedID)
	if !errors.Is(err, ErrSMSRequestPreviouslyFailed) || messageID != failedID {
		t.Fatalf("failed replay = (%q, %v)", messageID, err)
	}
	if port.Len() != 0 {
		t.Fatalf("failed replay wrote UART: %q", port.String())
	}

	const ambiguousID = "4a005a84-5066-4b90-a6e0-f4ea708904df"
	ambiguous := &models.TextMessage{
		ID: ambiguousID, SIMID: simID, To: "10010", Content: "ambiguous request",
		Type: models.MessageTypeOutgoing, Status: models.MessageStatusAmbiguous,
		Ambiguous: true,
	}
	if err := db.Create(ambiguous).Error; err != nil {
		t.Fatal(err)
	}
	messageID, err = manager.SendSMSWithRequestID(simID, "10010", "ambiguous request", ambiguousID)
	if !errors.Is(err, ErrSMSSubmissionAmbiguous) || messageID != ambiguousID {
		t.Fatalf("ambiguous replay = (%q, %v)", messageID, err)
	}
	if port.Len() != 0 {
		t.Fatalf("ambiguous replay wrote UART: %q", port.String())
	}
}

func newTwoDeviceManager(t *testing.T) *SerialManager {
	t.Helper()
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Name: "设备一", Port: "COM8"},
		{ID: "two", Name: "设备二", Port: "COM9"},
	}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func setTestSIM(serialService *SerialService, iccid string) {
	status := &StatusData{SIMID: makeSIMID(iccid)}
	status.Mobile.Iccid = iccid
	status.Mobile.Imsi = "460001234567890"
	status.Mobile.Imei = "860000000000000"
	status.Mobile.SimReady = true
	status.IdentityValid = true
	status.Version = minimumSIMIdentityProtocolVersion
	serialService.deviceCache.Set(CacheKeyDeviceStatus, status, CacheTTL)
	serialService.setConnected(true)
}

func setTestEmptySIMState(serialService *SerialService, identityState string) {
	status := &StatusData{
		Version:       minimumSIMIdentityProtocolVersion,
		IdentityState: identityState,
	}
	serialService.deviceCache.Set(CacheKeyDeviceStatus, status, CacheTTL)
	serialService.setConnected(true)
}

func setTestFlymodeClaim(serialService *SerialService, simID string) {
	serialService.setConnected(true)
	serialService.flyMode.Store(true)
	serialService.setFlymodeOwner(simID)
}
