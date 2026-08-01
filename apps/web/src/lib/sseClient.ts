/**
 * A thin, framework-agnostic wrapper over one SSE connection.
 *
 * It holds NO application state. Its whole job is: open a stream, hand each
 * validated trace event to a callback, report status changes, and shut down
 * cleanly. All interpretation of those events lives in the reducer; all React
 * wiring lives in the hook. Keeping those three apart is what makes the
 * transport testable without a DOM and the reducer testable without a network.
 */
import { apiUrl } from './apiUrl'
import { parseEvent, type TraceEventV1 } from './protocol'

export type SseStatus = 'idle' | 'connecting' | 'open' | 'closed' | 'error'

/**
 * Minimal EventSource shape, so tests can inject a fake rather than standing
 * up a server. jsdom ships no EventSource, so this seam is not optional.
 */
export interface EventSourceLike {
  onopen: ((this: unknown, ev: Event) => unknown) | null
  onmessage: ((this: unknown, ev: MessageEvent) => unknown) | null
  onerror: ((this: unknown, ev: Event) => unknown) | null
  close(): void
}

export type EventSourceCtor = new (url: string) => EventSourceLike

export interface SseClientOptions {
  /** Base URL of the agent runtime. Defaults to VITE_API_URL. */
  baseUrl?: string
  onEvent: (event: TraceEventV1) => void
  onStatus?: (status: SseStatus) => void
  /** Called for frames that failed validation. The stream stays open. */
  onInvalidFrame?: (error: string) => void
  EventSourceImpl?: EventSourceCtor
}

export interface SseClient {
  /** Opens a stream for one run. Closes any stream already open. */
  start(runPath: string): void
  /** Closes the current stream, if any. */
  close(): void
  /** Closes and detaches every handler. The client is unusable afterwards. */
  dispose(): void
}

export function createSseClient(options: SseClientOptions): SseClient {
  const Impl = options.EventSourceImpl ?? (globalThis.EventSource as unknown as EventSourceCtor)

  let source: EventSourceLike | null = null
  let disposed = false

  const setStatus = (status: SseStatus) => {
    if (!disposed) options.onStatus?.(status)
  }

  /**
   * Detaching every handler before closing matters more than it looks: an
   * EventSource can fire onerror after close(), and a late callback from a
   * finished run would otherwise dispatch into the next one.
   */
  const teardown = () => {
    if (!source) return
    source.onopen = null
    source.onmessage = null
    source.onerror = null
    source.close()
    source = null
  }

  return {
    start(runPath: string) {
      if (disposed) return
      teardown()

      if (typeof Impl !== 'function') {
        setStatus('error')
        options.onInvalidFrame?.('no EventSource implementation available')
        return
      }

      let url: string
      try {
        url = apiUrl(runPath, options.baseUrl)
      } catch (err) {
        setStatus('error')
        options.onInvalidFrame?.((err as Error).message)
        return
      }

      setStatus('connecting')
      const es = new Impl(url)
      source = es

      es.onopen = () => setStatus('open')

      es.onmessage = (ev: MessageEvent) => {
        if (disposed) return
        // A non-string frame cannot have come from a conforming server.
        if (typeof ev.data !== 'string') {
          options.onInvalidFrame?.('received a non-string frame')
          return
        }
        const parsed = parseEvent(ev.data)
        if (!parsed.ok) {
          // One bad frame degrades the stream; it does not end the run.
          options.onInvalidFrame?.(parsed.error)
          return
        }
        options.onEvent(parsed.value)
      }

      es.onerror = () => {
        if (disposed) return
        setStatus('error')
      }
    },

    close() {
      teardown()
      setStatus('closed')
    },

    dispose() {
      teardown()
      disposed = true
    },
  }
}
