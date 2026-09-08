package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"go.bug.st/serial"
	"go.uber.org/zap"
)

func boolConfig(value bool) *bool { return &value }

func TestSerialManagerDefaultConfigDiscoversAllAir780Ports(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.autoDiscover {
		t.Fatal("empty Serial config must enable automatic discovery")
	}
	if got := manager.GetStatuses(); len(got) != 0 {
		t.Fatalf("startup statuses = %d, want 0 before devices are attached", len(got))
	}

	manager.listDiscoveryPorts = func(allowed []string) ([]string, error) {
		if allowed != nil {
			t.Fatalf("default discovery unexpectedly restricted to %v", allowed)
		}
		return []string{"COM3", "COM1", "COM2"}, nil
	}
	manager.probeDiscoveryPort = func(_ context.Context, port string) (air780ProbeResult, error) {
		if port == "COM2" {
			return air780ProbeResult{}, errors.New("not an Air780")
		}
		return air780ProbeResult{Project: air780Project, Version: "1.4.0", IMEI: port}, nil
	}
	var startedMu sync.Mutex
	started := make([]string, 0)
	manager.startSerialService = func(_ context.Context, service *SerialService) {
		startedMu.Lock()
		started = append(started, service.config.Port)
		startedMu.Unlock()
	}
	manager.registryMu.Lock()
	manager.started = true
	manager.runContext = context.Background()
	manager.registryMu.Unlock()

	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	statuses := manager.GetStatuses()
	if len(statuses) != 2 {
		t.Fatalf("discovered statuses = %d, want 2: %+v", len(statuses), statuses)
	}
	ports := []string{statuses[0].PortName, statuses[1].PortName}
	sort.Strings(ports)
	if !reflect.DeepEqual(ports, []string{"COM1", "COM3"}) {
		t.Fatalf("discovered ports = %v", ports)
	}
	startedMu.Lock()
	sort.Strings(started)
	if !reflect.DeepEqual(started, []string{"COM1", "COM3"}) {
		t.Fatalf("started ports = %v", started)
	}
	startedMu.Unlock()

	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	startedMu.Lock()
	defer startedMu.Unlock()
	if len(started) != 2 {
		t.Fatalf("repeat discovery started duplicate workers: %v", started)
	}
}

func TestSerialManagerAutoDiscoveryUsesOptionalPortAllowlist(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{
		AutoDiscover: boolConfig(true),
		Ports:        []string{" COM8 ", "COM8", "COM9"},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.listDiscoveryPorts = func(allowed []string) ([]string, error) {
		if !reflect.DeepEqual(allowed, []string{"COM8", "COM9"}) {
			t.Fatalf("allowlist = %v", allowed)
		}
		return []string{"COM8", "COM9"}, nil
	}
	probed := make([]string, 0)
	manager.probeDiscoveryPort = func(_ context.Context, port string) (air780ProbeResult, error) {
		probed = append(probed, port)
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(probed, []string{"COM8", "COM9"}) {
		t.Fatalf("probed ports = %v", probed)
	}
}

func TestAllowedDiscoveryPortsIncludeExplicitDeviceSymlink(t *testing.T) {
	const stablePath = "/dev/serial/by-id/usb-air780"
	allowed := map[string]string{
		canonicalSerialPort("COM8"):     "COM8",
		canonicalSerialPort(stablePath): stablePath,
	}
	result := selectAllowedDiscoveryPorts(
		allowed,
		[]string{"COM7", "COM8"},
		func(path string) bool { return path == stablePath },
	)
	if !reflect.DeepEqual(result, []string{stablePath, "COM8"}) {
		t.Fatalf("allowed discovery ports = %v", result)
	}
}

func TestDeduplicatePhysicalSerialPortsResolvesAliases(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first")
	alias := filepath.Join(directory, "alias")
	if err := os.WriteFile(first, []byte("device"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, alias); err != nil {
		t.Fatal(err)
	}
	result := deduplicatePhysicalSerialPorts([]string{first, alias})
	if !reflect.DeepEqual(result, []string{first}) {
		t.Fatalf("physical aliases were not deduplicated: %v", result)
	}
}

func TestSerialManagerAutoDiscoveryRemovesMissingEndpointAfterDebounce(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ports := []string{"COM8"}
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		return append([]string(nil), ports...), nil
	}
	manager.probeDiscoveryPort = func(context.Context, string) (air780ProbeResult, error) {
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	manager.startSerialService = func(context.Context, *SerialService) {}
	manager.registryMu.Lock()
	manager.started = true
	manager.runContext = context.Background()
	manager.registryMu.Unlock()
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ports = nil
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.GetStatuses()) != 1 {
		t.Fatal("endpoint was removed without the missing-scan debounce")
	}
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.GetStatuses()) != 0 {
		t.Fatal("missing endpoint was not removed after the debounce")
	}
}

func TestDiscoveryReclaimsPersistentlyDisconnectedEndpointAndBacksOff(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1000, 0)
	manager.now = func() time.Time { return clock }
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		return []string{"COM8"}, nil
	}
	probeCount := 0
	manager.probeDiscoveryPort = func(context.Context, string) (air780ProbeResult, error) {
		probeCount++
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	manager.startSerialService = func(context.Context, *SerialService) {}
	manager.registryMu.Lock()
	manager.started = true
	manager.runContext = context.Background()
	manager.registryMu.Unlock()

	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for scan := 1; scan < serialDiscoveryStallLimit; scan++ {
		if err := manager.discoverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	key := canonicalSerialPort("COM8")
	manager.registryMu.RLock()
	firstRetry := manager.probeRetryAfter[key]
	firstFailures := manager.probeFailures[key]
	endpointCount, serviceCount := len(manager.endpoints), len(manager.services)
	manager.registryMu.RUnlock()
	if endpointCount != 0 || serviceCount != 0 || firstFailures != 1 {
		t.Fatalf("after stalled endpoint: endpoints=%d services=%d failures=%d",
			endpointCount, serviceCount, firstFailures)
	}
	if want := clock.Add(serialDiscoveryRetryDelay); !firstRetry.Equal(want) {
		t.Fatalf("first retry = %v, want %v", firstRetry, want)
	}
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probeCount != 1 {
		t.Fatalf("probe ran during cooldown: count=%d", probeCount)
	}

	clock = firstRetry
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probeCount != 2 {
		t.Fatalf("probe did not resume after cooldown: count=%d", probeCount)
	}
	for scan := 1; scan < serialDiscoveryStallLimit; scan++ {
		if err := manager.discoverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	manager.registryMu.RLock()
	secondRetry := manager.probeRetryAfter[key]
	secondFailures := manager.probeFailures[key]
	manager.registryMu.RUnlock()
	if secondFailures != 2 {
		t.Fatalf("persistent connection failures were reset: failures=%d", secondFailures)
	}
	if want := clock.Add(2 * serialDiscoveryRetryDelay); !secondRetry.Equal(want) {
		t.Fatalf("second retry = %v, want %v", secondRetry, want)
	}
}

func TestDiscoveryOnlyBlocksRoutingAfterCandidatePassesProbe(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.discoveryReady.Store(true)
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		return []string{"COM8", "COM9"}, nil
	}
	entered := make(chan string, 2)
	release := map[string]chan struct{}{
		"COM8": make(chan struct{}),
		"COM9": make(chan struct{}),
	}
	manager.probeDiscoveryPort = func(_ context.Context, port string) (air780ProbeResult, error) {
		entered <- port
		<-release[port]
		if port == "COM8" {
			return air780ProbeResult{}, errors.New("not an Air780")
		}
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- manager.discoverOnce(context.Background()) }()
	waitForPort := func(want string) {
		t.Helper()
		select {
		case port := <-entered:
			if port != want {
				t.Fatalf("probe = %s, want %s", port, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for probe %s", want)
		}
	}

	waitForPort("COM8")
	if !manager.topologySettled() {
		t.Fatal("an unknown candidate must not pause an established topology")
	}
	routeLease := make(chan struct{})
	go func() {
		manager.routeGate.RLock()
		manager.routeGate.RUnlock()
		close(routeLease)
	}()
	select {
	case <-routeLease:
	case <-time.After(time.Second):
		t.Fatal("probe I/O held routeGate")
	}

	close(release["COM8"])
	waitForPort("COM9")
	if !manager.topologySettled() {
		t.Fatal("a candidate must remain outside topology until its probe succeeds")
	}
	close(release["COM9"])
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for discovery")
	}
	if manager.topologySettled() {
		t.Fatal("a verified Air780 endpoint must block routing until its SIM status settles")
	}
}

func TestDiscoveryEnumerationErrorKeepsLastTrustedTopology(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		return nil, errors.New("enumeration temporarily unavailable")
	}
	if err := manager.discoverOnce(context.Background()); err == nil {
		t.Fatal("expected initial discovery error")
	}
	if manager.discoveryReady.Load() {
		t.Fatal("initial failed scan must not open routing")
	}
	manager.discoveryReady.Store(true)
	if err := manager.discoverOnce(context.Background()); err == nil {
		t.Fatal("expected repeated discovery error")
	}
	if !manager.discoveryReady.Load() {
		t.Fatal("runtime enumeration error discarded the last trusted topology")
	}
}

func TestRouteWriteGuardRejectsReclaimedService(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{Port: "COM8"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := manager.snapshotServices()[0]
	manager.routeGate.Lock()
	manager.registryMu.Lock()
	delete(manager.services, service.DeviceID())
	manager.registryMu.Unlock()
	manager.routeGate.Unlock()
	called := false
	err = service.withRouteWriteGuard(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrSIMOffline) || called {
		t.Fatalf("reclaimed service guard err=%v called=%v", err, called)
	}
}

func TestSerialManagerDynamicServiceReceivesScheduledUpdater(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	updater := func(context.Context, string, models.LastRunStatus) error { return nil }
	manager.SetScheduledTaskStatusUpdater(updater)
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) { return []string{"COM8"}, nil }
	manager.probeDiscoveryPort = func(context.Context, string) (air780ProbeResult, error) {
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	services := manager.snapshotServices()
	if len(services) != 1 || services[0].getScheduledTaskStatusUpdater() == nil {
		t.Fatal("dynamic service did not inherit the scheduled-task updater")
	}
}

func TestAutoDiscoveredDevicesRouteSwappedCardsByICCID(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		return []string{"COM8", "COM9"}, nil
	}
	manager.probeDiscoveryPort = func(context.Context, string) (air780ProbeResult, error) {
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	if err := manager.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	one := discoveredServiceAtPort(t, manager, "COM8")
	two := discoveredServiceAtPort(t, manager, "COM9")
	if manager.topologySettled() {
		t.Fatal("new endpoints must block routing until their live status is confirmed")
	}
	setTestSIM(one, "8986000000000000001")
	setTestSIM(two, "8986000000000000002")
	if !manager.topologySettled() {
		t.Fatal("verified endpoints should settle automatic-discovery topology")
	}

	setTestSIM(one, "8986000000000000002")
	setTestSIM(two, "8986000000000000001")
	service, identity, err := manager.resolveSIM("iccid:8986000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if service != two || identity.ICCID != "8986000000000000001" {
		t.Fatalf("swapped card resolved to service=%v identity=%+v", service, identity)
	}
}

func TestStaticDevicesCanOmitDiagnosticIDAndName(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{
		Devices: []config.SerialDeviceConfig{{Port: "COM8"}, {Port: "COM9"}},
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	statuses := manager.GetStatuses()
	if len(statuses) != 2 || statuses[0].DeviceID == statuses[1].DeviceID {
		t.Fatalf("generated diagnostic identities are not unique: %+v", statuses)
	}
	for _, status := range statuses {
		if status.DeviceID == "" || status.DeviceName == "" {
			t.Fatalf("missing generated diagnostic identity: %+v", status)
		}
	}
}

func TestAutoDiscoveryRejectsLegacyDeviceConfiguration(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{
		AutoDiscover: boolConfig(true),
		Devices:      []config.SerialDeviceConfig{{ID: "legacy-name"}},
	}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error for mixed automatic and legacy configuration")
	}
}

func TestAutoDiscoveryRejectsEmptyPortAllowlist(t *testing.T) {
	_, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{
		AutoDiscover: boolConfig(true),
		Ports:        []string{},
	}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an explicitly empty automatic-discovery allowlist")
	}
}

func TestSerialManagerConcurrentDiscoveryAndReads(t *testing.T) {
	manager, err := NewSerialManager(zap.NewNop(), nil, config.SerialConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var generation atomic.Uint32
	manager.listDiscoveryPorts = func(_ []string) ([]string, error) {
		switch generation.Add(1) % 4 {
		case 0, 1:
			return []string{"COM8"}, nil
		default:
			return []string{"COM8", "COM9"}, nil
		}
	}
	manager.probeDiscoveryPort = func(context.Context, string) (air780ProbeResult, error) {
		return air780ProbeResult{Project: air780Project, Version: "1.4.0"}, nil
	}
	manager.startSerialService = func(context.Context, *SerialService) {}
	manager.registryMu.Lock()
	manager.started = true
	manager.runContext = context.Background()
	manager.registryMu.Unlock()

	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				_ = manager.GetStatuses()
				_, _ = manager.DefaultDevice()
				_, _ = manager.GetSIMs(context.Background())
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if err := manager.discoverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
}

func TestProbeProtocolRequiresCompleteMatchingProjectFrame(t *testing.T) {
	const requestID = "probe-123"
	invalid := [][]byte{
		[]byte(`{"type":"probe_response","project":"uart_sms_forwarder","request_id":"probe-123"}`),
		[]byte(`SMS_START:{"type":"heartbeat","project":"uart_sms_forwarder","version":"1.4.0","request_id":"probe-123"}:SMS_END`),
		[]byte(`SMS_START:{"type":"probe_response","project":"another_project","version":"1.4.0","request_id":"probe-123"}:SMS_END`),
		[]byte(`SMS_START:{"type":"probe_response","project":"uart_sms_forwarder","version":"1.4.0","request_id":"stale"}:SMS_END`),
		[]byte(`SMS_START:{"type":"probe_response","project":"uart_sms_forwarder","version":"1.4.0","request_id":"probe-123"}`),
	}
	for _, frame := range invalid {
		if _, ok := parseAir780ProbeFrame(frame, requestID); ok {
			t.Fatalf("accepted invalid discovery frame: %q", frame)
		}
	}
	valid := []byte(`SMS_START:{"type":"probe_response","project":"uart_sms_forwarder","version":"1.4.0","request_id":"probe-123","imei":" 86001 ","muid":"M1"}:SMS_END`)
	result, ok := parseAir780ProbeFrame(valid, requestID)
	if !ok || result.IMEI != "86001" || result.MUID != "M1" {
		t.Fatalf("valid discovery frame result = %+v, ok=%v", result, ok)
	}
}

func TestConsumeSMSFramesHandlesSplitAndMultipleFrames(t *testing.T) {
	first := []byte(`noiseSMS_START:{"type":"one"}:SMS_END\r\nSMS_STA`)
	frames, remaining := consumeSMSFrames(first)
	if len(frames) != 1 {
		t.Fatalf("first chunk frames = %d", len(frames))
	}
	second := append(remaining, []byte(`RT:{"type":"two"}:SMS_END\r\n`)...)
	frames, remaining = consumeSMSFrames(second)
	if len(frames) != 1 || len(remaining) >= len(smsPrefix) {
		t.Fatalf("second chunk frames=%d remaining=%q", len(frames), remaining)
	}
}

func TestProbeAir780PortHandlesShortWritesNoiseAndFragmentedFrames(t *testing.T) {
	port := newScriptedProbePort(7, true)
	opener := func(ctx context.Context, portName string, mode *serial.Mode) (serial.Port, error) {
		if portName != "COM8" {
			t.Fatalf("opened port = %s", portName)
		}
		if mode.InitialStatusBits == nil || mode.InitialStatusBits.DTR || mode.InitialStatusBits.RTS {
			t.Fatalf("unsafe initial modem bits: %+v", mode.InitialStatusBits)
		}
		return port, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := probeAir780PortWithOpener(ctx, "COM8", opener)
	if err != nil {
		t.Fatal(err)
	}
	if result.Project != air780Project || result.Version != "1.4.0" ||
		result.IMEI != "860001234567890" || result.MUID != "muid-1" {
		t.Fatalf("probe result = %+v", result)
	}
	if !port.isClosed() {
		t.Fatal("probe did not close serial port")
	}
	if port.writeCalls < 2 {
		t.Fatalf("short writes were not exercised: calls=%d", port.writeCalls)
	}
}

func TestProbeAir780PortCancellationClosesBlockedRead(t *testing.T) {
	port := newScriptedProbePort(0, false)
	opener := func(context.Context, string, *serial.Mode) (serial.Port, error) {
		return port, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := probeAir780PortWithOpener(ctx, "COM8", opener); err == nil {
		t.Fatal("expected canceled probe to fail")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled probe took too long: %v", elapsed)
	}
	if !port.isClosed() {
		t.Fatal("cancellation did not close blocked serial port")
	}
}

type scriptedProbePort struct {
	mu             sync.Mutex
	written        []byte
	reads          [][]byte
	maxWrite       int
	writeCalls     int
	responseQueued bool
	respond        bool
	closed         chan struct{}
	closeOnce      sync.Once
}

func newScriptedProbePort(maxWrite int, respond bool) *scriptedProbePort {
	return &scriptedProbePort{maxWrite: maxWrite, respond: respond, closed: make(chan struct{})}
}

func (p *scriptedProbePort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeCalls++
	n := len(data)
	if p.maxWrite > 0 && n > p.maxWrite {
		n = p.maxWrite
	}
	p.written = append(p.written, data[:n]...)
	if !p.respond || p.responseQueued || !bytes.Contains(p.written, []byte(":CMD_END\r\n")) {
		return n, nil
	}
	payload := strings.TrimPrefix(string(p.written), "CMD_START:")
	payload = strings.TrimSuffix(payload, ":CMD_END\r\n")
	var command map[string]string
	if json.Unmarshal([]byte(payload), &command) != nil || command["action"] != "probe" {
		return n, nil
	}
	requestID := command["request_id"]
	stale := fmt.Sprintf(
		`{"type":"probe_response","project":%q,"version":"1.4.0","request_id":"stale"}`,
		air780Project,
	)
	valid := fmt.Sprintf(
		`{"type":"probe_response","project":%q,"version":"1.4.0","request_id":%q,"imei":" 860001234567890 ","muid":"muid-1"}`,
		air780Project, requestID,
	)
	p.reads = [][]byte{
		[]byte("boot noiseSMS_START:" + stale + ":SMS_END\r\nSMS_STA"),
		[]byte("RT:" + valid + ":SMS_END\r\n"),
	}
	p.responseQueued = true
	return n, nil
}

func (p *scriptedProbePort) Read(destination []byte) (int, error) {
	p.mu.Lock()
	if len(p.reads) > 0 {
		chunk := p.reads[0]
		p.reads = p.reads[1:]
		n := copy(destination, chunk)
		if n < len(chunk) {
			p.reads = append([][]byte{append([]byte(nil), chunk[n:]...)}, p.reads...)
		}
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	<-p.closed
	return 0, io.EOF
}

func (p *scriptedProbePort) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (*scriptedProbePort) SetMode(*serial.Mode) error         { return nil }
func (*scriptedProbePort) Drain() error                       { return nil }
func (*scriptedProbePort) ResetInputBuffer() error            { return nil }
func (*scriptedProbePort) ResetOutputBuffer() error           { return nil }
func (*scriptedProbePort) SetDTR(bool) error                  { return nil }
func (*scriptedProbePort) SetRTS(bool) error                  { return nil }
func (*scriptedProbePort) SetReadTimeout(time.Duration) error { return nil }
func (*scriptedProbePort) Break(time.Duration) error          { return nil }
func (*scriptedProbePort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}

func (p *scriptedProbePort) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

func discoveredServiceAtPort(t *testing.T, manager *SerialManager, port string) *SerialService {
	t.Helper()
	for _, service := range manager.snapshotServices() {
		if service.config.Port == port {
			return service
		}
	}
	t.Fatalf("no discovered service at %s", port)
	return nil
}
