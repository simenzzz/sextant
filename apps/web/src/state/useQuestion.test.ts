import { act, renderHook } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'

import type { EventSourceCtor, EventSourceLike } from '../lib/sseClient'
import { useQuestion } from './useQuestion'

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

const setup = () =>
  renderHook(() => useQuestion({ baseUrl: 'http://runtime.test', EventSourceImpl: Ctor }))

beforeEach(() => {
  FakeEventSource.instances = []
})

describe('useQuestion', () => {
  it('starts idle with no events', () => {
    const { result } = setup()
    expect(result.current.state.status).toBe('idle')
    expect(result.current.state.events).toHaveLength(0)
  })

  it('opens a stream and folds events into state', () => {
    const { result } = setup()

    act(() => result.current.ask('/runs/abc/events'))
    expect(result.current.state.status).toBe('connecting')

    const es = FakeEventSource.instances[0]
    expect(es.url).toBe('http://runtime.test/runs/abc/events')

    act(() => {
      es.onopen?.call(es, new Event('open'))
      es.onmessage?.call(es, { data: frame({ type: 'executed', step: 1, row_count: 3 }) } as MessageEvent)
    })

    expect(result.current.state.status).toBe('open')
    expect(result.current.state.rowCount).toBe(3)
  })

  it('records an invalid frame as a warning without dropping the run', () => {
    const { result } = setup()
    act(() => result.current.ask('/e'))
    const es = FakeEventSource.instances[0]

    act(() => es.onmessage?.call(es, { data: '{not json' } as MessageEvent))

    expect(result.current.state.warnings).toHaveLength(1)
    expect(result.current.state.events).toHaveLength(0)
  })

  it('closes the stream on stop', () => {
    const { result } = setup()
    act(() => result.current.ask('/e'))
    act(() => result.current.stop())

    expect(FakeEventSource.instances[0].closed).toBe(true)
    expect(result.current.state.status).toBe('closed')
  })

  it('disposes the client on unmount', () => {
    // dispose(), not close(): an unmounted component must not be reachable by
    // a late callback from a stream that is still winding down.
    const { result, unmount } = setup()
    act(() => result.current.ask('/e'))
    const es = FakeEventSource.instances[0]

    unmount()
    expect(es.closed).toBe(true)
    expect(es.onmessage).toBeNull()
    expect(es.onerror).toBeNull()
  })
})
