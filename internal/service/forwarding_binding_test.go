package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestForwardingRequiresBindingProtocolForEveryOperation(t *testing.T) {
	for _, version := range []string{"1.5.9", "1.6.0", "1.6.1", "1.7.0", "1.x.0", ""} {
		if supportsForwardingConfigProtocol(version) {
			t.Fatalf("unsafe script accepted: %q", version)
		}
	}
	for _, version := range []string{"1.7.1", "v1.7.2", "1.8.0", "2.0.0"} {
		if !supportsForwardingConfigProtocol(version) {
			t.Fatalf("binding-capable script rejected: %q", version)
		}
	}
	for _, operation := range []string{"get", "set", "delete"} {
		t.Run(operation, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			s.rememberScriptVersion("1.7.0")
			var err error
			switch operation {
			case "get":
				_, err = m.GetForwardingConfig(context.Background(), "iccid:8986001")
			case "set":
				_, err = m.SetForwardingConfig(context.Background(), "iccid:8986001", validTestForwardingConfig())
			case "delete":
				_, err = m.DeleteForwardingConfig(context.Background(), "iccid:8986001", "8986002")
			}
			if !errors.Is(err, ErrSIMProtocolOutdated) || port.String() != "" {
				t.Fatalf("old script received a binding operation: %v", err)
			}
		})
	}
}

func TestForwardingBindingStateRejectsUnsafeOrPrivateSummaries(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]interface{})
	}{
		{"wrong_bound_SIM", func(p map[string]interface{}) { p["bound_iccid"] = "8986002" }},
		{"not_persisted", func(p map[string]interface{}) { p["persisted"] = false }},
		{"missing_binding_metadata", func(p map[string]interface{}) { delete(p, "sim_bound") }},
		{"missing_summary", func(p map[string]interface{}) { p["bindings"] = []ForwardingBinding{} }},
		{"missing_summary_array", func(p map[string]interface{}) { delete(p, "bindings") }},
		{"unbound_direct", func(p map[string]interface{}) {
			p["sim_bound"], p["persisted"], p["bound_iccid"] = false, false, ""
			p["bindings"] = []ForwardingBinding{}
		}},
		{"different_summary_mode", func(p map[string]interface{}) {
			p["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "uart"}}
		}},
		{"different_summary_number", func(p map[string]interface{}) {
			p["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138001", Mode: "direct"}}
		}},
		{"duplicate_bindings", func(p map[string]interface{}) {
			p["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "direct"}, {ICCID: "8986001", Mode: "direct"}}
		}},
		{"too_many_bindings", func(p map[string]interface{}) {
			p["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "direct"},
				{ICCID: "8986002", Mode: "direct"}, {ICCID: "8986003", Mode: "direct"},
				{ICCID: "8986004", Mode: "direct"}, {ICCID: "8986005", Mode: "direct"}}
		}},
		{"missing_limit", func(p map[string]interface{}) { delete(p, "binding_limit") }},
		{"other_SIM_credentials_in_summary", func(p map[string]interface{}) {
			p["bindings"] = []interface{}{map[string]interface{}{
				"iccid": "8986002", "number": "", "mode": "direct", "config": map[string]interface{}{"key": "other-SIM-private-key"},
			}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := boundTestForwardingPayload(validTestForwardingConfig(), true)
			tt.edit(payload)
			state, err := decodeForwardingConfigState(payload, "8986001")
			if err == nil || state != nil {
				t.Fatal("unsafe state was accepted")
			}
			if strings.Contains(err.Error(), "other-SIM-private-key") || strings.Contains(err.Error(), "private-device-key") {
				t.Fatal("rejected state exposed credentials")
			}
		})
	}
}

func TestForwardingSaveNeedsBoundACKAndBoundReadback(t *testing.T) {
	for _, scenario := range []string{"ACK_unbound", "ACK_wrong_SIM", "readback_unbound", "readback_wrong_SIM"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil {
					return
				}
				payload := boundTestForwardingPayload(validTestForwardingConfig(), true)
				isACK := cmd["action"] == "set_forward_config"
				if scenario == "ACK_unbound" && isACK {
					payload["sim_bound"] = false
				}
				if scenario == "ACK_wrong_SIM" && isACK || scenario == "readback_wrong_SIM" && !isACK {
					payload["bound_iccid"] = "8986002"
				}
				if scenario == "readback_unbound" && !isACK {
					defaults := validTestForwardingConfig()
					defaults.Mode = "uart"
					payload = unboundTestForwardingPayload(defaults)
				}
				testForwardingResponse(s, cmd, "ok", payload)
			}
			if state, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", validTestForwardingConfig()); err == nil || state != nil {
				t.Fatal("save succeeded without a current SIM binding")
			}
		})
	}
}

func TestGetForwardingConfigAllowsInactiveBindingAfterNumberChanges(t *testing.T) {
	m, s, port := testForwardingManager(t)
	defaults := validTestForwardingConfig()
	defaults.Mode, defaults.Channels = "uart", []ForwardingChannel{}
	payload := unboundTestForwardingPayload(defaults)
	payload["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "direct"}}
	port.onWrite = func(frame []byte) {
		cmd := decodeTestForwardingCommand(frame)
		if cmd["request_id"] != nil {
			testForwardingResponse(s, cmd, "ok", payload)
		}
	}
	state, err := m.GetForwardingConfig(context.Background(), "iccid:8986001")
	if err != nil {
		t.Fatal(err)
	}
	if state.SIMBound || state.Persisted || state.BoundICCID != "" || state.BoundNumber != "" ||
		state.Config.Mode != "uart" || len(state.Bindings) != 1 || state.Bindings[0].Number != "13800138000" {
		t.Fatalf("inactive old-number binding was not available for reconfirmation: %+v", state)
	}
	// Inactive summaries are allowed, but cannot be reported as an active binding.
	payload["bound_number"] = "13800138001"
	if _, err := decodeForwardingConfigState(payload, "8986001"); !errors.Is(err, ErrForwardingConfigNotPersisted) {
		t.Fatalf("unbound response advertised an active number: %v", err)
	}
}

func TestDeleteForwardingBindingUsesCurrentSIMAndVerifiesReadback(t *testing.T) {
	for _, scenario := range []string{"old_binding", "current_binding", "target_remains", "SIM_changes_after_ACK"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			target := "8986002"
			if scenario == "current_binding" {
				target = "8986001"
			}
			var actions []string
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil {
					return
				}
				actions = append(actions, cmd["action"].(string))
				if cmd["expected_iccid"] != "8986001" {
					t.Error("delete/readback command lost the live current SIM guard")
				}
				payload := boundTestForwardingPayload(validTestForwardingConfig(), true)
				if cmd["action"] == "delete_forward_config" {
					if cmd["delete_iccid"] != target {
						t.Error("wrong binding selected for deletion")
					}
					if scenario == "SIM_changes_after_ACK" {
						s.handleStatusResponse(&ParsedMessage{JSON: `{"type":"status_response","version":"1.7.1","identity_valid":true,"mobile":{"sim_ready":true,"iccid":"8986003"}}`})
					}
				} else if scenario == "current_binding" {
					defaults := validTestForwardingConfig()
					defaults.Mode = "uart"
					payload = unboundTestForwardingPayload(defaults)
				} else if scenario == "target_remains" {
					payload["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "direct"}, {ICCID: target, Mode: "direct"}}
				}
				testForwardingResponse(s, cmd, "ok", payload)
			}
			state, err := m.DeleteForwardingConfig(context.Background(), "iccid:8986001", target)
			if scenario == "target_remains" || scenario == "SIM_changes_after_ACK" {
				if err == nil || state != nil {
					t.Fatal("delete succeeded without verified removal on the intended board")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actions, []string{"delete_forward_config", "get_forward_config"}) {
				t.Fatalf("delete was not read back: %v", actions)
			}
			if scenario == "current_binding" && (state.SIMBound || state.Persisted || state.Config.Mode != "uart") {
				t.Fatal("deleted current binding did not return to unbound UART mode")
			}
			encoded, _ := json.Marshal(state.Bindings)
			if strings.Contains(string(encoded), "private-device-key") {
				t.Fatal("binding summary exposed channel credentials")
			}
		})
	}
}

func TestForwardingBindingLimitProducesExplicitError(t *testing.T) {
	m, s, port := testForwardingManager(t)
	port.onWrite = func(frame []byte) {
		cmd := decodeTestForwardingCommand(frame)
		if cmd["request_id"] != nil {
			testForwardingResponse(s, cmd, "error", map[string]interface{}{"error": "binding_limit_reached"})
		}
	}
	if _, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", validTestForwardingConfig()); !errors.Is(err, ErrForwardingBindingLimit) {
		t.Fatalf("binding limit was not identified: %v", err)
	}
}

func TestUARTForwardingUsesNoBindingSlot(t *testing.T) {
	for _, scenario := range []string{"unconfigured_SIM", "switch_existing_direct", "four_other_direct_bindings"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			if scenario == "switch_existing_direct" {
				s.handleStatusResponse(&ParsedMessage{JSON: `{"type":"status_response","version":"1.7.1","identity_valid":true,"forward_mode":"direct","forward_bound_iccid":"8986001","mobile":{"sim_ready":true,"iccid":"8986001"}}`})
			}
			wanted := validTestForwardingConfig()
			wanted.Mode, wanted.APN, wanted.Template = "uart", "must-not-be-stored", "host template is not a board binding"
			defaults := validTestForwardingConfig()
			defaults.Mode, defaults.APN, defaults.Channels = "uart", "", []ForwardingChannel{}
			payload := unboundTestForwardingPayload(defaults)
			payload["legacy_unbound"] = false
			if scenario == "four_other_direct_bindings" {
				payload["bindings"] = []ForwardingBinding{{ICCID: "8986002", Mode: "direct"},
					{ICCID: "8986003", Mode: "direct"}, {ICCID: "8986004", Mode: "direct"}, {ICCID: "8986005", Mode: "direct"}}
			}
			var actions []string
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil {
					return
				}
				actions = append(actions, cmd["action"].(string))
				if cmd["expected_iccid"] != "8986001" {
					t.Error("UART switch lost the current SIM guard")
				}
				if cmd["action"] == "set_forward_config" && cmd["config"].(map[string]interface{})["mode"] != "uart" {
					t.Error("UART switch sent a direct configuration")
				}
				testForwardingResponse(s, cmd, "ok", payload)
			}
			state, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", wanted)
			if err != nil {
				t.Fatal(err)
			}
			if state.SIMBound || state.Persisted || state.BoundICCID != "" || state.BoundNumber != "" || state.Config.Mode != "uart" {
				t.Fatalf("host mode retained a binding: %+v", state)
			}
			for _, binding := range state.Bindings {
				if binding.ICCID == "8986001" {
					t.Fatal("host mode still consumes the current SIM's binding slot")
				}
			}
			if scenario == "four_other_direct_bindings" && len(state.Bindings) != 4 {
				t.Fatal("switching to host mode changed other SIMs' four direct bindings")
			}
			if !reflect.DeepEqual(actions, []string{"set_forward_config", "get_forward_config"}) {
				t.Fatalf("UART switch was not confirmed by readback: %v", actions)
			}
			if reflect.DeepEqual(state.Config, wanted) {
				t.Fatal("fixture must verify UART settings need not be stored or match the request")
			}
		})
	}
}

func TestUARTForwardingNeverReportsSuccessWithoutConfirmedRemoval(t *testing.T) {
	for _, scenario := range []string{"ACK_persisted", "ACK_bound", "ACK_missing_field", "ACK_bound_number", "ACK_storage_error",
		"readback_active_direct", "readback_inactive_direct_record", "readback_UART_record"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, port := testForwardingManager(t)
			wanted := validTestForwardingConfig()
			wanted.Mode = "uart"
			port.onWrite = func(frame []byte) {
				cmd := decodeTestForwardingCommand(frame)
				if cmd["request_id"] == nil {
					return
				}
				defaults := validTestForwardingConfig()
				defaults.Mode, defaults.Channels = "uart", []ForwardingChannel{}
				payload := unboundTestForwardingPayload(defaults)
				if cmd["action"] == "set_forward_config" {
					switch scenario {
					case "ACK_persisted":
						payload["persisted"] = true
					case "ACK_bound":
						payload["sim_bound"] = true
					case "ACK_missing_field":
						delete(payload, "persisted")
					case "ACK_bound_number":
						payload["bound_number"] = "13800138000"
					case "ACK_storage_error":
						testForwardingResponse(s, cmd, "error", map[string]interface{}{"error": "storage_write_failed"})
						return
					}
				} else {
					switch scenario {
					case "readback_active_direct":
						payload = boundTestForwardingPayload(validTestForwardingConfig(), true)
					case "readback_inactive_direct_record":
						payload["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "old-number", Mode: "direct"}}
					case "readback_UART_record":
						payload["bindings"] = []ForwardingBinding{{ICCID: "8986001", Mode: "uart"}}
					}
				}
				testForwardingResponse(s, cmd, "ok", payload)
			}
			if state, err := m.SetForwardingConfig(context.Background(), "iccid:8986001", wanted); err == nil || state != nil {
				t.Fatal("UART switch succeeded without confirmed deletion of the current direct record")
			}
		})
	}
}

func TestForwardingStateRejectsUARTBindingSlots(t *testing.T) {
	for _, scenario := range []string{"current_bound_UART", "other_UART_slot"} {
		t.Run(scenario, func(t *testing.T) {
			var payload map[string]interface{}
			if scenario == "current_bound_UART" {
				config := validTestForwardingConfig()
				config.Mode = "uart"
				payload = boundTestForwardingPayload(config, true)
			} else {
				payload = boundTestForwardingPayload(validTestForwardingConfig(), true)
				payload["bindings"] = []ForwardingBinding{{ICCID: "8986001", Number: "13800138000", Mode: "direct"},
					{ICCID: "8986002", Mode: "uart"}}
			}
			if state, err := decodeForwardingConfigState(payload, "8986001"); err == nil || state != nil {
				t.Fatal("device still storing UART in a binding slot was accepted")
			}
		})
	}
}
