# cpa-fefo-router

**First to Expire, First Out** — a [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) scheduler plugin that spends subscription quota (Claude, Codex) in the order it would otherwise be wasted, while keeping each project on its own accounts.

Subscription quota comes in rolling windows (5-hour and weekly). Unused weekly quota is lost at the reset. CPA's built-in strategies (round-robin, fill-first) don't know about resets, and other reset-aware plugins rank every request against every account. This plugin instead:

- **pins** each client API key + session to one account and drains it, instead of spreading load;
- groups accounts into **buckets** and gives every client an ordered list of buckets it owns;
- lets clients borrow **spare** quota from other buckets — quota that the owning clients are not expected to need before it resets — and burns the spare that **expires first, first**;
- decides what happens when a client runs out: use **any** account, **wait** (hold, then 429 with `Retry-After`), or **fail**;
- **paces** a client (adds up to 30 s per request) when its last account would run out shortly before a refill, so the client's own loop stretches to the refill instead of hitting a wall;
- explains every decision: logs, a status API, Prometheus metrics, and a page inside CPA's management panel with runtime overrides and a config editor.

## How a request is routed

For each request CPA offers the plugin the credentials that can serve the model. The plugin identifies the client from CPA's *caller scope* (a hash of the client API key) and the session from CPA's session metadata (Claude Code / Codex session IDs, `X-Session-Id`, …).

1. **Pinned session.** If this (key, session) was routed before and that account is still in the client's plan, it stays there. Sessions never move mid-way because a better account appeared; they move only if their account becomes unusable (limit reached, CPA cooldown, disabled) or stops being lendable.
2. **Otherwise the client's plan**, in this order:
   1. `probe` — an own (or unassigned) account with no quota data yet gets one request to learn its windows.
   2. `spare-first` — spare quota (from another bucket, or from one of the client's own later buckets) whose weekly window **resets before** the client's first own account.
   3. `own` — the client's buckets in preference order; within a bucket, the account whose weekly window resets first.
   4. `spare` — remaining spare quota, soonest reset first.
   5. `last-resort` — only under the `any` policy: every other usable account, most spare first.
3. **Nothing left:** `any` hands the choice to CPA (fill-first); `fail` rejects with `quota_exhausted`; `wait` is handled before selection (see below).

An account is **blocked** while any window has less than `min_remaining` left (until that window's reset), while CPA has it in cooldown, when it is disabled, or when the provider reported its limit reached. Paid extra usage is never used unless `allow_extra_usage: true`.

### Spare

For every account the plugin estimates how much its **home clients** (clients whose buckets include it) will use before its weekly reset:

```
expected(account) = Σ over home clients of the demand that reaches this account
                    + usage seen on the account from outside CPA
spare(account)    = remaining(account) − spare_factor × expected(account)
```

A client's demand is its consumption in plan-weeks per hour — the **higher** of its 24-hour and 7-day averages — measured from the weekly utilization headers and split between clients by tokens. Demand "reaches" an account after the client's earlier accounts are used up (an earlier account that resets before this one counts as refilled). An account lends while `spare > min_spare` (and keeps sessions it already lent while `spare > 0`). With less than `min_history` of history nothing is lent.

`spare_factor` (default **1.5**: lend only what is more than 50 % above the expected need) can be changed at runtime from the UI or the API.

### Wait and pacing

A request interceptor runs before credential selection:

- **wait policy:** when a client has no own or spare account left and the next refill of one of its accounts is within `wait.horizon` (2 h), each request is held up to `wait.max_wait` (30 s) and, if still nothing is usable, answered `429` with `Retry-After` = time to refill. Beyond the horizon it answers 429 at once.
- **pacing:** when a client is down to its last account and, at the last hour's pace, that account's 5-hour or weekly window would empty before it refills within `pacing.horizon`, each request is delayed so the remaining quota lasts until the refill (at most `pacing.max_delay` per request).

## Observability

| Where | What |
| --- | --- |
| CPA management panel → plugin menu → **FEFO Router** (`/v0/resource/plugins/fefo-router/ui`) | Overview of accounts (windows, resets, expected use, spare, why), each client's live plan, traffic matrix (client → account → role), recent decisions with full ranking and exclusions, overrides, config editor, unknown keys. Uses the panel's stored management key, or asks for it. |
| `GET /v0/management/fefo/status` (`?format=text`, `?decisions=N`) | The same data as JSON or a plain-text table. |
| `GET /v0/management/fefo/metrics` | Prometheus: `fefo_account_*` (remaining, reset, expected, spare, usable), `fefo_client_demand_per_hour`, `fefo_picks_total{client,account,role,outcome}`, `fefo_tokens_total`, `fefo_requests_total`, `fefo_paced_*`, `fefo_wait_rejections_total`. |
| CPA log | One line per decision: `fefo selected client=laptop claude → me@work.example [own] bucket work, weekly 63% left, resets Thu 10:00Z`, plus pacing lines. |

All management routes require CPA's management key.

## Runtime changes

**Temporary overrides** (expire on their own, survive restarts):

```sh
# ci may use any account for the next 2 hours
curl -X POST -H "Authorization: Bearer $MGMT" -d '{"client":"ci","policy":"any","for":"2h","note":"deadline"}' \
  http://127.0.0.1:8317/v0/management/fefo/override
# lend more conservatively for an hour (all clients)
curl -X POST -H "Authorization: Bearer $MGMT" -d '{"spare_factor":2,"for":"1h"}' http://127.0.0.1:8317/v0/management/fefo/override
# clear
curl -X POST -H "Authorization: Bearer $MGMT" -d '{"client":"ci","clear":true}' http://127.0.0.1:8317/v0/management/fefo/override
```

**Runtime config.** The Config tab (or `POST /v0/management/fefo/config`) edits buckets, clients, policies and knobs. The result is validated, applied at once and stored in the state file; it **replaces** the routing part of `config.yaml` until you revert it (`{"revert":true}` or the button). `mode` and `state_file` always come from `config.yaml`. Editing `config.yaml` itself also applies without a restart (CPA hot-reloads plugin config), but while a runtime config is active it wins.

Unknown keys show up in the Config tab with an **Assign** button that adds their caller scope to a client.

## Install

Requirements: a plugin-capable CPA v8 build (not `_no-plugin`), tested with **v8.0.75**, Linux amd64 (Debian bookworm image `eceasy/cli-proxy-api`).

```sh
VERSION=0.1.0 scripts/build-linux.sh      # docker build → dist/fefo-router.so (glibc 2.36, GOAMD64=v1)
```

1. Copy `dist/fefo-router.so` into CPA's `plugins/` directory (mount it at `/CLIProxyAPI/plugins` in Docker).
2. Add the `fefo-router` section from [`config.example.yaml`](config.example.yaml) to `config.yaml`. Give it a writable `state_file` outside the auth directory.
3. Compute client keys' caller scopes: `scripts/client-scope.sh secrets/clients/*` (or paste raw `sha256(key)` hashes).
4. Only **one** scheduler plugin is used (the highest `priority`). Disable other scheduler plugins.
5. Restart CPA (shared libraries are loaded at start). Check the log for `plugin registered plugin_id=fefo-router` and open the FEFO Router page.

To try it without routing anything, start with `mode: observe`: it registers only the usage observer and the UI, so it collects history (needed before lending starts) while your current scheduler keeps routing. Switch to `active` and restart.

## Configuration reference

See [`config.example.yaml`](config.example.yaml); every key is optional.

| Key | Default | Meaning |
| --- | --- | --- |
| `mode` | `active` | `active` registers scheduler + interceptor; `observe` only records. Restart after changing. |
| `state_file` | — | Persists quota snapshots, 8 days of hourly usage, overrides and runtime config. |
| `spare_factor` | `1.5` | Lend while remaining > factor × expected home use. |
| `min_spare` | `0.03` | Plan-weeks of spare needed to start lending. |
| `min_history` | `6h` | History needed before demand is trusted (nothing is lent before). |
| `min_remaining` | `0.01` | Window share below which an account is blocked. |
| `session_ttl` | `1h` | Idle time after which a (key, session) pin is forgotten. |
| `probe_interval` | `5m` | Minimum gap between probe requests to an account without data. |
| `stale_after` | `30m` | Lifetime of limit flags that carry no reset time. |
| `allow_extra_usage` | `false` | Allow exhausted accounts that still serve from paid overage. |
| `buckets` | — | `name: [account, …]`; accounts by e-mail, auth file name or auth ID. |
| `capacity` | `1` each | Relative plan size per account. |
| `clients.<name>` | — | `keys` (caller scopes or sha256), `buckets` (ordered), `on_exhausted` (`any`/`wait`/`fail`), `borrow` (default true). |
| `default_client` | `any`, no buckets | Policy for keys not in `clients`. |
| `pacing` | on, `2h`, `30s` | See *Wait and pacing*. |
| `wait` | `2h`, `30s` | See *Wait and pacing*. |
| `log_decisions` | `true` | One log line per decision. |

## Limits

- Quota data comes from response headers (`anthropic-ratelimit-unified-*`, `x-codex-*`), so an account is only re-measured when CPA sends it traffic. Usage outside CPA appears as "outside CPA" demand once CPA sees the account again. Headers move in ~1 % steps, so demand is only meaningful over hours.
- Only Claude and Codex subscriptions report quota; other providers are routed by preference and the `any` fallback.
- CPA Home mode dispatch does not consult scheduler plugins.
- Pacing and waiting hold the request inside CPA; clients see a slower response, not an error.
- `mode` changes need a CPA restart because capabilities are declared at registration.

## Development

```sh
go test -race ./...
```

### CI and releases

- `ci` runs tests and builds the `.so` on every push to `main` and every PR, against the CPA version pinned in `go.mod` and against the latest CPA.
- `release` runs on a `v*` tag: it tests and builds against the **latest** CPA v8 release and attaches `fefo-router-<tag>-cpa-<cpa>-linux-amd64.so` (plus `.sha256`) to the GitHub release, creating it if needed. It can also be run by hand for an existing tag and a chosen CPA version.
- `cpa-watch` runs weekly (Monday 06:17 UTC). When a CPA version is out that a release published in the last 30 days has no build for, it builds that tag against it and adds the `.so` to the same release. Pick the file whose `cpa-` part matches your CPA.

`internal/router/router_test.go` encodes the routing rules as scenarios (bucket order, spare-first, no lending without history or when home clients need it, pinned sessions, policies and overrides, wait/429, pacing, runtime config, persistence).

## License

MIT — see [LICENSE](LICENSE). The C ABI glue, header parsers and host client are derived from [cliproxyapi-quota-router](https://github.com/OlivierDijkstra/cliproxyapi-quota-router) (MIT).
