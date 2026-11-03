# Configuration

The server reads settings from three places:

1. **Environment variables** for things the server needs before it can reach
   the database: database and Redis addresses, ports, the public URL and
   secrets. See the table in the [README](../README.md#configuration).
2. **A JSON configuration file**, read from
   `AFFINE_BACKEND_RUNTIME_CONFIG_PATH`, else `./config.json`, else
   `~/.affine/config/config.json`. It provides defaults for runtime settings.
3. **The admin panel** (`/admin`), which stores runtime settings in the
   database. A value saved there overrides the same key in the file.

Changes to the file or environment take effect after a restart.

## File format

The file is a JSON object. Each runtime setting has a dotted key such as
`storages.blob.storage`; in the file, write it as nested objects:

```json
{
  "server": {
    "name": "Team Notes",
    "externalUrl": "https://notes.example.com"
  },
  "storages": {
    "blob": {
      "storage": { "provider": "fs", "bucket": "blobs", "config": { "path": "/data/storage" } }
    }
  }
}
```

## File storage

Uploaded files, avatars and AI assistant files are kept in three storage
scopes:

| Key                      | Holds                        | Default bucket |
| ------------------------ | ---------------------------- | -------------- |
| `storages.blob.storage`  | Files attached to documents  | `blobs`        |
| `storages.avatar.storage`| User and workspace avatars   | `avatars`      |
| `copilot.storage`        | Files used by the AI assistant | `copilot`    |

By default all three use the local disk under `~/.affine/storage`. Each value
has a `provider`, a `bucket` and a provider-specific `config`.

### Local disk

```json
{ "provider": "fs", "bucket": "blobs", "config": { "path": "/data/storage" } }
```

### S3 or an S3-compatible service

```json
{
  "provider": "aws-s3",
  "bucket": "notes-blobs",
  "config": {
    "region": "us-east-1",
    "endpoint": "https://s3.example.com",
    "forcePathStyle": true,
    "credentials": {
      "accessKeyId": "...",
      "secretAccessKey": "..."
    }
  }
}
```

`endpoint` and `forcePathStyle` are only needed for S3-compatible services
such as MinIO or RustFS.

### Cloudflare R2

```json
{
  "provider": "cloudflare-r2",
  "bucket": "notes-blobs",
  "config": {
    "accountId": "...",
    "credentials": { "accessKeyId": "...", "secretAccessKey": "..." }
  }
}
```

For S3 and R2, browsers upload and download through short-lived signed URLs,
so the bucket must allow CORS requests from your server's public origin.

## Email

Email is needed for magic-link sign-in, invitations and notifications.
Configure SMTP with environment variables or in the file under `mailer`:

| Environment variable | File key              | Default                               |
| -------------------- | --------------------- | ------------------------------------- |
| `MAILER_HOST`        | `mailer.SMTP.host`    | —                                     |
| `MAILER_PORT`        | `mailer.SMTP.port`    | `465`                                 |
| `MAILER_USER`        | `mailer.SMTP.username`| —                                     |
| `MAILER_PASSWORD`    | `mailer.SMTP.password`| —                                     |
| `MAILER_SECURE`      | `mailer.SMTP.secure`  | `false`: STARTTLS when offered; `true`: TLS from the start |
| `MAILER_IGNORE_TLS`  | `mailer.SMTP.ignoreTLS` | `false` (skip certificate checks when `true`) |
| `MAILER_SENDER`      | `mailer.SMTP.sender`  | `FydeOS Notes <noreply@example.com>`  |

Environment variables take precedence over the file and the admin panel.
Send a test message from the admin panel to check the settings.

## Sign-in

| Key                              | Description                                                   |
| -------------------------------- | ------------------------------------------------------------- |
| `auth.allowSignup`               | Whether anyone can create an account                          |
| `auth.allowSignupForOauth`       | Whether OAuth sign-in can create new accounts                 |
| `auth.requireEmailDomainVerification` | Before a new account signs up by email, check that its domain has mail (MX) records |
| `auth.passwordRequirements`      | `{ "min": 8, "max": 32 }` by default                          |
| `oauth.providers.google`         | Google sign-in client settings                                |
| `oauth.providers.github`         | GitHub sign-in client settings                                |
| `oauth.providers.oidc`           | Any OpenID Connect provider                                   |

OAuth providers are easiest to set up in the admin panel, which shows the
fields each provider needs.

## AI assistant

The assistant is off by default. Turn it on with `copilot.enabled`, then let
workspaces use their own provider keys with `copilot.byok.enabled`.

| Key                                  | Description                                                   |
| ------------------------------------ | ------------------------------------------------------------- |
| `copilot.enabled`                    | Turn the assistant on                                         |
| `copilot.byok.enabled`               | Let workspaces add their own provider API keys                |
| `copilot.byok.allowedProviders`      | Limit which providers can be used                             |
| `copilot.byok.allowCustomEndpoint`   | Allow custom API base URLs                                    |
| `copilot.byok.allowPrivateEndpoint`  | Allow custom URLs on private networks (off by default)        |

Provider keys are encrypted with the server private key, so keep
`NOTES_PRIVATE_KEY_SECRET` unchanged once keys are stored.

## Search

Full-text search works with PostgreSQL alone. `indexer.enabled` turns on
document indexing; vector search for the AI assistant also needs the
`pgvector` extension in the database.

## Calendar

| Key               | Description                                     |
| ----------------- | ----------------------------------------------- |
| `calendar.google` | Google Calendar OAuth client settings           |
| `calendar.caldav` | CalDAV provider presets shown to users          |

## Other settings

| Key                             | Description                                                      |
| ------------------------------- | ---------------------------------------------------------------- |
| `server.name`                   | Server name shown in the web app                                 |
| `flags.allowGuestDemoWorkspace` | Let visitors try a local workspace without signing in            |
| `websocket.maxHttpBufferSize`   | Largest real-time message, in bytes (default 100,000,000)        |
| `auth.trustedCloudflareHeaders` | Trust client IP headers set by Cloudflare                        |
