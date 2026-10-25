import { useEffect, useState } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import type { Page } from '../api/types'

export function useDebouncedValue<T>(value: T, delay = 250): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delay)
    return () => window.clearTimeout(timer)
  }, [value, delay])
  return debounced
}

export interface PaginatedResult<T> {
  items: T[]
  isPending: boolean
  isSuccess: boolean
  isError: boolean
  error: unknown
  refetch: () => void
  retry: () => void
  hasNextPage: boolean
  fetchNextPage: () => void
  isFetchingNextPage: boolean
  isFetching: boolean
  isRefetchError: boolean
  isFetchNextPageError: boolean
}

export function usePaginated<T>(
  queryKey: readonly unknown[],
  fetchPage: (cursor?: string) => Promise<Page<T>>,
  collectionKey: string,
  options: { enabled?: boolean; refetchInterval?: number } = {},
): PaginatedResult<T> {
  const query = useInfiniteQuery({
    queryKey,
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage?.next_cursor || undefined,
    enabled: options.enabled ?? true,
    retry: false,
    refetchInterval: options.refetchInterval,
  })

  const items: T[] = []
  for (const page of query.data?.pages || []) {
    const list = (page as unknown as Record<string, unknown>)?.[collectionKey]
    if (Array.isArray(list)) items.push(...(list as T[]))
  }

  const auxiliaryError = query.isFetchNextPageError || query.isRefetchError
  // Retry only the request that failed. Refetch stays a full refresh of loaded pages.
  const retry = () => {
    if (query.isFetchNextPageError && query.hasNextPage) {
      query.fetchNextPage().catch(() => {})
    } else {
      query.refetch().catch(() => {})
    }
  }
  return {
    items,
    isPending: query.isPending,
    isSuccess: query.isSuccess && !auxiliaryError,
    isError: query.isError || auxiliaryError,
    error: query.error,
    refetch: () => { query.refetch().catch(() => {}) },
    retry,
    hasNextPage: Boolean(query.hasNextPage),
    fetchNextPage: () => { query.fetchNextPage().catch(() => {}) },
    isFetchingNextPage: query.isFetchingNextPage,
    isFetching: query.isFetching,
    isRefetchError: query.isRefetchError,
    isFetchNextPageError: query.isFetchNextPageError,
  }
}

export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), intervalMs)
    return () => window.clearInterval(timer)
  }, [intervalMs])
  return now
}

export function elapsedSince(start?: string, now = Date.now()): string {
  if (!start) return '—'
  const started = new Date(start).getTime()
  if (Number.isNaN(started)) return '—'
  const seconds = Math.max(0, Math.floor((now - started) / 1000))
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  return `${minutes}m ${seconds % 60}s`
}
