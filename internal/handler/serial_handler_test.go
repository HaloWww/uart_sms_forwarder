package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

func TestSendSMSRequiresUUIDRequestID(t *testing.T) {
	handler := newOfflineSerialHandler(t)

	status, body := callSendSMS(t, handler, `{"simId":"iccid:8986000000000000001","to":"10086","content":"test"}`, "")
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_id_required"`) {
		t.Fatalf("missing requestId response = %d %s", status, body)
	}

	status, body = callSendSMS(t, handler, `{"simId":"iccid:8986000000000000001","to":"10086","content":"test","requestId":"bad"}`, "")
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_id_invalid"`) {
		t.Fatalf("invalid requestId response = %d %s", status, body)
	}
}

func TestSendSMSAcceptsIdempotencyKeyHeader(t *testing.T) {
	handler := newOfflineSerialHandler(t)
	const requestID = "f851ccee-2c10-4ebb-9992-3532dc5f31b8"
	status, body := callSendSMS(
		t, handler,
		`{"simId":"iccid:8986000000000000001","to":"10086","content":"test"}`,
		requestID,
	)
	// 路由到离线 SIM 才返回 sim_unknown，说明请求头已通过幂等键校验。
	if status != http.StatusNotFound || !strings.Contains(body, `"code":"sim_unknown"`) {
		t.Fatalf("Idempotency-Key response = %d %s", status, body)
	}
}

func TestSendSMSComparesCanonicalRequestIDs(t *testing.T) {
	handler := newOfflineSerialHandler(t)
	const lower = "f851ccee-2c10-4ebb-9992-3532dc5f31b8"
	bodyRequestID := strings.ToUpper(lower)
	body := `{"simId":"iccid:8986000000000000001","to":"10086","content":"test","requestId":"` + bodyRequestID + `"}`
	status, responseBody := callSendSMS(t, handler, body, lower)
	if status != http.StatusNotFound || strings.Contains(responseBody, "request_id_mismatch") {
		t.Fatalf("canonical UUID comparison response = %d %s", status, responseBody)
	}
}

func newOfflineSerialHandler(t *testing.T) *SerialHandler {
	t.Helper()
	manager, err := service.NewSerialManager(
		zap.NewNop(), nil, config.SerialConfig{Port: "COM9"}, nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewSerialHandler(zap.NewNop(), manager)
}

func callSendSMS(t *testing.T, handler *SerialHandler, body, idempotencyKey string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/serial/sms", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	ctx := echo.NewContext(request, recorder, echo.New())
	if err := handler.SendSMS(ctx); err != nil {
		t.Fatal(err)
	}
	return recorder.Code, recorder.Body.String()
}
