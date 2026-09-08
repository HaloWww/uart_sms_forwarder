package handler

import (
	"errors"
	"net/http"

	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// writeServiceError 将可预期的业务状态转换为稳定的 HTTP 状态和机器可读错误码。
func writeServiceError(
	c *echo.Context,
	err error,
	fallbackStatus int,
	fallbackCode string,
	fallbackMessage string,
) error {
	status := fallbackStatus
	code := fallbackCode
	message := fallbackMessage

	switch {
	case errors.Is(err, service.ErrSMSRequestIDInvalid):
		status = http.StatusBadRequest
		code = "request_id_invalid"
		message = err.Error()
	case errors.Is(err, service.ErrSMSRequestConflict):
		status = http.StatusConflict
		code = "request_id_conflict"
		message = "同一 requestId 已用于另一条短信"
	case errors.Is(err, service.ErrSMSRequestPreviouslyFailed):
		status = http.StatusConflict
		code = "request_already_failed"
		message = "该 requestId 对应的短信请求已失败"
	case errors.Is(err, service.ErrSMSDestinationInvalid):
		status = 400
		code = "sms_destination_invalid"
		message = err.Error()
	case errors.Is(err, service.ErrSMSOperationPending):
		status = http.StatusConflict
		code = "sms_operation_pending"
		message = "仍有短信等待设备最终结果，请等待状态收口后重试"
	case errors.Is(err, service.ErrSMSContentTooLong):
		status = 400
		code = "sms_content_too_long"
		message = err.Error()
	case errors.Is(err, service.ErrSIMIdentityRequired), errors.Is(err, service.ErrScheduledTaskSIMRequired):
		status = 400
		code = "sim_id_required"
		message = "必须指定 SIM"
	case errors.Is(err, service.ErrSIMOffline):
		status = 409
		code = "sim_offline"
		message = "目标 SIM 当前离线或身份尚未确认"
	case errors.Is(err, service.ErrSIMMismatch):
		status = 409
		code = "sim_identity_mismatch"
		message = "当前模块内的 SIM 与目标不一致，操作已阻止"
	case errors.Is(err, service.ErrSIMUnknown):
		status = 404
		code = "sim_unknown"
		message = "目标 SIM 尚未登记"
	case errors.Is(err, service.ErrSIMConflict):
		status = 409
		code = "sim_conflict"
		message = "检测到重复 SIM 身份，已阻止操作"
	case errors.Is(err, service.ErrSIMTopologyUnsettled):
		status = 409
		code = "sim_topology_unsettled"
		message = "仍有已连接设备正在确认 SIM 身份，请稍后重试"
	case errors.Is(err, service.ErrSIMProtocolOutdated):
		status = 409
		code = "lua_script_outdated"
		message = "Air780 脚本版本过旧，请升级 main.lua 后重试"
	case errors.Is(err, service.ErrScheduledTaskBusy):
		status = 409
		code = "scheduled_task_busy"
		message = "计划任务正在执行或修改，请稍后重试"
	case errors.Is(err, gorm.ErrRecordNotFound):
		status = 404
		code = "not_found"
		message = "记录不存在"
	}

	return c.JSON(status, map[string]string{
		"code":  code,
		"error": message,
	})
}
