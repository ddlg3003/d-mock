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
- **Setup hooks.** `after_ready` commands run migrations, create topics or load fixtures once a fresh container is ready.
- **Safe to repeat.** `up` starts only what is down and restarts a mock when its spec changed. `down` never touches remote dependencies.

**Mocks** (on [Mockoon](https://mockoon.com))
- **Short YAML instead of Mockoon JSON.** Routes, responses and variants compile to a Mockoon environment.
- **Per-request state.** Each request is keyed by a URL param, body field or header, so every test entity gets its own answer: `dmock mock set payments order-1 status=APPROVED`.
- **Fault injection.** HTTP 500s, timeouts or any custom response, for N calls or until cleared: `dmock mock fault payments order-1 http500 --times 3`.
- **Call recording.** Assert exactly what your service sent (body, query, headers), how often and in what order.
- **Encoded and encrypted payloads.** Values are never HTML-escaped. Fixed encrypted or signed fixtures are served byte for byte; with a `codec`, d-mock decrypts each request before the mock sees it and encrypts each answer per request, so state, faults and recorded calls work on plain data and answers can echo the request.

**Scenarios**
- **One YAML format for every kind of step.** `http`, `exec`, `sql`, `kafka` (produce and consume), `temporal`, `mock`, `log`, `service`, `stack`, `parallel`, `k6` and `sleep`, each with `expect`, `save` and `retry`.
- **Races.** A `parallel` step fires steps at the same moment (double submits, a callback racing a poll), optionally offset with `after: 200ms`, and checks the outcome whoever wins: `count.status.202: 1`.
- **Outages and crashes.** A `stack` step stops, kills or restarts a database, a mock or a service mid-scenario; whatever it took down comes back after the run.
- **Events.** `kafka: { consume: { topic, key } }` asserts what your service published, without a consumer group.
- **Encrypted calls into your service.** `http` with `encode` / `decode` sends an encrypted body built for the run (a callback, a webhook) and reads an encrypted answer.
- **Table-driven and repeated runs.** `matrix` runs a scenario once per case; `repeat: 20` shakes out flaky races.
- **Shared steps and hooks.** `include` reuses step lists; suite hooks (`before_all`, `before_each`, `after_each`, `after_all`) run checks and setup around every scenario.
- **Load tests with [k6](https://k6.io).** A `k6` step runs your script and turns its summary into checkable values: `metrics.http_req_duration.p95: { lt: 500 }`. Combine load with mock faults and resource limits to answer "does it survive a slow third party with 256 MiB?".
- **Resource checks.** A `service` step reports a container's state, OOM kill, exit code, CPU % and memory use.
- **Rich checks.** `eq`, `ne`, `contains`, `match`, `gt`/`gte`/`lt`/`lte`, `len` and `exists` on any path of a step's result, including list items (`calls.0.status`).
- **Waiting for asynchronous results.** `retry: { timeout: 30s, every: 1s }` polls a step until its checks pass.
- **Isolated runs.** Every run gets a unique `${run}` id to put in the data it creates, so runs never collide on shared servers.
- **Automatic cleanup.** Inserted rows are deleted, started workflows are terminated, and mock state is reset, even when the test fails.

**Safety and output**
- **Values never become commands.** In any shell line, each `${...}` is one quoted word, so an answer from the system under test or a DSN with `&` can't run as shell syntax; encoders and decoders are argv commands with a timeout and size limit.
- **Guardrails on shared servers.** No DDL on any database, every write on a remote database must include the run id, and `k6` load refuses to run while any dep is remote unless you pass `--allow-remote-load`.
- **Machine-readable.** Every command supports `--json`, and exit codes are fixed: `0` ok, `1` failed, `2` bad input.
- **Readable failures.** Failed checks are reported in words, e.g. `json.status: got "WAITING", want "APPROVED"`.
- **Built-in docs.** `dmock help guide` works without the repo.

## Examples

Start here if you want to see how you'd use d-mock on your own service:

- **[`examples/checkout`](examples/checkout)**: a service that charges orders through a payment provider.
  - *"If the provider times out, do we charge twice?"*: a scenario breaks the provider for one order, then checks the DB, every call the provider received, and the logs.
  - *"Double click: charged twice?"*, *"MySQL down: 503 and recovery?"*, *"Is the PAID event published?"*, *"Every provider answer → the right status?"*: `parallel`, `stack`, `kafka consume` and `matrix` in action, with suite hooks and an `after_ready` migration.
  - *"Will it survive launch day inside its pod?"*: k6 load against the service in a container limited to 500m CPU and 128Mi. It checks p95, errors, OOM, and that every accepted checkout really got charged.
- **[`examples/encrypted`](examples/encrypted)**: a third-party API whose payloads are encrypted. A `codec` mock echoes the request inside an encrypted answer; scenarios check the service refuses an answer about another reference and a forged callback.
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

## Writing suites

A suite is a folder with a `dmock.yaml` (any name, anywhere, several per repo):

```
test/it/
├── dmock.yaml                   the stack: deps, mocks, services, hooks
├── mocks/<name>.yaml            fake third-party APIs
├── configs/<service>.tmpl.yaml  service configs with ${...} where addresses go
├── scenarios/                   one question per file, run in path order
├── steps/                       shared step lists for `include`
└── pod-limits/dmock.yaml        a nested suite (e.g. the service in a container)
```

Every field of every file is in **[docs/spec.md](docs/spec.md)** (`dmock help spec`); the working
loop and common mistakes in [docs/guide.md](docs/guide.md); patterns for faults, retries, races,
outages, events and load in [docs/examples.md](docs/examples.md).

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
| `dmock help spec` | [docs/spec.md](docs/spec.md): every YAML field: suite layout, `dmock.yaml`, hooks, mock specs, scenarios, every step |
| `dmock help examples` | [docs/examples.md](docs/examples.md): patterns for faults, retries, races, outages, events, table-driven cases and load inside pod limits |
| `dmock version` | the installed version |

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
| `parallel`, `stack`, `kafka` consume, `matrix`/`repeat`, `include`, hooks, `after_ready` | Tested end to end (`examples/checkout`: double submit, DB outage, order event, provider answers) |
| Mock `codec`, `http` `encode`/`decode`, shell quoting of references | Tested end to end (`examples/encrypted`) and with unit tests against a real `sh` |
| `k6` step | Tested end to end (load against a 0.5 CPU / 64 MiB container; OOM shown in the failing step) |
| `mockoon.runner: docker` | Not tested yet |
