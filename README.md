# Sextant

An analyst agent that answers natural-language questions over a real database —
retrieve the relevant schema subset, write SQL, validate it at the AST level,
execute it read-only, repair on failure, escalate to a stronger model when
uncertain, and abstain rather than answer wrongly — with every token, dollar,
and millisecond accounted for.

> **Status: P1 in progress.** The thin slice — question → SQL → guarded execute
> → result table, streaming over SSE, with the cost ledger wired from day one —
> is scaffolded, and four core functions still panic. Nothing below is
> measured yet: the table is reserved so the numbers land in the place they will
> be read, and every claim in this README stays in the future tense until an
> eval run fills it in. See [PLAN.md](./PLAN.md) for the full build plan.

## Results

*Against the [BIRD](https://bird-bench.github.io/) dev set. Empty until the eval
harness lands in P2 and produces a real run.*

| Metric | Value |
|---|---|
| BIRD dev execution accuracy | — |
| Valid Efficiency Score | — |
| Schema recall@10 | — |
| Cost per correct answer | — |
| Escalation rate | — |
| Accuracy at 90% coverage | — |
| p95 latency | — |

## What it will do

```
question
  → retrieve the relevant schema subset (embeddings + foreign-key expansion)
  → generate SQL
  → validate at the AST level (SELECT-only allowlist, table-subset check)
  → execute read-only, with an injected LIMIT and a statement timeout
  → observe the result or the classified error
  → repair, bounded
  → escalate to a stronger model when candidates disagree
  → abstain when they still disagree
  → render a chart
```

Bounded on three independent axes — repair depth, dollar budget per question,
and wall-clock — whichever trips first. A run that ends at a cap is a recorded
outcome, not an error.

## Architecture

| Component | Stack | Role |
|---|---|---|
| `apps/runtime-go` | Go 1.25 | Agent runtime: orchestrator loop, SSE single-writer, provider interface, budget/step/time caps, trace store, semantic cache |
| `apps/retriever-python` | Python 3.12, FastAPI | Schema embeddings, retrieval + foreign-key expansion, reranking, eval harness |
| `apps/web` | React 19, Vite, TS | Question box, live trace timeline, SQL panel, chart + result table, cost ledger |
| `packages/contracts` | JSON Schema 2020-12 | Source of truth for the wire types, generating into all three languages |

SQL is executed only on a read-only connection under a read-only database role.
The guard is not the only thing between a bad generation and a dropped table.

## Getting started

```bash
make help              # every target, with descriptions
make generate-schemas  # regenerate contract types for Go, Python, TypeScript
make build             # compile and lint all three services
make test              # run all three test suites
make stubs             # list the functions that still panic
make up                # demo Postgres + both services
```

Working in this repo? Read [CLAUDE.md](./.claude/CLAUDE.md) first — it defines
the invariants, the CI gate, and the code rules.

## License

Not yet chosen.
