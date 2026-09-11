package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasttemplate"
	"go.uber.org/zap"
	"gopkg.in/gomail.v2"
)

// Notifier 告警通知服务
type Notifier struct {
	logger          *zap.Logger
	httpClient      *http.Client
	weComAPIBaseURL string
}

func NewNotifier(logger *zap.Logger) *Notifier {
	return &Notifier{
		logger:          logger,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		weComAPIBaseURL: "https://qyapi.weixin.qq.com",
	}
}

// NotificationMessage 通用通知消息（支持短信、来电、飞行模式状态等）
type NotificationMessage struct {
	Type       string // "sms"、"call" 或 "flymode"
	DeviceID   string
	DeviceName string
	SIMID      string
	ICCID      string
	IMSI       string
	IMEI       string
	From       string
	To         string // 入站短信接收号码
	Content    string // 短信内容（来电时为空）
	Timestamp  int64
	Incoming   bool
	Rendered   string // 已应用全局包装的最终通知正文
}

func (m NotificationMessage) identitySummary() string {
	device := m.DeviceName
	if device == "" {
		device = m.DeviceID
	}
	if device == "" {
		device = "默认设备"
	}
	lines := []string{"设备: " + device}
	if m.SIMID != "" {
		lines = append(lines, "SIM: "+m.SIMID)
	}
	if m.ICCID != "" {
		lines = append(lines, "ICCID: "+m.ICCID)
	}
	if m.IMSI != "" {
		lines = append(lines, "IMSI: "+m.IMSI)
	}
	if m.IMEI != "" {
		lines = append(lines, "IMEI: "+m.IMEI)
	}
	return strings.Join(lines, "\n")
}

func (m NotificationMessage) String() string {
	if m.Rendered != "" {
		return m.Rendered
	}
	timestamp := time.Unix(m.Timestamp, 0)
	identity := m.identitySummary()
	switch m.Type {
	case "call":
		return fmt.Sprintf(`来电通知
----
%s
来电号码: %s
时间: %s
`,
			identity,
			m.From,
			timestamp.Format(time.DateTime),
		)
	case "flymode":
		return fmt.Sprintf(`飞行模式通知
----
%s
切换方式: %s
%s
时间: %s
`,
			identity,
			m.From,
			m.Content,
			timestamp.Format(time.DateTime),
		)
	default: // "sms"
		return fmt.Sprintf(`%s
----
%s
来自: %s
时间: %s
`,
			m.Content,
			identity,
			m.From,
			timestamp.Format(time.DateTime),
		)
	}
}

type BarkResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (n *Notifier) sendBarkByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	serverURL, _ := config["serverUrl"].(string)
	deviceKey, _ := config["deviceKey"].(string)
	title, _ := config["title"].(string)
	group, _ := config["group"].(string)
	sound, _ := config["sound"].(string)
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	deviceKey = strings.TrimSpace(deviceKey)
	if serverURL == "" {
		serverURL = "https://api.day.app"
	}
	parsedURL, err := url.Parse(serverURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return fmt.Errorf("Bark serverUrl 必须是有效的 HTTP(S) 地址")
	}
	if deviceKey == "" {
		return fmt.Errorf("Bark 配置缺少 deviceKey")
	}
	if title == "" {
		title = "UART 短信转发器"
	}
	body := map[string]interface{}{
		"device_key": deviceKey,
		"title":      title,
		"body":       message,
	}
	if group != "" {
		body["group"] = group
	}
	if sound != "" {
		body["sound"] = sound
	}
	result, err := n.sendJSONRequest(ctx, serverURL+"/push", body)
	if err != nil {
		return err
	}
	var response BarkResult
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("解析 Bark 响应失败: %w", err)
	}
	if response.Code != 200 {
		return fmt.Errorf("Bark 推送失败: code=%d message=%s", response.Code, response.Message)
	}
	return nil
}

type weComTokenResult struct {
	Errcode     int    `json:"errcode"`
	Errmsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
}

func (n *Notifier) sendWeComAppByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	corpID, _ := config["corpId"].(string)
	secret, _ := config["secret"].(string)
	agentIDValue, _ := config["agentId"].(string)
	toUser, _ := config["toUser"].(string)
	corpID = strings.TrimSpace(corpID)
	secret = strings.TrimSpace(secret)
	if corpID == "" || secret == "" || strings.TrimSpace(agentIDValue) == "" {
		return fmt.Errorf("企业微信应用配置缺少 corpId、agentId 或 secret")
	}
	agentID, err := strconv.Atoi(strings.TrimSpace(agentIDValue))
	if err != nil || agentID <= 0 {
		return fmt.Errorf("企业微信应用 agentId 必须是正整数")
	}
	if strings.TrimSpace(toUser) == "" {
		toUser = "@all"
	}
	client, err := n.httpClientByProxyConfig(config)
	if err != nil {
		return fmt.Errorf("企业微信应用代理配置错误: %w", err)
	}

	tokenURL := n.weComAPIBaseURL + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(corpID) +
		"&corpsecret=" + url.QueryEscape(secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return fmt.Errorf("创建企业微信令牌请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("获取企业微信访问令牌失败: %w", err)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("读取企业微信令牌响应失败: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("获取企业微信访问令牌失败，状态码: %d", resp.StatusCode)
	}
	var tokenResult weComTokenResult
	if err := json.Unmarshal(data, &tokenResult); err != nil {
		return fmt.Errorf("解析企业微信令牌响应失败: %w", err)
	}
	if tokenResult.Errcode != 0 || tokenResult.AccessToken == "" {
		return fmt.Errorf("获取企业微信访问令牌失败: %s", tokenResult.Errmsg)
	}

	body := map[string]interface{}{
		"touser":  toUser,
		"msgtype": "text",
		"agentid": agentID,
		"text": map[string]string{
			"content": message,
		},
		"safe": 0,
	}
	result, err := n.sendJSONRequestWithClient(ctx, client, n.weComAPIBaseURL+
		"/cgi-bin/message/send?access_token="+url.QueryEscape(tokenResult.AccessToken), body)
	if err != nil {
		return err
	}
	var sendResult WeComResult
	if err := json.Unmarshal(result, &sendResult); err != nil {
		return fmt.Errorf("解析企业微信应用响应失败: %w", err)
	}
	if sendResult.Errcode != 0 {
		return fmt.Errorf("企业微信应用推送失败: %s", sendResult.Errmsg)
	}
	return nil
}

func (n *Notifier) httpClientByProxyConfig(config map[string]interface{}) (*http.Client, error) {
	proxyEnabled, _ := config["proxyEnabled"].(bool)
	if !proxyEnabled {
		if n.httpClient != nil {
			return n.httpClient, nil
		}
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	proxyURL, _ := config["proxyUrl"].(string)
	proxyUsername, _ := config["proxyUsername"].(string)
	proxyPassword, _ := config["proxyPassword"].(string)
	parsedProxyURL, err := buildProxyURL(proxyURL, proxyUsername, proxyPassword)
	if err != nil {
		return nil, err
	}
	if parsedProxyURL.Scheme != "http" && parsedProxyURL.Scheme != "https" && parsedProxyURL.Scheme != "socks5" {
		return nil, fmt.Errorf("代理地址仅支持 http、https 或 socks5")
	}
	if parsedProxyURL.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsedProxyURL),
		},
	}, nil
}

func (n *Notifier) SendBarkByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendBarkByConfig(ctx, config, message)
}

func (n *Notifier) SendWeComAppByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendWeComAppByConfig(ctx, config, message)
}

// sendDingTalk 发送钉钉通知
func (n *Notifier) sendDingTalk(ctx context.Context, webhook, secret, message string) error {
	// 构造钉钉消息体
	body := map[string]interface{}{
		"msgtype": "text",
		"text": map[string]string{
			"content": message,
		},
	}

	// 如果有加签密钥，计算签名
	timestamp := time.Now().UnixMilli()
	if secret != "" {
		sign := n.calculateDingTalkSign(timestamp, secret)
		webhook = fmt.Sprintf("%s&timestamp=%d&sign=%s", webhook, timestamp, sign)
	}
	_, err := n.sendJSONRequest(ctx, webhook, body)
	if err != nil {
		return err
	}
	return nil
}

// calculateDingTalkSign 计算钉钉加签
func (n *Notifier) calculateDingTalkSign(timestamp int64, secret string) string {
	stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

type WeComResult struct {
	Errcode   int    `json:"errcode"`
	Errmsg    string `json:"errmsg"`
	Type      string `json:"type"`
	MediaId   string `json:"media_id"`
	CreatedAt string `json:"created_at"`
}

// sendWeCom 发送企业微信通知
func (n *Notifier) sendWeCom(ctx context.Context, webhook, message string) error {
	body := map[string]interface{}{
		"msgtype": "text",
		"text": map[string]string{
			"content": message,
		},
	}
	result, err := n.sendJSONRequest(ctx, webhook, body)
	if err != nil {
		return err
	}
	var weComResult WeComResult
	if err := json.Unmarshal(result, &weComResult); err != nil {
		return err
	}
	if weComResult.Errcode != 0 {
		return fmt.Errorf("%s", weComResult.Errmsg)
	}
	return nil
}

// sendFeishu 发送飞书通知
func (n *Notifier) sendFeishu(ctx context.Context, webhook, signSecret, message string) error {
	body := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]string{
			"text": message,
		},
	}

	// 如果有加签密钥，计算签名
	if signSecret != "" {
		timestamp := time.Now().Unix()
		stringToSign := fmt.Sprintf("%v", timestamp) + "\n" + signSecret
		var data []byte
		h := hmac.New(sha256.New, []byte(stringToSign))
		_, err := h.Write(data)
		if err != nil {
			return err
		}
		signature := base64.StdEncoding.EncodeToString(h.Sum(nil))

		// 将签名和时间戳加入请求头
		body["timestamp"] = fmt.Sprintf("%v", timestamp)
		body["sign"] = signature
	}

	_, err := n.sendJSONRequest(ctx, webhook, body)
	if err != nil {
		return err
	}
	return nil
}

// 导出方法
func (n *Notifier) SendTelegramByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendTelegramByConfig(ctx, config, message)
}

func (n *Notifier) sendTelegramByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	apitoken, _ := config["apiToken"].(string)
	userid, _ := config["userid"].(string)
	if apitoken == "" || userid == "" {
		return fmt.Errorf("Telegram 配置缺少 apiToken 或 userid")
	}
	proxyEnabled, _ := config["proxyEnabled"].(bool)
	proxyUrl, _ := config["proxyUrl"].(string)
	proxyUsername, _ := config["proxyUsername"].(string)
	proxyPassword, _ := config["proxyPassword"].(string)

	// 构建发送消息的URL
	baseURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", apitoken)
	body := map[string]interface{}{
		"chat_id": userid,
		"text":    message,
		//"parse_mode": "markdown",
	}

	if proxyEnabled {
		proxyFullUrl, err := buildProxyURL(proxyUrl, proxyUsername, proxyPassword)
		if err != nil {
			n.logger.Error("代理配置错误", zap.Error(err))
			return err
		}
		_, err = n.sendJSONRequestWithProxy(ctx, baseURL, proxyFullUrl, body)
		if err != nil {
			return err
		}
	} else {
		_, err := n.sendJSONRequest(ctx, baseURL, body)
		if err != nil {
			return err
		}
	}
	return nil
}

// sendCustomWebhook 发送自定义Webhook
func (n *Notifier) sendCustomWebhook(ctx context.Context, config map[string]interface{}, msg NotificationMessage) error {
	// 解析配置
	webhookURL, ok := config["url"].(string)
	if !ok || webhookURL == "" {
		return fmt.Errorf("自定义Webhook配置缺少 url")
	}

	// 获取请求方法，默认 POST
	method := "POST"
	if m, ok := config["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}

	// 获取自定义请求头
	headers := make(map[string]string)
	if h, ok := config["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			if strVal, ok := v.(string); ok {
				headers[k] = strVal
			}
		}
	}

	customBody, ok := config["body"].(string)
	if !ok || customBody == "" {
		return fmt.Errorf("自定义Webhook配置缺少 body")
	}

	// 使用 fasttemplate 进行变量替换
	t := fasttemplate.New(customBody, "{{", "}}")
	escape := func(s string) string {
		b, _ := json.Marshal(s)
		// json.Marshal 会返回带双引号的字符串，例如 "hello\nworld"
		// 模板中不需要外层双引号，所以去掉
		return string(b[1 : len(b)-1])
	}

	bodyStr := t.ExecuteFuncString(func(w io.Writer, tag string) (int, error) {
		var v string

		switch tag {
		case "from":
			v = msg.From
		case "content":
			v = msg.Content
			if msg.Rendered != "" {
				v = msg.Rendered
			}
		case "receiver", "to":
			v = msg.To
		case "type":
			v = msg.Type
		case "timestamp":
			timestamp := time.Unix(msg.Timestamp, 0).Format(time.DateTime)
			v = timestamp
		case "device_id":
			v = msg.DeviceID
		case "device_name":
			v = msg.DeviceName
		case "sim_id":
			v = msg.SIMID
		case "iccid":
			v = msg.ICCID
		case "imsi":
			v = msg.IMSI
		case "imei":
			v = msg.IMEI
		default:
			return w.Write([]byte("{{" + tag + "}}"))
		}

		// 写入 JSON 安全转义后的值
		return w.Write([]byte(escape(v)))
	})
	n.logger.Sugar().Debugf("自定义Webhook请求体: %s", bodyStr)
	var reqBody = strings.NewReader(bodyStr)
	contentType, _ := config["contentType"].(string)
	if contentType == "" {
		contentType = "application/json"
	}

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, method, webhookURL, reqBody)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置 Content-Type
	req.Header.Set("Content-Type", contentType)

	// 设置自定义请求头
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 发送请求
	client := n.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("请求失败，状态码: %d, 响应: %s", resp.StatusCode, string(respBody))
	}

	n.logger.Info("自定义Webhook发送成功",
		zap.String("host", requestHost(webhookURL)),
		zap.String("method", method),
		zap.String("response", string(respBody)),
	)

	return nil
}

// sendJSONRequest 发送JSON请求
func (n *Notifier) sendJSONRequest(ctx context.Context, url string, body interface{}) ([]byte, error) {
	client := n.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return n.sendJSONRequestWithClient(ctx, client, url, body)
}

func (n *Notifier) sendJSONRequestWithClient(
	ctx context.Context, client *http.Client, url string, body interface{},
) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("请求失败，状态码: %d, 响应: %s", resp.StatusCode, string(respBody))
	}

	n.logger.Info("通知发送成功", zap.String("host", requestHost(url)), zap.String("response", string(respBody)))
	return respBody, nil
}

func (n *Notifier) sendJSONRequestWithProxy(ctx context.Context, url string, proxyUrl *url.URL, body interface{}) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	transport := &http.Transport{}
	transport.Proxy = http.ProxyURL(proxyUrl)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 读取响应
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("请求失败，状态码: %d, 响应: %s", resp.StatusCode, string(respBody))
	}

	n.logger.Info("通知发送成功", zap.String("host", requestHost(url)), zap.String("response", string(respBody)))
	return respBody, nil
}

func requestHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "invalid-url"
	}
	return parsed.Host
}

// sendDingTalkByConfig 根据配置发送钉钉通知
func (n *Notifier) sendDingTalkByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	secretKey, ok := config["secretKey"].(string)
	if !ok || secretKey == "" {
		return fmt.Errorf("钉钉配置缺少 secretKey")
	}

	// 构造 Webhook URL
	webhook := fmt.Sprintf("https://oapi.dingtalk.com/robot/send?access_token=%s", secretKey)

	// 检查是否有加签密钥
	signSecret, _ := config["signSecret"].(string)

	return n.sendDingTalk(ctx, webhook, signSecret, message)
}

// sendWeComByConfig 根据配置发送企业微信通知
func (n *Notifier) sendWeComByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	secretKey, ok := config["secretKey"].(string)
	if !ok || secretKey == "" {
		return fmt.Errorf("企业微信配置缺少 secretKey")
	}

	// 构造 Webhook URL
	webhook := fmt.Sprintf("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=%s", secretKey)

	return n.sendWeCom(ctx, webhook, message)
}

// sendFeishuByConfig 根据配置发送飞书通知
func (n *Notifier) sendFeishuByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	secretKey, ok := config["secretKey"].(string)
	if !ok || secretKey == "" {
		return fmt.Errorf("飞书配置缺少 secretKey")
	}

	// 构造 Webhook URL
	webhook := fmt.Sprintf("https://open.feishu.cn/open-apis/bot/v2/hook/%s", secretKey)

	// 检查是否有加签密钥
	signSecret, _ := config["signSecret"].(string)

	return n.sendFeishu(ctx, webhook, signSecret, message)
}

// SendDingTalkByConfig 导出方法供外部调用
func (n *Notifier) SendDingTalkByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendDingTalkByConfig(ctx, config, message)
}

// SendWeComByConfig 导出方法供外部调用
func (n *Notifier) SendWeComByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendWeComByConfig(ctx, config, message)
}

// SendFeishuByConfig 导出方法供外部调用
func (n *Notifier) SendFeishuByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendFeishuByConfig(ctx, config, message)
}

// SendWebhookByConfig 导出方法供外部调用
func (n *Notifier) SendWebhookByConfig(ctx context.Context, config map[string]interface{}, msg NotificationMessage) error {
	return n.sendCustomWebhook(ctx, config, msg)
}

// sendEmail 发送邮件通知
func (n *Notifier) sendEmail(ctx context.Context, config map[string]interface{}, msg NotificationMessage) error {
	// 解析配置
	smtpHost, ok := config["smtpHost"].(string)
	if !ok || smtpHost == "" {
		return fmt.Errorf("邮件配置缺少 smtpHost")
	}

	smtpPortStr, ok := config["smtpPort"].(string)
	if !ok || smtpPortStr == "" {
		smtpPortStr = "587"
	}

	// 转换端口为整数
	smtpPort, err := strconv.Atoi(smtpPortStr)
	if err != nil {
		return fmt.Errorf("无效的 SMTP 端口: %s", smtpPortStr)
	}

	username, ok := config["username"].(string)
	if !ok || username == "" {
		return fmt.Errorf("邮件配置缺少 username")
	}

	password, ok := config["password"].(string)
	if !ok || password == "" {
		return fmt.Errorf("邮件配置缺少 password")
	}

	from, ok := config["from"].(string)
	if !ok || from == "" {
		return fmt.Errorf("邮件配置缺少 from")
	}

	to, ok := config["to"].(string)
	if !ok || to == "" {
		return fmt.Errorf("邮件配置缺少 to")
	}

	subject, ok := config["subject"].(string)
	if !ok || subject == "" {
		if msg.Type == "call" {
			subject = "来电通知 - {{from}}"
		} else if msg.Type == "flymode" {
			subject = "飞行模式状态通知"
		} else {
			subject = "收到新短信 - {{from}}"
		}
	}

	// 模板变量替换函数
	replaceVars := func(template string) string {
		t := fasttemplate.New(template, "{{", "}}")
		return t.ExecuteFuncString(func(w io.Writer, tag string) (int, error) {
			var v string
			switch tag {
			case "from":
				v = msg.From
			case "content":
				v = msg.Content
			case "receiver", "to":
				v = msg.To
			case "type":
				v = msg.Type
			case "timestamp":
				v = time.Unix(msg.Timestamp, 0).Format(time.DateTime)
			case "device_id":
				v = msg.DeviceID
			case "device_name":
				v = msg.DeviceName
			case "sim_id":
				v = msg.SIMID
			case "iccid":
				v = msg.ICCID
			case "imsi":
				v = msg.IMSI
			case "imei":
				v = msg.IMEI
			default:
				return w.Write([]byte("{{" + tag + "}}"))
			}
			return w.Write([]byte(v))
		})
	}

	// 替换主题中的变量
	subject = replaceVars(subject)

	// 构造邮件内容
	body := msg.String()

	// 分隔多个收件人
	toList := strings.Split(to, ",")
	for i, addr := range toList {
		toList[i] = strings.TrimSpace(addr)
	}

	// 使用 gomail 创建邮件
	m := gomail.NewMessage()
	m.SetHeader("From", from)
	m.SetHeader("To", toList...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/plain", body)

	// 创建 SMTP 拨号器
	d := gomail.NewDialer(smtpHost, smtpPort, username, password)

	// 发送邮件
	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("发送邮件失败: %w", err)
	}

	n.logger.Info("邮件发送成功",
		zap.String("from", from),
		zap.String("to", to),
		zap.String("subject", subject),
	)

	return nil
}

// sendEmailByConfig 根据配置发送邮件通知（用于测试）
func (n *Notifier) sendEmailByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	// 构造一个临时的 NotificationMessage 对象用于测试
	msg := NotificationMessage{
		Type:      "sms",
		From:      "测试发送方",
		Content:   message,
		Timestamp: time.Now().Unix(),
	}
	return n.sendEmail(ctx, config, msg)
}

// SendEmailByConfig 导出方法供外部调用（用于测试）
func (n *Notifier) SendEmailByConfig(ctx context.Context, config map[string]interface{}, message string) error {
	return n.sendEmailByConfig(ctx, config, message)
}

// SendEmail 发送邮件通知（通用方法）
func (n *Notifier) SendEmail(ctx context.Context, config map[string]interface{}, msg NotificationMessage) error {
	return n.sendEmail(ctx, config, msg)
}

func buildProxyURL(rawProxyURL string, username string, password string) (*url.URL, error) {
	u, err := url.Parse(rawProxyURL)
	if err != nil {
		return nil, err
	}

	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u, nil
}
