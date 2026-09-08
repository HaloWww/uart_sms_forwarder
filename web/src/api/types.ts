// 短信记录
export interface TextMessage {
    id: string;
    from: string;
    to: string;
    content: string;
    type: 'incoming' | 'outgoing';
    status: 'received' | 'sending' | 'sent' | 'failed' | 'ambiguous';
    submitted: boolean;
    ambiguous: boolean;
    sendError: string;
    timestamp: number;
    createdAt: number;
    updatedAt: number;
    simId: string;
    iccid: string;
    imsi: string;
    imei: string;
    // 设备和串口仅用于审计；短信归属始终由 simId 决定。
    deviceId: string;
    deviceName: string;
}

// 查询结果
export interface ListResult {
    total: number;
    items: TextMessage[];
}

// 统计信息
export interface Stats {
    totalCount: number;
    incomingCount: number;
    outgoingCount: number;
    todayCount: number;
}

// 发送短信请求
export interface SendSMSRequest {
	simId: string;
	to: string;
	content: string;
	requestId: string;
}

export interface SendSMSResponse {
	message: string;
	messageId: string;
	status?: 'ambiguous';
}

// 设置飞行模式请求
export interface SetFlymodeRequest {
    enabled: boolean;
}

// 移动网络信息（来自 Lua 脚本的 get_mobile_info 函数）
export interface MobileInfo {
    sim_ready: boolean;          // SIM卡是否就绪
    iccid: string;               // SIM卡 ICCID
    imsi: string;                // IMSI
    imei: string;                // 当前 Air780 模块 IMEI
    rssi: number;                // 信号强度 (dBm)
    signal_level: number;        // 信号等级 (0-31)
    signal_desc: string;         // 信号描述 (强/中/弱/无信号)
    is_registered: boolean;      // 是否已注册网络
    is_roaming: boolean;         // 是否漫游
    operator: string;            // 运营商英文简称
    csq: number;
    rsrp: number;
    rsrq: number;
    number: string;
    uptime: number;              // 开机时长 (毫秒)
}

// 设备状态响应（来自 Lua 脚本的 status_response）
export interface DeviceStatus {
    device_id: string;
    device_name: string;
    sim_id: string;
    imei: string;
    identity_valid: boolean;
    type: string;                // 消息类型: "status_response"
    timestamp: number;           // 时间戳
    mem_kb: number;              // 内存使用 (KB)
    flymode: boolean;            // 飞行模式是否启用
    mobile: MobileInfo;          // 移动网络信息
    port_name: string;           // 串口名称
    connected: boolean;          // 串口连接状态
    version: string;             // Lua 版本
}

// 持久化 SIM 档案及其当前承载设备。SIM 离线时 currentStatus 为空，
// 但档案仍会保留在列表中，供历史消息和计划任务继续使用。
export interface SimStatus {
    simId: string;
    name: string;
    iccid: string;
    imsi: string;
    number: string;
    online: boolean;
    sendReady: boolean;
    scriptCompatible: boolean;
    conflict: boolean;
    lastImei: string;
    lastDeviceId: string;
    lastDeviceName: string;
    lastPort: string;
    firstSeenAt: number;
    lastSeenAt: number;
    currentStatus?: DeviceStatus;
}

// 手机号码响应
export interface PhoneNumberResponse {
    type: string;
    timestamp: number;
    phone_number: string;
}

// 会话信息
export interface Conversation {
    simId: string;
    peer: string;              // 对方号码
    lastMessage: TextMessage;  // 最后一条消息
    messageCount: number;      // 消息总数
    unreadCount: number;       // 未读数量
}
