-- The list, count, category, save and export queries of internal/store (store.go, category.go, export.go) with literal
-- values, each under EXPLAIN ANALYZE. Expected plans are in the comment above each query. Compare the plan shapes
-- (an index scan by user_id that stops at the LIMIT, no Seq Scan of dictionary_saved_words), not the milliseconds.
-- The statements that write run in a transaction that is rolled back.
\set u '''00000000-0000-0000-0000-0000000000aa'''
\set big '''00000000-0000-0000-0000-0000000000b0'''
\set small '''00000000-0000-0000-0000-0000000000bb'''
\set mid '''00000000-0000-0000-0000-000000000101'''
\set explain 'EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF)'

\echo
\echo '=== L1. ListPending, first page (no filter, no query)'
-- expect: Index Scan on dictionary_saved_words_list_idx (or a similar index of user_id, created_at), stops at 51
:explain
SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words w
WHERE user_id = :u AND removed_at IS NULL ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L2. ListPending, deep page (keyset after w450000)'
-- expect: the same index scan, starting at the key; the cost does not grow with the depth
:explain
SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words w
WHERE user_id = :u AND removed_at IS NULL
  AND (created_at, word) < ((SELECT created_at FROM dictionary_saved_words WHERE user_id = :u AND word = 'w450000'), 'w450000')
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L3a. ListPending with a query of 1 character (7: matches about a third of the words)'
-- expect: stops at 51 matches; a Seq Scan is a failure. 1 and 2 characters have no trigram, so this walks the list index
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND word ILIKE '%' || '7' || '%' ESCAPE '\'
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L3b. ListPending with a query of 2 characters (77)'
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND word ILIKE '%' || '77' || '%' ESCAPE '\'
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L3c. ListPending with a query of 3 or more characters (7777)'
-- expect: Bitmap Index Scan on dictionary_saved_words_word_trgm_idx
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND word ILIKE '%' || '7777' || '%' ESCAPE '\'
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L3d. ListPending with a rare query (999999: one word)'
-- expect: the trigram index finds it at once; walking the list index would read everything
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND word ILIKE '%' || '999999' || '%' ESCAPE '\'
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L3e. ListPending with a query of 2 characters that matches nothing (zz)'
-- the worst case without a trigram: nothing to stop the walk, so it reads the whole list index
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND word ILIKE '%' || 'zz' || '%' ESCAPE '\'
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== L4a. ListPending, big category (300,000 words)'
-- expect: Index Scan on dictionary_word_categories_list_idx (the links in order) + a primary-key lookup per word, stops at 51
:explain
SELECT w.word FROM dictionary_word_categories wc JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word
WHERE wc.user_id = :u AND w.user_id = :u AND wc.category_id = :big::uuid AND w.removed_at IS NULL
ORDER BY wc.created_at DESC, wc.word DESC LIMIT 51;

\echo
\echo '=== L4b. ListPending, small category (100 words, all among the oldest)'
-- the words of the category are old: walking the list newest first would read about 900,000 words before the first
:explain
SELECT w.word FROM dictionary_word_categories wc JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word
WHERE wc.user_id = :u AND w.user_id = :u AND wc.category_id = :small::uuid AND w.removed_at IS NULL
ORDER BY wc.created_at DESC, wc.word DESC LIMIT 51;

\echo
\echo '=== L4d. ListPending, a category of 6,000 words spread over the list (cat 1)'
-- the words of the category are all older than 300,000 others: the links give them in order, 52 lookups
:explain
SELECT w.word FROM dictionary_word_categories wc JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word
WHERE wc.user_id = :u AND w.user_id = :u AND wc.category_id = :mid::uuid AND w.removed_at IS NULL
ORDER BY wc.created_at DESC, wc.word DESC LIMIT 51;

\echo
\echo '=== L4c. ListPending, Uncategorized'
-- expect: Index Scan on dictionary_saved_words_uncategorized_idx (partial: NOT categorized), stops at 51
:explain
SELECT word FROM dictionary_saved_words w WHERE user_id = :u AND removed_at IS NULL AND NOT w.categorized
ORDER BY created_at DESC, word DESC LIMIT 51;

\echo
\echo '=== C1. CountPending, no filter: the counter + the capped count of new words'
-- expect: Index Scan on dictionary_user_stats_pkey; the new count is an index scan that stops at 10000
:explain SELECT listed FROM dictionary_user_stats WHERE user_id = :u;
:explain
SELECT (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL AND w.exported_at IS NULL LIMIT 10000) t);

\echo
\echo '=== C2. CountPending, big category (total and new, capped at 1000)'
:explain
SELECT (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL AND w.exported_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :big::uuid) x WHERE listed LIMIT 1000) t),
       (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :big::uuid) x WHERE listed LIMIT 1000) t);

\echo
\echo '=== C3. CountPending, small category (100 words)'
-- the count has to find all of them: it reads the list until the end unless it starts from the category
:explain
SELECT (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL AND w.exported_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :small::uuid) x WHERE listed LIMIT 1000) t),
       (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :small::uuid) x WHERE listed LIMIT 1000) t);

\echo
\echo '=== C5. CountPending, a category of 6,000 words (cat 1)'
:explain
SELECT (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL AND w.exported_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :mid::uuid) x WHERE listed LIMIT 1000) t),
       (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
            WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL) AS listed
          FROM dictionary_word_categories wc WHERE wc.user_id = :u AND wc.category_id = :mid::uuid) x WHERE listed LIMIT 1000) t);

\echo
\echo '=== C4. CountPending, Uncategorized (99,900 words)'
:explain
SELECT (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL AND w.exported_at IS NULL AND NOT w.categorized LIMIT 10000) t),
       (SELECT count(*) FROM (SELECT 1 FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL AND NOT w.categorized LIMIT 10000) t);

\echo
\echo '=== K1. ListCategories (100 categories, total and new, capped at 1000 each)'
-- expect: per category an index scan on dictionary_word_categories_category_idx with a primary-key lookup per link
-- (a subplan); a Seq Scan or a hash of dictionary_saved_words per category took 17 s with a join
:explain
SELECT c.id::text, c.name,
  (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
      WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL) AS listed
    FROM dictionary_word_categories wc WHERE wc.user_id = c.user_id AND wc.category_id = c.id) x WHERE listed LIMIT 1000) t),
  (SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
      WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL AND w.exported_at IS NULL) AS listed
    FROM dictionary_word_categories wc WHERE wc.user_id = c.user_id AND wc.category_id = c.id) x WHERE listed LIMIT 1000) t)
FROM dictionary_categories c WHERE c.user_id = :u ORDER BY lower(c.name), c.id;

\echo
\echo '=== S0. SaveWord before: the two count(*) of the list it replaced (for comparison)'
:explain SELECT count(*) FROM dictionary_saved_words WHERE user_id = :u AND removed_at IS NULL;

\echo
\echo '=== S1. SaveWord now: the statements of the transaction (new word, then the same word again)'
-- expect: every statement is a primary-key or index lookup, a few buffers
BEGIN;
:explain SELECT true FROM dictionary_saved_words WHERE user_id = :u AND word = 'brand-new' AND removed_at IS NULL;
:explain INSERT INTO dictionary_user_stats (user_id) VALUES (:u) ON CONFLICT (user_id) DO NOTHING;
:explain UPDATE dictionary_user_stats SET listed = listed + 1 WHERE user_id = :u AND listed < 1000000 RETURNING listed;
:explain
INSERT INTO dictionary_saved_words (user_id, word, content, audio_uk, audio_us) VALUES (:u, 'brand-new', 'c', '', '')
ON CONFLICT (user_id, word) DO UPDATE SET content = EXCLUDED.content, created_at = now(), exported_at = NULL, removed_at = NULL;
:explain SELECT listed FROM dictionary_user_stats WHERE user_id = :u;
ROLLBACK;

\echo
\echo '=== R1. RemoveExported, one batch of 5,000 (500001..600000 are exported and listed)'
-- expect: the batch is found through an index (not a Seq Scan of the list); WAL and locks are those of 5,000 rows
BEGIN;
:explain
WITH b AS MATERIALIZED (
  SELECT word FROM dictionary_saved_words WHERE user_id = :u AND removed_at IS NULL AND exported_at IS NOT NULL LIMIT 5000)
UPDATE dictionary_saved_words w SET removed_at = now() FROM b
WHERE w.user_id = :u AND w.word = b.word AND w.removed_at IS NULL;
ROLLBACK;

\echo
\echo '=== E1. Export job: PendingPage, the first batch of 2,000 (scope new, oldest first)'
-- expect: an index scan from the oldest word that stops at 2000
:explain
SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words w
WHERE user_id = :u AND removed_at IS NULL AND exported_at IS NULL ORDER BY created_at, word LIMIT 2000;

\echo
\echo '=== E2. Export job: PendingPage, a later batch (after w300000, scope new)'
:explain
SELECT word, content, audio_uk, audio_us, created_at, exported_at IS NOT NULL FROM dictionary_saved_words w
WHERE user_id = :u AND removed_at IS NULL AND exported_at IS NULL
  AND (created_at, word) > ((SELECT created_at FROM dictionary_saved_words WHERE user_id = :u AND word = 'w300000'), 'w300000')
ORDER BY created_at, word LIMIT 2000;

\echo
\echo '=== E3. Export job: PendingPage of a category (big), the first batch'
:explain
SELECT w.word FROM dictionary_word_categories wc JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word
WHERE wc.user_id = :u AND w.user_id = :u AND wc.category_id = :big::uuid AND w.removed_at IS NULL AND w.exported_at IS NULL
ORDER BY wc.created_at, wc.word LIMIT 2000;

\echo
\echo '=== E4. Export job: CountExportable (exact, once per job)'
:explain SELECT count(*) FROM dictionary_saved_words w WHERE w.user_id = :u AND w.removed_at IS NULL AND w.exported_at IS NULL;
