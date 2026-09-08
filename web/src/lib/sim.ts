import type {SimStatus} from '@/api/types';

export function simIdentityTail(simId: string, iccid = '') {
    const value = iccid || simId.replace(/^iccid:/i, '');
    return value ? value.slice(-6) : '';
}

export function formatSimLabel(sim?: SimStatus, fallbackSimId = '') {
    if (sim?.name?.trim()) return sim.name.trim();
    const number = sim?.currentStatus?.mobile?.number?.trim();
    if (number) return number;
    const tail = simIdentityTail(sim?.simId || fallbackSimId, sim?.iccid);
    return tail ? `SIM •${tail}` : '未知 SIM';
}

export function simAvailabilitySuffix(sim: SimStatus) {
    if (!isAssignableSim(sim)) return '（仅历史）';
    if (sim.conflict) return '（身份冲突）';
    if (!sim.online) return '（离线）';
    if (!sim.scriptCompatible) return '（请升级 main.lua）';
    if (!sim.sendReady) return '（SIM 未就绪）';
    return '';
}

export function isAssignableSim(sim: SimStatus) {
    return Boolean(sim.simId && !sim.simId.startsWith('legacy:'));
}

export function getErrorMessage(error: unknown, fallback: string) {
    if (!(error instanceof Error) || !error.message) return fallback;
    try {
        const parsed = JSON.parse(error.message) as {error?: unknown};
        return typeof parsed.error === 'string' ? parsed.error : fallback;
    } catch {
        return error.message;
    }
}
