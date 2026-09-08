package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

// SerialHandler 串口控制API处理器
type SerialHandler struct {
	logger        *zap.Logger
	serialManager *service.SerialManager
}

// NewSerialHandler 创建串口Handler实例
func NewSerialHandler(logger *zap.Logger, serialManager *service.SerialManager) *SerialHandler {
	return &SerialHandler{
		logger:        logger,
		serialManager: serialManager,
	}
}

// SendSMSRequest 发送短信请求
type SendSMSRequest struct {
	SIMID     string `json:"simId"`
	To        string `json:"to"`
	Content   string `json:"content"`
	RequestID string `json:"requestId"`
}

// SendSMS 发送短信
// POST /api/serial/sms
// Body: {"simId":"iccid:8986...","to":"13800138000","content":"测试短信","requestId":"UUID"}
func (h *SerialHandler) SendSMS(c *echo.Context) error {
	var req SendSMSRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": "请求参数错误",
		})
	}

	req.SIMID = strings.TrimSpace(req.SIMID)
	req.To = strings.TrimSpace(req.To)
	if req.SIMID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "sim_id_required",
			"error": "必须指定 SIM",
		})
	}
	if req.To == "" || req.Content == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": "手机号和内容不能为空",
		})
	}
	if strings.Contains(req.Content, ":CMD_END") {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_content",
			"error": "短信内容包含串口协议保留标记",
		})
	}

	bodyRequestID, bodyRequestIDErr := normalizeSMSRequestID(req.RequestID)
	headerRequestID, headerRequestIDErr := normalizeSMSRequestID(c.Request().Header.Get("Idempotency-Key"))
	if bodyRequestIDErr != nil || headerRequestIDErr != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "request_id_invalid", "error": "requestId 必须是有效的 UUID",
		})
	}
	if bodyRequestID != "" && headerRequestID != "" && bodyRequestID != headerRequestID {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "request_id_mismatch", "error": "requestId 与 Idempotency-Key 不一致",
		})
	}
	requestID := bodyRequestID
	if requestID == "" {
		requestID = headerRequestID
	}
	if requestID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "request_id_required", "error": "必须指定 requestId 或 Idempotency-Key",
		})
	}
	messageID, err := h.serialManager.SendSMSWithRequestID(req.SIMID, req.To, req.Content, requestID)
	if err != nil {
		if errors.Is(err, service.ErrSMSSubmissionAmbiguous) {
			return c.JSON(http.StatusAccepted, map[string]string{
				"message":   "串口写入状态不确定，短信可能已经发送，请勿立即重试",
				"messageId": messageID,
				"status":    string(models.MessageStatusAmbiguous),
			})
		}
		if errors.Is(err, service.ErrSMSRequestPreviouslyFailed) {
			return c.JSON(http.StatusConflict, map[string]string{
				"code":      "request_already_failed",
				"error":     "该 requestId 对应的短信请求已失败",
				"messageId": messageID,
				"status":    string(models.MessageStatusFailed),
			})
		}
		h.logger.Error("发送短信失败", zap.Error(err))
		return writeServiceError(c, err, http.StatusServiceUnavailable, "send_failed", "短信提交失败")
	}

	return c.JSON(http.StatusAccepted, map[string]string{
		"message":   "短信已提交发送",
		"messageId": messageID,
	})
}

func normalizeSMSRequestID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// GetStatus 获取设备状态（包含移动网络信息）
// GET /api/serial/status
func (h *SerialHandler) GetStatus(c *echo.Context) error {
	simID := strings.TrimSpace(c.QueryParam("simId"))
	deviceID := strings.TrimSpace(c.QueryParam("deviceId"))
	if simID != "" && deviceID != "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "ambiguous_selector",
			"error": "simId 和 deviceId 不能同时指定",
		})
	}

	var data *service.StatusData
	var err error
	if simID != "" {
		data, err = h.serialManager.GetStatusBySIM(simID)
	} else {
		// 状态查询保留旧 deviceId 语义；不指定时仍查询默认物理连接。
		data, err = h.serialManager.GetDeviceStatus(deviceID)
	}
	if err != nil {
		return writeServiceError(c, err, http.StatusNotFound, "device_not_found", "设备不存在或未启用")
	}

	return c.JSON(http.StatusOK, data)
}

// GetDevices 返回全部已配置设备及其实时状态。
func (h *SerialHandler) GetDevices(c *echo.Context) error {
	return c.JSON(http.StatusOK, h.serialManager.GetStatuses())
}

// GetSIMs 返回持久化的 SIM 档案及其实时路由状态，离线 SIM 也会保留。
func (h *SerialHandler) GetSIMs(c *echo.Context) error {
	sims, err := h.serialManager.GetSIMs(c.Request().Context())
	if err != nil {
		h.logger.Error("获取 SIM 列表失败", zap.Error(err))
		return writeServiceError(c, err, http.StatusInternalServerError, "list_sims_failed", "获取 SIM 列表失败")
	}
	if sims == nil {
		sims = []service.SIMStatus{}
	}
	return c.JSON(http.StatusOK, sims)
}

// SetFlymodeRequest 设置飞行模式请求
type SetFlymodeRequest struct {
	SIMID   string `json:"simId"`
	Enabled bool   `json:"enabled"`
}

// SetFlymode 设置飞行模式
// POST /api/serial/flymode
// Body: {"enabled": true}
func (h *SerialHandler) SetFlymode(c *echo.Context) error {
	var req SetFlymodeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "请求参数错误",
		})
	}

	req.SIMID = strings.TrimSpace(req.SIMID)
	if req.SIMID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "sim_id_required",
			"error": "必须指定 SIM",
		})
	}
	err := h.serialManager.SetFlymode(req.SIMID, req.Enabled)
	if err != nil {
		h.logger.Error("设置飞行模式失败", zap.Error(err))
		return writeServiceError(c, err, http.StatusServiceUnavailable, "flymode_failed", "设置飞行模式失败")
	}
	return c.JSON(http.StatusOK, map[string]any{})
}

// RebootMcu 重启模块
// POST /api/serial/reboot
func (h *SerialHandler) RebootMcu(c *echo.Context) error {
	var req struct {
		SIMID    string `json:"simId"`
		DeviceID string `json:"deviceId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "请求参数错误"})
	}
	req.SIMID = strings.TrimSpace(req.SIMID)
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	if req.SIMID != "" && req.DeviceID != "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "ambiguous_selector",
			"error": "simId 和 deviceId 不能同时指定",
		})
	}
	err := h.serialManager.RebootMcu(req.SIMID, req.DeviceID)
	if err != nil {
		h.logger.Error("重启模块", zap.Error(err))
		return writeServiceError(c, err, http.StatusServiceUnavailable, "reboot_failed", "重启模块失败")
	}
	return c.JSON(http.StatusOK, map[string]any{})
}
