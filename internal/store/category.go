package store

import (
	"context"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrCategoryExists: the user already has a category with that name (case-insensitive).
var ErrCategoryExists = errors.New("store: category exists")

// Category is a row of dictionary_categories with the number of words of the list in it.
type Category struct {
	ID   string
	Name string
	// Total: words of the list in the category; New: how many of them were never exported.
	Total, New int
}

// WordFilter narrows the words of the list by category. The zero value keeps every word.
type WordFilter struct {
	// CategoryID keeps the words in that category (an unknown or malformed id matches nothing).
	CategoryID string
	// Uncategorized keeps the words that are in no category; CategoryID is then ignored.
	Uncategorized bool
}

// clause is the condition (starting with " AND ") on the saved words table called alias, or "". It appends the
// argument it needs to args.
func (f WordFilter) clause(alias string, args *[]any) string {
	switch {
	case f.Uncategorized:
		return ` AND NOT ` + alias + `.categorized`
	case f.CategoryID != "":
		id, err := uuid.Parse(f.CategoryID)
		if err != nil {
			id = uuid.Nil
		}
		*args = append(*args, id.String())
		return ` AND EXISTS (SELECT 1 FROM dictionary_word_categories wc WHERE wc.user_id = ` + alias + `.user_id AND wc.word = ` + alias +
			`.word AND wc.category_id = $` + strconv.Itoa(len(*args)) + `::uuid)`
	}
	return ""
}

// wordSource is what a query of the list reads: the words of the user (alias w) that pass the filter, and the columns
// that order them. The words of one category are read from its links, whose index (user_id, category_id, created_at,
// word) gives them in order and stops at the LIMIT; every other filter reads the list itself. The links carry the
// created_at of their word (kept equal by the store), so both orders and both keys are the same.
type wordSource struct {
	from, where   string
	created, word string // the sort key columns
}

// source builds the source of the words for userID, which is $1 of the query; it appends the arguments it needs
// to args.
func (f WordFilter) source(args *[]any) wordSource {
	if f.CategoryID != "" && !f.Uncategorized {
		id, err := uuid.Parse(f.CategoryID)
		if err != nil {
			id = uuid.Nil
		}
		*args = append(*args, id.String())
		return wordSource{
			from: `dictionary_word_categories wc JOIN dictionary_saved_words w ON w.user_id = wc.user_id AND w.word = wc.word`,
			// w.user_id = $1 is redundant, but without it the planner joins the words by the trigram index (4 ms a word)
			where:   `wc.user_id = $1 AND w.user_id = $1 AND wc.category_id = $` + strconv.Itoa(len(*args)) + `::uuid AND w.removed_at IS NULL`,
			created: "wc.created_at", word: "wc.word",
		}
	}
	return wordSource{
		from:    `dictionary_saved_words w`,
		where:   `w.user_id = $1 AND w.removed_at IS NULL` + f.clause("w", args),
		created: "w.created_at", word: "w.word",
	}
}

// categoryCountCap bounds the counts of one category in ListCategories. It is lower than countCap because the page
// of the categories asks for 2 counts of every category (up to 100), each word costing a lookup: 100 categories of
// 9,000 words took 10 s with the cap of 10,000. The web shows "1000+" (CATEGORY_COUNT_CAP in api.ts).
const categoryCountCap = 1000

// Categories is the result of ListCategories.
type Categories struct {
	List []Category
	// UncatTotal, UncatNew: the words of the list in no category (total, new).
	UncatTotal, UncatNew int
	// Capped: a count stopped at its cap (categoryCountCap for a category, countCap for the uncategorized words), so
	// it is a lower bound.
	Capped bool
}

// ListCategories returns the categories of the user by name, and the number of words (total, new) of the list in
// no category. The counts of a category stop at categoryCountCap and the ones of the uncategorized words at countCap,
// so the cost is bounded by that and the number of categories, not by the size of the list.
func (s *Store) ListCategories(ctx context.Context, userID string) (Categories, error) {
	// Per category, a bounded range scan on the links of the category (index user_id, category_id). The word is
	// looked up with a subplan per link, not joined: a join lets the planner hash the whole list of the user for
	// every category (17 s for 100 categories of 1,000,000 words), a subplan stops at the cap.
	count := func(extra string) string {
		return `(SELECT count(*) FROM (SELECT 1 FROM (SELECT EXISTS (SELECT 1 FROM dictionary_saved_words w
				WHERE w.user_id = wc.user_id AND w.word = wc.word AND w.removed_at IS NULL` + extra + `) AS listed
			FROM dictionary_word_categories wc WHERE wc.user_id = c.user_id AND wc.category_id = c.id) x WHERE listed LIMIT $2) t)`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id::text, c.name, `+count("")+`, `+count(" AND w.exported_at IS NULL")+`
		FROM dictionary_categories c WHERE c.user_id = $1 ORDER BY lower(c.name), c.id`, userID, categoryCountCap)
	if err != nil {
		return Categories{}, mapErr(err)
	}
	defer rows.Close()
	var out Categories
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Total, &c.New); err != nil {
			return Categories{}, err
		}
		out.Capped = out.Capped || c.Total >= categoryCountCap || c.New >= categoryCountCap
		out.List = append(out.List, c)
	}
	if err := rows.Err(); err != nil {
		return Categories{}, err
	}
	rows.Close()
	var capped bool
	out.UncatTotal, out.UncatNew, capped, err = s.CountPending(ctx, userID, WordFilter{Uncategorized: true})
	out.Capped = out.Capped || capped
	return out, err
}

// CategoryName returns the name of the category of the user; ErrNotFound for an unknown id.
func (s *Store) CategoryName(ctx context.Context, userID, id string) (string, error) {
	cid, err := uuid.Parse(id)
	if err != nil {
		return "", ErrNotFound
	}
	var name string
	err = s.pool.QueryRow(ctx, `SELECT name FROM dictionary_categories WHERE user_id = $1 AND id = $2`, userID, cid).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, mapErr(err)
}

// CreateCategory adds a category; ErrLimit when the user already has max, ErrCategoryExists for a name in use.
func (s *Store) CreateCategory(ctx context.Context, userID, name string, max int) (Category, error) {
	c := Category{Name: name}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "dictionary-categories:"+userID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_categories WHERE user_id = $1`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return ErrLimit
		}
		return tx.QueryRow(ctx, `INSERT INTO dictionary_categories (user_id, name) VALUES ($1, $2) RETURNING id::text`,
			userID, name).Scan(&c.ID)
	})
	return c, mapErr(err)
}

// RenameCategory renames the category of the user; ErrNotFound for an unknown id, ErrCategoryExists for a name
// another category of the user has.
func (s *Store) RenameCategory(ctx context.Context, userID, id, name string) (Category, error) {
	cid, err := uuid.Parse(id)
	if err != nil {
		return Category{}, ErrNotFound
	}
	c := Category{ID: cid.String()}
	err = s.pool.QueryRow(ctx, `UPDATE dictionary_categories SET name = $3 WHERE user_id = $1 AND id = $2 RETURNING name`,
		userID, cid, name).Scan(&c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	return c, mapErr(err)
}

// unlinkBatch is how many links DeleteCategory removes per transaction.
const unlinkBatch = 5000

// DeleteCategory deletes the category (the words stay in the list); ErrNotFound for an unknown id. The links of a
// big category go in batches of unlinkBatch, each with the categorized flag of its words in one transaction, so the
// flag always matches the links and no transaction is huge; the call returns when the last batch is done.
func (s *Store) DeleteCategory(ctx context.Context, userID, id string) error {
	cid, err := uuid.Parse(id)
	if err != nil {
		return ErrNotFound
	}
	if _, err := s.CategoryName(ctx, userID, id); err != nil {
		return err
	}
	for {
		var n int
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `DELETE FROM dictionary_word_categories
				WHERE (user_id, word, category_id) IN (
					SELECT user_id, word, category_id FROM dictionary_word_categories WHERE user_id = $1 AND category_id = $2 LIMIT $3)
				RETURNING word`, userID, cid, unlinkBatch)
			if err != nil {
				return err
			}
			words, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			n = len(words)
			return refreshCategorized(ctx, tx, userID, words)
		})
		if err != nil {
			return mapErr(err)
		}
		if n < unlinkBatch {
			break
		}
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM dictionary_categories WHERE user_id = $1 AND id = $2`, userID, cid)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// refreshCategorized sets the categorized flag of the words from their links.
func refreshCategorized(ctx context.Context, tx pgx.Tx, userID string, words []string) error {
	if len(words) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE dictionary_saved_words w SET categorized = EXISTS (
			SELECT 1 FROM dictionary_word_categories wc WHERE wc.user_id = w.user_id AND wc.word = w.word)
		WHERE w.user_id = $1 AND w.word = ANY($2)`, userID, words)
	return err
}

// SetWordCategories adds the words to the categories in add and takes them out of the ones in remove. Words that
// are not in the list are skipped; it returns how many were. ErrNotFound when a category is not the user's.
func (s *Store) SetWordCategories(ctx context.Context, userID string, words, add, remove []string) (int, error) {
	addIDs, err := parseIDs(add)
	if err != nil {
		return 0, err
	}
	removeIDs, err := parseIDs(remove)
	if err != nil {
		return 0, err
	}
	var updated int
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := ownCategories(ctx, tx, userID, append(append([]string{}, addIDs...), removeIDs...)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT word FROM dictionary_saved_words WHERE user_id = $1 AND removed_at IS NULL AND word = ANY($2)`,
			userID, words)
		if err != nil {
			return err
		}
		listed, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		updated = len(listed)
		if len(listed) == 0 {
			return nil
		}
		if len(addIDs) > 0 {
			// the link carries the created_at of its word, see wordSource
			if _, err := tx.Exec(ctx, `INSERT INTO dictionary_word_categories (user_id, word, category_id, created_at)
				SELECT $1, w.word, c, w.created_at FROM dictionary_saved_words w, unnest($3::uuid[]) c
				WHERE w.user_id = $1 AND w.word = ANY($2) ON CONFLICT DO NOTHING`, userID, listed, addIDs); err != nil {
				return err
			}
		}
		if len(removeIDs) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM dictionary_word_categories
				WHERE user_id = $1 AND word = ANY($2) AND category_id = ANY($3::uuid[])`, userID, listed, removeIDs); err != nil {
				return err
			}
		}
		return refreshCategorized(ctx, tx, userID, listed)
	})
	return updated, mapErr(err)
}

// parseIDs checks the ids are UUIDs (ErrNotFound otherwise) and drops the duplicates.
func parseIDs(ids []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, ErrNotFound
		}
		if !seen[u.String()] {
			seen[u.String()] = true
			out = append(out, u.String())
		}
	}
	return out, nil
}

// ownCategories returns ErrNotFound unless every id is a category of the user.
func ownCategories(ctx context.Context, tx pgx.Tx, userID string, ids []string) error {
	ids, err := parseIDs(ids)
	if err != nil || len(ids) == 0 {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dictionary_categories WHERE user_id = $1 AND id = ANY($2::uuid[])`,
		userID, ids).Scan(&n); err != nil {
		return err
	}
	if n != len(ids) {
		return ErrNotFound
	}
	return nil
}

// addCategories links the word to the categories (the ones it has stay).
func addCategories(ctx context.Context, tx pgx.Tx, userID, word string, ids []string) error {
	ids, err := parseIDs(ids)
	if err != nil || len(ids) == 0 {
		return err
	}
	if err := ownCategories(ctx, tx, userID, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO dictionary_word_categories (user_id, word, category_id, created_at)
		SELECT $1, $2, unnest($3::uuid[]), (SELECT created_at FROM dictionary_saved_words WHERE user_id = $1 AND word = $2)
		ON CONFLICT DO NOTHING`, userID, word, ids); err != nil {
		return err
	}
	return refreshCategorized(ctx, tx, userID, []string{word})
}

// categoryIDs returns the category ids of the words, by word.
func (s *Store) categoryIDs(ctx context.Context, userID string, words []string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT word, category_id::text FROM dictionary_word_categories
		WHERE user_id = $1 AND word = ANY($2) ORDER BY category_id`, userID, words)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var w, id string
		if err := rows.Scan(&w, &id); err != nil {
			return nil, err
		}
		out[w] = append(out[w], id)
	}
	return out, rows.Err()
}
