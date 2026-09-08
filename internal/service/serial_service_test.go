package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"go.bug.st/serial"
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

func TestSerialWriteTimeoutDetachesAndClosesBlockedPort(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "default", "Air780", nil, nil, nil)
	service.writeWait = 25 * time.Millisecond
	port := newBlockingWriteSerialPort()
	service.installSerialPort(port)
	service.setConnected(true)

	started := time.Now()
	err := service.sendJSONCommand(map[string]string{"action": "get_status"})
	if !errors.Is(err, context.DeadlineExceeded) || !writeMayHaveReachedDevice(err) {
		t.Fatalf("blocked write error = %v, want uncertain deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked write took too long: %v", elapsed)
	}
	if current, _ := service.serialPortSnapshot(); current != nil {
		t.Fatal("timed-out serial port remained attached")
	}
	if _, connected := service.getConnectionInfo(); connected {
		t.Fatal("timed-out serial port remained marked connected")
	}
	select {
	case <-port.closed:
	case <-time.After(time.Second):
		t.Fatal("timed-out serial port was not closed")
	}
}

func TestCloseSerialDoesNotWaitForBlockedWrite(t *testing.T) {
	service := NewSerialService(zap.NewNop(), config.SerialConfig{}, "default", "Air780", nil, nil, nil)
	service.writeWait = time.Second
	port := newBlockingWriteSerialPort()
	service.installSerialPort(port)
	done := make(chan error, 1)
	go func() { done <- service.sendJSONCommand(map[string]string{"action": "get_status"}) }()
	select {
	case <-port.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("serial write did not start")
	}

	closed := make(chan struct{})
	go func() {
		service.closeSerial()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closeSerial waited for writeMu")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("closing the serial port did not release blocked Write")
	}
}

type shortWriter struct {
	bytes.Buffer
	limit int
}

type failingWriter struct {
	written int
}

type blockingWriteSerialPort struct {
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingWriteSerialPort() *blockingWriteSerialPort {
	return &blockingWriteSerialPort{
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (p *blockingWriteSerialPort) Write([]byte) (int, error) {
	p.writeOnce.Do(func() { close(p.writeStarted) })
	<-p.closed
	return 0, io.ErrClosedPipe
}

func (p *blockingWriteSerialPort) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}

func (p *blockingWriteSerialPort) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (*blockingWriteSerialPort) SetMode(*serial.Mode) error         { return nil }
func (*blockingWriteSerialPort) Drain() error                       { return nil }
func (*blockingWriteSerialPort) ResetInputBuffer() error            { return nil }
func (*blockingWriteSerialPort) ResetOutputBuffer() error           { return nil }
func (*blockingWriteSerialPort) SetDTR(bool) error                  { return nil }
func (*blockingWriteSerialPort) SetRTS(bool) error                  { return nil }
func (*blockingWriteSerialPort) SetReadTimeout(time.Duration) error { return nil }
func (*blockingWriteSerialPort) Break(time.Duration) error          { return nil }
func (*blockingWriteSerialPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
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
