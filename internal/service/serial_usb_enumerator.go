//go:build linux || windows || (darwin && cgo)

package service

import (
	"sort"
	"strings"

	"go.bug.st/serial/enumerator"
)

// listPassiveUSBSerialPorts 不传 active probe filter，只读取操作系统已经
// 提供的 USB 元数据。主动读取 USB 描述符可能干扰正在工作的串口设备。
func listPassiveUSBSerialPorts() ([]string, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(details))
	seen := make(map[string]struct{}, len(details))
	for _, detail := range details {
		if detail == nil || strings.TrimSpace(detail.Name) == "" {
			continue
		}
		if !detail.IsUSB && !looksLikeUSBSerialPort(detail.Name) {
			continue
		}
		key := canonicalSerialPort(detail.Name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, strings.TrimSpace(detail.Name))
	}
	sort.Strings(result)
	return result, nil
}
