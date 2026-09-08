// 定时任务配置
import apiClient from "@/api/client.ts";

export type LastRunStatus = 'unknown' | 'success' | 'failed' | 'ambiguous';

export interface ScheduledTask {
    id: string;
    name: string;
    enabled: boolean;
    intervalDays: number;
    phoneNumber: string;
    content: string;
    simId: string;
    // 仅用于识别尚未迁移的旧任务，新的保存请求不会再提交 deviceId。
    deviceId?: string;
    createdAt?: number;
    lastRunAt?: number;
    lastMsgId?: string;
    lastRunStatus?: LastRunStatus;
}

export type ScheduledTaskInput = Pick<
    ScheduledTask,
    'simId' | 'name' | 'enabled' | 'intervalDays' | 'phoneNumber' | 'content'
>;

export interface ScheduledTaskTriggerResponse {
	message: string;
	messageId: string;
	status?: 'ambiguous';
}

export interface ScheduledTaskTriggerRequest {
	id: string;
	requestId: string;
}

// 定时任务 API (RESTful)
// 获取所有定时任务
export const getScheduledTasks = () => {
    return apiClient.get<ScheduledTask[]>('/scheduled-tasks');
};

// 获取单个定时任务
export const getScheduledTask = (id: string) => {
    return apiClient.get<ScheduledTask>(`/scheduled-tasks/${id}`);
};

// 创建定时任务
export const createScheduledTask = (task: ScheduledTaskInput) => {
    return apiClient.post<ScheduledTask>('/scheduled-tasks', task);
};

// 更新定时任务
export const updateScheduledTask = (id: string, task: ScheduledTaskInput) => {
    return apiClient.put<ScheduledTask>(`/scheduled-tasks/${id}`, task);
};

// 删除定时任务
export const deleteScheduledTask = (id: string) => {
    return apiClient.delete<{ message: string }>(`/scheduled-tasks/${id}`);
};

// 立即触发定时任务
export const triggerScheduledTask = ({id, requestId}: ScheduledTaskTriggerRequest) => {
	return apiClient.post<ScheduledTaskTriggerResponse>(
		`/scheduled-tasks/${id}/trigger`,
		{requestId},
		{headers: {'Idempotency-Key': requestId}},
	);
};
