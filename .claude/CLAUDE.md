# Sextant

Sextant is an analyst agent that answers natural-language questions over a real
database: retrieve the relevant schema subset, write SQL, validate it at the AST
level, execute it read-only, classify the failure and repair, escalate to a
stronger model when uncertain, and abstain rather than answer wrongly — with
every token, dollar, and millisecond accounted for, measured against the public
BIRD text-to-SQL benchmark.

**`PLAN.md` is the source of truth** for scope, architecture, component
specifications, the phase plan (P0–P9), and the open decisions. Read it before
any non-trivial work. `PORTFOLIO.md` is positioning work that runs alongside the
project; a session working on Sextant does not need it.

| Component | Stack | Role |
| --- | --- | --- |
| `apps/runtime-go` | Go 1.25 | Agent runtime: orchestrator loop, SSE single-writer, provider interface, budget/step/time caps, trace store, SQL guard and executor, semantic cache |
| `apps/retriever-python` | Python 3.12, FastAPI | Schema embeddings, retrieval + foreign-key expansion, reranking, the BIRD eval harness |
| `apps/web` | React 19, Vite, TS | Question box, live trace timeline, SQL panel, chart + result table, cost ledger |
| `packages/contracts` | JSON Schema 2020-12 | Source of truth for the wire types, generating into all three languages |

---

## The TODO(you) contract

**This is a learning project. Claude builds the scaffolding; Sami writes the core
logic.** The division is fixed and not up for renegotiation mid-task.

Claude commits **compiling** files whose core bodies are `panic("TODO(you): …")`
(Go) or `raise NotImplementedError("TODO(you): …")` (Python). Each carries a
doc-comment recipe: what the function must do, the algorithm in numbered steps,
the invariants, and the reference. Alongside every stub Claude commits
table-driven tests that **fail** until the body is written.

Rules, non-negotiable:

- Claude must **never** fill, sketch, paraphrase, or "just show what it would
  look like" for a `TODO(you)` body unless Sami explicitly asks — **even when
  tests fail on the panics.** A red suite on a `TODO(you)` panic is the intended
  state of the loop, not a bug to fix.
- If a stub blocks Claude's own work, Claude names the blocking stub and stops.
  It may improve the recipe or tighten the test; never write the body.
- Before editing anything under `apps/runtime-go/internal/` or
  `apps/retriever-python/src/`, run `make stubs` and check the open sites.
- If a diff would replace a `TODO(you)` body unasked, revert it.

### Who owns what

Sami implements these. They are the pieces `PLAN.md` §5 identifies as the
intellectual core; most do not exist yet and arrive with their phase.

| Package | Function | Phase |
| --- | --- | --- |
| `internal/agent/loop.go` | `(*Agent).Run` — plan→retrieve→generate→validate→execute→observe | P1/P4 |
| `internal/agent/budget.go` | `(Budget).Charge` — three independent caps, correct recorded outcome on trip | P1 |
| `internal/agent/extract.go` | `ExtractSQL` — recover the statement from raw model output | P1 |
| `internal/guard/guard.go` | `Validate` — allowlist node walk, function allowlist, table-subset check, LIMIT settlement | P1 |
| `internal/guard/taxonomy.go` | `Classify` — execution error → failure kind | P3 |
| `internal/router/router.go` | `Decide` — result-set agreement, escalate, abstain | P6 |
| `internal/cache/cache.go` | `Lookup` / `Store` — fingerprint scoping, threshold, invalidation | P7 |
| `retriever-python/src/retrieve/expand.py` | `expand_fk` — foreign-key hop expansion under a table budget | P5 |
| `retriever-python/src/retrieve/rerank.py` | `rerank` — rerank policy and truncation | P5 |
| `internal/claims/extract.go` | `Extract` — prose → structured claims, verbatim quotes only | P6.5 |
| `internal/verdict/verdict.go` | `Decide` — evidence rows → SUPPORTED/CONTRADICTED/UNVERIFIABLE, conformal-calibrated | P6.5 |

Plumb's indexer (`internal/index`, `cmd/plumb`) is **Claude's**, decided
2026-08-12: it is mechanical — a tree walk and a SQLite projection — and holds
none of the intellectual core. The judgement layers above it are Sami's, per
the table.

Claude owns everything else: HTTP and SSE transport, config, trace store,
executor and driver plumbing, read-only connections, provider adapters and
`FakeProvider`, prompt construction and schema-card rendering, the sqlglot
parse endpoint, the guard's allowlists and sidecar client, the cost ledger,
embeddings (off the shelf, per `PLAN.md` §3), the eval harness base and
record/replay, contracts and codegen, Docker, CI, the Makefile, and all test
harnesses — including the table-driven cases for Sami's stubs.

Two entries above changed at P1 and the reasons are worth keeping:
`(Budget).Charge` is a **value** method returning a new `Budget` rather than
`(*Budget).Charge` mutating in place — the immutability rule requires it, and it
lets the P6 router evaluate whether an escalation fits without committing to the
charge. `guard.Validate` moved **P3 → P1** because P1 executes model-written SQL
and cannot ship behind a placeholder check; see `PLAN.md` §11.2.

---

## The two-job CI gate

`.github/workflows/ci.yml` splits the gate deliberately:

| Job | Runs | Must be green |
| --- | --- | --- |
| `contracts-drift` | regenerate + `git add -A` + `git diff --cached --exit-code` over the three gen dirs | Always |
| `security` | `govulncheck`, `pip-audit`, `npm audit` | Always |
| `build` | `go build`, `go vet`, `ruff`, `mypy`, `tsc`, `oxlint`, `vite build` | **Always.** Stubs compile, so an open stub never turns this red. |
| `test` | `go test -race`, `pytest`, `vitest run` | Green at P0; red exactly while a phase's stubs are open. |

**The red `test` job is the worklist, not a broken pipeline.** A scaffolding
commit from Claude lands stubs and turns it red — that phase is 🚧, by design.
Sami's implementation turns it green — that phase is ✅.

This is a deliberate departure from `/home/sami/loom`, whose CI runs the stub
tests with no split and is therefore permanently red, so no loom phase can ever
satisfy the workspace rule that done means committed **and** CI green.

Status markers mean exactly: ✅ committed and CI green · 🚧 in progress ·
⬜ not started. Never write ✅ next to work that appears in `git status --short`.

---

## Layout

```
sextant/
  PLAN.md                     source of truth for scope and phasing
  Makefile                    every workflow; `make help` lists them
  packages/contracts/
    schemas/                  7 hand-authored JSON Schema 2020-12 contracts
    fixtures/<name>/{valid,invalid}/   shared corpus, run by ALL THREE suites
    codegen/generate.sh       pinned generators; refuses to run on version drift
    codegen/versions.env      the pins
  apps/runtime-go/
    cmd/server/main.go        slog, config, signal ctx, ServeMux, graceful drain
    internal/config/          Load() + typed env helpers
    internal/contracts/       embedded schemas + Validate at every boundary
    internal/provider/        Provider interface, FakeProvider, Anthropic adapter
    internal/pricing/         versioned price table + the date it was checked
    internal/clock/           the ONLY source of time; nothing else calls time.Now
    internal/dbreg/           database slug → dialect + DSN; a key, never a path
    internal/schema/          introspection, the schema card, the fingerprint
    internal/executor/        read-only execution of a sql_plan.v1
    internal/guard/           allowlists, the sidecar parse client, Validate
    internal/agent/           the loop, budget, prompt, ledger, SQL extraction
    internal/api/             POST /v1/questions, GET /v1/runs/{id}/events
    internal/ratelimit/       per-client token bucket
    internal/trace/           SQLite trace store and cost ledger
    internal/httpx/           SSE stream writer
    internal/index/           repo → repo.db; the deterministic Plumb checks
    cmd/plumb/                the claim-verification CLI (PLAN.md 5.7)
    internal/contracts/gen/   GENERATED — do not hand-edit
  apps/retriever-python/
    src/main.py               create_app() with an injectable runtime factory
    src/config.py             load_settings()
    src/contracts.py          validate at every boundary, in and out
    src/sqlguard/             sqlglot parsing — reports facts, decides nothing
    src/routes/               one module per resource
    src/{embed,retrieve,rerank,schema}/   empty until P5
    src/models/gen/           GENERATED — do not hand-edit
  apps/web/
    src/lib/protocol.ts       ajv validators + never-throwing parseEvent
    src/lib/sseClient.ts      transport only; holds no application state
    src/state/                reducer (pure) + useQuestion (wiring only)
    src/contracts/gen/        GENERATED — do not hand-edit
  infra/
    fixtures/toy.sqlite       committed; what tests and CI run against
    demo-db/                  Postgres seed + env-driven read-only role
    scripts/fetch-bird.sh     manual, license-gated, digest-checked, never in CI
  eval/                       eval harness (P2) — see eval/README.md
```

---

## Working rules

### Reviews (mandatory)

- After **any code change that touches logic**, run the
  `everything-claude-code:code-reviewer` sub agent before declaring the work
  complete. Tests and a type checker are not a substitute.
- Additionally run `everything-claude-code:security-reviewer` whenever the change
  touches SQL execution, user input, the provider or its API key, the SSE
  protocol surface, or the cache. **Mandatory for P1, P3, and P7** — those touch
  SQL execution, user input, and cache poisoning respectively.
- Run both in parallel (one message, multiple agent calls). Address every
  CRITICAL and HIGH before reporting done; fix or explicitly acknowledge MEDIUM.
- Trivial changes (typos, comments, one-line fixes) may skip the pass.

### Invariants — do not weaken these

- **SQL executes only on a read-only connection under a read-only database
  role.** The AST guard is not the only thing between a bad generation and a
  dropped table. See `infra/demo-db/02-readonly-role.sh`.
- **Every provider HTTP client has an explicit timeout.** `PLAN.md` §12 names
  council's missing timeout as the one known defect not to repeat: a hung
  upstream connection was bounded only by the session context.
- **The price table lives in versioned config with the date it was last
  checked.** Never inline a price.
- **The cost ledger records provider-reported token counts, never estimates.** A
  step whose usage was not reported is a step whose cost is unknown, and the
  ledger says so rather than guessing.
- **Never forward a raw upstream provider or driver error body to the browser.**
  They carry auth and quota state. Classify, log the detail server-side, return
  a generic message. This exact class of leak was caught by review in council.
- **A run that ends at a cap is a recorded outcome, not an error.** So is an
  abstention. The eval reports their rates; the UI must not render them as
  failures.
- **The origin allowlist is enforced, not just parsed.** Both services install
  it (`internal/httpx/cors.go`, `create_app`), it never emits `*`, it never
  allows credentials, and an empty allowlist permits nothing. If a browser call
  is blocked, add the origin to `SEXTANT_ALLOWED_ORIGINS` — never widen the
  middleware.
- **SSE event names are allowlisted and every frame write has a deadline.** An
  unvalidated event name can inject a whole second frame; an undeadlined write
  lets one slow reader pin a goroutine forever.
- **SSE has one writer.** `http.ResponseWriter` is not safe for concurrent use
  and the failure mode is interleaved half-frames, not a clean panic. Producers
  fan into a channel; one goroutine drains it.

### Code

- **Keep every file under 400 lines.** Split by responsibility as a file
  approaches the limit; favour many small cohesive files.
- **Immutability.** Return new copies; never mutate in place. This applies in Go,
  Python, and TypeScript alike.
- **Validate at every boundary**: the question request, the LLM output, the SQL
  before execution, the result before rendering. `packages/contracts` is how —
  each service embeds its own verbatim schema copy.
- **Never hand-edit anything under a `gen/` directory.** Change the schema and
  regenerate; the drift gate will revert you otherwise.
- **Tests are table-driven, with fakes.** No sleeps, no wall-clock, no live
  network, no paid calls. Target 80%+ coverage per service.
- **When unsure, ask** rather than guessing on scope or design trade-offs.

### Docs

When a change makes `PLAN.md` or this file wrong, update it in the same change,
or surface the drift explicitly. Never leave it silent.

---

## Build and test

| Task | Command |
| --- | --- |
| First-time setup | `make setup` |
| Regenerate contracts | `make generate-schemas` |
| Check for contract drift | `make check-schemas` |
| Compile and type-check all | `make build` |
| Lint all | `make lint` |
| Test all | `make test` |
| Coverage | `make coverage` |
| **List open stubs** | `make stubs` |
| Build plumb | `make plumb` |
| Check this repo's own docs | `make plumb-self` |
| Fail on doc contradictions | `make plumb-check` (not in CI — see PLAN.md §5.7) |
| Rebuild the toy database | `make toy-db` |
| Full stack | `make up` / `make down` / `make logs` |
| Compose smoke test | `make smoke` |

Per service: `cd apps/runtime-go && go test -race ./...` ·
`cd apps/retriever-python && .venv/bin/pytest` · `npm --prefix apps/web test`.

The entire suite runs against `infra/fixtures/toy.sqlite`, so no test needs a
network, a credential, or a download. `SEXTANT_PROVIDER` defaults to `fake`: a
fresh clone runs with no API key and cannot make a paid call by accident.

`make eval` is **paid and manual**. `make eval-smoke` is the replayed subset CI
runs. Both land at P2.

If `make up` brings up containers that report healthy but the browser cannot
reach, suspect a published-port collision before anything else. Every host port
is overridable — see `.env.example`. On WSL2 the squatter may be a **Windows**
process, in which case `ss -ltnp` inside WSL shows nothing at all and the
conflict looks causeless.
