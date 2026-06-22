# ExternalDNS Porkbun Webhook

[![CI](https://github.com/internetliquid/external-dns-porkbun-webhook/actions/workflows/ci.yml/badge.svg)](https://github.com/internetliquid/external-dns-porkbun-webhook/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/internetliquid/external-dns-porkbun-webhook)](https://goreportcard.com/report/github.com/internetliquid/external-dns-porkbun-webhook)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) **webhook
provider** for [Porkbun](https://porkbun.com/) DNS. It lets ExternalDNS manage
records in your Porkbun-hosted zones by running as a small sidecar alongside
ExternalDNS and speaking the ExternalDNS webhook protocol over localhost.

> **Disclaimer:** This is an independent, community project. It is **not
> affiliated with, endorsed by, or supported by Porkbun LLC.** "Porkbun" is a
> trademark of Porkbun LLC.

## Why a webhook?

ExternalDNS no longer accepts new in-tree providers; the
[webhook provider](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/)
pattern is the supported way to add one. ExternalDNS talks to this sidecar over
HTTP on localhost, and the sidecar translates those calls into Porkbun API
requests via the [`nrdcg/porkbun`](https://github.com/nrdcg/porkbun) client.

```text
┌──────────────────────── Pod ────────────────────────┐
│  external-dns  ──HTTP──▶  webhook (localhost:8888)   │
│  (controller)             │                          │
│                           └──HTTPS──▶ Porkbun API     │
│                          health/metrics :8080         │
└──────────────────────────────────────────────────────┘
```

## Features

- Talks to the Porkbun v3 JSON API through the maintained
  [`nrdcg/porkbun`](https://github.com/nrdcg/porkbun) client.
- **Internal rate limiter** (default 3 req/s, burst 5) so a busy reconcile does
  not trip Porkbun's API limits; a `429`/`503` from Porkbun surfaces to
  ExternalDNS as a retryable error.
- **Per-zone record cache** with a configurable TTL, invalidated on writes, so
  reconciles don't re-list unchanged zones.
- Structured logging via `slog`; the API key and secret are **never logged**
  (and are scrubbed from transport errors).
- Distroless, non-root, multi-arch image (`linux/amd64` + `linux/arm64`).
- Health/readiness probes and Prometheus metrics on a separate port — including
  Porkbun API call counts/latency, rate-limiter wait time, and cache hit ratio.

## Quick start

1. Create the credentials secret (edit it first — it needs **both** the API key
   and the secret key from <https://porkbun.com/account/api>):

   ```sh
   kubectl create namespace external-dns
   kubectl apply -f deploy/secret.yaml
   ```

2. **Helm** (recommended) — as a sidecar of the official chart:

   ```sh
   helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
   helm upgrade --install external-dns external-dns/external-dns \
     -n external-dns -f deploy/helm-values.yaml
   ```

   Or **raw manifests**:

   ```sh
   kubectl apply -f deploy/manifests.yaml
   ```

See [`deploy/`](deploy/) for the full examples. `DOMAIN_FILTER` is **required** —
set it to the zones you want managed.

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
| --- | --- | --- |
| `PORKBUN_API_KEY` | — | **Required.** Porkbun API key. Treated as a secret. |
| `PORKBUN_API_SECRET` | — | **Required.** Porkbun secret API key. Treated as a secret. |
| `DOMAIN_FILTER` | — | **Required.** Comma-separated zones this webhook manages. There is no account-wide discovery mode. |
| `DRY_RUN` | `false` | Log intended changes without calling the Porkbun API. |
| `DEFAULT_TTL` | `3600` | TTL (seconds) applied to records whose TTL ExternalDNS leaves unset. |
| `PORKBUN_RATE_LIMIT` | `3` | Maximum Porkbun requests per second. |
| `PORKBUN_BURST` | `5` | Token-bucket burst size for the rate limiter. |
| `RECORD_CACHE_TTL` | `60s` | How long record lists are cached per zone (`0` disables). |
| `PORKBUN_TIMEOUT` | `30s` | Per-request timeout for Porkbun API calls. |
| `WEBHOOK_HOST` | `localhost` | Bind host for the ExternalDNS provider API. |
| `WEBHOOK_PORT` | `8888` | Bind port for the ExternalDNS provider API. |
| `METRICS_HOST` | `0.0.0.0` | Bind host for the health/metrics server. |
| `METRICS_PORT` | `8080` | Bind port for the health/metrics server. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `LOG_FORMAT` | `json` | `json` or `text`. |

The health/metrics server exposes `GET /healthz` (liveness), `GET /readyz`
(readiness), and `GET /metrics` (Prometheus).

### Metrics

Alongside the standard Go runtime and process collectors, `/metrics` exposes
webhook-specific series for operating against a rate-limited registrar:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `porkbun_webhook_api_requests_total` | counter | `operation`, `result` | Porkbun API calls by operation and `success`/`error`. |
| `porkbun_webhook_api_request_duration_seconds` | histogram | `operation` | API call latency. |
| `porkbun_webhook_ratelimit_wait_seconds` | histogram | — | Time spent blocked on the rate limiter before a call. |
| `porkbun_webhook_cache_hits_total` | counter | — | Record-list cache hits. |
| `porkbun_webhook_cache_misses_total` | counter | — | Record-list cache misses (lists that reached the API). |

## Supported record types

`A`, `AAAA`, `CNAME`, `MX`, and `TXT`. `TXT` is required because ExternalDNS
uses TXT records for its ownership registry. MX targets use ExternalDNS's
`<preference> <exchange>` format (e.g. `10 mail.example.com`).

## Known limitations

- **No bulk apply.** Every record change is an individual Porkbun API call.
- **`DOMAIN_FILTER` is required.** The Porkbun client exposes no list-domains
  endpoint and "manage every account zone" is never wanted, so the webhook
  manages exactly the zones you name. It will not start with an empty filter.
- **Minimum TTL.** Porkbun enforces a 600-second minimum TTL. Unlike some
  providers, this webhook does **not** silently clamp lower values — a sub-600
  TTL is sent as-is and Porkbun rejects it, surfacing the misconfiguration to
  you. Keep TTLs at `600` or above (the `3600` default is safe).
- **Rate limiting.** Porkbun publishes no fixed DNS rate limit and returns `429`
  / `503` under load. The default limiter (3 req/s, burst 5) is deliberately
  conservative; large changesets apply gradually and a reconcile that exceeds
  ExternalDNS's client deadline simply completes over subsequent reconciles
  (DNS reconciliation is idempotent).
- **Propagation delay.** Porkbun may take some time to propagate changes; they
  won't be visible to `Records` until they appear in a record list.

## Compatibility

Built against ExternalDNS **v0.21.0** and its webhook provider API
(`application/external.dns.webhook+json;version=1`). It works with ExternalDNS
versions that support the webhook provider (v0.14+). Go 1.26+.

## Dependencies and licenses

This project is Apache-2.0 licensed (matching ExternalDNS upstream). Its notable
dependencies and their licenses:

| Dependency | License | Notes |
| --- | --- | --- |
| [`github.com/nrdcg/porkbun`](https://github.com/nrdcg/porkbun) | MPL-2.0 | Imported as an unmodified library. MPL-2.0's file-level copyleft applies only to modifications of the MPL-licensed files themselves; importing it from separate Apache-2.0-licensed files does not affect this project's license. |
| [`sigs.k8s.io/external-dns`](https://github.com/kubernetes-sigs/external-dns) | Apache-2.0 | |
| [`github.com/prometheus/client_golang`](https://github.com/prometheus/client_golang) | Apache-2.0 | |
| [`github.com/sirupsen/logrus`](https://github.com/sirupsen/logrus) | MIT | Used internally by the ExternalDNS webhook helper. |

## Development

```sh
make build    # build ./bin/external-dns-porkbun-webhook
make test     # go test -race with coverage
make lint     # golangci-lint
make cover    # coverage summary
make docker-build
make help     # list all targets
```

## Contributing

Issues and pull requests are welcome. Please keep commits
[Conventional](https://www.conventionalcommits.org/), run `make test` and
`gofmt`, and note any new assumptions about Porkbun's API behaviour in the PR
description.

## License

[Apache 2.0](LICENSE).
