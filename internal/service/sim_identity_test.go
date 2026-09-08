package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"github.com/glebarez/sqlite"
	"go.bug.st/serial"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestMakeSIMIDRequiresICCID(t *testing.T) {
	if got := makeSIMID(""); got != "" {
		t.Fatalf("makeSIMID() = %q; empty ICCID must not get a fallback identity", got)
	}
	if got := makeSIMID(" 8986000000000000001 "); got != "iccid:8986000000000000001" {
		t.Fatalf("makeSIMID() = %q", got)
	}
}

func TestSIMIdentityProtocolVersion(t *testing.T) {
	for _, version := range []string{"1.3.0", "1.3.1", "2.0.0", "v1.4.0"} {
		if !supportsSIMIdentityProtocol(version) {
			t.Errorf("supportsSIMIdentityProtocol(%q) = false", version)
		}
	}
	for _, version := range []string{"", "1.1.9", "unknown", "1.x.0"} {
		if supportsSIMIdentityProtocol(version) {
			t.Errorf("supportsSIMIdentityProtocol(%q) = true", version)
		}
	}
}

func TestStatusWithoutReadySIMDoesNotExposeRoute(t *testing.T) {
	serialService := NewSerialService(zap.NewNop(), config.SerialConfig{Port: "COM9"}, "one", "设备一", nil, nil, nil)
	serialService.setConnected(true)
	serialService.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"mobile":{"sim_ready":false,"iccid":"8986000000000000001","imsi":"460001234567890","imei":"860000000000000"}
	}`})

	status, err := serialService.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SIMID != "" {
		t.Fatalf("unready SIM exposed route %q", status.SIMID)
	}
}

func TestUnverifiedStatusCannotPreserveReportedSIMRoute(t *testing.T) {
	serialService := NewSerialService(zap.NewNop(), config.SerialConfig{Port: "COM9"}, "one", "设备一", nil, nil, nil)
	serialService.setConnected(true)
	serialService.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"sim_id":"iccid:8986000000000000001",
		"identity_valid":false,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001","imsi":"460001234567890","imei":"860000000000000"}
	}`})

	status, err := serialService.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SIMID != "" {
		t.Fatalf("unverified status preserved reported route %q", status.SIMID)
	}
}

func TestKnownSIMRemainsListedWhenOffline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("sim_identity_test")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	status := &StatusData{DeviceID: "default", DeviceName: "Air780", SIMID: "iccid:8986000000000000001", IMEI: "860000000000000", PortName: "COM9", Connected: true}
	status.Mobile.SimReady = true
	status.Mobile.Iccid = "8986000000000000001"
	status.Mobile.Imsi = "460001234567890"
	status.Mobile.Imei = "860000000000000"
	status.Mobile.Number = "13800138000"
	status.IdentityValid = true
	status.Version = minimumSIMIdentityProtocolVersion
	manager.observeStatus(status)
	var profile models.SIMProfile
	if err := db.First(&profile, "id = ?", status.SIMID).Error; err != nil {
		t.Fatal(err)
	}
	if profile.ICCID != status.Mobile.Iccid || profile.LastDeviceID != status.DeviceID ||
		profile.LastPort != status.PortName || profile.LastSeenAt == 0 ||
		profile.Number != status.Mobile.Number {
		t.Fatalf("observed SIM profile was not fully updated: %+v", profile)
	}
	persistedNumber := profile.Number
	status.Mobile.Number = ""
	manager.observeStatus(status)
	if err := db.First(&profile, "id = ?", status.SIMID).Error; err != nil {
		t.Fatal(err)
	}
	if profile.Number != persistedNumber {
		t.Fatalf("empty MSISDN erased persisted number: %+v", profile)
	}

	sims, err := manager.GetSIMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sims) != 1 || sims[0].SIMID != status.SIMID || sims[0].Online ||
		sims[0].Number != persistedNumber {
		t.Fatalf("offline persisted SIMs = %+v", sims)
	}
	if err := manager.ValidateSIM(context.Background(), status.SIMID); err != nil {
		t.Fatalf("ValidateSIM() rejected known offline SIM: %v", err)
	}
}

func TestSIMRoutingWaitsForEveryConnectedDeviceToSettle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("sim_topology_settle")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Port: "COM1"}, {ID: "two", Port: "COM2"},
	}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	one := manager.services["one"]
	two := manager.services["two"]
	one.setConnected(true)
	two.setConnected(true)
	one.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","version":"1.3.0","identity_valid":true,
		"identity_state":"verified",
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	const simID = "iccid:8986000000000000001"

	if _, _, err := manager.resolveLiveSIM(simID); !errors.Is(err, ErrSIMTopologyUnsettled) {
		t.Fatalf("unsettled second device did not close routing: %v", err)
	}

	two.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","version":"1.3.0","identity_valid":false,
		"identity_state":"no_sim","mobile":{"sim_ready":false}
	}`})
	service, identity, err := manager.resolveLiveSIM(simID)
	if err != nil {
		t.Fatalf("settled empty second device kept route closed: %v", err)
	}
	if service != one || identity.SIMID != simID {
		t.Fatalf("resolved service=%p identity=%+v", service, identity)
	}
}

func TestDuplicateSIMInboundMessageKeepsFrozenIdentityWithConflictAudit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("duplicate_inbound_route")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Port: "COM1"}, {ID: "two", Port: "COM2"},
	}}, messageService, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	statusJSON := `{
		"type":"status_response","version":"1.3.0","identity_valid":true,
		"identity_state":"verified",
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`
	for _, id := range []string{"one", "two"} {
		manager.services[id].setConnected(true)
		manager.services[id].handleStatusResponse(&ParsedMessage{JSON: statusJSON})
	}
	if _, _, err := manager.resolveLiveSIM("iccid:8986000000000000001"); !errors.Is(err, ErrSIMConflict) {
		t.Fatalf("duplicate route error = %v, want ErrSIMConflict", err)
	}

	manager.services["one"].handleIncomingSMS(&ParsedMessage{JSON: `{
		"type":"incoming_sms","message_id":"duplicate-claim","identity_valid":true,
		"identity_revision":1,"iccid":"8986000000000000001","imei":"860000000000001",
		"from":"10086","content":"test"
	}`})
	var stored models.TextMessage
	if err := db.Where("source_id = ?", "duplicate-claim").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SIMID != "iccid:8986000000000000001" || !stored.IdentityConflict {
		t.Fatalf("duplicate ICCID inbound lost frozen identity or conflict flag: %+v", stored)
	}
	if stored.ICCID != "8986000000000000001" || stored.DeviceID != "one" ||
		!strings.Contains(stored.SendError, ErrSIMConflict.Error()) {
		t.Fatalf("conflict audit record lost provenance: %+v", stored)
	}
}

func TestUnverifiedInboundPersistsIdentityError(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("unverified_inbound_identity_error")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "one", "设备一", messageService, nil, nil,
	)
	serialService.handleIncomingSMS(&ParsedMessage{JSON: `{
		"type":"incoming_sms","message_id":"unverified-source",
		"identity_valid":false,"identity_error":"incoming_identity_changed",
		"iccid":"8986000000000000001","imei":"860000000000001",
		"from":"10086","content":"test"
	}`})

	var stored models.TextMessage
	if err := db.Where("source_id = ?", "unverified-source").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SIMID != "" || stored.ICCID != "" {
		t.Fatalf("unverified inbound unexpectedly received SIM ownership: %+v", stored)
	}
	if stored.SendError != "incoming_identity_changed" || stored.IMEI != "860000000000001" {
		t.Fatalf("unverified inbound lost identity failure audit: %+v", stored)
	}
}

func TestSMSSendResultRequiresVerifiedMatchingICCID(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("sms_result_identity")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	messageService := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
	serialService := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", messageService, nil, nil)

	for _, test := range []struct {
		name          string
		success       bool
		submitted     bool
		actualICCID   string
		identityValid bool
		wantStatus    models.MessageStatus
		wantAmbiguous bool
	}{
		{name: "verified match", success: true, submitted: true, actualICCID: "8986000000000000001", identityValid: true, wantStatus: models.MessageStatusSent},
		{name: "unverified after submit", success: true, submitted: true, actualICCID: "8986000000000000001", identityValid: false, wantStatus: models.MessageStatusAmbiguous, wantAmbiguous: true},
		{name: "different SIM after submit", success: true, submitted: true, actualICCID: "8986000000000000002", identityValid: true, wantStatus: models.MessageStatusAmbiguous, wantAmbiguous: true},
		{name: "rejected before submit", success: false, submitted: false, actualICCID: "8986000000000000002", identityValid: true, wantStatus: models.MessageStatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := "result-" + test.name
			record := &models.TextMessage{
				ID: id, SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001",
				To: "10086", Type: models.MessageTypeOutgoing, Status: models.MessageStatusSending,
			}
			if err := db.Create(record).Error; err != nil {
				t.Fatal(err)
			}
			serialService.handleSMSSendResult(&ParsedMessage{Payload: map[string]interface{}{
				"success":        test.success,
				"submitted":      test.submitted,
				"ambiguous":      false,
				"request_id":     id,
				"to":             "10086",
				"iccid":          test.actualICCID,
				"identity_valid": test.identityValid,
			}})
			updated, err := messageService.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Status != test.wantStatus {
				t.Fatalf("status=%q, want %q", updated.Status, test.wantStatus)
			}
			if updated.Ambiguous != test.wantAmbiguous {
				t.Fatalf("ambiguous=%v, want %v", updated.Ambiguous, test.wantAmbiguous)
			}
		})
	}
}

func TestFlymodeUsesLastVerifiedConnectionOnlyForWakeup(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("flymode_identity_route")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	const simID = "iccid:8986000000000000001"
	if err := db.Create(&models.SIMProfile{
		ID: simID, ICCID: "8986000000000000001", LastDeviceID: "default", LastSeenAt: 20,
	}).Error; err != nil {
		t.Fatal(err)
	}
	const oldSIMID = "iccid:8986000000000000000"
	if err := db.Create(&models.SIMProfile{
		ID: oldSIMID, ICCID: "8986000000000000000", LastDeviceID: "default", LastSeenAt: 10,
	}).Error; err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	serialService := manager.services["default"]
	serialService.setConnected(true)
	serialService.flyMode.Store(true)
	serialService.setFlymodeOwner(simID)
	status := &StatusData{Version: minimumSIMIdentityProtocolVersion, IdentityState: "stale_flymode"}
	status.Mobile.Flymode = true
	serialService.deviceCache.Set(CacheKeyDeviceStatus, status, CacheTTL)

	resolved, identity, err := manager.resolveSIM(simID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != serialService || identity.ICCID != "8986000000000000001" || identity.Verified {
		t.Fatalf("flymode fallback resolved service=%p identity=%+v", resolved, identity)
	}
	if _, _, err := manager.resolveSIM(oldSIMID); !errors.Is(err, ErrSIMOffline) {
		t.Fatalf("historical SIM reused stale flymode route: %v", err)
	}
	sims, err := manager.GetSIMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sims) != 2 {
		t.Fatalf("flymode SIM status=%+v", sims)
	}
	byID := make(map[string]SIMStatus, len(sims))
	for _, sim := range sims {
		byID[sim.SIMID] = sim
	}
	if !byID[simID].Online || !byID[simID].SendReady || byID[simID].CurrentStatus == nil {
		t.Fatalf("latest flymode SIM status=%+v", byID[simID])
	}
	if byID[oldSIMID].Online || byID[oldSIMID].SendReady {
		t.Fatalf("historical SIM incorrectly shown online=%+v", byID[oldSIMID])
	}
	serialService.resetPhysicalConnectionState()
	if _, _, err := manager.resolveSIM(simID); !errors.Is(err, ErrSIMOffline) {
		t.Fatalf("reconnected service reused old flymode owner: %v", err)
	}
}

func TestFlymodeOwnerFollowsSIMAcrossConfiguredPorts(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("flymode_owner_new_port")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.SIMProfile{}, &models.TextMessage{}); err != nil {
		t.Fatal(err)
	}
	const simID = "iccid:8986000000000000001"
	if err := db.Create(&models.SIMProfile{
		ID: simID, ICCID: "8986000000000000001", LastDeviceID: "one", LastPort: "COM1",
	}).Error; err != nil {
		t.Fatal(err)
	}
	manager, err := NewSerialManager(zap.NewNop(), db, config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Port: "COM1"}, {ID: "two", Port: "COM2"},
	}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	newPort := manager.services["two"]
	newPort.setConnected(true)
	newPort.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","version":"1.3.0",
		"flymode_owner_iccid":"8986000000000000001","flymode_source":"manual",
		"identity_state":"stale_flymode",
		"mobile":{"flymode":true,"sim_ready":false}
	}`})

	resolved, identity, err := manager.resolveSIM(simID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != newPort || identity.SIMID != simID || identity.ICCID != "8986000000000000001" {
		t.Fatalf("current Lua owner did not override historical port: service=%p identity=%+v", resolved, identity)
	}
}

func TestStatusRestoresFlymodeOwnerFromCurrentLuaConnection(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"flymode_owner_iccid":"8986000000000000001",
		"flymode_source":"automatic",
		"mobile":{"flymode":true,"sim_ready":false}
	}`})
	if !service.FlyMode() || service.FlymodeOwnerSIMID() != "iccid:8986000000000000001" {
		t.Fatalf("flymode owner was not restored: flymode=%v owner=%q",
			service.FlyMode(), service.FlymodeOwnerSIMID())
	}
	if !service.autoFlymodeActive.Load() {
		t.Fatal("automatic flymode source was not restored from Lua status")
	}
}

func TestManualFlymodeRestoreTokenRejectsStatusGenerationChange(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	expected := SIMIdentity{SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001"}
	token := service.newManualFlymodeRestoreToken(expected, service.manualFlymodeGen.Load())
	if err := service.canRestoreManualFlymode(token); err != nil {
		t.Fatalf("current restore token rejected: %v", err)
	}
	service.invalidateDeviceStatus(true)
	if err := service.canRestoreManualFlymode(token); err == nil {
		t.Fatal("stale restore token survived a SIM/connection status generation change")
	}
}

func TestMetadataOnlySIMEventKeepsStatusGeneration(t *testing.T) {
	service := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil,
	)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	before := service.statusEpoch.Load()

	service.handleSIMEvent(&ParsedMessage{Payload: map[string]any{
		"status":         "GET_NUMBER",
		"identity_valid": true,
		"iccid":          "8986000000000000001",
	}})
	if got := service.statusEpoch.Load(); got != before {
		t.Fatalf("metadata-only event advanced status generation: got %d, want %d", got, before)
	}

	service.handleSIMEvent(&ParsedMessage{Payload: map[string]any{
		"status":         "RDY",
		"identity_valid": true,
		"iccid":          "8986000000000000001",
	}})
	if got := service.statusEpoch.Load(); got != before+1 {
		t.Fatalf("identity-changing event generation = %d, want %d", got, before+1)
	}
}

func TestStatusResponseIdentityChangeStartsNewEpochAndResetsIdleTime(t *testing.T) {
	service := NewSerialService(
		zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil,
	)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	beforeEpoch := service.statusEpoch.Load()
	oldActivity := time.Now().Add(-time.Hour).UnixMilli()
	service.lastSMSActivityAt.Store(oldActivity)

	// 不经过 sim_event，直接复现下一份状态已经观察到另一张卡。
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000002"}
	}`})

	if got := service.statusEpoch.Load(); got != beforeEpoch+1 {
		t.Fatalf("status identity change epoch = %d, want %d", got, beforeEpoch+1)
	}
	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SIMID != "iccid:8986000000000000002" ||
		status.Mobile.Iccid != "8986000000000000002" {
		t.Fatalf("new status was lost while advancing epoch: %+v", status)
	}
	if got := service.lastSMSActivityAt.Load(); got <= oldActivity {
		t.Fatalf("status identity change did not reset idle activity: got %d", got)
	}
}

func TestManualFlymodeRestoreWaitsForEveryPendingSMS(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	port := &capturingSerialPort{}
	service.port = port
	autoRespondToControls(service, port, "ok", "")
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	expected := SIMIdentity{SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001"}
	token := service.newManualFlymodeRestoreToken(expected, service.manualFlymodeGen.Load())
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	service.pendingSMSTimers.Store("second-message", timer)

	retry, err := service.tryRestoreManualFlymode(token)
	if err != nil {
		t.Fatal(err)
	}
	if !retry || service.FlyMode() || port.Len() != 0 {
		t.Fatalf("restore with pending SMS: retry=%v flymode=%v frame=%q", retry, service.FlyMode(), port.String())
	}
	service.pendingSMSTimers.Delete("second-message")
	service.lastSMSActivityAt.Store(time.Now().Add(-manualFlymodeRestoreDelay).UnixMilli())
	retry, err = service.tryRestoreManualFlymode(token)
	if err != nil {
		t.Fatal(err)
	}
	if retry || !service.FlyMode() {
		t.Fatalf("restore after SMS completion: retry=%v flymode=%v", retry, service.FlyMode())
	}
	frame := port.String()
	for _, want := range []string{`"enabled":true`, `"expected_iccid":"8986000000000000001"`, `"source":"manual"`} {
		if !strings.Contains(frame, want) {
			t.Errorf("restore frame %q does not contain %q", frame, want)
		}
	}
}

func TestManualFlymodeRestoreRetriesDeviceBusyWithoutInvalidatingIdentity(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	port := &capturingSerialPort{}
	service.port = port
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	autoRespondToControls(service, port, "error", "sms_operation_pending")
	expected := SIMIdentity{SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001"}
	token := service.newManualFlymodeRestoreToken(expected, service.manualFlymodeGen.Load())
	beforeEpoch := service.statusEpoch.Load()

	retry, err := service.tryRestoreManualFlymode(token)
	if err != nil {
		t.Fatalf("tryRestoreManualFlymode() error = %v", err)
	}
	if !retry {
		t.Fatal("device busy response was treated as a permanent restore failure")
	}
	if service.statusEpoch.Load() != beforeEpoch {
		t.Fatal("device busy response invalidated an otherwise trustworthy identity")
	}
}

func TestManualFlymodeTakesOwnershipFromAutomaticWhileIdentityIsUnreadable(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	port := &capturingSerialPort{}
	service.port = port
	autoRespondToControls(service, port, "ok", "")
	service.setConnected(true)
	expected := SIMIdentity{SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001"}
	service.flyMode.Store(true)
	service.setFlymodeOwner(expected.SIMID)
	service.autoFlymodeActive.Store(true)
	beforeGeneration := service.manualFlymodeGen.Load()

	if err := service.SetFlymode(expected, true); err != nil {
		t.Fatalf("SetFlymode() source takeover error = %v", err)
	}
	if service.autoFlymodeActive.Load() {
		t.Fatal("manual source takeover left automatic ownership active")
	}
	if service.manualFlymodeGen.Load() != beforeGeneration+1 {
		t.Fatal("manual source takeover did not advance the manual generation")
	}
	frame := port.String()
	for _, want := range []string{
		`"enabled":true`,
		`"expected_iccid":"8986000000000000001"`,
		`"source":"manual"`,
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("source takeover frame %q does not contain %q", frame, want)
		}
	}
}

type capturingSerialPort struct {
	bytes.Buffer
	onWrite func([]byte)
}

func (*capturingSerialPort) SetMode(*serial.Mode) error { return nil }
func (*capturingSerialPort) Read([]byte) (int, error)   { return 0, io.EOF }
func (p *capturingSerialPort) Write(data []byte) (int, error) {
	n, err := p.Buffer.Write(data)
	if p.onWrite != nil {
		p.onWrite(append([]byte(nil), data...))
	}
	return n, err
}
func (*capturingSerialPort) Drain() error             { return nil }
func (*capturingSerialPort) ResetInputBuffer() error  { return nil }
func (*capturingSerialPort) ResetOutputBuffer() error { return nil }
func (*capturingSerialPort) SetDTR(bool) error        { return nil }
func (*capturingSerialPort) SetRTS(bool) error        { return nil }
func (*capturingSerialPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (*capturingSerialPort) SetReadTimeout(time.Duration) error { return nil }
func (*capturingSerialPort) Close() error                       { return nil }
func (*capturingSerialPort) Break(time.Duration) error          { return nil }

func autoRespondToControls(
	service *SerialService,
	port *capturingSerialPort,
	result string,
	errorCode string,
) {
	port.onWrite = func(frame []byte) {
		payload := strings.TrimPrefix(string(frame), "CMD_START:")
		payload = strings.TrimSuffix(payload, ":CMD_END\r\n")
		var command map[string]any
		if err := json.Unmarshal([]byte(payload), &command); err != nil {
			return
		}
		requestID, _ := command["request_id"].(string)
		action, _ := command["action"].(string)
		if requestID == "" {
			return
		}
		service.handleCommandResponse(&ParsedMessage{Payload: map[string]any{
			"action": action, "result": result, "error": errorCode, "request_id": requestID,
		}})
	}
}

func TestRejectedRebootKeepsStatusRefreshEnabled(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "设备一", nil, nil, nil)
	port := &capturingSerialPort{}
	service.port = port
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response",
		"version":"1.3.0",
		"identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001"}
	}`})
	autoRespondToControls(service, port, "error", "sim_identity_mismatch")
	expected := SIMIdentity{SIMID: "iccid:8986000000000000001", ICCID: "8986000000000000001"}

	err := service.RebootMcu(&expected)
	if !errors.Is(err, ErrSIMMismatch) {
		t.Fatalf("RebootMcu() error = %v, want ErrSIMMismatch", err)
	}
	if !service.statusAccepting.Load() {
		t.Fatal("rejected reboot permanently disabled status acceptance")
	}
}
