package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

type ScheduledTaskHandler struct {
	logger           *zap.Logger
	schedulerService *service.SchedulerService
}

type TriggerScheduledTaskRequest struct {
	RequestID string `json:"requestId"`
}

func NewScheduledTaskHandler(logger *zap.Logger, schedulerService *service.SchedulerService) *ScheduledTaskHandler {
	return &ScheduledTaskHandler{
		logger:           logger,
		schedulerService: schedulerService,
	}
}

// List 获取所有定时任务
func (h *ScheduledTaskHandler) List(c *echo.Context) error {
	ctx := c.Request().Context()

	tasks, err := h.schedulerService.GetAll(ctx)
	if err != nil {
		h.logger.Error("获取定时任务列表失败", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "获取任务列表失败",
		})
	}

	// 如果为空，返回空数组而不是 null
	if tasks == nil {
		tasks = []models.ScheduledTask{}
	}

	return c.JSON(http.StatusOK, tasks)
}

// Get 根据ID获取定时任务
func (h *ScheduledTaskHandler) Get(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	task, err := h.schedulerService.GetById(ctx, id)
	if err != nil {
		h.logger.Error("获取定时任务失败", zap.String("id", id), zap.Error(err))
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "任务不存在",
		})
	}

	return c.JSON(http.StatusOK, task)
}

// Create 创建定时任务
func (h *ScheduledTaskHandler) Create(c *echo.Context) error {
	ctx := c.Request().Context()

	var task models.ScheduledTask
	if err := c.Bind(&task); err != nil {
		h.logger.Error("解析请求失败", zap.Error(err))
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": "请求参数错误",
		})
	}

	// 验证必填字段
	if err := h.validateTask(&task); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": err.Error(),
		})
	}

	// 创建任务
	if err := h.schedulerService.Create(ctx, &task); err != nil {
		h.logger.Error("创建定时任务失败", zap.Error(err))
		return writeServiceError(c, err, http.StatusInternalServerError, "create_task_failed", "创建任务失败")
	}

	h.logger.Info("定时任务创建成功", zap.String("id", task.ID), zap.String("name", task.Name))

	return c.JSON(http.StatusCreated, task)
}

// Update 更新定时任务
func (h *ScheduledTaskHandler) Update(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	var task models.ScheduledTask
	if err := c.Bind(&task); err != nil {
		h.logger.Error("解析请求失败", zap.Error(err))
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": "请求参数错误",
		})
	}

	// 验证必填字段
	if err := h.validateTask(&task); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code":  "invalid_request",
			"error": err.Error(),
		})
	}

	// 确保 ID 一致
	task.ID = id

	// 更新任务
	if err := h.schedulerService.Update(ctx, &task); err != nil {
		h.logger.Error("更新定时任务失败", zap.String("id", id), zap.Error(err))
		return writeServiceError(c, err, http.StatusInternalServerError, "update_task_failed", "更新任务失败")
	}

	h.logger.Info("定时任务更新成功", zap.String("id", id), zap.String("name", task.Name))

	return c.JSON(http.StatusOK, task)
}

// Delete 删除定时任务
func (h *ScheduledTaskHandler) Delete(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")

	if err := h.schedulerService.Delete(ctx, id); err != nil {
		h.logger.Error("删除定时任务失败", zap.String("id", id), zap.Error(err))
		return writeServiceError(c, err, http.StatusInternalServerError, "delete_task_failed", "删除任务失败")
	}

	h.logger.Info("定时任务删除成功", zap.String("id", id))

	return c.JSON(http.StatusOK, map[string]string{
		"message": "任务已删除",
	})
}

// Trigger 立即触发执行定时任务
func (h *ScheduledTaskHandler) Trigger(c *echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var req TriggerScheduledTaskRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"code": "invalid_request", "error": "请求参数错误",
		})
	}
	bodyRequestID, bodyErr := normalizeSMSRequestID(req.RequestID)
	headerRequestID, headerErr := normalizeSMSRequestID(c.Request().Header.Get("Idempotency-Key"))
	if bodyErr != nil || headerErr != nil {
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

	messageID, err := h.schedulerService.TriggerTaskWithRequestID(ctx, id, requestID)
	if err != nil {
		h.logger.Error("触发定时任务失败", zap.String("id", id), zap.Error(err))
		if errors.Is(err, service.ErrSMSSubmissionAmbiguous) {
			return c.JSON(http.StatusAccepted, map[string]string{
				"message":   "短信结果尚未确定，可能已经发送，请勿使用新请求号重试",
				"messageId": messageID,
				"status":    string(models.LastRunStatusAmbiguous),
			})
		}
		return writeServiceError(c, err, http.StatusServiceUnavailable, "trigger_task_failed", "触发任务失败")
	}

	h.logger.Info("定时任务已触发执行", zap.String("id", id))

	return c.JSON(http.StatusOK, map[string]string{
		"message":   "任务已触发执行",
		"messageId": messageID,
	})
}

// validateTask 验证任务字段
func (h *ScheduledTaskHandler) validateTask(task *models.ScheduledTask) error {
	task.SIMID = strings.TrimSpace(task.SIMID)
	task.Name = strings.TrimSpace(task.Name)
	task.PhoneNumber = strings.TrimSpace(task.PhoneNumber)
	if task.SIMID == "" || task.SIMID == service.UnassignedSIMID {
		return fmt.Errorf("必须选择目标 SIM")
	}
	if task.Name == "" {
		return fmt.Errorf("任务名称不能为空")
	}
	if task.IntervalDays <= 0 {
		return fmt.Errorf("执行间隔天数必须大于0")
	}
	if task.PhoneNumber == "" {
		return fmt.Errorf("目标手机号不能为空")
	}
	if strings.Contains(task.PhoneNumber, ":CMD_END") {
		return fmt.Errorf("目标手机号包含串口协议保留标记")
	}
	if task.Content == "" {
		return fmt.Errorf("短信内容不能为空")
	}
	if strings.Contains(task.Content, ":CMD_END") {
		return fmt.Errorf("短信内容包含串口协议保留标记")
	}
	return nil
}
