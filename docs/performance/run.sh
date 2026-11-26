#!/bin/sh
# Seeds the database, prints the EXPLAIN of the review queries (explain-review.sql) and of the list, count, category,
# save and export queries (explain-list.sql), removes the seed.
#   ./run.sh                      uses the container of `make up` in db-migration
#   PSQL="psql postgres://..." ./run.sh   uses any other database (migrated to V30)
# KEEP=1 keeps the seed afterwards. ONLY=list or ONLY=review runs one of the two files.
set -e
cd "$(dirname "$0")"
PSQL=${PSQL:-docker exec -i db-migration-postgres-1 psql -U postgres -d app}
$PSQL -q < seed.sql
[ "$ONLY" = list ] || $PSQL < explain-review.sql
[ "$ONLY" = review ] || $PSQL < explain-list.sql
[ -n "$KEEP" ] || $PSQL -q < cleanup.sql
