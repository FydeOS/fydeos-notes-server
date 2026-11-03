# Integration tests

Most packages have plain unit tests that need nothing but Go. Tests that need
PostgreSQL, Redis or S3 are skipped unless you point them at disposable
services. A skipped test is not a passing test: when you report results,
include the number of skipped tests.

## Running the full suite

Run from the repository root. You need Docker with Compose.

```sh
make test-integration
```

Or step by step:

```sh
docker compose -f test/compose.yaml up -d --wait
export NOTES_TEST_DATABASE_URL="postgres://notes:notes@$(docker compose -f test/compose.yaml port postgres 5432)/notes_test?sslmode=disable"
export NOTES_TEST_REDIS_URL="redis://$(docker compose -f test/compose.yaml port redis 6379)/0"
export NOTES_TEST_S3_ENDPOINT="http://$(docker compose -f test/compose.yaml port s3 9000)"

go test ./... -count=1 -timeout=15m
go test -race ./internal/blobs ./internal/graphql ./internal/storage ./internal/worker -count=1 -timeout=15m
```

`test/compose.yaml` starts PostgreSQL 14 with pgvector, Redis 7 and RustFS (an
S3-compatible store). Images are pinned by digest.

- Services listen on loopback only, on ports chosen by Docker.
- The credentials in `compose.yaml` are for throwaway test data only.
- Every database test creates its own schema and drops it afterwards. S3
  tests create a uniquely named bucket and remove it on exit.
- The tests refuse to run against a non-loopback host or a database not named
  `notes_test`.
- To run several copies side by side, pass the same `-p <name>` to every
  `docker compose` command.

Stop the services and delete their data:

```sh
docker compose -f test/compose.yaml down -v
```

## Search capacity check

An opt-in test measures search over 1,000 and 10,000 documents (10,000 and
100,000 content blocks), half of them unreadable by the querying user. It
checks permissions, counts, ordering, de-duplication and pagination, and
reports wall time, allocations and peak heap growth.

```sh
NOTES_TEST_SEARCH_CAPACITY=1 go test ./internal/graphql \
  -run '^TestWorkspaceSearchAndAggregateFilterByCanonicalDocACL$' \
  -v -count=3 -timeout=5m
```

Run it on its own, with no other database load. The limits it enforces (30
seconds per run, 512 MiB heap growth per concurrent request) catch
regressions on synthetic data. They are not production latency targets.

## Golden files

Some tests compare output with files in `testdata/` directories. To
regenerate the sample document used by the Markdown and MCP tests:

```sh
go test ./internal/documents -run TestSampleDocumentFixture -update
```

Review the diff before committing regenerated files.
