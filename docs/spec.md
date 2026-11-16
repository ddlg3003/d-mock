# d-mock YAML spec

Every file d-mock reads and every field in it: the suite (`dmock.yaml`), mock specs, scenarios,
hooks and steps. Print it with `dmock help spec`. How to work with it day to day is in
`dmock help guide`, patterns in `dmock help examples`.

- [Suite layout](#suite-layout)
- [dmock.yaml](#dmockyaml): [deps](#deps), [mocks](#mocks), [services](#services), [hooks](#hooks), [references](#references-)
- [Mock spec](#mock-spec-mocksnameyaml): [encoded or encrypted payloads](#encoded-or-encrypted-payloads)
- [Scenario file](#scenario-file): [matrix and repeat](#matrix-and-repeat), [include](#include), [common step fields](#common-step-fields), [expect](#expect)
- Steps: [http](#http) · [sql](#sql) · [mock](#mock) · [log](#log) · [exec](#exec) · [kafka](#kafka) · [temporal](#temporal) · [service](#service) · [stack](#stack) · [parallel](#parallel) · [k6](#k6) · [sleep](#sleep)
- [Shell lines and references](#shell-lines-and-references) · [Lint warnings](#lint-warnings) · [Safety and cleanup](#safety-and-cleanup)

## Suite layout

A suite is a folder with a `dmock.yaml`. The folder can have any name and live anywhere; one
repo can hold several.

```
test/it/                         any name
├── dmock.yaml                   the stack: project, deps, mocks, services, hooks
├── mocks/<name>.yaml            mock specs; mocks.<name> in dmock.yaml needs only a port
├── configs/<service>.tmpl.yaml  service config templates, picked up by name
├── scenarios/                   every *.yaml under it, recursively, run in path order
│   ├── 01_happy_path.yaml
│   └── payments/01_timeout.yaml
├── steps/                       shared step lists for `include` (any folder outside scenarios/)
├── load/                        k6 scripts
├── pod-limits/dmock.yaml        a nested suite: a variant of the stack with its own .dmock/
└── .dmock/                      generated state, logs, rendered configs; gitignore it
```

**Which suite.** Every command walks up to the nearest `dmock.yaml` from `-f` (a suite folder or
a stack file), else from the paths passed to `run`/`test`/`lint`/`doctor`, else from the current
folder. Paths from two suites in one command are refused.

**Which scenarios.** `run` and `test` take any mix of: nothing or the suite folder (every
scenario in `scenarios/`), a folder (every `*.yaml` under it, except nested suites and hidden
folders), a file.

**Conventions.** `mocks.<name>.spec` defaults to `mocks/<name>.yaml`. A service's
`config.template` defaults to `configs/<service>.tmpl.<ext>` when exactly one such file exists.
Write either explicitly only when the file lives elsewhere (e.g. a nested suite reusing
`../mocks/payments.yaml`).

**Nested suites** hold a variant of the stack (e.g. the service in a container with pod limits).
Each has its own `.dmock/`; give it its own ports and it runs next to its parent.

## dmock.yaml

A complete example; every field is described below it.

```yaml
project: shop                          # required; containers are named dmock-<project>-<name>

deps:
  db:
    kind: mysql
    mode: container                    # container | remote; override per run: dmock --mode db=remote
    container:
      image: mysql:8.0.36
      ports: ["13306:3306"]
      env: { MYSQL_ROOT_PASSWORD: root, MYSQL_DATABASE: shop }
      vars: { dsn: "root:root@tcp(localhost:13306)/shop" }
    remote:
      vars: { dsn: "${env.DEV_DB_DSN}" }   # secrets stay in your environment
    ready: { tcp: "localhost:13306", timeout: 120s }
    after_ready:                       # on a fresh container, once ready
      - go run ./cmd/migrate -dsn "${deps.db.dsn}"

mocks:
  payments: { port: 14001 }            # spec: mocks/payments.yaml

mockoon: { runner: cli, version: 9.10.0 }   # optional

services:
  checkout:                            # config: configs/checkout.tmpl.yaml
    dir: ../..                         # the repo root, relative to the suite folder
    cmd: ["go", "run", "./cmd/checkout"]
    env: { CHECKOUT_CONFIG: "${services.checkout.config}" }
    ready: { http: "http://localhost:18090/healthz", timeout: 120s }
    depends_on: [db, payments]

hooks:
  before_each:
    - http: { url: "http://localhost:18090/healthz" }
      retry: { timeout: 30s }
      expect: { status: 200 }
```

| Field | |
|---|---|
| `project` | required; names containers and scopes `.dmock/` state |
| `deps.<name>` | infrastructure the services use (see [deps](#deps)) |
| `mocks.<name>` | fake third-party APIs served by Mockoon (see [mocks](#mocks)) |
| `mockoon.runner` | `cli` (default: `mockoon-cli` on PATH) or `docker` (the `mockoon/cli` image) |
| `mockoon.version` | the image tag for `runner: docker`; the expected CLI version for `doctor` |
| `services.<name>` | the code under test (see [services](#services)) |
| `hooks` | steps run around the scenarios (see [hooks](#hooks)) |

### deps

| Field | |
|---|---|
| `kind` | `mysql`, `kafka`, `temporal`, `redis` or anything: tells steps how to talk to it |
| `mode` | `container` (dmock starts it) or `remote` (an existing server; `down` never touches it) |
| `container.image` | the image |
| `container.ports` | `host:container` published ports |
| `container.env`, `container.args` | container env and extra `run` arguments |
| `container.vars` | values for this mode, as `${deps.<name>.<var>}`: what steps and configs connect with |
| `remote.vars` | the same for `remote` mode |
| `ready` | how to know it's up: one of `tcp: host:port`, `http: URL` (any answer below 500), `log: REGEX`; `timeout` (default `60s`) |
| `after_ready` | commands run in order on a **fresh** container once it's ready, before anything that depends on it starts: migrations, topics, fixtures. Not run for `remote` deps or a container that was already running. |

Vars each step type needs: `sql` → `dsn` (kind `mysql`); `kafka` → `brokers` (list or
comma-separated); `temporal` → `address`, `namespace`, optional `cli_args`.

A **command** (in `after_ready`) is a shell line (`- go run ./migrate "${deps.db.dsn}"`, run with
`sh -c`) or an argv list (`- ["go", "run", "./migrate"]`). It runs from the suite folder, may use
[references](#references-), and its output goes to the dep's or service's log
(`.dmock/logs/<name>.log`). A non-zero exit fails `up` with the last lines of output.
In a shell line each reference becomes exactly one quoted word (see [Shell lines](#shell-lines-and-references)).

### mocks

| Field | |
|---|---|
| `port` | required; where the mock listens (`${mocks.<name>.url}` = `http://localhost:<port>`) |
| `spec` | default `mocks/<name>.yaml` (see [Mock spec](#mock-spec-mocksnameyaml)) |

### services

A service is a local process (`cmd`) or a container (`container`).

| Field | |
|---|---|
| `dir` | working directory of `cmd`, relative to the suite folder |
| `cmd` | argv list; runs in its own process group (`down` stops children such as `go run`'s binary) |
| `env` | environment; may use references |
| `config.template` | default `configs/<service>.tmpl.<ext>`; rendered with references to `.dmock/rendered/<service>.<ext>`, available as `${services.<service>.config}` |
| `ready` | as for deps; a service that exits before it's ready fails `up` at once with its last log lines |
| `depends_on` | deps, mocks and services started (and ready) first |
| `after_ready` | commands run each time the service has (re)started and is ready |
| `container.image` or `container.build` | `build: { context, dockerfile, args, pre: [cmd] }`; `pre` runs in the context first (e.g. a local `go build`) |
| `container.cpus` | `--cpus`, e.g. `"0.5"` ≈ `resources.limits.cpu: 500m` |
| `container.memory` | `--memory` with no swap, e.g. `128m` ≈ `resources.limits.memory: 128Mi`; over it the container is OOM killed |
| `container.ports`, `container.env`, `container.args` | as for deps |
| `container.mounts` | `host:container[:ro]`; usually the rendered config: `"${services.checkout.config}:/config/checkout.yaml:ro"` |

Inside a container `localhost` is the container: use `${host}` in its config to reach mocks and
other local ports. Container logs are copied into `.dmock/logs/<service>.log`, so `logs` and
`log` steps work the same for both kinds.

**Ports.** `lint` and `up` refuse two things listening on the same host port.

**Logs.** `.dmock/logs/<name>.log`; over 20 MiB at start it moves to `<name>.log.1`. Secret-looking
flags are written as `***` in the start line.

### hooks

Steps (the same syntax as scenario steps, including `include`) run around the scenarios of
`run` and `test`:

| Hook | Runs | When it fails |
|---|---|---|
| `before_all` | once, before the first scenario | every scenario is reported **SKIP**; `after_all` still runs |
| `before_each` | in each scenario run, before its steps (shown as `before_each: …`) | that scenario fails; its cleanup still runs |
| `after_each` | in each scenario run, after its `cleanup` steps, even when it failed | that scenario fails |
| `after_all` | once, after the last scenario, always | reported as a failed `hook: after_all` |

What `before_all` saves is visible to every scenario as `${saved.<name>}`. Rows, mock state and
workflows created in `before_all` are cleaned up after `after_all`; those created in
`before_each`/`after_each` with the scenario. `after_all`'s `log` steps see the whole run.

### References (`${...}`)

Usable in configs, `env`, `cmd`, `after_ready`, mounts, and every scenario value.

| Reference | Value |
|---|---|
| `${deps.<dep>.<var>}` | the dep's vars for its active mode; also `.mode`, and `.container` (container name, container mode only) |
| `${mocks.<mock>.url}`, `.port` | where a mock listens |
| `${services.<svc>.config}`, `.log` | rendered config file, log file |
| `${env.X}` | an environment variable |
| `${host}` | the host as seen from a container (`host.docker.internal` / `host.containers.internal`) |
| `${stack.dir}`, `${stack.project}` | suite folder, project |
| `${stack.engine}` | `docker` or `podman` (e.g. `${stack.engine} exec ${deps.kafka.container} …`) |

Scenarios add `${run}`, `${vars.x}`, `${saved.x}`, `${last.path}`, `${matrix.x}`,
`${iteration}` (see [Scenario file](#scenario-file)).

An unresolved reference is an error; d-mock never fills in a default. `$${` is a literal `${`.
A string that is exactly one reference keeps the value's type (number, list, map).

## Mock spec (`mocks/<name>.yaml`)

Compiles to a Mockoon environment (`dmock mock compile NAME` prints it).

```yaml
routes:
  - id: charge                       # for `calls: {route: charge}`
    method: POST
    path: charges                    # Mockoon path syntax: orders/:id
    key: "bodyRaw 'order_id'"        # picks the state key from the request
    state: { result: SUCCEEDED }     # default of each state field the route reads
    status: 200
    headers: { Content-Type: application/json }
    body: '{"charge_id":"ch-${key}","status":"${state.result}"}'
    latency: 0s
    variants:                        # other answers, picked by state
      - when: { result: GONE }
        status: 404
        body: '{"error":"no such charge"}'
    faults:                          # switched on per key at runtime
      timeout: { latency: 5s }       # no body: the normal body, late
      http500: { status: 500, body: '{"error":"provider down"}' }

  - id: blob
    method: POST
    path: secure/inquiry
    body_file: fixtures/approved.json
    encode: ["./encrypt.sh"]         # run once at compile time, fixture on stdin, served as is
```

| Route field | |
|---|---|
| `id` | route id, used by `calls` and errors |
| `method`, `path` | what to match |
| `key` | a Mockoon helper choosing the state key: `urlParam 'x'`, `bodyRaw 'x'`, `queryParam 'x'`, `header 'x'`; without it, every request shares the state key `-`. `bodyRaw` can't read an encoded or encrypted body: add a `codec` with `decode`, or key on a readable part; with no readable id, omit `key` and run the scenarios that use this mock one at a time |
| `state` | declared state fields and their defaults; `mock set` refuses undeclared fields |
| `status`, `headers`, `body` / `body_file`, `latency` | the default answer; `${key}` and `${state.<field>}` are filled in raw (not HTML-escaped) |
| `encode` | an argv command that transforms `body_file` **once, when the mock starts** (signing, encryption); served byte for byte with templating off. For an answer that depends on the request, use `codec` |
| `variants[]` | `when: {field: value}` plus `status`, `headers`, `body`/`body_file`, `encode`, `latency` |
| `faults.<name>` | `status`, `body`, `latency`; served to a key while switched on (`times` N, or until cleared) |
| `codec` | this route's own codec, replacing the spec's (`codec: {}` = none, e.g. a plain health route) |

### Encoded or encrypted payloads

Two ways, depending on whether the answer must depend on the request:

| Need | Use | Runs |
|---|---|---|
| a fixed answer per state (status-only replies, error bodies) | route `body_file` + `encode`, a `variants` entry per state | once, when the mock starts |
| an answer that echoes the request (its id, a signature over its nonce, a fresh timestamp), or a request body you need to read (`key`, `calls`) | `codec` | per request |

```yaml
codec:                                  # top level of the spec: every route (a route's codec replaces it)
  decode: ["../codec/decrypt.sh"]       # request body in → plain text out, before Mockoon sees it
  encode: ["../codec/encrypt.sh"]       # each templated response body in → what is sent back
routes:
  - id: inquiry
    method: POST
    path: inquiry
    key: "bodyRaw 'ref'"                # reads the decoded body
    state: { status: APPROVED }
    body: '{"ref":"${key}","status":"${state.status}"}'   # echoes the request, then encrypted
```

With a codec, Mockoon moves to an internal port and a small d-mock proxy listens on the mock's
port: it decodes each request body, forwards it to Mockoon, and encodes the response body.
Everything else stays as without a codec: state, variants, faults (a fault's body is encoded
too), latency, the admin API, and `mock calls`, which records the **decoded** request (so
`calls.0.json.ref` works). When the decoded body is JSON, Mockoon receives it as
`application/json` and the original content type is kept in the header
`x-dmock-encoded-content-type`.

Codec commands are argv lists (no shell), run from the spec's folder, the payload on stdin and
the result on stdout, with a 10 s timeout and a 16 MiB limit. A request that fails to decode is
answered `400` without reaching Mockoon (and isn't recorded); a response that fails to encode
is answered `502`; both are logged in the mock's log with the command's error. A route can't
have both a codec `encode` and a static `encode`.

Mockoon helpers (`{{now}}`, `{{faker 'person.firstName'}}`) work in bodies. A mock keeps its
last 1000 requests for `calls`.

## Scenario file

```yaml
name: provider times out once — checkout retries and charges exactly once
description: optional longer text
vars:                                # your values; may use ${run}, ${matrix.x}, other vars
  order: "ord-${run}"
  api: "http://localhost:18090"
matrix: {}                           # optional: one run per combination
repeat: 1                            # optional: run each case N times
steps:
  - name: check out                  # shown in the report; write it as a sentence
    http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
    expect: { status: 202 }
cleanup:                             # optional: always runs, even after a failure
  - http: { method: DELETE, url: "${vars.api}/orders/${vars.order}" }
```

| Field | |
|---|---|
| `name` | required |
| `description` | optional |
| `vars` | scenario values as `${vars.x}` |
| `matrix`, `repeat` | see below |
| `steps` | required; run in order; a failed step skips the rest |
| `cleanup` | steps that always run after `steps` (failures are reported, not hidden) |

Scenario references: `${run}` (unique per run: `dm` + 6 hex; put it in every id you create),
`${vars.x}`, `${saved.x}` (a value an earlier step saved), `${last.path}` (a path in the previous
step's result), `${matrix.x}`, `${iteration}`, plus every [suite reference](#references-).

### matrix and repeat

`matrix` runs the scenario once per combination of its lists (a table-driven test). Keys combine
in file order; a list item may be a map, read with dotted paths.

```yaml
matrix:
  region: [EU, US]
  case:
    - { callback: APPROVED, want: DONE }
    - { callback: REJECTED, want: CANCELLED }
# 4 runs: [region=EU case={callback:APPROVED want:DONE}], …
# in steps: ${matrix.region}, ${matrix.case.callback}, ${matrix.case.want}
```

`repeat: N` runs every case N times (`#1`, `#2`, … in the name, `${iteration}` = 1..N), each with
its own `${run}`. Use it to shake out races. Each run is a separate report (and JUnit test case).

### include

A step `- include: PATH` is replaced, when the file loads, by the steps in PATH: a YAML list of
steps, path relative to the including file. Included steps see the including scenario's vars.
Includes can nest (cycles are refused). Keep shared files outside `scenarios/` (e.g. `steps/`),
or they would run as scenarios.

```yaml
# steps/new_order.yaml
- name: an order to check out
  sql: { dep: db, insert: { table: orders, key: id, row: { id: "${vars.order}", amount: 150000, status: NEW } } }
```

```yaml
# scenarios/06_order_event.yaml
steps:
  - include: ../steps/new_order.yaml
  - name: check out
    http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
```

### Common step fields

A step has exactly one action key (`http`, `sql`, …, or `include`), plus:

| Field | |
|---|---|
| `name` | shown in the report; may use references |
| `expect` | checks on the step's result (see [expect](#expect)) |
| `retry` | `{timeout, every}` (every default `1s`): re-run until `expect` passes or time is up |
| `save` | `{name: result.path}`: keep a value as `${saved.name}` for later steps |
| `after` | only on a `parallel` branch: wait this long after the common start |

`after`, `retry.timeout` and `retry.every` are fixed durations (`200ms`, `30s`): they are read
before references are filled in, so `${...}` there is a lint error. Use a `sleep` step (it takes
references) to vary timing per matrix case.

`--dry-run` expands each step without running it; a value a step would save shows as `<saved.name>`.

### expect

Each key is a dotted path into the step's result; list indexes work (`calls.0.status`). A plain
value is compared as text; a map is one or more matchers:

```yaml
expect:
  status: 200                                  # equals
  json.status: { ne: WAITING }
  count: { gte: 1, lt: 4 }                     # gt gte lt lte compare numbers
  events: { contains: TimerCanceled, not_contains: ActivityTaskScheduled }
  body: { match: "^\\{" }                      # regex
  calls: { len: 2 }
  json.error: { exists: false }
```

A failure reads `json.status: got "WAITING", want "APPROVED"`.

## Steps

### http

Call an API (your service, or a mock directly).

| Argument | |
|---|---|
| `url` | required |
| `method` | default `GET` |
| `headers` | map |
| `json` | a value sent as JSON (sets `Content-Type`) |
| `body` | raw body text (instead of `json`) |
| `encode` | argv command: the request body (references filled in) goes in on stdin, its stdout is sent |
| `decode` | argv command: the response body goes in, its stdout is parsed for `json` |
| `timeout` | default `30s` |

**Result:** `status`, `body` (raw, as received), `json` (when the body, or the decoded body, is
JSON), `decoded` (with `decode`), `headers`.

```yaml
- http: { method: POST, url: "${vars.api}/orders", json: { id: "${vars.order}", amount: 1500 } }
  expect: { status: 201, json.id: "${vars.order}" }

- name: an encrypted callback, built for this run
  http:
    method: POST
    url: "${vars.api}/callbacks"
    json: { ref: "${vars.ref}", status: APPROVED }
    encode: ["./codec/encrypt.sh"]
    decode: ["./codec/decrypt.sh"]     # only if the answer is encrypted too
  expect: { status: 200 }
```

### sql

Read or write a `mysql` dep (dep var `dsn`).

| Argument | |
|---|---|
| `dep` | the dep name |
| `query`, `args` | one statement; `?` placeholders filled from `args` |
| `insert: {table, key, row}` | insert one row; `row[key]` must contain the run id; deleted after the run |

**Result:** reads give `rows` (list of column maps) and `count`; writes give `affected`; `insert`
gives `affected` and `key`.

```yaml
- sql: { dep: db, insert: { table: orders, key: id, row: { id: "${vars.order}", amount: 150000, status: NEW } } }
- sql: { dep: db, query: "SELECT status FROM orders WHERE id = ?", args: ["${vars.order}"] }
  retry: { timeout: 20s, every: 1s }
  expect: { rows.0.status: PAID }
```

### mock

Change or inspect a mock at runtime. `name` plus exactly one of:

| Argument | Does |
|---|---|
| `set: {key, state: {field: value}}` | set state fields for one key (declared fields only) |
| `fault: {key, fault, times}` | serve a declared fault to that key, `times` calls or (no `times`) until cleared |
| `clear: KEY` | clear a key's fault |
| `reset: true` | drop all state, faults and recorded calls |
| `calls: {route, key}` | recorded requests, filtered by route id and/or key (whole-token match) |

**Result** (`calls`): `count`, `calls[]` oldest first, each with `method`, `path`, `route_id`,
`query`, `headers` (keys lowercased: `headers.idempotency-key`), `status`, `body`, `json`,
`response`, `timestamp_ms`. State and faults a scenario sets are reset after the run.

```yaml
- mock: { name: payments, fault: { key: "${vars.order}", fault: timeout, times: 1 } }
- mock: { name: payments, calls: { route: charge, key: "${vars.order}" } }
  expect: { count: 2, calls.1.json.idempotency_key: "pay-${vars.order}", calls.0.headers.authorization: { exists: true } }
```

### log

Grep a service's or mock's log: only lines written since the scenario started (in `after_all`:
since the run started).

| Argument | |
|---|---|
| `service` or `mock` | whose log |
| `grep` | a regex |

**Result:** `count`, `lines`.

```yaml
- log: { service: checkout, grep: "order=${vars.order} charge attempt 1 failed" }
  expect: { count: 1 }
```

### exec

Run a command from the suite folder.

| Argument | |
|---|---|
| `cmd` | a shell line, or an argv list |
| `dir`, `env`, `stdin`, `timeout` | optional |

**Result:** `exit`, `stdout`, `stderr` (raw, with the trailing newline), `json` (when stdout is JSON).

An `exec` step passes whatever its exit code unless you `expect: { exit: 0 }`. Saving `stdout`
keeps the newline: print without one (`printf`) or print JSON and save `json.x`. In a shell line,
each reference is one quoted word (see [Shell lines](#shell-lines-and-references)).

```yaml
- exec: { cmd: "./seed.sh ${vars.order}" }
  expect: { exit: 0 }
```

### kafka

Produce one message, or read what was published (`consume`). Dep var `brokers`.

**Produce:**

| Argument | |
|---|---|
| `dep`, `topic` | required |
| `key` | message key |
| `value` or `json` | the payload |
| `headers` | map |

**Result:** `topic`, `sent`.

**Consume** (`dep` plus `consume: {…}`): reads every partition of the topic, from one second
before the scenario started, without a consumer group (nothing is committed; your service's
consumers are not disturbed). Use it with `retry` to wait for a message.

| `consume` argument | |
|---|---|
| `topic` | required |
| `key` | only messages with this key |
| `match` | `{path: value}`: only messages whose JSON value has these values (dotted paths, compared as text) |
| `since` | read from this long before the scenario started instead (e.g. `5m`) |

**Result:** `topic`, `count`, `messages[]` oldest first, each with `key`, `value`, `json`
(when JSON), `headers`, `partition`, `offset`, `time`.

```yaml
- kafka: { dep: kafka, topic: payment-events, key: "${vars.order}", json: { order_id: "${vars.order}", status: PAID } }
- kafka: { dep: kafka, consume: { topic: order-events, key: "${vars.order}", match: { status: PAID } } }
  retry: { timeout: 15s }
  expect: { count: 1, messages.0.json.order_id: "${vars.order}" }
```

### temporal

Drive a workflow through the `temporal` CLI (dep vars `address`, `namespace`, optional `cli_args`).

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

### service

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

### stack

Take part of the stack down or bring it back, to test outages and crash recovery. Exactly one of:

| Argument | Does |
|---|---|
| `stop: NAME` | graceful stop: a container dep or service is stopped (data kept), a process gets SIGTERM, a mock stops |
| `kill: NAME` | the same, hard (SIGKILL / `kill`): a crash |
| `start: NAME` | start it again (a stopped container keeps its data); waits until ready, runs the service's `after_ready` |
| `restart: NAME` | stop then start (a mock restarts with its state and calls cleared) |

NAME is a dep (container mode only; a `remote` dep is refused), mock or service. **Result:**
`action`, `name`, `kind`, `state`.

**After the run, every part a scenario touched is put back as it was before the scenario.** The
first `stack` step on a part records whether it was running; later steps on it don't change that
record. In cleanup, before anything else:

1. parts that were **not** running and are running now are stopped (e.g. a worker the scenario
   started);
2. then parts that **were** running and are down now are started again (e.g. the database a
   scenario stopped).

Stopping first means two parts that share a port or a queue (an old and a new worker in a
deploy-swap test) are never up together during cleanup. Parts a scenario never touched with a
`stack` step are left alone.

```yaml
- stack: { stop: db }
- http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } }
  expect: { status: 503 }
- stack: { start: db }
```

### parallel

Run steps at the same moment and wait for all of them: double submits, a callback racing a poll,
two workers on one row. Branches start together behind a barrier (they arrive within a few
milliseconds); `after` on a branch delays it to force an order. Either:

| Form | |
|---|---|
| `steps: [step, step, …]` | different steps side by side (two or more) |
| `times: N` + `step: {…}` | the same step N times (N ≥ 2); branches are named `<name> #1`, `#2`, … |

Each branch is a normal step (with its own `expect`, `retry`, `save`); branches can't be
`parallel` themselves, two branches can't `save` the same name, and a repeated step can't
`save`. What branches save is visible after the step. The step fails when any branch fails
(`N of M branches failed: branch i (name): …`); its own `expect` then checks the combined result.

**Result** (order-independent, so a race can be asserted whoever wins):

| Path | |
|---|---|
| `results.<i>.*` | branch i's result (in the order written), plus `name`, `passed`, `error`, `failures` |
| `count.<field>.<value>` | how many branches had that value of a scalar result field: `count.status.202` |
| `passed`, `failed` | how many branches passed / failed |
| `finished` | branch indexes in the order they finished |

```yaml
- name: double click — two checkouts at once
  parallel:
    times: 2
    step: { http: { method: POST, url: "${vars.api}/checkout", json: { order_id: "${vars.order}" } } }
  expect: { count.status.202: 1, count.status.409: 1 }

- name: the callback lands 200 ms after the poll starts
  parallel:
    steps:
      - name: poll
        http: { method: POST, url: "${vars.api}/jobs/poll" }
      - name: callback
        after: 200ms
        http: { method: POST, url: "${vars.api}/callbacks", json: { id: "${vars.order}", status: APPROVED } }
```

### k6

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
  expect: { metrics.http_req_failed.rate: { lt: 0.01 }, services.checkout.oom_killed: false }
  save: { accepted: checks.accepted.passes }
```

### sleep

```yaml
- sleep: 2s
```

Prefer `retry` on a check; sleep only to prove that something does **not** happen.

## Shell lines and references

Suite files are code: they run commands. Values are data: in a line a shell reads (an `exec`
`cmd` string, a string `after_ready` command, the script of any `sh -c` / `bash -c` argv), every
`${...}` becomes exactly **one literal word**, quoted for where it sits:

| In the line | `${saved.x}` = `a&b; c $(d)` becomes |
|---|---|
| `echo ${saved.x}` | `echo 'a&b; c $(d)'` |
| `echo "id=${saved.x}"` | `echo "id=a&b; c \$(d)"` |
| `echo 'id=${saved.x}'` | `echo 'id=a&b; c $(d)'` |

So a DSN with `&` stays one argument, and an answer from the system under test can never run
as a command. A reference can't add flags or shell syntax to a line: write those in the line, or
use an argv list. Argv lists (service `cmd`, `build.pre`, `encode`, `decode`, `codec`) never go
through a shell.

## Lint warnings

`dmock lint` also prints warnings that don't fail it: a service whose `cmd` (or image) runs
Mockoon itself has no `mock set/fault/calls`, per-key state or cleanup, so it should be a mock
under `mocks:` (with `body_file` + `encode` or a `codec` for encoded payloads).

## Safety and cleanup

- **No DDL** (`CREATE/ALTER/DROP/TRUNCATE/RENAME/GRANT/REVOKE`) in `sql` steps, on any dep.
  Schemas belong in `after_ready`.
- **Writes are scoped to the run on `remote` deps:** every statement that isn't a plain read
  must contain the run id in the statement or its args (not in a comment). One statement per step.
- **`sql.insert`** rows are deleted after the run.
- **Temporal workflows** a run started are terminated if still running.
- **Mock state and faults** a run set are reset.
- **Stack parts** a run touched with `stack` steps are put back as they were before it, first:
  what it started is stopped, what it took down is started.
- **`--keep`** skips the automatic cleanup to inspect a failure; `cleanup:` steps still run.
