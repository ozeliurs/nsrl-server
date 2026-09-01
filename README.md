# NSRL Server

A small Go service that discovers the newest **modern** National Software Reference Library (NSRL) Reference Data Set in NIST's public bucket, downloads it atomically, and serves the original ZIP with HTTP range support. The database is stored on a persistent volume and refreshed every 24 hours by default.

## Run

```sh
docker run --rm -p 8080:8080 -v nsrl-data:/data ghcr.io/OWNER/nsrl-server:latest
```

The initial multi-gigabyte download happens in the background. Until it completes, `/v1/nsrl` returns `503` while the health endpoint remains available.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check |
| `GET /v1/status` | Current version, size, SHA-256, refresh state, and last error |
| `GET` or `HEAD /v1/nsrl` | Download the current ZIP; supports ranges and conditional requests |

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `NSRL_ADDR` | `:8080` | Listen address |
| `NSRL_DATA_DIR` | `/data` | Persistent archive directory |
| `NSRL_REFRESH_INTERVAL` | `24h` | How often to check NIST |
| `NSRL_HTTP_TIMEOUT` | `6h` | Index/download request timeout |
| `NSRL_INDEX_URL` | NIST's `RDS/current/` bucket listing | Alternate S3-compatible XML listing |
| `NSRL_SOURCE_URL` | unset | Bypass discovery and download a fixed ZIP URL (useful for mirrors/testing) |

For a fixed source, checks happen on the configured interval and the archive is downloaded only once per process. Delete `metadata.json` or change the source URL to force replacement.

## Local development

```sh
go test ./...
NSRL_DATA_DIR=./data NSRL_SOURCE_URL=https://example.test/RDS-modern.zip go run ./cmd/nsrl-server
```

Images are built for AMD64 and ARM64 by GitHub Actions. Pull requests validate the image; pushes to `main` and version tags authenticate with `GITHUB_TOKEN` and publish to `ghcr.io/<owner>/<repository>`.

NSRL data is provided by NIST; consult NIST's NSRL download page for its documentation and notices.
