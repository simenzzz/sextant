/**
 * Boundary validation for everything arriving from the agent runtime.
 *
 * The schemas here are the same files the Go and Python services embed —
 * copied verbatim by packages/contracts/codegen and held in place by the CI
 * drift gate — so all three sides agree on what a valid frame is. The types
 * come from the same schemas, which is why none of them are hand-written.
 */
import { Ajv2020, type ValidateFunction } from 'ajv/dist/2020'
import addFormats from 'ajv-formats'

import costLedgerV1 from '../contracts/gen/schemas/cost_ledger.v1.schema.json'
import questionRequestV1 from '../contracts/gen/schemas/question_request.v1.schema.json'
import resultSetV1 from '../contracts/gen/schemas/result_set.v1.schema.json'
import sqlPlanV1 from '../contracts/gen/schemas/sql_plan.v1.schema.json'
import traceEventV1 from '../contracts/gen/schemas/trace_event.v1.schema.json'
import type { CostLedgerV1 } from '../contracts/gen/cost_ledger.v1'
import type { QuestionRequestV1 } from '../contracts/gen/question_request.v1'
import type { ResultSetV1 } from '../contracts/gen/result_set.v1'
import type { TraceEventV1 } from '../contracts/gen/trace_event.v1'

export type { CostLedgerV1, QuestionRequestV1, ResultSetV1, TraceEventV1 }

/**
 * A single SSE frame larger than this is treated as hostile rather than
 * parsed. JSON.parse on an unbounded string is a denial-of-service the client
 * inflicts on itself.
 *
 * Applies to trace-event and cost-ledger frames, which are small by
 * construction — a trace event is one state transition and a ledger is bounded
 * at 256 entries.
 */
export const MAX_FRAME_CHARS = 256 * 1024

/**
 * The result-set frame gets its own, larger ceiling.
 *
 * A result set legitimately carries data — up to the plan's row limit — so the
 * trace-event ceiling would reject honest answers. It is NOT unbounded: the
 * runtime enforces a byte budget on the frame it emits (SEXTANT_MAX_RESULT_BYTES,
 * default 1 MiB, ceiling 4 MiB) and truncates rows to fit, setting `truncated`.
 * This constant is that ceiling, so the largest frame the runtime can legally
 * emit is always admissible here and anything above it did not come from a
 * conforming server. Raising one without the other silently breaks large
 * results — apps/runtime-go/internal/config asserts the same number.
 */
export const MAX_RESULT_FRAME_CHARS = 4 * 1024 * 1024

/**
 * Two instances on purpose.
 *
 * `allErrors: true` disables Ajv's short-circuit evaluation, so a document
 * crafted to fail many subschemas costs proportionally more to validate —
 * Ajv's own guidance is not to use it on untrusted input. Inbound frames off
 * the wire are exactly that, so they get the short-circuiting instance.
 * Outbound requests we built ourselves get the full error list, where it is
 * a developer-facing convenience rather than an attack surface.
 */
const inboundAjv = new Ajv2020({ strict: true })
addFormats(inboundAjv)

const outboundAjv = new Ajv2020({ strict: true, allErrors: true })
addFormats(outboundAjv)

const validators: Record<string, ValidateFunction> = {
  'cost_ledger.v1': inboundAjv.compile(costLedgerV1),
  'result_set.v1': inboundAjv.compile(resultSetV1),
  'sql_plan.v1': inboundAjv.compile(sqlPlanV1),
  'trace_event.v1': inboundAjv.compile(traceEventV1),
  'question_request.v1': outboundAjv.compile(questionRequestV1),
}

/** The set of contracts this build can validate. */
export const contractNames = Object.keys(validators)

export type ParseResult<T> = { ok: true; value: T } | { ok: false; error: string }

function describeErrors(validate: ValidateFunction): string {
  return (
    validate.errors
      ?.map((e) => `${e.instancePath || '/'} ${e.message ?? 'is invalid'}`)
      .join('; ') ?? 'failed validation'
  )
}

/** Validates an already-parsed document against a named contract. */
export function validate<T>(contract: string, doc: unknown): ParseResult<T> {
  const validator = validators[contract]
  if (!validator) {
    return { ok: false, error: `unknown contract ${contract}` }
  }
  if (!validator(doc)) {
    return { ok: false, error: `${contract}: ${describeErrors(validator)}` }
  }
  return { ok: true, value: doc as T }
}

/**
 * Parses one raw SSE frame against a named contract, under a size ceiling.
 *
 * Never throws. A malformed frame is a routine event on a network boundary,
 * not an exception: the caller shows the connection as degraded and carries
 * on, rather than tearing down a React tree over one bad line.
 *
 * The length check precedes JSON.parse rather than following it — parsing an
 * unbounded string to find out how big it is defeats the point of the limit.
 */
function parseFrame<T>(raw: string, contract: string, maxChars: number): ParseResult<T> {
  if (raw.length > maxChars) {
    return { ok: false, error: `${contract} frame exceeds ${maxChars} characters` }
  }

  let doc: unknown
  try {
    doc = JSON.parse(raw)
  } catch (err) {
    return { ok: false, error: `malformed JSON: ${(err as Error).message}` }
  }
  return validate<T>(contract, doc)
}

/** Parses one default-named SSE frame into a trace event. */
export function parseEvent(raw: string): ParseResult<TraceEventV1> {
  return parseFrame<TraceEventV1>(raw, 'trace_event.v1', MAX_FRAME_CHARS)
}

/** Parses one `result_set` SSE frame — the rows the run actually returned. */
export function parseResultSet(raw: string): ParseResult<ResultSetV1> {
  return parseFrame<ResultSetV1>(raw, 'result_set.v1', MAX_RESULT_FRAME_CHARS)
}

/** Parses one `cost_ledger` SSE frame — what the run spent, itemized. */
export function parseCostLedger(raw: string): ParseResult<CostLedgerV1> {
  return parseFrame<CostLedgerV1>(raw, 'cost_ledger.v1', MAX_FRAME_CHARS)
}

/**
 * Builds a question request and validates it before it leaves the browser.
 *
 * Client-side validation mirrors the server's rather than replacing it: the
 * server revalidates everything. Doing it here turns a rejected request into
 * an immediate inline message instead of a round trip.
 */
export function buildQuestionRequest(
  question: string,
  database: string,
  options: Partial<Omit<QuestionRequestV1, 'schema' | 'question' | 'database'>> = {},
): ParseResult<QuestionRequestV1> {
  const candidate = {
    schema: 'question_request.v1',
    question: question.trim(),
    database,
    ...options,
  }
  return validate<QuestionRequestV1>('question_request.v1', candidate)
}
