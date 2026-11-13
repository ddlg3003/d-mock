# d-mock examples

Patterns for the questions d-mock is built to answer. Print it with `dmock help examples`.
Full runnable suites: `examples/checkout` and `examples/orders` in the d-mock repo.

## Set up a suite for your service

```bash
dmock init test/it                 # any folder; writes dmock.yaml, mocks/, configs/, scenarios/
dmock doctor test/it               # what tools this suite needs
```

Then edit `test/it/dmock.yaml`:

```yaml
project: checkout-it
deps:
  db:
    kind: mysql
    mode: container                # or remote (your shared dev DB): dmock --mode db=remote up
    container:
      image: mysql:8.0.36
      ports: ["13306:3306"]
      env: { MYSQL_ROOT_PASSWORD: root, MYSQL_DATABASE: shop }
      vars: { dsn: "root:root@tcp(localhost:13306)/shop" }
    remote:
      vars: { dsn: "${env.DEV_DB_DSN}" }
    ready: { tcp: "localhost:13306", timeout: 120s }
mocks:
  payments: { port: 14001 }        # spec: mocks/payments.yaml
services:
  checkout:                        # config: configs/checkout.tmpl.yaml
    dir: ../..                     # the repo root, relative to the suite folder
    cmd: ["go", "run", "./cmd/checkout"]
    env: { CHECKOUT_CONFIG: "${services.checkout.config}" }
    ready: { http: "http://localhost:18090/healthz", timeout: 120s }
    depends_on: [db, payments]
```

`configs/checkout.tmpl.yaml` is your normal config, with d-mock filling in addresses:

```yaml
listen: ":18090"
db_dsn: "${deps.db.dsn}"
payments_url: "${mocks.payments.url}"
```

## Fake a third party, keyed per entity

`mocks/payments.yaml`. The `key` lets each test entity get its own answer and its own failures:

```yaml
routes:
  - id: charge
    method: POST
    path: charges
    key: "bodyRaw 'order_id'"      # or: urlParam 'id' | queryParam 'ref' | header 'X-Ref'
    state: { result: SUCCEEDED }   # default answer
    headers: { Content-Type: application/json }
    body: '{"charge_id":"ch-${key}","status":"${state.result}"}'
    variants:
      - when: { result: GONE }
        status: 404
        body: '{"error":"no such charge"}'
    faults:
      timeout: { latency: 5s }     # longer than your client timeout
      http500: { status: 500, body: '{"error":"provider down"}' }
```

## Question: does a retry after a timeout charge twice?

`scenarios/01_provider_timeout.yaml`:

```yaml
name: provider times out once — checkout retries and charges exactly once
vars:
  order: "ord-${run}"
  api: "http://localhost:18090"
steps:
  - name: an order to check out
    sql: { dep: db, insert: { table: orders, key: id, row: { id: "${vars.order}", amount: 150000, status: NEW } } }
  - name: the provider's first answer comes too late
    mock: { name: payments, fault: { key: "${vars.order}", fault: timeout, times: 1 } }
  - name: check out
    http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
    expect: { status: 202 }
  - name: the order ends up PAID
    sql: { dep: db, query: "SELECT status FROM orders WHERE id = ?", args: ["${vars.order}"] }
    retry: { timeout: 20s, every: 1s }
    expect: { rows.0.status: PAID }
  - name: two charge calls, both with the same idempotency key
    mock: { name: payments, calls: { route: charge, key: "${vars.order}" } }
    expect: { count: 2, calls.0.json.idempotency_key: "pay-${vars.order}", calls.1.json.idempotency_key: "pay-${vars.order}" }
  - name: the timeout is in the log
    log: { service: checkout, grep: "order=${vars.order} charge attempt 1 failed" }
    expect: { count: 1 }
```

The DB alone would say "PAID" even with the double-charge bug; the recorded mock calls catch it.

## More questions, same shape

| Question | Change |
|---|---|
| Provider down for good: do we give up and mark FAILED? | `fault: http500` with no `times`; expect `rows.0.status: FAILED`, `count: 3` calls |
| Declined: do we stop retrying? | `mock: { set: { key, state: { result: DECLINED } } }`; expect `count: 1` call |
| Callback races the poll: which one wins? | `http` the callback endpoint while the mock answers the poll; expect the decision recorded once |
| Async event consumed? | `kafka` a message, then `sql` / `http` with `retry` until the effect shows |
| Workflow timer cancelled by a signal? | `temporal: { action: signal }`, then `temporal: { action: history }` expect `events: { contains: TimerCanceled }` |
| Nothing should happen | `sleep` past the deadline, then expect `count: 0` calls |

## Question: does it survive load inside its pod?

A nested suite, e.g. `test/it/pod-limits/`, runs the same service as a container with the
pod's limits (its own ports and `.dmock/`, so it can run next to the parent):

```yaml
# pod-limits/dmock.yaml (deps/mocks like the parent, on other ports)
services:
  checkout:                        # config: pod-limits/configs/checkout.tmpl.yaml (use ${host}, not localhost)
    container:
      build: { context: ../../.., dockerfile: Dockerfile }
      cpus: "0.5"                  # = resources.limits.cpu: 500m
      memory: 128m                 # = resources.limits.memory: 128Mi (no swap: over it = OOM kill)
      ports: ["18091:18090"]
      env: { CHECKOUT_CONFIG: /config/checkout.yaml }
      mounts: ["${services.checkout.config}:/config/checkout.yaml:ro"]
    ready: { http: "http://localhost:18091/healthz", timeout: 120s }
```

`pod-limits/scenarios/01_launch_day.yaml`, with a plain k6 script in `pod-limits/load/`:

```yaml
name: 50 shoppers for 30s inside 500m CPU / 128Mi
steps:
  - name: load
    k6: { script: load/checkout.js, vus: 50, duration: 30s, env: { API: "http://localhost:18091", RUN: "${run}" } }
    expect:
      exit: 0
      metrics.http_req_failed.rate: { lt: 0.01 }
      metrics.http_req_duration.p95: { lt: 500 }
      services.checkout.oom_killed: false
      checks.accepted.fails: 0
    save: { accepted: checks.accepted.passes }
  - name: the pod is still healthy
    service: { name: checkout }
    expect: { state: running, mem_percent: { lt: 80 } }
  - name: every accepted request really did its work (nothing lost in the background)
    sql: { dep: db, query: "SELECT COUNT(*) AS done FROM orders WHERE id LIKE ? AND status IN ('PAID','FAILED')", args: ["load-${run}-%"] }
    retry: { timeout: 15s, every: 1s }
    expect: { rows.0.done: "${saved.accepted}" }
```

A load run on a laptop is a rough check, not a benchmark. Run a failure again before trusting
it, then read `dmock logs` and `dmock mock calls`. Under load, look for unbounded goroutines,
undersized connection pools and missing client/driver timeouts.

## Run it

```bash
dmock test test/it --junit report.xml           # CI: up, every scenario, always down
dmock test test/it/pod-limits                     # the load suite
# while developing, keep the stack up:
dmock -f test/it up
dmock run test/it/scenarios/01_provider_timeout.yaml
dmock -f test/it up --restart checkout            # after a code change
dmock -f test/it down
```
