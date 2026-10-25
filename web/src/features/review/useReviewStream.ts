import { useCallback, useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { streamUrl, UNAUTHORIZED_EVENT } from '../../api/client'
import type { ReviewDetail } from '../../api/types'

export type StreamState = 'idle' | 'connecting' | 'open' | 'error' | 'closed'

interface StreamEvent {
  id?: string
  status?: string
  verdict?: string | null
  publication_state?: string
  decision?: string
}

// The backend keeps the stream open until the publication settles. Mirror that
// rule so a completed review that is still publishing does not close early.
function workflowSettled(status?: string, publicationState?: string): boolean {
  if (!status || !['completed', 'failed', 'cancelled'].includes(status)) return false
  return publicationState === undefined || publicationState === ''
    || ['published', 'skipped', 'failed'].includes(publicationState)
}

export function useReviewStream(reviewId: string | undefined, review: ReviewDetail | undefined) {
  const queryClient = useQueryClient()
  const [state, setState] = useState<StreamState>('idle')
  const [error, setError] = useState<string | null>(null)
  const [attempt, setAttempt] = useState(0)
  const sourceRef = useRef<EventSource | null>(null)
  const terminal = workflowSettled(review?.status, review?.publication_state)

  useEffect(() => {
    if (!reviewId || terminal) {
      setState('closed')
      return
    }
    setState('connecting')
    setError(null)
    const source = new EventSource(streamUrl(reviewId))
    sourceRef.current = source

    const invalidateReview = () => {
      void queryClient.invalidateQueries({ queryKey: ['review', reviewId] })
    }
    const invalidateList = () => {
      void queryClient.invalidateQueries({ queryKey: ['reviews'] })
      void queryClient.invalidateQueries({ queryKey: ['stats'] })
    }

    const onOpen = () => setState('open')
    const onError = () => {
      setState('error')
      setError('实时连接中断。可重试；页面数据不会因此显示为成功。')
      source.close()
    }
    const onNode = (event: MessageEvent<string>) => {
      try {
        const data = JSON.parse(event.data) as StreamEvent
        void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'detail'] })
        void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'graph'] })
        if (data.id) {
          void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'node', data.id] })
          void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'calls', data.id] })
        }
        invalidateList()
      } catch {
        setError('收到无法解析的节点事件。')
      }
    }
    const onFinding = (event: MessageEvent<string>) => {
      try {
        JSON.parse(event.data)
        void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'findings'] })
        void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'detail'] })
        void queryClient.invalidateQueries({ queryKey: ['reviews'] })
      } catch {
        setError('收到无法解析的问题事件。')
      }
    }
    const onWorkflow = (event: MessageEvent<string>) => {
      try {
        const data = JSON.parse(event.data) as StreamEvent
        invalidateReview()
        invalidateList()
        if (workflowSettled(data.status, data.publication_state)) {
          source.close()
          setState('closed')
        }
      } catch {
        setError('收到无法解析的工作流事件。')
      }
    }
    const onPublished = (event: MessageEvent<string>) => {
      try {
        JSON.parse(event.data)
        void queryClient.invalidateQueries({ queryKey: ['review', reviewId, 'detail'] })
      } catch {
        setError('收到无法解析的发布事件。')
      }
    }
    const onUnauthorized = () => {
      source.close()
      setState('error')
      setError('会话已过期，实时更新已停止。')
    }

    source.addEventListener('open', onOpen)
    source.addEventListener('error', onError)
    source.addEventListener('node', onNode as EventListener)
    source.addEventListener('finding', onFinding as EventListener)
    source.addEventListener('workflow', onWorkflow as EventListener)
    source.addEventListener('published', onPublished as EventListener)
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized)

    return () => {
      source.close()
      sourceRef.current = null
      source.removeEventListener('open', onOpen)
      source.removeEventListener('error', onError)
      source.removeEventListener('node', onNode as EventListener)
      source.removeEventListener('finding', onFinding as EventListener)
      source.removeEventListener('workflow', onWorkflow as EventListener)
      source.removeEventListener('published', onPublished as EventListener)
      window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
    }
  }, [reviewId, terminal, attempt, queryClient])

  const retry = useCallback(() => setAttempt((value) => value + 1), [])
  return { state, error, retry, terminal, sourceRef }
}
