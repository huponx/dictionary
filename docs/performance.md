# Performance of the list, categories, save and export at 1,000,000 words

Measured with `docs/performance/` (`./run.sh`, or `ONLY=list ./run.sh`): one user with 1,000,000 words (200,000 exported
and removed, 100,000 exported and listed), 100 categories (300,000 / 6,000 x 98 / 100 words), 90% categorized, on the
local Postgres of `make up` (migrated to V33). Milliseconds are from one run on a laptop: compare plan shapes, not times.
Counts stop at 10,000 rows (`countCap`); the counts of one category stop at 1,000 (`categoryCountCap`).

| Query | Plan | Time |
|---|---|---|
| ListPending, first page, deep page (keyset) | Index Scan `list_idx`, stops at 51 | 0.2 ms |
| query of 1 / 2 characters | Index Only Scan `list_idx`, filter | 0.2 / 1.1 ms |
| query of 3+ characters, rare query | Bitmap Index Scan `word_trgm_idx` | 6 ms |
| **query of 2 characters that matches nothing** | Parallel Seq Scan (no trigram, nothing stops the walk) | **94 ms** |
| category of 300,000 / 6,000 / 100 words | Index Only Scan `word_categories_list_idx` (the links in order) + a primary-key lookup per word | 0.9 / 0.9 / 0.2 ms |
| Uncategorized (list) | Index Only Scan `uncategorized_idx` | 0.1 ms |
| CountPending unfiltered (counter + capped new) | counter row + Index Only Scan `unexported_idx` to 10,000 | 1 ms |
| CountPending, category 300,000 / 6,000 / 100 (cap 1,000) | a lookup per link (subplan) | 10 / 20 / 0.5 ms |
| CountPending, Uncategorized | capped index scans | 3 ms |
| **ListCategories, 100 categories of 9,000 links (cap 1,000)** | 100 x 2 x 1,000 primary-key lookups | **770 ms** |
| SaveWord (statements of the transaction) | key lookups only | 0.05 to 0.75 ms; the two count(*) it replaced: 2 x 46 ms |
| RemoveExported, one batch of 5,000 | `exported_idx` + primary key (`MATERIALIZED`) | 36 ms |
| Export job: batch of 2,000, first and later | Index Scan Backward `list_idx` | 0.7 ms |
| Export job: batch of a category of 300,000 | the links in order | 27 ms |
| CountExportable (once per job) | Parallel Index Only Scan | 26 ms |

## How to run it again by hand

Needs the containers of `make up` (Postgres `db-migration-postgres-1`) with the schema migrated to V33 (`make -C
db-migration migrate`). From `dictionary/docs/performance`:

```bash
./run.sh                 # seed (about a minute), EXPLAIN of the review and the list queries, remove the seed
ONLY=list ./run.sh       # only explain-list.sql (this document); ONLY=review for explain-review.sql
KEEP=1 ./run.sh          # keep the seed afterwards, to try queries by hand
PSQL="psql postgres://user:pass@host/db" ./run.sh   # another database instead of the container
```

By hand with a kept seed (the user is `00000000-0000-0000-0000-0000000000aa`):

```bash
docker exec -i db-migration-postgres-1 psql -U postgres -d app < explain-list.sql > /tmp/out.txt   # one run, all queries
grep -E "^=== |Execution Time|Seq Scan|Hash Join" /tmp/out.txt                                   # titles, times and the bad shapes
docker exec -i db-migration-postgres-1 psql -U postgres -d app -q < cleanup.sql                    # remove the seed
```

| File | What it does |
|---|---|
| `seed.sql` | one user with 1,000,000 words, 200,000 reviewed, a 2,000,000 row review log, 100 categories, 300,000 exported and removed or kept; ends with `ANALYZE`; safe to run again (it deletes the seed user first) |
| `explain-review.sql` | the review queries (`store/review.go`, `fsrs.go`) under `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF)` |
| `explain-list.sql` | the queries of this document (rows L, C, K, S, R, E in the same order as the table); the expected plan is a comment above each query; the statements that write run in a transaction that is rolled back |
| `cleanup.sql` | deletes the seed user; everything it owns cascades |
| `run.sh` | seed, explain, cleanup |

How to read it: each section prints `=== <name>`, the plan and `Execution Time`. A good plan is an index scan on
`user_id` that stops at the `LIMIT` (the shapes in the table above). Look for a `Seq Scan on dictionary_saved_words`, a
`Hash Join` over the whole list, or a `Bitmap Index Scan on dictionary_saved_words_word_trgm_idx` where the primary key
should be used: those are the regressions this document is about. Compare the plans, not the milliseconds (the first run
after a seed reads from disk; run it twice).

Things to know:

* **Do not leave the seed in the database while you run the Go tests** (`go test ./...` with
  `DICTIONARY_TEST_POSTGRES_URL`): the cleanup tests delete rows of every user and the seed has 200,000 removed words, so
  they fail or delete the seed. `./run.sh` without `KEEP` removes it; otherwise run `cleanup.sql`.
* The seed is added to the database of `make up` next to your own data, under its own user, and `cleanup.sql` removes
  only that user. Nothing else is touched.
* The Go side (`SaveWord`, an export job of 20,000 words: time and memory) is not in these scripts, see "Not measured".
* To test the export job in the running app, not the database, see `make dict-seed` (`LOCAL.md`).

## What these measurements changed

* `RemoveExported`: a batch read the whole table (190 ms, a Seq Scan of every user's words). New index
  `dictionary_saved_words_exported_idx` (V31) and the batch is a `MATERIALIZED` CTE joined by primary key (36 ms).
* The words of one category are read from its links (`dictionary_word_categories.created_at` with the index
  `(user_id, category_id, created_at DESC, word DESC)`, V32), not from the list with an EXISTS per word: a category of old
  words took 450 ms (listing) and 380 ms (export batch), now 1 and 27 ms. The links carry the `created_at` of their word;
  the store keeps them equal (a word saved again updates its links, a new link copies it) and a test checks it.
* The query joins the words with `w.user_id = $1` written out: without it the planner joined by the trigram index
  (4 ms a word, 236 ms a page).
* `ListCategories`: 17.7 s with a join (it hashed the whole list per category), 9.9 s with a lookup per link and the cap of
  10,000, 770 ms with the cap of 1,000 (`CATEGORY_COUNT_CAP` in the web). The counts of a category filter use the same cap.
* The capped count of the new words of Uncategorized has its own partial index (V31).

## Open

1. **ListCategories** is bound by `categories x 2 x cap` lookups: 770 ms for 100 categories of 9,000 links, the
   extreme of the seed; a user with a few small categories pays milliseconds, and the 30 s cache hides repeats. Counters per
   category (kept with save, delete, export, remove, link, unlink) would make it constant.
2. **A query of 1 or 2 characters that matches nothing** reads the whole list (94 ms, growing with it): there is no trigram
   for it and nothing stops the walk. Requiring 3 characters for the search removes it.
3. A category whose count reached its cap shows "1000+", and the export dialog cannot know how many of its words were
   exported already. It offers "Include words exported before" without a number (the box is shown when the count of
   the new words is at its cap). A category with exactly 1,000 new words also shows "1000+".

## Not measured

Go benchmarks of `SaveWord` at 1,000,000 words and of an export job of 20,000 words (time, peak memory): the job's
database side is the E1 to E4 rows; building the decks (images, audio, zip) was not profiled.
