# NSRL Server

A small Go service that serves the **modern and legacy** National Software Reference Library (NSRL) Reference Data Sets with HTTP range support. In Kubernetes, a dedicated init container downloads and atomically installs both archives on the shared persistent volume before the server starts.

## Run

```sh
docker run --rm -p 8080:8080 -v nsrl-data:/data ghcr.io/ozeliurs/nsrl-server:latest
```

The container image includes separate `nsrl-download` and `nsrl-server` executables. The standalone Docker command above starts only the server, so populate the volume first with `docker run --rm -v nsrl-data:/data --entrypoint /usr/local/bin/nsrl-download ghcr.io/ozeliurs/nsrl-server:latest`. The Helm chart automates this step with an init container.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check |
| `GET /readyz` | Readiness check; returns `200` only when both databases are available |
| `GET /v1/status` | Installed archive source, filename, size, and SHA-256 |
| `GET /docs` | Interactive Swagger UI |
| `GET /openapi.json` | OpenAPI 3.1 specification |
| `GET` or `HEAD /v1/nsrl/modern` | Download the current modern ZIP; `/v1/nsrl` is an alias |
| `GET` or `HEAD /v1/nsrl/legacy` | Download the current legacy ZIP |

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `NSRL_ADDR` | `:8080` | Listen address |
| `NSRL_DATA_DIR` | `/data` | Persistent archive directory |
| `NSRL_HTTP_TIMEOUT` | `6h` | Archive download request timeout |
| `NSRL_SOURCE_URL` | NIST `RDS_2026.03.1_modern_minimal.zip` URL | Modern minimal ZIP URL (override to use a mirror or another release) |
| `NSRL_LEGACY_SOURCE_URL` | NIST `RDS_2026.03.1_legacy_minimal.zip` URL | Legacy minimal ZIP URL (override to use a mirror or another release) |

The source and timeout variables are used by `nsrl-download`; the address is used by `nsrl-server`, and both use the data directory. An existing archive is reused when its persisted metadata matches the configured source. Delete the corresponding metadata file, or change its source URL, to force replacement.

## Local development

```sh
go test ./...
NSRL_DATA_DIR=./data go run ./cmd/nsrl-download
NSRL_DATA_DIR=./data go run ./cmd/nsrl-server
```

Images are built for AMD64 and ARM64 by GitHub Actions. Pull requests validate the image; pushes to `main` and version tags authenticate with `GITHUB_TOKEN` and publish to `ghcr.io/<owner>/<repository>`.

## Kubernetes / Helm

The included chart mounts one persistent volume in a download init container and the server container. Kubernetes does not start the server until `nsrl-download` has atomically installed both archives. If downloading fails, the init container exits non-zero and Kubernetes retries the pod initialization; the serving process never performs network downloads.

```sh
helm upgrade --install nsrl-server ./charts/nsrl-server \
  --namespace nsrl --create-namespace
```

The chart defaults to one replica, a `Recreate` deployment strategy, and a
100 GiB `ReadWriteOnce` claim. Because its default image tag is `latest`, it
always checks the registry for an updated image. Adjust `persistence.size` and
`persistence.storageClass` for the cluster. An existing claim can be supplied
with `persistence.existingClaim`.

Ingress is disabled by default. Enable it and configure the ingress class and
host for your cluster, for example:

```sh
helm upgrade --install nsrl-server ./charts/nsrl-server \
  --namespace nsrl --create-namespace \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.hosts[0].host=nsrl.example.com \
  --set ingress.hosts[0].paths[0].path=/ \
  --set ingress.hosts[0].paths[0].pathType=Prefix
```

Use `ingress.annotations` for controller-specific settings and `ingress.tls`
to configure TLS secrets and hosts.

NSRL data is provided by NIST; consult NIST's NSRL download page for its documentation and notices.
