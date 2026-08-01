import { describe, expect, it } from 'vitest'

import type { TraceEventV1 } from '../lib/protocol'
import {
  MAX_EVENTS,
  MAX_WARNINGS,
  initialState,
  questionReducer,
  TERMINAL_TYPES,
} from './questionReducer'

const ev = (overrides: Partial<TraceEventV1> & Pick<TraceEventV1, 'type'>): TraceEventV1 =>
  ({
    schema: 'trace_event.v1',
    step: 0,
    elapsed_ms: 0,
    ...overrides,
  }) as TraceEventV1

const apply = (events: TraceEventV1[]) =>
  events.reduce((s, event) => questionReducer(s, { type: 'event_received', event }), initialState)

describe('questionReducer', () => {
  it('starts a run from a clean slate', () => {
    const dirty = apply([ev({ type: 'executed', row_count: 9 })])
    const next = questionReducer(dirty, { type: 'run_requested' })

    // Leftover results from the previous question next to a new question is
    // worse than showing nothing.
    expect(next.events).toHaveLength(0)
    expect(next.rowCount).toBeNull()
    expect(next.usd).toBe(0)
    expect(next.status).toBe('connecting')
  })

  it('accumulates events in arrival order without mutating prior state', () => {
    const first = questionReducer(initialState, {
      type: 'event_received',
      event: ev({ type: 'run_started' }),
    })
    const second = questionReducer(first, {
      type: 'event_received',
      event: ev({ type: 'retrieved', step: 1, tables: ['orders', 'customers'] }),
    })

    expect(first.events).toHaveLength(1)
    expect(second.events).toHaveLength(2)
    expect(second.tables).toEqual(['orders', 'customers'])
    // The earlier state object is untouched.
    expect(first.tables).toEqual([])
  })

  it('prefers the validated statement over the raw generation', () => {
    const state = apply([
      ev({ type: 'generated', step: 1, sql: 'SELECT * FROM orders' }),
      ev({ type: 'validated', step: 2, sql: 'SELECT * FROM orders LIMIT 500' }),
    ])
    // The SQL panel must never show a statement that did not actually run.
    expect(state.sql).toBe('SELECT * FROM orders LIMIT 500')
  })

  it('shows the current attempt during a repair loop, not the first', () => {
    const state = apply([
      ev({ type: 'generated', step: 1, sql: 'SELECT * FROM order' }),
      ev({ type: 'rejected', step: 2, failure_kind: 'unknown_table' }),
      ev({ type: 'repairing', step: 3, repair_depth: 1 }),
      ev({ type: 'generated', step: 4, sql: 'SELECT * FROM orders' }),
    ])
    expect(state.sql).toBe('SELECT * FROM orders')
  })

  // A looping or hostile runtime can emit valid frames at line rate; each one
  // is retained for the life of the run and re-renders the tree.
  it('stops accumulating events at the cap', () => {
    const flood = Array.from({ length: MAX_EVENTS + 50 }, (_, i) =>
      ev({ type: 'generating', step: i, delta: 'x' }),
    )
    const state = apply(flood)
    expect(state.events).toHaveLength(MAX_EVENTS)
  })

  it('stops accumulating warnings at the cap', () => {
    let state = initialState
    for (let i = 0; i < MAX_WARNINGS + 20; i++) {
      state = questionReducer(state, { type: 'invalid_frame', error: `bad frame ${i}` })
    }
    expect(state.warnings).toHaveLength(MAX_WARNINGS)
  })

  it('sums cost across steps', () => {
    const state = apply([
      ev({ type: 'generated', step: 1, usd: 0.002 }),
      ev({ type: 'escalated', step: 2, usd: 0.01 }),
      ev({ type: 'executed', step: 3 }),
    ])
    expect(state.usd).toBeCloseTo(0.012, 6)
  })

  it.each(TERMINAL_TYPES)('treats %s as a terminal outcome', (type) => {
    const state = apply([ev({ type })])
    expect(state.outcome).toBe(type)
  })

  it('does not set an outcome for a mid-run event', () => {
    expect(apply([ev({ type: 'repairing', repair_depth: 1 })]).outcome).toBeNull()
  })

  it('records an abstention as an outcome rather than an error', () => {
    const state = apply([ev({ type: 'abstained' })])
    // The project's thesis is that abstaining beats answering wrongly, so the
    // UI must not render this as a failure.
    expect(state.outcome).toBe('abstained')
    expect(state.warnings).toEqual([])
  })

  it('collects invalid-frame warnings without discarding events', () => {
    const withEvent = questionReducer(initialState, {
      type: 'event_received',
      event: ev({ type: 'run_started' }),
    })
    const next = questionReducer(withEvent, { type: 'invalid_frame', error: 'malformed JSON' })

    expect(next.warnings).toEqual(['malformed JSON'])
    expect(next.events).toHaveLength(1)
  })

  it('resets to the initial state', () => {
    const dirty = apply([ev({ type: 'answered', usd: 0.5 })])
    expect(questionReducer(dirty, { type: 'reset' })).toEqual(initialState)
  })

  it('tracks connection status independently of events', () => {
    const next = questionReducer(initialState, { type: 'status_changed', status: 'open' })
    expect(next.status).toBe('open')
    expect(next.events).toHaveLength(0)
  })
})
