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
