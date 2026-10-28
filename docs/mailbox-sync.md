# Read-only mailbox workers

Gmail and Outlook workers import Amazon.com order-report evidence through the same
deduplicated importer as offline `.eml` files. No purchases, mailbox writes or stock
changes occur. Endpoints are tested with synthetic HTTP transports and a dedicated
PostgreSQL database. Live account validation and deployment are separate steps.

## Setup

1. Build `go build -trimpath -o bin/logistics ./cmd/logistics`.
2. Set `DATABASE_URL` securely. Review and explicitly run `bin/logistics migrate`
   against the intended database with the migration role. Serving never applies DDL.
   Migration 003 adds `logistics.mail_connections`. A restricted worker role needs
   SELECT/INSERT/UPDATE on this table and the existing mail-import tables, plus
   schema access; no schema ownership or DDL rights. Use direct PostgreSQL or session
   pooling: advisory locks require a pinned session, not transaction pooling.
3. Create a Google Desktop OAuth client and enable Gmail API, or a Microsoft
   mobile/desktop public-client app supporting the intended account types. Google
   uses `http://127.0.0.1:<ephemeral-port>/callback`; Microsoft uses
   `http://localhost:<ephemeral-port>/callback`. Configure loopback redirects
   according to the provider's native-app rules. Set `MAIL_GOOGLE_CLIENT_ID` and
   `MAIL_GOOGLE_CLIENT_SECRET`, or `MAIL_MICROSOFT_CLIENT_ID` and optional
   `MAIL_MICROSOFT_CLIENT_SECRET`; tenant defaults to `common`. Google needs
   `gmail.readonly`; Microsoft needs delegated `Mail.Read`, `User.Read` and
   `offline_access`. Google testing-mode refresh grants may expire after seven days;
   production use can require provider verification. Organizational consent policy
   can also require an administrator.
4. Inject app credentials and Infisical bootstrap credentials securely. Set
   `INFISICAL_URL` to the actual HTTPS instance origin (default US Cloud:
   `https://app.infisical.com`, without `/api`). Set
   `INFISICAL_PROJECT_ID`, `INFISICAL_ENVIRONMENT`, `INFISICAL_SECRET_PATH` and
   `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID` / `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET`.
   Scope that machine identity to read/create/update secrets in a dedicated folder.
   The worker renews machine access tokens automatically. `INFISICAL_TOKEN` is an
   alternative for short-lived jobs and is not renewed. Never put credentials in Git.
   On Cloud Run prefer `INFISICAL_GCP_IDENTITY_ID` instead: the worker exchanges
   the platform's Google-signed identity token for scoped Infisical access, without
   a downloaded Google key or Universal Auth secret. Configure the Infisical identity
   to allow only the logistics runtime service account. Select exactly one auth
   mode; mixed or partial credentials are rejected. The existing task-service
   identity and its permissions remain separate.
5. Copy an example below to a private `*.connection.json`, change its date and IDs,
   then run `mail-configure`. `account` is the logical merchant-account namespace,
   not an email address; use the same value across sources only when intentionally
   combining evidence. Each connection requires a unique Infisical secret reference.
   Configurations are immutable: a changed window/folder requires a new connection
   ID and secret reference. Pause the previous connection first.

```json
{
  "id": "sample-gmail",
  "provider": "gmail",
  "account": "sample-shopping",
  "credential_ref": "SAMPLE_GMAIL_OAUTH",
  "since": "2026-10-01T00:00:00Z",
  "folder": ""
}
```

For Outlook use `provider: "outlook"`, a distinct ID/secret reference and
`folder: "inbox"` (or a Graph folder ID). Gmail's optional folder is a **label ID**,
not its display name. Empty Gmail folder searches the mailbox's normal list scope.
Choose a recent start date: Outlook's filtered initial delta query caps results at
5,000 messages, including messages excluded later by the sender filter. At that
boundary the worker stops with `narrower_backfill_window_required` instead of
silently claiming a complete sync. Outlook sync is per folder; moving an old message
between folders is outside this initial created-message tracking scope.

```sh
bin/logistics mail-configure sample.connection.json
bin/logistics mail-authorize sample-gmail
# Open the printed provider URL locally and sign in there.
bin/logistics mail-sync -max-pages 20 sample-gmail
bin/logistics mail-status
bin/logistics mail-worker -interval 15m -timeout 5m -max-pages 20
```

Authorization runs a five-minute local callback listener. Passwords and authorization
codes never belong in chat. The refresh grant is saved as a JSON secret in US
Infisical, with rotations persisted before the new access token is used. Reconnecting
checks the mailbox identity before replacing the stored grant. Google may require
revoking an earlier app grant if it does not issue an offline refresh token.

## Scheduling and recovery

`mail-worker` runs immediately, then every 15 minutes by default. Alternatively,
schedule bounded `mail-sync` jobs. No cloud job or scheduler is provisioned here.
Without a connection ID both commands process all configured connections serially.
Each connection holds a PostgreSQL advisory lock, so overlapping workers skip it.
Each active connection needs a reserved database connection plus capacity for imports.
Start with one worker; replicas are unnecessary for a personal mailbox.

Gmail captures its history watermark before backfill, then catches up via history.
Outlook preserves opaque next/delta links. Checkpoints advance only after an entire
page is handled; interrupted pages replay safely through transactional deduplication.
Expired cursors trigger one bounded backfill recovery per run using the configured
start date. The page budget bounds work and reports `pending` when more remains.
Transient requests retry up to four attempts, respecting bounded Retry-After delays;
long delays defer work to the next scheduled pass. Rejected grants report
`reconnect_required` without repeated authorization attempts until an operator acts.

```sh
bin/logistics mail-pause sample-gmail
bin/logistics mail-resume sample-gmail
bin/logistics mail-reset sample-gmail
```

Reset clears the cursor and resumes replay from the original date; it never deletes
evidence or changes the configuration. Resume does not repair a revoked grant:
run `mail-authorize` to reconnect. The authenticated `GET /v1/mail/sync-status`
endpoint exposes the same redacted status as `mail-status`. Imported counts mean
handled import attempts in the latest pass, including deduplicated replays, not new
orders. Secrets, cursors and mailbox identity hashes are excluded from this output.

Only matching Amazon sender metadata leads to MIME downloads. Read-only consent
still grants mailbox-wide access. Raw MIME is bounded to 2 MiB and discarded after
parsing. Oversized messages create a metadata-only exception with a provider-ID
marker hash (not a raw-message digest), allowing subsequent messages to proceed.
Unsupported formats remain exceptions. Deleted mail does not erase evidence.
Email headers and order reports remain unverified, even after provider OAuth.

## Provider references

- [Gmail synchronization](https://developers.google.com/workspace/gmail/api/guides/sync)
- [Google native-app OAuth](https://developers.google.com/identity/protocols/oauth2/native-app)
- [Outlook message delta](https://learn.microsoft.com/en-us/graph/delta-query-messages)
- [Microsoft authorization code flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow)
- [Infisical v4 secrets](https://infisical.com/docs/api-reference/endpoints/secrets/read)
- [Infisical GCP identity auth](https://infisical.com/docs/documentation/platform/identities/gcp-auth)

## Container deployment

The pinned, non-root Dockerfile builds the same CLI used locally. Build once with
the tested commit as `REVISION`, then deploy an immutable image digest. Container
build context excludes Git history, local configuration, credentials and fixtures.
No real operational data belongs in the image.

```sh
docker build --platform linux/amd64 --build-arg REVISION=TESTED_COMMIT -t logistics-runtime:TESTED_COMMIT .
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges --network none logistics-runtime:TESTED_COMMIT openapi
```

For hosted execution set non-secret Infisical location/identity fields and
`INFISICAL_BOOTSTRAP_SECRETS=DATABASE_URL,MAIL_GOOGLE_CLIENT_ID,MAIL_GOOGLE_CLIENT_SECRET`
(use Microsoft names for Outlook). Each named secret is loaded from the scoped
folder into the process environment before database/provider initialization. Set
`INFISICAL_CONFIG_PATH` to a separate read-only configuration folder; keep
`INFISICAL_SECRET_PATH` scoped to writable mailbox-token secrets. When omitted,
configuration reads use the token folder for compatibility. For
an API deployment include `API_TOKEN`; for a migration job bootstrap only
`DATABASE_URL` from a separate migration-only identity/folder. Unknown or duplicate
bootstrap names fail before any secret access. Do not grant the worker migration
credentials, schema ownership or broad project secret access.

Use `mail-sync -timeout 5m -max-pages 20 CONNECTION_ID` as a bounded Cloud Run Job,
one task/parallelism 1 and no platform retries. Cloud Scheduler can invoke it every
15 minutes through a dedicated job-scoped invoker identity. Start with the schedule
paused until the limited live-mail acceptance test passes. Configure job-failure
and missing-execution alerts with an actual notification channel before activation.
An always-running `mail-worker` is not appropriate for a request-throttled,
scale-to-zero HTTP service. Environment-specific resources, shared PostgreSQL
placement, grants and deployment records belong in the private infrastructure
repository; this image does not provision them.
