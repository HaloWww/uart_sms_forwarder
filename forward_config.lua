-- 同时烧录 main.lua、direct_forward.lua、本文件，并勾选“添加默认 lib”。
-- uart 保持原有 Ubuntu/Windows 转发；direct 无需主机，使用当前 SIM 的流量。
return {
    mode = "uart",
    apn = "", -- 普通手机卡留空；专网卡按运营商要求填写
    queue_limit = 8, -- 1~8，入队记录最多 3400 个 JSON 字节，预留重试状态空间
    max_attempts = 3, -- 每条消息、每个渠道的总尝试次数（含首次）
    retry_base_seconds = 30,
    retry_max_seconds = 900,
    min_interval_seconds = 2,
    stats_save_seconds = 60,
    timezone_offset_seconds = 8 * 3600,
    template = "{{content}}\n接收号码：{{receiver}}\n时间：{{time}}",
    channels = {
        {
            id = "bark", type = "bark", enabled = false,
            server = "https://api.day.app", key = "", title = "短信通知",
            -- ca_cert = io.readFile("/luadb/bark-ca.crt"),
        },
        {
            id = "wecom", type = "wecom_app", enabled = false,
            corp_id = "", agent_id = 1000002, secret = "", to_user = "@all",
            -- 企业微信的可信 IP 限制仍适用，SIM 出口 IP 可能变化。
            -- ca_cert = io.readFile("/luadb/wecom-ca.crt"),
        },
        {
            id = "webhook", type = "webhook", enabled = false,
            url = "https://example.com/sms",
            -- POST JSON，成功需返回 HTTP 2xx；建议服务器返回 204。
            -- 使用 message_id / Idempotency-Key 做幂等去重。
            -- ca_cert = io.readFile("/luadb/webhook-ca.crt"),
        },
    },
}
