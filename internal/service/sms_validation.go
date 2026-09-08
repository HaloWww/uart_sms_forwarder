package service

import (
	"errors"
	"fmt"
	"strings"
)

// MaxSMSContentBytes 采用保守的 UTF-8 上限，既远低于 LuatOS sendLong 的
// 134 字节 × 128 分片边界，也保证最坏 JSON 转义后仍装得进 16 KiB 串口帧。
const MaxSMSContentBytes = 2 * 1024

// MaxSMSDestinationBytes 给国际号码、服务号码和前导 + 留出余量，同时阻止
// 异常输入无界放大串口命令。
const MaxSMSDestinationBytes = 64

var (
	ErrSMSRequestIDInvalid        = errors.New("短信 requestId 必须是有效的 UUID")
	ErrSMSRequestPreviouslyFailed = errors.New("该 requestId 对应的短信请求已失败")
	ErrSMSContentTooLong          = errors.New("短信内容超过安全长度上限")
	ErrSMSDestinationInvalid      = errors.New("短信目标号码无效")
)

func ValidateSMSContent(content string) error {
	if len([]byte(content)) > MaxSMSContentBytes {
		return fmt.Errorf("%w: 最多 %d 字节", ErrSMSContentTooLong, MaxSMSContentBytes)
	}
	return nil
}

func ValidateSMSDestination(destination string) error {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return fmt.Errorf("%w: 号码不能为空", ErrSMSDestinationInvalid)
	}
	if len([]byte(destination)) > MaxSMSDestinationBytes {
		return fmt.Errorf("%w: 最多 %d 字节", ErrSMSDestinationInvalid, MaxSMSDestinationBytes)
	}
	if strings.Contains(destination, ":CMD_END") {
		return fmt.Errorf("%w: 包含串口协议保留标记", ErrSMSDestinationInvalid)
	}
	return nil
}
