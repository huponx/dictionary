-- Seed one user with 1,000,000 words, 200,000 reviewed (5,000 due now), a 2,000,000 row review log and 100 categories
-- (a big one of 300,000 words, a small one of 100, 98 of about 6,000). Needs the schema up to V32.
--   words 1..1000000   w<g>, created g seconds ago (so w1 is the newest)
--   600001..800000     exported and out of the list (removed_at): 200,000
--   500001..600000     exported, still in the list (remove after export is off): 100,000
--   1..900100          have a category (90%, categorized = true); 900101..1000000 have none Safe to run again: it deletes the seed user first.
\set ON_ERROR_STOP on
\set u '''00000000-0000-0000-0000-0000000000aa'''

DELETE FROM identity_users WHERE id = :u;
INSERT INTO identity_users (id, email) VALUES (:u, 'review-seed@dictionary.test');

INSERT INTO dictionary_saved_words (user_id, word, content, created_at, review_started, exported_at, removed_at, categorized)
SELECT :u, 'w'||g, 'x', now() - (g||' seconds')::interval, g <= 200000,
  CASE WHEN g BETWEEN 500001 AND 800000 THEN now() - interval '1 day' END,
  CASE WHEN g BETWEEN 600001 AND 800000 THEN now() - interval '1 day' END,
  g <= 900100
FROM generate_series(1,1000000) g;

INSERT INTO dictionary_user_stats (user_id, listed) VALUES (:u, 800000)
ON CONFLICT (user_id) DO UPDATE SET listed = EXCLUDED.listed;

INSERT INTO dictionary_review_state (user_id, word, due_at, first_reviewed_at, last_reviewed_at, learning, stability, difficulty)
SELECT :u, 'w'||g,
  CASE WHEN g <= 5000 THEN now() - interval '1 hour' + (g||' seconds')::interval ELSE now() + (g||' minutes')::interval END,
  now() - interval '30 days',
  -- 300 cards were reviewed in the last 24 hours (so the "today" query has rows), the rest 3 days ago
  CASE WHEN g BETWEEN 5001 AND 5300 THEN now() - interval '2 hours' ELSE now() - interval '3 days' END,
  (g % 50 = 0), 5, 5
FROM generate_series(1,200000) g;

INSERT INTO dictionary_review_log (user_id, word, reviewed_at, grade, elapsed_seconds)
SELECT :u, 'w'||(g % 200000 + 1), now() - (g||' seconds')::interval, 3, 86400
FROM generate_series(1,2000000) g ON CONFLICT DO NOTHING;

-- a category of 100 new (never reviewed) words
INSERT INTO dictionary_categories (id, user_id, name) VALUES ('00000000-0000-0000-0000-0000000000bb', :u, 'seed');
INSERT INTO dictionary_word_categories (user_id, word, category_id, created_at)
SELECT :u, 'w'||g, '00000000-0000-0000-0000-0000000000bb', now() - (g||' seconds')::interval FROM generate_series(900001, 900100) g;

-- the big category (300,000 words) and 98 of about 6,000
INSERT INTO dictionary_categories (id, user_id, name) VALUES ('00000000-0000-0000-0000-0000000000b0', :u, 'big');
INSERT INTO dictionary_word_categories (user_id, word, category_id, created_at)
SELECT :u, 'w'||g, '00000000-0000-0000-0000-0000000000b0', now() - (g||' seconds')::interval FROM generate_series(1, 300000) g;
INSERT INTO dictionary_categories (id, user_id, name)
SELECT ('00000000-0000-0000-0000-0000000001' || lpad(c::text, 2, '0'))::uuid, :u, 'cat ' || c FROM generate_series(1, 98) c;
INSERT INTO dictionary_word_categories (user_id, word, category_id, created_at)
SELECT :u, 'w'||g, ('00000000-0000-0000-0000-0000000001' || lpad((g % 98 + 1)::text, 2, '0'))::uuid, now() - (g||' seconds')::interval
FROM generate_series(300001, 900000) g;

ANALYZE dictionary_saved_words;
ANALYZE dictionary_categories;
ANALYZE dictionary_user_stats;
ANALYZE dictionary_review_state;
ANALYZE dictionary_review_log;
ANALYZE dictionary_word_categories;
