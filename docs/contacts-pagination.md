# Contact page implementation and measurements

Read when: reviewing canonical contact pagination, reproducing its fixture measurements, or interpreting memory/database costs.

The reader uses a dedicated validated read-only archive connection, attaches only an existing read-only session database, and leases one read transaction per call. It projects public `lid/pn` and own-device `jid/lid`, streams raw rows sorted by canonical identity and deterministic source order, folds one identity at a time, and keeps a typed max heap of at most `limit+1` results. The heap, continuation boundary and final result use the same complete Go `(Name fallback JID, JID)` comparison. The existing merge function is shared with the legacy show reader; differential tests use that existing view rather than a second identity implementation.

A group stores fixed contact fields, preferred/alternate/source alias fields and OR-ed match flags. It never accumulates all source IDs/aliases. Go's **retained candidate memory** is O(K × field width + largest current row), K = limit+1. Arbitrarily large field widths and the normal agent envelope/cursor caps still matter. Cumulative allocations, SQLite temporary data and process RSS are different measurements; none is claimed constant in N.

Every page scans N raw contact rows. Existing public-map and alias indexes support point joins. SQLite orders groups using a temporary B-tree, with O(N log N) sorting work, and computes a temporary canonical projection. Own-device projection/indexing costs depend on D linked-device rows and the query plan; no assumption of an always constant D or guaranteed index choice is part of the contract. Schema/scope setup and the streaming SELECT require a constant number of roundtrips; correlated identity lookups are work inside SQLite, not extra client calls per contact. No new schema/index/cache or forced `temp_store=MEMORY` is introduced. SQLite can spill temporary projection/sort data to files.

## Measured parser re-evaluation

Measured locally on Linux/amd64, Intel Core i9-13950HX, project Go 1.27.1 toolchain and go-sqlite3 1.14.52 / SQLite 3.53.4, October 4, 2026. Synthetic fixtures have 10k/100k identities, 15k/150k contact rows (50% PN+LID pairs), repeated Unicode names, PN aliases on every seventh identity and a public own-device pair. No real accounts/stores are involved.

The initial flattened SQL repeatedly evaluated pure JID functions/identity expressions for output fields. The final plan explicitly materializes `canonical` and the small public `own_pair` projection **inside SQLite**. The before variant changes only those two CTE hints back to `NOT MATERIALIZED`. Both variants consume the same complete row projection on the same fixture, in before/after order, with no forced cache flush. These are single diagnostic runs, not universal latency forecasts.

| Identities | Plan | Streaming time | UDF calls | Cumulative Go allocation | Sampled process RSS peak |
| --- | --- | --- | --- | --- | --- |
| 10,000 | Flattened | 1.980 s | 900,031 | 130.4 MB | 37.8 MB |
| 10,000 | Materialized | 0.977 s | 230,006 | 39.6 MB | 39.0 MB |
| 100,000 | Flattened | 6.240 s | 9,000,031 | 1,277.6 MB | 44.6 MB |
| 100,000 | Materialized | 2.305 s | 2,300,006 | 381.5 MB | 49.5 MB |

The sampler reads Go heap and `/proc/self/status` every 5 ms. RSS includes Go/runtime, SQLite native allocations and caches; it does not isolate SQLite's allocator and can miss short peaks. Go heap baselines were about 2 MB, sampled live heap peaks 4.1–4.3 MB in all four runs. RSS baselines differed (30.6–38.6 MB) because the process and fixture/cache history are shared. Sampling and function-call instrumentation contribute overhead/allocations. Materialization reduced parser work and cumulative allocation while sampled process RSS was higher; native memory was not isolated. This is not a proof of bounded RSS or actual disk spill on every fixture.

Independent, uninstrumented `BenchmarkReadContacts -benchtime=1x` measured the entire reader/heap: before 0.823/6.282 s, 126.7/1,266.2 MB allocated; after 0.342/2.467 s, 37.8/377.7 MB allocated for 10k/100k respectively. Both after fixtures retained exactly 21 candidate slots for a page of 20. This corroborates the observed re-evaluation cost; no broader optimization is part of this change.

`EXPLAIN QUERY PLAN` before shows a contact scan, indexed alias/map lookups, repeated correlated own-pair scans and a temporary ORDER BY B-tree. After shows `MATERIALIZE canonical`, `MATERIALIZE own_pair`, indexed map/alias lookups, automatic temporary covering indexes on own-pair lid/pn (in this SQLite build), correlated public lookups and the final ORDER BY B-tree. These temporary indexes are planner-owned, not persistent schema changes. Selectivity does not eliminate the full merge scan: hidden original matches/metadata on a nonchosen row must survive grouping.

## Reproduce

Use the project toolchain and a hidden TMPDIR in the worktree, as for the normal gate:

```bash
source /home/gabriel-alonso/Projetos/wacli/dist/dev-env.sh
mkdir -p dist/.tmp
export TMPDIR="$PWD/dist/.tmp"
go test ./internal/app -run '^$' -bench '^BenchmarkReadContacts$' -benchtime=1x -count=1
WACLI_CONTACT_MEASURE=1 go test ./internal/app -run '^TestContactReadMaterializationEvidence$' -v -count=1
WACLI_CONTACT_QUERY_PLAN=1 go test ./internal/app -run '^TestContactReadQueryPlan$' -v -count=1
```

These diagnostics are opt-in and never appear in CLI responses. The standard plain/FTS tests cover differential identity/metadata matching, >2 static pages, source ties, unavailable/corrupt mappings, semantic scope checks before matching, live changes, cancellation/connection closure and public-column-only reads. `TestContactsBinaryFixturePages` can also run against an explicitly supplied plain/FTS CLI via `WACLI_CONTACT_E2E_BIN`, using only synthetic stores.
