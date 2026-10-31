import {useId, useState, type ReactNode} from 'react';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import {CircuitBoard, Loader2, Plus, RefreshCw, Save, Trash2} from 'lucide-react';
import {toast} from 'sonner';
import {getForwardingConfig, saveForwardingConfig} from '@/api/serial';
import type {DeviceStatus, ForwardingChannel, ForwardingChannelType, ForwardingConfig, ForwardingConfigResponse} from '@/api/types';
import {Button} from '@/components/ui/button';
import {Card, CardContent, CardHeader, CardTitle} from '@/components/ui/card';
import {Input} from '@/components/ui/input';
import {Select, SelectContent, SelectItem, SelectTrigger, SelectValue} from '@/components/ui/select';
import {Switch} from '@/components/ui/switch';
import {Textarea} from '@/components/ui/textarea';
import {getErrorMessage} from '@/lib/sim';

const CHANNEL_NAMES: Record<ForwardingChannelType, string> = {
    bark: 'Bark',
    wecom_app: '企业微信应用',
    webhook: 'Webhook',
};

function supportsBoardSettings(version = '') {
    const match = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(version);
    if (!match) return false;
    return Number(match[1]) > 1 || (Number(match[1]) === 1 && Number(match[2]) >= 6);
}

function defaultChannel(type: ForwardingChannelType, id: string = type): ForwardingChannel {
    if (type === 'bark') return {id, type, enabled: false, server: 'https://api.day.app', key: '', title: '短信通知', group: ''};
    if (type === 'wecom_app') return {id, type, enabled: false, corp_id: '', agent_id: 1000002, secret: '', to_user: '@all'};
    return {id, type, enabled: false, url: ''};
}

function normalizeConfig(config: ForwardingConfig): ForwardingConfig {
    return {
        mode: config.mode === 'direct' ? 'direct' : 'uart',
        apn: config.apn ?? '',
        queue_limit: config.queue_limit ?? 8,
        max_attempts: config.max_attempts ?? 3,
        retry_base_seconds: config.retry_base_seconds ?? 30,
        retry_max_seconds: config.retry_max_seconds ?? 900,
        min_interval_seconds: config.min_interval_seconds ?? 2,
        stats_save_seconds: config.stats_save_seconds ?? 60,
        timezone_offset_seconds: config.timezone_offset_seconds ?? 28800,
        template: config.template ?? '{{content}}\n接收号码：{{receiver}}\n时间：{{time}}',
        channels: (config.channels ?? []).map((channel) => ({...channel})),
    };
}

function usesHttps(value = '') {
    try {
        const url = new URL(value);
        return url.protocol === 'https:' && Boolean(url.hostname);
    } catch {
        return false;
    }
}

function validateConfig(config: ForwardingConfig): string | null {
    if (config.mode === 'direct' && !config.channels.some((channel) => channel.enabled)) {
        return '请至少启用一个板载通知渠道。';
    }
    for (const channel of config.channels) {
        if (!channel.enabled) continue;
        if (channel.type === 'bark' && (!usesHttps(channel.server) || !channel.key?.trim())) {
            return '请为已启用的 Bark 填写 HTTPS 服务地址和 Device Key。';
        }
        if (channel.type === 'wecom_app' && (!channel.corp_id?.trim() || !channel.secret?.trim() ||
            !Number.isInteger(channel.agent_id) || (channel.agent_id ?? 0) < 1 || !channel.to_user?.trim())) {
            return '请完整填写已启用的企业微信应用配置。';
        }
        if (channel.type === 'webhook' && !usesHttps(channel.url)) return 'Webhook 必须填写有效的 HTTPS 地址。';
    }
    if (new TextEncoder().encode(JSON.stringify(config)).length > 3800) {
        return '配置超过板载存储大小限制，请缩短模板或证书、减少渠道。';
    }
    return null;
}

function Field({id, label, children, hint}: {id: string; label: string; children: ReactNode; hint?: string}) {
    return <div className="min-w-0 space-y-1.5">
        <label htmlFor={id} className="text-xs font-semibold text-slate-700">{label}</label>
        {children}
        {hint && <p className="text-xs leading-5 text-slate-500">{hint}</p>}
    </div>;
}

function ForwardingEditor({simId, snapshot, ready, reading, onReload, onSaved}: {
    simId: string;
    snapshot: ForwardingConfigResponse;
    ready: boolean;
    reading: boolean;
    onReload: () => void;
    onSaved: (snapshot: ForwardingConfigResponse) => void;
}) {
    const fieldId = useId();
    const queryClient = useQueryClient();
    const [config, setConfig] = useState(() => normalizeConfig(snapshot.config));
    const [confirmed, setConfirmed] = useState(() => JSON.stringify(normalizeConfig(snapshot.config)));
    const [persisted, setPersisted] = useState(snapshot.persisted);
    const [newChannelType, setNewChannelType] = useState<ForwardingChannelType>('bark');
    const dirty = JSON.stringify(config) !== confirmed;
    const saveMutation = useMutation({
        mutationFn: async (submitted: ForwardingConfig) => {
            const validationError = validateConfig(submitted);
            if (validationError) throw new Error(validationError);
            const response = await saveForwardingConfig(simId, submitted);
            if (!response.persisted) throw new Error('板子没有确认持久化成功，请重新读取板子配置后核对。');
            return response;
        },
        onSuccess: async (response) => {
            const saved = normalizeConfig(response.config);
            setConfig(saved);
            setConfirmed(JSON.stringify(saved));
            setPersisted(true);
            onSaved(response);
            toast.success(saved.mode === 'direct' ? '已保存到板子，关闭 exe 后仍会独立转发' : '已保存到板子，使用主机转发模式');
            await queryClient.invalidateQueries({queryKey: ['sims']});
        },
    });
    const disabled = !ready || reading || saveMutation.isPending;
    const update = <K extends keyof ForwardingConfig>(key: K, value: ForwardingConfig[K]) =>
        setConfig((current) => ({...current, [key]: value}));
    const updateChannel = (index: number, values: Partial<ForwardingChannel>) =>
        setConfig((current) => ({...current, channels: current.channels.map((channel, i) => i === index ? {...channel, ...values} : channel)}));
    const input = (index: number, key: keyof ForwardingChannel, label: string, options: {secret?: boolean; number?: boolean; placeholder?: string} = {}) => {
        const id = `${fieldId}-${index}-${key}`;
        return <Field key={key} id={id} label={label}>
            <Input id={id} type={options.secret ? 'password' : options.number ? 'number' : 'text'}
                autoComplete={options.secret ? 'new-password' : 'off'} min={options.number ? 1 : undefined}
                value={String(config.channels[index][key] ?? '')} placeholder={options.placeholder}
                onChange={(event) => updateChannel(index, {[key]: options.number ? Number(event.target.value) : event.target.value})}/>
        </Field>;
    };
    return <div className="space-y-5">
        <div className={`rounded-lg px-3.5 py-3 text-xs leading-5 ${dirty ? 'bg-amber-50 text-amber-800' : persisted ? 'bg-emerald-50 text-emerald-800' : 'bg-slate-50 text-slate-600'}`} role="status">
            {dirty ? '有未保存修改。板子仍使用上次确认的设置。' : persisted ? '当前配置已保存在板子中，断电重新上电后会自动加载。' : '当前读取的是 Lua 脚本默认配置，尚未通过此页面保存到板子。'}
        </div>
        <fieldset disabled={disabled} className="min-w-0 space-y-5 disabled:opacity-60">
            <div className="grid gap-4 sm:grid-cols-2">
                <Field id={`${fieldId}-mode`} label="转发方式">
                    <Select value={config.mode} onValueChange={(value) => update('mode', value as ForwardingConfig['mode'])}>
                        <SelectTrigger id={`${fieldId}-mode`} className="w-full"><SelectValue/></SelectTrigger>
                        <SelectContent><SelectItem value="uart">主机转发</SelectItem><SelectItem value="direct">SIM 流量独立转发</SelectItem></SelectContent>
                    </Select>
                </Field>
                <Field id={`${fieldId}-apn`} label="SIM 数据 APN" hint="普通手机卡留空；专网卡按运营商要求填写。修改后如未联网，请重启模块。">
                    <Input id={`${fieldId}-apn`} value={config.apn} onChange={(event) => update('apn', event.target.value)} placeholder="使用运营商默认 APN"/>
                </Field>
            </div>
            <p className="text-xs leading-5 text-slate-500">独立模式使用下面的板载渠道；主机模式使用“通知渠道”页面的配置。两份设置互不覆盖。</p>
            <div className="space-y-3">
                {config.channels.map((channel, index) => <section key={`${channel.id}-${index}`} className="min-w-0 rounded-xl border border-slate-200 p-4">
                    <div className="mb-3 flex items-center justify-between gap-3">
                        <div className="flex items-center gap-2.5">
                            <Switch id={`${fieldId}-${index}-enabled`} checked={channel.enabled} onCheckedChange={(enabled) => updateChannel(index, {enabled})}/>
                            <label htmlFor={`${fieldId}-${index}-enabled`} className="text-sm font-semibold text-slate-800">{CHANNEL_NAMES[channel.type]}</label>
                        </div>
                        <Button type="button" variant="ghost" size="sm" aria-label={`移除${CHANNEL_NAMES[channel.type]}渠道`} onClick={() => update('channels', config.channels.filter((_, i) => i !== index))}>
                            <Trash2 className="size-4"/>
                        </Button>
                    </div>
                    {channel.enabled && <div className="grid gap-3 sm:grid-cols-2">
                        {channel.type === 'bark' && <>
                            {input(index, 'server', '服务地址', {placeholder: 'https://api.day.app'})}
                            {input(index, 'key', 'Device Key', {secret: true})}
                            {input(index, 'title', '推送标题')}
                            {input(index, 'group', '推送分组（可选）')}
                        </>}
                        {channel.type === 'wecom_app' && <>
                            {input(index, 'corp_id', 'CorpID（企业 ID）')}
                            {input(index, 'agent_id', 'AgentID（应用 ID）', {number: true})}
                            {input(index, 'secret', '应用 Secret', {secret: true})}
                            {input(index, 'to_user', '接收成员', {placeholder: '@all 或成员账号'})}
                            <p className="text-xs leading-5 text-slate-500 sm:col-span-2">企业微信可信 IP 限制仍适用。SIM 公网出口可能变化，直连被拒绝时需核对应用的可信 IP。</p>
                        </>}
                        {channel.type === 'webhook' && <div className="sm:col-span-2">{input(index, 'url', 'HTTPS 接收地址', {placeholder: 'https://example.com/sms'})}<p className="mt-1.5 text-xs leading-5 text-slate-500">发送精简 JSON，服务端返回 HTTP 2xx 表示成功，推荐返回无正文的 204。</p></div>}
                        <details className="sm:col-span-2">
                            <summary className="cursor-pointer text-xs text-slate-500">服务器 CA 证书（可选）</summary>
                            <div className="mt-2"><Field id={`${fieldId}-${index}-ca`} label="PEM 格式 CA 证书" hint="不填写时，不能仅凭 HTTPS 地址认定固件已校验证书链；证书计入板载配置大小。">
                                <Textarea id={`${fieldId}-${index}-ca`} value={channel.ca_cert ?? ''} onChange={(event) => updateChannel(index, {ca_cert: event.target.value})} rows={3} autoComplete="off"/>
                            </Field></div>
                        </details>
                    </div>}
                </section>)}
                {config.channels.length < 3 && <div className="flex flex-wrap gap-2">
                    <Select value={newChannelType} onValueChange={(value) => setNewChannelType(value as ForwardingChannelType)}>
                        <SelectTrigger className="w-44" aria-label="添加渠道类型"><SelectValue/></SelectTrigger>
                        <SelectContent>{Object.entries(CHANNEL_NAMES).map(([type, name]) => <SelectItem key={type} value={type}>{name}</SelectItem>)}</SelectContent>
                    </Select>
                    <Button type="button" variant="outline" onClick={() => {
                        let id: string = newChannelType;
                        for (let suffix = 2; config.channels.some((channel) => channel.id === id); suffix++) id = `${newChannelType}_${suffix}`;
                        update('channels', [...config.channels, defaultChannel(newChannelType, id)]);
                    }}><Plus className="size-4"/>添加渠道</Button>
                </div>}
            </div>
            <Field id={`${fieldId}-template`} label="板载转发内容" hint="支持 {{content}}、{{from}}、{{receiver}}、{{time}}、{{timestamp}}、{{iccid}}、{{imei}}、{{imsi}}；{{time}} 默认使用北京时间。">
                <Textarea id={`${fieldId}-template`} value={config.template} onChange={(event) => update('template', event.target.value)} rows={4}/>
            </Field>
            <details>
                <summary className="cursor-pointer text-xs font-medium text-slate-600">重试与流量控制</summary>
                <div className="mt-3 grid gap-3 sm:grid-cols-3">
                    <Field id={`${fieldId}-attempts`} label="每渠道最多尝试次数">
                        <Input id={`${fieldId}-attempts`} type="number" min={1} max={10} value={config.max_attempts} onChange={(event) => update('max_attempts', Number(event.target.value))}/>
                    </Field>
                    <Field id={`${fieldId}-retry`} label="首次重试等待（秒）">
                        <Input id={`${fieldId}-retry`} type="number" min={5} max={3600} value={config.retry_base_seconds} onChange={(event) => update('retry_base_seconds', Number(event.target.value))}/>
                    </Field>
                    <Field id={`${fieldId}-interval`} label="转发尝试间隔（秒）">
                        <Input id={`${fieldId}-interval`} type="number" min={1} max={3600} value={config.min_interval_seconds} onChange={(event) => update('min_interval_seconds', Number(event.target.value))}/>
                    </Field>
                </div>
                <p className="mt-2 text-xs leading-5 text-slate-500">失败时指数退避，断网或飞行模式期间不消耗尝试次数。只启用需要的渠道、保持简短模板可以减少流量。</p>
            </details>
        </fieldset>
        {saveMutation.isError && <p role="alert" className="rounded-lg bg-rose-50 px-3.5 py-3 text-xs leading-5 text-rose-700">{getErrorMessage(saveMutation.error, '保存失败，请重新读取板子配置后核对。')}</p>}
        <div className="flex flex-wrap gap-2 border-t border-slate-100 pt-4">
            <Button type="button" disabled={disabled} onClick={() => saveMutation.mutate(config)} className="bg-blue-600 text-white hover:bg-blue-700">
                {saveMutation.isPending ? <Loader2 className="size-4 animate-spin"/> : <Save className="size-4"/>}保存到板子
            </Button>
            <Button type="button" variant="outline" disabled={!ready || reading || saveMutation.isPending} onClick={onReload} title={dirty ? '重新读取会放弃未保存的修改' : '重新读取板子实际保存的配置'}>
                <RefreshCw className={`size-4 ${reading ? 'animate-spin' : ''}`}/>重新读取{dirty ? '（放弃修改）' : ''}
            </Button>
        </div>
    </div>;
}

export function DeviceForwardingSettings({simId, status, connected}: {simId?: string; status?: DeviceStatus; connected: boolean}) {
    const queryClient = useQueryClient();
    const compatible = supportsBoardSettings(status?.version);
    const ready = Boolean(simId && connected && compatible && status?.identity_valid && status.sim_id === simId && !status.flymode);
    const queryKey = ['boardForwardingConfig', simId, status?.device_id, status?.imei];
    const configQuery = useQuery({
        queryKey,
        queryFn: () => getForwardingConfig(simId!),
        enabled: ready,
        staleTime: 0,
        gcTime: 0,
        retry: false,
        refetchOnWindowFocus: false,
        refetchOnReconnect: false,
    });
    return <Card className="mt-4 gap-0 py-0">
        <CardHeader className="border-b border-slate-100 py-5">
            <CardTitle className="flex items-center gap-2 text-base"><CircuitBoard className="size-4 text-blue-600"/>板载独立转发</CardTitle>
            <p className="mt-1.5 text-xs leading-5 text-slate-500">保存到当前 Air780 板子的持久存储。独立模式保存成功后，关闭 exe 或板子重新上电仍可使用 SIM 流量转发。</p>
        </CardHeader>
        <CardContent className="py-5">
            {!ready ? <p className="text-sm leading-6 text-slate-500">
                {!connected ? '请连接 Air780 并选择在线 SIM。' : !compatible ? '请先将 Air780 的 main.lua 升级到 1.6.0 或更高版本（同时更新配套 Lua 文件），之后即可在此修改设置，无需每次重新烧录。' : status?.flymode ? '请先关闭飞行模式并等待 SIM 身份确认，再读取或保存板载配置。' : '正在确认当前 SIM 身份，确认后才能配置板子。'}
            </p> : configQuery.isPending ? <div className="flex items-center gap-2 text-sm text-slate-500"><Loader2 className="size-4 animate-spin"/>正在读取板子配置</div>
                : configQuery.isError ? <div className="space-y-3"><p role="alert" className="text-sm text-rose-700">{getErrorMessage(configQuery.error, '读取板子配置失败')}</p><Button variant="outline" onClick={() => void configQuery.refetch()} disabled={configQuery.isFetching}><RefreshCw className="size-4"/>重新读取</Button></div>
                    : configQuery.data && <ForwardingEditor key={configQuery.dataUpdatedAt} simId={simId!} snapshot={configQuery.data} ready={ready} reading={configQuery.isFetching} onReload={() => void configQuery.refetch()} onSaved={(saved) => queryClient.setQueryData(queryKey, saved)}/>}
            <p className="mt-4 text-xs leading-5 text-slate-400">设置属于当前物理板子，换板后需重新保存。首次配置需要连接 exe；后续独立转发仍需板子正常供电、SIM 短信与数据业务可用。</p>
        </CardContent>
    </Card>;
}
