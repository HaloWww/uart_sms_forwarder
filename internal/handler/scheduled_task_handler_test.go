package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

func TestTriggerScheduledTaskRequiresStableRequestID(t *testing.T) {
	handler := NewScheduledTaskHandler(zap.NewNop(), nil)

	status, body := callTriggerScheduledTask(t, handler, `{}`, "")
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_id_required"`) {
		t.Fatalf("missing requestId response=%d %s", status, body)
	}

	status, body = callTriggerScheduledTask(t, handler, `{"requestId":"bad"}`, "")
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_id_invalid"`) {
		t.Fatalf("invalid requestId response=%d %s", status, body)
	}

	status, body = callTriggerScheduledTask(
		t, handler,
		`{"requestId":"11111111-1111-4111-8111-111111111111"}`,
		"22222222-2222-4222-8222-222222222222",
	)
	if status != http.StatusBadRequest || !strings.Contains(body, `"code":"request_id_mismatch"`) {
		t.Fatalf("mismatched requestId response=%d %s", status, body)
	}
}

func callTriggerScheduledTask(
	t *testing.T,
	handler *ScheduledTaskHandler,
	body string,
	idempotencyKey string,
) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/scheduled-tasks/task/trigger", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	ctx := echo.NewContext(request, recorder, echo.New())
	ctx.SetPathValues(echo.PathValues{{Name: "id", Value: "task"}})
	if err := handler.Trigger(ctx); err != nil {
		t.Fatal(err)
	}
	return recorder.Code, recorder.Body.String()
}
