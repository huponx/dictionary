package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Settings returns every row of dictionary_settings by key; a setting without a row is not in the map.
func (s *Store) Settings(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM dictionary_settings`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			k string
			v []byte
		)
		if err := rows.Scan(&k, &v); err != nil {
			return nil, mapErr(err)
		}
		out[k] = json.RawMessage(v)
	}
	return out, mapErr(rows.Err())
}

// PutSettings upserts the given rows in one transaction; the other rows are left alone.
func (s *Store) PutSettings(ctx context.Context, rows map[string]json.RawMessage) error {
	if len(rows) == 0 {
		return nil
	}
	return mapErr(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for k, v := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO dictionary_settings (key, value, updated_at) VALUES ($1, $2::jsonb, now())
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, k, string(v)); err != nil {
				return err
			}
		}
		return nil
	}))
}
