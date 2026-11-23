# dictionary

Dictionary service: a gRPC API to look English words up (English definitions, or Vietnamese meanings), to keep a per-user list of saved words and to export that list as an Anki deck (`.apkg`).

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

One process, `cmd/dictionary`, gRPC only. It never sees a password or a session: gatekeeper exchanges the session of the user for a short JWT whose audience is `dictionary`, and dictionary verifies it with the keys of identity (`DICTIONARY_JWKS_URL`). The user of every call is the `sub` of that token, never a field of a request.

## API

`DictionaryService` (see `dictionary/v1` in the protos repo). Every method needs a permission; the server is deny by default (`grpcserver.WithMethodPermissions`, `internal/service` `Permissions`, covered by a test).

| RPC | Permission | What it does |
| --- | --- | --- |
| `LookupWord` | `dictionary:word:read` | Entry of one word in `DICTIONARY_ENGLISH` (default) or `DICTIONARY_ENGLISH_VIETNAMESE` |
| `LookupWords` | `dictionary:word:read` | 1 to 20 words; words without an entry come back in `not_found` |
| `ListSavedWords` | `dictionary:word:read` | Pending saved words, newest first, token pagination (`page_size`, `page_token`) |
| `SaveWord` | `dictionary:word:write` | Adds or refreshes a pending word (word, card back HTML, audio URLs) |
| `DeleteSavedWord` | `dictionary:word:write` | Removes a pending word |
| `ExportAnki` | `dictionary:word:write` | Builds the `.apkg` of the pending words, returns its bytes and starts a new batch |
| `CreateDeckLink` | `dictionary:word:write` | Same as `ExportAnki`, but keeps the deck behind a random token (see "Download link") and returns the token |
| `GetSettings` | `dictionary:settings:read` | Settings of the system (max saved words, export as deck, export as link, English, English-Vietnamese) |
| `UpdateSettings` | `dictionary:settings:write` | Replaces them; only the values that changed are written; at least one dictionary stays on |
| `GetMySettings` | `dictionary:word:read` | Preferences of the user (enabled dictionaries, default dictionary, default deck name, words per lookup) next to the system settings; the defaults when identity is down |
| `UpdateMySettings` | `dictionary:word:read` | Stores the preferences in identity (validated against the system settings) |
| `DownloadDeck` | any authenticated caller, **service token only** | Returns the deck of a token and deletes it; called by gatekeeper for the anonymous download |

The permissions are in the catalog of identity (migration `V16__dictionary_permissions.sql` of `db-migration`); give them to roles in the admin app (an admin holding `*` has them already).

Errors a client may want to recognize start with a code in the message: `MAX_SAVED_WORDS_REACHED` (`RESOURCE_EXHAUSTED`), `NO_WORDS_TO_EXPORT` (`INVALID_ARGUMENT`), `EXPORT_TOO_LARGE` (`FAILED_PRECONDITION`), `FEATURE_DISABLED` (`FAILED_PRECONDITION`, the export of a deck or of a link is turned off) and `DICTIONARY_DISABLED` (`FAILED_PRECONDITION`, the dictionary is turned off). An unknown word is `NOT_FOUND`; a provider that is down is `UNAVAILABLE` (nothing about the provider is sent). Field numbers and enum values use multiples of 10 (`UNSPECIFIED` = 0).

## Settings

Two kinds, both optional to set (a missing value is the default, so a new setting needs no migration):

- **System settings**, edited by an administrator (admin app, page Dictionary): rows `key` / `value jsonb` of `dictionary_settings` (migration `V18`). Keys: `max_saved_words` (1 to 1000, default `DICTIONARY_MAX_SAVED_WORDS`), `allow_export_deck`, `allow_export_link`, `english_enabled`, `english_vi_enabled` (default on; at least one dictionary stays on). They are enforced here: `SaveWord` uses the stored maximum (words above a lowered maximum stay, only a new save is refused), `ExportAnki` / `CreateDeckLink` answer `FEATURE_DISABLED` when off, a lookup in a turned off dictionary answers `DICTIONARY_DISABLED`.
- **Preferences of a user** (dict app, page Settings): one JSON object for the application `dictionary` in `identity_user_settings`, which identity owns. Dictionary calls `GetMyUserSettings` / `PutMyUserSettings` of identity on behalf of the user: it exchanges the token of the request for one with the audience `DICTIONARY_IDENTITY_AUDIENCE` (RFC 8693, `gocommon/tokenexchange`) and calls `DICTIONARY_IDENTITY_ADDR`. The preferences narrow what the system allows; the web app applies them (enabled and default dictionary, deck name, words per lookup), the server only validates them. If identity is down `GetMySettings` answers with the defaults.

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

`dictionary_saved_words` (migration `V15__dictionary_saved_words.sql`) holds one row per user and word.

- A row is **pending** while `exported_at` is `NULL`. The pending rows are the next export and count against `DICTIONARY_MAX_SAVED_WORDS` (default 100). Saving a pending word again overwrites it (card, audio and save time, so it moves to the top of the list); the limit is checked under a per-user advisory lock, so parallel saves cannot pass it.
- `ExportAnki` reads the pending words (oldest first, one card each: the word on the front, the saved HTML on the back), builds the deck and then sets `exported_at = now()` on exactly those rows. That starts a new batch: the pending count is back to 0. A deck that fails or is too large leaves the words pending. Words saved while the deck was built stay pending.
- Saving a word that was already exported makes it pending again.
- Exported rows are kept for `DICTIONARY_EXPORTED_RETENTION` (default `168h`, 7 days) and then deleted by a cleanup that runs inside the service every `DICTIONARY_CLEANUP_INTERVAL` (default `1h`), in batches of 500 rows with `FOR UPDATE SKIP LOCKED`, so several replicas can run it. There is no API to export an old batch again; the retention only covers a response that got lost.
- The card back may contain the placeholders `replace_audio_uk` and `replace_audio_us`: in the deck they become sound tags of the downloaded files (or nothing when a file could not be downloaded).

Audio files are downloaded by the service when it builds a deck, so the URLs a client saves are restricted: `https` and a host of `DICTIONARY_AUDIO_HOSTS` (default `api.dictionaryapi.dev`, `upload.wikimedia.org`, `ssl.gstatic.com`), checked at save time, again at download time and on every redirect. Files are at most 512 KiB, the downloads of one deck share a 20 second budget, and a file that fails is left out without failing the export.

### Download link (Anki app "import from a link")

AnkiMobile can import a deck from a URL, and it fetches it without cookies. `CreateDeckLink` therefore stores the deck in `dictionary_deck_links` (migration `V17`) behind a random 256-bit token: only the SHA-256 of the token is kept, a user has one link at a time (a new one replaces the old), the link works **once** (the download deletes the row) and for `DICTIONARY_DECK_LINK_TTL` (default `15m`); the cleanup loop deletes the expired rows. Gatekeeper serves it at `GET /api/dictionary/downloads/{token}.apkg`, the only anonymous route: it calls `DownloadDeck` with its own service token, and the method refuses user tokens. The token in the URL is the whole authorization, so treat the link like a password until it is used.

The deck travels in the gRPC response (and as base64 in the JSON of gatekeeper), so it is capped by `DICTIONARY_EXPORT_MAX_BYTES` (default 3 MiB, also the gRPC send limit of this service). Raising it needs the same room in gatekeeper.

## Local development

`go.mod` pins released versions of `gocommon` and `protogen`. To work against the sibling checkouts instead, use a Go workspace (`go.work` is git-ignored and not sent to Docker builds):

```bash
go work init . ../gocommon ../protogen/go   # once, in dictionary/; delete go.work to go back to the pinned versions
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

All settings are environment variables (see `.env.example`).

| Variable | Default | Notes |
| --- | --- | --- |
| `HOST` / `PORT` | `0.0.0.0` / `9090` | gRPC listen |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful drain |
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
| `DICTIONARY_MAX_SAVED_WORDS` | `100` | Default of the setting "max saved words" (1 to 1000); the value an administrator saved wins |
| `DICTIONARY_EXPORT_MAX_BYTES` | `3145728` | Largest `.apkg`; also the gRPC send limit |
| `DICTIONARY_DECK_LINK_TTL` | `15m` | How long a download link works (1m to 24h) |
| `DICTIONARY_EXPORTED_RETENTION` | `168h` | How long exported rows are kept (at least `1h`) |
| `DICTIONARY_CLEANUP_INTERVAL` | `1h` | Period of the cleanup (at least `1m`) |
| `DICTIONARY_AUDIO_HOSTS` | `api.dictionaryapi.dev,upload.wikimedia.org,ssl.gstatic.com` | Allowed hosts of audio URLs; comma or one per line |
| `DICTIONARY_LOOKUP_CACHE_TTL` / `SIZE` | `24h` / `2000` | In-memory cache of entries; `0` turns it off |
| `DICTIONARY_HTTP_TIMEOUT` | `10s` | One call to a provider or an audio file |
| `DICTIONARY_USER_AGENT` | `huponx-dictionary/1.0 (+https://github.com/huponx/dictionary)` | Sent to the providers |
| `DICTIONARY_VI_FALLBACK_MT` | `true` | Machine translation when the Wiktionary has no entry |
| `DICTIONARY_FREEDICT_URL` / `WIKTIONARY_URL` / `MYMEMORY_URL` | the public endpoints | Overridden in tests |

## Kubernetes

- gRPC health on the API port (`grpcserver` health is on by default, no token required for `/grpc.health.v1.Health`); the chart probes the TCP port like courier.
- The pod is read-only apart from `/tmp` (an `emptyDir` in the chart): the Anki library writes the deck through a temporary SQLite file.
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
    curl -s -b cookies.txt https://api.example.com/api/dictionary/words/hello
    ```
