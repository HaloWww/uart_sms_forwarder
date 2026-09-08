package internal

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/dushixiang/uart_sms_forwarder/internal/handler"
	"github.com/dushixiang/uart_sms_forwarder/internal/middleware"
	"github.com/dushixiang/uart_sms_forwarder/internal/models"
	"github.com/dushixiang/uart_sms_forwarder/internal/repo"
	"github.com/dushixiang/uart_sms_forwarder/internal/service"
	"github.com/dushixiang/uart_sms_forwarder/internal/version"
	"github.com/dushixiang/uart_sms_forwarder/web"
	"github.com/go-orz/orz"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	echomiddleware "github.com/labstack/echo/v5/middleware"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Handlers 所有Handler的集合
type Handlers struct {
	Auth          *handler.AuthHandler
	Property      *handler.PropertyHandler
	TextMessage   *handler.TextMessageHandler
	Serial        *handler.SerialHandler
	ScheduledTask *handler.ScheduledTaskHandler
}

func Run(configPath string) {
	err := orz.Quick(configPath, setup)
	if err != nil {
		log.Fatal(err)
	}
}

func setup(app *orz.App) error {
	logger := app.Logger()
	db := app.GetDatabase()

	// 1. 数据库迁移
	if err := autoMigrate(db); err != nil {
		logger.Error("数据库迁移失败", zap.Error(err))
		return err
	}
	if err := disableUnboundScheduledTasks(db); err != nil {
		logger.Error("停用未绑定 SIM 的旧计划任务失败", zap.Error(err))
		return err
	}
	if err := markInterruptedSendingMessages(db); err != nil {
		logger.Error("恢复进程重启前未完成的短信状态失败", zap.Error(err))
		return err
	}
	if err := reconcileScheduledTaskResults(db); err != nil {
		logger.Error("对账计划任务与短信结果失败", zap.Error(err))
		return err
	}

	// 2. 读取应用配置
	var appConfig config.AppConfig
	_config := app.GetConfig()
	if _config != nil {
		if err := _config.App.Unmarshal(&appConfig); err != nil {
			logger.Error("读取配置失败", zap.Error(err))
			return err
		}
	}

	// 3. 设置默认值
	setDefaultConfig(&appConfig, logger)

	// 4. 初始化 Repository
	textMessageRepo := repo.NewTextMessageRepo(db)

	// 5. 初始化 Service
	propertyService := service.NewPropertyService(logger, db)
	notifier := service.NewNotifier(logger)
	textMessageService := service.NewTextMessageService(logger, textMessageRepo)

	// 初始化默认配置
	ctx := context.Background()
	if err := propertyService.InitializeDefaultConfigs(ctx); err != nil {
		logger.Error("初始化默认配置失败", zap.Error(err))
	}

	// 6. 初始化串口服务
	serialManager, err := service.NewSerialManager(
		logger,
		db,
		appConfig.Serial,
		textMessageService,
		notifier,
		propertyService,
	)
	if err != nil {
		return err
	}
	// 旧数据缺少可验证的 ICCID，不能依据当前串口或当前插卡自动归属。
	// 历史记录保留为未分配；旧计划任务必须由用户明确选择 SIM 后才能执行。

	// 7. 初始化定时任务服务
	schedulerService := service.NewSchedulerService(
		logger,
		db,
		serialManager,
	)
	serialManager.SetScheduledTaskStatusUpdater(schedulerService.UpdateLastRunStatusByMsgId)

	// 8. 初始化 OIDC 和 Account Service
	oidcService := service.NewOIDCService(logger, &appConfig)
	accountService := service.NewAccountService(logger, oidcService, &appConfig)

	// 9. 初始化 Handler
	authHandler := handler.NewAuthHandler(logger, accountService)
	propertyHandler := handler.NewPropertyHandler(logger, propertyService, notifier)
	textMessageHandler := handler.NewTextMessageHandler(logger, textMessageService, textMessageRepo)
	serialHandler := handler.NewSerialHandler(logger, serialManager)
	scheduledTaskHandler := handler.NewScheduledTaskHandler(logger, schedulerService)

	handlers := &Handlers{
		Auth:          authHandler,
		Property:      propertyHandler,
		TextMessage:   textMessageHandler,
		Serial:        serialHandler,
		ScheduledTask: scheduledTaskHandler,
	}

	// 10. 设置 API 路由
	setupApi(app, handlers, &appConfig, logger)

	// 11. 启动后台服务
	background := context.Background()
	// 启动串口服务
	serialManager.Start(background)

	// 启动定时任务服务
	if err := schedulerService.Start(background); err != nil {
		logger.Error("启动定时任务服务失败", zap.Error(err))
	} else {
		logger.Info("定时任务服务启动成功")
	}

	logger.Info("应用启动完成")
	return nil
}

// setDefaultConfig 设置默认配置
func setDefaultConfig(appConfig *config.AppConfig, logger *zap.Logger) {
	// JWT 默认值
	if appConfig.JWT.Secret == "" {
		appConfig.JWT.Secret = uuid.NewString()
		logger.Warn("未配置JWT密钥，使用随机UUID")
	}
	if appConfig.JWT.ExpiresHours == 0 {
		appConfig.JWT.ExpiresHours = 168 // 7天
	}
}

// autoMigrate 数据库迁移
func autoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.Property{},
		&models.TextMessage{},
		&models.ScheduledTask{},
		&models.SIMProfile{},
	)
}

// disableUnboundScheduledTasks 是幂等的安全迁移：旧任务没有可验证的 ICCID 时先停用，
// 防止升级后根据当前插卡猜测目标。用户明确选择 SIM 后可重新启用。
func disableUnboundScheduledTasks(db *gorm.DB) error {
	return db.Model(&models.ScheduledTask{}).
		Where("(sim_id = '' OR sim_id IS NULL) AND enabled = ?", true).
		Update("enabled", false).Error
}

// markInterruptedSendingMessages 在启动时收口上次进程留下的 sending 记录。
// 内存计时器和设备回执关联已丢失，不能宣称失败，否则人工重试可能重复发送。
func markInterruptedSendingMessages(db *gorm.DB) error {
	return db.Model(&models.TextMessage{}).
		Where("type = ? AND status = ?", models.MessageTypeOutgoing, models.MessageStatusSending).
		Updates(map[string]any{
			"status": models.MessageStatusAmbiguous, "ambiguous": true,
			"send_error": "process_restarted_before_result",
		}).Error
}

// reconcileScheduledTaskResults 修复进程在“短信结果落库”和“任务状态更新”之间
// 退出留下的不一致。Claim 后尚未创建短信就退出时，串口命令一定还未下发，
// 因此可明确记为失败；已有终态短信则以短信事实为准。
func reconcileScheduledTaskResults(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var tasks []models.ScheduledTask
		if err := tx.Where(
			"last_run_status = ? AND COALESCE(last_msg_id, '') <> ''",
			models.LastRunStatusAmbiguous,
		).Find(&tasks).Error; err != nil {
			return err
		}
		for _, task := range tasks {
			var message models.TextMessage
			err := tx.Where("id = ?", task.LastMsgId).First(&message).Error
			status := models.LastRunStatusAmbiguous
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				status = models.LastRunStatusFailed
			case err != nil:
				return err
			case message.Status == models.MessageStatusSent:
				status = models.LastRunStatusSuccess
			case message.Status == models.MessageStatusFailed:
				status = models.LastRunStatusFailed
			}
			if status == models.LastRunStatusAmbiguous {
				continue
			}
			if err := tx.Model(&models.ScheduledTask{}).
				Where("id = ? AND last_msg_id = ? AND last_run_status = ?",
					task.ID, task.LastMsgId, models.LastRunStatusAmbiguous).
				Update("last_run_status", status).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// setupApi 设置API路由
func setupApi(app *orz.App, handlers *Handlers, appConfig *config.AppConfig, logger *zap.Logger) {
	e := app.GetEcho()

	e.Use(echomiddleware.StaticWithConfig(echomiddleware.StaticConfig{
		Skipper: func(c *echo.Context) bool {
			// 不处理接口
			if strings.HasPrefix(c.Request().RequestURI, "/api") {
				return true
			}
			if strings.HasPrefix(c.Request().RequestURI, "/health") {
				return true
			}
			return false
		},
		Index:      "index.html",
		HTML5:      true,
		Browse:     false,
		IgnoreBase: false,
		Filesystem: web.Assets(),
	}))

	// 登录路由（不需要认证）
	e.POST("/api/login", handlers.Auth.Login)
	e.GET("/api/auth/config", handlers.Auth.GetAuthConfig)
	e.GET("/api/auth/oidc/url", handlers.Auth.GetOIDCAuthURL)
	e.POST("/api/auth/oidc/callback", handlers.Auth.OIDCCallback)

	// API 路由组（需要认证）
	api := e.Group("/api")
	api.Use(middleware.JWTMiddleware(appConfig.JWT.Secret, logger))

	// Version
	api.GET("/version", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]any{
			"version": version.GetVersion(),
		})
	})

	// Property API
	api.GET("/properties/:id", handlers.Property.GetProperty)
	api.PUT("/properties/:id", handlers.Property.SetProperty)
	api.POST("/notifications/:type/test", handlers.Property.TestNotificationChannel)

	// TextMessage API
	api.GET("/messages/stats", handlers.TextMessage.GetStats)
	api.GET("/messages/conversations", handlers.TextMessage.GetConversations)
	api.GET("/messages/conversations/:peer/messages", handlers.TextMessage.GetConversationMessages)
	api.DELETE("/messages/conversations/:peer", handlers.TextMessage.DeleteConversation)
	api.DELETE("/messages/:id", handlers.TextMessage.Delete)
	api.DELETE("/messages", handlers.TextMessage.Clear)

	// Serial API
	api.POST("/serial/sms", handlers.Serial.SendSMS)
	api.GET("/serial/status", handlers.Serial.GetStatus) // 包含移动网络信息
	api.GET("/serial/devices", handlers.Serial.GetDevices)
	api.GET("/serial/sims", handlers.Serial.GetSIMs)
	api.POST("/serial/flymode", handlers.Serial.SetFlymode)
	api.POST("/serial/reboot", handlers.Serial.RebootMcu)

	// ScheduledTask API (RESTful)
	api.GET("/scheduled-tasks", handlers.ScheduledTask.List)
	api.GET("/scheduled-tasks/:id", handlers.ScheduledTask.Get)
	api.POST("/scheduled-tasks", handlers.ScheduledTask.Create)
	api.PUT("/scheduled-tasks/:id", handlers.ScheduledTask.Update)
	api.DELETE("/scheduled-tasks/:id", handlers.ScheduledTask.Delete)
	api.POST("/scheduled-tasks/:id/trigger", handlers.ScheduledTask.Trigger)

	// 健康检查接口（无需认证）
	e.GET("/health", func(c *echo.Context) error {
		return c.JSON(200, map[string]string{
			"status": "ok",
		})
	})
}
