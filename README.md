# d-mock

**Run integration tests against a real stack on your own machine or in CI, from one suite folder and one CLI.**

d-mock starts what your services need: databases, brokers, mocked external APIs, and the services themselves. It then runs YAML test scenarios against that stack and cleans up after each run. It's built for the tests that are hard to write as unit tests: callbacks racing polls, retries, timeouts, third-party outages, and workflows that span several services.

```
dmock.yaml    ──dmock up──▶   deps              mocks                services
                              (remote or        (HTTP APIs via       (the code under
                               container)        Mockoon)             test, as processes)
                                  ▲                  ▲                    ▲
scenario.yaml ──dmock run──▶  sql · kafka ·     mock set · fault ·   http · exec · log
                              temporal          calls
```

## Features

**Stack**
- **A suite is a folder, like a Helm chart.** Any folder with a `dmock.yaml` is a suite: the stack in `dmock.yaml`, mocks in `mocks/`, config templates in `configs/`, scenarios in `scenarios/`. Put it anywhere, in any repo; `dmock init` writes a starter one.
- **The path picks what runs.** `dmock run it/` runs the whole suite, `dmock run it/scenarios/payments/` one folder, `dmock run it/scenarios/payments/01_timeout.yaml` one file. d-mock walks up from the path to the nearest `dmock.yaml`, the way git finds `.git`.
- **Built for CI.** `dmock test it/ --junit report.xml` brings the stack up, runs every scenario, always takes it down, and writes a JUnit report that GitLab, GitHub and Jenkins display.
- **Choose per dependency: real or local.** Each dependency either points at an existing server (`remote`) or runs in a container. Switch for one run with `--mode db=container`.
- **Generated service configs.** Config files are templates (`${deps.db.dsn}`, `${mocks.payments.url}`, `${env.TOKEN}`). A missing value is an error, never a silent default.
- **Services as processes or containers.** Run the code under test as a local process, or build and run it as a container with k8s-like limits (`cpus: "0.5"`, `memory: 256m`). Hitting the memory limit kills the container with exit 137 (OOM), the same as a pod.
- **Safe to repeat.** `up` starts only what is down and restarts a mock when its spec changed. `down` never touches remote dependencies.

**Mocks** (on [Mockoon](https://mockoon.com))
- **Short YAML instead of Mockoon JSON.** Routes, responses and variants compile to a Mockoon environment.
- **Per-request state.** Each request is keyed by a URL param, body field or header, so every test entity gets its own answer: `dmock mock set payments order-1 status=APPROVED`.
- **Fault injection.** HTTP 500s, timeouts or any custom response, for N calls or until cleared: `dmock mock fault payments order-1 http500 --times 3`.
- **Call recording.** Assert exactly what your service sent, how often and in what order.
- **Raw and encoded bodies.** Values are never HTML-escaped. Pre-encrypted or signed fixtures are served byte for byte.

**Scenarios**
- **One YAML format for every kind of step.** `http`, `exec`, `sql`, `kafka`, `temporal`, `mock`, `log`, `service`, `k6` and `sleep`, each with `expect`, `save` and `retry`.
- **Load tests with [k6](https://k6.io).** A `k6` step runs your script and turns its summary into checkable values: `metrics.http_req_duration.p95: { lt: 500 }`. Combine load with mock faults and resource limits to answer "does it survive a slow third party with 256 MiB?".
- **Resource checks.** A `service` step reports a container's state, OOM kill, exit code, CPU % and memory use.
- **Rich checks.** `eq`, `ne`, `contains`, `match`, `gt`/`gte`/`lt`/`lte`, `len` and `exists` on any path of a step's result, including list items (`calls.0.status`).
- **Waiting for asynchronous results.** `retry: { timeout: 30s, every: 1s }` polls a step until its checks pass.
- **Isolated runs.** Every run gets a unique `${run}` id to put in the data it creates, so runs never collide on shared servers.
- **Automatic cleanup.** Inserted rows are deleted, started workflows are terminated, and mock state is reset, even when the test fails.

**Safety and output**
- **Guardrails on shared servers.** No DDL on any database, every write on a remote database must include the run id, and `k6` load refuses to run while any dep is remote unless you pass `--allow-remote-load`.
- **Machine-readable.** Every command supports `--json`, and exit codes are fixed: `0` ok, `1` failed, `2` bad input.
- **Readable failures.** Failed checks are reported in words, e.g. `json.status: got "WAITING", want "APPROVED"`.
- **Built-in docs.** `dmock help guide` works without the repo.

## Examples

Start here if you want to see how you'd use d-mock on your own service:

- **[`examples/checkout`](examples/checkout)**: a service that charges orders through a payment provider.
  - *"If the provider times out, do we charge twice?"*: a scenario breaks the provider for one order, then checks the DB, every call the provider received, and the logs.
  - *"Will it survive launch day inside its pod?"*: k6 load against the service in a container limited to 500m CPU and 128Mi. It checks p95, errors, OOM, and that every accepted checkout really got charged.
- **[`examples/orders`](examples/orders)**: a callback racing a poll, faults and timeouts, plus a real OOM kill under a memory limit. This is the repo's own end-to-end test (`make e2e`).

## Install

```bash
go install github.com/ddlg3003/d-mock/cmd/dmock@latest
dmock doctor        # checks the tools below and prints how to install what's missing
```

| Tool | Needed for | Install |
|---|---|---|
| [Mockoon CLI](https://mockoon.com/cli/) 9.x | mocked HTTP APIs (any stack with `mocks:`) | `npm i -g @mockoon/cli@9.10.0` (Node.js 18+), or set `mockoon.runner: docker` |
| Docker or Podman | `mode: container` deps, container services, `mockoon.runner: docker` | [Docker Desktop](https://docs.docker.com/get-docker/), or `brew install podman && podman machine init && podman machine start` |
| [Temporal CLI](https://docs.temporal.io/cli) | `temporal` steps | `brew install temporal` |
| [k6](https://grafana.com/docs/k6/latest/set-up/install-k6/) | `k6` load-test steps | `brew install k6` |

Run `dmock doctor` inside a suite and it marks what that suite's `dmock.yaml` actually needs. Run `dmock doctor test/it/scenarios` and it also checks the tools your scenarios use (k6, temporal). It exits 1 when something needed is missing.

The container engine is `docker`, or `podman` if docker isn't installed. Override it with `$DMOCK_CONTAINER_CLI`.

## Quick start

```bash
dmock init test/it       # a starter suite (needs only mockoon-cli), any folder name
dmock test test/it       # up → every scenario → down; exit 0 when all pass
```

Or run an example. `orders` is a small service that waits for a decision from an external API, which arrives by callback, by polling, or not at all (then it cancels):

```bash
dmock test examples/orders          # or: make e2e
# or step by step, keeping the stack up between runs:
dmock -f examples/orders up         # redis container + API mock + orders service
dmock run examples/orders           # all 3 scenarios
dmock run examples/orders/scenarios/02_poll_path_with_faults.yaml   # just one
dmock -f examples/orders down
```

```
▶ poll path — API fails twice, then approves
  ✓ API answers HTTP 500 for the next 2 polls [mock] 2ms
  ✓ API has approved [mock] 1ms
  ✓ create order (no callback will come) [http] 4ms
  ✓ order approved by polling [http] 4.1s (9 attempts)
  ✓ exactly 3 polls (500, 500, 200) [mock] 3ms
  ...
PASS poll path — API fails twice, then approves run=dm582f22 5.1s
```

## Suites

A suite is a folder with a `dmock.yaml` in it. The folder can have any name and live anywhere; one repo can hold several.

```
test/it/                         ← any name
├── dmock.yaml                   ← makes the folder a suite: project, deps, mocks, services
├── mocks/<name>.yaml            ← mock specs: mocks.<name> needs only a port
├── configs/<service>.tmpl.yaml  ← service config templates, picked up by name
├── scenarios/                   ← every *.yaml under it, recursively, in path order
│   ├── 01_happy_path.yaml
│   └── payments/
│       ├── 01_timeout.yaml
│       └── 02_declined.yaml
├── load/                        ← k6 scripts (paths in steps resolve from the suite folder)
├── pod-limits/dmock.yaml        ← a nested suite: a variant with its own stack (e.g. in containers)
└── .dmock/                      ← generated state, logs, rendered configs; gitignore it
```

**Which suite?** Every command finds its suite the same way: from `-f` (a suite folder, or any stack file), else from the paths you pass to `run`, `test`, `lint` and `doctor`, else from the current folder. Each walks up to the nearest `dmock.yaml`. Paths from two different suites in one command are refused.

**Which scenarios?** `run` and `test` take any mix of:

| Path | Runs |
|---|---|
| nothing, or the suite folder | every scenario in `scenarios/` |
| a folder | every `*.yaml` under it, recursively, except nested suites and hidden folders |
| a file | that scenario |

**Conventions.** `mocks.<name>.spec` defaults to `mocks/<name>.yaml`. A service's `config.template` defaults to `configs/<service>.tmpl.<ext>` when exactly one such file exists. Anything written explicitly in `dmock.yaml` wins, so older stack files (`stack.yaml`, explicit paths) keep working.

**Nested suites** hold a variant of the stack, for example the same service in a container with CPU/memory limits for load tests. A nested suite has its own `.dmock/`, so give it its own ports and it can run next to its parent. It can reuse the parent's files with relative paths (`spec: ../mocks/partner.yaml`).

**In CI** (GitLab shown; the job needs `mockoon-cli`, plus docker/podman and k6 if the suite uses them):

```yaml
integration:
  script:
    - dmock test test/it --junit report.xml
  artifacts:
    when: always
    reports: { junit: report.xml }
```

## CLI

```
dmock [-f SUITE] [--json] [--mode DEP=MODE] <command>

init DIR                                       write a starter suite into DIR (never overwrites)
test [SUITE|DIR|FILE...] [--junit FILE]        up → run → down (always): the CI entry point
lint [SUITE|DIR|FILE...]                       check dmock.yaml, mock specs and scenarios (alias: validate)

up [--only a,b] [--restart a,b] [--dry-run]   start what is not running (idempotent)
down [name...]                                 stop services, mocks, containers (remote deps untouched)
status                                         what is running
logs NAME [--grep RE] [--tail N]               mock/service log file, or a dep's container log

mock set NAME KEY field=value...               per-key state ("-" = routes without a key)
mock fault NAME KEY FAULT [--times N]          inject a fault (default: until cleared)
mock clear NAME KEY                            clear a key's fault
mock calls NAME [--route ID] [--key K]         recorded requests
mock reset NAME                                drop all state, faults, recorded calls
mock compile NAME                              print the generated Mockoon environment

run [SUITE|DIR|FILE...] [--junit FILE] [--dry-run] [--keep] [--run-id ID] [--verbose]
```

**Exit codes:** `0` ok, `1` failed (a scenario failed or a component errored), `2` usage or config error.

### Built-in help

Everything needed to write and run suites is built into the binary, so a person or an AI agent with only `dmock` installed can work without this repo:

| Command | Prints |
|---|---|
| `dmock help guide` | [docs/guide.md](docs/guide.md): the run → read → fix loop, reading a JSON report, a scenario template, mistakes that look like bugs |
| `dmock help suite` | [docs/suite.md](docs/suite.md): suite layout, every `dmock.yaml` field, the mock spec format |
| `dmock help steps` | [docs/steps.md](docs/steps.md): every step's arguments, result fields and an example |
| `dmock help examples` | [docs/examples.md](docs/examples.md): patterns for faults, retries, races and load inside pod limits |
| `dmock version` | the installed version |

## dmock.yaml

```yaml
project: orders-example          # container names: dmock-<project>-<dep>

deps:
  redis:
    kind: redis                  # mysql | kafka | temporal | redis | anything (tells steps how to connect)
    mode: container              # container | remote
    container:
      image: redis:7.2.4
      ports: ["16379:6379"]
      env: {}
      args: []
      vars: { addr: "localhost:16379" }
    remote:
      vars: { addr: "${env.DEV_REDIS_ADDR}" }   # secrets come from your environment, not the file
    ready: { tcp: "localhost:16379", timeout: 30s }

mocks:
  partner: { port: 13001 }       # spec defaults to mocks/partner.yaml

mockoon:                         # optional
  runner: cli                    # cli (mockoon-cli on PATH) | docker (mockoon/cli image)
  version: 9.10.0

services:
  orders:
    dir: toy                     # working directory (relative to the suite folder)
    cmd: ["go", "run", "."]
    env: { ORDERS_CONFIG: "${services.orders.config}" }
    # config defaults to configs/orders.tmpl.yaml; rendered to .dmock/rendered/orders.yaml
    ready: { http: "http://localhost:18080/healthz", timeout: 90s }   # or tcp: / log: <regex>
    depends_on: [redis, partner]

  # the same service as a container with resource limits (see the nested suite examples/orders/containers)
  orders-in-container:
    config: { template: configs/orders-container.tmpl.yaml }
    container:
      image: my/orders:dev           # or build: { context, dockerfile, args, pre: [local build cmd] }
      cpus: "0.5"                    # docker/podman --cpus   ≈ resources.limits.cpu: 500m
      memory: 256m                   # --memory (no swap)     ≈ resources.limits.memory: 256Mi
      ports: ["18080:18080"]
      env: { ORDERS_CONFIG: /config/orders.yaml }
      mounts: ["${services.orders-in-container.config}:/config/orders.yaml:ro"]
    ready: { http: "http://localhost:18080/healthz" }
```

**Ports.** `lint` and `up` refuse a suite where two things would listen on the same host port: two mocks, or a mock and a container's published port.

**Logs** live in `.dmock/logs/<name>.log`. When a mock or service starts and its log is over 20 MiB, the log moves to `<name>.log.1` (replacing the older one), so logs stay bounded and the previous run stays readable. Secret-looking flags (`--admin-api-token`, `--password=…`) are written as `***` in the start line.

Inside a container, `localhost` is the container itself. Use `${host}` to reach the machine (mocks, container deps' published ports, other local services): it is `host.docker.internal` on docker and `host.containers.internal` on podman. Container logs are copied into `.dmock/logs/<service>.log`, so `logs` and `log` steps work the same for both kinds. `status` shows `OOM-KILLED exit=137` when a container hit its memory limit.

**References.** Service `config` templates, `env` and `cmd` can use `${...}` references:

- `${deps.<name>.<var>}`: the active mode's vars, plus `.mode`.
- `${mocks.<name>.url|port}`
- `${services.<name>.config|log}`
- `${env.VAR}`
- `${stack.dir}`

An unresolved reference is an error; d-mock never fills in a default. Write `$${` for a literal `${`.

**Processes.** Services run in their own process group, so `down` also stops child processes such as the binary `go run` builds. A service that exits before it's ready fails `up` straight away and shows the last log lines.

## Mock specs

The YAML compiles to a Mockoon environment (`dmock mock compile NAME` shows it).

```yaml
routes:
  - id: status                       # used by `mock calls --route status`
    method: GET
    path: orders/:id                 # Mockoon path syntax
    key: "urlParam 'id'"             # Mockoon helper that picks the state key from the request:
                                     #   urlParam 'x' | bodyRaw 'x' | queryParam 'x' | header 'x'
    state: { status: PENDING }       # default of each state field the route reads
    status: 200
    headers: { Content-Type: application/json }
    body: '{"id":"${key}","status":"${state.status}"}'
    variants:                        # other responses picked by state
      - when: { status: GONE }
        status: 404
        body: '{"error":"not found"}'
    faults:                          # switched on per key at runtime
      http500: { status: 500, body: '{"error":"down"}' }
      timeout: { latency: 40s }      # no body: reuses the normal body, just late

  - id: blob
    method: POST
    path: secure/inquiry
    body_file: fixtures/approved.json
    encode: ["./encrypt.sh"]         # run once at compile time, fixture on stdin, result served as is
```

**How it works inside Mockoon:**

- **State.** Per-key state lives in Mockoon global variables named `<mock>|<field>|<key>`. `dmock mock set` and scenario `mock` steps write them through Mockoon's admin API. Each run gets a random admin token, stored in `.dmock/state.json`.
- **Raw values.** `${state.x}` and `${key}` are emitted with `{{{ }}}`, so values aren't HTML-escaped. Mockoon's `{{ }}` would turn `=` into `&#x3D;` and break signed or encoded payloads.
- **Faults.** A fault is a response with rules on `<mock>|fault|<key>` and a hit counter `<mock>|faultn|<key>`. `--times N` serves the fault N times and then falls back to normal. Without `--times`, it lasts until `mock clear`.
- **Encoded bodies.** `encode` bodies are served with templating disabled, so the bytes reach the client unchanged. Use this for PGP-encrypted or signed fixtures.
- **Recorded calls.** Calls come from Mockoon's transaction log (last 1000) and are mapped back to route ids.

You can also use Mockoon helpers such as `{{now}}` or `{{faker 'person.firstName'}}` directly in a body.

## Scenarios

```yaml
name: poll path — partner fails twice, then approves
vars:
  order: "ord-${run}-2"            # ${run} is unique per run (dm + 6 hex)
  api: "http://localhost:18080"

steps:
  - name: partner fails twice
    mock: { name: partner, fault: { key: "${vars.order}", fault: http500, times: 2 } }

  - name: create order
    http: { method: POST, url: "${vars.api}/orders", json: { id: "${vars.order}" } }
    expect: { status: 201 }
    save: { created: json.id }     # → ${saved.created}

  - name: approved by polling
    http: { url: "${vars.api}/orders/${vars.order}" }
    retry: { timeout: 15s, every: 500ms }        # re-run until expect passes
    expect: { json.status: APPROVED, json.decided_by: poll }

cleanup:                           # always runs, even after a failure
  - http: { method: DELETE, url: "${vars.api}/orders/${vars.order}" }
```

When a step fails, the remaining steps are skipped and cleanup still runs. `${last.<path>}` refers to the previous step's result.

### Step types and results

| Step | Arguments | Result paths |
|---|---|---|
| `http` | `method, url, headers, body` or `json`, `timeout` | `status, body, json, headers` |
| `exec` | `cmd` (shell line or argv list), `dir, env, stdin, timeout` | `exit, stdout, stderr, json` |
| `sql` | `dep, query, args`, or `dep, insert: {table, key, row}` | `rows, count` / `affected, key` |
| `kafka` | `dep, topic, key, value` or `json`, `headers` | `topic, sent` |
| `temporal` | `dep, action: start\|signal\|describe\|history\|terminate`, `workflow_id, type, task_queue, signal, input` | `status` (RUNNING, COMPLETED…), `events` (TimerCanceled…), `json` |
| `mock` | `name` plus one of `set: {key, state}`, `fault: {key, fault, times}`, `clear: key`, `reset: true`, `calls: {route, key}` | `count, calls[].{method,path,status,body,json,response}` |
| `log` | `service` or `mock`, `grep` (a regex; only lines logged since the scenario started) | `count, lines` |
| `service` | `name` | `state` (running, exited, stopped), `oom_killed`, `exit_code`, `cpu_percent`, `mem_bytes`, `mem_limit_bytes`, `mem_percent` |
| `k6` | `script, vus, duration, iterations, env, args, timeout` | `exit` (99 = a k6 threshold failed), `metrics.<name>.<stat>` (`p95`, `p99`, `avg`, `max`, `rate`, `count`), `checks.<name>.passes/fails`, `services.<name>.state/oom_killed` (container services after the load) |
| `sleep` | `"2s"` | |

**Dep vars each step needs:**

- `sql` (kind `mysql`): `dsn`.
- `kafka`: `brokers` (a list or comma-separated).
- `temporal`: `address`, `namespace`, and optionally `cli_args` (for TLS flags and the like).

### expect

Each key is a dotted path into the result, and list indexes work (`calls.0.status`). The value is either a literal, compared as text, or a matcher:

```yaml
expect:
  status: 200
  json.status: { ne: WAITING }
  count: { gte: 1, lt: 4 }
  events: { contains: TimerCanceled, not_contains: ActivityTaskScheduled }
  body: { match: "^\\{" }
  calls: { len: 2 }
  json.error: { exists: false }
```

### Safety and cleanup

- **No DDL.** `sql` refuses DDL (`CREATE/ALTER/DROP/TRUNCATE/RENAME/GRANT/REVOKE`) on every dep.
- **Writes are scoped to the run.** On a `remote` dep, every statement that isn't a plain read (`SELECT/SHOW/DESCRIBE/EXPLAIN`, or a `WITH` that only selects) counts as a write, including `CALL`, `SET` and `LOAD DATA`. A write must contain the run id in the statement or its args, not in a comment. A scenario can only touch rows it created itself.
- **One statement per step.** Comments are stripped before these checks, so they can't hide a statement's kind.
- **`sql.insert`** requires the key value to contain the run id, records the row, and deletes it after the run.
- **Temporal workflows** started by `temporal start` are terminated after the run if they're still running.
- **Mock state and faults** a scenario set are reset to their defaults after the run.
- **`--keep`** skips this automatic cleanup so you can inspect the state. Explicit `cleanup:` steps still run.
- **`--dry-run`** expands and prints each step without running it. A value a step would `save` shows up in later steps as `<saved.name>`.

## Why d-mock has its own scenario runner (and not Venom)

[Venom](https://github.com/ovh/venom) is an existing tool for YAML test steps (HTTP, SQL, Kafka…). Using it as the scenario engine was the first idea. It was dropped for two reasons:

1. **It can't be extended in Go.** New Venom step types can only combine existing ones in YAML. The steps d-mock needs most (`temporal`, `mock set/fault/calls`) would each have become "run a shell command that calls dmock".
2. **The safety features would live outside it.** Putting the run id on every write, the SQL write guard, and deleting test rows and workflows after a run all need to see every step. Wrapped around Venom, they would be glue scripts, not guarantees.

So d-mock has a small built-in runner: one binary and one JSON report format. If you already have Venom suites, call them from an `exec` step: `exec: { cmd: "venom run suite.yml" }`.

## Development

```bash
make test   # unit tests; the Mockoon live test runs when mockoon-cli is on PATH
make e2e            # dmock test examples/orders
make e2e-all        # every example suite, including containers and k6 load
```

## Status

| Part | Status |
|---|---|
| Stack lifecycle (container deps via docker/podman, mockoon-cli mocks, services) | Tested end to end with the example |
| Mock compiler and admin API | Tested live against Mockoon CLI 9.10 |
| `http`, `mock`, `log`, `sleep`, `exec` steps | Tested end to end |
| `temporal` steps | Tested against `temporal server start-dev` and a remote TLS cluster |
| `sql` and `kafka` steps | Used against a shared MySQL server and Kafka broker (reads, plus writes scoped to the run id) |
| Container services (build, limits, host access, OOM detection) | Tested end to end with podman (`examples/orders/containers`) |
| `k6` step | Tested end to end (load against a 0.5 CPU / 64 MiB container; OOM shown in the failing step) |
| `mockoon.runner: docker` | Not tested yet |
