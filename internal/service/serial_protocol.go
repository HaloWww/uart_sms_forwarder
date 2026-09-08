package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	smsPrefix = "SMS_START:"
	smsSuffix = ":SMS_END"
)

var (
	errNotSMSFrame = errors.New("not sms frame")
	errMissingType = errors.New("message type missing")
)

type ParsedMessage struct {
	JSON    string
	Type    string
	Payload map[string]interface{}
}

func parseSMSFrame(data string) (*ParsedMessage, error) {
	if !strings.HasPrefix(data, smsPrefix) || !strings.HasSuffix(data, smsSuffix) {
		return nil, errNotSMSFrame
	}

	jsonData := data[len(smsPrefix) : len(data)-len(smsSuffix)]
	payload := make(map[string]interface{})
	if err := json.Unmarshal([]byte(jsonData), &payload); err != nil {
		return nil, fmt.Errorf("JSON解析失败: %w", err)
	}

	msgType, ok := payload["type"].(string)
	if !ok || msgType == "" {
		return nil, errMissingType
	}

	return &ParsedMessage{
		JSON:    jsonData,
		Type:    msgType,
		Payload: payload,
	}, nil
}

func buildCommandMessage(cmd any) ([]byte, string, error) {
	jsonData, err := json.Marshal(cmd)
	if err != nil {
		return nil, "", fmt.Errorf("JSON编码失败: %w", err)
	}

	message := fmt.Sprintf("CMD_START:%s:CMD_END\r\n", string(jsonData))
	return []byte(message), string(jsonData), nil
}
