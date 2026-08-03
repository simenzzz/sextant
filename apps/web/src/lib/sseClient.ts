/**
 * A thin, framework-agnostic wrapper over one SSE connection.
 *
 * It holds NO application state. Its whole job is: open a stream, hand each
 * validated frame to a callback, report status changes, and shut down cleanly.
 * All interpretation of those frames lives in the reducer; all React wiring
 * lives in the hook. Keeping those three apart is what makes the transport
 * testable without a DOM and the reducer testable without a network.
 */
import { apiUrl } from './apiUrl'
import {
  parseCostLedger,
  parseEvent,
  parseResultSet,
  type CostLedgerV1,
  type ResultSetV1,
  type TraceEventV1,
} from './protocol'

export type SseStatus = 'idle' | 'connecting' | 'open' | 'closed' | 'error'

/**
 * Minimal EventSource shape, so tests can inject a fake rather than standing
 * up a server. jsdom ships no EventSource, so this seam is not optional.
 *
 * `addEventListener` is here because the runtime sends three different
 * contracts on one stream: trace events on the default `message` name, and the
 * result set and cost ledger on their own. `onmessage` only ever receives the
 * default name, so without this the answer and the bill would never arrive.
 */
export interface EventSourceLike {
  onopen: ((this: unknown, ev: Event) => unknown) | null
  onmessage: ((this: unknown, ev: MessageEvent) => unknown) | null
  onerror: ((this: unknown, ev: Event) => unknown) | null
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void
  removeEventListener(type: string, listener: (ev: MessageEvent) => void): void
  close(): void
}

export type EventSourceCtor = new (url: string) => EventSourceLike

export interface SseClientOptions {
  /** Base URL of the agent runtime. Defaults to VITE_API_URL. */
  baseUrl?: string
  onEvent: (event: TraceEventV1) => void
  /** Called once with the rows a run returned, when it returned any. */
  onResult?: (result: ResultSetV1) => void
  /** Called once with what the run spent. Always sent, including on a cap. */
  onLedger?: (ledger: CostLedgerV1) => void
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

/** The named frames the runtime emits, alongside the default trace event. */
const RESULT_EVENT = 'result_set'
const LEDGER_EVENT = 'cost_ledger'

export function createSseClient(options: SseClientOptions): SseClient {
  const Impl = options.EventSourceImpl ?? (globalThis.EventSource as unknown as EventSourceCtor)

  let source: EventSourceLike | null = null
  let disposed = false
  /** Named listeners, kept so teardown can detach exactly what it attached. */
  let named: Array<[string, (ev: MessageEvent) => void]> = []

  const setStatus = (status: SseStatus) => {
    if (!disposed) options.onStatus?.(status)
  }

  /**
   * Detaching every handler before closing matters more than it looks: an
   * EventSource can fire onerror after close(), and a late callback from a
   * finished run would otherwise dispatch into the next one. The named
   * listeners have to come off too — a `result_set` arriving after teardown
   * would render one run's rows under another run's question.
   */
  const teardown = () => {
    if (!source) return
    source.onopen = null
    source.onmessage = null
    source.onerror = null
    for (const [type, listener] of named) {
      source.removeEventListener(type, listener)
    }
    named = []
    source.close()
    source = null
  }

  /** Wires one named frame through its own contract validator. */
  const attach = <T>(
    es: EventSourceLike,
    type: string,
    parse: (raw: string) => { ok: true; value: T } | { ok: false; error: string },
    deliver: ((value: T) => void) | undefined,
  ) => {
    const listener = (ev: MessageEvent) => {
      if (disposed) return
      if (typeof ev.data !== 'string') {
        options.onInvalidFrame?.(`received a non-string ${type} frame`)
        return
      }
      const parsed = parse(ev.data)
      if (!parsed.ok) {
        options.onInvalidFrame?.(parsed.error)
        return
      }
      deliver?.(parsed.value)
    }
    es.addEventListener(type, listener)
    named.push([type, listener])
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

      attach(es, RESULT_EVENT, parseResultSet, options.onResult)
      attach(es, LEDGER_EVENT, parseCostLedger, options.onLedger)

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
