import { beforeEach, describe, expect, it, vi } from 'vitest'

import { createSseClient, type EventSourceCtor, type EventSourceLike, type SseStatus } from './sseClient'

/**
 * A controllable EventSource stand-in. jsdom ships no EventSource, so the
 * injectable constructor is not a convenience — it is the only way to test
 * this layer at all.
 */
class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = []

  onopen: ((this: unknown, ev: Event) => unknown) | null = null
  onmessage: ((this: unknown, ev: MessageEvent) => unknown) | null = null
  onerror: ((this: unknown, ev: Event) => unknown) | null = null
  closed = false

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this)
  }

  close() {
    this.closed = true
  }

  emitOpen() {
    this.onopen?.call(this, new Event('open'))
  }

  emitMessage(data: unknown) {
    this.onmessage?.call(this, { data } as MessageEvent)
  }

  emitError() {
    this.onerror?.call(this, new Event('error'))
  }
}

const Ctor = FakeEventSource as unknown as EventSourceCtor

const traceFrame = (overrides: Record<string, unknown> = {}) =>
  JSON.stringify({
    schema: 'trace_event.v1',
    type: 'run_started',
    step: 0,
    elapsed_ms: 0,
    ...overrides,
  })

beforeEach(() => {
  FakeEventSource.instances = []
})

describe('createSseClient', () => {
  it('opens against the configured base URL and reports status', () => {
    const statuses: SseStatus[] = []
    const client = createSseClient({
      baseUrl: 'http://runtime.test',
      onEvent: vi.fn(),
      onStatus: (s) => statuses.push(s),
      EventSourceImpl: Ctor,
    })

    client.start('/runs/abc/events')
    const es = FakeEventSource.instances[0]
    expect(es.url).toBe('http://runtime.test/runs/abc/events')
    expect(statuses).toEqual(['connecting'])

    es.emitOpen()
    expect(statuses).toEqual(['connecting', 'open'])
  })

  it('delivers validated events', () => {
    const onEvent = vi.fn()
    const client = createSseClient({ onEvent, EventSourceImpl: Ctor, baseUrl: 'http://x' })
    client.start('/e')

    FakeEventSource.instances[0].emitMessage(traceFrame({ type: 'executed', step: 3, row_count: 7 }))
    expect(onEvent).toHaveBeenCalledTimes(1)
    expect(onEvent.mock.calls[0][0]).toMatchObject({ type: 'executed', row_count: 7 })
  })

  it('reports an invalid frame without ending the stream', () => {
    const onEvent = vi.fn()
    const onInvalidFrame = vi.fn()
    const client = createSseClient({
      onEvent,
      onInvalidFrame,
      EventSourceImpl: Ctor,
      baseUrl: 'http://x',
    })
    client.start('/e')
    const es = FakeEventSource.instances[0]

    es.emitMessage('{not json')
    expect(onInvalidFrame).toHaveBeenCalledTimes(1)
    expect(onEvent).not.toHaveBeenCalled()

    // The stream stays usable: one bad frame degrades, it does not terminate.
    es.emitMessage(traceFrame())
    expect(onEvent).toHaveBeenCalledTimes(1)
  })

  it('rejects a non-string frame', () => {
    const onEvent = vi.fn()
    const onInvalidFrame = vi.fn()
    const client = createSseClient({
      onEvent,
      onInvalidFrame,
      EventSourceImpl: Ctor,
      baseUrl: 'http://x',
    })
    client.start('/e')

    FakeEventSource.instances[0].emitMessage({ notAString: true })
    expect(onInvalidFrame).toHaveBeenCalledWith('received a non-string frame')
    expect(onEvent).not.toHaveBeenCalled()
  })

  it('closes the previous stream when a new run starts', () => {
    const client = createSseClient({ onEvent: vi.fn(), EventSourceImpl: Ctor, baseUrl: 'http://x' })
    client.start('/first')
    client.start('/second')

    expect(FakeEventSource.instances).toHaveLength(2)
    expect(FakeEventSource.instances[0].closed).toBe(true)
    expect(FakeEventSource.instances[1].closed).toBe(false)
  })

  /**
   * The reason dispose() detaches handlers instead of only calling close():
   * an EventSource can fire onerror after close, and a late callback from a
   * finished run would otherwise dispatch into whatever came next.
   */
  it('ignores callbacks that arrive after dispose', () => {
    const onEvent = vi.fn()
    const onStatus = vi.fn()
    const client = createSseClient({
      onEvent,
      onStatus,
      EventSourceImpl: Ctor,
      baseUrl: 'http://x',
    })
    client.start('/e')
    const es = FakeEventSource.instances[0]

    client.dispose()
    onStatus.mockClear()

    es.emitMessage(traceFrame())
    es.emitError()

    expect(onEvent).not.toHaveBeenCalled()
    expect(onStatus).not.toHaveBeenCalled()
    expect(es.closed).toBe(true)
  })

  it('does nothing when start is called after dispose', () => {
    const client = createSseClient({ onEvent: vi.fn(), EventSourceImpl: Ctor, baseUrl: 'http://x' })
    client.dispose()
    client.start('/e')
    expect(FakeEventSource.instances).toHaveLength(0)
  })

  it('reports an error status when the connection fails', () => {
    const statuses: SseStatus[] = []
    const client = createSseClient({
      onEvent: vi.fn(),
      onStatus: (s) => statuses.push(s),
      EventSourceImpl: Ctor,
      baseUrl: 'http://x',
    })
    client.start('/e')
    FakeEventSource.instances[0].emitError()
    expect(statuses).toContain('error')
  })

  it('refuses a run path that could rewrite the origin', () => {
    const onInvalidFrame = vi.fn()
    const client = createSseClient({
      onEvent: vi.fn(),
      onInvalidFrame,
      EventSourceImpl: Ctor,
      baseUrl: 'http://runtime.test',
    })

    client.start('@evil.example/steal')

    expect(FakeEventSource.instances).toHaveLength(0)
    expect(onInvalidFrame).toHaveBeenCalledWith(expect.stringContaining('must start with'))
  })

  it('reports closed after close()', () => {
    const statuses: SseStatus[] = []
    const client = createSseClient({
      onEvent: vi.fn(),
      onStatus: (s) => statuses.push(s),
      EventSourceImpl: Ctor,
      baseUrl: 'http://x',
    })
    client.start('/e')
    client.close()
    expect(statuses).toContain('closed')
    expect(FakeEventSource.instances[0].closed).toBe(true)
  })
})
