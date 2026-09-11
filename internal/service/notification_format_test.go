package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestFormatSMSForwardingMessage(t *testing.T) {
	msg := NotificationMessage{
		Content: "验证码 1234", From: "10086", To: "13800138000",
		DeviceName: "客厅设备", SIMID: "iccid:123", ICCID: "123",
		Timestamp: time.Date(2026, 9, 11, 20, 30, 0, 0, time.Local).Unix(),
	}
	got := formatSMSForwardingMessage(
		"{{content}}\n接收: {{receiver}}\n发送: {{from}}\n时间: {{timestamp}}\n{{unknown}}", msg,
	)
	for _, want := range []string{"验证码 1234", "接收: 13800138000", "发送: 10086", "2026-09-11 20:30:00", "{{unknown}}"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted message %q does not contain %q", got, want)
		}
	}
}

func TestSendBarkByConfig(t *testing.T) {
	var payload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer server.Close()

	notifier := NewNotifier(zap.NewNop())
	err := notifier.SendBarkByConfig(context.Background(), map[string]interface{}{
		"serverUrl": server.URL,
		"deviceKey": "device-key",
		"title":     "短信通知",
		"group":     "sms",
	}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if payload["device_key"] != "device-key" || payload["body"] != "hello" || payload["group"] != "sms" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSendWeComAppByConfig(t *testing.T) {
	var sendPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			if r.URL.Query().Get("corpid") != "corp-id" || r.URL.Query().Get("corpsecret") != "secret" {
				t.Errorf("token query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"token"}`))
		case "/cgi-bin/message/send":
			if r.URL.Query().Get("access_token") != "token" {
				t.Errorf("access token = %q", r.URL.Query().Get("access_token"))
			}
			if err := json.NewDecoder(r.Body).Decode(&sendPayload); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	notifier := NewNotifier(zap.NewNop())
	notifier.weComAPIBaseURL = server.URL
	err := notifier.SendWeComAppByConfig(context.Background(), map[string]interface{}{
		"corpId": "corp-id", "secret": "secret", "agentId": "1000002", "toUser": "zhangsan",
	}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if sendPayload["touser"] != "zhangsan" || sendPayload["agentid"] != float64(1000002) {
		t.Fatalf("send payload = %#v", sendPayload)
	}
}

func TestSendWeComAppUsesProxyForTokenAndMessage(t *testing.T) {
	proxyCalls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls++
		if r.URL.Host != "qyapi.example" {
			t.Errorf("proxied host = %q", r.URL.Host)
		}
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"proxy-token"}`))
		case "/cgi-bin/message/send":
			if r.URL.Query().Get("access_token") != "proxy-token" {
				t.Errorf("access token = %q", r.URL.Query().Get("access_token"))
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()

	notifier := NewNotifier(zap.NewNop())
	notifier.weComAPIBaseURL = "http://qyapi.example"
	err := notifier.SendWeComAppByConfig(context.Background(), map[string]interface{}{
		"corpId": "corp-id", "secret": "secret", "agentId": "1000002", "toUser": "@all",
		"proxyEnabled": true, "proxyUrl": proxy.URL,
	}, "through proxy")
	if err != nil {
		t.Fatal(err)
	}
	if proxyCalls != 2 {
		t.Fatalf("proxy calls = %d, want token and message requests", proxyCalls)
	}
}
