# Logistics runtime

Go/PostgreSQL inventory comparisons, reusable shopping proposals and offline email
order reports. This export contains application code and fabricated fixtures only.
It has no original repository history, personal inventory, order data, owner-specific
shopping template, deployment credentials or private planning issues.

## Build and review

```sh
go build -trimpath -o bin/logistics ./cmd/logistics
bin/logistics basket-preview examples/templates/sample-restock.json sample /tmp/sample-restock.html
```

The HTML file is a shopping proposal with product links, not a merchant cart or
purchase. Live merchant and mailbox integrations are not implemented. All data API
routes require a bearer token. Private instances should remain private even when
application source code is public.

## Verification

```sh
go mod verify
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

PostgreSQL integration tests require TEST_DATABASE_URL pointing to a dedicated
`logistics_test` database. CI creates only synthetic test data. Without that variable,
database tests explicitly skip. Migrations run only via the explicit `migrate`
command; serving and imports never run DDL. Never run CI fixtures on a real database.

## Implemented commands

- `save-template`, `prepare-basket`, `basket-export`: immutable saved shopping proposals.
- `basket-preview`: private standalone export with no database required.
- `import-mail`, `import-mail-dir`: bounded, deduplicated offline .eml evidence import.
- `orders`, `mail-exceptions`: partial chronological reports and unsupported messages.
- `serve`, `migrate`, `import-proposals`, `openapi`: inventory and application operations.

Email reports are explicitly unverified. No raw message bodies are persisted,
no email links are fetched, and delivered reports never change physical stock.
Mailbox OAuth/sync, actual merchant email-format validation, purchasing, public
browser sessions and cloud deployment remain outside this implementation.

Public source code does not grant an open-source license; no license has been
selected. Personal inventory, actual mail and deployment data belong outside this
repository. All included mail and inventory fixtures are fabricated.
