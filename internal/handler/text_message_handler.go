package handler

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"

	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

func messageScope(c *echo.Context) service.MessageScope {
	return service.MessageScope{
		SIMID: strings.TrimSpace(c.QueryParam("simId")),
	}
}

func requireMessageScope(c *echo.Context) (service.MessageScope, bool) {
	scope := messageScope(c)
	if scope.SIMID == "" {
		return scope, false
	}
	return scope, true
}

func writeMessageScopeRequired(c *echo.Context) error {
	return c.JSON(http.StatusBadRequest, map[string]string{
		"code":  "sim_id_required",
		"error": "必须指定 simId",
	})
}

// TextMessageHandler 短信API处理器
type TextMessageHandler struct {
	logger  *zap.Logger
	service *service.TextMessageService
	repo    *repo.TextMessageRepo
}

// NewTextMessageHandler 创建短信Handler实例
func NewTextMessageHandler(logger *zap.Logger, service *service.TextMessageService, repo *repo.TextMessageRepo) *TextMessageHandler {
	return &TextMessageHandler{
		logger:  logger,
		service: service,
		repo:    repo,
	}
}

// Delete 删除单条短信
// DELETE /api/messages/:id
func (h *TextMessageHandler) Delete(c *echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id 参数不能为空"})
	}
	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	if err := h.service.Delete(c.Request().Context(), id, scope); err != nil {
		h.logger.Error("删除短信失败", zap.Error(err), zap.String("id", id))
		return writeServiceError(c, err, http.StatusInternalServerError, "delete_failed", "删除失败")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message": "删除成功",
	})
}

// Clear 清空所有短信
// DELETE /api/messages
func (h *TextMessageHandler) Clear(c *echo.Context) error {
	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	if err := h.service.Clear(c.Request().Context(), scope); err != nil {
		h.logger.Error("清空短信失败", zap.Error(err))
		return writeServiceError(c, err, http.StatusInternalServerError, "clear_failed", "清空失败")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message": "清空成功",
	})
}

// GetStats 获取统计信息
// GET /api/messages/stats
func (h *TextMessageHandler) GetStats(c *echo.Context) error {
	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	stats, err := h.service.GetStats(c.Request().Context(), scope)
	if err != nil {
		h.logger.Error("获取统计信息失败", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "获取统计信息失败",
		})
	}

	return c.JSON(http.StatusOK, stats)
}

// GetConversations 获取会话列表
// GET /api/messages/conversations
func (h *TextMessageHandler) GetConversations(c *echo.Context) error {
	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	conversations, err := h.service.GetConversations(c.Request().Context(), scope)
	if err != nil {
		h.logger.Error("获取会话列表失败", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "获取会话列表失败",
		})
	}

	return c.JSON(http.StatusOK, conversations)
}

// GetConversationMessages 获取指定会话的所有消息
// GET /api/messages/conversations/:peer/messages
func (h *TextMessageHandler) GetConversationMessages(c *echo.Context) error {
	peer := c.Param("peer")
	if peer == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "peer 参数不能为空",
		})
	}

	// 路径解码必须保留国际号码前导 +；QueryUnescape 会错误地把 + 变为空格。
	decodedPeer, err := url.PathUnescape(peer)
	if err != nil {
		h.logger.Error("URL 解码失败", zap.Error(err), zap.String("peer", peer))
		// 如果解码失败，使用原始值
		decodedPeer = peer
	}

	h.logger.Debug("获取会话消息",
		zap.String("peer_raw", peer),
		zap.String("peer_decoded", decodedPeer))

	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	messages, err := h.service.GetConversationMessages(c.Request().Context(), scope, decodedPeer)
	if err != nil {
		h.logger.Error("获取会话消息失败", zap.Error(err), zap.String("peer", decodedPeer))
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "获取会话消息失败",
		})
	}

	return c.JSON(http.StatusOK, messages)
}

// DeleteConversation 删除整个会话（与某个联系人的所有消息）
// DELETE /api/messages/conversations/:peer
func (h *TextMessageHandler) DeleteConversation(c *echo.Context) error {
	peer := c.Param("peer")
	if peer == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "peer 参数不能为空",
		})
	}

	// 路径解码必须保留国际号码前导 +；QueryUnescape 会错误地把 + 变为空格。
	decodedPeer, err := url.PathUnescape(peer)
	if err != nil {
		h.logger.Error("URL 解码失败", zap.Error(err), zap.String("peer", peer))
		// 如果解码失败，使用原始值
		decodedPeer = peer
	}

	h.logger.Debug("删除会话",
		zap.String("peer_raw", peer),
		zap.String("peer_decoded", decodedPeer))

	scope, ok := requireMessageScope(c)
	if !ok {
		return writeMessageScopeRequired(c)
	}
	if err := h.service.DeleteConversation(c.Request().Context(), scope, decodedPeer); err != nil {
		h.logger.Error("删除会话失败", zap.Error(err), zap.String("peer", decodedPeer))
		return writeServiceError(c, err, http.StatusInternalServerError, "delete_failed", "删除会话失败")
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message": "删除成功",
	})
}
