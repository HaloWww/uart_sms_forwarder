import {useEffect, useRef, useState} from 'react';
import {Loader2, MoreVertical, Plus, RefreshCw, Search, Send, Trash2, User} from 'lucide-react';
import {useSearchParams} from 'react-router-dom';
import {toast} from 'sonner';
import {clearMessages, getConversations, getConversationMessages, deleteConversation, deleteMessage} from '../api/messages';
import {sendSMS} from '../api/serial';
import {Input} from '@/components/ui/input';
import {Button} from '@/components/ui/button';
import {Textarea} from '@/components/ui/textarea';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
} from '@/components/ui/dialog';
import {
    DropdownMenu,
    DropdownMenuContent,
    DropdownMenuItem,
    DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import {useMutation, useQuery, useQueryClient} from '@tanstack/react-query';
import type {Conversation, TextMessage} from '@/api/types';
import {PageHeader} from '@/components/PageHeader';
import {useSim} from '@/providers/SimContext';
import {formatSimLabel, getErrorMessage, isAssignableSim} from '@/lib/sim';
import {createRequestId, isDefinitiveAPIRejection} from '@/lib/request-id';

interface SendSMSVariables {
    simId: string;
    to: string;
    content: string;
    requestId: string;
    source: 'conversation' | 'compose';
}

interface DraftRequest {
    fingerprint: string;
    requestId: string;
}

interface ConversationDraft {
    content: string;
    request?: DraftRequest;
}

interface ComposeDraft {
    recipient: string;
    content: string;
    request?: DraftRequest;
}

interface SMSDraftStore {
    version: 1;
    conversations: Record<string, ConversationDraft>;
    compose: Record<string, ComposeDraft>;
}

const SMS_DRAFT_STORAGE_KEY = 'uart-sms-forwarder:sms-drafts:v1';

function emptyDraftStore(): SMSDraftStore {
    return {version: 1, conversations: {}, compose: {}};
}

function validDraftRequest(value: unknown): DraftRequest | undefined {
    if (!value || typeof value !== 'object') return undefined;
    const candidate = value as Partial<DraftRequest>;
    if (typeof candidate.fingerprint !== 'string' || typeof candidate.requestId !== 'string') return undefined;
    return {fingerprint: candidate.fingerprint, requestId: candidate.requestId};
}

function loadDraftStore(): SMSDraftStore {
    if (typeof window === 'undefined') return emptyDraftStore();
    try {
        const raw = window.localStorage.getItem(SMS_DRAFT_STORAGE_KEY);
        if (!raw) return emptyDraftStore();
        const parsed = JSON.parse(raw) as Partial<SMSDraftStore>;
        if (parsed.version !== 1) return emptyDraftStore();

        const conversations: Record<string, ConversationDraft> = {};
        if (parsed.conversations && typeof parsed.conversations === 'object') {
            for (const [key, value] of Object.entries(parsed.conversations)) {
                if (!value || typeof value !== 'object') continue;
                const candidate = value as Partial<ConversationDraft>;
                if (typeof candidate.content !== 'string') continue;
                conversations[key] = {
                    content: candidate.content,
                    request: validDraftRequest(candidate.request),
                };
            }
        }

        const compose: Record<string, ComposeDraft> = {};
        if (parsed.compose && typeof parsed.compose === 'object') {
            for (const [key, value] of Object.entries(parsed.compose)) {
                if (!value || typeof value !== 'object') continue;
                const candidate = value as Partial<ComposeDraft>;
                if (typeof candidate.recipient !== 'string' || typeof candidate.content !== 'string') continue;
                compose[key] = {
                    recipient: candidate.recipient,
                    content: candidate.content,
                    request: validDraftRequest(candidate.request),
                };
            }
        }
        return {version: 1, conversations, compose};
    } catch {
        return emptyDraftStore();
    }
}

function saveDraftStore(store: SMSDraftStore): void {
    if (typeof window === 'undefined') return;
    try {
        window.localStorage.setItem(SMS_DRAFT_STORAGE_KEY, JSON.stringify(store));
    } catch {
        // 隐私模式或存储配额不足时仍允许本次页面内发送。
    }
}

function conversationDraftKey(simId: string, peer: string): string {
    return JSON.stringify([simId, peer]);
}

function requestIdForDraft(current: DraftRequest | null, fingerprint: string): DraftRequest {
    if (current?.fingerprint === fingerprint) return current;
    return {fingerprint, requestId: createRequestId()};
}

export default function Messages() {
    const {selectedSimId, selectedSim} = useSim();
    const queryClient = useQueryClient();
    const messagesEndRef = useRef<HTMLDivElement>(null);
    const [searchParams, setSearchParams] = useSearchParams();
    const [draftStore, setDraftStore] = useState<SMSDraftStore>(loadDraftStore);
    const draftStoreRef = useRef(draftStore);

    const updateDraftStore = (update: (current: SMSDraftStore) => SMSDraftStore) => {
        const next = update(draftStoreRef.current);
        draftStoreRef.current = next;
        // 同步持久化，确保发起网络请求后即使立刻刷新页面也不会丢失 requestId。
        saveDraftStore(next);
        setDraftStore(next);
    };

    // 每张 SIM 独立保存当前联系人，切卡不会把另一张卡的会话上下文带过来。
    const [selectedPeers, setSelectedPeers] = useState<Record<string, string>>({});
    const selectedPeer = selectedSimId ? selectedPeers[selectedSimId] ?? null : null;
    const setSelectedPeer = (peer: string | null, simId = selectedSimId) => {
        if (!simId) return;
        setSelectedPeers((current) => {
            if (peer) return {...current, [simId]: peer};
            if (!(simId in current)) return current;
            const next = {...current};
            delete next[simId];
            return next;
        });
    };
    const activeConversationDraftKey = selectedSimId && selectedPeer
        ? conversationDraftKey(selectedSimId, selectedPeer)
        : '';
    const activeConversationDraft = activeConversationDraftKey
        ? draftStore.conversations[activeConversationDraftKey]
        : undefined;
    const inputText = activeConversationDraft?.content ?? '';
    const setInputText = (content: string) => {
        if (!activeConversationDraftKey) return;
        updateDraftStore((current) => {
            const conversations = {...current.conversations};
            if (!content) {
                delete conversations[activeConversationDraftKey];
            } else {
                conversations[activeConversationDraftKey] = {content};
            }
            return {...current, conversations};
        });
    };
    // 搜索关键词
    const [searchQuery, setSearchQuery] = useState('');
    const [composeOpen, setComposeOpen] = useState(searchParams.get('compose') === '1');
    const activeComposeDraft = selectedSimId ? draftStore.compose[selectedSimId] : undefined;
    const newRecipient = activeComposeDraft?.recipient ?? '';
    const newContent = activeComposeDraft?.content ?? '';
    const updateComposeDraft = (patch: Partial<Pick<ComposeDraft, 'recipient' | 'content'>>) => {
        if (!selectedSimId) return;
        const simId = selectedSimId;
        updateDraftStore((current) => {
            const compose = {...current.compose};
            const previous = current.compose[simId] ?? {recipient: '', content: ''};
            const recipient = patch.recipient ?? previous.recipient;
            const content = patch.content ?? previous.content;
            if (!recipient && !content) {
                delete compose[simId];
            } else {
                const fingerprint = JSON.stringify([simId, recipient.trim(), content.trim()]);
                compose[simId] = {
                    recipient,
                    content,
                    request: previous.request?.fingerprint === fingerprint ? previous.request : undefined,
                };
            }
            return {...current, compose};
        });
    };

    // 草稿及对应 requestId 按 SIM/会话持久化。页面刷新、切卡或切换联系人后，
    // 对同一份未确认请求重试时仍使用原 ID，不会再次写入设备。

    // 根据手机号生成头像颜色
    const getAvatarColor = (phoneNumber: string) => {
        const colors = [
            'bg-blue-600',
            'bg-blue-700',
            'bg-blue-800',
            'bg-blue-700',
            'bg-blue-500',
            'bg-blue-700',
            'bg-slate-600',
            'bg-blue-500',
        ];
        // 使用手机号的数字总和来选择颜色
        const sum = phoneNumber.split('').reduce((acc, char) => acc + char.charCodeAt(0), 0);
        return colors[sum % colors.length];
    };

    // 使用新的会话列表 API
    const {data: conversations = [], isLoading, refetch} = useQuery<Conversation[]>({
        queryKey: ['conversations', selectedSimId],
        queryFn: () => getConversations(selectedSimId),
        enabled: Boolean(selectedSimId),
        refetchInterval: 5000, // 每 5 秒自动刷新
    });

    // 获取指定会话的所有消息
    const {data: currentMessages = []} = useQuery<TextMessage[]>({
        queryKey: ['conversation-messages', selectedSimId, selectedPeer],
        queryFn: () => {
            if (!selectedPeer) return Promise.resolve([]);
            return getConversationMessages(selectedPeer, selectedSimId);
        },
        enabled: Boolean(selectedSimId && selectedPeer),
        refetchInterval: 5000,
    });

    // 发送短信 Mutation
    const sendSMSMutation = useMutation({
		mutationFn: (variables: SendSMSVariables) => sendSMS({
			simId: variables.simId,
			to: variables.to,
			content: variables.content,
			requestId: variables.requestId,
		}),
		onSuccess: (result, variables) => {
            // 请求携带点击发送时的 simId；发送途中切卡时不跳入另一张卡的同名会话。
            setSelectedPeer(variables.to, variables.simId);
			if (result.status === 'ambiguous') {
                // 状态不确定时保留草稿和 requestId；如用户再次提交，
                // 服务端只会回放原结果，不会再写一次 UART。
                toast.warning(result.message);
            } else {
                if (variables.source === 'conversation') {
                    const key = conversationDraftKey(variables.simId, variables.to);
                    updateDraftStore((current) => {
                        const draft = current.conversations[key];
                        if (draft?.request?.requestId !== variables.requestId) return current;
                        const conversations = {...current.conversations};
                        delete conversations[key];
                        return {...current, conversations};
                    });
                } else {
                    updateDraftStore((current) => {
                        const draft = current.compose[variables.simId];
                        if (draft?.request?.requestId !== variables.requestId) return current;
                        const compose = {...current.compose};
                        delete compose[variables.simId];
                        return {...current, compose};
                    });
                    setComposeOpen(false);
                    const nextParams = new URLSearchParams(searchParams);
                    nextParams.delete('compose');
                    setSearchParams(nextParams, {replace: true});
                }
                toast.success('短信已提交发送');
            }
            queryClient.invalidateQueries({queryKey: ['conversations', variables.simId]});
            queryClient.invalidateQueries({queryKey: ['conversation-messages', variables.simId, variables.to]});
        },
		onError: (error, variables) => {
            console.error('发送失败:', error);
			// 服务端明确拒绝表示本次没有进入不确定发送；下次点击使用新 ID。
			// 网络错误则保留 ID，防止响应丢失后再次写入 UART。
			if (isDefinitiveAPIRejection(error)) {
				updateDraftStore((current) => {
					if (variables.source === 'conversation') {
						const key = conversationDraftKey(variables.simId, variables.to);
						const draft = current.conversations[key];
						if (draft?.request?.requestId !== variables.requestId) return current;
						return {
							...current,
							conversations: {...current.conversations, [key]: {content: draft.content}},
						};
					}
					const draft = current.compose[variables.simId];
					if (draft?.request?.requestId !== variables.requestId) return current;
					return {
						...current,
						compose: {
							...current.compose,
							[variables.simId]: {recipient: draft.recipient, content: draft.content},
						},
					};
				});
			}
            toast.error(getErrorMessage(error, '发送失败'));
        },
    });

    // 清空所有短信
    const clearMutation = useMutation({
        mutationFn: (simId: string) => clearMessages(simId),
        onSuccess: (_, simId) => {
            toast.success('清空成功');
            setSelectedPeer(null, simId);
            queryClient.invalidateQueries({queryKey: ['conversations', simId]});
            queryClient.invalidateQueries({queryKey: ['conversation-messages', simId]});
        },
        onError: (error) => {
            console.error('清空失败:', error);
            toast.error(getErrorMessage(error, '清空失败'));
        },
    });

    // 删除整个会话
    const deleteConversationMutation = useMutation({
        mutationFn: ({peer, simId}: {peer: string; simId: string}) => deleteConversation(peer, simId),
        onSuccess: (_, {peer, simId}) => {
            toast.success('会话已删除');
            // 如果删除的是当前选中的会话，清除选中状态
            setSelectedPeers((current) => {
                if (current[simId] !== peer) return current;
                const next = {...current};
                delete next[simId];
                return next;
            });
            queryClient.invalidateQueries({queryKey: ['conversations', simId]});
        },
        onError: (error) => {
            console.error('删除失败:', error);
            toast.error(getErrorMessage(error, '删除会话失败'));
        },
    });

    // 删除单条消息
    const deleteMessageMutation = useMutation({
        mutationFn: ({messageId, simId}: {messageId: string; simId: string}) => deleteMessage(messageId, simId),
        onSuccess: (_, {simId}) => {
            toast.success('消息已删除');
            queryClient.invalidateQueries({queryKey: ['conversations', simId]});
            queryClient.invalidateQueries({queryKey: ['conversation-messages', simId]});
        },
        onError: (error) => {
            console.error('删除失败:', error);
            toast.error(getErrorMessage(error, '删除消息失败'));
        },
    });

    // 自动滚动到底部
    useEffect(() => {
        messagesEndRef.current?.scrollIntoView({behavior: "smooth"});
    }, [selectedPeer, currentMessages]);

    // 获取当前选中的会话信息
    const activeConversation = conversations.find(c => c.peer === selectedPeer);

    // 过滤会话列表
    const filteredConversations = conversations.filter(conv =>
        conv.peer.toLowerCase().includes(searchQuery.toLowerCase()) ||
        conv.lastMessage.content.toLowerCase().includes(searchQuery.toLowerCase())
    );

    const handleSendSMS = (e: React.FormEvent) => {
        e.preventDefault();
        if (!selectedPeer || !inputText.trim()) {
            toast.warning('请输入短信内容');
            return;
        }
        if (!selectedSimId || !canSend) {
            toast.warning('当前 SIM 不可发送，请等待其上线并完成识别');
            return;
        }
        const fingerprint = JSON.stringify([selectedSimId, selectedPeer, inputText]);
        const key = conversationDraftKey(selectedSimId, selectedPeer);
        const request = requestIdForDraft(draftStoreRef.current.conversations[key]?.request ?? null, fingerprint);
        updateDraftStore((current) => ({
            ...current,
            conversations: {...current.conversations, [key]: {content: inputText, request}},
        }));
        sendSMSMutation.mutate({
            simId: selectedSimId,
            to: selectedPeer,
            content: inputText,
            requestId: request.requestId,
            source: 'conversation',
        });
    };

    const handleSendNewSMS = (event: React.FormEvent) => {
        event.preventDefault();
        const recipient = newRecipient.trim();
        const message = newContent.trim();
        if (!recipient || !message) {
            toast.warning('请输入目标手机号和短信内容');
            return;
        }
        if (!selectedSimId || !canSend) {
            toast.warning('当前 SIM 不可发送，请等待其上线并完成识别');
            return;
        }
        const fingerprint = JSON.stringify([selectedSimId, recipient, message]);
        const request = requestIdForDraft(draftStoreRef.current.compose[selectedSimId]?.request ?? null, fingerprint);
        updateDraftStore((current) => ({
            ...current,
            compose: {...current.compose, [selectedSimId]: {recipient: newRecipient, content: newContent, request}},
        }));
        sendSMSMutation.mutate({
            simId: selectedSimId,
            to: recipient,
            content: message,
            requestId: request.requestId,
            source: 'compose',
        });
    };

    const handleComposeOpenChange = (open: boolean) => {
        setComposeOpen(open);
        if (!open) {
            const nextParams = new URLSearchParams(searchParams);
            nextParams.delete('compose');
            setSearchParams(nextParams, {replace: true});
        }
    };

    const handleClear = () => {
        if (!selectedSimId) return;
        if (!confirm(`确定要清空 ${selectedSimLabel} 的所有短信吗？此操作不可恢复！`)) return;
        clearMutation.mutate(selectedSimId);
    };

    const handleDeleteConversation = () => {
        if (!selectedPeer) return;
        if (!confirm(`确定要删除与 ${selectedPeer} 的所有消息吗？此操作不可恢复！`)) return;
        if (!selectedSimId) return;
        deleteConversationMutation.mutate({peer: selectedPeer, simId: selectedSimId});
    };

    const handleDeleteMessage = (messageId: string, e: React.MouseEvent) => {
        e.stopPropagation();
        if (!confirm('确定要删除这条消息吗？此操作不可恢复！')) return;
        if (!selectedSimId) return;
        deleteMessageMutation.mutate({messageId, simId: selectedSimId});
    };

    const formatTime = (timestamp: number) => {
        const date = new Date(timestamp);
        const now = new Date();
        const diff = now.getTime() - date.getTime();
        const oneDay = 24 * 60 * 60 * 1000;

        // 今天
        if (diff < oneDay && date.getDate() === now.getDate()) {
            return date.toLocaleTimeString('zh-CN', {hour: '2-digit', minute: '2-digit'});
        }
        // 昨天
        if (diff < 2 * oneDay && date.getDate() === now.getDate() - 1) {
            return '昨天 ' + date.toLocaleTimeString('zh-CN', {hour: '2-digit', minute: '2-digit'});
        }
        // 更早
        return date.toLocaleDateString('zh-CN', {month: '2-digit', day: '2-digit'}) + ' ' +
            date.toLocaleTimeString('zh-CN', {hour: '2-digit', minute: '2-digit'});
    };

    const getStatusBadge = (status: string) => {
        switch (status) {
            case 'sent':
                return <span className="text-[10px] text-green-600">✓ 已发送</span>;
            case 'failed':
                return <span className="text-[10px] text-red-600">✗ 失败</span>;
            case 'ambiguous':
                return <span className="text-[10px] font-medium text-amber-600" title="设备可能已经提交，请先核实，勿立即重试">⚠ 状态不确定</span>;
            case 'sending':
                return <span className="text-[10px] text-gray-400">发送中...</span>;
            default:
                return null;
        }
    };

    const selectedSimLabel = formatSimLabel(selectedSim, selectedSimId);
    const canSend = Boolean(
        selectedSimId && selectedSim && isAssignableSim(selectedSim) && selectedSim.online &&
        selectedSim.scriptCompatible && selectedSim.sendReady && !selectedSim.conflict,
    );
    const unavailableReason = selectedSim && !isAssignableSim(selectedSim)
        ? '“未分配短信”仅供查看和管理，不能用于发送'
        : selectedSim?.conflict
        ? '当前 SIM 身份冲突，已禁止发送'
        : selectedSim?.online && !selectedSim.scriptCompatible
            ? '当前 Air780 脚本版本不兼容，请升级 main.lua'
        : selectedSimId ? '当前 SIM 离线或尚未完成识别' : '请先选择 SIM';

    if (selectedSimId && isLoading) {
        return (
            <div className="flex min-h-[560px] h-[calc(100dvh-108px)] items-center justify-center">
                <div className="animate-spin rounded-full h-12 w-12 border-b-2 border-blue-600"></div>
            </div>
        );
    }

    return (
        <div className="flex h-[calc(100dvh-108px)] min-h-[560px] flex-col">
            {/* 顶部操作栏 */}
            <PageHeader
                title="短信中心"
                description="查看短信会话、搜索历史记录，或向任意号码发送新短信。"
                action={<div className="flex gap-2">
                    <Button
                        onClick={() => handleComposeOpenChange(true)}
                        disabled={!canSend}
                        title={canSend ? '新建短信' : unavailableReason}
                        size="sm"
                        className="bg-blue-600 text-white hover:bg-blue-700"
                    >
                        <Plus className="mr-2 size-4"/>
                        新建短信
                    </Button>
                    <Button
                        onClick={() => refetch()}
                        disabled={!selectedSimId}
                        variant="outline"
                        size="sm"
                        className="hover:bg-gray-50"
                    >
                        <RefreshCw className="w-4 h-4 mr-2"/>
                        刷新
                    </Button>
                    <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                            <Button variant="outline" size="sm" className="hover:bg-gray-50">
                                <MoreVertical className="w-4 h-4 mr-2"/>
                                更多
                            </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                            <DropdownMenuItem
                                onClick={handleClear}
                                disabled={!selectedSimId || clearMutation.isPending}
                                className="cursor-pointer text-rose-600 focus:bg-rose-50 focus:text-rose-700"
                            >
                                <Trash2 className="mr-2 size-4"/>
                                清空所有短信
                            </DropdownMenuItem>
                        </DropdownMenuContent>
                    </DropdownMenu>
                </div>}
            />

            {/* 聊天界面 */}
            <div
                className="mt-6 flex min-h-0 flex-1 overflow-hidden rounded-2xl border border-slate-200 bg-white">
                {/* 左侧：会话列表 */}
                <div className={`${
                    selectedPeer ? 'hidden md:flex' : 'flex'
                } w-full flex-col border-r border-gray-200 bg-white md:w-[300px] xl:w-[330px]`}>
                    {/* 搜索框 */}
                    <div className="p-4 border-b border-gray-100">
                        <div className="relative">
                            <Search className="absolute left-3 top-2.5 w-4 h-4 text-gray-400"/>
                            <Input
                                type="text"
                                placeholder="搜索联系人或内容..."
                                value={searchQuery}
                                onChange={(e) => setSearchQuery(e.target.value)}
                                className="pl-9 pr-4 h-9 bg-gray-50 border-transparent focus:bg-white focus:border-blue-500"
                            />
                        </div>
                    </div>

                    {/* 会话列表 */}
                    <div className="flex-1 overflow-y-auto">
                        {filteredConversations.length === 0 ? (
                            <div className="flex flex-col items-center justify-center h-full text-gray-400">
                                <User className="w-12 h-12 mb-2 opacity-30"/>
                                <p className="text-sm">{selectedSimId ? '暂无会话' : '请先选择 SIM'}</p>
                            </div>
                        ) : (
                            filteredConversations.map(conv => (
                                <div
                                    key={conv.peer}
                                    onClick={() => setSelectedPeer(conv.peer)}
                                    className={`p-4 cursor-pointer transition-all border-l-2 hover:bg-gray-50 ${
                                        selectedPeer === conv.peer
                                            ? 'bg-blue-50/50 border-blue-500'
                                            : 'border-transparent'
                                    }`}
                                >
                                    <div className="flex items-start justify-between mb-1">
                                        <div className="flex items-center space-x-2">
                                            <div
                                                className={`flex h-9 w-9 items-center justify-center rounded-full text-sm font-bold text-white ${getAvatarColor(conv.peer)}`}>
                                                {conv.peer.slice(-2)}
                                            </div>
                                            <span className={`text-sm font-semibold ${
                                                selectedPeer === conv.peer ? 'text-gray-900' : 'text-gray-700'
                                            }`}>
                                                {conv.peer}
                                            </span>
                                        </div>
                                        <span className="text-xs text-gray-400">
                                            {formatTime(conv.lastMessage.createdAt)}
                                        </span>
                                    </div>
                                    <p className="text-xs text-gray-500 line-clamp-2 ml-11">
                                        {conv.lastMessage.type === 'outgoing' && '我: '}
                                        {conv.lastMessage.content}
                                    </p>
                                </div>
                            ))
                        )}
                    </div>
                </div>

                {/* 右侧：聊天区域 */}
                <div className={`${
                    selectedPeer ? 'flex' : 'hidden md:flex'
                } flex-1 flex-col bg-gray-50/30`}>
                    {/* 聊天头部 */}
                    <div
                        className="flex h-15 shrink-0 items-center justify-between border-b border-gray-200 bg-white px-4 md:px-6">
                        {selectedPeer ? (
                            <>
                                <div className="flex items-center space-x-3">
                                    {/* 移动端返回按钮 */}
                                    <Button
                                        variant="ghost"
                                        size="sm"
                                        onClick={() => setSelectedPeer(null)}
                                        className="md:hidden -ml-2 text-gray-600"
                                    >
                                        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2}
                                                  d="M15 19l-7-7 7-7"/>
                                        </svg>
                                    </Button>
                                    <div
                                        className={`flex h-10 w-10 items-center justify-center rounded-full font-bold text-white ${getAvatarColor(selectedPeer)}`}>
                                        {selectedPeer.slice(-2)}
                                    </div>
                                    <div>
                                        <h3 className="text-sm font-bold text-gray-900">{selectedPeer}</h3>
                                        <span className="text-xs text-gray-500">
                                            共 {activeConversation?.messageCount || 0} 条消息
                                        </span>
                                    </div>
                                </div>
                                <DropdownMenu>
                                    <DropdownMenuTrigger asChild>
                                        <Button
                                            variant="ghost"
                                            size="sm"
                                            className="text-gray-400 hover:text-gray-600"
                                        >
                                            <MoreVertical className="w-4 h-4"/>
                                        </Button>
                                    </DropdownMenuTrigger>
                                    <DropdownMenuContent align="end">
                                        <DropdownMenuItem
                                            onClick={handleDeleteConversation}
                                            className="text-red-600 focus:text-red-700 focus:bg-red-50 cursor-pointer"
                                        >
                                            <Trash2 className="w-4 h-4 mr-2"/>
                                            删除会话
                                        </DropdownMenuItem>
                                    </DropdownMenuContent>
                                </DropdownMenu>
                            </>
                        ) : (
                            <div className="text-gray-400 text-sm">请选择会话</div>
                        )}
                    </div>

                    {/* 消息列表 */}
                    <div className="flex-1 overflow-y-auto p-4 md:p-6">
                        {selectedPeer && currentMessages.length > 0 ? (
                            <div className="mx-auto w-full max-w-[980px] space-y-4">
                                {currentMessages.map((msg) => (
                                    <div
                                        key={msg.id}
                                        className={`flex ${msg.type === 'outgoing' ? 'justify-end' : 'justify-start'} animate-in fade-in slide-in-from-bottom-2 duration-200 group`}
                                    >
                                        <div
                                            className={`relative flex max-w-[82%] flex-col sm:max-w-[75%] xl:max-w-[68%] ${msg.type === 'outgoing' ? 'items-end' : 'items-start'}`}>
                                            <div
                                                className={`rounded-2xl px-4 py-2.5 shadow-none text-sm leading-relaxed relative ${
                                                    msg.type === 'outgoing'
                                                        ? 'bg-[#0b2a55] text-white rounded-tr-sm'
                                                        : 'bg-white text-gray-800 border border-gray-100 rounded-tl-sm'
                                                }`}
                                            >
                                                <p className="break-words">{msg.content}</p>
                                                {/* 删除按钮 - 悬停时显示 */}
                                                <button
                                                    onClick={(e) => handleDeleteMessage(msg.id, e)}
                                                    className={`absolute -top-2 ${msg.type === 'outgoing' ? '-left-2' : '-right-2'} rounded-full border border-rose-200 bg-white p-1 text-rose-500 opacity-0 transition-opacity hover:bg-rose-50 group-hover:opacity-100`}
                                                    title="删除消息"
                                                >
                                                    <Trash2 className="w-3 h-3"/>
                                                </button>
                                            </div>
                                            <div className={`flex items-center space-x-2 mt-1 px-1 ${
                                                msg.type === 'outgoing' ? 'flex-row-reverse space-x-reverse' : ''
                                            }`}>
                                                <span
                                                    className={`text-[10px] ${msg.type === 'outgoing' ? 'text-blue-600' : 'text-gray-400'}`}>
                                                    {formatTime(msg.createdAt)}
                                                </span>
                                                {msg.type === 'outgoing' && getStatusBadge(msg.status)}
                                            </div>
                                        </div>
                                    </div>
                                ))}
                                <div ref={messagesEndRef}/>
                            </div>
                        ) : (
                            <div className="h-full flex flex-col items-center justify-center text-gray-400">
                                <Send className="w-12 h-12 mb-4 opacity-20"/>
                                <p className="text-sm">
                                    {selectedPeer ? '暂无消息，可以发送第一条短信' : '选择左侧联系人开始查看消息'}
                                </p>
                            </div>
                        )}
                    </div>

                    {/* 输入框 */}
                    <div className="p-4 bg-white border-t border-gray-200">
                        <form className="flex gap-3" onSubmit={handleSendSMS}>
                            <Input
                                type="text"
                                value={inputText}
                                onChange={(e) => setInputText(e.target.value)}
                                placeholder={!canSend ? unavailableReason : selectedPeer ? '输入消息内容...' : '请先选择联系人'}
                                disabled={!canSend || !selectedPeer || sendSMSMutation.isPending}
                                className="flex-1 bg-gray-50 border-gray-200 focus:bg-white focus:border-blue-500 h-10"
                            />
                            <Button
                                type="submit"
                                disabled={!canSend || !selectedPeer || !inputText.trim() || sendSMSMutation.isPending}
                                className="h-10 bg-[#0b2a55] px-6 text-white shadow-none hover:bg-slate-800"
                            >
                                {sendSMSMutation.isPending ? (
                                    <div
                                        className="w-4 h-4 border-2 border-white border-t-transparent rounded-full animate-spin"/>
                                ) : (
                                    <>
                                        <Send className="w-4 h-4 mr-2"/>
                                        发送
                                    </>
                                )}
                            </Button>
                        </form>
                    </div>
                </div>
            </div>

            <Dialog open={composeOpen} onOpenChange={handleComposeOpenChange}>
                <DialogContent className="sm:max-w-lg">
                    <form onSubmit={handleSendNewSMS}>
                        <DialogHeader>
                            <DialogTitle>新建短信</DialogTitle>
                            <DialogDescription>输入目标号码和短信内容，发送后将自动打开对应会话。</DialogDescription>
                        </DialogHeader>

                        <div className="space-y-5 py-5">
                            <div className="space-y-1.5">
                                <label htmlFor="new-sms-recipient" className="block text-sm font-medium text-slate-800">目标手机号</label>
                                <Input
                                    id="new-sms-recipient"
                                    type="tel"
                                    value={newRecipient}
                                    onChange={(event) => updateComposeDraft({recipient: event.target.value})}
                                    placeholder="请输入手机号"
                                    autoComplete="tel"
                                    disabled={sendSMSMutation.isPending}
                                    autoFocus
                                />
                            </div>
                            <div className="space-y-1.5">
                                <div className="flex items-center justify-between">
                                    <label htmlFor="new-sms-content" className="block text-sm font-medium text-slate-800">短信内容</label>
                                    <span className="text-xs text-slate-400">{newContent.length} 字</span>
                                </div>
                                <Textarea
                                    id="new-sms-content"
                                    value={newContent}
                                    onChange={(event) => updateComposeDraft({content: event.target.value})}
                                    placeholder="请输入短信内容"
                                    className="min-h-32 resize-none"
                                    disabled={sendSMSMutation.isPending}
                                />
                            </div>
                            {!canSend && (
                                <p className="rounded-lg border border-rose-200 bg-rose-50 px-3.5 py-3 text-xs leading-5 text-rose-700">
                                    {unavailableReason}。历史短信仍可正常查看和管理。
                                </p>
                            )}
                        </div>

                        <DialogFooter>
                            <Button type="button" variant="outline" onClick={() => handleComposeOpenChange(false)} disabled={sendSMSMutation.isPending}>
                                取消
                            </Button>
                            <Button
                                type="submit"
                                disabled={!canSend || !newRecipient.trim() || !newContent.trim() || sendSMSMutation.isPending}
                                className="bg-blue-600 text-white hover:bg-blue-700"
                            >
                                {sendSMSMutation.isPending ? <Loader2 className="size-4 animate-spin"/> : <Send className="size-4"/>}
                                {sendSMSMutation.isPending ? '发送中...' : '发送短信'}
                            </Button>
                        </DialogFooter>
                    </form>
                </DialogContent>
            </Dialog>
        </div>
    );
}
