# cpa-fefo-router

**First to Expire, First Out** — a [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) scheduler plugin that spends subscription quota (Claude, Codex) in the order it would otherwise be wasted, while keeping each project on its own accounts.

Subscription quota comes in rolling windows (5-hour and weekly). Unused weekly quota is lost at the reset. CPA's built-in strategies (round-robin, fill-first) don't know about resets, and other reset-aware plugins rank every request against every account. This plugin instead:

- **pins** each client API key + session to one account and drains it, instead of spreading load;
- groups accounts into **buckets** and gives every client an ordered list of buckets it owns;
- measures quota in **API-equivalent dollars**: every request is priced at the model's public API rates, and each account's weekly capacity is learned in dollars;
- lets clients borrow **spare** quota from other buckets — quota that the owning clients are not expected to need before it resets — and burns the spare that **expires first, first**;
- keeps **Fable** (and other separately limited model families) off accounts where it would be billed as paid extra usage;
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

For a request whose model family has its own allowance (Fable), an account is also **excluded for that family only** when its allowance is estimated below `min_remaining`, or when its plan has no allowance for the family and extra usage is enabled. This applies to pinned sessions too; see [Fable protection](#fable-protection).

### Spare

For every account the plugin estimates how much its **home clients** (clients whose buckets include it) will use before its weekly reset:

```
remaining(account) = weekly share left × capacity_usd(account)
expected(account)  = Σ over home clients of the demand that reaches this account
                     + usage seen on the account from outside CPA,
                     at most remaining(account)
spare(account)     = remaining(account) − spare_factor × expected(account)
```

All of these are API-equivalent dollars (see [Dollars](#dollars-capacity-learning-and-fable-protection)). A client's demand is its consumption in dollars per hour — the **higher** of its 24-hour and 7-day averages — billed at what its requests cost on every account it used, including borrowed accounts, paid extra usage and API-key accounts that the weekly window does not show (hours recorded before requests were priced are left out, and the averages start after the last of them). Demand "reaches" an account after the client's earlier accounts are used up (an earlier account that resets before this one counts as refilled). Demand spent on borrowed accounts still counts: lending can stop at any time, so the client's own accounts are planned as if they had to carry all of it. Need beyond what an account has left is shown in its explanation, not in `expected`. An account lends while `spare > min_spare × capacity_usd` (and keeps sessions it already lent while `spare > 0`). With less than `min_history` of history nothing is lent.

`spare_factor` (default **1.5**: lend only what is more than 50 % above the expected need) can be changed at runtime from the UI or the API.

### Wait and pacing

A request interceptor runs before credential selection:

- **wait policy:** when a client has no own or spare account left and the next refill of one of its accounts is within `wait.horizon` (2 h), each request is held up to `wait.max_wait` (30 s) and, if still nothing is usable, answered `429` with `Retry-After` = time to refill. Beyond the horizon it answers 429 at once.
- **pacing:** when a client is down to its last account and, at the last hour's pace, that account's 5-hour or weekly window would empty before it refills within `pacing.horizon`, each request is delayed so the remaining quota lasts until the refill (at most `pacing.max_delay` per request).

## Dollars, capacity learning and Fable protection

### Dollars

Raw token counts are dominated by cache reads, which cost 1/20 of uncached input on Claude Opus, so they misjudge how much quota a client uses. Every request is priced instead:

```
usd = uncached_input × in + output × out + cache_read × cr + cache_write × cw   (per 1M tokens, the model's API price)
```

Each hour's weekly-utilization increase on an account is split between clients by these dollars. Hours recorded before pricing existed fall back to tokens, then requests.

Prices come from, highest first: `prices:` in the config, the last [models.dev](https://models.dev) sync (daily, `pricing.sync_interval`, stored in the state file), and a snapshot compiled into the plugin (`scripts/update-prices.sh`). A failed sync keeps the last table and shows in status. A model missing from the table (a new release) is priced as the cheapest-to-dearest range of its family, never at zero: a home client of the account is charged the high end and a borrower the low end, so the uncertainty never makes an account lend more. Status warns about such models with their request count and the range applied.

### Capacity learning

An account's **capacity** is the dollars one full weekly window holds. The plugin learns it per account from CPA's own traffic: priced dollars ÷ the weekly share they consumed. It re-learns every weekly cycle, so plan upgrades and downgrades show up within a week:

- the current cycle's estimate is used once it has seen `learn_min_delta` (5 %) of the week;
- until then, the previous cycle's estimate;
- until then, a prior: `capacity` (relative, Pro = 1) × the median learned dollars per unit over the provider's accounts, or `default_unit_usd` ($220, a measured Pro week) when none has learned yet.

Usage from outside CPA, hours with unpriced models and hours in which the weekly window reset are left out. `capacity_usd` pins an account's capacity instead. Status shows each account's capacity, where it came from (`learned this cycle`, `learned last cycle`, `prior`, `pinned`) and the numbers behind it.

### Fable protection

Anthropic limits some model families (today Fable) with a separate weekly allowance that is a share of the weekly window, and bills them as paid **extra usage** when the plan has no allowance (Pro) or once it is used up. Response headers do not carry these limits, so the refresher polls each Claude OAuth account's usage report (`GET https://api.anthropic.com/api/oauth/usage`) every `usage_poll_interval` (5m, each account's next poll jittered by ±30 % so they do not arrive together) through CPA (`host.auth.get` for the access token, `host.http.do` for the request). The token is only put in that request's header: it is never logged, stored or shown, and errors are scrubbed of it.

For a request of such a family (the model's family from the price table, so a new `claude-fable-6` counts):

- **the account has an allowance for it**: its remaining share is estimated as the polled value minus the dollars spent on the family since the poll ÷ (allowance × capacity). Below `min_remaining` the account is excluded for that family;
- **the report shows no allowance** for a family in `scoped_families` (default `[fable]`): the account is excluded for it while extra usage is enabled or its state is unknown;
- **not polled yet**: allowed.

Other models on the same account are not affected. Excluded accounts are dropped from the plan, pinned sessions move, and they are never handed to CPA's own pick. Among the accounts left, Fable requests prefer the allowance that resets first, within each role of the plan. Decisions and log lines list the accounts excluded for the family and why. `allow_extra_usage: true` lifts this protection, as it does for exhausted shared windows.

## Observability

| Where | What |
| --- | --- |
| CPA management panel → plugin menu → **FEFO Router** (`/v0/resource/plugins/fefo-router/ui`) | Overview of accounts (windows, resets, expected use, spare, why), each client's live plan, traffic matrix (client → account → role), recent decisions with full ranking and exclusions, overrides, config editor, unknown keys. Uses the panel's stored management key, or asks for it. |
| `GET /v0/management/fefo/status` (`?format=text`, `?units=usd\|tokens`, `?decisions=N`) | The same data as JSON (dollars and tokens) or a plain-text table in the chosen units. The UI has the same toggle. |
| `GET /v0/management/fefo/metrics` | Prometheus, below. |
| CPA log | One line per decision: `fefo selected client=laptop claude → me@work.example [own] bucket work, weekly 63% left, resets Thu 10:00Z`, plus pacing lines. |

All management routes require CPA's management key.

Metrics (dollars are API-equivalent USD):

| Metric | Meaning |
| --- | --- |
| `fefo_account_weekly_remaining_ratio`, `fefo_account_short_remaining_ratio`, `fefo_account_weekly_reset_seconds`, `fefo_account_usable{reason}` | Window state per account. |
| `fefo_account_capacity_usd{source}` | Weekly capacity and where it came from (`learned this cycle`, `learned last cycle`, `prior`, `pinned`). |
| `fefo_account_remaining_usd`, `fefo_account_expected_usd`, `fefo_account_spare_usd` | Remaining, expected home use and spare. These replace `fefo_account_expected_home_usage` and `fefo_account_spare`, which were in plan-weeks. |
| `fefo_account_scoped_remaining{window}` | Estimated share left of a family's own allowance (`window="7d:fable"`). |
| `fefo_client_demand_per_hour{window,unit}` | Client demand, `unit="usd"` or `unit="tokens"`, 24h and 7d averages. |
| `fefo_cost_usd_total`, `fefo_tokens_total`, `fefo_requests_total` | Traffic per client and account. |
| `fefo_unknown_model_requests_total{model}` | Requests priced as a family range because the model is missing from the price table. |
| `fefo_prices_synced_timestamp` | Last successful models.dev sync (0: never). |
| `fefo_picks_total{client,account,role,outcome}`, `fefo_paced_*`, `fefo_wait_rejections_total` | Decisions, pacing and waits. |

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

**Making it permanent.** *Make permanent in config.yaml* writes the runtime config into the plugin's `config.yaml` section through CPA's own plugin config API (`PUT /v0/management/plugins/fefo-router/config`), so CPA validates it and keeps the rest of the file. `enabled`, `priority`, `mode`, `state_file` and `refresh_interval` stay as they were. The replaced section is kept as a backup in the state file (the last 20), and the runtime config ends. Backups are listed under *Advanced → config.yaml backups*; restoring one backs up the current section first. If `config.yaml` is generated from a template, update the template too, or the next render undoes the change.

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
| `min_spare` | `0.03` | Spare needed to start lending, as a share of the account's weekly capacity. |
| `min_history` | `6h` | History needed before demand is trusted (nothing is lent before). |
| `min_remaining` | `0.01` | Window share below which an account is blocked. |
| `session_ttl` | `1h` | Idle time after which a (key, session) pin is forgotten. |
| `probe_interval` | `5m` | Minimum gap between probe requests to an account without data. |
| `stale_after` | `30m` | Lifetime of limit flags that carry no reset time. |
| `allow_extra_usage` | `false` | Allow exhausted accounts that still serve from paid overage, and lift the Fable protection. |
| `scoped_families` | `[fable]` | Model families with their own allowance, kept off plans without one while extra usage is on. |
| `usage_poll_interval` | `5m` | How often each Claude account's usage report is fetched, ±30 % jitter per poll (checked on each `refresh_interval` tick); `0` disables (and with it the no-allowance check). |
| `buckets` | — | `name: [account, …]`; accounts by e-mail, auth file name or auth ID. |
| `capacity` | `1` each | Relative plan size per account (Pro = 1): the capacity prior until dollars are learned. |
| `capacity_usd` | learned | Pin an account's weekly capacity in API-equivalent dollars. |
| `default_unit_usd` | `220` | Dollars per unit of `capacity` before any account of the provider has learned its capacity. |
| `learn_min_delta` | `0.05` | Share of the week a cycle must have seen before its capacity estimate is used. |
| `pricing` | sync on, `24h`, models.dev | `sync`, `sync_interval` (≥ 1h), `url` of the models.dev API. |
| `prices` | — | `model: {input, output, cache_read, cache_write}` in dollars per 1M tokens; wins over every other source. |
| `display_units` | `usd` | Units of the text status and the UI by default: `usd` or `tokens`. |
| `clients.<name>` | — | `keys` (caller scopes or sha256), `buckets` (ordered), `on_exhausted` (`any`/`wait`/`fail`), `borrow` (default true). |
| `default_client` | `any`, no buckets | Policy for keys not in `clients`. |
| `pacing` | on, `2h`, `30s` | See *Wait and pacing*. |
| `wait` | `2h`, `30s` | See *Wait and pacing*. |
| `log_decisions` | `true` | One log line per decision. |

## Limits

- Quota data comes from response headers (`anthropic-ratelimit-unified-*`, `x-codex-*`) and, for Claude OAuth accounts, the usage report polled every `usage_poll_interval`. Usage outside CPA appears as "outside CPA" demand once CPA sees the account again. Headers move in ~1 % steps, so demand and capacity are only meaningful over hours.
- Capacity is learned only from traffic through CPA; an account CPA barely uses keeps its prior.
- Between polls a family's allowance is estimated from API prices; the next poll corrects it.
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

`internal/router/router_test.go` encodes the routing rules as scenarios (bucket order, spare-first, no lending without history or when home clients need it, pinned sessions, policies and overrides, wait/429, pacing, runtime config, persistence). `usd_test.go`, `poll_test.go` and `fable_test.go` cover dollar accounting, capacity learning, price sync, the usage poller (including that the token never leaks) and Fable protection.

## License

MIT — see [LICENSE](LICENSE). The C ABI glue, header parsers and host client are derived from [cliproxyapi-quota-router](https://github.com/OlivierDijkstra/cliproxyapi-quota-router) (MIT).
