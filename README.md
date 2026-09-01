# NSRL Server

A small Go service that discovers the newest **modern and legacy** National Software Reference Library (NSRL) Reference Data Sets in NIST's public bucket, downloads them atomically, and serves the original ZIPs with HTTP range support. The databases are stored on a persistent volume and refreshed every 24 hours by default.

## Run

```sh
docker run --rm -p 8080:8080 -v nsrl-data:/data ghcr.io/ozeliurs/nsrl-server:latest
```

The initial multi-gigabyte downloads happen sequentially in the background. Until each one completes, its download endpoint returns `503` while the liveness endpoint remains available. Kubernetes readiness requires both archives.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check |
| `GET /readyz` | Readiness check; returns `200` only when both databases are available |
| `GET /v1/status` | Current version, size, SHA-256, refresh state, and last error |
| `GET /docs` | Interactive Swagger UI |
| `GET /openapi.json` | OpenAPI 3.1 specification |
| `GET` or `HEAD /v1/nsrl/modern` | Download the current modern ZIP; `/v1/nsrl` is an alias |
| `GET` or `HEAD /v1/nsrl/legacy` | Download the current legacy ZIP |

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `NSRL_ADDR` | `:8080` | Listen address |
| `NSRL_DATA_DIR` | `/data` | Persistent archive directory |
| `NSRL_REFRESH_INTERVAL` | `24h` | How often to check NIST |
| `NSRL_RETRY_INTERVAL` | `5m` | Retry delay after a discovery or download failure |
| `NSRL_HTTP_TIMEOUT` | `6h` | Index/download request timeout |
| `NSRL_INDEX_URL` | NIST's `RDS/current/` bucket listing | Alternate S3-compatible XML listing |
| `NSRL_SOURCE_URL` | unset | Bypass discovery and download a fixed ZIP URL (useful for mirrors/testing) |
| `NSRL_LEGACY_SOURCE_URL` | unset | Bypass discovery for the legacy ZIP URL |

For fixed sources, checks happen on the configured interval and each archive is downloaded only once while its persisted metadata matches. Delete `modern-metadata.json` or `legacy-metadata.json`, or change the corresponding source URL, to force replacement.

## Local development

```sh
go test ./...
NSRL_DATA_DIR=./data NSRL_SOURCE_URL=https://example.test/RDS-modern.zip go run ./cmd/nsrl-server
```

Images are built for AMD64 and ARM64 by GitHub Actions. Pull requests validate the image; pushes to `main` and version tags authenticate with `GITHUB_TOKEN` and publish to `ghcr.io/<owner>/<repository>`.

## Kubernetes / Helm

The included chart uses a persistent volume and deliberately separates probes:
`/healthz` stays healthy while the initial database downloads, preventing a
restart loop, while `/readyz` keeps the pod out of Service endpoints until the
archive has been installed atomically.

```sh
helm upgrade --install nsrl-server ./charts/nsrl-server \
  --namespace nsrl --create-namespace
```

The chart defaults to one replica, a `Recreate` deployment strategy, and a
100 GiB `ReadWriteOnce` claim. Adjust `persistence.size` and
`persistence.storageClass` for the cluster. An existing claim can be supplied
with `persistence.existingClaim`.

NSRL data is provided by NIST; consult NIST's NSRL download page for its documentation and notices.
