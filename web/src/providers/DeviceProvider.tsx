import {createContext, useContext, useEffect, useMemo, useState, type ReactNode} from 'react';
import {useQuery} from '@tanstack/react-query';
import {getDevices} from '@/api/serial';
import type {DeviceStatus} from '@/api/types';

interface DeviceContextValue {
    devices: DeviceStatus[];
    selectedDeviceId: string;
    setSelectedDeviceId: (id: string) => void;
    selectedDevice?: DeviceStatus;
}

const DeviceContext = createContext<DeviceContextValue | null>(null);

export function DeviceProvider({children}: {children: ReactNode}) {
    const [selectedDeviceId, setSelectedDeviceIdState] = useState(() => localStorage.getItem('selectedDeviceId') || '');
    const {data: devices = []} = useQuery<DeviceStatus[]>({
        queryKey: ['devices'],
        queryFn: getDevices,
        refetchInterval: 10000,
    });

    useEffect(() => {
        if (devices.length > 0 && !devices.some((device) => device.device_id === selectedDeviceId)) {
            setSelectedDeviceIdState(devices[0].device_id);
            localStorage.setItem('selectedDeviceId', devices[0].device_id);
        }
    }, [devices, selectedDeviceId]);

    const setSelectedDeviceId = (id: string) => {
        setSelectedDeviceIdState(id);
        localStorage.setItem('selectedDeviceId', id);
    };

    const value = useMemo(() => ({
        devices,
        selectedDeviceId,
        setSelectedDeviceId,
        selectedDevice: devices.find((device) => device.device_id === selectedDeviceId),
    }), [devices, selectedDeviceId]);

    return <DeviceContext.Provider value={value}>{children}</DeviceContext.Provider>;
}

export function useDevice() {
    const context = useContext(DeviceContext);
    if (!context) throw new Error('useDevice must be used inside DeviceProvider');
    return context;
}
