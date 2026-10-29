# Security boundaries

Public application code; private instances and operational data remain separate.
All data endpoints require bearer authentication. Keep tokens, database credentials,
private addresses, raw mail and production records out of source, logs and fixtures.
Email input is untrusted, bounded and parsed without network or model calls. Reports
are not authenticated proof of purchase or accepted physical receipts. Product links
are allowlisted but not fetched. A basket is not spending authorization. Hosted TLS,
private access, scoped runtime grants, secrets and backups need deployment-specific
configuration. No cloud deployment is included. The optional Python shopping
worker uses a private, dedicated local browser profile and explicit manifests.
Treat its profile as account credentials; keep it and cart journals outside Git.
It prepares carts only, with manual checkout and no purchase submission. Live
merchant mutation has not yet been verified. It does not authenticate order
reports or establish physical inventory; see tools/shop/README.md for limits.

Mailbox access uses delegated read-only OAuth with PKCE/state validation and a
loopback callback. Permissions cover the mailbox; sender, date and folder filters
are application restrictions, not separate OAuth permission boundaries. Tokens
reside in US Infisical, while private provider cursors and identity hashes reside
in PostgreSQL. Status responses omit both. Provider redirects are disabled.
Sender headers only route parsing; they do not authenticate a merchant. Unsupported
formats remain exceptions and cannot change inventory. Do not expose the database
or grant untrusted users connection-configuration access.

Cloud Run can exchange a Google metadata identity token for scoped Infisical
access; restrict the Infisical identity to the dedicated runtime service account.
Configuration secrets can be bootstrapped by an explicit setting-name allowlist.
Use separate read-only configuration and writable OAuth-token folders; the worker
must not read migration credentials. Auth modes are mutually exclusive, and the
configured Infisical origin must be HTTPS without userinfo, query or path.
The distroless container runs as numeric non-root with an allowlisted build context.
