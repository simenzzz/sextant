import { CostLedgerPanel } from './components/CostLedger'
import { HealthBadge } from './components/HealthBadge'
import { QuestionBox } from './components/QuestionBox'
import { ResultTable } from './components/ResultTable'
import { SqlPanel } from './components/SqlPanel'
import { TraceTimeline } from './components/TraceTimeline'
import { useQuestion } from './state/useQuestion'

/**
 * The databases the runtime is configured for.
 *
 * Hard-coded at P1 because there is exactly one, and the runtime does not
 * expose a listing endpoint. When a second corpus arrives this should come
 * from the server rather than being maintained in two places — noted here
 * rather than solved now, since a discovery endpoint for a single fixed entry
 * is machinery without a purpose.
 */
const DATABASES = ['toy'] as const

export function App() {
  const { state, ask, stop } = useQuestion()
  const busy = state.status === 'connecting' || state.status === 'open'

  return (
    <main>
      <h1>Sextant</h1>
      <p>Ask a database questions in plain English.</p>
      <HealthBadge />

      <QuestionBox
        onAsk={(question, database) => void ask(question, database)}
        onStop={stop}
        busy={busy}
        databases={DATABASES}
      />

      {state.warnings.length > 0 && (
        <section aria-label="Warnings">
          <ul>
            {state.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </section>
      )}

      <TraceTimeline events={state.events} outcome={state.outcome} />
      <SqlPanel sql={state.sql} tables={state.tables} />
      <ResultTable result={state.result} />
      <CostLedgerPanel ledger={state.ledger} runningUSD={state.usd} />
    </main>
  )
}
