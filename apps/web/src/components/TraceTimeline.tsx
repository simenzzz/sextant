import type { TraceEventV1 } from '../lib/protocol'
import { isFailure, type TerminalType } from '../state/questionReducer'

export interface TraceTimelineProps {
  events: readonly TraceEventV1[]
  outcome: TerminalType | null
}

/**
 * How each transition reads to a person.
 *
 * The three cap outcomes and `abstained` are phrased as results, not failures —
 * "spent its budget", not "budget error". The whole thesis is that abstaining
 * beats answering wrongly, and a UI that renders a deliberate abstention in red
 * argues against the thing the system is for.
 */
const LABELS: Record<string, string> = {
  run_started: 'Started',
  retrieved: 'Selected schema',
  generating: 'Writing SQL',
  generated: 'Wrote SQL',
  validated: 'Cleared by the guard',
  rejected: 'Refused by the guard',
  executing: 'Running the query',
  executed: 'Query returned',
  observed: 'Read the result',
  repairing: 'Repairing',
  escalated: 'Escalated to a stronger model',
  abstained: 'Abstained rather than answer unreliably',
  budget_exhausted: 'Stopped: spent its budget',
  depth_exhausted: 'Stopped: out of repair attempts',
  deadline_exceeded: 'Stopped: out of time',
  answered: 'Answered',
  error: 'Failed',
}

export function TraceTimeline({ events, outcome }: TraceTimelineProps) {
  if (events.length === 0) return null

  return (
    <section aria-label="Trace">
      <h2>Trace</h2>
      <ol>
        {events.map((ev) => (
          // step is the monotonic index the protocol carries; it is the only
          // stable identity an event has.
          <li key={ev.step} data-type={ev.type}>
            <span className="elapsed">{ev.elapsed_ms} ms</span>{' '}
            <span className="label">{LABELS[ev.type] ?? ev.type}</span>
            {ev.model ? <span className="model"> · {ev.model}</span> : null}
            {ev.tables?.length ? <span className="tables"> · {ev.tables.join(', ')}</span> : null}
            {ev.row_count !== undefined ? <span className="rows"> · {ev.row_count} rows</span> : null}
            {ev.failure_kind ? <span className="kind"> · {ev.failure_kind}</span> : null}
            {ev.message ? <span className="message"> — {ev.message}</span> : null}
          </li>
        ))}
      </ol>

      {outcome && (
        // data-failure distinguishes the one genuine failure from the five
        // recorded outcomes, so styling cannot accidentally paint a cap red.
        <p data-outcome={outcome} data-failure={isFailure(outcome)}>
          {LABELS[outcome] ?? outcome}
        </p>
      )}
    </section>
  )
}
