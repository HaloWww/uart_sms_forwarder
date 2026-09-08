import apiClient from './client';
import type {ListResult, Stats, Conversation, TextMessage} from './types';

// 获取统计信息
export const getStats = (deviceId?: string): Promise<Stats> => {
    return apiClient.get('/messages/stats', {params: {deviceId}});
};

// 获取会话列表（按对方号码分组）
export const getConversations = (deviceId?: string): Promise<Conversation[]> => {
    return apiClient.get('/messages/conversations', {params: {deviceId}});
};

// 获取指定会话的所有消息
export const getConversationMessages = (peer: string, deviceId?: string): Promise<TextMessage[]> => {
    return apiClient.get(`/messages/conversations/${encodeURIComponent(peer)}/messages`, {params: {deviceId}});
};

// 删除单条短信
export const deleteMessage = (id: string) => {
    return apiClient.delete(`/messages/${id}`);
};

// 删除整个会话（与某个联系人的所有消息）
export const deleteConversation = (peer: string, deviceId?: string) => {
    return apiClient.delete(`/messages/conversations/${encodeURIComponent(peer)}`, {params: {deviceId}});
};

// 清空所有短信
export const clearMessages = (deviceId?: string) => {
    return apiClient.delete('/messages', {params: {deviceId}});
};
