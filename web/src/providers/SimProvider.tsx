import {useMemo, useState, type ReactNode} from 'react';
import {useQuery} from '@tanstack/react-query';
import {getSims} from '@/api/serial';
import type {SimStatus} from '@/api/types';
import {SimContext, type SimContextValue} from '@/providers/SimContext';

const SELECTED_SIM_STORAGE_KEY = 'selectedSimId';

export function SimProvider({children}: {children: ReactNode}) {
    const [selectedSimId, setSelectedSimIdState] = useState(
        () => localStorage.getItem(SELECTED_SIM_STORAGE_KEY) || '',
    );
    const {data: sims = [], isLoading, isFetching, dataUpdatedAt} = useQuery<SimStatus[]>({
        queryKey: ['sims'],
        queryFn: getSims,
        refetchInterval: 10000,
    });

    const setSelectedSimId = (id: string) => {
        setSelectedSimIdState(id);
        if (id) localStorage.setItem(SELECTED_SIM_STORAGE_KEY, id);
        else localStorage.removeItem(SELECTED_SIM_STORAGE_KEY);
    };

    const value = useMemo<SimContextValue>(() => ({
        sims,
        selectedSimId,
        setSelectedSimId,
        selectedSim: sims.find((sim) => sim.simId === selectedSimId),
        isLoading,
        isFetching,
        dataUpdatedAt,
    }), [dataUpdatedAt, isFetching, isLoading, sims, selectedSimId]);

    return <SimContext.Provider value={value}>{children}</SimContext.Provider>;
}
