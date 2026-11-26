-- The review queries of internal/store (review.go, fsrs.go) with literal values, each under EXPLAIN ANALYZE.
-- "now" is now(); the queue and the counts look 20 minutes ahead (learnAhead in service/review.go).
-- Expected plans are in the comment above each query. Do not compare the milliseconds, compare the plan shapes.
\set u '''00000000-0000-0000-0000-0000000000aa'''
\set explain 'EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF)'

\echo
\echo '=== 1. due queue (ReviewQueue, due part, limit 50)'
-- expect: Index Scan on dictionary_review_state_due_idx + Index Scan on dictionary_saved_words_pkey, no Seq Scan
:explain
SELECT w.word, w.content, w.audio_uk, w.audio_us, r.ease, r.interval_days, r.reps, r.lapses, r.learning, r.due_at,
       r.stability, r.difficulty, r.last_reviewed_at
FROM dictionary_review_state r JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.word = r.word
WHERE r.user_id = :u AND r.due_at <= now() + interval '20 minutes' AND w.removed_at IS NULL
ORDER BY r.due_at, r.word LIMIT 50;

\echo
\echo '=== 2. new queue (ReviewQueue, new part, limit 10)'
-- expect: Index Only Scan on dictionary_saved_words_new_idx, a handful of buffers
:explain
SELECT w.word, w.content, w.audio_uk, w.audio_us
FROM dictionary_saved_words w
WHERE w.user_id = :u AND w.removed_at IS NULL AND NOT w.review_started
ORDER BY w.created_at, w.word LIMIT 10;

\echo
\echo '=== 3. new queue of a category (WordFilter.clause)'
-- expect: no Seq Scan of dictionary_saved_words; read the plan to see whether it starts from the category
-- (dictionary_word_categories) or walks dictionary_saved_words_new_idx and tests EXISTS per word
:explain
SELECT w.word FROM dictionary_saved_words w
WHERE w.user_id = :u AND w.removed_at IS NULL AND NOT w.review_started
  AND EXISTS (SELECT 1 FROM dictionary_word_categories wc WHERE wc.user_id = w.user_id AND wc.word = w.word
              AND wc.category_id = '00000000-0000-0000-0000-0000000000bb'::uuid)
ORDER BY w.created_at, w.word LIMIT 10;

\echo
\echo '=== 4. counts (ReviewCounts), cap 10000'
-- expect: every count is bounded by the cap. total/new: a Limit over a scan of dictionary_saved_words (it stops at 10000
-- rows; on this one-user table that is a Seq Scan, on a shared table it should be the partial index).
-- due/learning: Index Scan on dictionary_review_state_due_idx with SubPlan 1 = Index Scan on dictionary_saved_words_pkey.
-- There must be NO Hash Join and NO Parallel Seq Scan over the 1,000,000 words.
-- next: Index Scan on dictionary_review_state_due_idx, LIMIT 1.
:explain
SELECT
  (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL LIMIT 10000) t),
  (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL AND NOT w.review_started LIMIT 10000) t),
  (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
        WHERE w.user_id = r.user_id AND w.word = r.word AND w.removed_at IS NULL) AS listed
      FROM dictionary_review_state r WHERE r.user_id = :u AND r.due_at <= now() + interval '20 minutes' AND NOT r.learning) x
    WHERE listed LIMIT 10000) t),
  (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
        WHERE w.user_id = r.user_id AND w.word = r.word AND w.removed_at IS NULL) AS listed
      FROM dictionary_review_state r WHERE r.user_id = :u AND r.due_at <= now() + interval '20 minutes' AND r.learning) x
    WHERE listed LIMIT 10000) t),
  (SELECT r.due_at FROM dictionary_review_state r JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.word = r.word
    WHERE r.user_id = :u AND r.due_at > now() + interval '20 minutes' AND w.removed_at IS NULL ORDER BY r.due_at LIMIT 1);

\echo
\echo '=== 5. cards reviewed in the last 24 hours (ReviewToday)'
-- expect: Index Scan on dictionary_review_state_last_idx, 300 rows (the seed), then pkey lookups of dictionary_saved_words
:explain
SELECT count(*) FILTER (WHERE r.first_reviewed_at > now() - interval '24 hours'),
       count(*) FILTER (WHERE r.first_reviewed_at <= now() - interval '24 hours')
FROM dictionary_review_state r
JOIN dictionary_saved_words w ON w.user_id = r.user_id AND w.word = r.word AND w.removed_at IS NULL
WHERE r.user_id = :u AND r.last_reviewed_at > now() - interval '24 hours';

\echo
\echo '=== 6. history the optimizer reads (ReviewLogs, 75000 latest grades)'
-- expect: Index Scan Backward on dictionary_review_log_user_idx for the 75000 latest grades. The join back to the
-- log may be a Hash Join with a Seq Scan here, because this user is the only data in the table and a word has 10
-- grades; with many users the log should be read through the primary key.
:explain
WITH recent AS (
  SELECT DISTINCT word FROM (
    SELECT word FROM dictionary_review_log WHERE user_id = :u ORDER BY reviewed_at DESC LIMIT 75000) t)
SELECT l.word, l.reviewed_at, l.grade, l.elapsed_seconds
FROM recent JOIN dictionary_review_log l ON l.user_id = :u AND l.word = recent.word
ORDER BY l.word, l.reviewed_at;
