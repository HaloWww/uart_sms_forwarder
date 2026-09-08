import apiClient from './client';
import type {DeviceStatus, SendSMSRequest, SendSMSResponse, SimStatus} from './types';

// 发送短信
export const sendSMS = (data: SendSMSRequest) => {
  return apiClient.post<SendSMSResponse>('/serial/sms', data, {
    headers: {'Idempotency-Key': data.requestId},
  });
};

// 获取设备状态（包含移动网络信息）
export const getStatus = (simId: string) =>
  apiClient.get<DeviceStatus>('/serial/status', {params: {simId}});

// 物理设备接口保留给底层诊断；业务页面统一使用持久化 SIM 档案。
export const getDevices = () => apiClient.get<DeviceStatus[]>('/serial/devices');

export const getSims = () => apiClient.get<SimStatus[]>('/serial/sims');

// 设置飞行模式
export const setFlymode = (enabled: boolean, simId: string) => {
  return apiClient.post('/serial/flymode', {enabled, simId});
};

// 重启模块
export const rebootMcu = (simId: string) => {
  return apiClient.post('/serial/reboot', {simId});
};

