# FydeOS Notes Server

The open-source, self-hostable server for **FydeOS Notes**.

[FydeOS Notes](https://notes.fydeos.io) is a note-taking and whiteboard app
made by FydeOS, available at [notes.fydeos.io](https://notes.fydeos.io) and
[notes.fydeos.com](https://notes.fydeos.com). Its editor is built on
[AFFiNE](https://github.com/toeverything/AFFiNE)'s open-source editor. This
repository is the open-source edition of the FydeOS Notes server, written in
Go, so you can run FydeOS Notes on your own infrastructure and keep your data
on your own servers.

One binary serves the GraphQL API, real-time collaboration, shared pages,
the web app's static files and background jobs. Run everything in one
process, or split the roles across several.

## How this relates to AFFiNE

- **FydeOS Notes uses AFFiNE's editor.** The FydeOS Notes web app is based on
  AFFiNE's MIT-licensed editor and frontend, adapted by FydeOS.
- **This server speaks the same client protocol.** It implements the GraphQL
  API, real-time sync (Yjs over Socket.IO) and document format that those
  clients expect. That is why you will see `affine` in some protocol names,
  headers, block types and environment variables; they are kept so the
  clients work unchanged.
- **It is a separate project.** This server is maintained by FydeOS. It is
  not the AFFiNE server, and it is not affiliated with or endorsed by the
  AFFiNE project or TOEVERYTHING PTE. LTD. Questions about this server
  belong in this repository, not in AFFiNE's.

## Compatibility

| Client                                 | Status                                     |
| -------------------------------------- | ------------------------------------------ |
| FydeOS Notes web app (browser)         | Supported; this is what we test against    |
| FydeOS Notes admin panel (`/admin`)    | Supported                                  |
| FydeOS Notes desktop app (Electron)    | Not yet tested                             |
| Other AFFiNE-based clients             | May work, but not tested or supported      |

## Hosted or self-hosted?

|                         | notes.fydeos.io / notes.fydeos.com | This server, self-hosted                   |
| ----------------------- | ----------------------------------- | ------------------------------------------ |
| Who runs it             | FydeOS                              | You                                        |
| Where data lives        | FydeOS servers                      | Your PostgreSQL and storage                |
| Sign-in                 | FydeOS account                      | Email and password, magic link, OAuth/OIDC |
| AI assistant            | Provided                            | Bring your own provider keys               |
| Price                   | See the service                     | Free, MIT-licensed                         |

If you just want to take notes, use the hosted service. Self-host if you need
your data on your own infrastructure, or want to build on the server.

## Features

- Workspaces, members, invitations and per-document permissions
- Real-time collaborative editing (Yjs over Socket.IO), document history
- Sign-in with email and password, magic links and OAuth/OIDC
- File storage on the local disk or any S3-compatible service
- Comments, notifications and email
- Full-text search, and vector search with pgvector
- AI assistant with your own provider keys (OpenAI, Anthropic, Gemini and
  others), with usage limits
- Calendar sync with Google Calendar and CalDAV
- An MCP endpoint so AI tools can read a workspace
- Admin API for users, workspaces and server settings

## Documentation

- [Configuration](docs/configuration.md): storage, email, sign-in, AI and
  other settings
- [Deployment](docs/deployment.md): images, upgrades, reverse proxy, scaling,
  backups
- [Integration tests](test/README.md)

## Requirements

| Component  | Version                     | Notes                                       |
| ---------- | --------------------------- | ------------------------------------------- |
| PostgreSQL | 14 or later                 | Install `pgvector` to enable vector search  |
| Redis      | 7 or later                  | Sessions, real-time fan-out, caches         |
| Storage    | Local disk or S3-compatible | Defaults to `~/.affine/storage`             |
| ffmpeg     | Any recent version          | Optional; needed for audio transcription    |
| Go         | 1.26 or later               | Only to build from source                   |

## Quick start with Docker Compose

```sh
docker compose -f deploy/compose.yaml up -d
```

This builds the image and starts PostgreSQL, Redis and the server on
<http://localhost:3010>. A one-off `migrate` container prepares the database
first. Change every `change-me` value in `deploy/compose.yaml` before you use
it for anything real.

Then create the first administrator (see [First administrator](#first-administrator)).

## Running from source

```sh
make build  # writes bin/fydeos-notes-server

export DATABASE_URL="postgres://notes:secret@localhost:5432/notes"
export REDIS_SERVER_URL="redis://localhost:6379/0"
export NOTES_PRIVATE_KEY_SECRET="a-long-random-string"

./bin/fydeos-notes-server migrate             # create or update the database schema
./bin/fydeos-notes-server ensure-private-key  # create the server signing key
./bin/fydeos-notes-server                     # start the server on port 3010
```

Run `migrate` and `ensure-private-key` before every start of a new version.
Both are safe to repeat.

`GET /info` returns the server version and `GET /readyz` reports whether the
server can reach its database.

### Serving the web app

The server serves the FydeOS Notes web app if a built copy is in a `static/`
directory next to the binary or in the working directory (`/app/static` in
the container). Without it, the server only provides the API, and you serve
the frontend separately.

## First administrator

A new server has no users. The first administrator can be created once, by
whoever reaches the server first, so finish this step before exposing the
server to the internet.

- In the web app, open `/admin/setup` and follow the steps.
- Or call the API directly:

  ```sh
  curl -X POST http://localhost:3010/api/setup/create-admin-user \
    -H 'Content-Type: application/json' \
    -d '{"name":"Admin","email":"admin@example.com","password":"choose-a-strong-password"}'
  ```

After that, the endpoint refuses further requests. Sign in with the new
account and manage everything else from the admin panel at `/admin`.

## Configuration

Settings come from environment variables and from an optional JSON file. The
file is read from `AFFINE_BACKEND_RUNTIME_CONFIG_PATH`, else `./config.json`,
else `~/.affine/config/config.json`. Most server settings (mail, OAuth
providers, storage, AI) can also be changed at runtime in the admin panel.

| Variable                       | Default                   | Description                                                     |
| ------------------------------ | ------------------------- | --------------------------------------------------------------- |
| `DATABASE_URL`                 | —                         | PostgreSQL connection string (required)                         |
| `REDIS_SERVER_URL`             | —                         | Redis URL, or use `REDIS_SERVER_HOST`, `_PORT`, `_PASSWORD`, `_DATABASE` |
| `NOTES_PRIVATE_KEY_SECRET`     | —                         | Encrypts the server signing key stored in the database          |
| `AFFINE_SERVER_EXTERNAL_URL`   | —                         | Public URL, e.g. `https://notes.example.com`                    |
| `AFFINE_SERVER_HOST`           | `localhost`               | Public host name, if `AFFINE_SERVER_EXTERNAL_URL` is not set    |
| `AFFINE_SERVER_HTTPS`          | `false`                   | Whether the public URL uses HTTPS                               |
| `AFFINE_SERVER_PORT`           | `3010`                    | Listen port                                                     |
| `AFFINE_SERVER_SUB_PATH`       | —                         | Serve under a path prefix, e.g. `/notes`                        |
| `SERVER_FLAVOR`                | `allinone`                | Which role this process runs, see below                         |
| `DEPLOYMENT_TYPE`              | `selfhosted`              | Leave as `selfhosted`                                           |
| `NODE_ENV`                     | `production`              | `production`, `development` or `test`                           |

Keep `NOTES_PRIVATE_KEY_SECRET` safe and do not change it: the stored key, and
the credentials encrypted with it, cannot be read without it.

Some names start with `AFFINE_` because the FydeOS Notes web app is built on
the AFFiNE editor, and these names are part of the protocol it shares with
the server. The same applies to request headers, cookie names and document
block types.

### Server roles

| `SERVER_FLAVOR` | Serves                                     |
| --------------- | ------------------------------------------ |
| `allinone`      | Everything below, in one process           |
| `graphql`       | GraphQL and REST API                       |
| `sync`          | Real-time document sync (Socket.IO)        |
| `front`         | Web app files and sign-in pages            |
| `renderer`      | Shared-page rendering and link previews    |
| `worker`        | Background jobs                            |

Put a reverse proxy in front of split roles so they share one public origin.

## Commands

| Command                                  | Purpose                                        |
| ---------------------------------------- | ---------------------------------------------- |
| `fydeos-notes-server`                    | Start the server                               |
| `fydeos-notes-server migrate`            | Create or update the database schema           |
| `fydeos-notes-server ensure-private-key` | Create the server signing key if it is missing |
| `fydeos-notes-server version`            | Print the version and build revision as JSON   |

## Development

```sh
make build              # build bin/fydeos-notes-server
make test               # unit tests; tests that need services are skipped
make test-integration   # start PostgreSQL, Redis and S3 in Docker and run every test
make vet
make fmt
make generate           # regenerate GraphQL code after editing schema.graphqls
make deps-down          # stop the test services and delete their data
```

See [test/README.md](test/README.md) for details.

### Layout

```
cmd/fydeos-notes-server/  Command-line entry point and server wiring
internal/                 One package per area: auth, workspaces, documents, blobs,
                          copilot, search, realtime, worker, ...
internal/graphql/         GraphQL schema, resolvers and generated code (gqlgen)
internal/database/        PostgreSQL pool and schema migrations
deploy/                   Example Docker Compose deployment
docs/                     Configuration and deployment guides
test/                     Services for integration tests
third_party/              Licenses of bundled third-party data
```

Packages talk to each other through service interfaces. GraphQL resolvers,
HTTP handlers and workers must not write tables directly or skip a domain
package's checks.

### Changing the database schema

Add a new numbered file to `internal/database/migrations/`. Never edit a
migration that has been released: `migrate` records a checksum of each file
and refuses to run if a released file changes.

## Not supported

- Paid plans, subscriptions and license keys. The related GraphQL fields
  exist for client compatibility but return an empty result or a "not
  available" error.
- Upgrading a database created by another server. This edition sets up new
  installations only.

## Contributing

Issues and merge requests are welcome. Before you submit:

1. Run `make fmt vet test`, and `make test-integration` if you touched
   database, storage or sync code.
2. Add or update tests for behaviour changes.
3. Only contribute code you wrote yourself or that is under a license
   compatible with this project's.

## License

Copyright (c) 2026 Fyde Innovations Limited.

Licensed under the [MIT License](LICENSE). See [NOTICE](NOTICE) for
third-party components and their notices. Run `make third-party-licenses`
to collect the licenses of all Go dependencies.

FydeOS Notes is not affiliated with or endorsed by the AFFiNE project or
TOEVERYTHING PTE. LTD.
