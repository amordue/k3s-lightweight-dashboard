# k3s Dashboard

Small in-cluster workload monitor for a personal k3s cluster. It collects pod CPU and memory usage from the Kubernetes Metrics API, falls back to kubelet summary stats when needed, resolves pods back to workloads, stores historical snapshots in SQLite, and serves a lightweight dashboard plus JSON endpoints from a single Go binary.

## What it does

- Polls `metrics.k8s.io` for pod CPU and memory usage on a fixed interval.
- Falls back to kubelet `/stats/summary` through the node proxy when the Metrics API is unavailable or empty.
- Resolves pod ownership into higher-level workloads such as Deployments, StatefulSets, DaemonSets, Jobs, and CronJobs.
- Persists snapshots in SQLite on a PVC for multi-day history.
- Exposes a browser UI for namespace, workload, and pod drill-down.
- Exposes a separate workload explorer page for multi-workload comparison over selectable time windows.
- Exposes JSON endpoints for summary and detail views.

## MVP scope

- Included: cluster summary, namespace view, workload view, pod view, SQLite retention, private dashboard, JSON API.
- Not included: auth, alerting, node dashboards, HA replicas, Prometheus integration.

## Architecture

1. The collector runs inside the same process as the HTTP server.
2. Every scrape stores one row per pod in SQLite.
3. The API builds current summaries from the most recent scrape timestamp.
4. Historical detail endpoints aggregate rows over a requested window.
5. The collector uses the Metrics API first and falls back to kubelet node proxy summary stats when required.
6. The embedded dashboard polls the API and renders current state plus simple history charts.

## Storage options

1. SQLite on a PVC
Recommended for your stated goal. One pod, one database file, no extra service.
2. In-memory only
Good for live views, but history disappears on restart.
3. No persistence
Smallest operational footprint, but only current usage is available.

## Configuration

Environment variables:

- `LISTEN_ADDR`: HTTP listen address. Default `:8080`.
- `SCRAPE_INTERVAL`: How often to collect metrics. Default `1m`.
- `RETENTION_PERIOD`: How long to keep history. Default `168h`.
- `SQLITE_PATH`: SQLite file path. Default `./run-data/metrics.db` for local runs. The k3s deployment overrides this to `/data/state/metrics.db`.
- `FAIL_IF_DATA_DIR_EXISTS`: When set to `true`, fail startup if the parent directory of `SQLITE_PATH` already exists. Default `false`.
- `KUBECONFIG`: Optional local kubeconfig for running outside the cluster.

The readiness payload also reports the active metrics source as either `metrics-api` or `kubelet-summary`.

## JSON endpoints

- `GET /api/v1/summary`
- `GET /api/v1/cluster-history?window=168h`
- `GET /api/v1/workloads`
- `POST /api/v1/workload-history`
- `GET /api/v1/namespaces/{namespace}?window=168h`
- `GET /api/v1/workloads/{namespace}/{kind}/{name}?window=168h`
- `GET /api/v1/pods/{namespace}/{name}?window=168h`
- `GET /healthz`
- `GET /readyz`

## Local development

1. Install Go.
2. From the repository root, run `go mod tidy`.
3. Start the server with a kubeconfig that can access your cluster.

Example:

```bash
export KUBECONFIG=$HOME/.kube/config
export SQLITE_PATH=./run-data/metrics.db
go mod tidy
go run ./cmd/server
```

Then open `http://localhost:8080`.

By default the application starts even if the parent directory of `SQLITE_PATH` already exists. If you want the stricter behavior, set `FAIL_IF_DATA_DIR_EXISTS=true`.

If your kubeconfig points at an API server hostname that does not match the certificate SANs, add `tls-server-name` to the cluster entry or use a temporary kubeconfig with that override. Example:

```yaml
clusters:
- cluster:
	server: https://master.kube.halogensunlight.co.uk:6443
	tls-server-name: didymus
```

This project uses the kubeconfig as provided by `client-go`, so local execution depends on the kubeconfig TLS settings being valid for the API server certificate.

## Container build

```bash
docker build -t ghcr.io/alexmordue/k3s-dashboard:latest .
```

## Deploy to k3s

1. Build and push the image.
2. Update the image reference in `deploy/deployment.yaml` or override it with kustomize.
3. Apply the manifests.

```bash
kubectl apply -k deploy
```

Optional ingress:

```bash
kubectl apply -f deploy/ingress.yaml
```

## Notes

- This implementation prefers `metrics-server`, but can fall back to kubelet summary stats if RBAC allows node proxy access.
- The readiness endpoint stays non-ready until the first successful scrape.
- The application allows an existing SQLite parent directory by default. Set `FAIL_IF_DATA_DIR_EXISTS=true` to force startup failure when that directory already exists.
- For a personal cluster, a 1 GiB PVC and 7-day retention is a reasonable default starting point.