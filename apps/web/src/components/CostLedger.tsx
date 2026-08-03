import type { CostLedgerV1 } from '../lib/protocol'

export interface CostLedgerPanelProps {
  ledger: CostLedgerV1 | null
  /** The running total from the trace, shown before the ledger arrives. */
  runningUSD: number
}

/** Dollars at a precision that does not round a real charge to zero. */
function usd(value: number): string {
  return `$${value.toFixed(6)}`
}

export function CostLedgerPanel({ ledger, runningUSD }: CostLedgerPanelProps) {
  if (!ledger) {
    // Before the ledger frame arrives, the trace's per-step dollars are the
    // only figure there is. Labelled as running so it is not mistaken for the
    // final accounting.
    return (
      <section aria-label="Cost">
        <h2>Cost</h2>
        <p>Running: {usd(runningUSD)}</p>
      </section>
    )
  }

  const { totals, entries } = ledger
  const unknown = totals.steps_cost_unknown ?? 0

  return (
    <section aria-label="Cost">
      <h2>Cost</h2>

      <p>
        <strong>{usd(totals.usd)}</strong>
        {unknown > 0 ? (
          // The invariant made visible: a step whose usage the provider never
          // reported has an unknown cost, not a zero one, so the total is a
          // floor. Anything deriving cost per correct answer has to say so.
          <> — at least, {unknown} step{unknown === 1 ? '' : 's'} of unknown cost</>
        ) : null}
      </p>
      <p>
        {totals.tokens_in} in / {totals.tokens_out} out · {totals.ms} ms ·{' '}
        {totals.provider_calls ?? 0} provider call{(totals.provider_calls ?? 0) === 1 ? '' : 's'}
        {(totals.cache_hits ?? 0) > 0 ? <> · {totals.cache_hits} cache hits</> : null}
      </p>

      {entries.length > 0 && (
        <table>
          <thead>
            <tr>
              <th scope="col">Step</th>
              <th scope="col">Model</th>
              <th scope="col">In</th>
              <th scope="col">Out</th>
              <th scope="col">Cost</th>
              <th scope="col">ms</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.step}>
                <td>{e.step}</td>
                <td>{e.model}</td>
                <td>{e.tokens_in}</td>
                <td>{e.tokens_out}</td>
                <td>{e.cost_known === false ? 'unknown' : usd(e.usd)}</td>
                <td>{e.ms}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <p className="footnote">Prices checked {ledger.price_table_date}</p>
    </section>
  )
}
