import apiClient from './client';
import type { SendSMSRequest } from './types';

// 发送短信
export const sendSMS = (data: SendSMSRequest) => {
  return apiClient.post('/serial/sms', data);
};

// 获取设备状态（包含移动网络信息）
export const getStatus = (deviceId?: string) =>
  apiClient.get('/serial/status', {params: {deviceId}});

export const getDevices = () => apiClient.get<import('./types').DeviceStatus[]>('/serial/devices');

// 设置飞行模式
export const setFlymode = (enabled: boolean, deviceId?: string) => {
  return apiClient.post('/serial/flymode', { enabled, deviceId });
};

// 重启模块
export const rebootMcu = (deviceId?: string) => {
  return apiClient.post('/serial/reboot', {deviceId});
};

