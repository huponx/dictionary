# dictionary

Dictionary service: a gRPC API to look words up in the dictionaries of a registry (English definitions, English words with Vietnamese meanings, and any dictionary added later, see "Dictionaries, groups and the registry"), to keep a per-user list of saved words and to export that list as an Anki deck (`.apkg`).

Module: `github.com/huponx/dictionary`. Contracts live in `protos` (`dictionary/v1`); generated stubs in `protogen`. Shared libraries come from `gocommon`. It replaces `old/x-word`, which scraped Cambridge (now behind Cloudflare) and kept the saved words in files.

## Architecture

```
browser --> gatekeeper --token exchange (aud dictionary)--> identity
                |
                +--gRPC + JWT--> dictionary --> PostgreSQL (saved words)
                                    |
                                    +--> Free Dictionary API, Vietnamese Wiktionary, MyMemory (lookups)
                                    +--> audio files (Anki export)
```

## Processes

| Process | Command | Runs | Scale and memory |
| --- | --- | --- | --- |
| API | `cmd/dictionary` (image `dictionary`, chart `dictionary`) | the gRPC server: lookups, the list, categories, review, uploads, the synchronous export, the stream of the parts of a job | light: 256Mi is enough |
| Worker | `cmd/dictionary-worker` (image `dictionary-worker`, chart `dictionary-worker`) | the export jobs (`RunExportWorker`) and the cleanup (expired words, review log, counters, unused and orphan files, expired jobs and deck links) | a job holds 2 to 3 times the size of its part in memory (up to 50 MiB), two jobs per replica: 1Gi by default; scales on its own |

Both read and write the same Postgres and the same volume (`DICTIONARY_FILES_DIR`: the worker writes `exports/` and reads `images/` and `audio/` to build a deck and deletes files; the API streams `exports/` and writes the uploads). There is no switch to run the jobs inside the API: **without a worker an export job stays `queued`** and nothing is cleaned. Several replicas of the worker are safe (a lease per job, DELETE in batches with `SKIP LOCKED`). When a worker stops in the middle of a job the lease (60 s) runs out and another replica goes on after the last part. The worker has no gRPC port and needs no identity settings: it listens on `HEALTH_ADDR` (default `:8081`) for `/healthz` and `/readyz` (Postgres).

**Storage:** the two pods share the claim `dictionary-files`, a hostpath ReadWriteOnce volume: it works while every pod runs on the same node (the one-node cluster of today). On more nodes it needs a ReadWriteMany class or object storage. The variables of the export limits, the files and the cleanup are the same in both processes; set the ceilings (`DICTIONARY_EXPORT_MAX_BYTES_CEILING`, `DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING`) to the same value in both, because each one reads the system settings with its own ceiling.

```bash
make run          # the API (reads the environment, see .env.example)
make run-worker   # the worker (same environment; HEALTH_ADDR=:8083 if 8081 is taken)
make docker-worker
```

The API is one process, `cmd/dictionary`, gRPC only. It never sees a password or a session: gatekeeper exchanges the session of the user for a short JWT whose audience is `dictionary`, and dictionary verifies it with the keys of identity (`DICTIONARY_JWKS_URL`). The user of every call is the `sub` of that token, never a field of a request.

## API

`DictionaryService` (see `dictionary/v1` in the protos repo). Every method needs a permission; the server is deny by default (`grpcserver.WithMethodPermissions`; the rules are the `authz.v1.rule` options of the RPCs in `protos/dictionary/v1/dictionary.proto`, `internal/service` `Permissions` reads them, `TestCoverage` fails an RPC without one).

| RPC | Permission | What it does |
| --- | --- | --- |
| `ListDictionaries` | `dictionary:word:read` | The dictionaries of the registry by group (`id`, `group`, `source`, `target`) with `enabled`: on for the system and for the caller |
| `LookupWord` | `dictionary:word:read` | Entry of one word in the dictionary `dictionary` (an id of the registry, required: `UNKNOWN_DICTIONARY` otherwise) |
| `LookupWords` | `dictionary:word:read` | 1 to 20 words (same `dictionary` id); words without an entry come back in `not_found` |
| `ListSavedWords` | `dictionary:word:read` | The words of the list (new ones and exported ones that were kept), newest first, with `exported` per word, `total` and `new_total`; token pagination (`page_size`, `page_token`) |
| `SaveWord` | `dictionary:word:write` | Adds or refreshes a word of the list (word, card back HTML, audio: an https URL on an allowed host or `dictaud:<id>` of an uploaded file); an exported word is new again |
| `DeleteSavedWord` | `dictionary:word:write` | Removes a word of the list |
| `ListCategories` | `dictionary:word:read` | The categories of the user by name with `total` / `new_total` (words of the list in them) and the same two counts for the words in no category |
| `CreateCategory` / `RenameCategory` | `dictionary:word:write` | Name 1 to 40 characters (trimmed), unique per user ignoring case (`CATEGORY_EXISTS`), at most 50 per user (`CATEGORY_LIMIT_REACHED`) |
| `DeleteCategory` | `dictionary:word:write` | Deletes the category only; its words stay in the list |
| `UpdateWordCategories` | `dictionary:word:write` | Adds the words to `add_category_ids` and takes them out of `remove_category_ids` in one step (words not in the list are skipped); it never touches the card or the exported flag |
| `RemoveExportedWords` | `dictionary:word:write` | Takes the exported words out of the list (the new ones stay); returns the count. Called by the dict app when the user answers "Remove them" |
| `ExportAnki` | `dictionary:word:write` | Builds the `.apkg` of the new words (`scope` `EXPORT_SCOPE_NEW`, the default) or of the whole list (`EXPORT_SCOPE_ALL`), returns its bytes and marks the words exported; with remove after export they also leave the list |
| `CreateDeckLink` | `dictionary:word:write` | Same as `ExportAnki`, but keeps the deck behind a random token (see "Download link") and returns the token |
| `UploadAudio` | `dictionary:word:write` | Stores an mp3, ogg, wav or m4a file of the user as it is (the bytes are checked, not re-encoded; at most `DICTIONARY_AUDIO_MAX_BYTES`) and returns an id; the card keeps it in `audio_uk` / `audio_us` as `dictaud:<id>`. Errors: `AUDIO_UNSUPPORTED`, `AUDIO_TOO_LARGE`, `AUDIO_QUOTA_EXCEEDED`, `AUDIO_DISABLED`; `SaveWord` answers `AUDIO_NOT_FOUND` for a file that is not the user's |
| `GetAudio` | `dictionary:word:read` | Returns an uploaded file of the user |
| `GetSettings` | `dictionary:settings:read` | Settings of the system (max saved words, export as deck, export as link, English, English-Vietnamese, remove after export, retention hours) |
| `UpdateSettings` | `dictionary:settings:write` | Replaces them; only the values that changed are written; at least one dictionary stays on |
| `GetUserQuota` | `dictionary:settings:read` | What a user stores (content, images, audio), the own limits an admin set for the user and the limits in effect |
| `SetUserQuota` | `dictionary:settings:write` | Sets the own limits of a user (0 or absent: the system setting again); a limit above the ceiling is `INVALID_ARGUMENT` |
| `GetMyStorage` | `dictionary:word:read` | What the user stores and the limit in effect of each kind (own limit included) |
| `GetMySettings` | `dictionary:word:read` | Preferences of the user (enabled dictionaries, default dictionary, default deck name, words per lookup, remove after export, whether the remove/keep question was answered) next to the system settings; the defaults when identity is down |
| `UpdateMySettings` | `dictionary:word:read` | Stores the preferences in identity (validated against the system settings) |
| `DownloadDeck` | any authenticated caller, **service token only** | Returns the deck of a token and deletes it; called by gatekeeper for the anonymous download |

The permissions are declared in `protos/dictionary/v1/permissions.proto` (one file) and **registered with identity at every start** (`RegisterPermissions`, the client needs the role `service_registrar`; `PERMISSIONS_SYNC_TIMEOUT`, default 2m, is how long the start retries; the service is not ready until identity has them and exits non-zero when it runs out). Give them to roles in the admin app (an admin holding `*` has them already). A permission removed from the file is retired by identity while a role still holds it, see its README.

Errors a client may want to recognize start with a code in the message: `MAX_SAVED_WORDS_REACHED` (`RESOURCE_EXHAUSTED`), `NO_WORDS_TO_EXPORT` (`INVALID_ARGUMENT`), `EXPORT_TOO_LARGE` (`FAILED_PRECONDITION`), `FEATURE_DISABLED` (`FAILED_PRECONDITION`, the export of a deck or of a link is turned off) and `DICTIONARY_DISABLED` (`FAILED_PRECONDITION`, the dictionary is turned off). An unknown word is `NOT_FOUND`; a provider that is down is `UNAVAILABLE` (nothing about the provider is sent). Field numbers and enum values use multiples of 10 (`UNSPECIFIED` = 0).

## Settings

Two kinds, both optional to set (a missing value is the default, so a new setting needs no migration):

- **System settings**, edited by an administrator (admin app, page Dictionary): rows `key` / `value jsonb` of `dictionary_settings` (migration `V18`). Keys: `max_saved_words` (1 to 1000000, default `DICTIONARY_MAX_SAVED_WORDS`), `allow_export_deck`, `allow_export_link`, `dictionaries` (one JSON object `{dictionary id: enabled}`; an id of the registry that is missing takes the default of its registry entry, on for `english` and `english-vietnamese` and off for a new dictionary until an administrator enables it; ids the registry does not have are ignored; at least one dictionary stays on), `remove_after_export` (default on, see "Saved words and export batches") `exported_retention_hours` (1 to 8760, default `DICTIONARY_EXPORTED_RETENTION`), `max_categories` (1 to 200, default 50) and `max_category_name_length` (1 to 100, default 40). They are enforced here: `SaveWord` uses the stored maximum (words above a lowered maximum stay, only a new save is refused), `CreateCategory` / `RenameCategory` use the two category limits (categories and names above a lowered limit stay, only a new one is refused), `ExportAnki` / `CreateDeckLink` answer `FEATURE_DISABLED` when off, a lookup in a turned off dictionary answers `DICTIONARY_DISABLED`.
- **Storage limits** (settings `max_content_bytes`, `content_user_quota`, `image_user_quota`, `audio_user_quota`, each chosen by the admin up to its `*_CEILING` env): `max_content_bytes` is the largest content of one word (64 KiB until chosen); the three quotas bound what one user stores of each kind, independently. Counters live in `dictionary_user_stats` (`content_bytes`, `image_bytes`, `audio_bytes`) and move in the transaction of the change; the worker's reconcile (50 users per cleanup tick) recomputes `listed`, `content_bytes`, `image_bytes` and `audio_bytes` from the rows and logs a mismatch. An administrator recalculates one user at once from the user's storage page of the admin app (`RecalculateUserStorage`, `POST /api/dictionary/users/{user_id}/storage/recalculate`, permission `dictionary:settings:write`), which answers with the counters before and after. `SaveWord` refuses a save that would pass the content quota with `RESOURCE_EXHAUSTED` `SAVED_BYTES_EXCEEDED` (a word new in the list adds its content, one already in it the difference, so a save that makes the content smaller always works and a user over a lowered quota can still edit and delete); the uploads refuse with `IMAGE_QUOTA_EXCEEDED` / `AUDIO_QUOTA_EXCEEDED`. An admin can give one user an own limit per kind (`SetUserQuota`, columns `content_quota`, `image_quota`, `audio_quota` of the stats row, NULL = the setting). Words that left the list (`removed_at`) do not count.
- **Preferences of a user** (dict app, page Settings): one JSON object for the application `dictionary` in `identity_user_settings`, which identity owns. Dictionary calls `GetMyUserSettings` / `PutMyUserSettings` of identity on behalf of the user: it exchanges the token of the request for one with the audience `DICTIONARY_IDENTITY_AUDIENCE` (RFC 8693, `gocommon/tokenexchange`) and calls `DICTIONARY_IDENTITY_ADDR`. The preferences narrow what the system allows: `dictionaries` (`{id: bool}`, a missing id is on) and `defaultDictionary` (an id; empty is the first one that is on) are validated against the registry and the system switches here (`ListDictionaries` and `GetMySettings` return the effective values), and the web app applies them (deck name, words per lookup), the server only validates them. The exceptions are `removeAfterExport` and `freshDeck`, which the server reads at export time (effective value = system AND user; identity down means the default, remove). `removeAcknowledged` is only kept for the dict app. If identity is down `GetMySettings` answers with the defaults.

## Dictionaries, groups and the registry

A **dictionary** is one `lookup.Provider` with an id (`english`, `english-vietnamese`), a **group** and a source and target language. The dictionaries of one group are looked up one by one by the client and merge into **one card**; a saved word belongs to a group and its key is `(user_id, grp, word)`, so the same word in two groups is two cards (the lists, the counts, the categories counts, the exports, the review and the removal of the exported words all work on one group, and every request that names a saved word or lists them carries a required `group`, `UNKNOWN_GROUP` when it is empty or not in the registry). The group `en` holds `english` and `english-vietnamese`.

The registry (`internal/lookup/registry.go`, built from `internal/catalog`) lists the entries in the order they are shown; `ListDictionaries` serves it, the web app renders its tabs, group switcher and the Settings and admin switches from it, and `lookup.Service` finds the provider of a request by id (the cache key already contains it). Nothing else knows the ids: the settings keep a `dictionaries` map, and `SaveWord` only checks that the group exists and that at least one of its dictionaries is on for the system and the user (a user who turned one off still saves the words of the others).

### Add a dictionary

1. Write a `lookup.Provider` (`Lookup(ctx, word) (*Word, error)`: `ErrNotFound` without an entry, an error wrapping `ErrUnavailable` when it could not tell). A source with an HTTP API follows `FreeDict` (`freedict.go`, with `http.go` for the retries); one that composes others follows `Vietnamese` (`vietnamese.go`). `NormalizeWord` lets the letters of any script through; a script without case or spaces (CJK) may need its own rule for the length and the lower-casing, which today are the same for every group.
2. Add its entry in `internal/catalog/catalog.go` (`Entries`: `ID`, `Group` (a new group or an existing one), `Source`, `Target`, `DefaultOn`), and its provider in the map of `cmd/dictionary/main.go`. Leave `DefaultOn` false: the dictionary then stays off until an administrator enables it in the admin app, so a deployment does not show a dictionary it did not choose.
3. The URL of its source goes in `internal/appconfig/appconfig.go` (like `DICTIONARY_FREEDICT_URL`, overridable in tests), and the hosts of its audio files in `DICTIONARY_AUDIO_HOSTS` (the export downloads only from those).
4. In the web app, add the labels `lookup.dictionaries.<id>` and `lookup.groups.<group>` to the i18n files of dict and admin (an id without a label shows the id) and, for a card that should look different from the generic one, a renderer for the id in `apps/dict/src/anki.ts`.

Nothing to migrate: the group is a value of the column `grp`, and the settings are a JSON map. Existing cards of another group are never moved to the new one, and the `content` of a card is not translated after the fact.

## Lookup sources

Cambridge answers 403 behind Cloudflare, so nothing is scraped from it. The entries come from open APIs behind one `lookup.Provider` interface (`internal/lookup`):

| Dictionary | Source | Gives |
| --- | --- | --- |
| `english` | [Free Dictionary API](https://dictionaryapi.dev) (`api.dictionaryapi.dev`) | definitions, examples, parts of speech, phonetics, UK/US audio |
| `english-vietnamese` | [Vietnamese Wiktionary](https://vi.wiktionary.org) (MediaWiki API, plain text of the section "Tiếng Anh") | Vietnamese meanings, parts of speech, IPA |
| | same, for the phonetics and the audio | taken from the English entry |
| | [MyMemory](https://mymemory.translated.net), only when the Wiktionary has no entry | machine translation of the first 3 English definitions, marked `machine_translated` |

- `DICTIONARY_VI_FALLBACK_MT=false` turns the machine translation off: a word the Wiktionary lacks then comes back with its English meanings.
- The anonymous quota of MyMemory is about 5000 characters a day, which is why only 3 definitions are translated; when it is used up the English meanings are returned.
- Calls have a timeout (`DICTIONARY_HTTP_TIMEOUT`), a `User-Agent` with a contact (`DICTIONARY_USER_AGENT`, Wikimedia asks for it), and 3 attempts with backoff on a network error, 429 or 5xx. Entries are cached in memory (`DICTIONARY_LOOKUP_CACHE_TTL`, `DICTIONARY_LOOKUP_CACHE_SIZE`) so the free services are not hammered, and `LookupWords` runs at most 3 lookups at once.
- A word is trimmed, lower-cased and checked (letters, digits, space, apostrophe, hyphen, dot, at most 64 characters) before it reaches a URL or the database.
- Risks: the Free Dictionary API is a single-maintainer service without an SLA, and the Wiktionary has uneven coverage of rare words. A provider is one small interface, so another source can replace either.
- Attribution: the Wiktionary text and the Wikimedia audio are available under CC BY-SA 4.0; show the source where the meanings are displayed.

## Saved words and export batches

`dictionary_saved_words` (migration `V15__dictionary_saved_words.sql`) holds one row per user, group and word (the key is `(user_id, grp, word)` since `V40__dictionary_groups.sql`, which gave the rows that existed the group `en`; the audio and image references, the review state and log and the category links carry the same key). The per-user quotas (words in the list, content, images, audio) count across the groups.

- Two timestamps: `exported_at` (the word was packed in a deck; `NULL` means new) and `removed_at` (the word left the list; `NULL` means it is in the list; migration `V20`). The words with `removed_at` `NULL` are the list: they count against `DICTIONARY_MAX_SAVED_WORDS` (default 100) and `ListSavedWords` returns them. Saving a word again overwrites it (card, audio and save time, so it moves to the top of the list) and makes it new again (both timestamps cleared); the limit is checked under a per-user advisory lock, so parallel saves cannot pass it.
- `ExportAnki` / `CreateDeckLink` read the new words (`scope` NEW, the default) or the whole list (`scope` ALL), oldest first, one card each: the word on the front, the saved HTML on the back. They build the deck and then set `exported_at = now()` on exactly those rows, and `removed_at = now()` too when remove after export is in effect. A deck that fails or is too large changes nothing. Words saved while the deck was built are not touched. With nothing new and `scope` NEW the answer is `NO_WORDS_TO_EXPORT`.
- **Categories** (migration `V25`: `dictionary_categories`, `dictionary_word_categories`): a word may be in several categories, like tags. The links belong to `(user_id, grp, word)` (the categories belong to the user, their counts to a group): `SaveWord` only **adds** the `category_ids` it gets (saving a word again keeps its categories), deleting the word deletes its links, deleting a category deletes only the links. `ListSavedWords`, `ExportAnki`, `CreateDeckLink` and `GetReviewQueue` take `category_id` or `uncategorized` (both empty: every word). The export requests also take `words`: exactly those words are packed (`category_id`, `uncategorized` and `scope` are ignored, exported words are packed again, unknown words are skipped, at most 5000 words per request). The default deck name is the category name (or "My Words"). The daily limit of new review words stays one for all categories.
- **Remove after export** is the system setting `remove_after_export` AND the preference `removeAfterExport` of the user (both default on). Off: exported words stay in the list, flagged `exported`, and the user chooses "export all" or "export new only" the next time. When it comes on again (an administrator or the user), the dict app asks once whether to remove the exported words that are still in the list (`RemoveExportedWords`) or keep them.
- Rows that left the list are kept for the system setting `exported_retention_hours` (default `DICTIONARY_EXPORTED_RETENTION`, `168h`, 7 days; 1 to 8760 hours) and then deleted by the cleanup of `dictionary-worker` every `DICTIONARY_CLEANUP_INTERVAL` (default `1h`), in batches of 500 rows with `FOR UPDATE SKIP LOCKED`, so several replicas can run it. It reads the setting on every run and never deletes a word that is in the list. There is no API to export an old batch again; the retention only covers a response that got lost.
- **Importing again updates, it does not duplicate.** The Anki library is the fork `huponx/go-anki-deck`: the guid of a card is the hash of its word (trimmed, lower case) and the ids of the deck and of the note type come from the deck name. Anki therefore treats a word exported twice as the same note and updates its front and back (the review history stays). Keep the same deck name between exports (the user's `deckName` preference). A user who wants every export to be a separate deck turns on the preference `freshDeck` (dict app, Settings, "New deck on every export"; `UserSettings.fresh_deck`): then the ids come from the clock and the guid also depends on the back, as in the upstream library (`anki.Options{Fresh: true}`). It is read at export time, like `removeAfterExport`, and is off by default. Decks exported before this change carry other guids: the first import after it adds one more copy of each word, and the old copies can be deleted once.
- The card back may contain the placeholders `replace_audio_uk` and `replace_audio_us`: in the deck they become sound tags of the downloaded files, or of the uploaded files (`dictaud:`, read from the volume, with their real extension); nothing when a file is missing or could not be downloaded.

Audio files are downloaded by the service when it builds a deck, so the URLs a client saves are restricted: `https` and a host of `DICTIONARY_AUDIO_HOSTS` (default `api.dictionaryapi.dev`, `upload.wikimedia.org`, `ssl.gstatic.com`, `dictionary.cambridge.org`), checked at save time, again at download time and on every redirect. Files are at most 512 KiB, the downloads of one deck share a 20 second budget, and a file that fails is left out without failing the export.

### Download link (Anki app "import from a link")

AnkiMobile can import a deck from a URL, and it fetches it without cookies. `CreateDeckLink` therefore stores the deck in `dictionary_deck_links` (migration `V17`) behind a random 256-bit token: only the SHA-256 of the token is kept, a user has one link at a time (a new one replaces the old), the link works **once** (the download deletes the row) and for `DICTIONARY_DECK_LINK_TTL` (default `15m`); the cleanup loop deletes the expired rows. Gatekeeper serves it at `GET /api/dictionary/downloads/{token}.apkg`, the only anonymous route: it calls `DownloadDeck` with its own service token, and the method refuses user tokens. The token in the URL is the whole authorization, so treat the link like a password until it is used.

The deck travels in the gRPC response (and as base64 in the JSON of gatekeeper), so it is capped by the system setting "max size of an export" (`export_max_bytes`, default `DICTIONARY_EXPORT_MAX_BYTES` = 3 MiB; the admin chooses 1, 3, 6 or 10 MiB, never above `DICTIONARY_EXPORT_MAX_BYTES_CEILING`, 10 MiB by default, which is also the gRPC send limit of this service). A ceiling above the default needs `GATEKEEPER_UPSTREAM_MAX_RECV_BYTES` to match in gatekeeper.

### Size of the parts of an export job

A part of an export job is not a gRPC message: `DownloadExportPart` is a server-streaming method that sends the file in chunks of 256 KiB (the first one has `filename` and `size`) and gatekeeper writes them to the browser as they arrive (`GET /api/dictionary/export-downloads/{token}.apkg`). Its size is the system setting `export_part_max_bytes` (default `DICTIONARY_EXPORT_PART_MAX_BYTES`, 3 MiB; the admin chooses 3, 6, 10, 25 or 50 MiB, never above `DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING`, 50 MiB by default): fewer, larger files to import in Anki for a long list. The job reads the setting before every batch, so a change applies to the next part of a running job. A part also has a download link (`POST /api/dictionary/exports/{id}/parts/{n}/link`, the web page shows it under "Copy link") that, unlike the deck link above, works any number of times until the job expires (`DICTIONARY_EXPORT_JOB_TTL`), so it can be pasted into AnkiMobile; asking for it again replaces the token, so the page makes it once per part. A worker that dies after it wrote a part file and before it recorded the part leaves an orphan `.apkg`; the same sweep deletes the files of `exports/` that no row of `dictionary_export_parts` records.

The words of a part are read in a batch that starts at 2000. After every full part the next batch is sized from the bytes per word of that part to fill 80% of the limit (at most twice the previous batch, up to `DICTIONARY_EXPORT_JOB_BATCH_MAX`, 20000), so the parts come close to the limit and a batch is rarely built twice; a deck over the limit (heavier words than the earlier ones) halves the batch and builds it again. The progress on the page never goes down. **Memory:** the worker holds the cards, the files of their images and audio and the built deck, about 2 to 3 times the size of the part, for up to 2 jobs per instance, so with 50 MiB parts plan on 300 MiB at most per instance (the chart of the worker has a 1Gi limit and a 512Mi `/tmp`); lower the ceiling if the pods are small. The API pod does not carry this load. `deck.Save()` still builds the deck in memory.

## Performance

Lists of up to 1,000,000 words per user: the counters, caps and indexes behind it, the measurements and how to repeat them by hand are in [docs/performance.md](docs/performance.md) (scripts in `docs/performance/`); the review queue is in [docs/review-performance.md](docs/review-performance.md).

## Local development

`go.mod` pins released versions of `gocommon`, `protogen` and `go-anki-deck`. To work against the sibling checkouts instead, use a Go workspace (`go.work` is git-ignored and not sent to Docker builds):

```bash
go work init . ../gocommon ../protogen/go ../go-anki-deck   # once, in dictionary/; delete go.work to go back to the pinned versions
```

```bash
# from the monorepo root: regenerate the Go stubs after proto changes (the stubs of ../protogen/go are used through go.work)
PROTO_REF=v0.1.6 PROTO_DIR=protos PROTOGEN_DIR=protogen bash protos/scripts/generate.sh go

cd dictionary
cp .env.example .env     # optional; config is env-based. PORT=9092 avoids identity (9090) and courier (9091)
# Postgres with the schema (V15 and later): `make up` in ../db-migration
set -a; . ./.env; set +a
go run ./cmd/dictionary
```

Identity must be running for the keys (`DICTIONARY_JWKS_URL`). To call the API without gatekeeper, take a token from identity whose audience is `dictionary` (a token exchange for the user) and send it as `authorization: Bearer <token>`; `ENV=local` turns reflection on, so `grpcurl -plaintext localhost:9092 list` works.

```bash
make test          # unit tests; the store tests are skipped without a database
make test-store    # store tests against $TEST_DB (default: the postgres of db-migration, migrated to V15)
make test-live     # one call to each real API, to check the parsers after a provider changes its format
```

## Configuration

All settings are environment variables (see `.env.example`). `dictionary-worker` reads the same variables except the ones for the gRPC server and identity (`HOST`, `PORT`, `DICTIONARY_ISSUER`, `DICTIONARY_JWKS_URL`, `DICTIONARY_AUTH_AUDIENCE`, `DICTIONARY_IDENTITY_*`, `DICTIONARY_TOKEN_URL`, `DICTIONARY_CLIENT_*`, the lookup providers and cache, `DICTIONARY_EXPORT_SYNC_MAX`, `DICTIONARY_DECK_LINK_TTL`, the user quotas) and adds `HEALTH_ADDR`. Shared by both: `POSTGRES_*`, `LOG_*`, `SERVICE_NAME`, `ENV`, `SHUTDOWN_TIMEOUT`, the export size limits, `DICTIONARY_EXPORT_JOB_*`, `DICTIONARY_MAX_SAVED_WORDS`, `DICTIONARY_EXPORTED_RETENTION`, `DICTIONARY_CLEANUP_INTERVAL`, `DICTIONARY_FILES_DIR`, the image and audio size limits, the file ages and `DICTIONARY_AUDIO_HOSTS`, `DICTIONARY_HTTP_TIMEOUT`.

| Variable | Default | Notes |
| --- | --- | --- |
| `HOST` / `PORT` | `0.0.0.0` / `9090` | gRPC listen |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful drain |
| `HEALTH_ADDR` | `:8081` | Worker only: `/healthz` and `/readyz` |
| `LOG_LEVEL` / `LOG_ENCODING` | `info` / `console` | |
| `SERVICE_NAME` / `ENV` | `app` / `dev` | `ENV=local` turns gRPC reflection on and allows an `http` issuer; any other value needs an `https` issuer |
| `POSTGRES_HOST` / `PORT` / `USER` / `PASSWORD` / `DB` / `SSLMODE` | `localhost` / `5432` / `postgres` / empty / `app` / `disable` | The password comes from a Secret |
| `DICTIONARY_ISSUER` | required | `iss` of the identity tokens (its public URL) |
| `DICTIONARY_JWKS_URL` | required | Keys of identity, in the cluster `http://identity.application.svc.cluster.local:8080/.well-known/jwks.json` |
| `DICTIONARY_AUTH_AUDIENCE` | `dictionary` | `aud` the tokens must have; the exchange policy `gatekeeper -> dictionary` issues them |
| `DICTIONARY_IDENTITY_ADDR` | empty | gRPC address of identity for the preferences of the users (in the cluster `identity.application.svc.cluster.local:9090`); empty: defaults only, no changes |
| `DICTIONARY_IDENTITY_INSECURE` | `true` | Plain gRPC to identity (in-cluster traffic) |
| `DICTIONARY_IDENTITY_AUDIENCE` | `identity` | Audience of the exchanged token |
| `DICTIONARY_TOKEN_URL` | required with the address | Token endpoint of identity (`.../oauth2/token`) |
| `DICTIONARY_CLIENT_ID` / `DICTIONARY_CLIENT_SECRET` | required with the address | OAuth client of this service (grants `client_credentials` and token exchange); the secret comes from a Secret |
| `DICTIONARY_MAX_SAVED_WORDS` | `100` | Default of the setting "max saved words" (1 to 1000000); the value an administrator saved wins |
| `DICTIONARY_EXPORT_MAX_BYTES` | `3145728` | Default of the setting "max size of an export" (the `.apkg` of ExportAnki and CreateDeckLink) |
| `DICTIONARY_EXPORT_MAX_BYTES_CEILING` | `10485760` | The most an admin can set for it; also the gRPC send limit of this service. Gatekeeper must accept that much (`GATEKEEPER_UPSTREAM_MAX_RECV_BYTES`) |
| `DICTIONARY_EXPORT_PART_MAX_BYTES` | `3145728` | Default of the setting "max size of an export part" (a part of an export job, streamed) |
| `DICTIONARY_EXPORT_PART_MAX_BYTES_CEILING` | `52428800` | The most an admin can set for it; bounded by the memory of the worker (2 to 3 times the size per running job); set it in the API and in the worker |
| `DICTIONARY_EXPORT_SYNC_MAX` | `2000` | API only. Default of the setting "max words of an export at once" (`export_sync_max_words`, 1 to 5000, an administrator changes it; the stored value wins): a synchronous export (`ExportAnki`, `CreateDeckLink`) of a longer list is refused with `EXPORT_USE_JOB` |
| `DICTIONARY_EXPORT_JOB_BATCH_MAX` | `20000` | Most words an export job packs at once (it starts at 2000 and is then sized from the bytes per word of the last part, to fill 80% of the limit; 100 to 100000) |
| `DICTIONARY_DECK_LINK_TTL` | `15m` | How long a download link works (1m to 24h) |
| `DICTIONARY_EXPORTED_RETENTION` | `168h` | Default of the system setting "keep removed words for": how long rows that left the list are kept (`1h` to `8760h`) |
| `DICTIONARY_CLEANUP_INTERVAL` | `1h` | Period of the cleanup (at least `1m`) |
| `DICTIONARY_AUDIO_HOSTS` | `api.dictionaryapi.dev,upload.wikimedia.org,ssl.gstatic.com,dictionary.cambridge.org` | Allowed hosts of audio URLs; comma or one per line |
| `DICTIONARY_FILES_DIR` | empty | Directory of the card files: images in `<dir>/images`, audio in `<dir>/audio` (each `<user>/<id>.<ext>`); empty turns the image and audio RPCs off. In Kubernetes it is the PVC `dictionary-files` mounted at `/data/files` |
| `DICTIONARY_IMAGE_MAX_BYTES` | `307200` | Largest stored image (after the resize and re-encoding) |
| `DICTIONARY_IMAGE_MAX_DIM` | `1024` | Longest side in pixels; larger images are scaled down |
| `DICTIONARY_IMAGE_MAX_PIXELS` | `25000000` | Uploads with more pixels are refused before they are decoded |
| `DICTIONARY_IMAGE_USER_QUOTA` | `20971520` | **Default** of the system setting `image_user_quota` (bytes of images one user may keep); the admin changes it in the app, up to `DICTIONARY_IMAGE_USER_QUOTA_CEILING`, and the value stored there wins |
| `DICTIONARY_AUDIO_MAX_BYTES` | `409600` | Largest uploaded audio file (mp3, ogg, wav or m4a, stored as it is); 1 KiB to 700 KiB because of the 1 MiB body limit of the gateway |
| `DICTIONARY_AUDIO_USER_QUOTA` | `10485760` | **Default** of the system setting `audio_user_quota` (bytes of audio files one user may keep; at least `DICTIONARY_AUDIO_MAX_BYTES`), same rules as the image quota |
| `DICTIONARY_CONTENT_USER_QUOTA` | `268435456` | Default of the system setting `content_user_quota`: bytes of card content one user may keep in the list (checked by `SaveWord`) |
| `DICTIONARY_CONTENT_MAX_BYTES_CEILING` | `262144` | The most an admin can set for `max_content_bytes` (the content of one word, 64 KiB until chosen); 64 KiB to 512 KiB, under the 1 MiB body limit of the gateway and message limit of gRPC. API only |
| `DICTIONARY_CONTENT_USER_QUOTA_CEILING`, `DICTIONARY_IMAGE_USER_QUOTA_CEILING`, `DICTIONARY_AUDIO_USER_QUOTA_CEILING` | `1073741824` | The most an admin can set (in the settings or for one user) for the quota of content, images and audio; each at least its default above. API only (`dictionary-worker` does not read them) |
| `DICTIONARY_UNUSED_FILE_AGE` | `24h` | An image or audio file no card refers to is deleted after this long (min `1h`: an upload comes before the save of its card). |
| `DICTIONARY_ORPHAN_SWEEP_EVERY` | `24h` | How often the image, audio and export volumes are compared with their tables to delete files without a row (min `1m`). |
| `DICTIONARY_ORPHAN_FILE_AGE` | `1h` | Files younger than this are never swept, so an upload in progress (or a part of an export job being written) is safe (min `10m`). |
| `DICTIONARY_PURGE_GRACE` | `720h` | Deleting an account (identity `DeleteUser`) is soft: `dictionary-worker` deletes the data of the user in batches (words, review log, images, audio, exports, categories, counters; the files by directory) at once, and the row of `identity_users` this long after that. There is no undo; the wait only lets the purge finish safely. `0` deletes the row as soon as the data is gone. |
| `DICTIONARY_LOOKUP_CACHE_TTL` / `SIZE` | `24h` / `2000` | In-memory cache of entries; `0` turns it off |
| `DICTIONARY_HTTP_TIMEOUT` | `10s` | One call to a provider or an audio file |
| `DICTIONARY_USER_AGENT` | `huponx-dictionary/1.0 (+https://github.com/huponx/dictionary)` | Sent to the providers |
| `DICTIONARY_VI_FALLBACK_MT` | `true` | Machine translation when the Wiktionary has no entry |
| `DICTIONARY_FREEDICT_URL` / `WIKTIONARY_URL` / `MYMEMORY_URL` | the public endpoints | Overridden in tests |

## Kubernetes

- The worker is its own Deployment (chart `dictionary-worker`, Application `dictionary-worker` in mm-deploy): HTTP probes on 8081, no Service, 1Gi memory, `/tmp` of 512Mi, the same PVC as the API.
- gRPC health on the API port (`grpcserver` health is on by default, no token required for `/grpc.health.v1.Health`); the chart probes the TCP port like courier.
- The pod is read-only apart from `/tmp` (an `emptyDir` in the chart): the Anki library writes the deck through a temporary SQLite file.
- The card images and audio files are files in `DICTIONARY_FILES_DIR` (`images/` and `audio/`): the chart mounts the PVC `dictionary-files` at `/data/files` and sets `fsGroup: 65532` so uid 65532 can write to it. The claim is not part of the chart: mm-ansible creates it (`playbooks/cluster.yml --tags pvc`), and it must exist before the pod starts, or the pod stays `Pending`. It lives outside ArgoCD, so deleting the Application keeps the files. ReadWriteOnce hostpath only works on a one-node cluster (also with the HPA): with more nodes switch to a ReadWriteMany class or object storage. `ci/full-values.yaml` uses an `emptyDir` instead of the claim.
- The only caller is gatekeeper: there is no Ingress, only the Service `dictionary:9090` of the namespace `application`.
- It reads two things outside the cluster network: the dictionary APIs and the audio hosts (HTTPS egress).

## Images and charts

One image is built from the repository root (distroless, non-root):

```bash
make docker IMAGE_TAG=dev     # ghcr.io/huponx/dictionary:dev
```

`build/dictionary/Dockerfile`. The Anki library writes the deck with SQLite, so the binary is built with **cgo** on the target platform and runs on `distroless/base` (glibc), not `static`; a multi-arch image is built natively or under QEMU, not cross compiled.

The Helm chart `charts/dictionary` wraps the `go-application` chart (dependency alias `application`, so all values sit under `application:`). `version`, `appVersion` and `image.tag` are written at release time by the CI; the committed values only keep `helm template` working.

```bash
cd charts/dictionary && helm dependency build && helm lint . -f ci/full-values.yaml
helm template dictionary . -f ci/full-values.yaml
```

Secret values never go in git: set `application.secrets` with `secretName`/`key` in the deployment repository (`mm-deploy`). The Application files `mm-deploy/applications/<env>/dictionary.yaml` set the issuer, the JWKS URL and the database.

## CI/CD

`.github/config/ci.yaml` is the contract and `.github/workflows/{pr,main,staging,production}.yml` call the reusable workflows of `huponx/actions` (`service-*.yml`):

| Flow | Trigger | Result |
|---|---|---|
| pr | pull request to `main` | lint, test, build, Trivy scan, `helm lint`; nothing pushed |
| main | push to `main` | image and chart pushed (`1.YYYYMMDD.<run>-main-<sha7>`), deployment PR for `dev` in `mm-deploy` |
| staging | push to `release*` | pre-release `v<YYYY>.<MM>.N-release-<sha7>`, image and chart pushed, deployment PR for `staging` |
| production | release published | no build; verifies the image and chart of the release tag exist, deployment PR for `production` |

`N` only increases after the previous pre-release of the month was published; until then a new push changes only the short sha. Required secret: `BOT_TOKEN` (write access to `mm-deploy`).

## Release order

A change that touches several repositories is released in this order; every step needs the tag of the previous one:

1. `protos` (the contract), then `protogen` (stubs; the tag the services pin).
2. `gocommon`, only if it changed.
3. `db-migration` (`V15`, `V16`, `V18`): the table and the permissions exist before the service starts.
4. `dictionary`: bump `github.com/huponx/protogen/go` (and `gocommon`) in `go.mod` to the released tags, run `make tidy`, merge.
5. `gatekeeper`: the routes, `descriptors.binpb` and the upstream of the chart (a gatekeeper release).
6. `identity`: the exchange policy `gatekeeper -> dictionary` (see step 8 of the setup).
7. `mm-deploy`: `applications/<env>/dictionary.yaml`, then the bump PRs of gatekeeper and identity.

## Setup (one time)

Commands use the [GitHub CLI](https://cli.github.com) (`gh auth login` first, with admin rights on the repositories). Package visibility and access are set in the GitHub UI (package settings).

1. **Create the repository** `huponx/dictionary` and push this folder (`git init`, `gh repo create huponx/dictionary --private --source . --push`). Until `protogen` has a tag with `dictionary/v1`, `go.mod` cannot resolve the stubs: use the Go workspace of [Local development](#local-development), and pin the tag as soon as it exists (`go get github.com/huponx/protogen/go@<tag> && make tidy`).

2. **Publish `huponx/actions`**: merge it and make sure a release exists and the floating `v1` tag points to it (the workflows use `huponx/actions/...@v1`). Other repositories must be allowed to use its workflows:

   ```bash
   gh release list -R huponx/actions --limit 3                     # a release must exist
   gh api repos/huponx/actions/git/ref/tags/v1 --jq .object.sha    # v1 must exist
   # private repo only: share the actions with the other repositories of the owner
   gh api -X PUT repos/huponx/actions/actions/permissions/access -f access_level=user
   ```

3. **Token for `mm-deploy`**: `GITHUB_TOKEN` cannot open PRs in another repository. Pick one:

   ```bash
   # a) PAT (fine-grained, repository huponx/mm-deploy: Contents and Pull requests write)
   gh secret set BOT_TOKEN -R huponx/dictionary                    # paste the token

   # b) GitHub App installed on mm-deploy (Contents and Pull requests write)
   gh variable set DEPLOY_APP_ID -R huponx/dictionary --body "<app id>"
   gh secret set DEPLOY_APP_PRIVATE_KEY -R huponx/dictionary < app-private-key.pem
   ```

   The App takes precedence when `DEPLOY_APP_ID` is set; without it `BOT_TOKEN` is used.

4. **Optional**: Slack notification of deployment PRs (set `slack-channel` in `ci.yaml`):

   ```bash
   gh secret set SLACK_WEBHOOK_URL -R huponx/dictionary
   ```

5. **Workflow permissions** (packages and the staging pre-release need write):

   ```bash
   gh api -X PUT repos/huponx/dictionary/actions/permissions/workflow \
     -f default_workflow_permissions=write -F can_approve_pull_request_reviews=false
   ```

6. **GHCR packages**: after the first `main` run the packages `dictionary` and `charts/dictionary` exist. Link each to the repository (package settings, "Manage Actions access", role Write) and give the ArgoCD clusters pull access (public, or an image pull secret and an OCI repository credential in each ArgoCD). `go-application` 1.3.0 must be readable from `oci://ghcr.io/huponx/charts`.

   ```bash
   gh api "/user/packages?package_type=container" --jq '.[].name'   # list the packages (use /orgs/<org>/packages for an org)
   ```

7. **Branch ruleset** for `main` (PR required, squash only, required checks of the `pr` flow, no force push or deletion). Merge the workflows to `main` first, while `main` is unprotected: status checks only show up in the ruleset picker after they have run once. Save as `main-ruleset.json` and apply (the file is only an input, do not commit it):

   ```json
   {
     "name": "protect main",
     "target": "branch",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["~DEFAULT_BRANCH"], "exclude": [] } },
     "rules": [
       { "type": "deletion" },
       { "type": "non_fast_forward" },
       { "type": "pull_request", "parameters": {
           "required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false,
           "require_code_owner_review": false, "require_last_push_approval": false,
           "required_review_thread_resolution": false,
           "allowed_merge_methods": ["squash"] } },
       { "type": "required_status_checks", "parameters": {
           "strict_required_status_checks_policy": true,
           "required_status_checks": [
             { "context": "pr / contract" },
             { "context": "pr / lint-test" },
             { "context": "pr / build / build (dictionary)" } ] } }
     ],
     "bypass_actors": []
   }
   ```

   ```bash
   REPO=huponx/dictionary
   gh api repos/$REPO/rulesets --method POST --input main-ruleset.json
   gh api repos/$REPO/rulesets --jq '.[] | {id, name, enforcement}'   # verify
   ```

   Or in the UI: Settings > Rules > Rulesets > New ruleset > Import a ruleset. The check names are `<caller job> / <reusable job>`; confirm them with `gh pr checks <pr>` after the first PR run. To change it later: `gh api repos/$REPO/rulesets/<id> --method PUT --input main-ruleset.json`. Rulesets on a private repo need a paid GitHub plan.

   Second ruleset for `release*`: a merge into it creates a staging pre-release, so it needs a PR with the same checks as `main` (the `pr` flow also runs on PRs into `release*`), and no force push or deletion. Save as `release-ruleset.json` and apply the same way (`gh api repos/$REPO/rulesets --method POST --input release-ruleset.json`):

   ```json
   {
     "name": "protect release branches",
     "target": "branch",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["refs/heads/release*"], "exclude": [] } },
     "rules": [
       { "type": "deletion" },
       { "type": "non_fast_forward" },
       { "type": "pull_request", "parameters": {
           "required_approving_review_count": 0, "dismiss_stale_reviews_on_push": false,
           "require_code_owner_review": false, "require_last_push_approval": false,
           "required_review_thread_resolution": false,
           "allowed_merge_methods": ["merge", "squash"] } },
       { "type": "required_status_checks", "parameters": {
           "strict_required_status_checks_policy": false,
           "required_status_checks": [
             { "context": "pr / contract" },
             { "context": "pr / lint-test" },
             { "context": "pr / build / build (dictionary)" } ] } }
     ],
     "bypass_actors": []
   }
   ```

   The ruleset applies to every `release*` branch, so create the first one (for example `release`) from `main` before applying it, or push it while the ruleset is not yet active.

8. **Identity: the exchange policy and the roles.** gatekeeper may only ask for a token for an audience that has a policy:
   - Run the migrations of `db-migration` (`V15`, `V16`) so the table and the permissions `dictionary:word:read` and `dictionary:word:write` exist.
   - Add the policy `gatekeeper -> dictionary` in the admin app (Exchange policies: client `gatekeeper`, audience `dictionary`, max depth 1), or put `{"client":"gatekeeper","audience":"dictionary","max_depth":1}` in the `exchange_policies` of `SETUP_CONFIG` of the identity Application (it is already there in `mm-deploy/applications/<env>/identity.yaml`; the setup Job only adds policies, so switch `setup.enabled` on for one sync, then off again).
   - Run `V18` too: the permissions `dictionary:settings:read` and `dictionary:settings:write` and the tables `dictionary_settings` and `identity_user_settings`.
   - Create the OAuth client `dictionary` (confidential, resource `dictionary`, grants `client_credentials` and `urn:ietf:params:oauth:grant-type:token-exchange`) and the policy `dictionary -> identity` with max depth 2 (the token of gatekeeper already has one actor, the exchange adds this service): both are in the `SETUP_CONFIG` of `mm-deploy` `applications/<env>/identity.yaml`. Its secret is the Secret `dictionary-client` (key `secret`, same as `gatekeeper-client`), mounted as `DICTIONARY_CLIENT_SECRET`.
   - Give the roles of the users the permissions (Roles, `dictionary:word:read`, `dictionary:word:write`; `dictionary:settings:read` and `dictionary:settings:write` for those who may edit the settings of the system).

9. **`mm-deploy`**:

   ```bash
   gh repo edit huponx/mm-deploy --enable-auto-merge --delete-branch-on-merge
   gh label create automerge -R huponx/mm-deploy --color 0E8A16 --description "Merge automatically when checks pass" || true
   ```

   - required checks on `main`: `ci`, `diff`, `cve`, `pr-title`;
   - each ArgoCD (one per environment) has a root app with `path: applications/<env>`;
   - `applications/<env>/dictionary.yaml` exists with a SAMPLE `targetRevision`: set it to the version published by the first run of the `main` flow, and check `DICTIONARY_ISSUER` and the database settings; later versions come from the deployment PRs;
   - the database user is the one of identity (`postgres-data-rw-credentials`); no new Secret is needed. `data-user-rw` needs `INSERT`, `UPDATE`, `DELETE` and `SELECT` on `dictionary_saved_words`.

10. **gatekeeper**: the routes `/api/dictionary/...` and the upstream `dictionary` are part of the gatekeeper release (see `gatekeeper/routes.yaml`). Release gatekeeper after `dictionary` is deployed, then bump `mm-deploy/applications/<env>/gatekeeper.yaml`.

11. **Environments**: staging is `enabled: false` in `.github/config/ci.yaml`; remove that line once its ArgoCD and `applications/staging` exist. For production add `reviewers` and keep `automerge` off.

12. **Verify**:

    ```bash
    gh secret list -R huponx/dictionary && gh variable list -R huponx/dictionary
    # merge a PR into main, then follow the run
    gh run list -R huponx/dictionary --workflow main.yml --limit 1
    gh run watch -R huponx/dictionary
    gh pr list -R huponx/mm-deploy --label automerge                 # the dev deployment PR
    # after the release of gatekeeper, as a signed-in user with the permission:
    curl -s -b cookies.txt "https://api.example.com/api/dictionary/words/hello?dictionary=english"
    ```
