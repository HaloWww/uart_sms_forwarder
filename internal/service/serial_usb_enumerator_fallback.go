//go:build !linux && !windows && (!darwin || !cgo)

package service

import "errors"

var errPassiveUSBEnumerationUnavailable = errors.New("当前构建不支持 USB 串口详细枚举")

func listPassiveUSBSerialPorts() ([]string, error) {
	return nil, errPassiveUSBEnumerationUnavailable
}
