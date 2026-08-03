import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { CostLedgerV1, ResultSetV1, TraceEventV1 } from '../lib/protocol'
import { CostLedgerPanel } from './CostLedger'
import { ResultTable } from './ResultTable'
import { SqlPanel } from './SqlPanel'
import { TraceTimeline } from './TraceTimeline'

const result = (over: Partial<ResultSetV1> = {}): ResultSetV1 => ({
  schema: 'result_set.v1',
  run_id: 'r_1',
  columns: [{ name: 'name' }, { name: 'total' }],
  rows: [['Amal Haddad', 129]],
  row_count: 1,
  truncated: false,
  ...over,
})

const ledger = (over: Partial<CostLedgerV1> = {}): CostLedgerV1 => ({
  schema: 'cost_ledger.v1',
  run_id: 'r_1',
  price_table_date: '2026-08-03',
  entries: [
    { step: 1, model: 'claude-haiku-4-5', tokens_in: 400, tokens_out: 25, usd: 0.000525, ms: 900 },
  ],
  totals: { tokens_in: 400, tokens_out: 25, usd: 0.000525, ms: 900, provider_calls: 1 },
  ...over,
})

const event = (over: Partial<TraceEventV1> = {}): TraceEventV1 => ({
  schema: 'trace_event.v1',
  type: 'run_started',
  step: 0,
  elapsed_ms: 0,
  ...over,
})

describe('ResultTable', () => {
  it('renders columns and rows', () => {
    render(<ResultTable result={result()} />)
    expect(screen.getByRole('columnheader', { name: /name/ })).toBeInTheDocument()
    expect(screen.getByRole('cell', { name: 'Amal Haddad' })).toBeInTheDocument()
    expect(screen.getByText('1 rows')).toBeInTheDocument()
  })

  it('marks a NULL rather than rendering it as an empty cell', () => {
    // An empty string and a NULL are different answers. A table that shows
    // them identically misreports the query.
    render(<ResultTable result={result({ rows: [[null, 1]] })} />)
    expect(screen.getByText('NULL')).toBeInTheDocument()
  })

  it('says an empty result is an empty result, not a failure', () => {
    render(<ResultTable result={result({ rows: [], row_count: 0 })} />)
    expect(screen.getByText(/matched no rows/)).toBeInTheDocument()
  })

  it('says how many rows were withheld when the frame was truncated', () => {
    // Presenting a partial table as complete would misreport the answer.
    render(<ResultTable result={result({ row_count: 500, truncated: true })} />)
    expect(screen.getByText(/500 rows, showing the first 1/)).toBeInTheDocument()
  })

  it('renders nothing before a result arrives', () => {
    const { container } = render(<ResultTable result={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('CostLedgerPanel', () => {
  it('shows the running total before the ledger arrives', () => {
    render(<CostLedgerPanel ledger={null} runningUSD={0.0004} />)
    expect(screen.getByText(/Running/)).toBeInTheDocument()
  })

  it('shows the itemized ledger once it arrives', () => {
    render(<CostLedgerPanel ledger={ledger()} runningUSD={0} />)
    // The total and the sole entry carry the same figure here, so target the
    // total by its element rather than by text.
    expect(screen.getAllByText('$0.000525')).toHaveLength(2)
    expect(document.querySelector('strong')?.textContent).toBe('$0.000525')
    expect(screen.getByRole('cell', { name: 'claude-haiku-4-5' })).toBeInTheDocument()
    expect(screen.getByText(/Prices checked 2026-08-03/)).toBeInTheDocument()
  })

  it('says the total is a floor when a step cost is unknown', () => {
    // The invariant made visible: a step whose usage was never reported has an
    // unknown cost, not a zero one.
    render(
      <CostLedgerPanel
        ledger={ledger({
          entries: [
            { step: 1, model: 'm', tokens_in: 0, tokens_out: 0, usd: 0, ms: 12, cost_known: false },
          ],
          totals: { tokens_in: 0, tokens_out: 0, usd: 0, ms: 12, steps_cost_unknown: 1 },
        })}
        runningUSD={0}
      />,
    )
    expect(screen.getByText(/at least/)).toBeInTheDocument()
    expect(screen.getByRole('cell', { name: 'unknown' })).toBeInTheDocument()
  })
})

describe('SqlPanel', () => {
  it('renders the statement and the tables in scope', () => {
    render(<SqlPanel sql="SELECT 1 FROM orders LIMIT 500" tables={['orders']} />)
    expect(screen.getByText('SELECT 1 FROM orders LIMIT 500')).toBeInTheDocument()
    expect(screen.getByText(/Tables in scope: orders/)).toBeInTheDocument()
  })

  it('renders nothing before there is SQL', () => {
    const { container } = render(<SqlPanel sql={null} tables={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('TraceTimeline', () => {
  it('labels transitions in words', () => {
    render(
      <TraceTimeline
        events={[event(), event({ type: 'executed', step: 1, elapsed_ms: 40, row_count: 3 })]}
        outcome={null}
      />,
    )
    expect(screen.getByText('Started')).toBeInTheDocument()
    expect(screen.getByText('Query returned')).toBeInTheDocument()
  })

  it('renders a cap as a recorded outcome rather than a failure', () => {
    // A UI that paints a deliberate abstention or a cap in red argues against
    // the thing the system is for.
    for (const outcome of ['abstained', 'budget_exhausted', 'depth_exhausted', 'deadline_exceeded'] as const) {
      const { unmount } = render(<TraceTimeline events={[event()]} outcome={outcome} />)
      const el = document.querySelector(`[data-outcome="${outcome}"]`)
      expect(el).not.toBeNull()
      expect(el?.getAttribute('data-failure')).toBe('false')
      unmount()
    }
  })

  it('marks a genuine error as a failure', () => {
    render(<TraceTimeline events={[event()]} outcome="error" />)
    expect(document.querySelector('[data-outcome="error"]')?.getAttribute('data-failure')).toBe('true')
  })

  it('renders nothing before the run produces anything', () => {
    const { container } = render(<TraceTimeline events={[]} outcome={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})
