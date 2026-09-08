export function createRequestId(): string {
    try {
        if (typeof globalThis.crypto?.randomUUID === 'function') {
            return globalThis.crypto.randomUUID();
        }
    } catch {
        // 局域网 HTTP 可能不提供 randomUUID，下方仍生成标准 v4 形式。
    }

    const bytes = new Uint8Array(16);
    if (typeof globalThis.crypto?.getRandomValues === 'function') {
        globalThis.crypto.getRandomValues(bytes);
    } else {
        for (let index = 0; index < bytes.length; index += 1) {
            bytes[index] = Math.floor(Math.random() * 256);
        }
    }
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function isDefinitiveAPIRejection(error: unknown): boolean {
    if (!(error instanceof Error) || !error.message) return false;
    try {
        const response = JSON.parse(error.message) as {code?: unknown};
        return typeof response.code === 'string' && response.code.length > 0;
    } catch {
        // 网络中断、网关纯文本错误等无法证明服务端未受理。
        return false;
    }
}
