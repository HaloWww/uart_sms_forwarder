import type {MobileInfo} from '@/api/types';

export function formatServingNetwork(mobile: MobileInfo | undefined, connected: boolean, flymode = false) {
    if (!connected) return '—';
    if (flymode) return '飞行模式';
    if (!mobile?.is_registered) return '未注册';

    const mcc = mobile.serving_mcc;
    const mnc = mobile.serving_mnc;
    const validMcc = typeof mcc === 'number' && Number.isInteger(mcc) && mcc > 0 && mcc <= 999;
    const validMnc = typeof mnc === 'number' && Number.isInteger(mnc) && mnc >= 0 && mnc <= 999;
    // The modem returns numeric MNC values without their original 2/3-digit width.
    const networkCode = validMcc && validMnc ? `MCC ${mcc} / MNC ${mnc}` : '';
    const operator = mobile.serving_operator?.trim();
    if (operator && networkCode) return `${operator}（${networkCode}）`;
    if (networkCode) return networkCode;
    if (operator) return operator;
    return mobile.serving_network_supported ? '暂未读取到网络' : '需 main.lua 1.6.1+ 及支持服务小区查询的底层固件';
}
