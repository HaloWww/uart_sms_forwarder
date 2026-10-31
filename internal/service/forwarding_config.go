package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const MaxForwardingConfigBytes = 3800

var (
	ErrForwardingConfigInvalid      = errors.New("独立转发配置无效")
	ErrForwardingConfigNotPersisted = errors.New("板子未确认持久保存配置")
	forwardingChannelID             = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// ForwardingConfig is a complete device-side configuration, not a host property.
// Credentials travel only on authenticated requests and the UART response;
// they must never be copied into public status snapshots or logs.
type ForwardingConfig struct {
	Mode                  string              `json:"mode"`
	APN                   string              `json:"apn"`
	QueueLimit            int                 `json:"queue_limit"`
	MaxAttempts           int                 `json:"max_attempts"`
	RetryBaseSeconds      int                 `json:"retry_base_seconds"`
	RetryMaxSeconds       int                 `json:"retry_max_seconds"`
	MinIntervalSeconds    int                 `json:"min_interval_seconds"`
	StatsSaveSeconds      int                 `json:"stats_save_seconds"`
	TimezoneOffsetSeconds int                 `json:"timezone_offset_seconds"`
	Template              string              `json:"template"`
	Channels              []ForwardingChannel `json:"channels"`
}

type ForwardingChannel struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	Server  string `json:"server,omitempty"`
	Key     string `json:"key,omitempty"`
	Title   string `json:"title,omitempty"`
	Group   string `json:"group,omitempty"`
	CorpID  string `json:"corp_id,omitempty"`
	AgentID int64  `json:"agent_id,omitempty"`
	Secret  string `json:"secret,omitempty"`
	ToUser  string `json:"to_user,omitempty"`
	URL     string `json:"url,omitempty"`
	CACert  string `json:"ca_cert,omitempty"`
}

type ForwardingConfigState struct {
	Config    ForwardingConfig `json:"config"`
	Persisted bool             `json:"persisted"`
}

func ValidateForwardingConfig(config ForwardingConfig) error {
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrForwardingConfigInvalid, reason) }
	if config.Mode != "uart" && config.Mode != "direct" {
		return invalid("mode 必须为 uart 或 direct")
	}
	if config.QueueLimit < 1 || config.QueueLimit > 8 {
		return invalid("queue_limit 必须在 1~8 之间")
	}
	if config.MaxAttempts < 1 || config.MaxAttempts > 10 {
		return invalid("max_attempts 必须在 1~10 之间")
	}
	if config.RetryBaseSeconds < 5 || config.RetryBaseSeconds > 3600 {
		return invalid("retry_base_seconds 必须在 5~3600 之间")
	}
	if config.RetryMaxSeconds < config.RetryBaseSeconds || config.RetryMaxSeconds > 86400 {
		return invalid("retry_max_seconds 不能小于基础间隔且不能超过 86400")
	}
	if config.MinIntervalSeconds < 1 || config.MinIntervalSeconds > 3600 {
		return invalid("min_interval_seconds 必须在 1~3600 之间")
	}
	if config.StatsSaveSeconds < 10 || config.StatsSaveSeconds > 3600 {
		return invalid("stats_save_seconds 必须在 10~3600 之间")
	}
	if config.TimezoneOffsetSeconds < -43200 || config.TimezoneOffsetSeconds > 50400 {
		return invalid("timezone_offset_seconds 超出范围")
	}
	if len(config.APN) > 100 || strings.ContainsAny(config.APN, "\r\n\x00") {
		return invalid("APN 过长或包含非法字符")
	}
	if strings.TrimSpace(config.Template) == "" || len(config.Template) > 1024 {
		return invalid("模板不能为空且不能超过 1024 个 UTF-8 字节")
	}
	if len(config.Channels) > 3 {
		return invalid("最多配置 3 个独立转发渠道")
	}
	ids := make(map[string]bool)
	enabled := 0
	for _, channel := range config.Channels {
		if !forwardingChannelID.MatchString(channel.ID) || ids[channel.ID] {
			return invalid("渠道 id 需唯一，仅可使用 1~40 位字母、数字、下划线和短横线")
		}
		ids[channel.ID] = true
		if channel.Type != "bark" && channel.Type != "wecom_app" && channel.Type != "webhook" {
			return invalid("独立模式仅支持 Bark、企业微信应用和 Webhook")
		}
		if channel.AgentID < 0 || channel.AgentID > 2147483647 {
			return invalid("企业微信应用 ID 超出设备支持的范围")
		}
		for _, value := range []string{channel.Server, channel.Key, channel.Title, channel.Group, channel.CorpID, channel.Secret, channel.ToUser, channel.URL, channel.CACert} {
			if strings.ContainsRune(value, '\x00') {
				return invalid("渠道配置包含非法字符")
			}
		}
		if !channel.Enabled {
			continue
		}
		enabled++
		switch channel.Type {
		case "bark":
			if !validForwardingHTTPS(channel.Server) || strings.TrimSpace(channel.Key) == "" {
				return invalid("Bark 需要 HTTPS 服务地址和设备 Key")
			}
		case "wecom_app":
			if strings.TrimSpace(channel.CorpID) == "" || strings.TrimSpace(channel.Secret) == "" || strings.TrimSpace(channel.ToUser) == "" || channel.AgentID < 1 {
				return invalid("企业微信应用需要企业 ID、应用 ID、Secret 和接收成员")
			}
		case "webhook":
			if !validForwardingHTTPS(channel.URL) {
				return invalid("Webhook 地址必须为 HTTPS")
			}
		}
	}
	if config.Mode == "direct" && enabled == 0 {
		return invalid("启用独立模式前至少启用一个渠道")
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return invalid("配置不能编码")
	}
	if len(encoded) > MaxForwardingConfigBytes {
		return invalid("配置超过板子 3800 字节持久存储上限，请缩短模板或证书")
	}
	if bytes.Contains(encoded, []byte(":CMD_END")) || bytes.Contains(encoded, []byte(":SMS_END")) {
		return invalid("配置包含串口协议保留标记")
	}
	return nil
}

func validForwardingHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(raw, "\r\n\x00@")
}

func supportsForwardingConfigProtocol(version string) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	if len(parts) != 3 {
		return false
	}
	parsed := make([]int, 3)
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return false
		}
		parsed[i] = value
	}
	return parsed[0] > 1 || parsed[0] == 1 && parsed[1] >= 6
}

func (m *SerialManager) GetForwardingConfig(ctx context.Context, simID string) (*ForwardingConfigState, error) {
	s, expected, err := m.resolveLiveSIM(simID)
	if err != nil {
		return nil, err
	}
	return s.forwardingConfig(ctx, expected, nil)
}

func (m *SerialManager) SetForwardingConfig(ctx context.Context, simID string, config ForwardingConfig) (*ForwardingConfigState, error) {
	if config.Channels == nil {
		config.Channels = []ForwardingChannel{}
	}
	if err := ValidateForwardingConfig(config); err != nil {
		return nil, err
	}
	s, expected, err := m.resolveLiveSIM(simID)
	if err != nil {
		return nil, err
	}
	return s.forwardingConfig(ctx, expected, &config)
}

func (s *SerialService) forwardingConfig(ctx context.Context, expected SIMIdentity, wanted *ForwardingConfig) (*ForwardingConfigState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.smsSendMu.Lock()
	defer s.smsSendMu.Unlock()
	var state *ForwardingConfigState
	err := s.withRouteWriteGuard(func() error {
		validateRoute := func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !supportsForwardingConfigProtocol(s.rememberScriptVersion("")) {
				return fmt.Errorf("%w: 修改独立转发配置需要 main.lua 1.6.0 或更高版本", ErrSIMProtocolOutdated)
			}
			validator, _ := s.getSIMRouteCallbacks()
			if validator != nil {
				if err := validator(s.deviceID, expected); err != nil {
					return err
				}
			}
			return s.ensureSIMIdentity(expected)
		}
		if err := validateRoute(); err != nil {
			return err
		}
		if wanted != nil {
			if s.hasPendingSMS() {
				return ErrSMSOperationPending
			}
			payload, err := s.forwardingCommand(ctx, map[string]any{"action": "set_forward_config", "expected_iccid": expected.ICCID, "config": *wanted})
			if err != nil {
				return err
			}
			if persisted, _ := payload["persisted"].(bool); !persisted {
				return ErrForwardingConfigNotPersisted
			}
		}
		if err := validateRoute(); err != nil {
			return err
		}
		payload, err := s.forwardingCommand(ctx, map[string]any{"action": "get_forward_config", "expected_iccid": expected.ICCID})
		if err != nil {
			return err
		}
		configuration, ok := payload["config"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("%w: 设备未返回配置", ErrControlCommandRejected)
		}
		// LuatOS encodes an empty Lua table as {}, including an empty channels list.
		if empty, ok := configuration["channels"].(map[string]interface{}); ok && len(empty) == 0 {
			configuration["channels"] = []ForwardingChannel{}
		}
		encoded, err := json.Marshal(configuration)
		if err != nil {
			return fmt.Errorf("%w: 设备返回的配置无效", ErrControlCommandRejected)
		}
		var result ForwardingConfigState
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result.Config); err != nil {
			return fmt.Errorf("%w: 设备返回的配置无法解析", ErrControlCommandRejected)
		}
		if err := ValidateForwardingConfig(result.Config); err != nil {
			return fmt.Errorf("%w: 设备返回的配置未通过校验", ErrControlCommandRejected)
		}
		result.Persisted, _ = payload["persisted"].(bool)
		if wanted != nil && (!result.Persisted || !reflect.DeepEqual(result.Config, *wanted)) {
			return fmt.Errorf("%w: 保存后的回读不一致，请重新读取设备确认", ErrForwardingConfigNotPersisted)
		}
		if err := validateRoute(); err != nil {
			return err
		}
		state = &result
		return nil
	})
	if err != nil {
		return nil, err
	}
	if wanted != nil {
		s.RequestCacheUpdate()
	}
	return state, nil
}

func (s *SerialService) forwardingCommand(ctx context.Context, command map[string]any) (map[string]interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestID := uuid.NewString()
	command["request_id"] = requestID
	responseCh := make(chan controlCommandResponse, 1)
	s.pendingControls.Store(requestID, responseCh)
	defer s.pendingControls.Delete(requestID)
	if err := s.sendJSONCommand(command); err != nil {
		return nil, err
	}
	timer := time.NewTimer(controlCommandResponseTimeout)
	defer timer.Stop()
	select {
	case response := <-responseCh:
		if response.Action != command["action"] {
			return nil, fmt.Errorf("%w: 设备响应动作不匹配", ErrControlCommandRejected)
		}
		if response.Result != "ok" {
			switch response.Error {
			case "sim_identity_mismatch":
				return nil, ErrSIMMismatch
			case "sim_identity_unavailable":
				return nil, ErrSIMOffline
			case "expected_iccid_required":
				return nil, ErrSIMIdentityRequired
			case "sms_operation_pending", "direct_forward_busy":
				return nil, ErrSMSOperationPending
			case "storage_unavailable", "storage_write_failed", "storage_verify_failed":
				return nil, ErrForwardingConfigNotPersisted
			default:
				return nil, fmt.Errorf("%w: 板子拒绝配置操作，请检查渠道、存储和脚本版本", ErrControlCommandRejected)
			}
		}
		return response.Payload, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("配置结果未确认，请重新读取设备确认: %w", ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("%w: 配置可能已写入，请重新读取设备确认", ErrControlCommandTimeout)
	}
}

// DecodeForwardingConfigRequest rejects unknown fields and trailing JSON so a
// host-only proxy option cannot silently become a supposedly persisted setting.
func DecodeForwardingConfigRequest(reader io.Reader) (string, ForwardingConfig, error) {
	var request struct {
		SIMID  string           `json:"simId"`
		Config ForwardingConfig `json:"config"`
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 16*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return "", ForwardingConfig{}, fmt.Errorf("%w: 请求字段或 JSON 格式无效", ErrForwardingConfigInvalid)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return "", ForwardingConfig{}, fmt.Errorf("%w: 请求只能包含一份 JSON", ErrForwardingConfigInvalid)
	}
	if err := ValidateForwardingConfig(request.Config); err != nil {
		return "", ForwardingConfig{}, err
	}
	return strings.TrimSpace(request.SIMID), request.Config, nil
}
