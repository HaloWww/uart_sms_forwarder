package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.uber.org/zap"
)

func TestWriteAllHandlesShortWrites(t *testing.T) {
	writer := &shortWriter{limit: 3}
	want := []byte("complete frame")
	if err := writeAll(writer, want); err != nil {
		t.Fatalf("writeAll() error = %v", err)
	}
	if !bytes.Equal(writer.Bytes(), want) {
		t.Fatalf("writeAll() wrote %q, want %q", writer.Bytes(), want)
	}
}

func TestWriteAllErrorsAfterDriverCallAreUncertain(t *testing.T) {
	for _, written := range []int{0, 5, len("complete frame")} {
		t.Run(fmt.Sprintf("written_%d", written), func(t *testing.T) {
			err := writeAll(&failingWriter{written: written}, []byte("complete frame"))
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("writeAll() error = %v, want wrapped ErrClosedPipe", err)
			}
			if !writeMayHaveReachedDevice(err) {
				t.Fatal("driver error after Write started was treated as a definite non-submission")
			}
		})
	}
}

func TestGetStatusReturnsCopy(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "default", "Air780", nil, nil, nil)
	cached := &StatusData{Version: "1.0.4", PortName: "cached"}
	service.deviceCache.Set(CacheKeyDeviceStatus, cached, CacheTTL)
	service.setPortName("active")
	service.setConnected(true)

	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status == cached {
		t.Fatal("GetStatus() returned the mutable cached pointer")
	}
	if status.PortName != "active" || !status.Connected {
		t.Fatalf("GetStatus() = %+v", status)
	}
	if cached.PortName != "cached" || cached.Connected {
		t.Fatalf("GetStatus() mutated cache: %+v", cached)
	}
}

func TestGetStatusRejectsPreviousConnectionGeneration(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "default", "Air780", nil, nil, nil)
	service.setConnected(true)
	stale := &StatusData{SIMID: "iccid:old", IdentityValid: true}
	stale.Mobile.SimReady = true
	stale.Mobile.Iccid = "old"
	service.deviceCache.Set(CacheKeyDeviceStatus, stale, CacheTTL)

	service.invalidateDeviceStatus(true)
	// 模拟上一代连接中已经在途、但较晚写入的状态。
	service.deviceCache.Set(CacheKeyDeviceStatus, stale, CacheTTL)
	status, err := service.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SIMID != "" || status.Mobile.SimReady {
		t.Fatalf("previous connection status leaked into current route: %+v", status)
	}
}

type shortWriter struct {
	bytes.Buffer
	limit int
}

type failingWriter struct {
	written int
}

func (w *failingWriter) Write(data []byte) (int, error) {
	if w.written > len(data) {
		w.written = len(data)
	}
	return w.written, io.ErrClosedPipe
}

func (w *shortWriter) Write(data []byte) (int, error) {
	if len(data) > w.limit {
		data = data[:w.limit]
	}
	return w.Buffer.Write(data)
}
