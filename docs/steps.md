# d-mock steps reference

Every step a scenario can use, with its arguments, what it returns, and an example. Print it
with `dmock help steps`.

## Scenario file

```yaml
name: provider times out once — checkout retries and charges exactly once
vars:                                # your own values; may use ${run} and other vars
  order: "ord-${run}"                # ${run} is unique per run (dm + 6 hex): put it in every id you create
  api: "http://localhost:18090"
steps:
  - name: what this step proves      # shown in the report; write it as a sentence
    http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
    expect: { status: 202 }          # checks on the step's result (see "expect")
    retry: { timeout: 20s, every: 1s }   # optional: re-run until expect passes or time is up
    save: { charge: json.charge_id }     # optional: keep a result value as ${saved.charge}
cleanup:                             # optional: always runs, even after a failure
  - http: { method: DELETE, url: "${vars.api}/orders/${vars.order}" }
```

- A step has exactly one action key (`http`, `sql`, …).
- When a step fails, the remaining steps are skipped and cleanup still runs.
- `--dry-run` expands each step without running it; a value a step would `save` shows as `<saved.name>`.

## References (`${...}`)

| Reference | Value |
|---|---|
| `${run}` | this run's id |
| `${vars.x}` | a scenario var |
| `${saved.x}` | a value saved by an earlier step |
| `${last.path}` | a path in the previous step's result |
| `${deps.<dep>.<var>}` | the dep's vars for its active mode (e.g. `${deps.db.dsn}`), plus `.mode` |
| `${mocks.<mock>.url}`, `.port` | where a mock listens |
| `${services.<svc>.config}`, `.log` | a service's rendered config file and log file |
| `${env.X}` | an environment variable |
| `${host}` | the host as seen from a container (`host.docker.internal` / `host.containers.internal`) |

An unresolved reference is an error; d-mock never fills in a default. `$${` is a literal `${`.
A string that is exactly one reference keeps the value's type (number, list, map).

## expect

Each key is a dotted path into the step's result; list indexes work (`calls.0.status`). A
plain value is compared as text; a map is one or more matchers:

```yaml
expect:
  status: 200                                  # equals
  json.status: { ne: WAITING }
  count: { gte: 1, lt: 4 }                     # gt gte lt lte compare numbers
  events: { contains: TimerCanceled, not_contains: ActivityTaskScheduled }
  body: { match: "^\\{" }                       # regex
  calls: { len: 2 }
  json.error: { exists: false }
```

A failure reads `json.status: got "WAITING", want "APPROVED"`.

## http

Call an API (your service, or a mock directly).

| Argument | |
|---|---|
| `url` | required |
| `method` | default `GET` |
| `headers` | map |
| `json` | a value sent as JSON (sets `Content-Type`) |
| `body` | raw body text (instead of `json`) |
| `timeout` | default `30s` |

**Result:** `status`, `body`, `json` (when the body is JSON), `headers`.

```yaml
- http: { method: POST, url: "${vars.api}/orders", json: { id: "${vars.order}", amount: 1500 } }
  expect: { status: 201, json.id: "${vars.order}" }
```

## sql

Read or write a `mysql` dep (needs the dep var `dsn`).

| Argument | |
|---|---|
| `dep` | the dep name |
| `query`, `args` | one statement; `?` placeholders filled from `args` |
| `insert: {table, key, row}` | insert one row; `row[key]` must contain the run id; the row is deleted after the run |

**Result:** reads give `rows` (list of column maps) and `count`; writes give `affected`; `insert` gives `affected` and `key`.

Rules: no DDL; one statement per step; on a `remote` dep every non-read statement must contain
the run id (in the SQL or args, not a comment).

```yaml
- sql:
    dep: db
    insert: { table: orders, key: id, row: { id: "${vars.order}", amount: 150000, status: NEW } }
- sql: { dep: db, query: "SELECT status FROM orders WHERE id = ?", args: ["${vars.order}"] }
  retry: { timeout: 20s, every: 1s }
  expect: { rows.0.status: PAID }
```

## mock

Change or inspect a mock at runtime. `name` plus exactly one of:

| Argument | Does |
|---|---|
| `set: {key, state: {field: value}}` | set state fields for one key (fields must be declared in the spec) |
| `fault: {key, fault, times}` | serve a declared fault to that key, `times` calls or (no `times`) until cleared |
| `clear: KEY` | clear a key's fault |
| `reset: true` | drop all state, faults and recorded calls |
| `calls: {route, key}` | recorded calls, filtered by route id and/or key (whole-token match) |

**Result** (`calls`): `count`, `calls[].{method, path, route_id, query, status, body, json, response, timestamp_ms}`, oldest first.
State and faults a scenario sets are reset after the run.

```yaml
- mock: { name: payments, fault: { key: "${vars.order}", fault: timeout, times: 1 } }
- mock: { name: payments, set: { key: "${vars.order}", state: { result: DECLINED } } }
- mock: { name: payments, calls: { route: charge, key: "${vars.order}" } }
  expect: { count: 2, calls.1.json.idempotency_key: "pay-${vars.order}" }
```

## log

Grep a service's or mock's log, only lines written since the scenario started.

| Argument | |
|---|---|
| `service` or `mock` | whose log |
| `grep` | a regex |

**Result:** `count`, `lines`.

```yaml
- log: { service: checkout, grep: "order=${vars.order} charge attempt 1 failed" }
  expect: { count: 1 }
```

## exec

Run a command from the suite folder.

| Argument | |
|---|---|
| `cmd` | a shell line, or an argv list |
| `dir`, `env`, `stdin`, `timeout` | optional |

**Result:** `exit`, `stdout`, `stderr`, `json` (when stdout is JSON).

```yaml
- exec: { cmd: "./seed.sh ${vars.order}" }
  expect: { exit: 0 }
```

## kafka

Produce one message (needs the dep var `brokers`, a list or comma-separated).

| Argument | |
|---|---|
| `dep`, `topic` | required |
| `key` | message key |
| `value` or `json` | the payload |
| `headers` | map |

**Result:** `topic`, `sent`.

```yaml
- kafka: { dep: kafka, topic: payment-events, key: "${vars.order}", json: { order_id: "${vars.order}", status: PAID } }
```

## temporal

Drive a Temporal workflow through the `temporal` CLI (needs dep vars `address`, `namespace`, optional `cli_args`).

| `action` | Arguments |
|---|---|
| `start` | `workflow_id`, `type`, `task_queue`, `input` (terminated after the run if still running) |
| `signal` | `workflow_id`, `signal`, `input` |
| `describe` | `workflow_id` |
| `history` | `workflow_id` |
| `terminate` | `workflow_id`, `reason` |

**Result:** `status` (RUNNING, COMPLETED, …), `events` (TimerCanceled, …), `json`.

```yaml
- temporal: { dep: temporal, action: describe, workflow_id: "approval-${vars.order}" }
  retry: { timeout: 30s }
  expect: { status: COMPLETED }
```

## service

Inspect a service (most useful for containers).

| Argument | |
|---|---|
| `name` | the service |

**Result:** `state` (running, exited, stopped), `oom_killed`, `exit_code`, `cpu_percent`,
`mem_bytes`, `mem_limit_bytes`, `mem_percent`.

```yaml
- service: { name: checkout }
  expect: { state: running, oom_killed: false, mem_percent: { lt: 80 } }
```

## k6

Run a k6 load script (path relative to the suite folder). Refused while any dep is `remote`
unless `--allow-remote-load`.

| Argument | |
|---|---|
| `script` | required |
| `vus`, `duration`, `iterations` | load shape |
| `env` | passed to the script as `__ENV` |
| `args` | extra k6 flags |
| `timeout` | for the whole run |

**Result:** `exit` (99 = a k6 threshold failed), `metrics.<name>.<stat>` (`p95`, `p99`, `avg`,
`max`, `rate`, `count`), `checks.<name>.passes/fails`, `services.<name>.state/oom_killed`
(container services after the load), and `warning` when one was OOM killed.

```yaml
- k6: { script: load/checkout.js, vus: 50, duration: 30s, env: { API: "http://localhost:18091", RUN: "${run}" } }
  expect:
    metrics.http_req_failed.rate: { lt: 0.01 }
    metrics.http_req_duration.p95: { lt: 500 }
    services.checkout.oom_killed: false
  save: { accepted: checks.accepted.passes }
```

## sleep

```yaml
- sleep: 2s
```

Prefer `retry` on a check over a fixed sleep; sleep only to prove that something does **not** happen.
