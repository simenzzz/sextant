# Portfolio assessment — AI engineer target

> Assessment date: 2026-07-29. Verified against the live filesystem, git history,
> the GitHub API, and the live site — not from memory or project docs, which are
> known to drift in this workspace.
>
> Companion to [PLAN.md](./PLAN.md). That file is the project; this one is the
> positioning work that runs alongside it. A fresh session working on Sextant
> does not need this file.

---

## 1. Target

AI engineer, three flavors of interest: **agent/tooling platform**, **LLM product
engineering**, **AI infra/inference**. Applying in ~4–6 months. Positioning:
**AI-primary, SWE-secondary** — one AI-led CV and portfolio narrative, with a
generalist backend/full-stack variant retained for the Beirut market where AI
roles are thin. `cv-frontend.tex` gets retired.

## 2. What's genuinely strong

**Systems breadth most AI-engineer applicants don't have.** Go, Rust, Java/Spring,
Python, TypeScript — with real depth in each, not tutorial exposure:

| Project | Scale | Notable |
|---|---|---|
| `TPToolProj` (Deckgraph) | 146k LOC, 24 commits | Only project with LICENSE + CHANGELOG + CONTRIBUTING + CI + 132 tests + e2e together |
| `Website` (CareConnect) | 59k LOC, 72 commits | Backend + web + React Native, 68 test files, deployed |
| `tideway` | 38k LOC, 49 commits | 18 ADRs, `cargo-deny` supply-chain gate, 115 files with test modules |
| `ishtirak` | 34k LOC, 21 commits | 4 languages, CI across all of them, Playwright e2e, the least-drifted docs in the workspace |
| `discordClone` (Cove) | 57k LOC | Rust/Axum + SvelteKit + SurrealDB, deployed behind Caddy |

AI-infra teams hire for exactly this profile. It should be the SWE-secondary
spine of the CV.

**`secProj` (WAF) is the best AI project here and it is not being treated as
one.** Char-LSTM with calibration, Mondrian conformal abstention, a 3-tier
uncertainty router, plus an eval suite covering adversarial mutation, latency
loadgen, ablations, and an audit — 14.7k LOC of Python. Conformal prediction is a
genuinely sophisticated thing for a new grad to have implemented. It currently
has **4 commits, no CI, no README polish, and no demo**, and it is described on
GitHub as "Machine Learning Web Firewall Project."

**`learningproj` (Lucubrum)'s LLM layer is textbook AI engineering, buried inside
a study app.** `src/utils/retry.py` is the most job-relevant file in the
workspace: generate → extract JSON through five layered strategies → validate
against a Pydantic model → **feed the formatted validation errors back into the
next prompt**. Plus a versioned prompt registry (9 operations, one with a v1 and
v2), classification of retryable provider failures vs. terminal ones so a billing
error doesn't burn the retry budget, and provenance metadata on every artifact
(provider, model, prompt version, raw output hash, retry count).

**`council` proves shipping a streaming LLM product.** Hand-rolled SSE parsing,
fan-out orchestration with `errgroup`, per-IP rate limiting, SQLite daily quota,
graceful shutdown, deployed on Render + Vercel.

## 3. The gaps that cost interviews

Verified by grep across all project source, excluding `node_modules`, `.venv`,
`site-packages`:

1. **Zero retrieval.** `faiss|pgvector|qdrant|chroma|weaviate|pinecone|embedding`
   → one false positive in `learningproj/infra/scripts/set-user-tier.py`, no real
   hits. Retrieval is on nearly every AI-engineer JD. Biggest hole.
2. **Zero agents, zero tool use, zero function calling.** "Agent/tooling
   platform" is a stated target and there is no tool loop anywhere. `council` is
   parallel prompting.
3. **Zero token/cost/latency accounting.** `usage|input_tokens|cost` → no hits in
   `learningproj/src/` or `council`. For AI infra, cost-per-request *is* the job.
4. **No inference-side work.** No batching, caching, KV/paged attention,
   quantization, or serving optimization. `secProj`'s FastAPI model server is the
   closest and it's device-flag-level.
5. **Evals exist but gate nothing.** `learningproj/eval/` is a real harness with
   declarative pass/fail thresholds, but it calls a live paid endpoint, so it can
   never run in CI. Metrics are heuristic-only — no LLM-judge, no regression suite.

Sextant closes all five. See [PLAN.md](./PLAN.md).

## 4. Live liabilities

**`loom` is public and its README overclaims.** It states in the present tense:
"SPIMI inverted index with varbyte compression, BM25 + Block-Max WAND, from-
scratch HNSW, SymSpell spellcheck, hand-rolled LambdaMART inference." None of it
exists. `loom-indexer` is a 5-line stub that prints a message and `exit(2)`;
`/search` returns a hardcoded `[]`; `eval/` is an empty directory tree while the
Makefile advertises an NDCG harness (`make eval` fails instantly with "No such
file"); there are 33 `panic("TODO(you)")` sites. Anyone who clones it sees this.

*Loom is out of scope as a project, but the README is a five-minute fix and it is
currently working against you.* Rewrite it in future tense, or make the repo
private until P1 lands.

**samibk.com renders nothing to crawlers.** The site *is* deployed and working —
`/papers/*.pdf` serve correctly. But it's a client-rendered SPA with no
prerender and no OG tags, so Google, LinkedIn unfurls, and any recruiter tool
scraping it see `<div id="root">` and the title "Sami BK". Pasting the URL into
an application form or a LinkedIn message produces a blank card.

Worse, `portfolioPage/index.html:8` sets the meta description to the
developer-facing note:

> "A React single-page portfolio with a hidden internal studio for managing
> project entries."

That is the snippet Google shows, and it advertises the admin route.

**The GitHub profile is the primary proof source and it's cluttered.** 17 public
repos. Six are coursework: `ParallelLab1`, `ParallelLab3`, `parallel-lab-3` (the
same lab twice), `Assignment2-SamiBouKhaled`, `Parallel-Project`, `SBTest`. Most
of the real repos have empty descriptions. Nothing is pinned strategically, no
topics, no stars. `career-ops/modes/_profile.md` says outright: *"Use GitHub as
the strongest proof source until a portfolio URL is provided."*

**Three positionings in two months.** The June CV targets generalist new-grad
SWE; `cv-frontend.tex` (July) rewrites the summary to *"Seeking a React-focused
frontend engineering role"*; `interview-prep/` (2026-07-14) is a study roadmap
for two junior *frontend* interviews. Now the target is AI. Scattered signal
costs more than any missing project.

**The application pipeline stalled.** `career-ops/data/applications.md` has 14
applications all dated 2026-06-04, fit-scored, with the top three flagged "apply
immediately." `career-ops/data/follow-ups.md` is an empty table with a header
only. That batch was never worked. This is currently costing more than any
portfolio gap.

## 5. The repair track

~10 hours total, weeks 1–3, running alongside Sextant P0–P2. Cheap and
high-leverage, and it unblocks applying while the flagship is still cooking.

- [ ] **samibk.com meta + OG.** Replace the meta description with something
      recruiter-facing. Add `og:title`, `og:description`, `og:image`, and a
      canonical URL. Prerender or statically generate the shell so crawlers see
      content. (~2h)
- [ ] **`loom` README** — rewrite in future tense or make the repo private. (~15m)
- [ ] **Reframe `secProj` as an AI project.** README leading with conformal
      abstention and tier routing, not "web firewall." Add CI. Add a
      results table from the existing eval bundle. (~3h)
- [ ] **Reframe `learningproj` as an LLM-ops project.** README section surfacing
      the prompt registry, the validation-feedback retry loop, and the artifact
      provenance model. Link `retry.py` directly. (~1h)
- [ ] **GitHub hygiene.** Archive the six coursework repos. Write a one-line
      description on every remaining repo. Add topics. Pin six: Sextant, WAF,
      Lucubrum, Deckgraph, tideway, ishtirak. (~1h)
- [ ] **CV, AI-primary.** New variant leading WAF → Lucubrum → Sextant, with the
      MaidsCC data-analyst role framed as the origin story for Sextant. Keep the
      generalist SWE variant. Retire `cv-frontend.tex`. Fill in the portfolio URL
      — `career-ops/config/profile.yml` still has `portfolio_url: ""` and the CV
      still says "TBD" even though the site has been live for weeks. (~2h)
- [ ] **Work the June batch.** Follow up on the 14 logged applications, or
      formally close them and start a fresh batch against the AI-primary CV. (~1h)

## 6. The narrative to sell

Once Sextant ships, the portfolio tells one story instead of nine:

> **Route by confidence. Abstain rather than be wrong.**
>
> - **WAF** — calibrated char-LSTM, conformal abstention, uncertainty-routed
>   3-tier classifier. 0.972 F1 on CSIC 2010, 3.04% escalation to the expensive
>   tier.
> - **Sextant** — the same pattern applied to LLMs: cheap model first, escalate
>   on measured disagreement, abstain rather than hallucinate a join. Reported
>   against BIRD with cost per correct answer.
> - **Lucubrum** — the LLM-ops substrate: versioned prompts, validation-feedback
>   retries, provenance on every artifact, declarative eval thresholds.
> - **Council** — the same discipline in production: streaming, rate limits,
>   quotas, graceful shutdown.
>
> Backed by a systems spine: Rust, Go, Java, Python, TypeScript across
> `tideway`, `Deckgraph`, `ishtirak`, `CareConnect`.

That is a coherent engineer with a thesis, not a list of projects.
