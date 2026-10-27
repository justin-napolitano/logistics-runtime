# Security boundaries

Public application code; private instances and operational data remain separate.
All data endpoints require bearer authentication. Keep tokens, database credentials,
private addresses, raw mail and production records out of source, logs and fixtures.
Email input is untrusted, bounded and parsed without network or model calls. Reports
are not authenticated proof of purchase or accepted physical receipts. Product links
are allowlisted but not fetched. A basket is not spending authorization. Hosted TLS,
private access, scoped runtime grants, secrets and backups need deployment-specific
configuration. No live merchant integration or cloud deployment is included.
