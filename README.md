# eCommerce Go API

A Go implementation of the shop's backend. It serves the **same HTTP API** as the NestJS server in
[`eCommerce-api`](../eCommerce-api) against the **same databases** (PostgreSQL, MongoDB, Meilisearch, Redis,
RabbitMQ), so a frontend can talk to either one and the two can even run side by side on one stack.

| | Nest (`eCommerce-api`) | Go (this repository) |
|---|---|---|
| Port in the compose files | 3333 | 3334 |
| Owns the PostgreSQL schema | **yes** (Prisma migrations) | no, it uses the same tables |
| Image | ~825 MB | ~43 MB |

## How it is kept identical

1. **The Nest test suite runs against this server.** `eCommerce-api` has a test harness mode: with `GO_API_BIN`
   pointing at the binary built here, the 65 integration specs start it as a child process and run unchanged over
   HTTP (auth, accounts, catalog, admin catalog, cart, checkout, orders, payments, messaging, metrics, the
   contract-shape checks). One test needs the Nest process's internals and is covered by a Go test instead.
2. **The shared business rules are checked against golden output from the TypeScript original.** `testdata/golden.json`
   is produced by running `@ecom/contracts` over 400 generated carts and many coupons, amounts, products and roles;
   `internal/domain/golden_test.go` requires the Go port to match it exactly.
3. **Passwords and tokens interoperate.** Argon2id hashes are standard PHC strings in both directions, and the
   JWT access tokens use the same secret, claims, audience and issuer, so a user created on one server can sign in
   on the other and a token from one is accepted by the other.

## Run it

```bash
# 1. The data stores and the schema come from the Nest stack (eCommerce-api):
cd ../eCommerce-api && docker compose up -d postgres redis mongo rabbitmq meilisearch api-migrate catalog-seed
# 2. The Go API:
cd ../eCommerce-go && docker compose up --build -d      # http://localhost:3334
```

Or without Docker, with the same environment variables as the Nest API (`DATABASE_URL`, `MONGODB_URL`, `MEILI_URL`,
`REDIS_URL`, `RABBITMQ_URL`, `JWT_ACCESS_SECRET`, `JWT_REFRESH_SECRET`, `CORS_ORIGINS`, optional Razorpay test keys):

```bash
go run ./cmd/api
```

## Test it

```bash
go vet ./... && go test ./...          # unit tests, the golden parity tests, the scheduler test (needs the test Postgres)

# the shared integration specs, against this server
go build -o bin/api.exe ./cmd/api
cd ../eCommerce-api/apps/api
GO_API_BIN=<path to bin/api.exe> pnpm exec vitest run test/
```

Regenerate the golden file after the contract changes: `node testdata/gen/gen.mjs` (needs `eCommerce-contracts` built next to this repository).

## Layout

`cmd/api` entry point · `internal/server` wiring (the Nest `AppModule`) · `internal/httpx` HTTP plumbing (errors,
request ids, CORS, security headers, body validation, guards) · `internal/domain` the Go port of `@ecom/contracts` ·
`internal/db` typed queries on the Prisma-owned tables · `internal/{auth,accounts,addresses,catalog,commerce,cache,messaging,metrics,resilience,health}` the modules.

## Differences from the Nest server

- There is no OpenAPI/Swagger page (`/docs`); the routes are the same.
- `ETag`s differ in value (they are derived from a different JSON encoding), and Express's automatic weak ETag on
  other responses is not reproduced.
- The database schema is not migrated from here. Run the Nest API's migrations (`pnpm db:migrate:deploy`, or its
  `api-migrate` compose service) first.
- Only the modules the Nest API has exist (identity, catalog and search, cart/checkout/orders/payments, messaging,
  observability). The newer frontend-only features (returns, promotions, recommendations, analytics, marketplace)
  have no server-side implementation in either backend yet.
