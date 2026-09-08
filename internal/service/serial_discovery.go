package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.bug.st/serial"
)

const (
	air780Project              = "uart_sms_forwarder"
	serialDiscoveryInterval    = 5 * time.Second
	serialDiscoveryMissLimit   = 2
	serialDiscoveryStallLimit  = 6
	serialDiscoveryRetryDelay  = 15 * time.Second
	serialDiscoveryMaxRetry    = 5 * time.Minute
	serialDiscoveryProbeWindow = 2 * time.Second
	serialDiscoveryReadTimeout = 200 * time.Millisecond
	serialDiscoveryMaxBuffer   = 64 * 1024
)

type air780ProbeResult struct {
	Project string
	Version string
	IMEI    string
	MUID    string
}

type air780ProbeResponse struct {
	Type      string `json:"type"`
	Project   string `json:"project"`
	Version   string `json:"version"`
	RequestID string `json:"request_id"`
	IMEI      string `json:"imei"`
	MUID      string `json:"muid"`
}

// listAutoDiscoveryPorts 仅使用系统已经公开的被动 USB 元数据筛选候选口。
// enumerator 的主动 USB 探测会打开设备并可能干扰其他硬件，因此这里不传 filter。
func listAutoDiscoveryPorts(allowedPorts []string) ([]string, error) {
	allowed := make(map[string]string, len(allowedPorts))
	for _, portName := range allowedPorts {
		portName = strings.TrimSpace(portName)
		if portName != "" {
			allowed[canonicalSerialPort(portName)] = portName
		}
	}

	// 显式白名单的意义是“用户允许探测这些端口”。此时无需依赖 USB
	// 元数据，仍只探测操作系统当前能看到的白名单成员。
	if allowedPorts != nil {
		ports, err := serial.GetPortsList()
		if err != nil {
			return nil, err
		}
		var pathIsDevice func(string) bool
		if runtime.GOOS != "windows" {
			// GetPortsList 不一定返回 /dev/serial/by-id 等稳定软链接；显式
			// 白名单已代表用户授权，存在且指向设备节点时也应纳入候选。
			pathIsDevice = func(path string) bool {
				info, statErr := os.Stat(path)
				return statErr == nil && info.Mode()&os.ModeDevice != 0
			}
		}
		result := selectAllowedDiscoveryPorts(allowed, ports, pathIsDevice)
		if runtime.GOOS != "windows" {
			result = deduplicatePhysicalSerialPorts(result)
		}
		return result, nil
	}

	result, detailsErr := listPassiveUSBSerialPorts()
	if detailsErr == nil {
		return result, nil
	}

	// 某些 BSD 平台没有详细枚举器；Linux/macOS 仍可按系统 USB 串口
	// 命名安全降级。Windows 的 COM 名称本身无法区分 USB，拒绝盲探全部。
	ports, portsErr := serial.GetPortsList()
	if portsErr != nil {
		return nil, errors.Join(detailsErr, portsErr)
	}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("无法读取 USB 串口信息，请配置 Serial.Ports 白名单: %w", detailsErr)
	}
	result = make([]string, 0, len(ports))
	for _, portName := range ports {
		if looksLikeUSBSerialPort(portName) {
			result = append(result, strings.TrimSpace(portName))
		}
	}
	sort.Strings(result)
	return result, nil
}

func selectAllowedDiscoveryPorts(
	allowed map[string]string,
	listed []string,
	pathIsDevice func(string) bool,
) []string {
	result := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, portName := range listed {
		key := canonicalSerialPort(portName)
		if _, ok := allowed[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, strings.TrimSpace(portName))
	}
	if pathIsDevice != nil {
		for key, portName := range allowed {
			if _, ok := seen[key]; ok || !pathIsDevice(portName) {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, portName)
		}
	}
	sort.Strings(result)
	return result
}

func deduplicatePhysicalSerialPorts(ports []string) []string {
	result := make([]string, 0, len(ports))
	infos := make([]os.FileInfo, 0, len(ports))
	for _, portName := range ports {
		info, err := os.Stat(portName)
		duplicate := false
		if err == nil {
			for _, existing := range infos {
				if os.SameFile(info, existing) {
					duplicate = true
					break
				}
			}
		}
		if duplicate {
			continue
		}
		result = append(result, portName)
		if err == nil {
			infos = append(infos, info)
		}
	}
	return result
}

func looksLikeUSBSerialPort(portName string) bool {
	name := strings.ToLower(strings.TrimSpace(portName))
	return strings.Contains(name, "ttyusb") ||
		strings.Contains(name, "ttyacm") ||
		strings.Contains(name, "usbserial") ||
		strings.Contains(name, "usbmodem") ||
		strings.Contains(name, "/dev/cu.usb") ||
		strings.Contains(name, "/dev/tty.usb") ||
		strings.Contains(name, "/dev/cuau") ||
		strings.Contains(name, "/dev/ttyu")
}

func canonicalSerialPort(portName string) string {
	portName = strings.TrimSpace(portName)
	if runtime.GOOS == "windows" {
		return strings.ToLower(portName)
	}
	return portName
}

// probeAir780Port 使用带随机 request_id 的项目专用、无副作用握手。
// 只有完整协议帧的 type/project/request_id 全部匹配才会认领该串口。
func probeAir780Port(ctx context.Context, portName string) (air780ProbeResult, error) {
	return probeAir780PortWithOpener(ctx, portName, openSerialPortWithContext)
}

type serialPortOpener func(context.Context, string, *serial.Mode) (serial.Port, error)

func probeAir780PortWithOpener(
	ctx context.Context,
	portName string,
	opener serialPortOpener,
) (air780ProbeResult, error) {
	requestID := uuid.NewString()
	probeCtx, cancel := context.WithTimeout(ctx, serialDiscoveryProbeWindow)
	defer cancel()
	if err := probeCtx.Err(); err != nil {
		return air780ProbeResult{}, err
	}
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
		// 探测未知 USB 串口时不主动拉高 DTR/RTS，减少复位或控制其他硬件的风险。
		InitialStatusBits: &serial.ModemOutputBits{},
	}
	port, err := opener(probeCtx, portName, mode)
	if err != nil {
		return air780ProbeResult{}, err
	}
	var closeOnce sync.Once
	closePort := func() { closeOnce.Do(func() { _ = port.Close() }) }
	stopCloseWatcher := make(chan struct{})
	closeWatcherDone := make(chan struct{})
	go func() {
		defer close(closeWatcherDone)
		select {
		case <-probeCtx.Done():
			closePort()
		case <-stopCloseWatcher:
		}
	}()
	defer func() {
		close(stopCloseWatcher)
		<-closeWatcherDone
		closePort()
	}()
	if err := port.SetReadTimeout(serialDiscoveryReadTimeout); err != nil {
		return air780ProbeResult{}, err
	}
	message, _, err := buildCommandMessage(map[string]string{
		"action":     "probe",
		"request_id": requestID,
	})
	if err != nil {
		return air780ProbeResult{}, err
	}
	if err := writeAll(port, message); err != nil {
		return air780ProbeResult{}, err
	}

	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		select {
		case <-probeCtx.Done():
			return air780ProbeResult{}, fmt.Errorf("Air780 探测超时: %w", probeCtx.Err())
		default:
		}

		n, readErr := port.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			frames, remaining := consumeSMSFrames(buffer)
			buffer = remaining
			for _, frame := range frames {
				if result, ok := parseAir780ProbeFrame(frame, requestID); ok {
					return result, nil
				}
			}
			if len(buffer) > serialDiscoveryMaxBuffer {
				return air780ProbeResult{}, fmt.Errorf("Air780 探测响应超过 %d 字节", serialDiscoveryMaxBuffer)
			}
		}
		if readErr != nil {
			return air780ProbeResult{}, readErr
		}
	}
}

type serialOpenResult struct {
	port serial.Port
	err  error
}

var serialOpenFlights = struct {
	sync.Mutex
	ports map[string]struct{}
}{ports: make(map[string]struct{})}

// serial.Open 本身没有 context API。把它隔离后，发现协调器至少能按时
// 释放路由闸门；若驱动迟到返回，后台接收者会立即关闭句柄。
func openSerialPortWithContext(
	ctx context.Context, portName string, mode *serial.Mode,
) (serial.Port, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := canonicalSerialPort(portName)
	serialOpenFlights.Lock()
	if _, opening := serialOpenFlights.ports[key]; opening {
		serialOpenFlights.Unlock()
		return nil, fmt.Errorf("串口 %s 仍有一次打开操作未返回", portName)
	}
	serialOpenFlights.ports[key] = struct{}{}
	serialOpenFlights.Unlock()

	// 无缓冲通道很重要：调用方一旦因 context 返回，Open goroutine 就只
	// 能走取消分支关闭迟到句柄，不会把无人接收的端口遗留在缓冲区里。
	resultCh := make(chan serialOpenResult)
	go func() {
		port, err := serial.Open(portName, mode)
		result := serialOpenResult{port: port, err: err}
		select {
		case resultCh <- result:
		case <-ctx.Done():
			if port != nil {
				_ = port.Close()
			}
		}
		serialOpenFlights.Lock()
		delete(serialOpenFlights.ports, key)
		serialOpenFlights.Unlock()
	}()
	select {
	case result := <-resultCh:
		return result.port, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func parseAir780ProbeFrame(frame []byte, requestID string) (air780ProbeResult, bool) {
	msg, err := parseSMSFrame(string(frame))
	if err != nil {
		return air780ProbeResult{}, false
	}
	var response air780ProbeResponse
	if json.Unmarshal([]byte(msg.JSON), &response) != nil ||
		response.Type != "probe_response" ||
		response.Project != air780Project ||
		response.RequestID != requestID ||
		strings.TrimSpace(response.Version) == "" {
		return air780ProbeResult{}, false
	}
	return air780ProbeResult{
		Project: response.Project,
		Version: response.Version,
		IMEI:    normalizeIdentityValue(response.IMEI),
		MUID:    normalizeIdentityValue(response.MUID),
	}, true
}

// consumeSMSFrames 从任意分片中提取完整上行协议帧，并保留可能不完整的尾部。
func consumeSMSFrames(buffer []byte) ([][]byte, []byte) {
	prefix := []byte(smsPrefix)
	suffix := []byte(smsSuffix)
	frames := make([][]byte, 0)
	for {
		start := bytes.Index(buffer, prefix)
		if start < 0 {
			keep := len(prefix) - 1
			if len(buffer) <= keep {
				return frames, buffer
			}
			return frames, append([]byte(nil), buffer[len(buffer)-keep:]...)
		}
		if start > 0 {
			buffer = buffer[start:]
		}
		endOffset := bytes.Index(buffer[len(prefix):], suffix)
		if endOffset < 0 {
			return frames, append([]byte(nil), buffer...)
		}
		end := len(prefix) + endOffset + len(suffix)
		frames = append(frames, append([]byte(nil), buffer[:end]...))
		buffer = buffer[end:]
	}
}
