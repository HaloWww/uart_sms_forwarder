package service

import (
	"context"
	"time"

	"github.com/dushixiang/uart_sms_forwarder/config"
	"github.com/go-errors/errors"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func NewAccountService(logger *zap.Logger, oidcService *OIDCService, appConfig *config.AppConfig) *AccountService {
	jwtSecret := appConfig.JWT.Secret
	tokenExpireHours := appConfig.JWT.ExpiresHours

	if jwtSecret == "" {
		logger.Fatal("JWT secret cannot be empty")
	}
	if len(jwtSecret) < 32 {
		logger.Warn("JWT secret is too short, should be at least 32 characters for security")
	}
	if tokenExpireHours <= 0 {
		tokenExpireHours = 168 // 默认7天
	}

	service := &AccountService{
		logger:           logger,
		oidcService:      oidcService,
		jwtSecret:        jwtSecret,
		tokenExpireHours: tokenExpireHours,
		users:            appConfig.Users,
	}
	return service
}

type AccountService struct {
	logger           *zap.Logger
	oidcService      *OIDCService
	jwtSecret        string
	tokenExpireHours int

	users map[string]string
}

// JWTClaims JWT 声明
type JWTClaims struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// UserInfo 用户信息（简化版）
type UserInfo struct {
	Username string `json:"username"`
}

// LoginResponse 登录响应
type LoginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt int64     `json:"expiresAt"`
	User      *UserInfo `json:"user"`
}

// Login 用户登录（Basic Auth）
func (s *AccountService) Login(ctx context.Context, username, password string) (*LoginResponse, error) {
	// 使用 Basic Auth 验证
	if err := s.ValidateCredentials(ctx, username, password); err != nil {
		return nil, err
	}

	// 生成 JWT token
	token, expiresAt, err := s.generateToken(username, username)
	if err != nil {
		return nil, err
	}

	s.logger.Info("用户登录成功", zap.String("username", username))

	return &LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User: &UserInfo{
			Username: username,
		},
	}, nil
}

// LoginWithOIDC OIDC 登录
func (s *AccountService) LoginWithOIDC(ctx context.Context, code, state string) (*LoginResponse, error) {
	// 使用 OIDC 验证
	username, nickname, err := s.oidcService.ExchangeCode(ctx, code, state)
	if err != nil {
		return nil, err
	}

	// 生成 JWT token
	token, expiresAt, err := s.generateToken(username, nickname)
	if err != nil {
		return nil, err
	}

	s.logger.Info("OIDC 登录成功", zap.String("username", username))

	return &LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User: &UserInfo{
			Username: username,
		},
	}, nil
}

// generateToken 生成 JWT token
func (s *AccountService) generateToken(username, nickname string) (string, int64, error) {
	expiresAt := time.Now().Add(time.Duration(s.tokenExpireHours) * time.Hour)
	claims := &JWTClaims{
		UserID:   username, // 使用 username 作为 userID
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
			Issuer:    "pika",
			Subject:   username,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(s.jwtSecret))
	if err != nil {
		s.logger.Error("生成token失败", zap.Error(err))
		return "", 0, errors.New("生成token失败")
	}

	return tokenString, expiresAt.UnixMilli(), nil
}

// Logout 用户登出
func (s *AccountService) Logout(ctx context.Context, userID string) error {

	s.logger.Info("用户登出成功", zap.String("userID", userID))
	return nil
}

// ValidateCredentials 验证用户名和密码
func (s *AccountService) ValidateCredentials(ctx context.Context, username, password string) error {
	// 从配置中获取用户的bcrypt密码哈希
	hashedPassword, exists := s.users[username]
	if !exists {
		s.logger.Debug("用户不存在", zap.String("username", username))
		return errors.New("用户名或密码错误")
	}

	// 验证密码
	if err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password)); err != nil {
		s.logger.Debug("密码验证失败", zap.String("username", username), zap.Error(err))
		return errors.New("用户名或密码错误")
	}

	s.logger.Info("User 认证成功", zap.String("username", username))
	return nil
}

// ValidateToken 验证 JWT token
func (s *AccountService) ValidateToken(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		// 验证签名方法
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("无效的签名方法")
		}
		return []byte(s.jwtSecret), nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*JWTClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("无效的token")
}

// AuthConfig 认证配置
type AuthConfig struct {
	OIDCEnabled     bool `json:"oidcEnabled"`
	GitHubEnabled   bool `json:"githubEnabled"`
	PasswordEnabled bool `json:"passwordEnabled"`
}

// GetAuthConfig 获取认证配置
func (s *AccountService) GetAuthConfig() *AuthConfig {
	return &AuthConfig{
		OIDCEnabled:     s.oidcService.IsEnabled(),
		PasswordEnabled: len(s.users) > 0,
	}
}

// OIDCAuthURL OIDC 认证 URL 响应
type OIDCAuthURL struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}

// GetOIDCAuthURL 获取 OIDC 认证 URL
func (s *AccountService) GetOIDCAuthURL() (*OIDCAuthURL, error) {
	if !s.oidcService.IsEnabled() {
		return nil, errors.New("OIDC 未启用")
	}

	authURL, state, err := s.oidcService.GenerateAuthURL()
	if err != nil {
		return nil, err
	}

	return &OIDCAuthURL{
		AuthURL: authURL,
		State:   state,
	}, nil
}

// GitHubAuthURL GitHub 认证 URL 响应
type GitHubAuthURL struct {
	AuthURL string `json:"authUrl"`
	State   string `json:"state"`
}
