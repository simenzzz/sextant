/**
 * The run state machine, as a pure reducer.
 *
 * Pure and framework-free on purpose: the interesting behaviour — what a
 * repair does to the timeline, which outcomes are terminal — is testable by
 * feeding it events, with no DOM and no network. Every transition returns a
 * new state; nothing here mutates.
 */
import type { SseStatus } from '../lib/sseClient'
import type { CostLedgerV1, ResultSetV1, TraceEventV1 } from '../lib/protocol'

/**
 * Outcomes that end a run. `abstained` and the three cap outcomes are
 * recorded results, not errors — the whole thesis is that abstaining beats
 * answering wrongly, so the UI must not render them as failures.
 */
export const TERMINAL_TYPES = [
  'answered',
  'abstained',
  'budget_exhausted',
  'depth_exhausted',
  'deadline_exceeded',
  'error',
] as const

export type TerminalType = (typeof TERMINAL_TYPES)[number]

/**
 * The outcomes that are results rather than failures.
 *
 * Exported because more than one component needs the distinction and each
 * deciding for itself is how "a cap is not an error" quietly stops being true
 * in one corner of the UI.
 */
export const RECORDED_OUTCOMES: readonly TerminalType[] = [
  'answered',
  'abstained',
  'budget_exhausted',
  'depth_exhausted',
  'deadline_exceeded',
]

export function isFailure(outcome: TerminalType | null): boolean {
  return outcome === 'error'
}

/**
 * Caps on state a remote server drives.
 *
 * `MAX_FRAME_CHARS` bounds one frame; nothing bounded how many. A runtime that
 * loops — hostile or merely buggy — can emit valid events at line rate, each
 * retained for the life of the run and each triggering a re-render, until the
 * tab dies. A run is bounded by construction (repair depth <= 10, k <= 16), so
 * these ceilings are far above anything legitimate.
 */
export const MAX_EVENTS = 2000
export const MAX_WARNINGS = 50

export interface QuestionState {
  readonly status: SseStatus
  readonly events: readonly TraceEventV1[]
  readonly sql: string | null
  readonly tables: readonly string[]
  readonly rowCount: number | null
  readonly usd: number
  readonly outcome: TerminalType | null
  readonly warnings: readonly string[]
  /** The rows the run returned, once its result frame arrives. */
  readonly result: ResultSetV1 | null
  /** What the run spent, itemized. Arrives on every run, capped or not. */
  readonly ledger: CostLedgerV1 | null
}

export const initialState: QuestionState = {
  status: 'idle',
  events: [],
  sql: null,
  tables: [],
  rowCount: null,
  usd: 0,
  outcome: null,
  warnings: [],
  result: null,
  ledger: null,
}

export type Action =
  | { type: 'run_requested' }
  | { type: 'status_changed'; status: SseStatus }
  | { type: 'event_received'; event: TraceEventV1 }
  | { type: 'result_received'; result: ResultSetV1 }
  | { type: 'ledger_received'; ledger: CostLedgerV1 }
  | { type: 'invalid_frame'; error: string }
  | { type: 'reset' }

function isTerminal(t: TraceEventV1['type']): t is TerminalType {
  return (TERMINAL_TYPES as readonly string[]).includes(t)
}

export function questionReducer(state: QuestionState, action: Action): QuestionState {
  switch (action.type) {
    case 'run_requested':
      // A new run starts from a clean slate rather than layering onto the
      // last one; leftover SQL from a previous question next to a new
      // question's results is worse than showing nothing.
      return { ...initialState, status: 'connecting' }

    case 'status_changed':
      // Once a run has reached a terminal event it is over, and the transport
      // closing is expected rather than a fault. EventSource reports a normal
      // server close as `onerror`, so without this an answered run would end
      // up rendered as an error.
      if (state.outcome !== null && action.status === 'error') {
        return { ...state, status: 'closed' }
      }
      return { ...state, status: action.status }

    case 'event_received': {
      const ev = action.event
      return {
        ...state,
        events:
          state.events.length >= MAX_EVENTS ? state.events : [...state.events, ev],
        // `validated` carries the statement actually cleared for execution;
        // `generated` carries the raw generation. A validated statement always
        // wins, and a later generation replaces an earlier one — during a
        // repair loop the panel must show the current attempt, not attempt #1.
        sql:
          ev.type === 'validated' || ev.type === 'generated'
            ? (ev.sql ?? state.sql)
            : state.sql,
        tables: ev.type === 'retrieved' && ev.tables ? [...ev.tables] : state.tables,
        rowCount: ev.type === 'executed' && ev.row_count !== undefined ? ev.row_count : state.rowCount,
        usd: state.usd + (ev.usd ?? 0),
        // The FIRST terminal type wins. A stream that somehow carries two is
        // contradicting itself, and the earlier one is the one the run
        // actually reached.
        outcome: state.outcome ?? (isTerminal(ev.type) ? ev.type : null),
      }
    }

    case 'result_received':
      return { ...state, result: action.result, rowCount: action.result.row_count }

    case 'ledger_received':
      // The ledger's total replaces the running sum from the trace: the trace
      // carries per-step dollars for live feedback, the ledger is the
      // authoritative accounting, and they can differ when a step's cost was
      // unknown.
      return { ...state, ledger: action.ledger, usd: action.ledger.totals.usd }

    case 'invalid_frame':
      if (state.warnings.length >= MAX_WARNINGS) return state
      return { ...state, warnings: [...state.warnings, action.error] }

    case 'reset':
      return initialState

    default: {
      // Exhaustiveness: adding an action without handling it fails the build.
      const unreachable: never = action
      return unreachable
    }
  }
}
