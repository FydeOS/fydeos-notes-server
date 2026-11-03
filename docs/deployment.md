# Deployment

This guide covers running the server in production. For a first try, the
[Docker Compose example](../deploy/compose.yaml) is the quickest start.

## Building the image

```sh
make docker VERSION=1.0.0
```

The image contains the server binary, `LICENSE`, `NOTICE` and the licenses of
all Go dependencies under `/app/licenses`. It runs as an unprivileged `notes`
user whose home directory is `/var/lib/notes`, so the default storage path is
`/var/lib/notes/.affine/storage`. Mount a volume there, or configure S3 (see
[Configuration](configuration.md#file-storage)).

To serve the web app from the same container, mount a built copy of the
FydeOS Notes web app at `/app/static`.

## Every release

Run these two commands with the new version before starting its servers.
Both are safe to repeat and do nothing if there is nothing to do.

```sh
fydeos-notes-server migrate
fydeos-notes-server ensure-private-key
```

Run them from one place, such as a one-off job, not from every server
process. Back up the database before upgrading.

## Reverse proxy

Put the server behind a reverse proxy that terminates TLS, and set
`AFFINE_SERVER_EXTERNAL_URL` to the public URL. The proxy must:

- forward WebSocket upgrades on `/socket.io/`;
- allow request bodies at least as large as your largest upload;
- pass the `Host` header and `X-Forwarded-For`.

Example for nginx:

```nginx
server {
    listen 443 ssl;
    server_name notes.example.com;
    client_max_body_size 100m;

    location / {
        proxy_pass http://127.0.0.1:3010;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

## Scaling out

One `allinone` process is enough for most teams. To scale, run several
processes with different `SERVER_FLAVOR` values against the same PostgreSQL,
Redis and storage, and route them behind one origin:

| Path                              | Flavor     |
| --------------------------------- | ---------- |
| `/graphql`, `/api/`               | `graphql`  |
| `/socket.io/`                     | `sync`     |
| `/workspace/` (shared pages)      | `renderer` |
| everything else                   | `front`    |

Run at least one `worker` process for background jobs; workers do not serve
HTTP. You can run several copies of each role.

## Backups

Back up three things together, from the same point in time:

1. The PostgreSQL database (`pg_dump`, or your provider's snapshots).
2. File storage: the storage directory or the S3/R2 buckets.
3. `NOTES_PRIVATE_KEY_SECRET`. Without it, stored credentials such as AI
   provider keys and calendar tokens cannot be decrypted.

## Health checks

| Endpoint      | Meaning                                         |
| ------------- | ----------------------------------------------- |
| `GET /readyz` | `200` when the server can reach its database    |
| `GET /info`   | Server version, for monitoring and debugging    |

## Security checklist

- Create the first administrator before the server is reachable from the
  internet (see the [README](../README.md#first-administrator)).
- Use long random values for `NOTES_PRIVATE_KEY_SECRET` and all database,
  Redis and storage passwords.
- Keep PostgreSQL and Redis on a private network.
- Turn off `auth.allowSignup` if only invited people should have accounts.
