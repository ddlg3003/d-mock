# Example: checkout and a payment provider

A runnable example of how you'd use d-mock on your own service. It answers two questions
every team that talks to a third party has to answer:

1. **"If the provider times out, do we charge the customer twice?"** (behaviour under faults)
2. **"Will it survive launch day inside its pod?"** (load inside CPU/memory limits)

## The service under test

`service/` is a small Go service standing in for **your** code. In your repo, you point
d-mock at your own service and leave its code as it is.

```
            POST /orders {id, amount}         ┌──────────┐
 client ──▶ POST /checkout {order_id} ──202──▶│ checkout │──▶ MySQL (orders: NEW → PAID / FAILED)
                                              └────┬─────┘
                       in the background:          │ POST /charges {order_id, amount, idempotency_key}
                       retry timeouts and 5xx      ▼
                       (same idempotency key),   payment provider
                       stop on a decline          (here: a d-mock mock)
```

**Without d-mock**, testing "what if the provider times out?" means: start MySQL and the
service, find a way to make a sandbox provider slow (usually you can't), curl the API, read
logs, query the DB by hand, delete the test rows, and redo all of it after every change.

**With d-mock**, you describe the stack once. Each question becomes a scenario file that
runs in seconds and cleans up after itself.

## What's in this folder

| File | What it is | In your repo |
|---|---|---|
| `stack.yaml` | what to start: MySQL, the fake provider, the service | you write once |
| `config.tmpl.yaml` | the service's normal config, with addresses filled in by d-mock | your config file, with `${...}` where addresses go |
| `mocks/payments.yaml` | the fake payment provider: routes, default answer, faults | one per third party |
| `scenarios/*.yaml` | use case 1: one question per file | grows with every bug you don't want back |
| `stack.load.yaml`, `config.container.tmpl.yaml`, `Dockerfile`, `mocks/payments-slow.yaml` | use case 2: the same pieces, with the service in a container | your Dockerfile already exists |
| `load/checkout.js`, `scenarios-load/*.yaml` | use case 2: a k6 script and its checks | |
| `service/` | the toy service | your service, not copied |

Needs: Go, `mockoon-cli`, docker or podman, and k6 for use case 2. `dmock doctor` checks
them all.

---

## Use case 1: "If the provider times out, do we charge twice?"

The first charge request times out, but the provider may have charged anyway. A correct
service retries with the **same** idempotency key, so the provider charges once.

### The fake provider: [`mocks/payments.yaml`](mocks/payments.yaml)

```yaml
routes:
  - id: charge
    method: POST
    path: charges
    key: "bodyRaw 'order_id'"             # state and faults are set per order
    state: { result: SUCCEEDED }          # default answer
    headers: { Content-Type: application/json }
    body: '{"charge_id":"ch-${key}","status":"${state.result}"}'
    faults:
      timeout: { latency: 5s }            # longer than checkout's 2s client timeout
      http500: { status: 500, body: '{"error":"provider down"}' }
```

The `key` is what lets every test order have its own behaviour. One test can make order A
time out while order B, in another test running at the same time, gets declined.

### The question as a scenario: [`scenarios/01_provider_timeout.yaml`](scenarios/01_provider_timeout.yaml)

```yaml
name: provider times out once — checkout retries and charges exactly once
vars:
  order: "ord-${run}"              # unique per run: runs never collide, even on a shared DB
  api: "http://localhost:18090"

steps:
  - name: an order to check out
    sql:
      dep: db
      insert: { table: orders, key: id, row: { id: "${vars.order}", amount: 150000, status: NEW } }

  - name: the provider's first answer comes too late
    mock: { name: payments, fault: { key: "${vars.order}", fault: timeout, times: 1 } }

  - name: check out
    http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
    expect: { status: 202 }

  - name: the order ends up PAID
    sql: { dep: db, query: "SELECT status FROM orders WHERE id = ?", args: ["${vars.order}"] }
    retry: { timeout: 20s, every: 1s }   # checkout charges in the background: poll until done
    expect: { rows.0.status: PAID }

  - name: two charge calls, both with the same idempotency key
    mock: { name: payments, calls: { route: charge, key: "${vars.order}" } }
    expect:
      count: 2
      calls.0.json.idempotency_key: "pay-${vars.order}"
      calls.1.json.idempotency_key: "pay-${vars.order}"

  - name: the timeout is in the log for on-call
    log: { service: checkout, grep: "order=${vars.order} charge attempt 1 failed" }
    expect: { count: 1 }
```

Each step is something you'd otherwise do by hand: seed a row, break the provider, call the
API, wait and look at the DB, check what the provider received, grep the log.

### Run it

```bash
cd examples/checkout
dmock up                 # MySQL in a container, the fake provider, the service; waits until ready
dmock run scenarios/
dmock down
```

```
▶ provider times out once — checkout retries and charges exactly once (scenarios/01_provider_timeout.yaml)
  ✓ an order to check out [sql] 143ms
  ✓ the provider's first answer comes too late [mock] 8ms
  ✓ check out [http] 4ms
  ✓ the order ends up PAID [sql] 3.062s (4 attempts)
  ✓ two charge calls, both with the same idempotency key [mock] 7ms
  ✓ the timeout is in the log for on-call [log] 11ms
  ✓ auto: clear mock payments fault for ord-dm686210 [cleanup]
  ✓ auto: delete orders.id=ord-dm686210 [cleanup]
PASS provider times out once — checkout retries and charges exactly once run=dm686210 3.471s
```

### What a bug looks like

Change `charge()` in `service/main.go` to build a new key on each attempt
(`fmt.Sprintf("pay-%s-%d", id, attempt)`), then `dmock up --restart checkout` and run again:

```
  ✓ the order ends up PAID [sql] 3.208s (4 attempts)
  ✗ two charge calls, both with the same idempotency key [mock] 3ms
      expect: calls.0.json.idempotency_key: got "pay-ord-dm6fe34f-1", want "pay-ord-dm6fe34f"
      expect: calls.1.json.idempotency_key: got "pay-ord-dm6fe34f-2", want "pay-ord-dm6fe34f"
FAIL provider times out once — checkout retries and charges exactly once run=dm6fe34f 3.271s
```

The order still ends up `PAID`, so a test that only checks the DB would pass. Only the
recorded provider calls show that the customer was charged twice. Either way, the test row
is deleted and the fault is cleared.

### The next questions are copies with one or two lines changed

| Scenario | Changed from 01 | Checks |
|---|---|---|
| [`02_provider_down.yaml`](scenarios/02_provider_down.yaml) | `fault: http500` with no `times` (until cleared) | gives up after exactly 3 attempts, order `FAILED` |
| [`03_card_declined.yaml`](scenarios/03_card_declined.yaml) | `set: { state: { result: DECLINED } }` instead of a fault | charged once and **not** retried, order `FAILED` |

You can also break things by hand while developing, with no scenario:

```bash
dmock mock fault payments ord-123 timeout --times 1   # next charge for ord-123 times out
dmock mock set payments ord-456 result=DECLINED       # ord-456 gets declined
dmock mock calls payments --key ord-123               # what did the service send?
dmock logs checkout --tail 20
```

**Against your shared dev database:** `dmock --mode db=remote up` with `DEV_DB_DSN` set
in your shell. The `${run}` id in every row d-mock writes, and the automatic cleanup, keep
your test data away from your teammates' data.

---

## Use case 2: "Will it survive launch day inside its pod?"

Production runs checkout with `cpu: 500m, memory: 128Mi`, and the provider takes about
300 ms per charge. The question: with 50 shoppers at once, do requests stay fast, does the
pod stay up, and is every accepted checkout really charged?

This is a rough check, not a benchmark: your laptop isn't a production node. It catches
the big problems early: a memory leak, a goroutine pile-up, a pool that is too small, work
lost in the background.

### What changes from use case 1: [`stack.load.yaml`](stack.load.yaml)

```yaml
services:
  checkout:
    config: { template: config.container.tmpl.yaml }
    container:
      build: { ... }           # your Dockerfile
      cpus: "0.5"              # = resources.limits.cpu: 500m
      memory: 128m             # = resources.limits.memory: 128Mi (no swap: over it = OOM kill)
      ports: ["18091:18090"]
```

The provider mock gets `latency: 300ms` ([`mocks/payments-slow.yaml`](mocks/payments-slow.yaml)),
and the DB is always a local container, because load never goes to a shared server.

### The load: [`load/checkout.js`](load/checkout.js) (plain k6) and its checks

```yaml
name: launch day — 50 shoppers for 30s, inside 500m CPU / 128Mi, provider at 300 ms
steps:
  - name: 50 virtual users for 30s
    k6:
      script: load/checkout.js
      vus: 50
      duration: 30s
      env: { API: "http://localhost:18091", RUN: "${run}" }
    expect:
      exit: 0
      metrics.http_req_failed.rate: { lt: 0.01 }       # under 1% errors
      metrics.http_req_duration.p95: { lt: 500 }       # 95% of requests under 500 ms
      services.checkout.oom_killed: false
      checks.accepted.fails: 0
    save: { accepted: checks.accepted.passes }        # → ${saved.accepted}

  - name: the pod is still healthy, with memory headroom left
    service: { name: checkout }
    expect: { state: running, oom_killed: false, mem_percent: { lt: 80 } }

  - name: every accepted checkout got charged (none lost in the background)
    sql: { dep: db, query: "SELECT COUNT(*) AS charged FROM orders WHERE id LIKE ? AND status IN ('PAID', 'FAILED')", args: ["load-${run}-%"] }
    retry: { timeout: 15s, every: 1s }
    expect: { rows.0.charged: "${saved.accepted}" }
```

The last step is the one load tools alone can't do: k6 sees fast `202`s, but only the
database knows whether the background charge really happened for each of them.

### Run it

`dmock down` the use case 1 stack first: both stack files share this folder's `.dmock/`,
and d-mock refuses to mix them.

```bash
dmock -f stack.load.yaml up
dmock -f stack.load.yaml run scenarios-load/
dmock -f stack.load.yaml down
```

```
▶ launch day — 50 shoppers for 30s, inside 500m CPU / 128Mi, provider at 300 ms (scenarios-load/01_launch_day.yaml)
  ✓ 50 virtual users for 30s [k6] 31.071s
  ✓ the pod is still healthy, with memory headroom left [service] 143ms
  ✓ every accepted checkout got charged (none lost in the background) [sql] 28ms
PASS launch day — 50 shoppers for 30s, inside 500m CPU / 128Mi, provider at 300 ms run=dm6cbec2 31.223s
```

### What this example caught while it was being written

The first version of `service/` failed this scenario about one run in three:

```
  ✓ 50 virtual users for 30s [k6] 1m1.523s
  ✓ the pod is still healthy, with memory headroom left [service] 224ms
  ✗ every accepted checkout got charged (none lost in the background) [sql] 14.307s (15 attempts)
      expect: rows.0.charged: got "2432", want "2441"
```

k6 was happy: under 1% errors and a fine p95. But 9 customers got a `202` and were never
charged. `dmock -f stack.load.yaml logs checkout` had no line at all for those orders, so
their background goroutines were stuck. Adding driver timeouts to the DSN turned the hang
into an error:

```
order=load-dm8a8d94-21-54 set status PAID: dial tcp 192.168.127.254:13307: i/o timeout
```

The cause: Go's `database/sql` keeps only 2 idle connections by default, so with 50 users
almost every query opened a new MySQL connection, and a few of those thousands of dials
hung with no timeout. Two lines in `main()` (`SetMaxOpenConns(20)`, `SetMaxIdleConns(20)`)
fixed it: 3 of 3 runs passed, with k6 back to 31s. That's the kind of bug that only shows
up under load, in the background, where a load tool alone can't see it.

**Running out of memory** looks like what Kubernetes does to a pod: the `k6` step reports
`services.checkout.oom_killed: true` with a warning, and `status` shows `OOM-KILLED exit=137`.
This toy is too lean to hit 128Mi. See
[`examples/orders/scenarios-containers/02_oom_kill.yaml`](../orders/scenarios-containers/02_oom_kill.yaml)
for a real OOM kill, checked as a scenario.

### Things to try

- **Load while the provider misbehaves.** This is when goroutines and connections pile up.
  Raise `latency` in `payments-slow.yaml` to `1500ms`: does checkout stay within 128Mi and
  under the p95?
- **Find the limit.** Raise `vus` until a check fails. That's your pod's rough ceiling at
  this CPU and memory, before any autoscaling.
- **Right-size the pod.** Lower `memory` and watch `mem_percent` in the second step's
  result (`--verbose`), then keep the headroom you're comfortable with.

A failed load run on a laptop can also be the laptop: the container VM stalls, or another
program uses the CPU. Before you trust a failure, run it again, then dig in with
`dmock logs` and `dmock mock calls`.

---

## Using this in your own repo

1. Copy this folder's layout into your repo (for example `test/dmock/`).
2. `stack.yaml`: point `services.<name>.cmd` (or `container.build`) at your service, and
   list its real dependencies.
3. One mock spec per third party, keyed by whatever identifies an entity in its requests.
4. One scenario per question. Start with the incident you never want to see again.

See the [main README](../../README.md) for every option, and `dmock help guide` for the
mistakes that look like bugs.
