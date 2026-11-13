# d-mock suites reference

How a suite is laid out, how d-mock finds it, and every field of `dmock.yaml` and of a mock
spec. Print it with `dmock help suite`. Steps are in `dmock help steps`.

## Layout

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
