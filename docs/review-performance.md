# Review performance: what was measured and how

This document records the measurements behind the FSRS change of the review (migration `V27__dictionary_review_fsrs.sql`,
`internal/store/review.go`, `internal/store/fsrs.go`, `internal/srs/optimize.go`). It has two parts: the optimizer
benchmark and the `EXPLAIN (ANALYZE, BUFFERS)` of every review query on a user with 1,000,000 words. Each part says how to
reproduce it, what came out, and what the measurement does **not** show.

Environment: PostgreSQL 16 (`postgres:16-alpine`, container `db-migration-postgres-1`, database `app`) on a laptop
(Apple M1 Pro), default configuration, one user in the tables. Numbers are from one run each, not averages.

## 1. Optimizer benchmark

Code: `BenchmarkOptimize50k` in `internal/srs/optimize_test.go`.

```bash
cd dictionary
go test -run xxx -bench Optimize -benchtime 1x ./internal/srs
```

**Data.** `simulate(DefaultWeights, 6500, seed)` builds 6,500 cards. Each card has one first grade and 8 later grades. The
gap before a later grade is between 0.5 and 2 times the stability, at least one day, and the grade is Again with the
probability the forgetting curve gives (otherwise Hard, Good or Easy). Every later grade follows a gap of a day or more, so
the data has about 52,000 samples (6,500 × 8), which is the `srs.MaxSamples` (50,000) scale the plan asks for.

**What `Optimize` does in the run.** The first 80% of the samples in time train, the last 20% judge. Training uses Adam
over mini-batches of 128 cards, the gradient comes from the reverse-mode tape (`autodiff.go`), the step is cosine-decayed,
and each weight is clamped to its bounds after every step. The epoch count is `max(5, ceil(300 / batches))`, so 5 epochs
here. The result is compared with the default weights on the held-out 20%.

**Result.**

| Benchmark | Time per run |
|---|---|
| `BenchmarkOptimize50k` | 406 ms |

The plan's target was under about 3 seconds. The benchmark measures `Optimize` only. It does not include reading the log
from the database (part 2, 1.4 s on the seed) or the handler.

**Accuracy check (not a benchmark).** `TestOptimizeBeatsDefaultsOnDifferentUser` simulates a user whose weights differ from
the defaults (`w2` halved, `w8` ×0.6, `w20` = 0.35), fits on 1,500 cards, and scores on 800 fresh cards of the same user:

| Weights | Log-loss on fresh data |
|---|---|
| default | 0.3942 |
| fitted | 0.3816 |
| true (the ones that generated the data) | 0.3806 |

The fitted weights end up within 0.001 of the true weights' loss. The test also checks that every weight is inside its
bounds and that two runs on the same input give the same weights.

**Gradient correctness.** `autodiff_test.go` compares the tape gradient of each operation and of the whole FSRS loss with
central finite differences (relative error under 1e-4), and `TestVarForwardMatchesFloat` checks that the `Var` forward pass
equals the `float64` one, because both are the same generic code.

**Not measured.** Memory use of the tape (it is bounded by the batch, 128 cards, but I did not profile it), the time with
the 15 s context timeout hit, and real user data: the simulated user recalls exactly as the model says, which real users do
not.

## 2. EXPLAIN of the review queries

Everything is in `docs/performance/` and runs end to end with one command:

```bash
cd dictionary/docs/performance
./run.sh                 # seed, EXPLAIN of every query, remove the seed (about 2 minutes)
KEEP=1 ./run.sh          # keep the seed afterwards, to try queries by hand
PSQL="psql postgres://user:pass@host/db" ./run.sh   # another database, migrated to V30
```

| File | What it does |
|---|---|
| `seed.sql` | creates the user `00000000-0000-0000-0000-0000000000aa` and its data (below), then `ANALYZE`; safe to run again |
| `explain-review.sql` | the review queries with literal values, each under `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF)`; the expected plan is written above each query |
| `cleanup.sql` | deletes the user; words, review state, log and category cascade |
| `run.sh` | the three steps; the default target is the `db-migration-postgres-1` container of `make up` |

`run.sh` needs the schema up to V27. If the database was not migrated by Flyway, apply
`db-migration/migrations/sql/V27__dictionary_review_fsrs.sql` first.

### 2.1 Seed

| Table | Rows | Notes |
|---|---|---|
| `dictionary_saved_words` | 1,000,000 | `review_started` true for the first 200,000; `created_at` = now minus `g` seconds |
| `dictionary_review_state` | 200,000 | 5,000 due 1 hour ago (distinct times); the rest due `g` minutes ahead; every 50th is `learning`; 300 reviewed 2 hours ago, the rest 3 days ago |
| `dictionary_review_log` | 2,000,000 | grade 3, `elapsed_seconds` 86400, spread over the 200,000 words |
| `dictionary_categories` / `dictionary_word_categories` | 1 / 100 | the category holds 100 never reviewed words |

### 2.2 What to expect and what came out

Environment of the numbers below: PostgreSQL 16, Apple M1 Pro, default configuration, one run, one user in the tables.
Compare the **shape of the plan**; milliseconds differ on every machine.

| # | Query | Expected plan | Seen | Execution |
|---|---|---|---|---|
| 1 | Due queue (`ReviewQueue`, `LIMIT 50`) | Index Scan `dictionary_review_state_due_idx` + `dictionary_saved_words_pkey`, no Seq Scan | as expected, reads 51 index entries | 0.44 ms |
| 2 | New queue (`LIMIT 10`) | Index Scan on `dictionary_saved_words_new_idx` | as expected | 0.06 ms |
| 3 | New queue of a category | no Seq Scan of `dictionary_saved_words` | starts from `dictionary_word_categories` (100 rows), 100 primary key lookups, top-N sort | 0.30 ms |
| 4 | Counts (`ReviewCounts`, cap 10000) | bounded by the cap; no Hash Join or Parallel Seq Scan over the words | total and new: Seq Scan stopped at 10,000 rows; due and learning: Bitmap Index Scan on `dictionary_review_state_due_idx` + primary key lookups (4,760 and 97); `next`: Index Scan, 1 row | 20.3 ms for the whole statement |
| 5 | Cards reviewed in 24 hours (`ReviewToday`) | Index Scan `dictionary_review_state_last_idx` | as expected, 300 rows | 1.0 ms |
| 6 | History for the optimizer (`ReviewLogs`) | Index Scan Backward on `dictionary_review_log_user_idx` for the 75,000 latest grades | as expected for the inner part; the join back is a Hash Join with a Seq Scan of the log and an external sort of 750,000 rows (30 MB on disk) | 1,445 ms |

Notes on each row:

- **1.** An earlier run of this query took 40.7 ms because the first seed gave all 5,000 due cards one identical `due_at`,
  so the incremental sort could not stop early. The seed now spreads them over time, which is how real due dates look.
- **2.** The partial index `(user_id, created_at, word) WHERE removed_at IS NULL AND NOT review_started` answers
  `ORDER BY created_at, word LIMIT n` directly. Before V27 this was an anti-join with a sort of the whole list.
- **3.** The plan starts from the small side because the category is small. A category of hundreds of thousands of words
  was not tried.
- **4.** The total and new counts are a Seq Scan only because this user is the only data in the table; the scan stops at
  the cap. On a shared table they should use `dictionary_saved_words_list_idx` and `dictionary_saved_words_new_idx`; that
  was not tested. See 2.3 for why the due count looks the way it does.
- **6.** In this seed the 75,000 latest grades touch 75,000 different words and each word has 10 grades, so "the whole
  history of those words" is 750,000 rows, more than a real user has per word. The Seq Scan is only cheap because this user
  is the only data in the table; with many users the planner can use the primary key prefix `(user_id, word, ...)` (not
  tested). Memory in the Go process is bounded by `capSamples` (at most 50,000 samples), not by the rows read.

### 2.3 The count query: a problem found and fixed

The first version of `ReviewCounts` counted due cards with a join:

```sql
SELECT count(*) FROM (
  SELECT 1 FROM dictionary_review_state r
  JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.word = r.word
  WHERE r.user_id = :u AND r.due_at <= now() AND NOT r.learning AND w.removed_at IS NULL
  LIMIT 10000) t;
```

The planner chose a **Parallel Hash Join with a Parallel Seq Scan of `dictionary_saved_words`** (333,333 rows x 3 workers,
8,259 buffers read), 76.9 ms for the whole statement. The cap does not help, because a hash join reads the whole table before
it returns a row, so the cost grows with the number of words: what the change was meant to remove. Writing the word check as
`EXISTS` in the `WHERE` gave the same hash semi join (49.5 ms).

The fix looks the word up in the select list, so the planner has no join to reorder:

```sql
SELECT count(*) FROM (
  SELECT 1 FROM (
    SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
                   WHERE w.user_id = r.user_id AND w.word = r.word AND w.removed_at IS NULL) AS listed
    FROM dictionary_review_state r
    WHERE r.user_id = :u AND r.due_at <= now() AND NOT r.learning) x
  WHERE listed LIMIT 10000) t;
```

The plan is an index scan of `dictionary_review_state_due_idx` with a SubPlan on the primary key. With a cap of 100 the outer
scan returned 100 rows and the SubPlan ran 100 times (0.56 ms): it stops at the cap. A form with `OFFSET 0` (to stop the
planner flattening) read all 4,900 due rows and ran the SubPlan 4,900 times, so the final form has no `OFFSET 0`.

`NOT learning` is a filter on an `INCLUDE` column, applied while scanning the index entries, not an index condition. A user
with very many due cards that are all in learning would make the scan read past them; learning cards due now are normally few.

Query 4 of `explain.sql` is this final form inside the whole `ReviewCounts` statement. Its plan must contain no Hash Join.

### 2.4 What this measurement does not show

- Real data: one synthetic user alone in the tables, so the planner choices that depend on other users' rows (the Seq Scans
  in rows 4 and 6) were not seen on a shared table.
- A very large category, a category-filtered due queue and counts with a category filter.
- One run each, no concurrency, no `GetReviewQueue` end-to-end benchmark.
