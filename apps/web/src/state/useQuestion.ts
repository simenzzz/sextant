/**
 * React wiring for a question run — and nothing else.
 *
 * Components call ask() / stop() and read state. All run logic lives in the
 * reducer, all transport in the SSE client. The hook owns only the wiring
 * between them.
 */
import { useCallback, useEffect, useReducer, useRef } from 'react'

import { createSseClient, type EventSourceCtor, type SseClient } from '../lib/sseClient'
import { initialState, questionReducer } from './questionReducer'

export interface UseQuestionOptions {
  baseUrl?: string
  /** Test seam: inject a fake EventSource. */
  EventSourceImpl?: EventSourceCtor
}

export function useQuestion(options: UseQuestionOptions = {}) {
  const [state, dispatch] = useReducer(questionReducer, initialState)
  const clientRef = useRef<SseClient | null>(null)
  const { baseUrl, EventSourceImpl } = options

  useEffect(() => {
    const client = createSseClient({
      baseUrl,
      EventSourceImpl,
      onEvent: (event) => dispatch({ type: 'event_received', event }),
      onStatus: (status) => dispatch({ type: 'status_changed', status }),
      onInvalidFrame: (error) => dispatch({ type: 'invalid_frame', error }),
    })
    clientRef.current = client
    // dispose(), not close(): an unmounted component must not receive a late
    // callback, and dispose detaches the handlers rather than only closing.
    return () => {
      client.dispose()
      clientRef.current = null
    }
  }, [baseUrl, EventSourceImpl])

  const ask = useCallback((runPath: string) => {
    dispatch({ type: 'run_requested' })
    clientRef.current?.start(runPath)
  }, [])

  const stop = useCallback(() => {
    clientRef.current?.close()
  }, [])

  return { state, ask, stop }
}
