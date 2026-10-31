package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestDirectForwardingPreservesArchiveAndACKWithoutDuplicateNotification(t *testing.T) {
	for _, mode := range []string{"direct", "uart", ""} {
		t.Run("mode_"+mode, func(t *testing.T) {
			requests := make(chan struct{}, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- struct{}{}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			db, err := gorm.Open(sqlite.Open(uniqueTestSQLiteDSN("direct_forward")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&models.TextMessage{}, &models.Property{}); err != nil {
				t.Fatal(err)
			}
			properties := NewPropertyService(zap.NewNop(), db)
			if err := properties.Set(context.Background(), PropertyIDNotificationChannels, "channels", []models.NotificationChannelConfig{{
				Type: "webhook", Enabled: true, Config: map[string]interface{}{"url": server.URL, "body": `{"content":"{{content}}"}`},
			}}); err != nil {
				t.Fatal(err)
			}
			messages := NewTextMessageService(zap.NewNop(), repo.NewTextMessageRepo(db))
			service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "Air780", messages, NewNotifier(zap.NewNop()), properties)
			port := &capturingSerialPort{}
			service.installSerialPort(port)
			service.setConnected(true)
			payload := map[string]interface{}{
				"type": "incoming_sms", "message_id": "direct-test", "forward_mode": mode,
				"identity_valid": true, "iccid": "8986000000000000001", "muid": "module-one",
				"from": "10086", "content": "test", "timestamp": 1700000000,
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				service.handleIncomingSMS(&ParsedMessage{JSON: string(encoded)})
			}
			var records []models.TextMessage
			if err := db.Find(&records).Error; err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].SIMID != "iccid:8986000000000000001" || records[0].Content != "test" {
				t.Fatalf("SMS archive/deduplication changed: %+v", records)
			}
			if strings.Count(port.String(), `"action":"ack_sms"`) != 2 {
				t.Fatalf("both original and replay must be ACKed: %s", port.String())
			}
			if mode != "direct" {
				select {
				case <-requests:
				case <-time.After(2 * time.Second):
					t.Fatal("host/legacy mode did not notify")
				}
			}
			select {
			case <-requests:
				t.Fatal("duplicate host notification")
			case <-time.After(100 * time.Millisecond):
			}
			callJSON, err := json.Marshal(map[string]interface{}{
				"type": "incoming_call", "forward_mode": mode, "from": "10086", "timestamp": 1700000001,
			})
			if err != nil {
				t.Fatal(err)
			}
			service.handleIncomingCall(&ParsedMessage{JSON: string(callJSON)})
			if mode != "direct" {
				select {
				case <-requests:
				case <-time.After(2 * time.Second):
					t.Fatal("host/legacy mode did not notify incoming call")
				}
			} else {
				select {
				case <-requests:
					t.Fatal("direct-mode call was forwarded again by the host")
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

func TestStatusExposesDirectForwardStatsAndDefaultsLegacyMode(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "Air780", nil, nil, nil)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","forward_mode":"direct",
		"direct_forward":{"enabled":true,"queued":2,"sent":5,"failed":1,"dropped":3,"requests":8,"unmetered_requests":2,
		"tx_bytes":4.294967296e9,"rx_bytes":23.0,"total_bytes":4294967319.0,
		"counter_available":true,"storage_available":true,"last_error":"http_timeout"}
	}`})
	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ForwardMode != "direct" || !status.DirectForward.Enabled || status.DirectForward.Queued != 2 ||
		status.DirectForward.TxBytes != 4294967296 || status.DirectForward.RxBytes != 23 ||
		status.DirectForward.TotalBytes != 4294967319 || status.DirectForward.UnmeteredRequests != 2 || !status.DirectForward.CounterAvailable ||
		!status.DirectForward.StorageAvailable || status.DirectForward.LastError != "http_timeout" {
		t.Fatalf("direct stats lost: %+v", status.DirectForward)
	}
	service.handleStatusResponse(&ParsedMessage{JSON: `{"type":"status_response","version":"1.4.0"}`})
	status, err = service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ForwardMode != "uart" || status.DirectForward.Enabled {
		t.Fatalf("legacy mode not preserved: %+v", status)
	}
}
