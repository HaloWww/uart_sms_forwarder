import apiClient from './client';
import type {Stats, Conversation, TextMessage} from './types';

// 获取统计信息
export const getStats = (simId: string): Promise<Stats> => {
    return apiClient.get('/messages/stats', {params: {simId}});
};

// 获取会话列表（按对方号码分组）
export const getConversations = (simId: string): Promise<Conversation[]> => {
    return apiClient.get('/messages/conversations', {params: {simId}});
};

// 获取指定会话的所有消息
export const getConversationMessages = (peer: string, simId: string): Promise<TextMessage[]> => {
    return apiClient.get(`/messages/conversations/${encodeURIComponent(peer)}/messages`, {params: {simId}});
};

// 删除单条短信
export const deleteMessage = (id: string, simId: string) => {
    return apiClient.delete(`/messages/${id}`, {params: {simId}});
};

// 删除整个会话（与某个联系人的所有消息）
export const deleteConversation = (peer: string, simId: string) => {
    return apiClient.delete(`/messages/conversations/${encodeURIComponent(peer)}`, {params: {simId}});
};

// 清空所有短信
export const clearMessages = (simId: string) => {
    return apiClient.delete('/messages', {params: {simId}});
};
