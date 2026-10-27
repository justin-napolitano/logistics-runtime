# Security boundaries

Public application code; private instances and operational data remain separate.
All data endpoints require bearer authentication. Keep tokens, database credentials,
private addresses, raw mail and production records out of source, logs and fixtures.
Email input is untrusted, bounded and parsed without network or model calls. Reports
are not authenticated proof of purchase or accepted physical receipts. Product links
are allowlisted but not fetched. A basket is not spending authorization. Hosted TLS,
private access, scoped runtime grants, secrets and backups need deployment-specific
configuration. No live merchant integration or cloud deployment is included.

Mailbox access uses delegated read-only OAuth with PKCE/state validation and a
loopback callback. Permissions cover the mailbox; sender, date and folder filters
are application restrictions, not separate OAuth permission boundaries. Tokens
reside in US Infisical, while private provider cursors and identity hashes reside
in PostgreSQL. Status responses omit both. Provider redirects are disabled.
Sender headers only route parsing; they do not authenticate a merchant. Unsupported
formats remain exceptions and cannot change inventory. Do not expose the database
or grant untrusted users connection-configuration access.
