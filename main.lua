-- =================================================================================
-- PROJECT: UART SMS Forwarder
-- DEVICE:  Air780EHV
-- VERSION: 1.3.0
-- 协议说明：
--   上行（MCU -> 模块）：CMD_START:{json}:CMD_END
--   下行（模块 -> MCU）：SMS_START:{json}:SMS_END
-- =================================================================================

PROJECT = "uart_sms_forwarder"
VERSION = "1.3.0"

log.info("main", PROJECT, VERSION)

-- 1. 引入必要库
sys = require("sys")

-- 2. 全局配置与变量
-- [注意] 如果接单片机物理引脚，通常是 uart.UART_1；如果是USB调试，用 uart.VUART_0
local uartid = uart.VUART_0
local max_buffer_size = 50
local max_send_queue_size = 10
local max_uart_tx_queue_size = 100
local max_sms_result_cache_size = 50
local max_sms_content_bytes = 2048
local max_uart_recv_buffer_size = 16384
local msg_buffer = {}
local send_queue = {}
local uart_tx_queue = {}
local sms_pending_requests = {}
local sms_result_cache = {}
local sms_result_order = {}
local uart_recv_buffer = ""
local call_ring_count = 0  -- 来电响铃计数
local message_sequence = 0
-- v1.3 主机必定确认成功持久化的入站短信；默认保留直到 ACK，避免模块
-- 先启动或握手首包丢失时把短信从 RAM 队列直接移除。
local sms_ack_enabled = true
local sms_send_active = false
local flymode_requested = false
local flymode_owner_iccid = ""
local flymode_source = ""
local manual_restore_owner_iccid = ""
local identity_epoch = 0
local identity_revision = 0
local identity_refresh_requested = true
local identity_refresh_reason = "startup"
local identity_refresh_status = "STARTUP"
local identity_refresh_value = nil
local hardware_identity = {
    imei = "",
    muid = ""
}
local active_identity = {
    imei = "",
    muid = "",
    sim_slot = -1,
    sim_ready = false,
    identity_valid = false,
    identity_state = "initializing",
    identity_revision = 0,
    iccid = "",
    imsi = "",
    number = ""
}
local active_call_identity = nil

-- 保持项目原有配置：关闭 mobile.setAuto 提供的周期性辅助恢复功能。
mobile.setAuto(0)

-- 3. 看门狗
if wdt then
    wdt.init(9000)
    sys.timerLoopStart(wdt.feed, 3000)
end

uart.setup(uartid, 115200, 8, 1)
log.info("System", "UART 初始化成功")

-- =================================================================================
-- 工具函数区
-- =================================================================================

function send_to_uart(data, priority)
    local prioritize = priority == true or
        (type(data) == "table" and
         (data.type == "cmd_response" or data.type == "sms_send_result"))
    local ok, json_str = pcall(json.encode, data)
    if ok and json_str then
        if #uart_tx_queue >= max_uart_tx_queue_size then
            log.error("UART", "发送队列已满，无法加入帧")
            return false, nil
        end
        local item = {
            frame = "SMS_START:" .. json_str .. ":SMS_END\r\n",
            sent = false,
            failed = false
        }
        if prioritize then
            table.insert(uart_tx_queue, 1, item)
        else
            table.insert(uart_tx_queue, item)
        end
        sys.publish("UART_TX_QUEUE_CHANGED")
        return true, item
    else
        log.error("UART", "JSON Encode Failed", json_str)
        return false, nil
    end
end

local function safe_mobile_call(func, ...)
    if type(func) ~= "function" then
        return nil
    end
    local ok, value = pcall(func, ...)
    if not ok then
        log.warn("Identity", "读取移动网络身份失败", value)
        return nil
    end
    return value
end

local function normalize_identifier(value)
    if type(value) ~= "string" then
        return ""
    end
    local normalized = value:gsub("^%s+", ""):gsub("%s+$", "")
    local lower = normalized:lower()
    if lower == "unknown" or lower == "nil" or lower == "null" then
        return ""
    end
    return normalized
end

local function copy_identity(source)
    local value = source or active_identity
    return {
        imei = value.imei or "",
        muid = value.muid or "",
        sim_slot = value.sim_slot or -1,
        sim_ready = value.sim_ready == true,
        identity_valid = value.identity_valid == true,
        identity_state = value.identity_state or "unavailable",
        identity_revision = value.identity_revision or 0,
        iccid = value.iccid or "",
        imsi = value.imsi or "",
        number = value.number or ""
    }
end

local function attach_identity(data, source)
    local identity = copy_identity(source)
    -- 保留扁平字段兼容已有上位机，同时提供完整、不可混搭的身份快照。
    data.imei = identity.imei
    data.muid = identity.muid
    data.sim_slot = identity.sim_slot
    data.sim_ready = identity.sim_ready
    data.identity_valid = identity.identity_valid
    data.identity_state = identity.identity_state
    data.identity_revision = identity.identity_revision
    data.iccid = identity.iccid
    data.imsi = identity.imsi
    data.number = identity.number
    data.identity = copy_identity(identity)
    return data
end

local function refresh_hardware_identity()
    if hardware_identity.imei == "" then
        local imei = normalize_identifier(safe_mobile_call(mobile.imei))
        if imei ~= "" then
            hardware_identity.imei = imei
        end
    end
    if hardware_identity.muid == "" then
        local muid = normalize_identifier(safe_mobile_call(mobile.muid))
        if muid ~= "" then
            hardware_identity.muid = muid
        end
    end
end

local function identity_fields_equal(left, right)
    -- revision 只表示会影响 SIM 路由安全的身份代际。号码、硬件审计字段
    -- 或展示状态晚到不应使已经核验的短信作废。
    return left.sim_slot == right.sim_slot and
           left.sim_ready == right.sim_ready and
           left.identity_valid == right.identity_valid and
           left.iccid == right.iccid and
           left.imsi == right.imsi
end

local function commit_identity(candidate)
    local changed = not identity_fields_equal(active_identity, candidate)
    if changed then
        identity_revision = identity_revision + 1
    end
    candidate.identity_revision = identity_revision
    active_identity = copy_identity(candidate)
    return changed, copy_identity(active_identity)
end

local function unavailable_identity(state, preserve_last_known)
    refresh_hardware_identity()
    local candidate = preserve_last_known and copy_identity(active_identity) or {
        sim_slot = -1,
        iccid = "",
        imsi = "",
        number = ""
    }
    candidate.imei = hardware_identity.imei
    candidate.muid = hardware_identity.muid
    candidate.sim_ready = false
    candidate.identity_valid = false
    candidate.identity_state = state or "unavailable"
    return candidate
end

-- 对同一活动卡槽前后各读取一次关键字段，防止换卡过程中拼出混合身份。
local function read_identity_candidate()
    refresh_hardware_identity()
    local slot_before = safe_mobile_call(mobile.simid)
    if slot_before == -1 then
        return nil, "no_active_slot"
    end
    if type(slot_before) ~= "number" or (slot_before ~= 0 and slot_before ~= 1) then
        -- API 异常/未知值不能伪装成“确认无卡”，否则会过早放开全局路由。
        return nil, "sim_slot_unavailable"
    end
    if safe_mobile_call(mobile.simPin, slot_before) ~= true then
        return nil, "not_ready"
    end

    local iccid = normalize_identifier(safe_mobile_call(mobile.iccid, slot_before))
    local imsi = normalize_identifier(safe_mobile_call(mobile.imsi, slot_before))
    local number = normalize_identifier(safe_mobile_call(mobile.number, slot_before))
    local slot_after = safe_mobile_call(mobile.simid)
    local ready_after = safe_mobile_call(mobile.simPin, slot_before)
    local iccid_after = normalize_identifier(safe_mobile_call(mobile.iccid, slot_before))
    local imsi_after = normalize_identifier(safe_mobile_call(mobile.imsi, slot_before))

    if slot_after ~= slot_before or ready_after ~= true or
       iccid_after ~= iccid or imsi_after ~= imsi then
        return nil, "identity_changing"
    end
    if iccid == "" then
        return nil, "missing_iccid"
    end
    return {
        imei = hardware_identity.imei,
        muid = hardware_identity.muid,
        sim_slot = slot_before,
        sim_ready = true,
        identity_valid = true,
        identity_state = "verified",
        identity_revision = identity_revision,
        iccid = iccid,
        imsi = imsi,
        number = number
    }, nil
end

local function stable_identity_equal(left, right)
    return left ~= nil and right ~= nil and
           left.imei == right.imei and
           left.muid == right.muid and
           left.sim_slot == right.sim_slot and
           left.iccid == right.iccid and
           left.imsi == right.imsi
end

-- 只能从 LuatOS task 调用；要求两次连续读取一致才认可该身份。
local function read_stable_identity(expected_epoch, max_attempts)
    local previous = nil
    local last_error = "identity_unavailable"
    for attempt = 1, (max_attempts or 10) do
        if identity_epoch ~= expected_epoch then
            return nil, "identity_changed"
        end
        local candidate, read_error = read_identity_candidate()
        if candidate then
            if stable_identity_equal(previous, candidate) then
                return candidate, nil
            end
            previous = candidate
        else
            previous = nil
            last_error = read_error or last_error
        end
        if attempt < (max_attempts or 10) then
            sys.wait(300)
        end
    end
    return nil, last_error
end

local function publish_identity_event(status, value, reason, snapshot)
    local event = {
        type = "sim_event",
        status = status or "IDENTITY",
        value = value,
        reason = reason,
        timestamp = os.time()
    }
    attach_identity(event, snapshot)
    send_to_uart(event)
end

local function request_identity_refresh(reason, status, value)
    identity_refresh_requested = true
    identity_refresh_reason = reason or "refresh"
    identity_refresh_status = status or "REFRESH"
    identity_refresh_value = value
    sys.publish("SIM_IDENTITY_REFRESH")
end

local function resolve_and_commit_identity(reason, status, value, max_attempts)
    local expected_epoch = identity_epoch
    local candidate, read_error = read_stable_identity(expected_epoch, max_attempts)
    if candidate and identity_epoch == expected_epoch then
        local changed, snapshot = commit_identity(candidate)
        if changed then
            publish_identity_event(status or "RDY", value, reason, snapshot)
        end
        return true, snapshot, nil
    end

    if identity_epoch == expected_epoch and not flymode_requested then
        local changed, snapshot = commit_identity(unavailable_identity(read_error, false))
        if changed then
            publish_identity_event(status or "UNAVAILABLE", value, reason, snapshot)
        end
        return false, snapshot, read_error
    end
    return false, copy_identity(active_identity), read_error or "identity_changed"
end

-- SMS_INC/来电事件都不会告诉脚本它来自哪个卡槽/ICCID。因此不能直接
-- 信任可能滞后的 active_identity：事件到达时同步复读当前卡，
-- 只有缓存身份和两次现场快照完全一致才允许归属；其余情况一律未归属并
-- 立即撤销旧路由，避免换卡事件延迟/漏报时把新卡短信写到旧卡名下。
local function capture_incoming_identity()
    local expected_epoch = identity_epoch
    local cached = copy_identity(active_identity)
    local first, first_error = read_identity_candidate()
    local second, second_error = read_identity_candidate()
    local verified =
        identity_epoch == expected_epoch and
        cached.identity_valid and
        first ~= nil and second ~= nil and
        stable_identity_equal(first, second) and
        stable_identity_equal(cached, second)
    if verified then
        return cached, nil
    end

    local reason = first_error or second_error or "incoming_identity_changed"
    local snapshot
    if cached.identity_valid then
        -- 已暴露过的旧路由必须当场失效；否则身份刷新 task 运行前，主机仍
        -- 可能从 status_response 看到旧 ICCID。
        identity_epoch = identity_epoch + 1
        local _
        _, snapshot = commit_identity(unavailable_identity(reason, false))
        publish_identity_event("UNAVAILABLE", nil, "incoming_sms_identity_check", snapshot)
    else
        snapshot = unavailable_identity(reason, false)
    end
    request_identity_refresh("incoming_sms_identity_check", "REFRESH", nil)
    return snapshot, reason
end

local function get_mobile_info()
    local info = copy_identity(active_identity)
    -- 使用 status 判断：0=未注册 1=已注册 2=搜索中 3=拒绝 5=漫游注册
    local net_stat = mobile.status()

    -- 获取信号强度指标
    local csq = mobile.csq() or 0 -- 范围 0-31，越大越好
    info.csq = csq
    info.rssi = mobile.rssi() or -113  -- 范围 0到-114，值越大越好
    info.rsrp = mobile.rsrp() or -140  -- 范围 -44到-140，值越大越好 (4G模块)
    info.rsrq = mobile.rsrq() or -20   -- 范围 -3到-19.5，值越大越好 (4G模块)

    -- 根据 CSQ 判断信号等级（仅供参考，4G模块应参考rsrp/rsrq）
    if csq == 0 or csq == 99 then
        info.signal_level = 0
        info.signal_desc = "无信号"
    else
        info.signal_level = csq
        info.signal_desc = csq >= 20 and "强" or (csq >= 10 and "中" or "弱")
    end

    info.is_registered = (net_stat == 1 or net_stat == 5)
    info.is_roaming = net_stat == 5
    info.uptime = mcu.ticks2() -- 单位为秒

    -- mobile.flymode() 在部分固件查询不准确，优先使用本脚本记录的请求状态。
    info.flymode = flymode_requested
    info.identity = copy_identity(active_identity)
    return info
end

local function validate_expected_iccid(expected_iccid, snapshot)
    local expected = normalize_identifier(expected_iccid)
    local identity = snapshot or copy_identity(active_identity)
    if expected == "" then
        return false, identity, "expected_iccid_required"
    end
    if not identity.identity_valid or identity.iccid == "" then
        return false, identity, "sim_identity_unavailable"
    end
    if identity.iccid ~= expected then
        return false, identity, "sim_identity_mismatch"
    end
    return true, identity, nil
end

-- 物理控制动作执行前直接读取当前卡，避免命令排队期间发生换卡后仍作用于旧路由。
local function verify_control_identity(expected_iccid)
    local expected = normalize_identifier(expected_iccid)
    if expected == "" then
        return false, copy_identity(active_identity), "expected_iccid_required"
    end
    local candidate, read_error = read_identity_candidate()
    if not candidate then
        return false, copy_identity(active_identity), read_error or "sim_identity_unavailable"
    end
    if candidate.iccid ~= expected then
        return false, candidate, "sim_identity_mismatch"
    end
    local _, snapshot = commit_identity(candidate)
    return true, snapshot, nil
end

local function send_sms_result(item, success, error_code, snapshot, submitted, ambiguous)
    local result = {
        type = "sms_send_result",
        success = success == true,
        submitted = submitted == true,
        ambiguous = ambiguous == true,
        request_id = tostring(item.request_id),
        to = item.to,
        expected_iccid = normalize_identifier(item.expected_iccid),
        accepted_identity_revision = item.accepted_identity_revision,
        error = error_code,
        timestamp = os.time()
    }
    attach_identity(result, snapshot)
    result.actual_iccid = result.iccid
    local request_id = result.request_id
    sms_pending_requests[request_id] = nil
    if sms_result_cache[request_id] == nil then
        table.insert(sms_result_order, request_id)
    end
    sms_result_cache[request_id] = result
    while #sms_result_order > max_sms_result_cache_size do
        local expired_id = table.remove(sms_result_order, 1)
        sms_result_cache[expired_id] = nil
    end
    send_to_uart(result, true)
end

local function next_message_id()
    message_sequence = message_sequence + 1
    refresh_hardware_identity()
    local module_id = hardware_identity.imei ~= "" and hardware_identity.imei or hardware_identity.muid
    if module_id == "" then
        module_id = "module"
    end
    return string.format(
        "%s-%d-%d-%d",
        module_id,
        math.floor(os.time() or 0),
        math.floor(mcu.ticks2() or 0),
        message_sequence
    )
end

function process_uart_command(cmd_data)
    if not cmd_data.action then
        send_to_uart({type = "error", msg = "missing action"})
        return
    end

    if cmd_data.action == "send_sms" and
       type(cmd_data.to) == "string" and type(cmd_data.content) == "string" then
        local item = {
            request_id = tostring(cmd_data.request_id or next_message_id()),
            to = cmd_data.to,
            content = cmd_data.content,
            expected_iccid = normalize_identifier(cmd_data.expected_iccid)
        }
        local cached_result = sms_result_cache[item.request_id]
        if cached_result then
            -- 主机可能因 HTTP/UART 响应丢失重发同一 request_id；返回原结果，
            -- 绝不再次调用 sms.sendLong。
            send_to_uart(cached_result)
            return
        elseif sms_pending_requests[item.request_id] then
            -- 原请求仍在队列或 sendLong 中，最终结果会携带同一 request_id 返回。
            return
        end
        sms_pending_requests[item.request_id] = true
        local identity_ok, identity, identity_error =
            validate_expected_iccid(item.expected_iccid, copy_identity(active_identity))
        if #item.content > max_sms_content_bytes then
            send_sms_result(item, false, "sms_content_too_long", identity, false, false)
        elseif not identity_ok then
            if identity_error == "sim_identity_unavailable" then
                request_identity_refresh("send_enqueue", "REFRESH", nil)
            end
            send_sms_result(item, false, identity_error, identity, false, false)
        elseif #send_queue >= max_send_queue_size then
            send_sms_result(item, false, "send_queue_full", identity, false, false)
        else
            item.accepted_identity_revision = identity.identity_revision
            item.accepted_identity_epoch = identity_epoch
            table.insert(send_queue, item)
            sys.publish("SMS_SEND_QUEUE_CHANGED")
        end

    elseif cmd_data.action == "get_status" then
        if not flymode_requested then
            request_identity_refresh("status_query", "REFRESH", nil)
        end
        local status = {
            type = "status_response",
            timestamp = os.time(),
            mem_kb = math.floor(collectgarbage("count")),
            version = VERSION,
            flymode_owner_iccid = flymode_requested and flymode_owner_iccid or "",
            flymode_source = flymode_requested and flymode_source or "",
            manual_restore_owner_iccid = manual_restore_owner_iccid,
            mobile = get_mobile_info()
        }
        attach_identity(status, active_identity)
        send_to_uart(status)

    elseif cmd_data.action == "ack_sms" and cmd_data.message_id then
        sys.publish("SMS_ACK_" .. tostring(cmd_data.message_id))

    elseif cmd_data.action == "enable_sms_ack" then
        sms_ack_enabled = true
        send_to_uart({type = "cmd_response", action = "enable_sms_ack", result = "ok"}, true)

    elseif cmd_data.action == "set_flymode" and cmd_data.enabled ~= nil then
        -- 规范化为布尔值：兼容 true/false、1/0、"true"/"false"
        -- Lua 中 0 也是真值，必须显式转换
        local flymode_enabled = (cmd_data.enabled == true or cmd_data.enabled == 1 or
                                 cmd_data.enabled == "true" or cmd_data.enabled == "1")

        -- 设备端也做互斥：即使主机在检查后崩溃或有其他控制端，
        -- 也不能用飞行模式中断已排队/正在提交的短信。
        if sms_send_active or #send_queue > 0 then
            local response = {
                type = "cmd_response",
                action = "set_flymode",
                result = "error",
                enabled = flymode_enabled,
                request_id = cmd_data.request_id,
                error = "sms_operation_pending"
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        end

        local requested_owner_iccid = ""
        local requested_source = cmd_data.source == "automatic" and "automatic" or "manual"
        local expected_iccid = normalize_identifier(cmd_data.expected_iccid)
        if flymode_enabled then
            -- 飞行模式中 ICCID 通常不可读。同一 owner 的 automatic/manual
            -- 转移只更新策略归属，但仍必须与进入飞行模式时冻结的 ICCID 一致。
            if flymode_requested then
                if expected_iccid == "" then
                    local response = {
                        type = "cmd_response",
                        action = "set_flymode",
                        result = "error",
                        enabled = true,
                        request_id = cmd_data.request_id,
                        error = "expected_iccid_required"
                    }
                    attach_identity(response, active_identity)
                    send_to_uart(response)
                    return
                elseif flymode_owner_iccid == "" or expected_iccid ~= flymode_owner_iccid then
                    local response = {
                        type = "cmd_response",
                        action = "set_flymode",
                        result = "error",
                        enabled = true,
                        request_id = cmd_data.request_id,
                        expected_iccid = expected_iccid,
                        error = "sim_identity_mismatch",
                        flymode_owner_iccid = flymode_owner_iccid
                    }
                    attach_identity(response, active_identity)
                    send_to_uart(response)
                    return
                end
                flymode_source = requested_source
                manual_restore_owner_iccid = ""
                local response = {
                    type = "cmd_response",
                    action = "set_flymode",
                    result = "ok",
                    enabled = true,
                    request_id = cmd_data.request_id,
                    flymode_owner_iccid = flymode_owner_iccid,
                    flymode_source = flymode_source
                }
                attach_identity(response, active_identity)
                send_to_uart(response)
                return
            end

            local identity_ok, identity, identity_error =
                verify_control_identity(expected_iccid)
            if not identity_ok then
                local response = {
                    type = "cmd_response",
                    action = "set_flymode",
                    result = "error",
                    enabled = flymode_enabled,
                    error = identity_error,
                    request_id = cmd_data.request_id,
                    expected_iccid = expected_iccid
                }
                attach_identity(response, identity)
                send_to_uart(response)
                return
            end
            requested_owner_iccid = identity.iccid
        elseif cmd_data.preserve_manual_owner == true and expected_iccid == "" then
            local response = {
                type = "cmd_response",
                action = "set_flymode",
                result = "error",
                enabled = false,
                request_id = cmd_data.request_id,
                error = "expected_iccid_required"
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        elseif flymode_requested and expected_iccid ~= "" and
               (flymode_owner_iccid == "" or expected_iccid ~= flymode_owner_iccid) then
            local response = {
                type = "cmd_response",
                action = "set_flymode",
                result = "error",
                enabled = false,
                request_id = cmd_data.request_id,
                expected_iccid = expected_iccid,
                error = "sim_identity_mismatch",
                flymode_owner_iccid = flymode_owner_iccid
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        end

        -- 设置飞行模式（0 表示 sim0）
        -- enabled = true 表示启用飞行模式（禁用蜂窝网络）
        -- enabled = false 表示禁用飞行模式（启用蜂窝网络）
        local control_ok, control_error = pcall(function()
            return mobile.flymode(0, flymode_enabled)
        end)
        -- mobile.flymode 的返回值是“切换前状态”，不是成功标志；这里只能
        -- 用 pcall 是否抛错判断调用本身是否完成。
        if not control_ok then
            local response = {
                type = "cmd_response",
                action = "set_flymode",
                result = "error",
                enabled = flymode_enabled,
                request_id = cmd_data.request_id,
                error = tostring(control_error)
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        end

        local previous_owner_iccid = flymode_owner_iccid
        local preserve_manual_owner =
            not flymode_enabled and cmd_data.preserve_manual_owner == true and
            previous_owner_iccid ~= "" and expected_iccid == previous_owner_iccid
        flymode_requested = flymode_enabled
        flymode_owner_iccid = flymode_enabled and requested_owner_iccid or ""
        flymode_source = flymode_enabled and requested_source or ""
        if flymode_enabled then
            manual_restore_owner_iccid = ""
        elseif preserve_manual_owner then
            manual_restore_owner_iccid = previous_owner_iccid
        else
            manual_restore_owner_iccid = ""
        end
        identity_epoch = identity_epoch + 1

        if not flymode_enabled then
            mobile.setAuto(0) -- 保持项目原有配置：关闭周期性辅助恢复
            local _, snapshot = commit_identity(unavailable_identity("identifying", true))
            request_identity_refresh("flymode_disabled", "REFRESH", nil)
            publish_identity_event("REFRESH", nil, "flymode_disabled", snapshot)
        else
            local _, snapshot = commit_identity(unavailable_identity("stale_flymode", true))
            publish_identity_event("STALE", nil, "flymode_enabled", snapshot)
        end

        local response = {
            type = "cmd_response",
            action = "set_flymode",
            result = "ok",
            enabled = flymode_enabled,
            request_id = cmd_data.request_id,
            flymode_owner_iccid = flymode_requested and flymode_owner_iccid or "",
            flymode_source = flymode_requested and flymode_source or "",
            manual_restore_owner_iccid = manual_restore_owner_iccid
        }
        attach_identity(response, active_identity)
        send_to_uart(response)

    elseif cmd_data.action == "reset_stack" then
        log.info("CMD", "重启协议栈")
        if sms_send_active or #send_queue > 0 then
            local response = {
                type = "cmd_response",
                action = "reset_stack",
                result = "error",
                request_id = cmd_data.request_id,
                error = "sms_operation_pending"
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        end
        identity_epoch = identity_epoch + 1
        local _, snapshot = commit_identity(unavailable_identity("resetting", true))
        mobile.reset()
        mobile.setAuto(0)
        request_identity_refresh("stack_reset", "REFRESH", nil)
        local response = {type = "cmd_response", action = "reset_stack", result = "ok"}
        attach_identity(response, snapshot)
        send_to_uart(response)

    elseif cmd_data.action == "reboot_mcu" then
        log.info("CMD", "重启模块")
        if sms_send_active or #send_queue > 0 then
            local response = {
                type = "cmd_response",
                action = "reboot_mcu",
                result = "error",
                request_id = cmd_data.request_id,
                error = "sms_operation_pending"
            }
            attach_identity(response, active_identity)
            send_to_uart(response)
            return
        end
        local expected_iccid = normalize_identifier(cmd_data.expected_iccid)
        if expected_iccid ~= "" then
            local identity_ok, identity, identity_error = verify_control_identity(expected_iccid)
            if not identity_ok then
                local response = {
                    type = "cmd_response",
                    action = "reboot_mcu",
                    result = "error",
                    request_id = cmd_data.request_id,
                    error = identity_error,
                    expected_iccid = expected_iccid
                }
                attach_identity(response, identity)
                send_to_uart(response)
                return
            end
        end
        local response = {
            type = "cmd_response",
            action = "reboot_mcu",
            result = "ok",
            request_id = cmd_data.request_id
        }
        attach_identity(response, active_identity)
        local queued, tx_item = send_to_uart(response, true)
        -- 等待相关响应真正写完再重启，避免主机收到半帧后误判。
        sys.taskInit(function()
            if queued and tx_item then
                for _ = 1, 200 do
                    if tx_item.sent or tx_item.failed then
                        break
                    end
                    sys.wait(10)
                end
            end
            pm.reboot()
        end)
    else
        send_to_uart({type = "error", msg = "unknown command"})
    end
end

-- =================================================================================
-- 事件监听区
-- =================================================================================

sys.subscribe("SMS_INC", function(phone, content)
    log.info("Event", "收到短信:", phone)
    local snapshot, identity_error = capture_incoming_identity()
    local msg = {
        type = "incoming_sms",
        message_id = next_message_id(),
        timestamp = os.time(),
        from = phone,
        content = content,
        identity_error = identity_error
    }
    attach_identity(msg, snapshot)
    table.insert(msg_buffer, msg)
    if #msg_buffer > max_buffer_size then
        table.remove(msg_buffer, 1) -- 移除旧的
    end
    sys.publish("NEW_MSG_IN_BUFFER")
end)

sys.subscribe("SIM_IND", function(status, value)
    if status == "RDY" or status == "NORDY" or status == "SIM_PIN" then
        -- 任何可能代表换卡的状态都会使正在进行的身份核对失效。
        identity_epoch = identity_epoch + 1
    end

    if status == "RDY" then
        local _, snapshot = commit_identity(unavailable_identity("identifying", false))
        publish_identity_event(status, value, "sim_ind", snapshot)
        request_identity_refresh("sim_ready", status, value)
    elseif status == "NORDY" then
        local _, snapshot = commit_identity(unavailable_identity("no_sim", false))
        publish_identity_event(status, value, "sim_ind", snapshot)
    elseif status == "SIM_PIN" then
        local _, snapshot = commit_identity(unavailable_identity("pin_required", false))
        publish_identity_event(status, value, "sim_ind", snapshot)
    elseif status == "GET_NUMBER" then
        publish_identity_event(status, value, "sim_ind", active_identity)
        request_identity_refresh("number_updated", status, value)
    else
        publish_identity_event(status, value, "sim_ind", active_identity)
    end
end)

-- 新版固件在短信子系统可用时发送该事件；同时触发一次身份复核。
sys.subscribe("SMS_READY", function(slot)
    request_identity_refresh("sms_ready", "SMS_READY", slot)
end)

-- 来电事件处理
sys.subscribe("CC_IND", function(state)
    if state == "READY" then
        log.info("Call", "通话准备完成")

    elseif state == "INCOMINGCALL" then
        -- 有电话呼入
        if call_ring_count == 0 then
            log.info("Call", "检测到来电")
            local phone_num = cc.lastNum()
            log.info("Call", "来电号码:", phone_num or "unknown")

            -- 与入站短信一样现场复核，换卡事件延迟时不使用旧缓存标注来电。
            local call_identity, identity_error = capture_incoming_identity()
            active_call_identity = call_identity
            local call = {
                type = "incoming_call",
                timestamp = os.time(),
                from = phone_num or "unknown",
                identity_error = identity_error
            }
            attach_identity(call, active_call_identity)
            send_to_uart(call)
        end

        call_ring_count = call_ring_count + 1

        -- 响4声后自动挂断（可根据需求调整）
--         if call_ring_count > 3 then
--             log.info("Call", "自动挂断来电")
--             cc.hangUp()
--         end

    elseif state == "DISCONNECTED" then
        -- 电话被挂断
        log.info("Call", "通话结束")
        call_ring_count = 0
        local call = {
            type = "call_disconnected",
            timestamp = os.time()
        }
        attach_identity(call, active_call_identity or active_identity)
        send_to_uart(call)
        active_call_identity = nil
    end
end)

-- =================================================================================
-- 任务循环区
-- =================================================================================

uart.on(uartid, "receive", function(id, len)
    local chunk = uart.read(id, len)
    if not chunk then return end

    uart_recv_buffer = uart_recv_buffer .. chunk

    -- 使用与下行一致的包围标志：CMD_START:{json}:CMD_END
    while true do
        local start_pos = uart_recv_buffer:find("CMD_START:", 1, true)
        if not start_pos then break end

        local end_pos = uart_recv_buffer:find(":CMD_END", start_pos + 10, true)
        if not end_pos then break end  -- 数据未接收完整，等待下次

        -- 提取 JSON 部分
        local json_str = uart_recv_buffer:sub(start_pos + 10, end_pos - 1)
        -- 移除已处理的数据
        uart_recv_buffer = uart_recv_buffer:sub(end_pos + 8)

        if #json_str > 0 then
            local success, cmd = pcall(json.decode, json_str)
            if success and cmd then
                process_uart_command(cmd)
            else
                log.warn("UART", "JSON解析失败:", json_str:sub(1, 50))
                send_to_uart({type="error", msg="Invalid JSON"})
            end
        end
    end

    -- 溢出保护：如果缓冲区过大且找不到有效包，清空
    if #uart_recv_buffer > max_uart_recv_buffer_size then
        log.error("UART", "Buffer Overflow - 清空缓冲区")
        uart_recv_buffer = ""
        send_to_uart({type="error", msg="Buffer overflow, cleared"})
    end
end)

-- 所有 UART 下行共用一个发送任务，并按 uart.write 的实际写入长度补齐
-- 短写。这样回调和多个 task 不会交叉写出两个残缺帧。
sys.taskInit(function()
    while true do
        if #uart_tx_queue == 0 then
            sys.waitUntil("UART_TX_QUEUE_CHANGED")
        end
        while #uart_tx_queue > 0 do
            local item = table.remove(uart_tx_queue, 1)
            local remaining = item.frame
            local failed_attempts = 0
            while #remaining > 0 and failed_attempts < 100 do
                local write_ok, written = pcall(uart.write, uartid, remaining)
                if write_ok and written == true then
                    written = #remaining
                end
                if write_ok and type(written) == "number" and written > 0 then
                    if written > #remaining then
                        written = #remaining
                    end
                    remaining = remaining:sub(written + 1)
                    failed_attempts = 0
                else
                    failed_attempts = failed_attempts + 1
                    sys.wait(20)
                end
            end
            if #remaining == 0 then
                item.sent = true
            else
                item.failed = true
                log.error("UART", "连续写入失败，已丢弃下行帧")
            end
        end
    end
end)

-- 串行维护经过稳定性确认的 SIM 身份。事件到达刷新期间也不会丢失后续刷新请求。
sys.taskInit(function()
    while true do
        if not identity_refresh_requested then
            sys.waitUntil("SIM_IDENTITY_REFRESH")
        end
        while identity_refresh_requested do
            local reason = identity_refresh_reason
            local status = identity_refresh_status
            local value = identity_refresh_value
            identity_refresh_requested = false
            if not flymode_requested then
                resolve_and_commit_identity(reason, status, value, 10)
            end
        end
    end
end)

-- 串行发送短信，避免多个 sms.sendLong 同时操作 modem。
sys.taskInit(function()
    while true do
        if #send_queue == 0 then
            sys.waitUntil("SMS_SEND_QUEUE_CHANGED")
        end
        while #send_queue > 0 do
            local item = table.remove(send_queue, 1)
            sms_send_active = true
            log.info("CMD", "发送短信 ->", item.to)
            local resolved, identity, resolve_error =
                resolve_and_commit_identity("send_preflight", "VERIFY", nil, 10)
            local identity_ok, _, identity_error =
                validate_expected_iccid(item.expected_iccid, identity)
            local admission_unchanged =
                item.accepted_identity_revision == identity.identity_revision and
                item.accepted_identity_epoch == identity_epoch
            if not resolved or not identity_ok or not admission_unchanged then
                local error_code = identity_error or resolve_error
                if resolved and identity_ok and not admission_unchanged then
                    error_code = "sim_identity_changed_before_send"
                end
                error_code = error_code or "sim_identity_unavailable"
                log.error("CMD", "SIM身份核对失败，拒绝发送", item.expected_iccid, identity.iccid)
                send_sms_result(item, false, error_code, identity, false, false)
            else
                local preflight_identity_revision = identity.identity_revision
                local preflight_identity_epoch = identity_epoch
                local invocation_ok, send_result = pcall(function()
                    return sms.sendLong(item.to, item.content).wait()
                end)
                local submitted = invocation_ok and send_result == true

                -- sms.sendLong 运行期间会让出执行权，换卡事件可能在此期间发生，必须再核对。
                local post_resolved, post_identity, post_resolve_error =
                    resolve_and_commit_identity("send_postflight", "VERIFY", nil, 10)
                local post_matches, _, post_identity_error =
                    validate_expected_iccid(item.expected_iccid, post_identity)
                local identity_generation_unchanged =
                    post_identity.identity_revision == preflight_identity_revision and
                    identity_epoch == preflight_identity_epoch
                local verified_after_send = post_resolved and post_matches and identity_generation_unchanged
                local error_code = nil
                local ambiguous = false

                if not invocation_ok then
                    error_code = "sms_send_exception"
                    ambiguous = true
                    log.error("CMD", "短信发送接口异常", send_result)
                elseif submitted and not verified_after_send then
                    error_code = post_identity_error or post_resolve_error
                    if not identity_generation_unchanged or error_code == "sim_identity_mismatch" then
                        error_code = "sim_identity_changed_after_send"
                    end
                    error_code = error_code or "sim_identity_changed_after_send"
                    ambiguous = true
                    log.error("CMD", "短信已提交但发送后SIM身份无法确认", item.expected_iccid, post_identity.iccid)
                elseif not submitted then
                    -- sendLong 的长短信可能已分片；false 只表示整体没有
                    -- 成功，不能证明前面的分片从未提交。保守标记为不确定，防止重试重复发送。
                    error_code = "sms_send_failed_after_invocation"
                    ambiguous = true
                    if not verified_after_send then
                        error_code = post_identity_error or post_resolve_error or error_code
                    end
                end

                send_sms_result(
                    item,
                    submitted and verified_after_send,
                    error_code,
                    post_identity,
                    submitted,
                    ambiguous
                )
            end
            sms_send_active = false
        end
    end
end)

sys.taskInit(function()
    while true do
        if #msg_buffer == 0 then
            sys.waitUntil("NEW_MSG_IN_BUFFER")
        end
        while #msg_buffer > 0 do
            local msg = msg_buffer[1]
            if msg then
                -- 身份在 SMS_INC 发生时即被冻结；若当时无法确认则保持未归属，
                -- 不用稍后可能属于另一张卡的身份补写。
                local queued = send_to_uart(msg)
                if not queued then
                    -- UART 队列满时不丢消息，保留原队首稍后重试。
                    sys.wait(1000)
                elseif not sms_ack_enabled then
                    table.remove(msg_buffer, 1)
                else
                    local acknowledged = sys.waitUntil("SMS_ACK_" .. msg.message_id, 5000)
                    if acknowledged then
                        table.remove(msg_buffer, 1)
                    else
                        -- 主机可能暂时离线，保留消息并稍后重试。
                        table.remove(msg_buffer, 1)
                        table.insert(msg_buffer, msg)
                        log.warn("UART", "短信未收到主机确认，将重试", msg.message_id)
                        sys.wait(1000)
                    end
                end
            end
        end
        if collectgarbage("count") > 1024 then
            collectgarbage("collect")
        end
    end
end)

sys.taskInit(function()
    sys.wait(5000)
    local ready = {
        type = "system_ready",
        project = PROJECT,
        version = VERSION,
        data_disabled = true
    }
    attach_identity(ready, active_identity)
    send_to_uart(ready)
    while true do
        sys.wait(60000)
        local info = get_mobile_info()
        local heartbeat = {
            type = "heartbeat",
            timestamp = os.time(),
            rssi = info.rssi,
            signal_level = info.signal_level,
            signal_desc = info.signal_desc,
            net_reg = info.is_registered,
            flymode = info.flymode,
            sim_ready = info.sim_ready,
            memory_usage = math.floor(collectgarbage("count")),
            buffer_size = #msg_buffer
        }
        attach_identity(heartbeat, active_identity)
        send_to_uart(heartbeat)
        if not flymode_requested then
            request_identity_refresh("heartbeat", "REFRESH", nil)
        end
    end
end)

sys.run()
