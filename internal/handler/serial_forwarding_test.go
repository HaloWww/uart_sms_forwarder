package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

func TestForwardingAPIRejectsMissingSIMAndUnknownSettings(t *testing.T) {
	manager, err := service.NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Port: "COM999999"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSerialHandler(zap.NewNop(), manager)
	for _, test := range []struct{ name, method, body, expectedCode string }{
		{"missing_sim", http.MethodGet, "", "sim_id_required"},
		{"unknown_field", http.MethodPut, `{"simId":"iccid:one","config":{"mode":"direct","proxy":"private-secret"}}`, "forwarding_config_invalid"},
		{"invalid_json", http.MethodPut, `{"simId":"iccid:one","config":`, "forwarding_config_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			request := httptest.NewRequest(test.method, "/api/serial/forwarding", strings.NewReader(test.body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			ctx := e.NewContext(request, response)
			var err error
			if test.method == http.MethodGet {
				err = handler.GetForwardingConfig(ctx)
			} else {
				err = handler.SetForwardingConfig(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.expectedCode) {
				t.Fatalf("unexpected rejection: %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("credential-bearing configuration responses must not be cached")
			}
			if strings.Contains(response.Body.String(), "private-secret") {
				t.Fatal("validation error leaked submitted credentials")
			}
		})
	}
}

func TestDeleteForwardingAPIRequiresCurrentSIMAndTargetICCID(t *testing.T) {
	manager, err := service.NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Port: "COM999999"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSerialHandler(zap.NewNop(), manager)
	for _, tt := range []struct{ query, code string }{
		{"?iccid=8986002", "sim_id_required"},
		{"?simId=iccid:8986001", "forwarding_config_invalid"},
		{"?simId=iccid:8986001&iccid=unsafe:CMD_END", "forwarding_config_invalid"},
	} {
		e := echo.New()
		request := httptest.NewRequest(http.MethodDelete, "/api/serial/forwarding"+tt.query, nil)
		response := httptest.NewRecorder()
		if err := handler.DeleteForwardingConfig(e.NewContext(request, response)); err != nil {
			t.Fatal(err)
		}
		var body map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusBadRequest || body["code"] != tt.code || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected delete rejection: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestForwardingBindingLimitHTTPError(t *testing.T) {
	e := echo.New()
	response := httptest.NewRecorder()
	ctx := e.NewContext(httptest.NewRequest(http.MethodPut, "/api/serial/forwarding", nil), response)
	if err := writeForwardingConfigError(ctx, service.ErrForwardingBindingLimit); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "binding_limit_reached") ||
		!strings.Contains(response.Body.String(), "4 条") {
		t.Fatalf("missing actionable binding limit error: %d %s", response.Code, response.Body.String())
	}
}
