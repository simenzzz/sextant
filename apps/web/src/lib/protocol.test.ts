import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  MAX_FRAME_CHARS,
  MAX_RESULT_FRAME_CHARS,
  buildQuestionRequest,
  contractNames,
  parseCostLedger,
  parseEvent,
  parseResultSet,
  validate,
} from './protocol'

// Anchored on the package root rather than import.meta.url: under the jsdom
// environment import.meta.url is an http:// URL, and fileURLToPath rejects it.
// Vitest runs with cwd set to this package (apps/web).
const FIXTURES = join(process.cwd(), '../../packages/contracts/fixtures')

/**
 * The same corpus the Go and Python suites run. A schema change without
 * matching fixtures fails all three at once instead of drifting quietly in
 * one language.
 */
describe('shared fixture corpus', () => {
  const contracts = readdirSync(FIXTURES, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => e.name)
    // Two contracts never reach the browser, so the web bundle does not carry
    // their validators: eval_result.v1 is produced by the Python harness, and
    // parse_summary.v1 travels only from the sidecar to the Go guard.
    .filter((name) => contractNames.includes(name))

  it('covers every contract the browser validates', () => {
    expect(contracts.sort()).toEqual([...contractNames].sort())
  })

  for (const contract of contracts) {
    for (const kind of ['valid', 'invalid'] as const) {
      const dir = join(FIXTURES, contract, kind)
      const files = readdirSync(dir).filter((f) => f.endsWith('.json'))

      it(`${contract}: ${kind}/ is not empty`, () => {
        expect(files.length).toBeGreaterThan(0)
      })

      for (const file of files) {
        it(`${contract}/${kind}/${file}`, () => {
          const doc = JSON.parse(readFileSync(join(dir, file), 'utf8'))
          const result = validate(contract, doc)
          expect(result.ok).toBe(kind === 'valid')
        })
      }
    }
  }
})

describe('parseEvent', () => {
  const validFrame = JSON.stringify({
    schema: 'trace_event.v1',
    type: 'run_started',
    step: 0,
    elapsed_ms: 0,
  })

  it('accepts a conforming frame', () => {
    const result = parseEvent(validFrame)
    expect(result.ok).toBe(true)
    if (result.ok) expect(result.value.type).toBe('run_started')
  })

  it('never throws on malformed JSON', () => {
    const result = parseEvent('{"schema":')
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('malformed JSON')
  })

  it('rejects a well-formed frame that violates the contract', () => {
    const result = parseEvent(JSON.stringify({ schema: 'trace_event.v1', type: 'thinking' }))
    expect(result.ok).toBe(false)
  })

  it('rejects an oversized frame before parsing it', () => {
    // The bound exists so a hostile server cannot make the client spend
    // unbounded time in JSON.parse.
    const result = parseEvent('x'.repeat(MAX_FRAME_CHARS + 1))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('exceeds')
  })
})

describe('parseResultSet', () => {
  const validFrame = JSON.stringify({
    schema: 'result_set.v1',
    run_id: 'r_1',
    columns: [{ name: 'n' }],
    rows: [[2]],
    row_count: 1,
    truncated: false,
  })

  it('accepts a conforming frame', () => {
    const result = parseResultSet(validFrame)
    expect(result.ok).toBe(true)
    if (result.ok) expect(result.value.rows).toEqual([[2]])
  })

  it('preserves null cells rather than coercing them', () => {
    const frame = JSON.stringify({
      schema: 'result_set.v1',
      run_id: 'r_1',
      columns: [{ name: 'a' }, { name: 'b' }],
      rows: [[null, 'x']],
      row_count: 1,
      truncated: false,
    })
    const result = parseResultSet(frame)
    expect(result.ok).toBe(true)
    // NULL is a real SQL answer. A validator that rejected it, or a client
    // that turned it into '', would report a different result than the query.
    if (result.ok) expect(result.value.rows[0][0]).toBeNull()
  })

  it('rejects a trace event sent on the result channel', () => {
    expect(parseResultSet(JSON.stringify({ schema: 'trace_event.v1' })).ok).toBe(false)
  })

  it('gets a larger ceiling than a trace-event frame, since it carries data', () => {
    // A result set legitimately carries rows, so the trace-event bound would
    // reject honest answers. It is still bounded, and the bound must be at
    // least the runtime's result byte ceiling or large results silently break.
    expect(MAX_RESULT_FRAME_CHARS).toBeGreaterThan(MAX_FRAME_CHARS)
    expect(MAX_RESULT_FRAME_CHARS).toBe(4 * 1024 * 1024)

    const result = parseResultSet('x'.repeat(MAX_RESULT_FRAME_CHARS + 1))
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('exceeds')
  })
})

describe('parseCostLedger', () => {
  it('accepts a ledger whose cost is unknown', () => {
    // A provider that closed without reporting usage leaves a step whose cost
    // is unknown, not zero. The contract has to be able to say so.
    const frame = JSON.stringify({
      schema: 'cost_ledger.v1',
      run_id: 'r_1',
      price_table_date: '2026-08-03',
      entries: [
        {
          step: 1,
          model: 'm',
          tokens_in: 0,
          tokens_out: 0,
          usd: 0,
          ms: 12,
          cost_known: false,
        },
      ],
      totals: { tokens_in: 0, tokens_out: 0, usd: 0, ms: 12, steps_cost_unknown: 1 },
    })
    const result = parseCostLedger(frame)
    expect(result.ok).toBe(true)
    if (result.ok) expect(result.value.totals.steps_cost_unknown).toBe(1)
  })

  it('rejects a malformed ledger without throwing', () => {
    expect(parseCostLedger('not json').ok).toBe(false)
  })
})

describe('buildQuestionRequest', () => {
  it('builds a valid minimal request', () => {
    const result = buildQuestionRequest('  How many orders?  ', 'demo')
    expect(result.ok).toBe(true)
    if (result.ok) {
      expect(result.value.question).toBe('How many orders?')
      expect(result.value.schema).toBe('question_request.v1')
    }
  })

  it('rejects an empty question client-side', () => {
    expect(buildQuestionRequest('   ', 'demo').ok).toBe(false)
  })

  it('rejects a database name that is not a slug', () => {
    // The runtime never interpolates this into SQL, but a name outside the
    // allowed shape means the client is about to ask for something that
    // cannot exist.
    expect(buildQuestionRequest('q', 'demo; DROP TABLE users').ok).toBe(false)
  })

  it('rejects a budget above the contract ceiling', () => {
    expect(buildQuestionRequest('q', 'demo', { budget_usd: 99 }).ok).toBe(false)
  })
})

describe('validate', () => {
  it('reports an unknown contract rather than throwing', () => {
    const result = validate('not_a_contract.v9', {})
    expect(result.ok).toBe(false)
    if (!result.ok) expect(result.error).toContain('unknown contract')
  })
})
