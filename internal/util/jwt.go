package util

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CustomClaims JWT 自定义声明
type CustomClaims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// GenerateToken 生成 JWT token
func GenerateToken(username string, secret string, expiresHours int) (string, int64, error) {
	// 计算过期时间
	expiresAt := time.Now().Add(time.Duration(expiresHours) * time.Hour)

	// 创建 claims
	claims := CustomClaims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	// 创建 token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// 签名并获取完整的 token 字符串
	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", 0, err
	}

	return tokenString, expiresAt.UnixMilli(), nil
}

// VerifyToken 验证并解析 token
func VerifyToken(tokenString string, secret string) (*CustomClaims, error) {
	// 解析 token
	token, err := jwt.ParseWithClaims(tokenString, &CustomClaims{}, func(token *jwt.Token) (interface{}, error) {
		// 验证签名算法
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("无效的签名方法")
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	// 验证 token 有效性并提取 claims
	if claims, ok := token.Claims.(*CustomClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, errors.New("无效的 token")
}
