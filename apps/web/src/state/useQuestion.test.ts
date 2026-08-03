import { act, renderHook, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { EventSourceCtor, EventSourceLike } from '../lib/sseClient'
import { useQuestion } from './useQuestion'

class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = []

  onopen: ((this: unknown, ev: Event) => unknown) | null = null
  onmessage: ((this: unknown, ev: MessageEvent) => unknown) | null = null
  onerror: ((this: unknown, ev: Event) => unknown) | null = null
  closed = false
  listeners = new Map<string, Array<(ev: MessageEvent) => void>>()

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: (ev: MessageEvent) => void) {
    const existing = this.listeners.get(type) ?? []
    this.listeners.set(type, [...existing, listener])
  }

  removeEventListener(type: string, listener: (ev: MessageEvent) => void) {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter((l) => l !== listener))
  }

  /** Delivers a named frame, as a real EventSource would. */
  emit(type: string, data: string) {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data } as MessageEvent)
    }
  }

  close() {
    this.closed = true
  }
}

const Ctor = FakeEventSource as unknown as EventSourceCtor

const frame = (overrides: Record<string, unknown> = {}) =>
  JSON.stringify({
    schema: 'trace_event.v1',
    type: 'run_started',
    step: 0,
    elapsed_ms: 0,
    ...overrides,
  })

/** A fetch that answers POST /v1/questions the way the runtime does. */
function okFetch(body: Record<string, unknown> = { run_id: 'r_abc', events: '/v1/runs/r_abc/events' }) {
  return vi.fn(async () =>
    new Response(JSON.stringify(body), {
      status: 201,
      headers: { 'Content-Type': 'application/json' },
    }),
  ) as unknown as typeof fetch
}

const setup = (fetchImpl: typeof fetch = okFetch()) =>
  renderHook(() =>
    useQuestion({ baseUrl: 'http://runtime.test', EventSourceImpl: Ctor, fetchImpl }),
  )

/** Asks, then waits for the POST to resolve and the stream to open. */
async function ask(result: { current: ReturnType<typeof useQuestion> }, q = 'how many orders?') {
  await act(async () => {
    await result.current.ask(q, 'toy')
  })
}

beforeEach(() => {
  FakeEventSource.instances = []
})

describe('useQuestion', () => {
  it('starts idle with no events', () => {
    const { result } = setup()
    expect(result.current.state.status).toBe('idle')
    expect(result.current.state.events).toHaveLength(0)
  })

  it('posts the question, then streams the run it was given', async () => {
    const fetchImpl = okFetch()
    const { result } = setup(fetchImpl)

    await ask(result)

    // EventSource can only GET, so the question goes over a POST and the
    // returned run path is what gets streamed.
    expect(fetchImpl).toHaveBeenCalledOnce()
    const [url, init] = (fetchImpl as unknown as ReturnType<typeof vi.fn>).mock.calls[0]
    expect(url).toBe('http://runtime.test/v1/questions')
    expect((init as RequestInit).method).toBe('POST')
    expect(JSON.parse((init as RequestInit).body as string)).toMatchObject({
      schema: 'question_request.v1',
      question: 'how many orders?',
      database: 'toy',
    })

    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1))
    expect(FakeEventSource.instances[0].url).toBe('http://runtime.test/v1/runs/r_abc/events')
  })

  it('folds trace events into state', async () => {
    const { result } = setup()
    await ask(result)
    const es = FakeEventSource.instances[0]

    act(() => {
      es.onopen?.call(es, new Event('open'))
      es.onmessage?.call(es, { data: frame({ type: 'executed', step: 1, row_count: 3 }) } as MessageEvent)
    })

    expect(result.current.state.status).toBe('open')
    expect(result.current.state.rowCount).toBe(3)
  })

  it('folds the named result and ledger frames into state', async () => {
    const { result } = setup()
    await ask(result)
    const es = FakeEventSource.instances[0]

    // These arrive on their own event names. onmessage never sees them, which
    // is why the client registers named listeners at all.
    act(() => {
      es.emit(
        'result_set',
        JSON.stringify({
          schema: 'result_set.v1',
          run_id: 'r_abc',
          columns: [{ name: 'cancelled' }],
          rows: [[2]],
          row_count: 1,
          truncated: false,
        }),
      )
      es.emit(
        'cost_ledger',
        JSON.stringify({
          schema: 'cost_ledger.v1',
          run_id: 'r_abc',
          price_table_date: '2026-08-03',
          entries: [],
          totals: { tokens_in: 400, tokens_out: 25, usd: 0.000525, ms: 900 },
        }),
      )
    })

    expect(result.current.state.result?.rows).toEqual([[2]])
    expect(result.current.state.rowCount).toBe(1)
    expect(result.current.state.ledger?.totals.tokens_in).toBe(400)
    expect(result.current.state.usd).toBeCloseTo(0.000525)
  })

  it('records an invalid frame as a warning without dropping the run', async () => {
    const { result } = setup()
    await ask(result)
    const es = FakeEventSource.instances[0]

    act(() => es.onmessage?.call(es, { data: '{not json' } as MessageEvent))

    expect(result.current.state.warnings).toHaveLength(1)
    expect(result.current.state.events).toHaveLength(0)
  })

  it('rejects an unusable question before making a request', async () => {
    const fetchImpl = okFetch()
    const { result } = setup(fetchImpl)

    await act(async () => {
      await result.current.ask('   ', 'toy')
    })

    // Client-side validation mirrors the server's rather than replacing it —
    // it turns a rejected request into an inline message instead of a round
    // trip.
    expect(fetchImpl).not.toHaveBeenCalled()
    expect(result.current.state.status).toBe('error')
    expect(result.current.state.warnings).toHaveLength(1)
  })

  it('surfaces a server rejection without opening a stream', async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify({ error: 'no such database' }), { status: 404 }),
    ) as unknown as typeof fetch
    const { result } = setup(fetchImpl)

    await act(async () => {
      await result.current.ask('q', 'toy')
    })

    expect(FakeEventSource.instances).toHaveLength(0)
    expect(result.current.state.status).toBe('error')
    expect(result.current.state.warnings[0]).toContain('no such database')
  })

  it('survives a network failure on the post', async () => {
    const fetchImpl = vi.fn(async () => {
      throw new Error('connection refused')
    }) as unknown as typeof fetch
    const { result } = setup(fetchImpl)

    await act(async () => {
      await result.current.ask('q', 'toy')
    })

    expect(result.current.state.status).toBe('error')
    expect(result.current.state.warnings[0]).toContain('connection refused')
  })

  it('refuses a response that carries no run to stream', async () => {
    const { result } = setup(okFetch({ nothing: true }))

    await act(async () => {
      await result.current.ask('q', 'toy')
    })

    expect(FakeEventSource.instances).toHaveLength(0)
    expect(result.current.state.status).toBe('error')
  })

  it('closes the stream on stop', async () => {
    const { result } = setup()
    await ask(result)
    act(() => result.current.stop())

    expect(FakeEventSource.instances[0].closed).toBe(true)
    expect(result.current.state.status).toBe('closed')
  })

  it('disposes the client on unmount', async () => {
    // dispose(), not close(): an unmounted component must not be reachable by
    // a late callback from a stream that is still winding down.
    const { result, unmount } = setup()
    await ask(result)
    const es = FakeEventSource.instances[0]

    unmount()
    expect(es.closed).toBe(true)
    expect(es.onmessage).toBeNull()
    expect(es.onerror).toBeNull()
    // The named listeners have to come off too — a result_set arriving after
    // teardown would render one run's rows under another run's question.
    expect(es.listeners.get('result_set') ?? []).toHaveLength(0)
    expect(es.listeners.get('cost_ledger') ?? []).toHaveLength(0)
  })
})
