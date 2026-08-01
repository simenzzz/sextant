# Eval harness

Lands at **P2**. See `PLAN.md` section 6.

This directory exists now so `make stubs` and the layout in `CLAUDE.md` refer
to something real rather than drifting from the tree on day one.

Planned contents:

```
eval/
  run.py          CLI entrypoint; exits non-zero when a criterion fails
  data/           BIRD dev subset + golden qrels — gitignored, fetched by
                  infra/scripts/fetch-bird.sh
  fixtures/       recorded provider responses for replay — gitignored
  evaluators/     one per metric, each declaring its own exit criteria
  results/        one JSON file per run, COMMITTED: the run history is part
                  of the story (PLAN.md section 6.5)
```

The design constraint that makes this CI-gateable: in replay mode the provider
adapter reads recorded responses from disk and makes **zero** network calls.
`learningproj/eval/base.py` is a good harness that could never gate CI because
it hits a live paid endpoint; fixing that is a deliberate improvement over
prior work, not an accident.
