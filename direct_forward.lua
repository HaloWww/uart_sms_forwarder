-- LuatOS 独立短信/来电转发。调度由 main.lua 负责，便于离线回归测试。
local M = {}
local SLOT_COUNT = 8
local STATS_KEY = "direct_stats_v1"
local function slot_key(slot) return "direct_queue_v1_" .. slot end
local function bounded(value, fallback, low, high)
    return math.max(low, math.min(high, math.floor(tonumber(value) or fallback)))
end
local function nonempty(value) return type(value) == "string" and value ~= "" end
local function https(value)
    return type(value) == "string" and value:match("^https://[^/%s]+") ~= nil and
        not value:find("[\r\n]")
end
local function encode_query(value)
    return tostring(value):gsub("([^%w%-_%.~])", function(c)
        return string.format("%%%02X", string.byte(c))
    end)
end
local function api_error(prefix, code)
    -- 只暴露数字业务错误码，绝不回显可能含有密钥或正文的 errmsg。
    if type(code) == "number" and code == code and math.abs(code) < math.huge then
        return prefix .. "_" .. tostring(code)
    end
    return prefix
end
local function route_key(channel)
    -- 只存不可直接读出的路由摘要，不把 key/secret 复制进待发队列。
    -- 配置改了接收方时，旧队列不会投递给新的接收方。
    local hash = 0
    for _, key in ipairs({"type", "server", "url", "key", "corp_id", "agent_id", "to_user", "secret"}) do
        local value = tostring(channel[key] or "") .. "\0"
        for index = 1, #value do hash = (hash * 31 + value:byte(index)) % 2147483647 end
    end
    return tostring(hash)
end

-- 独立校验/规范化用于串口配置与启动恢复；不会发起 HTTP 或修改 Flash。
function M.validate_config(value)
    if type(value) ~= "table" or (value.mode ~= "uart" and value.mode ~= "direct") then
        return nil, "invalid_forward_mode"
    end
    local normalized = {mode = value.mode, channels = {}}
    local ranges = {
        queue_limit = {8, 1, 8}, max_attempts = {3, 1, 10},
        retry_base_seconds = {30, 5, 3600}, retry_max_seconds = {900, 5, 86400},
        min_interval_seconds = {2, 1, 3600}, stats_save_seconds = {60, 10, 3600},
        timezone_offset_seconds = {28800, -43200, 50400},
    }
    for key, range in pairs(ranges) do
        local number = value[key]
        if number == nil then number = range[1] end
        if type(number) ~= "number" or number ~= number or number < range[2] or
            number > range[3] or number ~= math.floor(number) then return nil, "invalid_" .. key end
        normalized[key] = math.floor(number)
    end
    if normalized.retry_max_seconds < normalized.retry_base_seconds then return nil, "invalid_retry_range" end
    normalized.apn = value.apn or ""
    normalized.template = value.template or "{{content}}\n接收号码：{{receiver}}\n时间：{{time}}"
    if type(normalized.apn) ~= "string" or #normalized.apn > 100 or normalized.apn:find("[\r\n%z]") then
        return nil, "invalid_apn"
    end
    if type(normalized.template) ~= "string" or normalized.template:match("^%s*$") or #normalized.template > 1024 then
        return nil, "invalid_template"
    end
    if value.channels ~= nil and type(value.channels) ~= "table" then return nil, "invalid_channels" end
    local ids, count, enabled = {}, 0, 0
    for index, channel in pairs(value.channels or {}) do
        if type(index) ~= "number" or index < 1 or index ~= math.floor(index) or index > 3 or
            type(channel) ~= "table" then return nil, "invalid_channels" end
        count = count + 1
    end
    if count ~= #(value.channels or {}) then return nil, "invalid_channels" end
    for index, channel in ipairs(value.channels or {}) do
        if type(channel.id) ~= "string" or #channel.id == 0 or #channel.id > 40 or
            not channel.id:match("^[A-Za-z0-9_-]+$") or ids[channel.id] then return nil, "invalid_channel_id" end
        if channel.type ~= "bark" and channel.type ~= "wecom_app" and channel.type ~= "webhook" then
            return nil, "invalid_channel_type"
        end
        if channel.enabled ~= nil and type(channel.enabled) ~= "boolean" then return nil, "invalid_channel_enabled" end
        local item = {id = channel.id, type = channel.type, enabled = channel.enabled == true}
        ids[item.id] = true
        for _, key in ipairs({"server", "key", "title", "group", "corp_id", "secret", "to_user", "url", "ca_cert"}) do
            if channel[key] ~= nil then
                if type(channel[key]) ~= "string" or channel[key]:find("%z") then return nil, "invalid_channel_field" end
                item[key] = channel[key]
            end
        end
        if channel.agent_id ~= nil then
            if type(channel.agent_id) ~= "number" or channel.agent_id ~= channel.agent_id or
                channel.agent_id < 0 or channel.agent_id > 2147483647 or
                channel.agent_id ~= math.floor(channel.agent_id) then return nil, "invalid_agent_id" end
            item.agent_id = math.floor(channel.agent_id)
        end
        if item.enabled then
            enabled = enabled + 1
            local endpoint = item.type == "bark" and (item.server or "https://api.day.app") or item.url
            if item.type ~= "wecom_app" and (not https(endpoint) or endpoint:find("@", 1, true) or endpoint:find("#", 1, true)) then
                return nil, "invalid_channel_https_url"
            end
            if item.type == "bark" and not nonempty(item.key) then return nil, "bark_key_required" end
            if item.type == "wecom_app" and (not nonempty(item.corp_id) or not nonempty(item.secret) or
                not nonempty(item.to_user) or not item.agent_id or item.agent_id < 1) then return nil, "wecom_config_required" end
        end
        normalized.channels[index] = item
    end
    if value.mode == "direct" and enabled == 0 then return nil, "no_enabled_channels" end
    local ok, encoded = pcall(json.encode, normalized)
    if not ok or type(encoded) ~= "string" then return nil, "config_encoding_failed" end
    if #encoded > 3800 then return nil, "config_too_large" end
    if encoded:find(":CMD_END", 1, true) or encoded:find(":SMS_END", 1, true) then
        return nil, "config_contains_protocol_marker"
    end
    return normalized
end

function M.new(config, deps)
    config, deps = config or {}, deps or {}
    local self = {queue = {}, channels = {}, tokens = {}, config = config}
    local json_api, http_api = deps.json or json, deps.http or http
    local mobile_api, socket_api = deps.mobile or mobile, deps.socket or socket
    local store = deps.fskv or fskv
    local now = deps.now or function() return mcu.ticks2() end
    local can_send = deps.can_send or function() return true end
    local limit = bounded(config.queue_limit, 8, 1, SLOT_COUNT)
    local max_attempts = bounded(config.max_attempts, 3, 1, 10)
    local retry_base = bounded(config.retry_base_seconds, 30, 5, 3600)
    local retry_max = bounded(config.retry_max_seconds, 900, retry_base, 86400)
    local interval = bounded(config.min_interval_seconds, 2, 1, 3600)
    local save_interval = bounded(config.stats_save_seconds, 60, 10, 3600)
    local last_attempt, last_save = -math.huge, now()
    local dirty = false
    local stats = {sent = 0, failed = 0, dropped = 0, requests = 0, unmetered_requests = 0,
        tx_bytes = 0.0, rx_bytes = 0.0, last_error = "", counter_available = false,
        storage_available = false, enabled = config.mode == "direct"}

    local function error_status(reason)
        stats.last_error, dirty = reason, true
    end
    if stats.enabled then
        if not json_api or not http_api or not socket_api or socket_api.LWIP_GP == nil then
            stats.enabled = false
            error_status("firmware_missing_http_json_or_cellular_adapter")
        end
        local ids = {}
        for index, channel in ipairs(config.channels or {}) do
            if channel.enabled then
                local id = channel.id or tostring(index)
                local valid = not ids[id] and type(id) == "string" and id:match("^[%w_-]+$")
                if channel.type == "bark" then
                    valid = valid and https(channel.server or "https://api.day.app") and nonempty(channel.key)
                elseif channel.type == "wecom_app" then
                    valid = valid and nonempty(channel.corp_id) and nonempty(channel.secret) and
                        tonumber(channel.agent_id) and tonumber(channel.agent_id) > 0 and nonempty(channel.to_user)
                elseif channel.type == "webhook" then
                    valid = valid and https(channel.url)
                else
                    valid = false
                end
                if not valid then
                    stats.enabled = false
                    error_status("invalid_channel_config_" .. index)
                else
                    channel.id = id
                    ids[id], self.channels[id] = true, channel
                end
            end
        end
        if next(self.channels) == nil then
            stats.enabled = false
            error_status("no_enabled_channels")
        end
    end

    local store_ready = false
    if stats.enabled and store and store.init and store.get and store.set and store.del then
        local ok, result = pcall(store.init)
        store_ready = ok and result == true
        stats.storage_available = store_ready
        if store_ready then
            local read_ok, saved = pcall(store.get, STATS_KEY)
            if not read_ok then
                stats.storage_available = false
                error_status("storage_read_failed")
            end
            if read_ok and type(saved) == "table" then
                for _, key in ipairs({"sent", "failed", "dropped", "requests", "unmetered_requests", "tx_bytes", "rx_bytes"}) do
                    stats[key] = math.max(0, tonumber(saved[key]) or 0)
                end
                stats.tx_bytes, stats.rx_bytes = stats.tx_bytes + 0.0, stats.rx_bytes + 0.0
            end
            for slot = 1, SLOT_COUNT do
                local read_ok, item = pcall(store.get, slot_key(slot))
                if not read_ok then
                    stats.storage_available = false
                    error_status("storage_read_failed")
                end
                if read_ok and type(item) == "table" and type(item.message) == "table" and
                    type(item.targets) == "table" then
                    item.slot = slot
                    local complete = true
                    for _, target in ipairs(item.targets) do
                        target.next_at = 0
                        if not target.done then complete = false end
                    end
                    if complete then pcall(store.del, slot_key(slot))
                    else self.queue[#self.queue + 1] = item end
                end
            end
        end
    end
    if stats.enabled and not store_ready then error_status("storage_unavailable_ram_only") end

    local function persist(item, remove)
        if not store_ready then return false end
        local ok, result
        if remove then ok, result = pcall(store.del, slot_key(item.slot))
        else ok, result = pcall(store.set, slot_key(item.slot), item) end
        stats.storage_available = ok and result == true
        if not stats.storage_available then error_status("storage_write_failed") end
        return stats.storage_available
    end
    function self:flush()
        if store_ready and dirty then
            local values = {}
            for _, key in ipairs({"sent", "failed", "dropped", "requests", "unmetered_requests", "tx_bytes", "rx_bytes"}) do
                values[key] = stats[key]
            end
            local ok, result = pcall(store.set, STATS_KEY, values)
            if ok and result == true then dirty, last_save = false, now()
            else
                stats.storage_available = false
                error_status("stats_write_failed")
            end
        end
    end
    local function counter()
        if not mobile_api or type(mobile_api.dataTraffic) ~= "function" then return nil end
        local ok, tx_gb, tx_b, rx_gb, rx_b = pcall(mobile_api.dataTraffic)
        if not ok then return nil end
        for _, value in ipairs({tx_gb, tx_b, rx_gb, rx_b}) do
            if type(value) ~= "number" or value < 0 or value ~= value or value == math.huge then return nil end
        end
        if type(tx_gb) ~= "number" or type(tx_b) ~= "number" or
            type(rx_gb) ~= "number" or type(rx_b) ~= "number" then return nil end
        return {tx_gb, tx_b, rx_gb, rx_b}
    end
    local function ready(iccid)
        local ok, value = pcall(can_send, iccid)
        if not ok or not value then return false end
        local net_ok, connected = pcall(socket_api.adapter, socket_api.LWIP_GP)
        return net_ok and connected == true
    end
    local function request(channel, item, method, url, payload)
        if not ready(item.message.iccid) then return nil, nil, "network_or_sim_not_ready" end
        local headers = {["Accept"] = "application/json"}
        local body
        if payload then
            body = json_api.encode(payload)
            headers["Content-Type"] = "application/json"
            if channel.type == "webhook" then headers["Idempotency-Key"] = item.message.message_id end
        end
        local before = counter()
        stats.requests, dirty = stats.requests + 1, true
        local ok, code, _, response = pcall(function()
            return http_api.request(method, url, headers, body, {
                adapter = socket_api.LWIP_GP, timeout = 15000, debug = false,
            }, channel.ca_cert).wait()
        end)
        local after = counter()
        stats.counter_available = before ~= nil and after ~= nil
        if stats.counter_available then
            -- 用浮点累计，兼容 Lua 整数仅 32 位的固件；2^53 字节内仍为精确整数。
            local tx = (after[1] - before[1]) * 1073741824.0 + after[2] - before[2]
            local rx = (after[3] - before[3]) * 1073741824.0 + after[4] - before[4]
            if tx >= 0 and rx >= 0 then
                stats.tx_bytes, stats.rx_bytes = stats.tx_bytes + tx, stats.rx_bytes + rx
            else
                stats.counter_available = false
                error_status("traffic_counter_reset_during_request")
            end
        end
        if not stats.counter_available then
            stats.unmetered_requests = stats.unmetered_requests + 1
        end
        -- 不记录响应正文/URL，避免把 token、Secret、短信正文泄露到状态和日志。
        if not ok then return nil, nil, "http_exception" end
        local decoded
        if type(response) == "string" and #response > 0 then
            local decode_ok, result = pcall(json_api.decode, response)
            if decode_ok and type(result) == "table" then decoded = result end
        end
        return tonumber(code), decoded
    end
    local function transport_result(code, reason)
        return false, reason or ("http_" .. tostring(code or "network")),
            code == nil or code < 0 or code == 408 or code == 429 or code >= 500
    end
    local function send(channel, item)
        local message = item.message
        if channel.type == "wecom_app" then
            local token = self.tokens[channel.id]
            if not token or token.expires_at <= now() then
                local code, data, reason = request(channel, item, "GET",
                    "https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=" .. encode_query(channel.corp_id) ..
                    "&corpsecret=" .. encode_query(channel.secret))
                if not code or code < 200 or code >= 300 then return transport_result(code, reason) end
                if not data or data.errcode ~= 0 or not nonempty(data.access_token) then
                    return false, api_error("wecom_token_rejected", data and data.errcode), data and (data.errcode == -1 or data.errcode == 45009) or false
                end
                token = {value = data.access_token,
                    expires_at = now() + math.max(1, (tonumber(data.expires_in) or 7200) - 60)}
                self.tokens[channel.id] = token
            end
            local code, data, reason = request(channel, item, "POST",
                "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=" .. encode_query(token.value), {
                    touser = channel.to_user, msgtype = "text", agentid = tonumber(channel.agent_id),
                    text = {content = item.rendered}, enable_duplicate_check = 1, duplicate_check_interval = 1800,
                })
            if not code or code < 200 or code >= 300 then return transport_result(code, reason) end
            if data and (data.errcode == 40014 or data.errcode == 42001) then
                self.tokens[channel.id] = nil
                return false, "wecom_token_expired", true
            end
            if not data or data.errcode ~= 0 or nonempty(data.invaliduser) or
                nonempty(data.invalidparty) or nonempty(data.invalidtag) then
                return false, api_error("wecom_message_rejected", data and data.errcode), data and (data.errcode == -1 or data.errcode == 45009) or false
            end
            return true
        end
        local url, payload
        if channel.type == "bark" then
            url = (channel.server or "https://api.day.app"):gsub("/+$", "") .. "/push"
            payload = {device_key = channel.key, title = channel.title or "短信通知", body = item.rendered}
            if nonempty(channel.group) then payload.group = channel.group end
        else
            url = channel.url
            payload = {message_id = message.message_id, type = message.type, from = message.from,
                receiver = message.number, timestamp = message.timestamp, content = item.rendered,
                iccid = message.iccid}
        end
        local code, data, reason = request(channel, item, "POST", url, payload)
        if not code or code < 200 or code >= 300 then return transport_result(code, reason) end
        if channel.type == "bark" and (not data or data.code ~= 200) then
            return false, api_error("bark_message_rejected", data and data.code), data and tonumber(data.code) and tonumber(data.code) >= 500 or false
        end
        return true
    end
    function self:enqueue(message)
        if config.mode ~= "direct" then return false end
        if not stats.enabled then stats.dropped, dirty = stats.dropped + 1, true; return false end
        if not message.identity_valid or not nonempty(message.iccid) or not nonempty(message.message_id) then
            stats.dropped = stats.dropped + 1
            error_status("incoming_sim_identity_unavailable")
            return false
        end
        for _, item in ipairs(self.queue) do
            if item.message.message_id == message.message_id then return true end
        end
        if #self.queue >= limit then
            stats.dropped = stats.dropped + 1
            error_status("queue_full")
            return false
        end
        local item = {message = {}, targets = {}}
        for _, key in ipairs({"message_id", "type", "from", "content", "timestamp", "iccid", "number"}) do
            item.message[key] = message[key]
        end
        if message.type == "incoming_call" then item.message.content = "来电提醒" end
        local values = {content = item.message.content or "", from = message.from or "",
            receiver = message.number or "", to = message.number or "", iccid = message.iccid,
            imei = message.imei or "", imsi = message.imsi or "",
            timestamp = tostring(message.timestamp or 0)}
        values.time = os.date("!%Y-%m-%d %H:%M:%S", (message.timestamp or 0) +
            (tonumber(config.timezone_offset_seconds) or 28800))
        item.rendered = (config.template or "{{content}}\n来自：{{from}}\n{{time}}"):gsub("{{([%w_]+)}}", function(key)
            return values[key] or ""
        end)
        for _, channel in ipairs(config.channels or {}) do
            if channel.enabled then
                item.targets[#item.targets + 1] = {id = channel.id, route = route_key(channel), attempts = 0, next_at = 0}
            end
        end
        local used = {}
        for _, pending in ipairs(self.queue) do used[pending.slot] = true end
        for slot = 1, SLOT_COUNT do if not used[slot] then item.slot = slot; break end end
        -- 为完成状态、错误码与重试元数据预留空间，避免超过 fskv 单值限制。
        local ok, serialized = pcall(json_api.encode, item)
        if not ok or type(serialized) ~= "string" or #serialized > 3400 then
            stats.dropped = stats.dropped + 1
            error_status("queue_message_too_large")
            return false
        end
        self.queue[#self.queue + 1] = item
        dirty = true
        persist(item)
        return true
    end
    function self:step()
        if not stats.enabled then return end
        if dirty and now() - last_save >= save_interval then self:flush() end
        if now() - last_attempt < interval then return end
        for index, item in ipairs(self.queue) do
            if ready(item.message.iccid) then
                for _, target in ipairs(item.targets) do
                    if not target.done and (target.next_at or 0) <= now() then
                        local channel = self.channels[target.id]
                        if channel and target.route == route_key(channel) and (target.attempts or 0) < max_attempts then
                            last_attempt = now()
                            target.attempts = (target.attempts or 0) + 1
                            -- 存储暂时写失败时仅本地等待恢复，不借助重启反复重置尝试上限。
                            if store_ready and not persist(item) then
                                target.attempts = target.attempts - 1
                                return
                            end
                            -- 唯一发送 worker；每个渠道完成后立即持久化，重启不重发已确认成功的渠道。
                            local ok, success, reason, retry = pcall(send, channel, item)
                            if ok and success then
                                target.done, stats.sent = true, stats.sent + 1
                                stats.last_error = ""
                            else
                                reason = ok and reason or "forward_exception"
                                error_status(target.id .. ":" .. tostring(reason))
                                if ok and retry and target.attempts < max_attempts then
                                    target.next_at = now() + math.min(retry_max, retry_base * 2 ^ (target.attempts - 1))
                                else
                                    target.done, stats.failed = true, stats.failed + 1
                                end
                            end
                        else
                            target.done, stats.failed = true, stats.failed + 1
                            error_status("queued_channel_changed_or_attempts_exhausted")
                        end
                        dirty = true
                        local complete = true
                        for _, t in ipairs(item.targets) do if not t.done then complete = false end end
                        if complete then
                            persist(item) -- 先保存完成标记，删除失败时重启也不重发。
                            persist(item, true)
                            table.remove(self.queue, index)
                        else persist(item) end
                        self:flush()
                        return
                    end
                end
            end
        end
    end
    function self:status()
        local result = {}
        for key, value in pairs(stats) do result[key] = value end
        result.queued = #self.queue
        result.total_bytes = stats.tx_bytes + stats.rx_bytes
        return result
    end
    return self
end
return M
