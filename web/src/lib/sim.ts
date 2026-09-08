import type {SimStatus} from '@/api/types';

export function simIdentityTail(simId: string, iccid = '') {
    const value = iccid || simId.replace(/^iccid:/i, '');
    return value ? value.slice(-6) : '';
}

export function formatSimLabel(sim?: SimStatus, fallbackSimId = '') {
    const tail = simIdentityTail(sim?.simId || fallbackSimId, sim?.iccid);
    const number = sim?.currentStatus?.mobile?.number?.trim() || sim?.number?.trim();
    if (number) return tail ? `${number} · SIM •${tail}` : number;
    if (sim?.name?.trim()) return sim.name.trim();
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
