# Sextant — an analyst agent, built to be measured

> **Status:** approved plan, implementation not started (2026-07-29).
> This document is self-contained. A fresh session should be able to start from
> this file alone. Read it top to bottom before writing code.

---

## 1. What this is

An agent that answers natural-language questions over a real database:

```
question → retrieve the relevant schema subset → write SQL → validate it at the
AST level → execute read-only → observe the result or the error → repair →
escalate to a stronger model if still uncertain → abstain if uncertain after
that → render a chart
```

…with every token, dollar, and millisecond accounted for, and the whole thing
measured against the public **BIRD** text-to-SQL benchmark.

Working name **Sextant** — an instrument of measurement, which is the point.
Alternates if it doesn't stick: *Assay*, *Plumb*, *Quarry*.

## 2. Why this project

Sami is targeting AI engineer roles (agent/tooling platform, LLM product, AI
infra/inference), applying in ~4–6 months, ~20 hrs/week, optimizing for recruiter
signal. Positioning: **AI-primary, SWE-secondary**.

The existing portfolio is strong on polyglot systems engineering and has two
genuinely good AI artifacts that are marketed as something else. What it has
**none** of — verified by grep across the whole workspace:

| Gap | Evidence |
|---|---|
| Retrieval / embeddings | `faiss\|pgvector\|qdrant\|chroma\|embedding` across all project source → one false positive, zero real hits |
| Agents / tool use / function calling | no tool loop anywhere; `council` is parallel prompting, not agents |
| Token / cost / latency accounting | `usage\|input_tokens\|cost` across `learningproj` and `council` → zero hits |
| Inference-side work | no batching, caching, or serving optimization |
| CI-gated evals | `learningproj/eval/` is the only harness; it hits a live paid endpoint so it cannot gate CI |

Sextant closes all five in one deployed, clickable artifact.

Two things make it the right choice over a generic RAG demo:

1. **It publishes a real benchmark number.** Almost no junior portfolio reports
   execution accuracy against a public benchmark, and essentially none report
   **cost per correct answer**. That table is worth more than another CRUD app.
2. **It extends work already done.** Cheap-model-first / escalate-on-uncertainty
   is the same routing pattern as the 3-tier WAF in `/home/sami/secProj`
   (calibrated char-LSTM → Mondrian conformal abstention → specialist tier). The
   portfolio stops reading as scattered and starts reading as one engineer with a
   thesis: **route by confidence, abstain rather than be wrong.**

Narrative bonus: the only professional role on the CV is Data Analyst at MaidsCC
(SQL, Tableau, June 2024 – Nov 2025). *"I built the agent that does the job I used
to do"* is the best opening line available.

**Out of scope:** Loom. Separate side project, untouched by this plan.

## 3. Non-goals

- Not a from-scratch vector index. Use `sentence-transformers` and a simple exact
  or HNSW-from-a-library search. Hand-rolling ANN duplicates Loom and buys nothing
  here.
- Not a general BI tool. No dashboard builder, no scheduled reports, no user
  accounts beyond what the demo needs.
- Not a chat app. Single-turn question → answer, with an optional clarification
  round. Multi-turn conversation is a P9 stretch at most.
- Not fine-tuning. Prompting, retrieval, routing, and validation only. If a
  trained reranker becomes justified by the numbers, revisit then.
- Not multi-tenant SaaS. One demo corpus, one benchmark corpus.

---

## 4. Architecture

```
  React 19 + Vite + TS  ──SSE──┐
   · question box              │
   · live trace timeline       │
   · SQL panel                 │
   · chart + result table      │
   · cost ledger               │
                               ▼
                    Go agent runtime  ──HTTP──►  Python sidecar (FastAPI)
                     · orchestrator loop          · schema embeddings
                     · SSE single-writer          · retriever + FK expansion
                     · provider iface + fake      · reranker
                     · budget / step / time caps  · eval harness (BIRD)
                     · trace store (SQLite)
                     · semantic cache
                            │
                            ▼
                    SQL guard + executor
                     · AST validation (SELECT-only)
                     · injected LIMIT, statement timeout
                     · dialect adapters: SQLite | Postgres
                            │
                            ▼
                    SQLite (BIRD)  ·  Postgres (live demo)
```

### 4.1 Language split and why

**Go — agent runtime.** Directly reuses patterns already proven in
`/home/sami/council/backend/internal/`: `errgroup` fan-out with `SetLimit`,
single-writer streaming with every channel send `select`-guarded on `ctx.Done()`,
per-IP token-bucket rate limiting (`internal/ratelimit`), SQLite daily quota
(`internal/store`), graceful shutdown with a drain window, JSON structured
logging. Read those packages before writing the equivalent here — do not
reinvent them, port them.

**Python — retrieval and evaluation.** `sentence-transformers` for embeddings,
`sqlglot` for SQL parsing, and the eval harness. FastAPI sidecar, same shape as
`ishtirak/services/analytics-python` and `loom/apps/ml-python`.

**React 19 + Vite + TS — UI.** Same stack as `portfolioPage` and `council`'s
frontend, so it can be lifted into the portfolio without friction.

### 4.2 Repository layout

```
sextant/
├── PLAN.md                      ← this file
├── README.md                    ← benchmark table first, honest error analysis last
├── Makefile
├── docker-compose.yml
├── docker-compose.test.yml
├── .github/workflows/ci.yml
├── packages/contracts/
│   ├── schemas/                 ← hand-authored JSON Schema 2020-12
│   │   ├── question_request.v1.json
│   │   ├── trace_event.v1.json
│   │   ├── sql_plan.v1.json
│   │   ├── cost_ledger.v1.json
│   │   └── eval_result.v1.json
│   ├── fixtures/{valid,invalid}/
│   └── codegen/generate.sh      ← go-jsonschema | datamodel-code-generator | json-schema-to-typescript
├── apps/runtime-go/
│   └── internal/{agent,provider,guard,executor,trace,cache,budget,httpx,ratelimit,config}/
├── apps/retriever-python/
│   └── src/{embed,retrieve,rerank,schema}/
├── apps/web/
├── eval/
│   ├── run.py
│   ├── data/                    ← BIRD dev subset + golden qrels
│   ├── fixtures/                ← recorded provider responses for replay
│   ├── evaluators/
│   └── results/                 ← committed, one file per run
└── infra/
    ├── demo-db/                 ← Postgres seed for the live demo
    └── e2e/smoke.sh
```

Contracts-first with a CI drift gate is the pattern from
`loom/packages/contracts` — it worked, keep it. Codegen output is committed and
CI regenerates then runs `git diff --exit-code`.

---

## 5. Component specifications

### 5.1 Schema retriever

**Problem.** A real database schema does not fit in a context window, and
stuffing it in degrades accuracy even when it does fit.

**Approach.**
1. Offline: build a document per table — table name, column names and types,
   comments, primary/foreign keys, and 3 sampled distinct values per column
   (values matter enormously for text-to-SQL; a question saying "cancelled"
   only maps to `status = 'C'` if the model sees the values).
2. Embed each table document. Store vectors alongside the schema metadata.
3. Online: embed the question, retrieve top-k tables by cosine similarity.
4. **Expand along foreign-key edges** — a question that joins A→B→C will often
   only surface A and C by similarity; B is a bridge table with no lexical
   overlap. Expand one hop, optionally two, bounded by a table budget.
5. Rerank the expanded set and truncate to the budget.

**This step is the one to hand-build.** The embedding model is off the shelf;
the FK-graph expansion and the rerank policy are yours.

**Metric.** Schema recall@k — does the retrieved subset contain every table in
the gold query? Measured independently of end-to-end accuracy so you can tell
retrieval failures from generation failures. This separation is the single most
useful diagnostic in the whole system.

### 5.2 SQL guard

**AST-level validation, never regex.**

**Where the parsing happens — decided at P1.** `sqlglot` is the better parser
and lives in Python; the guard is Go. Rather than pick one, they are split: the
retriever sidecar parses and reports facts as a `parse_summary.v1` (statement
count, every AST node kind present, referenced tables, called functions, the
statement re-rendered under a row cap), and `internal/guard.Validate` applies
the policy to that summary. A summary is evidence, never a verdict. The sidecar
holds no notion of a "safe" query and the guard does no parsing.

Consequences to keep in view: the sidecar is on the hot path of every question,
so it has an explicit client timeout and **fails closed** — unreachable must
look nothing like approving. And because `parse_summary.v1` is a contract, a
pure-Go parser can replace the sidecar at P3 without the policy changing.

The guard then enforces:

- Exactly one statement. Reject stacked queries.
- Root node is a `SELECT` (or a CTE terminating in one). Reject every DDL/DML
  node type explicitly by allowlist, not denylist.
- No `PRAGMA`, no `ATTACH`, no `COPY`, no file functions, no `pg_read_file`.
  **Functions need their own allowlist, separate from node kinds.** A parser
  renders a function it knows as its own node type (`COUNT` → `Count`) but
  every function it does not know as one shared kind (`Anonymous`), so
  `pg_read_file`, `readfile` and `lo_import` are indistinguishable from each
  other — and from any unknown-but-harmless function — by node kind alone.
  `parse_summary.v1` therefore carries `functions` as well as `node_kinds`.
- Referenced tables must be a subset of the retrieved schema subset. A query
  touching a table the retriever never surfaced is a bug or an injection.
- Inject a `LIMIT` if absent; clamp it if present and larger than the cap.
- Statement timeout enforced by the driver, plus a wall-clock cap in Go.
- Executed on a **read-only connection with a read-only database role.** Defence
  in depth: the guard is not the only thing standing between a bad generation and
  a dropped table.

**Adversarial fixture set** (all must be rejected, all must be tests):
`DROP TABLE users`; `SELECT 1; DELETE FROM users`; `SELECT * FROM users -- ' OR
1=1`; a query with `ATTACH DATABASE`; a cartesian product with no `LIMIT`; a
recursive CTE that exceeds the timeout; a query referencing a table outside the
retrieved subset.

**Error taxonomy.** Classify execution failures into: syntax error, unknown
column, unknown table, type mismatch, timeout, empty result, too-many-rows. The
repair loop's next prompt depends on which one fired — a syntax error and an
empty result deserve completely different follow-ups. Model this on
`learningproj/src/utils/classify_llm_generation_error`, which correctly separates
retryable from terminal failures so a billing error doesn't burn the retry budget.

### 5.3 Agent loop

```
plan → retrieve → generate → validate → execute → observe
                     ↑                              │
                     └──── repair (bounded) ────────┘
                                                    │
                              escalate (bounded) ───┤
                                                    │
                                      abstain ◄─────┘
```

Bounded on three independent axes, all configurable, all enforced:
- **max repair depth** (default 3)
- **dollar budget per question** (default $0.05)
- **wall-clock cap** (default 30s)

Whichever trips first ends the run. A run that ends by hitting a cap is a
*recorded outcome*, not an error — the eval reports the rate.

Every state transition emits a `trace_event.v1` over SSE. The UI is a consumer of
that event stream, not a special case; the eval harness consumes the same stream.
One protocol, two consumers.

### 5.4 Uncertainty router

The intellectually distinctive part, and the direct descendant of `secProj`.

**Tier 1 — cheap model.** Default `claude-haiku-4-5-20251001`. Handles the
majority of questions.

**Confidence signal.** Sample k=3 SQL candidates, execute all three against the
database, and measure agreement **on the result sets, not on the SQL text.**

> ⚠️ **Corrected 2026-08-03, during P1.** This section originally said "at
> temperature > 0". **Claude Sonnet 5 rejects a non-default `temperature` with
> a 400**, as do Opus 4.7/4.8/5 and Fable 5 — the parameter was removed from
> those models. Since Sonnet 5 is the intended Tier 2 (§5.4), the sampling
> method described here cannot work on the escalation tier as written.
>
> Verified against the API reference, not assumed. `internal/provider`'s
> adapter refuses to send a temperature to those models rather than 400-ing on
> every escalated call or silently dropping it — a silent drop is the dangerous
> one, because the router would believe it drew k independent samples when it
> drew the same one k times, and self-consistency measured over identical
> samples reports maximum confidence exactly when it has none.
>
> **Open, to decide at P6.** Options: vary the prompt rather than the sampler
> (reorder the schema card, reword the instruction); use `effort` levels as the
> axis of variation; sample k on the *cheap* tier only, where Haiku 4.5 still
> accepts a temperature, and use Tier 2 purely as the tie-breaker; or drop
> k-sampling for a different confidence signal entirely. The cheap-tier-only
> option is the smallest change and may be sufficient, since §5.4's routing
> question is about whether Tier 1 candidates disagree. Two syntactically different queries that return identical rows
are the same answer; two similar-looking queries returning different rows are
not. This is the right notion of self-consistency for this domain and it is worth
a paragraph in the README.

**Tier 2 — escalate.** Route to `claude-sonnet-5` when: candidates disagree, or
the guard rejected every candidate, or repair depth exceeded a threshold.

**Tier 3 — abstain.** If Tier 2 candidates still disagree, return "I can't
answer this reliably" plus the closest attempt and the reason, rather than a
confident wrong join. Report the **coverage vs. accuracy curve** — accuracy at
100%, 90%, 80% coverage. The WAF work already established this framing; reuse it.

**The question the eval must answer:** does escalation actually pay? Report
accuracy on the escalated slice against what Tier 1 alone would have scored on
that same slice. If routing doesn't beat always-cheap on cost-adjusted accuracy,
say so in the README. A negative result honestly reported is a stronger signal
than a positive one vaguely claimed.

### 5.5 Cost ledger

Per step, persisted with the trace: model id, tokens in, tokens out, dollars, ms,
cache hit/miss, escalation flag, repair depth. Aggregated per question and per
eval run.

This is the gap that matters most for AI-infra roles and it is currently absent
from every project in the workspace. Build it in P1, not as an afterthought —
retrofitting accounting is how you end up without it.

Price table lives in config, versioned, with the date it was last checked.
Never hardcode prices inline.

### 5.5.1 Prompt caching — adopted at P1 (2026-08-03)

Distinct from the semantic cache below. This is the *provider's* prefix cache:
the system prompt carries the schema card, is byte-identical for every question
against one database, and is therefore exactly the prefix `cache_control`
exists for. `BuildRequest` orders the request so the volatile question comes
last, and the adapter marks the system block.

The accounting consequence is the reason it is written down. Input arrives in
**three classes billed at three different rates** — standard, cache read at
about 0.1x, cache write at about 1.25x — so `provider.Usage` and
`cost_ledger.v1` keep them apart. Summing them into one `tokens_in` would
overstate reads tenfold, understate writes, and leave the dollar figure
uncheckable by anyone reading the ledger, which is the opposite of what this
project is for.

**It does nothing yet, and that is expected.** Each model has a minimum
cacheable prefix; Haiku 4.5's is 4096 tokens, the highest of any current model.
The toy fixture's prompt measures about 450, so nothing is cached and the API
reports a zero rather than an error. It becomes live when the corpus grows at
P5. The machinery is in place now so the numbers are already right on the day
it does — retrofitting accounting is how you end up without it (5.5).

### 5.6 Semantic cache

Question embedding → nearest cached `(question, schema_fingerprint, SQL)` entry
above a similarity threshold. **Scoped per schema fingerprint and invalidated
when the schema changes** — a cached SQL against a renamed column is worse than a
cache miss. Re-validate the cached SQL through the guard before reuse; never
trust the cache to have been correct.

Measured: hit rate, dollars saved, and — the important one — **false-hit rate**,
where a cached SQL was returned for a question it doesn't actually answer.
Threshold tuning is a coverage/precision tradeoff and belongs in the README.

---

### 5.7 Plumb — the claim-verification surface

**Problem.** Documentation drifts from the repository it describes, and the
layer that drifts worst is the one loaded into every agent session. Verified
across this workspace on 2026-08-12: `loom/README.md` advertises a from-scratch
HNSW index and a CI regression gate against an `eval/` tree of six empty
directories and a `Makefile` calling a `loom/eval/run.py` that does not exist;
`pokemonScraper/ROADMAP.md` marks Phases 0–4 🚧 with all five committed and
Phases 6–10 shipped but absent; `council/CLAUDE.md:22` says the frontend is
"still greenfield" over a fully populated `council/frontend/src/`; and
`~/.claude/skills/claude-machinery-map` asserts four things about this machine
that stopped being true weeks ago, in a `description:` field every session
loads.

Five roadmaps in the workspace use **four incompatible marker dialects**
(`✅ 🚧 ⬜`, `[x]`, `COMPLETE`, `DONE`) across ~600 done-claims, so no regex
reaches this. `pokemonScraper`'s drift is semantic: the checkboxes are ticked,
the phase heading is not.

**Why it belongs in Sextant.** Index a repository into SQLite and the question
"is this claim still true?" is the same pipeline as "what does this data say?",
pointed at a different store. The guard, executor, repair loop, budget, trace,
ledger, uncertainty router, and eval harness are reused unchanged; only three
components are new.

| Engine component | Sextant surface | Plumb surface |
| --- | --- | --- |
| Store | Olist / BIRD | `repo.db` — files, dirs, stubs, commits, doc claims, doc paths, make targets |
| Retriever (§5.1) | table docs + FK expansion | symbol docs + import-graph expansion |
| Generation | SQL | SQL over `repo.db` |
| `guard.Validate` | ✅ | unchanged |
| Executor, repair, budget, trace, ledger | ✅ | unchanged |
| Router (§5.4) | "no answerable join" | `UNVERIFIABLE` |
| Render | chart | verdict + evidence rows |

**The three new components.**

1. **`internal/index` + `cmd/plumb` (P2.5).** Deterministic: no model,
   no network, no credential. A finding is a contradiction between two indexed
   facts. Five checks — `dangling-doc-path`, `empty-advertised-dir`,
   `missing-target-script`, `phase-marker-contradiction`,
   `undocumented-shipped-phase`.

2. **`internal/claims` (P6.5).** Prose → structured claims, verbatim quotes
   only. Reuses the validation-feedback retry loop from
   `learningproj/apps/curriculum-python/src/utils/retry.py`.

3. **`internal/verdict` (P6.5).** `SUPPORTED | CONTRADICTED | UNVERIFIABLE`
   with the evidence rows behind it, calibrated by porting
   `secProj/waf/specialist/conformal.py`. Abstention is the headline
   behaviour: a verifier that guesses reintroduces the hallucination it exists
   to catch.

**Claim surface.** Repo docs — README, ROADMAP, everything under docs/ including
the ADR set — carry
the benchmark rigor, scored against DocPrism and CASCADE. Agent context
(CLAUDE.md, AGENTS.md, the skill files under .claude/skills/, memory files) carries
the originality; its ground truth is one machine, so it is reported as a case
study and never as a benchmark.

**Known limits of the deterministic layer.** Both are inherent, not bugs, and
both are what the P6.5 claim layer exists to close:

- **Forward-looking references.** The ownership table names a P6 router file
  that does not exist yet, and discloses in prose that these "arrive
  with their phase". Only a reader of prose can tell disclosure from drift, so
  P2.5 reports seven such lines on this repository.
- **Cross-repo references without a repo prefix.** This file cites secProj
  paths such as its conformal router by a path relative to *that* repo. A reference whose first segment names
  a sibling directory is filtered; one written relative to the other repo's
  root cannot be, from inside this one.

`make plumb-check` is **not wired into CI** and is not called a gate: the two
limitation classes above mean it cannot be green on this repository until the
P6.5 claim layer can read the disclosures, and a target that is red by
construction teaches people to ignore it. Wiring it needs a committed baseline
or an allowlist first.

Precision is bought by refusing to guess: an ambiguous reference (`try/catch`,
`ui/Button`) is recorded and not reported, a gitignored path is an artifact
rather than drift, and `.claude/plans/` is a working log rather than a claim.
Those three rules took the false-positive count on `ishtirak` — the workspace's
least-drifted repository — from 47 to 0.

**Hardening.** The threat model is a repository someone else wrote — a clone, a
fork's pull request, a CI checkout — so its contents *and* its `.git/config` are
attacker-controlled. Four defences, each from a reproduced exploit in the P2.5
security review:

- **`hardenedConfig` on every git invocation** (`-c core.fsmonitor=false`,
  `-c core.hooksPath=/dev/null`, …). `core.fsmonitor` names a program git runs
  while refreshing the index, and `git check-ignore` refreshes the index, so
  pointing the tool at a hostile clone silently executed its command. Command-
  line `-c` is what overrides repo-local config; scrubbing the environment
  cannot. Re-verified against 18 vectors, including `include.path` chaining;
  pinned by `TestHostileRepoConfigCannotExecute`.
- **A byte cap on git output.** `defaultCommitLimit` bounds the commit *count*,
  but a subject has no length limit: one 200 MB subject reached 906 MB of RSS
  from a repository whose packed objects were 386 KiB.
- **A chunked gitignore lookup.** `git check-ignore --stdin` aborts the whole
  batch on the first path it refuses and prints nothing, so one reference
  through a symlinked directory disabled every suppression in the report.
  Failure now splits the batch to isolate the offender, and an incomplete
  lookup is reported rather than passed off as a clean run.
- **`cmd.WaitDelay`.** `CommandContext` kills the direct child only; a leaked
  grandchild holding the stdout pipe beat the 30s cap by 98s.

## 6. The eval harness

This is the centerpiece. Everything else exists to be measured by it.

### 6.1 Benchmark

**BIRD** (BIg Bench for LArge-scale Database Grounded Text-to-SQL Evaluation).
Dev set is roughly 1.5k questions across ~11 SQLite databases, each question
carrying an "evidence" field of external knowledge. Verify the exact counts and
license terms at download time — do not quote numbers from this document.

BIRD is the right choice over Spider: larger and dirtier schemas, value-level
reasoning required, and it defines **Valid Efficiency Score (VES)** alongside
execution accuracy, which gives a second axis to report. Add Spider later if
time allows; it is a useful cross-check but a smaller, cleaner benchmark.

### 6.2 Metrics

| Metric | What it tells you |
|---|---|
| **Execution accuracy (EX)** | The headline number. Result-set equality against gold. |
| Valid Efficiency Score (VES) | BIRD's efficiency-weighted accuracy |
| Schema recall@k | Retrieval quality, isolated from generation |
| Valid-SQL rate | How often generation produces something the guard accepts |
| Repair-loop lift | Accuracy with repair minus accuracy without |
| Escalation rate + escalated-slice accuracy | Does routing pay? |
| **Cost per correct answer ($/EA)** | The metric nobody reports |
| Coverage vs. accuracy curve | The abstention story |
| p50 / p95 latency | End-to-end, and per step |

### 6.3 Record/replay — the part that makes it CI-gateable

Cache real provider responses to disk keyed by a hash of `(model, prompt,
temperature, sample index)`. In replay mode the provider adapter reads from disk
and makes zero network calls.

Consequences:
- A deterministic subset (start with 50 questions) gates CI on every push, free.
- The full dev run is a manual, paid, recorded operation — `make eval`.
- Re-running an old fixture set after a prompt change shows exactly what moved.

`learningproj/eval/base.py` is a good harness that could never gate CI because it
hits `PYTHON_SERVICE_URL` live. Fixing that limitation is a deliberate,
explainable improvement over prior work — say so in the README.

For fakes, port the pattern from
`/home/sami/council/backend/internal/provider/fake.go`: a `FakeProvider` shipped
in the non-test package, mirroring the real streaming shape (one goroutine,
closed channel, `ctx.Done()` checked before every send), able to inject both
terminal and construction errors.

### 6.4 Exit criteria as declarative thresholds

Copy the pattern from `learningproj/eval/queries_eval.py`: each evaluator returns
`{metric: (op, threshold, description)}` and the runner prints PASS/FAIL per
criterion. CI fails on a regression below the floor, not on a fixed target.

Initial floors, to be set after the P2 baseline exists — do not invent them now.

### 6.5 Results are committed

One JSON file per run in `eval/results/`, with run metadata: git SHA, prompt
versions, model ids, price table date, fixture set hash. The eval history is part
of the story. A chart of accuracy over time, with the commits that moved it,
belongs in the README.

---

## 7. Phase plan

~16 weeks at 20 hrs/week. **Eval lands in week 4 on purpose** — everything after
that is measured rather than guessed.

| # | Weeks | Deliverable | Done when |
|---|---|---|---|
| **P0** | 1 | Scaffold: repo, JSON Schema contracts + 4-language codegen + CI drift gate, Dockerfiles, compose, provider interface + `FakeProvider`, trace event schema, demo Postgres seeded, BIRD subset downloaded and loaded | CI green on an empty-but-real pipeline |
| **P1** | 2–3 | **Thin slice.** Question → whole-schema prompt → SQL → guarded execute → result table in the browser, streaming over SSE. Cost ledger wired from day one. **The AST guard's policy (`guard.Validate`) moved here from P3** — P1 executes model-written SQL, so it cannot ship behind a placeholder check. No retrieval, no routing, no repair. | You can ask a question in a browser and get a correct table back, and see what it cost |
| **P2** | 3–4 | **Eval v0.** 50 BIRD questions, execution accuracy, record/replay fixtures, CI gate with a floor. | A baseline number exists and CI fails if it drops |
| **P2.5** | 4 | **Plumb, deterministic half.** `internal/index` + `cmd/plumb`: repo → SQLite, five fact-versus-fact checks, `make plumb`. No model, no network. | 0 findings on `ishtirak`; `loom`'s missing `loom/eval/run.py` and `pokemonScraper`'s phase contradictions both caught |
| **P3** | 4–6 | SQL guard hardened: **dialect adapters (SQLite + Postgres)**, executor limits, the error taxonomy (`Classify`), the full adversarial fixture set as executable fixtures, and revisiting the sidecar hop against a pure-Go parser | Every adversarial fixture is rejected, as a test |
| **P4** | 6–8 | Repair loop + full agent trace + SSE trace timeline UI | Repair-loop lift is a measured number |
| **P5** | 8–10 | Schema retriever: table docs, embeddings, FK expansion, rerank. Eval scaled to full BIRD dev. | Schema recall@k reported separately from EX |
| **P6** | 10–12 | Uncertainty router: k-sample self-consistency on result sets, escalation, abstention | Coverage/accuracy curve published; the "does routing pay" question answered either way |
| **P6.5** | 12 | **Plumb, LLM half.** `internal/claims` + `internal/verdict` on the P6 router. Scored against DocPrism and CASCADE in the same harness. **Blocked on P1**: reuses `guard.Validate`, which is an open `TODO(you)`. | Precision/recall/abstention published; `claude-machinery-map` returns CONTRADICTED on four lines |
| **P7** | 12–13 | Semantic cache + cost dashboard + $/correct-answer | Hit rate, dollars saved, and false-hit rate all measured |
| **P8** | 13–15 | Charts in the UI, deploy (Fly.io or Render + Vercel), README with the benchmark table and error analysis | Public URL, linked from samibk.com |
| **P9** | 15–16 | Buffer, write-up, portfolio integration | — |

**Phase gate, per the house rule in `~/.claude/skills/change-control`:** a phase
is done when it is committed, CI is green, *and* the eval number is recorded. A
phase where the number moved the wrong way is a finding to write down in the
README, not a failure to hide.

**Review gate, per `/home/sami/CLAUDE.md`:** run
`everything-claude-code:code-reviewer` and
`everything-claude-code:security-reviewer` in parallel before declaring any phase
complete. Security review is mandatory for P1, P3, and P7 — those touch SQL
execution, user input, and cache poisoning respectively.

---

## 8. Testing doctrine

Per `~/.claude/skills/validation-and-qa`:

- Table-driven tests with fakes. No sleeps, no wall-clock, no live network.
- Injected clock (`loom/apps/crawler-go/internal/fetch/clock.go` is the reference).
- `httptest` for provider adapters; `FakeProvider` for the agent loop.
- Boundary validation at every ingress: the question request, the LLM output, the
  SQL before execution, the result before rendering.
- 80%+ coverage.
- `go test -race ./...` must be green on every commit to `main`.

Specific test obligations for this project:
- Every entry in the adversarial SQL fixture set has a test asserting rejection.
- The guard's allowlist is tested by asserting that a representative statement of
  *every* rejected node type is rejected — so a `sqlglot` upgrade that adds a node
  type fails loudly.
- The semantic cache has a test asserting invalidation on schema fingerprint
  change.
- The budget caps have tests asserting the run terminates at the cap, with the
  correct recorded outcome.

---

## 9. Verification

**Per phase, locally:**
```bash
docker compose up -d          # demo Postgres + services
make test                     # Go + Python + TS
make eval-smoke               # replayed fixtures, zero paid calls, identical to CI
make eval                     # full BIRD dev run — PAID, manual only
make eval-report              # metrics table + cost breakdown
```

**End-to-end in the browser:** ask a question requiring a three-table join.
Confirm the trace timeline shows retrieve → generate → validate → execute; the
SQL panel shows the exact executed statement; a deliberately ambiguous question
triggers escalation and the ledger shows the model switch; and a question with no
answerable schema path produces an abstention rather than a hallucinated join.

**CI must prove:** unit tests pass in all three languages; contract codegen is not
drifted (`git diff --exit-code` after regeneration); the replayed eval subset
holds its accuracy floor; every adversarial SQL fixture is rejected.

---

## 10. Definition of done

Deployed at a public URL, linked from samibk.com, with a README that **opens**
with this table filled in:

| Metric | Value |
|---|---|
| BIRD dev execution accuracy | — |
| Valid Efficiency Score | — |
| Schema recall@10 | — |
| Cost per correct answer | — |
| Escalation rate | — |
| Accuracy at 90% coverage | — |
| p95 latency | — |

…and **closes** with an error-analysis section saying plainly what it still gets
wrong — matching the honesty already present in the research entries on
`portfolioPage` (the V2G entry says outright that "the benign class fails
entirely across devices"; keep that standard).

---

## 11. Open decisions

Resolve these when the phase arrives, not now. Record the answer here when made.

1. ~~**Demo corpus.**~~ **DECIDED 2026-08-03 — the Brazilian E-Commerce Public
   Dataset by Olist.** See §11.1 below.
2. **Chart selection.** Rule-based from the result shape, or LLM-chosen? Start
   rule-based (one numeric column + one categorical → bar; two numeric → scatter;
   time column → line). Revisit at P8. Load the `dataviz` skill before writing any
   chart code.
3. **Postgres vs. SQLite for the demo.** BIRD ships SQLite, so the dialect
   abstraction is needed regardless. Postgres for the demo is a stronger signal
   but costs deploy complexity. Still open: Olist (#1) ships as CSVs and as a
   community SQLite build, so it seeds cleanly into either engine and does not
   force the answer. Decide at P3 when the dialect adapters land. **P1 is
   SQLite-only**; the executor is built around a dialect seam and `dbreg`
   already parses a `postgres:` prefix, so P3 adds an adapter rather than
   restructuring.
4. **Provider mix.** Anthropic for both tiers is simplest and gives clean cost
   accounting. z.ai GLM as the cheap tier reuses `council`'s adapter and is
   cheaper, but muddies the cost story with two price tables. Decide at P6.
5. **Embedding model.** `all-MiniLM-L6-v2` is the default and adequate. Revisit
   only if schema recall@k is the measured bottleneck.

### 11.2 P1 decisions — decided (2026-08-03)

Taken before P1 scaffolding, recorded so they are not relitigated.

| # | Decision | Why |
|---|---|---|
| 1 | **`guard.Validate` moves P3 → P1** | P1 executes model-written SQL in a browser. Shipping it behind a placeholder check would contradict §5.2's "AST-level, never regex" and leave the P1 security review nothing real to review. P3 keeps dialect adapters, executor limits, `Classify`, and the fixture corpus. |
| 2 | **Hybrid parse: sqlglot reports, Go decides** | Resolves §5.2 (sqlglot, Python) against the guard living in Go. See the box in §5.2. |
| 3 | **New `result_set.v1`, streamed as a named SSE frame**; `cost_ledger.v1` likewise | `trace_event.v1` carries `row_count` but not the rows: every trace event is persisted, so result data there would bloat every stored timeline. Named frames keep "one protocol, two consumers" literally true. |
| 4 | **P1 runs on `toy.sqlite` only** | See §11.1. Olist seeding moves to P5. |
| 5 | **The run starts on the SSE connect, not on the POST** | `EventSource` can only GET. Starting on the GET means no event can be emitted before a reader exists — no buffer to size, no replay to write, no lost-first-frame race. |
| 6 | **Per-IP limiter and a concurrent-run cap ship at P1** | P1 is the first phase that can spend money. The per-run budget bounds one question, not a thousand. |
| 7 | **Sami's P1 stubs are four**: `Agent.Run`, `Budget.Charge`, `guard.Validate`, `agent.ExtractSQL` | Prompt construction and schema-card rendering stayed Claude's. |

Two corrections this phase forced, both recorded above rather than silently
patched: §5.4's temperature-based sampling does not work on the intended Tier 2
model, and §11.1's "seeded at P1" was wrong.

### 11.1 Demo corpus — decided (2026-08-03)

**The Brazilian E-Commerce Public Dataset by Olist.** ~100k orders, 2016–2018,
distributed as nine related CSVs and as a community-built SQLite database.

**The reframe that drove it.** The demo corpus is not what stresses retrieval —
BIRD is, at 95 databases and 33.4 GB. Olist is nine tables and fits in a context
window, so on the demo the retriever is *illustrated*, not stressed. The demo's
job is to make the loop legible: the trace timeline, a guard rejection, the cost
ledger, an escalation, an abstention. Legibility only counts if a viewer can
tell a correct answer from a wrong one, and that criterion decides the rest.

Why Olist:

- **Already relational.** `orders` is a central hub, `order_items` is a genuine
  bridge with no lexical overlap, and `customers → orders → order_items →
  products → sellers` is a real five-hop path. No schema synthesis required.
- **Product categories are stored in Portuguese** (`beleza_saude`,
  `cama_mesa_banho`) with English translations in a *separate table*. "How many
  health & beauty products sold in Q2" is unanswerable from column names, needs
  sampled distinct values, and needs an FK hop to a translation table that
  shares no vocabulary with the question. That is §5.1's argument occurring
  naturally in real data — a better retrieval demo than the contrived
  `status = 'C'` in `infra/fixtures/toy.sql`.
- **Off-benchmark.** BIRD dev's eleven databases are `california_schools`,
  `card_games`, `codebase_community`, `debit_card_specializing`,
  `european_football_2`, `financial`, `formula_1`, `student_club`, `superhero`,
  `toxicology`, `thrombosis_prediction`. No retail among them, so the demo shows
  the loop generalizing past the benchmarked domains.
- **Verifiable by a stranger**, and it matches the CV narrative in §2 — revenue,
  delivery times, review scores are the job the agent is claimed to replace.
- **Exercises all three chart rules** in open decision #2: revenue over time
  (line), revenue by category (bar), freight vs. distance (scatter).
- **Cheapest to adopt.** The P0 placeholders in `infra/demo-db/01-schema.sql`
  and `infra/fixtures/toy.sql` already commit to `customers / products / orders
  / order_items`, so the toy fixture stays a faithful miniature of the demo
  corpus and the P3 dialect adapters target one logical shape in both.

Rejected, and why — recorded so it is not relitigated:

- **Chess (Lichess).** CC0 and the richest value-encoding story of the three
  (ECO codes, `1-0`, `Time forfeit`, `180+2`), but the raw export is flat PGN,
  so the relational schema would be synthesized rather than real; its
  distinctive feature (`white_player_id` and `black_player_id` both keyed to
  `players`) stresses *generation*, not the FK expansion P5 exists to show;
  BIRD dev is already sports- and games-heavy, making it the least
  differentiated pick; and a non-player cannot sanity-check an answer, which
  defeats the demo's purpose.
- **Lebanon open data.** HDX, Open Data Lebanon, and CAS are overwhelmingly flat
  statistical CSVs with per-source licensing; there is no ready-made relational
  corpus, so the schema would be authored and the join topology thin. The
  richest available sets (blast damage, refugee populations) also carry tonal
  risk that this system's failure modes — a confident wrong join, a hallucinated
  number — make worse rather than better.

**When it is seeded — corrected 2026-08-03.** This section originally said P1
seeds the corpus. It does not: **P1 runs against `infra/fixtures/toy.sqlite`
only**, and Olist seeding moves to **P5**, where the retriever needs a schema
big enough to retrieve over. The reasoning is the same one that chose Olist —
the demo corpus is not what stresses retrieval — and P1's job is the loop, not
the corpus. Seeding it at P1 would have meant a Kaggle credential, a
multi-hundred-megabyte download, and an ingestion script inside a phase billed
as a thin slice. The toy fixture is a faithful miniature: four tables, a real
bridge table, and the `status = 'C'` encoding that makes sampled values matter.

Deferred, not blocking:

- **License.** Not verified; deliberately not gating a learning project. Check
  it before the public deploy in §10, not before P5 seeds the corpus locally.
- **Subset size.** ~100k orders / ~110k order items is a few hundred MB in
  Postgres and free tiers are tight. Decide a subset when P8 picks a host.
- **A second corpus, optional.** A small CAS inflation schema at P8 would give
  the fingerprint-scoped cache in §5.6 two real corpora to invalidate between.
  Worth doing only if P7 lands early; drop it without regret otherwise.

## 12. Prior art in this workspace — read before building

| What | Where | Why |
|---|---|---|
| Streaming orchestrator, `errgroup`, single-writer, ctx discipline | `council/backend/internal/orchestrator/` | Port it, don't reinvent it |
| `FakeProvider` shape | `council/backend/internal/provider/fake.go` | The reference fake |
| Rate limiting, quota store, graceful shutdown | `council/backend/internal/{ratelimit,store,httpx}/` | Phase-5-hardened already |
| Retry with validation-error feedback, error classification, provenance metadata | `learningproj/apps/curriculum-python/src/utils/retry.py` | The best LLM-ops file in the workspace |
| Eval harness with declarative exit criteria | `learningproj/eval/base.py` | Copy the structure, fix the live-endpoint limitation |
| Contracts + codegen + CI drift gate | `loom/packages/contracts/` | The pattern works; reuse it |
| Injected clock, no-sleep tests | `loom/apps/crawler-go/internal/fetch/clock.go` | Testing reference |
| Conformal abstention, tier routing, calibration | `secProj/train/` and `waf/routing/router.py` | The intellectual ancestor of §5.4 |

**One known defect worth not repeating:** `council`'s `NewZaiProvider` builds
`&http.Client{}` with no timeout, and there are no LLM retries — a hung provider
connection is bounded only by the session context. Set an explicit HTTP timeout
on every provider client in Sextant from the first commit.
