import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  MAX_FRAME_CHARS,
  buildQuestionRequest,
  contractNames,
  parseEvent,
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
    // eval_result.v1 is produced by the Python harness and never reaches the
    // browser, so the web bundle does not carry its validator.
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
