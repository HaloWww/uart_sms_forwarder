import {createContext, useContext} from 'react';
import type {SimStatus} from '@/api/types';

export interface SimContextValue {
    sims: SimStatus[];
    selectedSimId: string;
    setSelectedSimId: (id: string) => void;
    selectedSim?: SimStatus;
    isLoading: boolean;
    isFetching: boolean;
    dataUpdatedAt: number;
}

export const SimContext = createContext<SimContextValue | null>(null);

export function useSim() {
    const context = useContext(SimContext);
    if (!context) throw new Error('useSim must be used inside SimProvider');
    return context;
}
