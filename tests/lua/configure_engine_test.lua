-- Run with Lua 5.3. Hot SIM configuration must share one queue and traffic total.
package.path = "./?.lua;" .. package.path

local function copy(value)
    if type(value) ~= "table" then return value end
    local result = {}
    for key, item in pairs(value) do result[key] = copy(item) end
    return result
end

local fixtures = {}
local function encode(value)
    local kind = type(value)
    if kind == "nil" then return "null" end
    if kind == "number" or kind == "boolean" then return tostring(value) end
    if kind == "string" then return string.format("%q", value) end
    local values = {}
    if #value > 0 then
        for index, item in ipairs(value) do values[index] = encode(item) end
        return "[" .. table.concat(values, ",") .. "]"
    end
    local keys = {}
    for key in pairs(value) do keys[#keys + 1] = key end
    table.sort(keys)
    for _, key in ipairs(keys) do values[#values + 1] = encode(key) .. ":" .. encode(value[key]) end
    return "{" .. table.concat(values, ",") .. "}"
end
local json = {
    encode = function(value)
        local result = encode(value)
        fixtures[result] = copy(value)
        return result
    end,
    decode = function(value) return copy(fixtures[value]) end,
}
local Direct = require("direct_forward")
local function config(name)
    return {mode = "direct", template = name .. ":{{content}}", channels = {{id = "bark",
        enabled = true, type = "bark", server = "https://" .. name .. ".invalid", key = name .. "-key"}}}
end
local function message(name)
    return {type = "incoming_sms", message_id = name, iccid = "SIM-" .. name,
        identity_valid = true, content = name .. "-body", timestamp = 1700000000}
end
local function environment(storage)
    local env = {time = 100, tx = 0, rx = 0, allowed_sim = "SIM-A", storage = storage or {}, calls = {}}
    env.deps = {
        json = json, now = function() return env.time end,
        can_send = function(iccid) return env.allowed_sim == iccid end,
        socket = {LWIP_GP = 4, adapter = function(adapter) return adapter == 4 end},
        mobile = {dataTraffic = function() return 0, env.tx, 0, env.rx end},
        fskv = {
            init = function() return true end,
            get = function(key) return copy(env.storage[key]) end,
            set = function(key, value) env.storage[key] = copy(value); return true end,
            del = function(key) env.storage[key] = nil; return true end,
        },
        http = {request = function(method, url, _, body)
            env.calls[#env.calls + 1] = {method = method, url = url, body = json.decode(body)}
            env.tx, env.rx = env.tx + 100, env.rx + 200
            return {wait = function()
                if env.on_wait then
                    local callback = env.on_wait
                    env.on_wait = nil
                    callback()
                end
                return env.fail_http and 500 or 200, {}, json.encode({code = 200})
            end}
        end},
    }
    return env
end
local function equal(actual, expected)
    assert(actual == expected, "expected " .. tostring(expected) .. ", got " .. tostring(actual))
end

local tests = {}

function tests.uart_startup_restores_durable_queue_and_totals_without_http()
    local env = environment({direct_stats_v1 = {sent = 7, requests = 12, tx_bytes = 1000, rx_bytes = 2000}})
    local original = Direct.new(config("A"), env.deps)
    assert(original:enqueue(message("A")))
    original:flush()
    local engine = Direct.new({mode = "uart"}, env.deps)
    equal(engine:status().enabled, false)
    equal(engine:status().storage_available, true)
    equal(engine:status().queued, 1)
    equal(engine:status().sent, 7)
    equal(engine:status().total_bytes, 3000)
    engine:step()
    equal(#env.calls, 0)
    local queue = engine.queue
    assert(engine:configure(config("A")))
    equal(engine.queue, queue)
    engine:step()
    equal(engine:status().sent, 8)
    equal(engine:status().total_bytes, 3300)
    equal(engine:status().queued, 0)
end

function tests.new_sim_during_http_uses_its_own_frozen_route_and_template()
    local env = environment()
    local engine = Direct.new(config("A"), env.deps)
    local queue = engine.queue
    assert(engine:enqueue(message("A")))
    env.on_wait = function()
        assert(engine:enqueue(message("B"), config("B")))
        equal(engine.config.template, "A:{{content}}")
        equal(engine.channels.bark.key, "A-key")
    end
    engine:step()
    equal(engine.queue, queue)
    equal(engine:status().queued, 1)
    equal(engine.queue[1].message.iccid, "SIM-B")
    equal(engine.queue[1].rendered, "B:B-body")
    assert(engine.queue[1].slot ~= nil)
    env.allowed_sim, env.time = "SIM-B", 102
    assert(engine:configure(config("B")))
    engine:step()
    equal(#env.calls, 2)
    equal(env.calls[1].url, "https://A.invalid/push")
    equal(env.calls[1].body.body, "A:A-body")
    equal(env.calls[2].url, "https://B.invalid/push")
    equal(env.calls[2].body.device_key, "B-key")
    equal(env.calls[2].body.body, "B:B-body")
    equal(engine:status().sent, 2)
    equal(engine:status().requests, 2)
    equal(engine:status().total_bytes, 600)
    equal(engine:status().queued, 0)
end

function tests.explicit_direct_message_can_queue_while_active_mode_is_uart()
    local env = environment()
    env.allowed_sim = "SIM-B"
    local engine = Direct.new({mode = "uart"}, env.deps)
    assert(engine:enqueue(message("B"), config("B")))
    equal(engine:status().enabled, false)
    engine:step()
    equal(#env.calls, 0)
    local restored = Direct.new({mode = "uart"}, env.deps)
    equal(restored:status().queued, 1)
    assert(restored:configure(config("B")))
    restored:step()
    equal(env.calls[1].body.body, "B:B-body")
end

function tests.configure_clears_tokens_but_keeps_queue_and_counters()
    local env = environment()
    local engine = Direct.new(config("A"), env.deps)
    assert(engine:enqueue(message("A")))
    engine:step()
    local before = engine:status()
    engine.tokens.bark = {value = "old-secret"}
    assert(engine:configure({mode = "uart"}))
    equal(next(engine.tokens), nil)
    equal(engine:status().sent, before.sent)
    equal(engine:status().requests, before.requests)
    equal(engine:status().total_bytes, before.total_bytes)
    assert(engine:configure(config("B")))
    equal(engine.channels.bark.key, "B-key")
    equal(engine:status().requests, 1)
end

function tests.bad_configuration_disables_sending_without_losing_pending_messages()
    local env = environment()
    local engine = Direct.new(config("A"), env.deps)
    assert(engine:enqueue(message("A")))
    local ok, reason = engine:configure({mode = "direct", channels = {}})
    equal(ok, false)
    equal(reason, "no_enabled_channels")
    equal(engine:status().enabled, false)
    equal(next(engine.channels), nil)
    equal(engine:status().queued, 1)
    engine:step()
    equal(#env.calls, 0)
    assert(engine:configure(config("A")))
    engine:step()
    equal(engine:status().queued, 0)
end

function tests.missing_http_never_enables_direct_but_uart_remains_available()
    local env = environment()
    env.deps.http = nil
    local engine = Direct.new({mode = "uart"}, env.deps)
    local ok, reason = engine:configure(config("A"))
    equal(ok, false)
    equal(reason, "firmware_missing_http_json_or_cellular_adapter")
    equal(engine:status().enabled, false)
    assert(engine:configure({mode = "uart"}))
    engine:step()
    equal(#env.calls, 0)
end

function tests.event_queue_limit_and_active_retry_policy_are_updated()
    local env = environment()
    local engine = Direct.new(config("A"), env.deps)
    assert(engine:enqueue(message("A")))
    local restricted = config("B")
    restricted.queue_limit = 1
    equal(engine:enqueue(message("B"), restricted), false)
    equal(engine:status().dropped, 1)
    local no_retry = config("A")
    no_retry.max_attempts = 1
    assert(engine:configure(no_retry))
    env.fail_http = true
    engine:step()
    equal(engine:status().failed, 1)
    equal(engine:status().queued, 0)
    equal(#env.calls, 1)
end

local names = {}
for name in pairs(tests) do names[#names + 1] = name end
table.sort(names)
for _, name in ipairs(names) do tests[name](); io.write("PASS ", name, "\n") end
io.write(tostring(#names), " engine configuration tests passed\n")
