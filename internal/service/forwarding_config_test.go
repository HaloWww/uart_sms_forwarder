package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func validTestForwardingConfig() ForwardingConfig {
	return ForwardingConfig{Mode: "direct", QueueLimit: 8, MaxAttempts: 3,
		RetryBaseSeconds: 30, RetryMaxSeconds: 900, MinIntervalSeconds: 2,
		StatsSaveSeconds: 60, TimezoneOffsetSeconds: 28800, Template: "{{content}}\n{{receiver}}",
		Channels: []ForwardingChannel{{ID: "bark", Type: "bark", Enabled: true,
			Server: "https://api.day.app", Key: "private-device-key", Title: "SMS"}},
	}
}

func testForwardingManager(t *testing.T) (*SerialManager, *SerialService, *capturingSerialPort) {
	t.Helper()
	m, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := m.services["default"]
	port := &capturingSerialPort{}
	s.installSerialPort(port)
	s.setConnected(true)
	s.handleStatusResponse(&ParsedMessage{JSON: `{"type":"status_response","version":"1.7.0","identity_valid":true,"mobile":{"sim_ready":true,"iccid":"8986001"}}`})
	return m, s, port
}

func decodeTestForwardingCommand(frame []byte) map[string]interface{} {
	var cmd map[string]interface{}
	_ = json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(string(frame), "CMD_START:"), ":CMD_END\r\n")), &cmd)
	return cmd
}

func testForwardingResponse(s *SerialService, cmd map[string]interface{}, result string, payload map[string]interface{}) {
	if payload == nil {
		payload = make(map[string]interface{})
	}
	payload["action"], payload["request_id"], payload["result"] = cmd["action"], cmd["request_id"], result
	s.handleCommandResponse(&ParsedMessage{Payload: payload})
}

func configAsPayload(config ForwardingConfig) map[string]interface{} {
	encoded, _ := json.Marshal(config)
	var data map[string]interface{}
	_ = json.Unmarshal(encoded, &data)
	return data
}

func boundTestForwardingPayload(config ForwardingConfig, persisted bool) map[string]interface{} {
	return map[string]interface{}{
		"config": configAsPayload(config), "persisted": persisted, "sim_bound": true,
		"bound_iccid": "8986001", "bound_number": "13800138000", "binding_limit": 4,
		"legacy_unbound": false,
		"bindings":       []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: config.Mode}},
	}
}

func unboundTestForwardingPayload(config ForwardingConfig) map[string]interface{} {
	return map[string]interface{}{
		"config": configAsPayload(config), "persisted": false, "sim_bound": false,
		"bound_iccid": "", "bound_number": "", "binding_limit": 4,
		"legacy_unbound": true, "bindings": map[string]interface{}{},
	}
}

func TestForwardingConfigSaveRequiresPersistentACKAndReadback(t *testing.T) {
	m, s, port := testForwardingManager(t)
	wanted := validTestForwardingConfig()
	var actions []string
	var saved map[string]interface{}
	port.onWrite = func(frame []byte) {
		cmd := decodeTestForwardingCommand(frame)
		action, _ := cmd["action"].(string)
		if cmd["request_id"] == nil {
			return
		}
		actions = append(actions, action)
		if cmd["expected_iccid"] != "8986001" {
			t.Error("configuration command missing selected SIM guard")
		}
		if action == "set_forward_config" {
			saved, _ = cmd["config"].(map[string]interface{})
		}
		payload := boundTestForwardingPayload(wanted, true)
		payload["config"] = saved
		testForwardingResponse(s, cmd, "ok", payload)
	}
	state, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", wanted)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Persisted || !state.SIMBound || state.BoundICCID != "8986001" || !reflect.DeepEqual(state.Config, wanted) {
		t.Fatalf("incorrect readback: %+v", state)
	}
	if !reflect.DeepEqual(actions, []string{"set_forward_config", "get_forward_config"}) {
		t.Fatalf("must verify stored settings: %v", actions)
	}
}

func TestForwardingConfigDoesNotReportSuccessForUnpersistedOrDifferentReadback(t *testing.T) {
	for _, scenario := range []string{"ack_not_persisted", "readback_not_persisted", "readback_changed"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			wanted := validTestForwardingConfig()
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil {
					return
				}
				persisted := !(scenario == "ack_not_persisted" && cmd["action"] == "set_forward_config") &&
					!(scenario == "readback_not_persisted" && cmd["action"] == "get_forward_config")
				actual := wanted
				if scenario == "readback_changed" {
					actual.Mode = "uart"
				}
				testForwardingResponse(s, cmd, "ok", boundTestForwardingPayload(actual, persisted))
			}
			if _, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", wanted); !errors.Is(err, ErrForwardingConfigNotPersisted) {
				t.Fatalf("false save success: %v", err)
			}
		})
	}
}

func TestForwardingConfigRejectsOldFirmwareAndChangedSIMBeforeWire(t *testing.T) {
	for _, scenario := range []string{"old", "sim_changed"} {
		t.Run(scenario, func(t *testing.T) {
			_, s, port := testForwardingManager(t)
			if scenario == "old" {
				s.rememberScriptVersion("1.5.0")
			}
			expected := SIMIdentity{SIMID: "iccid:8986001", ICCID: "8986001", Verified: true}
			if scenario == "sim_changed" {
				s.handleStatusResponse(&ParsedMessage{JSON: `{"type":"status_response","version":"1.7.0","identity_valid":true,"mobile":{"sim_ready":true,"iccid":"8986002"}}`})
			}
			_, err := s.forwardingConfig(context.Background(), expected, nil)
			if err == nil {
				t.Fatal("unsafe configuration operation was allowed")
			}
			if port.String() != "" {
				t.Fatal("unsafe configuration was written before validation")
			}
		})
	}
}

func TestForwardingConfigCorrelationAndCancellation(t *testing.T) {
	for _, scenario := range []string{"wrong_request", "wrong_action", "missing_reply"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil || scenario == "missing_reply" {
					return
				}
				if scenario == "wrong_request" {
					cmd["request_id"] = "another-request"
				}
				if scenario == "wrong_action" {
					cmd["action"] = "set_flymode"
				}
				testForwardingResponse(s, cmd, "ok", boundTestForwardingPayload(validTestForwardingConfig(), true))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, err := m.GetForwardingConfig(ctx, "iccid:8986001"); err == nil {
				t.Fatal("uncorrelated reply or timeout reported success")
			}
			pending := 0
			s.pendingControls.Range(func(_, _ any) bool { pending++; return true })
			if pending != 0 {
				t.Fatal("interrupted request left pending credential response channel")
			}
		})
	}
}

func TestGetForwardingConfigAcceptsUnpersistedDefaultsAndEmptyLuaArray(t *testing.T) {
	m, s, port := testForwardingManager(t)
	wanted := validTestForwardingConfig()
	wanted.Mode, wanted.Channels = "uart", []ForwardingChannel{}
	port.onWrite = func(frame []byte) {
		cmd := decodeTestForwardingCommand(frame)
		if cmd["request_id"] == nil {
			return
		}
		payload := unboundTestForwardingPayload(wanted)
		payload["config"].(map[string]interface{})["channels"] = map[string]interface{}{}
		testForwardingResponse(s, cmd, "ok", payload)
	}
	state, err := m.GetForwardingConfig(context.Background(), "iccid:8986001")
	if err != nil {
		t.Fatal(err)
	}
	if state.Persisted || state.SIMBound || state.BoundICCID != "" || state.BindingLimit != 4 ||
		len(state.Bindings) != 0 || !state.LegacyUnbound || !reflect.DeepEqual(state.Config, wanted) {
		t.Fatalf("unexpected defaults: %+v", state)
	}
}

func TestValidateForwardingConfigAndStrictRequestDecoding(t *testing.T) {
	valid := validTestForwardingConfig()
	if err := ValidateForwardingConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ForwardingConfig){
		func(c *ForwardingConfig) { c.Mode = "both" },
		func(c *ForwardingConfig) { c.Channels[0].Server = "http://api.day.app" },
		func(c *ForwardingConfig) { c.Channels[0].Server = "https://user:pass@example.com" },
		func(c *ForwardingConfig) { c.Template = "unsafe :CMD_END" },
		func(c *ForwardingConfig) { c.Template = "unsafe :SMS_END" },
		func(c *ForwardingConfig) { c.Channels[0].CACert = strings.Repeat("X", 4000) },
		func(c *ForwardingConfig) { c.MaxAttempts = 100 },
		func(c *ForwardingConfig) { c.Channels = nil },
	} {
		candidate := validTestForwardingConfig()
		change(&candidate)
		if err := ValidateForwardingConfig(candidate); !errors.Is(err, ErrForwardingConfigInvalid) {
			t.Fatal("invalid device setting accepted")
		}
	}
	encoded, _ := json.Marshal(map[string]interface{}{"simId": "iccid:8986001", "config": valid})
	if id, got, err := DecodeForwardingConfigRequest(strings.NewReader(string(encoded))); err != nil || id != "iccid:8986001" || !reflect.DeepEqual(got, valid) {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	for _, malformed := range []string{string(encoded) + "{}", strings.Replace(string(encoded), `"mode":"direct"`, `"mode":"direct","proxy":"hidden"`, 1)} {
		if _, _, err := DecodeForwardingConfigRequest(strings.NewReader(malformed)); !errors.Is(err, ErrForwardingConfigInvalid) {
			t.Fatal("unknown/trailing JSON accepted")
		}
	}
}

func TestMalformedForwardingResponsesNeverLogCredentialPayload(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	s := NewSerialService(zap.New(core), config.SerialConfig{}, "test", "test", nil, nil, nil)
	secret := "sensitive-forwarding-secret"
	s.processReceivedData(`SMS_START:{"config":{"secret":"` + secret + `"}}:SMS_END`)
	s.processReceivedData(`SMS_START:{"config":{"secret":"` + secret + `"}:SMS_END`)
	for _, entry := range logs.All() {
		encoded, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(string(encoded), secret) || strings.Contains(entry.Message, secret) {
			t.Fatal("malformed UART response leaked credentials")
		}
	}
}
