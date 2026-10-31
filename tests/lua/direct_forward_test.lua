-- Run with Lua 5.3; all modem, HTTP, storage and time dependencies are mocked.
package.path = "./?.lua;" .. package.path

local function copy(value)
    if type(value) ~= "table" then return value end
    local result = {}
    for key, item in pairs(value) do result[key] = copy(item) end
    return result
end

-- A real JSON encoder keeps queue size tests representative. Responses use a
-- fixture registry because JSON decoding itself belongs to LuatOS, not here.
local fixtures = {}
local function encode(value)
    local kind = type(value)
    if kind == "nil" then return "null" end
    if kind == "boolean" or kind == "number" then return tostring(value) end
    if kind == "string" then
        return '"' .. value:gsub('[%z\1-\31\\"]', function(char)
            local escaped = {['"'] = '\\"', ['\\'] = '\\\\', ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t'}
            return escaped[char] or string.format("\\u%04x", char:byte())
        end) .. '"'
    end
    assert(kind == "table", "unsupported JSON value")
    local values = {}
    if #value > 0 then
        for index = 1, #value do values[index] = encode(value[index]) end
        return "[" .. table.concat(values, ",") .. "]"
    end
    local keys = {}
    for key in pairs(value) do keys[#keys + 1] = key end
    table.sort(keys)
    for _, key in ipairs(keys) do values[#values + 1] = encode(tostring(key)) .. ":" .. encode(value[key]) end
    return "{" .. table.concat(values, ",") .. "}"
end

local json = {
    encode = function(value)
        local encoded = encode(value)
        fixtures[encoded] = copy(value)
        return encoded
    end,
    decode = function(value) return copy(fixtures[value]) end,
}

local function response(code, value, tx, rx)
    return {code = code, body = json.encode(value), tx = tx or 100, rx = rx or 200}
end

local default_channel = {id = "bark", type = "bark", enabled = true, server = "https://api.day.app", key = "secret-bark-key"}
local function config(channels)
    return {
        mode = "direct", channels = channels or {copy(default_channel)},
        queue_limit = 8, max_attempts = 3,
        retry_base_seconds = 30, retry_max_seconds = 900,
        min_interval_seconds = 2, stats_save_seconds = 60,
        template = "{{content}}\n接收: {{receiver}}\n来自: {{from}}",
    }
end

local function message(id)
    return {
        type = "incoming_sms", message_id = id or "message-1", from = "10086",
        content = "短信正文", timestamp = 1700000000, iccid = "898600000000001",
        number = "13800138000", identity_valid = true,
    }
end

local Direct = require("direct_forward")
local function environment(options)
    options = options or {}
    local env = {
        time = 100, tx = 1000, rx = 2000, network = true, allowed_sim = "898600000000001",
        calls = {}, responses = {}, storage = options.storage or {},
    }
    env.deps = {
        json = json, now = function() return env.time end,
        can_send = function(iccid) return env.allowed_sim == iccid end,
        socket = {LWIP_GP = 4, adapter = function(adapter) return adapter == 4 and env.network end},
        mobile = {dataTraffic = function()
            if env.counter_missing then return nil end
            return math.floor(env.tx / 1073741824), env.tx % 1073741824,
                math.floor(env.rx / 1073741824), env.rx % 1073741824
        end},
        fskv = {
            init = function() return not env.storage_broken end,
            get = function(key) return copy(env.storage[key]) end,
            set = function(key, value)
                if env.storage_broken or env.fail_set == key then return false end
                env.storage[key] = copy(value)
                return true
            end,
            del = function(key)
                if env.storage_broken or env.fail_del == key then return false end
                env.storage[key] = nil
                return true
            end,
        },
        http = {request = function(method, url, headers, body, opts)
            env.calls[#env.calls + 1] = {method = method, url = url, headers = copy(headers), body = body, opts = copy(opts)}
            local item = table.remove(env.responses, 1) or response(200, {code = 200, errcode = 0})
            env.tx = env.tx + item.tx
            env.rx = env.rx + item.rx
            return {wait = function() return item.code, {}, item.body end}
        end},
    }
    function env:new(configuration) return Direct.new(configuration or config(), self.deps) end
    function env:advance(seconds) self.time = self.time + (seconds or 2) end
    return env
end

local function equal(actual, expected, reason)
    assert(actual == expected, (reason or "unexpected value") .. ": expected " .. tostring(expected) .. ", got " .. tostring(actual))
end

local tests = {}
function tests.uart_never_uses_mobile_data()
    local env = environment()
    local configuration = config()
    configuration.mode = "uart"
    local engine = env:new(configuration)
    engine:enqueue(message())
    engine:step()
    equal(#env.calls, 0)
    equal(engine:status().enabled, false)
end

function tests.offline_or_different_sim_pauses_without_spending_attempts()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message()))
    env.network = false
    for _ = 1, 10 do env:advance(60); engine:step() end
    equal(#env.calls, 0)
    equal(engine:status().queued, 1)
    env.network = true
    env.allowed_sim = "another-sim"
    engine:step()
    equal(#env.calls, 0)
    env.allowed_sim = message().iccid
    engine:step()
    equal(#env.calls, 1)
    equal(engine:status().queued, 0)
end

function tests.cellular_adapter_and_template_are_used()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message()))
    engine:step()
    equal(env.calls[1].opts.adapter, env.deps.socket.LWIP_GP)
    local payload = json.decode(env.calls[1].body)
    assert(payload.body:find("短信正文", 1, true))
    assert(payload.body:find("13800138000", 1, true))
    assert(payload.body:find("10086", 1, true))
    equal(engine:status().tx_bytes, 100)
    equal(engine:status().rx_bytes, 200)
    equal(engine:status().total_bytes, 300)
end

function tests.failed_requests_are_metered_and_backed_off()
    local env = environment()
    local engine = env:new()
    env.responses = {response(503, {}, 123, 456), response(200, {code = 200}, 10, 20)}
    assert(engine:enqueue(message()))
    engine:step()
    engine:step()
    env:advance(29)
    engine:step()
    equal(#env.calls, 1, "backoff must suppress repeated network requests")
    env:advance(1)
    engine:step()
    equal(#env.calls, 2)
    equal(engine:status().tx_bytes, 133)
    equal(engine:status().rx_bytes, 476)
    equal(engine:status().queued, 0)
end

function tests.completed_channels_are_not_retried()
    local env = environment()
    local second = copy(default_channel)
    second.id, second.key = "bark2", "second-key"
    local engine = env:new(config({copy(default_channel), second}))
    env.responses = {response(200, {code = 200}), response(503, {}), response(200, {code = 200})}
    assert(engine:enqueue(message()))
    engine:step()
    env:advance()
    engine:step()
    env:advance(30)
    engine:step()
    equal(#env.calls, 3)
    equal(engine:status().queued, 0)
    local first_payload = json.decode(env.calls[1].body)
    local last_payload = json.decode(env.calls[3].body)
    equal(first_payload.device_key, "secret-bark-key")
    equal(last_payload.device_key, "second-key")
end

function tests.retry_limit_stops_permanent_network_failure()
    local env = environment()
    local engine = env:new()
    env.responses = {response(503, {}), response(503, {}), response(503, {})}
    assert(engine:enqueue(message()))
    for _ = 1, 6 do engine:step(); env:advance(1000) end
    equal(#env.calls, 3)
    equal(engine:status().queued, 0)
    assert(engine:status().failed > 0)
end

local function wecom_channel()
    return {id = "wecom", type = "wecom_app", enabled = true,
        corp_id = "corp", agent_id = 1000002, secret = "never-log-this-secret", to_user = "user1"}
end

function tests.wecom_caches_tokens_and_counts_token_traffic()
    local env = environment()
    local engine = env:new(config({wecom_channel()}))
    env.responses = {
        response(200, {errcode = 0, access_token = "cached-token", expires_in = 7200}, 12, 34),
        response(200, {errcode = 0}, 56, 78),
        response(200, {errcode = 0}),
        response(200, {errcode = 0, access_token = "new-token", expires_in = 7200}),
        response(200, {errcode = 0}),
    }
    assert(engine:enqueue(message("one")))
    engine:step()
    equal(#env.calls, 2)
    equal(engine:status().tx_bytes, 68)
    equal(engine:status().rx_bytes, 112)
    env:advance()
    assert(engine:enqueue(message("two")))
    engine:step()
    equal(#env.calls, 3, "second message must reuse access token")
    env:advance(7201)
    assert(engine:enqueue(message("three")))
    engine:step()
    equal(#env.calls, 5, "expired token must refresh once")
end

function tests.http_200_business_failure_is_not_success_or_secret_leak()
    local env = environment()
    local engine = env:new(config({wecom_channel()}))
    env.responses = {response(200, {errcode = 40013, errmsg = "never-log-this-secret"})}
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().sent, 0)
    assert(not engine:status().last_error:find("never-log-this-secret", 1, true))
end

function tests.durable_queue_and_statistics_survive_restart()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message("one")))
    engine:step()
    engine:flush()
    env:advance()
    assert(engine:enqueue(message("two")))
    local restored = env:new()
    equal(restored:status().queued, 1)
    equal(restored:status().total_bytes, 300)
    restored:step()
    equal(restored:status().queued, 0)
    equal(#env.calls, 2)
end

function tests.capacity_and_oversize_messages_fail_visibly()
    local env = environment()
    local engine = env:new()
    for index = 1, 8 do assert(engine:enqueue(message("message-" .. index))) end
    equal(engine:enqueue(message("overflow")), false)
    equal(engine:status().queued, 8)
    assert(engine:status().dropped > 0)
    local fresh = environment():new()
    local oversized = message()
    oversized.content = string.rep("x", 10000)
    equal(fresh:enqueue(oversized), false)
    assert(fresh:status().dropped > 0)
end

function tests.counter_unavailable_does_not_report_payload_as_measured_traffic()
    local env = environment()
    env.counter_missing = true
    local engine = env:new()
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().counter_available, false)
    equal(engine:status().total_bytes, 0)
end

function tests.gigabyte_counter_rollover_is_measured()
    local env = environment()
    env.tx, env.rx = 1073741820, 2147483640
    local engine = env:new()
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().tx_bytes, 100)
    equal(engine:status().rx_bytes, 200)
end

function tests.plain_http_configuration_is_rejected_before_network_use()
    local env = environment()
    local unsafe = copy(default_channel)
    unsafe.server = "http://example.com"
    local engine = env:new(config({unsafe}))
    engine:enqueue(message())
    engine:step()
    equal(#env.calls, 0)
    assert(engine:status().last_error ~= "")
end

function tests.storage_write_failure_is_visible()
    local env = environment()
    local engine = env:new()
    env.storage_broken = true
    engine:enqueue(message())
    equal(engine:status().storage_available, false)
    assert(engine:status().last_error:find("storage", 1, true))
end

function tests.completed_channel_stays_completed_after_restart()
    local env = environment()
    local second = copy(default_channel)
    second.id, second.key = "bark2", "second-key"
    local configuration = config({copy(default_channel), second})
    local engine = env:new(configuration)
    assert(engine:enqueue(message()))
    engine:step()
    equal(#env.calls, 1)
    equal(engine:status().queued, 1)
    local restored = env:new(configuration)
    restored:step()
    equal(#env.calls, 2)
    equal(json.decode(env.calls[2].body).device_key, "second-key")
    equal(restored:status().queued, 0)
end

function tests.retry_attempt_limit_survives_repeated_restarts()
    local env = environment()
    local engine = env:new()
    env.responses = {response(503, {}), response(503, {}), response(503, {})}
    assert(engine:enqueue(message()))
    for _ = 1, 5 do
        engine:step()
        env:advance(1000)
        engine = env:new()
    end
    equal(#env.calls, 3)
    equal(engine:status().queued, 0)
end

function tests.wecom_expired_business_token_refreshes_after_backoff()
    local env = environment()
    local engine = env:new(config({wecom_channel()}))
    env.responses = {
        response(200, {errcode = 0, access_token = "expired-by-server", expires_in = 7200}),
        response(200, {errcode = 42001}),
        response(200, {errcode = 0, access_token = "replacement-token", expires_in = 7200}),
        response(200, {errcode = 0}),
    }
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().sent, 0)
    equal(#env.calls, 2)
    engine:step()
    equal(#env.calls, 2)
    env:advance(30)
    engine:step()
    equal(#env.calls, 4)
    assert(env.calls[4].url:find("replacement-token", 1, true))
    equal(engine:status().queued, 0)
    equal(engine:status().total_bytes, 1200)
end

function tests.counter_reset_during_request_does_not_inflate_statistics()
    local env = environment()
    local engine = env:new()
    local original_request = env.deps.http.request
    env.deps.http.request = function(...)
        local result = original_request(...)
        local original_wait = result.wait
        result.wait = function()
            env.tx, env.rx = 0, 0
            return original_wait()
        end
        return result
    end
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().total_bytes, 0)
    equal(engine:status().counter_available, false)
    equal(engine:status().unmetered_requests, 1)
    engine:flush()
    equal(env:new():status().unmetered_requests, 1)
end

function tests.counter_disappearing_during_request_is_not_zero_usage()
    local env = environment()
    local engine = env:new()
    local original_request = env.deps.http.request
    env.deps.http.request = function(...)
        local result = original_request(...)
        local original_wait = result.wait
        result.wait = function()
            env.counter_missing = true
            return original_wait()
        end
        return result
    end
    assert(engine:enqueue(message()))
    engine:step()
    equal(engine:status().counter_available, false)
    equal(engine:status().total_bytes, 0)
    equal(engine:status().unmetered_requests, 1)
end

function tests.completed_checkpoint_survives_failed_slot_cleanup()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message()))
    for key in pairs(env.storage) do
        if key:find("direct_queue", 1, true) then env.fail_del = key end
    end
    assert(env.fail_del, "queue must be durable before sending")
    engine:step()
    equal(#env.calls, 1)
    env.fail_del = nil
    local restored = env:new()
    restored:step()
    equal(#env.calls, 1, "confirmed success must not replay when slot deletion failed")
    equal(restored:status().queued, 0)
end

function tests.crashes_during_http_do_not_reset_attempt_budget()
    local env = environment()
    local engine = env:new()
    local original_request = env.deps.http.request
    env.deps.http.request = function(...)
        local request = original_request(...)
        request.wait = function() coroutine.yield("simulated power loss") end
        return request
    end
    assert(engine:enqueue(message()))
    for _ = 1, 5 do
        local worker = coroutine.create(function() engine:step() end)
        local ok, err = coroutine.resume(worker)
        assert(ok, err)
        -- A suspended worker represents power loss after the outgoing request;
        -- throw it away and restore only what actually reached Flash.
        env:advance(1000)
        engine = env:new()
    end
    equal(#env.calls, 3, "preflight attempt checkpoint must cap crash/reboot retries")
    equal(engine:status().queued, 0)
end

function tests.changed_recipient_does_not_receive_old_durable_messages()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message()))
    local changed = copy(default_channel)
    changed.key = "different-users-key"
    local restored = env:new(config({changed}))
    restored:step()
    equal(#env.calls, 0, "queued private message must keep its original destination")
    equal(restored:status().queued, 0)
    assert(restored:status().failed > 0)
end

function tests.failed_attempt_checkpoint_waits_for_storage_recovery()
    local env = environment()
    local engine = env:new()
    assert(engine:enqueue(message()))
    for key in pairs(env.storage) do
        if key:find("direct_queue", 1, true) then env.fail_set = key end
    end
    assert(env.fail_set)
    for _ = 1, 5 do engine:step(); env:advance(60) end
    equal(#env.calls, 0, "cannot spend data without durable attempt checkpoint")
    equal(engine:status().queued, 1, "storage failures must not spend the HTTP retry budget")
    env.fail_set = nil
    engine:step()
    equal(#env.calls, 1)
    equal(engine:status().queued, 0)
end

function tests.nonfinite_counters_are_unmetered()
    for _, invalid in ipairs({0 / 0, math.huge, -math.huge}) do
        local env = environment()
        env.deps.mobile.dataTraffic = function() return 0, invalid, 0, 0 end
        local engine = env:new()
        assert(engine:enqueue(message()))
        engine:step()
        equal(engine:status().counter_available, false)
        equal(engine:status().total_bytes, 0)
        equal(engine:status().unmetered_requests, 1)
    end
end

function tests.unmetered_requests_are_persisted_separately_from_measured_bytes()
    local env = environment()
    local engine = env:new()
    env.counter_missing = true
    assert(engine:enqueue(message("unknown-usage")))
    engine:step()
    equal(engine:status().unmetered_requests, 1)
    engine:flush()
    local restored = env:new()
    equal(restored:status().unmetered_requests, 1)
    env.counter_missing = false
    env:advance()
    assert(restored:enqueue(message("measured-usage")))
    restored:step()
    equal(restored:status().unmetered_requests, 1)
    equal(restored:status().total_bytes, 300)
end

-- Load the real entry point in an isolated Lua environment. Tasks are resumed
-- cooperatively only to establish the SIM snapshot; no real modem or network
-- library is loaded and no code substitutions are made in main.lua.
local function load_main(configuration, config_error, options)
    options = options or {}
    local env = {events = {}, tasks = {}, frames = {}, forwarded = {}, imports = {}, fly_calls = {},
        storage = options.storage or {}, http_calls = {}, time = 100, tx = 0, rx = 0,
        fail_set = options.fail_set, fail_get = options.fail_get, storage_broken = options.storage_broken,
        readback_mismatch = options.readback_mismatch, current_iccid = message().iccid}
    local sandbox = setmetatable({}, {__index = _G})
    local noop = function() end
    local sys = {
        subscribe = function(event, callback) env.events[event] = callback end,
        publish = noop, timerLoopStart = noop, run = noop,
        taskInit = function(task) env.tasks[#env.tasks + 1] = coroutine.create(task) end,
        wait = function() return coroutine.yield() end,
        waitUntil = function() return coroutine.yield() end,
    }
    sandbox.log = {info = noop, warn = noop, error = noop, debug = noop}
    sandbox.json = json
    sandbox.wdt = nil
    sandbox.uart = {
        VUART_0 = 100, setup = noop, on = noop,
        write = function(_, frame) env.frames[#env.frames + 1] = frame; return #frame end,
    }
    sandbox.mobile = {
        apn = noop, setAuto = noop, simid = function() return 0 end,
        simPin = function() return true end, iccid = function() return message().iccid end,
        imsi = function() return "460001234567890" end, number = function() return "13800138000" end,
        imei = function() return "123456789012345" end, muid = function() return "module-identity" end,
        status = function() return 1 end, csq = function() return 25 end,
        rssi = function() return -70 end, rsrp = function() return -90 end, rsrq = function() return -10 end,
        flymode = function(slot, enabled)
            env.fly_calls[#env.fly_calls + 1] = {slot = slot, enabled = enabled}
            return false
        end,
    }
    sandbox.mcu = {ticks2 = function() return 100 end}
    sandbox.cc = {lastNum = function() return "10010" end}
    sandbox.pm = {reboot = noop}
    sandbox.fskv = {
        init = function() return not env.storage_broken end,
        get = function(key)
            if env.fail_get == key then error("simulated Flash read failure") end
            if env.readback_mismatch and key == "direct_config_v1" and env.last_set == key then
                return {mode = "uart", channels = {}}
            end
            return copy(env.storage[key])
        end,
        set = function(key, value)
            if env.storage_broken or env.fail_set == key then return false end
            env.storage[key], env.last_set = copy(value), key
            return true
        end,
        del = function(key) env.storage[key] = nil; return true end,
    }
    sandbox.socket = {LWIP_GP = 4, adapter = function(adapter) return adapter == 4 end}
    sandbox.http = {request = function(method, url, headers, body, request_options)
        env.http_calls[#env.http_calls + 1] = {method = method, url = url, headers = copy(headers), body = body, options = copy(request_options)}
        env.tx, env.rx = env.tx + 100, env.rx + 200
        return {wait = function() return 200, {}, json.encode({code = 200, errcode = 0}) end}
    end}
    sandbox.mobile.iccid = function() return env.current_iccid end
    sandbox.mobile.dataTraffic = function() return 0, env.tx, 0, env.rx end
    sandbox.mcu.ticks2 = function() return env.time end
    local direct_module
    local function actual_direct_module()
        if not direct_module then
            direct_module = assert(loadfile("direct_forward.lua", "t", sandbox))()
        end
        return direct_module
    end
    sandbox.require = function(name)
        env.imports[name] = true
        if name == "sys" then return sys end
        if name == "forward_config" then
            if config_error then error(config_error) end
            return configuration
        end
        if name == "direct_forward" then
            if options.use_real_direct then return actual_direct_module() end
            return {validate_config = function(value) return actual_direct_module().validate_config(value) end,
                new = function(_, deps)
                env.direct_deps = deps
                return {
                    enqueue = function(_, msg) env.forwarded[#env.forwarded + 1] = copy(msg); return true end,
                    status = function() return {enabled = true, queued = #env.forwarded} end,
                    step = noop, flush = noop,
                }
            end}
        end
        error("unexpected module import: " .. name)
    end
    local chunk, err = loadfile("main.lua", "t", sandbox)
    assert(chunk, err)
    env.loaded, env.load_error = pcall(chunk)
    env.sandbox = sandbox
    function env:resume_tasks(rounds)
        for _ = 1, rounds or 1 do
            for _, task in ipairs(self.tasks) do
                if coroutine.status(task) ~= "dead" then
                    local ok, task_error = coroutine.resume(task)
                    assert(ok, task_error)
                end
            end
        end
    end
    function env:flush_uart()
        -- The worker transmits queued frames before yielding for more work.
        self:resume_tasks(1)
    end
    function env:last_frame(kind)
        for index = #self.frames, 1, -1 do
            local body = self.frames[index]:match("^SMS_START:(.*):SMS_END\r\n$")
            local data = body and json.decode(body)
            if data and data.type == kind then return data end
        end
    end
    function env:last_response(request_id)
        for index = #self.frames, 1, -1 do
            local body = self.frames[index]:match("^SMS_START:(.*):SMS_END\r\n$")
            local data = body and json.decode(body)
            if data and data.type == "cmd_response" and data.request_id == request_id then return data end
        end
    end
    function env:command(action, value, expected_iccid)
        local request_id = action .. "-" .. tostring(#self.frames + 1)
        self.sandbox.process_uart_command({action = action, request_id = request_id,
            expected_iccid = expected_iccid or message().iccid, config = value})
        self:flush_uart()
        local reply = self:last_response(request_id)
        assert(reply, "missing response for " .. action)
        return reply
    end
    return env
end

function tests.main_uart_default_does_not_import_direct_network_engine()
    local env = load_main({mode = "uart"})
    assert(env.loaded, env.load_error)
    equal(env.imports.direct_forward, nil)
    env:resume_tasks(3)
    env.events.SMS_INC("10086", "host forwarded")
    env:flush_uart()
    env:flush_uart()
    equal(env:last_frame("incoming_sms").forward_mode, "uart")
    equal(#env.forwarded, 0)
end

function tests.main_direct_sms_and_call_keep_mode_and_verified_sim()
    local env = load_main({mode = "direct"})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    env.events.SMS_INC("10086", "direct forwarded")
    env.events.CC_IND("INCOMINGCALL")
    equal(#env.forwarded, 2)
    for _, notification in ipairs(env.forwarded) do
        equal(notification.forward_mode, "direct")
        equal(notification.iccid, message().iccid)
        equal(notification.identity_valid, true)
    end
    equal(env.forwarded[1].type, "incoming_sms")
    equal(env.forwarded[2].type, "incoming_call")
    assert(env.direct_deps.can_send(message().iccid))
    equal(env.direct_deps.can_send("another-sim"), false)
end

function tests.main_config_syntax_error_stops_instead_of_falling_back()
    local env = load_main(nil, "forward_config.lua:12: unexpected symbol near '}'")
    equal(env.loaded, false)
    assert(tostring(env.load_error):find("forward_config", 1, true))
    equal(env.imports.direct_forward, nil)
    equal(#env.tasks, 0)
end

function tests.main_missing_optional_configuration_keeps_legacy_uart_mode()
    local env = load_main(nil, "module 'forward_config' not found: no file './forward_config.lua'")
    assert(env.loaded, env.load_error)
    equal(env.imports.direct_forward, nil)
    env:resume_tasks(3)
    env.sandbox.process_uart_command({action = "get_status"})
    env:flush_uart()
    equal(env:last_frame("status_response").forward_mode, "uart")
end

function tests.main_direct_rejects_auto_flight_enable_but_allows_disable()
    local env = load_main({mode = "direct"})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    env.sandbox.process_uart_command({action = "set_flymode", enabled = true, source = "automatic", expected_iccid = message().iccid})
    env:flush_uart()
    equal(#env.fly_calls, 0)
    equal(env:last_frame("cmd_response").result, "error")
    env.sandbox.process_uart_command({action = "set_flymode", enabled = false, source = "automatic", expected_iccid = message().iccid})
    env:flush_uart()
    equal(#env.fly_calls, 1)
    equal(env.fly_calls[1].enabled, false)
    equal(env:last_frame("cmd_response").result, "ok")
end

function tests.main_saved_direct_mode_works_without_host_and_survives_power_cycle()
    local env = load_main({mode = "uart", channels = {}}, nil, {use_real_direct = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    local initial = env:command("get_forward_config")
    equal(initial.result, "ok")
    equal(initial.persisted, false)
    equal(initial.config.mode, "uart")

    local saved = env:command("set_forward_config", config())
    equal(saved.result, "ok")
    equal(saved.persisted, true)
    equal(env.storage.direct_config_v1.mode, "direct")
    equal(env.storage.direct_config_v1.channels[1].key, default_channel.key)
    local readback = env:command("get_forward_config")
    equal(readback.config.mode, "direct")
    equal(readback.persisted, true)

    -- No get_status/host discovery after saving: the modem worker must run by
    -- itself, including when the boot mode was uart.
    env.events.SMS_INC("10086", "forward with the exe closed")
    env:resume_tasks(3)
    equal(#env.http_calls, 1)
    equal(env.http_calls[1].options.adapter, env.sandbox.socket.LWIP_GP)

    -- A new entry-point instance gets only durable Flash, not the old process
    -- state or a remembered Windows configuration.
    local rebooted = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, storage = env.storage})
    assert(rebooted.loaded, rebooted.load_error)
    rebooted:resume_tasks(3)
    rebooted.events.SMS_INC("10010", "forward after power cycle")
    rebooted:resume_tasks(3)
    equal(#rebooted.http_calls, 1)
    local restored = rebooted:command("get_forward_config")
    equal(restored.persisted, true)
    equal(restored.config.mode, "direct")
end

function tests.main_saved_uart_mode_disables_standalone_forwarding_after_reboot()
    local env = load_main(config(), nil, {use_real_direct = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    local saved = env:command("set_forward_config", {mode = "uart", channels = {}})
    equal(saved.result, "ok")
    equal(saved.persisted, true)
    env.events.SMS_INC("10086", "host only")
    env:resume_tasks(3)
    equal(#env.http_calls, 0)

    local rebooted = load_main(config(), nil, {use_real_direct = true, storage = env.storage})
    assert(rebooted.loaded, rebooted.load_error)
    rebooted:resume_tasks(3)
    rebooted.events.SMS_INC("10086", "still host only")
    rebooted:resume_tasks(3)
    equal(#rebooted.http_calls, 0)
    equal(rebooted:command("get_forward_config").config.mode, "uart")
end

function tests.main_configuration_save_failure_is_not_acknowledged_or_activated()
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, fail_set = "direct_config_v1"})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    local failed = env:command("set_forward_config", config())
    equal(failed.result, "error")
    assert(failed.persisted ~= true)
    equal(env.storage.direct_config_v1, nil)
    equal(env:command("get_forward_config").config.mode, "uart")
    env.events.SMS_INC("10086", "must remain host forwarded")
    env:resume_tasks(3)
    equal(#env.http_calls, 0)
end

function tests.main_configuration_requires_flash_before_success()
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, storage_broken = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    local failed = env:command("set_forward_config", config())
    equal(failed.result, "error")
    equal(env.storage.direct_config_v1, nil)
    equal(env:command("get_forward_config").config.mode, "uart")
end

function tests.main_configuration_queue_restore_error_does_not_activate_candidate()
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, fail_get = "direct_queue_v1_1"})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    equal(env:command("set_forward_config", config()).result, "error")
    equal(env.storage.direct_config_v1, nil)
    equal(env:command("get_forward_config").config.mode, "uart")
    env.events.SMS_INC("10086", "stay on the previous mode after restore failure")
    env:resume_tasks(3)
    equal(#env.http_calls, 0)
end

function tests.main_configuration_rejects_wrong_sim_and_missing_correlation()
    local env = load_main({mode = "uart", channels = {}}, nil, {use_real_direct = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    equal(env:command("set_forward_config", config(), "different-sim").result, "error")
    equal(env:command("get_forward_config", nil, "different-sim").result, "error")
    equal(env.storage.direct_config_v1, nil)
    env.sandbox.process_uart_command({action = "set_forward_config", expected_iccid = message().iccid, config = config()})
    env:flush_uart()
    equal(env.storage.direct_config_v1, nil)
    equal(env:command("get_forward_config").config.mode, "uart")
end

function tests.main_configuration_rejects_invalid_or_oversize_settings_without_secret_echo()
    local env = load_main({mode = "uart", channels = {}}, nil, {use_real_direct = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    local invalid = config()
    invalid.channels[1].server = "http://unsafe.invalid/secret-url"
    local rejected = env:command("set_forward_config", invalid)
    equal(rejected.result, "error")
    assert(not json.encode(rejected):find("secret-url", 1, true))
    assert(not json.encode(rejected):find(default_channel.key, 1, true))
    local oversized = config()
    oversized.template = string.rep("x", 5000)
    equal(env:command("set_forward_config", oversized).result, "error")
    local oversized_certificate = config()
    oversized_certificate.channels[1].ca_cert = string.rep("A", 4000)
    equal(env:command("set_forward_config", oversized_certificate).result, "error")
    local protocol_marker = config()
    protocol_marker.template = "{{content}}:CMD_END"
    equal(env:command("set_forward_config", protocol_marker).result, "error")
    equal(env.storage.direct_config_v1, nil)
end

function tests.main_configuration_readback_mismatch_does_not_activate_direct()
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, readback_mismatch = true})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    equal(env:command("set_forward_config", config()).result, "error")
    equal(env:command("get_forward_config").config.mode, "uart")
    env.events.SMS_INC("10086", "not activated without readback")
    env:resume_tasks(3)
    equal(#env.http_calls, 0)
    -- Rollback must prevent a later reboot from activating a failed save.
    equal(env.storage.direct_config_v1, nil)
end

function tests.main_generic_status_does_not_expose_saved_channel_keys()
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, storage = {direct_config_v1 = config()}})
    assert(env.loaded, env.load_error)
    env:resume_tasks(3)
    env.sandbox.process_uart_command({action = "get_status"})
    env:flush_uart()
    local status = env:last_frame("status_response")
    equal(status.forward_mode, "direct")
    equal(status.forward_config_persisted, true)
    assert(not json.encode(status):find(default_channel.key, 1, true))
end

function tests.main_invalid_saved_config_does_not_silently_revert_to_uart()
    local invalid = config()
    invalid.mode = "unexpected-mode"
    local env = load_main({mode = "uart", channels = {}}, nil,
        {use_real_direct = true, storage = {direct_config_v1 = invalid}})
    equal(env.loaded, false)
    assert(not tostring(env.load_error):find(default_channel.key, 1, true))
    equal(#env.http_calls, 0)
end

local names = {}
for name in pairs(tests) do names[#names + 1] = name end
table.sort(names)
local failed = 0
for _, name in ipairs(names) do
    local ok, err = pcall(tests[name])
    if ok then
        io.write("PASS ", name, "\n")
    else
        failed = failed + 1
        io.stderr:write("FAIL ", name, ": ", tostring(err), "\n")
    end
end
assert(failed == 0, tostring(failed) .. " Lua regression tests failed")
io.write(tostring(#names), " Lua regression tests passed\n")
