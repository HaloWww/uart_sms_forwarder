package service

import (
	"encoding/json"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.uber.org/zap"
)

func TestStatusSeparatesHomeAndServingNetwork(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "Air780", nil, nil, nil)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001","imsi":"454001234567890",
		"is_registered":true,"is_roaming":true,"serving_network_supported":true,
		"serving_mcc":460,"serving_mnc":0,"serving_operator":"untrusted modem label"}
	}`})
	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Mobile.Operator != "CSL HK" || status.Mobile.ServingOperator != "中国移动" ||
		status.Mobile.ServingMCC == nil || *status.Mobile.ServingMCC != 460 ||
		status.Mobile.ServingMNC == nil || *status.Mobile.ServingMNC != 0 {
		t.Fatalf("home and serving networks were conflated: %+v", status.Mobile)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip StatusData
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.Mobile.ServingMNC == nil || *roundtrip.Mobile.ServingMNC != 0 {
		t.Fatal("valid MNC zero was omitted from the status JSON")
	}
}

func TestServingNetworkValidation(t *testing.T) {
	intPointer := func(value int) *int { return &value }
	tests := []struct {
		name       string
		mcc, mnc   *int
		mutate     func(*StatusData)
		wantValues bool
		wantName   string
	}{
		{name: "known MNC zero", mcc: intPointer(460), mnc: intPointer(0), wantValues: true, wantName: "中国移动"},
		{name: "another serving network", mcc: intPointer(460), mnc: intPointer(11), wantValues: true, wantName: "中国电信"},
		{name: "unknown network does not use home IMSI", mcc: intPointer(999), mnc: intPointer(99), wantValues: true},
		{name: "unknown three digit MNC", mcc: intPointer(310), mnc: intPointer(260), wantValues: true},
		{name: "missing MCC", mnc: intPointer(0)},
		{name: "missing MNC", mcc: intPointer(460)},
		{name: "zero MCC", mcc: intPointer(0), mnc: intPointer(0)},
		{name: "negative MCC", mcc: intPointer(-1), mnc: intPointer(0)},
		{name: "MCC out of range", mcc: intPointer(1000), mnc: intPointer(0)},
		{name: "negative MNC", mcc: intPointer(460), mnc: intPointer(-1)},
		{name: "MNC out of range", mcc: intPointer(460), mnc: intPointer(1000)},
		{name: "unregistered", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Mobile.IsRegistered = false }},
		{name: "flight mode", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Mobile.Flymode = true }},
		{name: "top level flight mode", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Flymode = true }},
		{name: "SIM not ready", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Mobile.SimReady = false }},
		{name: "unverified SIM", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.IdentityValid = false }},
		{name: "missing ICCID", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Mobile.Iccid = "" }},
		{name: "unsupported script", mcc: intPointer(460), mnc: intPointer(0), mutate: func(s *StatusData) { s.Mobile.ServingNetworkSupported = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := StatusData{IdentityValid: true}
			status.Mobile.SimReady = true
			status.Mobile.Iccid = "8986000000000000001"
			status.Mobile.Imsi = "454001234567890"
			status.Mobile.Operator = "CSL HK"
			status.Mobile.IsRegistered = true
			status.Mobile.ServingNetworkSupported = true
			status.Mobile.ServingMCC, status.Mobile.ServingMNC = tt.mcc, tt.mnc
			status.Mobile.ServingOperator = "stale network name"
			if tt.mutate != nil {
				tt.mutate(&status)
			}
			wantSupported := status.Mobile.ServingNetworkSupported
			normalizeServingNetwork(&status)
			if status.Mobile.ServingOperator != tt.wantName {
				t.Fatalf("serving operator = %q, want %q", status.Mobile.ServingOperator, tt.wantName)
			}
			if tt.wantValues {
				if status.Mobile.ServingMCC == nil || status.Mobile.ServingMNC == nil ||
					*status.Mobile.ServingMCC != *tt.mcc || *status.Mobile.ServingMNC != *tt.mnc {
					t.Fatal("valid serving network values were lost")
				}
			} else if status.Mobile.ServingMCC != nil || status.Mobile.ServingMNC != nil {
				t.Fatalf("invalid or stale serving network was retained: %+v", status.Mobile)
			}
			if status.Mobile.ServingNetworkSupported != wantSupported || status.Mobile.Operator != "CSL HK" {
				t.Fatal("normalizing serving network changed capability or SIM home network")
			}
		})
	}
}

func TestLegacyStatusClearsPreviouslyReportedServingNetwork(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "one", "Air780", nil, nil, nil)
	service.setConnected(true)
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001","imsi":"454001234567890",
		"is_registered":true,"serving_network_supported":true,"serving_mcc":460,"serving_mnc":0}
	}`})
	service.handleStatusResponse(&ParsedMessage{JSON: `{
		"type":"status_response","version":"1.6.0","identity_valid":true,
		"mobile":{"sim_ready":true,"iccid":"8986000000000000001","imsi":"454001234567890","is_registered":true}
	}`})
	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Mobile.ServingNetworkSupported || status.Mobile.ServingMCC != nil ||
		status.Mobile.ServingMNC != nil || status.Mobile.ServingOperator != "" || status.Mobile.Operator != "CSL HK" {
		t.Fatalf("legacy status retained stale serving network: %+v", status.Mobile)
	}
}
