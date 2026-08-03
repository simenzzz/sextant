/**
 * React wiring for a question run — and nothing else.
 *
 * Components call ask() / stop() and read state. All run logic lives in the
 * reducer, all transport in the SSE client. The hook owns only the wiring
 * between them, plus the one round trip the transport cannot do: EventSource
 * can only GET, so the question is POSTed here and the returned run path is
 * handed to the stream.
 */
import { useCallback, useEffect, useReducer, useRef } from 'react'

import { apiUrl } from '../lib/apiUrl'
import { buildQuestionRequest } from '../lib/protocol'
import { createSseClient, type EventSourceCtor, type SseClient } from '../lib/sseClient'
import { initialState, questionReducer } from './questionReducer'

export interface UseQuestionOptions {
  baseUrl?: string
  /** Test seam: inject a fake EventSource. */
  EventSourceImpl?: EventSourceCtor
  /** Test seam: inject a fetch. Defaults to the global. */
  fetchImpl?: typeof fetch
}

/** Shape of the POST /v1/questions response. */
interface AskResponse {
  run_id?: unknown
  events?: unknown
}

export function useQuestion(options: UseQuestionOptions = {}) {
  const [state, dispatch] = useReducer(questionReducer, initialState)
  const clientRef = useRef<SseClient | null>(null)
  const { baseUrl, EventSourceImpl, fetchImpl } = options

  useEffect(() => {
    const client = createSseClient({
      baseUrl,
      EventSourceImpl,
      onEvent: (event) => dispatch({ type: 'event_received', event }),
      onResult: (result) => dispatch({ type: 'result_received', result }),
      onLedger: (ledger) => dispatch({ type: 'ledger_received', ledger }),
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

  const ask = useCallback(
    async (question: string, database: string) => {
      dispatch({ type: 'run_requested' })

      // Validated before it leaves the browser. The server revalidates — this
      // only turns a rejected request into an inline message instead of a
      // round trip.
      const request = buildQuestionRequest(question, database)
      if (!request.ok) {
        dispatch({ type: 'invalid_frame', error: request.error })
        dispatch({ type: 'status_changed', status: 'error' })
        return
      }

      const doFetch = fetchImpl ?? globalThis.fetch
      let response: Response
      try {
        response = await doFetch(apiUrl('/v1/questions', baseUrl), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(request.value),
        })
      } catch (err) {
        dispatch({ type: 'invalid_frame', error: (err as Error).message })
        dispatch({ type: 'status_changed', status: 'error' })
        return
      }

      if (!response.ok) {
        // The server's error messages are already sanitized — nothing from a
        // provider, a driver, or the request body reaches them — so showing
        // one is safe. A body that is not the expected shape falls back to the
        // status.
        let message = `request failed with status ${response.status}`
        try {
          const body: unknown = await response.json()
          if (body && typeof body === 'object' && typeof (body as { error?: unknown }).error === 'string') {
            message = (body as { error: string }).error
          }
        } catch {
          // Keep the status-derived message.
        }
        dispatch({ type: 'invalid_frame', error: message })
        dispatch({ type: 'status_changed', status: 'error' })
        return
      }

      let body: AskResponse
      try {
        body = (await response.json()) as AskResponse
      } catch (err) {
        dispatch({ type: 'invalid_frame', error: `malformed response: ${(err as Error).message}` })
        dispatch({ type: 'status_changed', status: 'error' })
        return
      }

      // The path is server-supplied, so it goes through apiUrl's checks rather
      // than being concatenated: a path like `@evil.example/x` would otherwise
      // produce a URL whose origin is evil.example.
      const path = typeof body.events === 'string'
        ? body.events
        : typeof body.run_id === 'string'
          ? `/v1/runs/${body.run_id}/events`
          : null

      if (!path) {
        dispatch({ type: 'invalid_frame', error: 'response carried no run to stream' })
        dispatch({ type: 'status_changed', status: 'error' })
        return
      }

      clientRef.current?.start(path)
    },
    [baseUrl, fetchImpl],
  )

  // Close the stream as soon as the run reaches a terminal event.
  //
  // Without this the server finishes, closes the connection, and EventSource
  // reports that close as `onerror` — so a perfectly ANSWERED run displayed as
  // an error — and then auto-reconnects, issuing a second GET on the same run.
  // That reconnect is the trigger that would re-run a question if anything
  // ever left a run row unfinished, so closing here removes the trigger as
  // well as the wrong status.
  useEffect(() => {
    if (state.outcome !== null) {
      clientRef.current?.close()
    }
  }, [state.outcome])

  const stop = useCallback(() => {
    clientRef.current?.close()
  }, [])

  const reset = useCallback(() => {
    clientRef.current?.close()
    dispatch({ type: 'reset' })
  }, [])

  return { state, ask, stop, reset }
}
