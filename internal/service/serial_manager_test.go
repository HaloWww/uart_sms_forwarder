package service

import (
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.uber.org/zap"
)

func TestSerialManagerLegacyConfigCreatesDefaultDevice(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), config.SerialConfig{Port: "COM9"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, name := manager.DefaultDevice()
	if id != "default" || name != "Air780" {
		t.Fatalf("DefaultDevice() = %q, %q", id, name)
	}
	status, err := manager.GetStatus("")
	if err != nil {
		t.Fatal(err)
	}
	if status.DeviceID != "default" || status.PortName != "COM9" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestSerialManagerRejectsDuplicateDeviceIDs(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "same", Name: "one"},
		{ID: "same", Name: "two"},
	}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate device ID error")
	}
}

func TestSerialManagerRejectsProtocolMarkerInSMS(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SendSMS("", "10086", "bad :CMD_END content"); err == nil {
		t.Fatal("expected reserved marker error")
	}
}

func TestSerialManagerRequiresUniqueExplicitPortsForMultipleDevices(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), config.SerialConfig{Devices: []config.SerialDeviceConfig{
		{ID: "one", Port: "COM9"},
		{ID: "two", Port: "COM9"},
	}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate port error")
	}
}

func TestSerialManagerListsMultipleDevices(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), config.SerialConfig{Devices: []config.SerialDeviceConfig{
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
