# d-mock guide

How to use `dmock` day to day. Run `dmock help guide` to print this from the binary alone; `dmock help` lists the other topics.

## What dmock does, in one paragraph

`dmock` runs integration tests on your machine or in CI. A **suite** is a folder with a `dmock.yaml`, which describes what has to be running:

- **deps:** databases, brokers and the like. Each is either a server that already exists (`remote`) or a container dmock starts.
- **mocks:** fake partner APIs, served by Mockoon.
- **services:** the code under test, run as local processes or as containers with CPU/memory limits.

A `scenario.yaml` is a test: a list of steps (call an API, change a mock, send a Kafka message, start a Temporal workflow, query the DB, read a log), each followed by checks. dmock runs the steps, reports which checks failed and why, then deletes what the test created.

## Suite layout

```
<any-name>/
├── dmock.yaml                   the stack: project, deps, mocks, services
├── mocks/<name>.yaml            one per third-party API (mocks.<name> needs only a port)
├── configs/<service>.tmpl.yaml  service config templates, picked up by name
├── scenarios/**/*.yaml          one question per file, run in path order (01_, 02_ …)
├── load/                        k6 scripts
├── <variant>/dmock.yaml         a nested suite (e.g. the same service in a container), own .dmock/
└── .dmock/                      generated; gitignore it
```

- `dmock init DIR` writes a starter suite that passes `dmock test DIR` as-is.
- Name a scenario after the question it answers (`02_provider_down_gives_up_after_3.yaml`). Start with the incident you never want again.
- Every command finds its suite by walking up from `-f`, the paths it was given, or the current folder, to the nearest `dmock.yaml`. So `dmock run it/scenarios/payments/01_timeout.yaml` works from anywhere.
- CI: `dmock test it --junit report.xml` (up, every scenario, always down; exit 0 only when all pass).

## The loop

Inside the suite folder (or pass `-f SUITE`, or a path to `run`):

```bash
dmock --json status                      # 1. what is running?
dmock --json up                          # 2. start what is missing (safe to repeat)
dmock --json lint                        # 3. catch YAML mistakes before running
dmock --json run scenarios/x.yaml        # 4. run; exit 0 = pass, 1 = a check failed, 2 = bad input
dmock --json logs worker --grep 'ERROR|panic' --tail 50   # 5. on failure, read the service log
dmock --json mock calls partner --key ID # 6. on failure, see what the service actually sent the mock
dmock --json down                        # 7. stop everything (remote deps are never touched)
```

- **Always pass `--json`.** Every command prints exactly one JSON document on stdout:
  - Success: `{"ok": true, ...}`.
  - Failure: `{"ok": false, "error": "...", "exit_code": N}`.
- **Exit codes:**
  - `0`: ok.
  - `1`: a scenario check failed, or a component failed to start.
  - `2`: you gave dmock bad input (YAML, flags, a missing `${env.X}`). Fix the input; don't retry.

## Reading a run report

`run --json` returns `reports[].steps[]`. For each step:

| Field | Meaning |
|---|---|
| `passed` | whether every check passed |
| `failures` | each failed check in words: `json.status: got "WAITING", want "APPROVED"` |
| `error` | the step itself could not run (connection refused, bad SQL, unknown mock…) |
| `attempts` | how many times a `retry` step ran |
| `result` | what the step returned; the paths `expect` can check |
| `skipped` | an earlier step failed, so this one did not run |

`cleanup[]` lists what was deleted or reset after the run.

## Writing a scenario: template

```yaml
name: short description of the behaviour under test
vars:
  id: "order-${run}"                 # ${run} is unique per run: use it in every id you create
steps:
  - name: arrange — partner will approve
    mock: { name: partner, set: { key: "${vars.id}", state: { status: APPROVED } } }
  - name: act — create the order
    http: { method: POST, url: "http://localhost:8080/orders", json: { id: "${vars.id}" } }
    expect: { status: 201 }
  - name: assert — eventually approved
    http: { url: "http://localhost:8080/orders/${vars.id}" }
    retry: { timeout: 30s, every: 1s }
    expect: { json.status: APPROVED }
  - name: assert — partner called exactly once
    mock: { name: partner, calls: { route: status, key: "${vars.id}" } }
    expect: { count: 1 }
```

More help, all built into the binary:

- `dmock help suite`: the suite layout, every `dmock.yaml` field, and the mock spec format.
- `dmock help steps`: every step type, its arguments, its result fields, and an example.
- `dmock help examples`: patterns for the common questions (faults, retries, races, load inside pod limits).

## Mistakes that look like bugs but are not

These came up in real use. Check them before you blame the code under test.

1. **The mock never changes state.** The mock picks its state key from the request (`key:` in the mock spec), and that may not be the id you think. A real case: the loan's partner contract id was `pcid-…`, but the connector sent the contract number `PCN-…`. Run `dmock mock calls NAME` and look at `path`/`body` to see what was really sent, then set the state on that key.
2. **A check for "nothing happened" passes too early.** `retry` only repeats a step while it fails. A check like "status is still 5" passes at once, even if the thing that could change it hasn't run yet. Put a `sleep` longer than that delay before the check.
3. **A log check finds nothing.** Services often log enums by name (`SIGN_CONTRACT`), not by number (`5`). Grep the log by hand first (`dmock logs svc --grep ID`), then copy the exact text into the pattern.
4. **`unresolved reference ${env.X}`.** dmock never fills in defaults. Export the variable before `up` and `run`.
5. **Two workers on the same Temporal task queue split the work.** Only one of them should be up (`dmock up --only worker-fast`).
6. **A container service can't reach a mock or dep.** Inside a container, `localhost` is the container. Use `${host}` in its config template.
7. **A load test is refused.** `k6` won't run while a dep is `remote`, to protect shared servers. Switch deps to containers (`--mode db=container`) or pass `--allow-remote-load` if you mean it.
8. **`sql` refuses a write.** On a `remote` dep, every write must contain the run id. That's on purpose: a test may only touch rows it created. Seed with `sql.insert` (its rows are deleted automatically) or with an `exec` step that calls your seed tool.

## Rules dmock enforces (don't try to work around them)

- No DDL (`CREATE/ALTER/DROP/TRUNCATE/...`) on any database.
- Writes on remote databases must include the run id (in the statement or args, not in a comment). Anything that isn't a plain read counts as a write, including `CALL` and `SET`. One statement per step.
- A suite's `.dmock/` belongs to that suite. Two stack files in one folder (the old layout) share it, so only one can be up at a time; d-mock refuses to act on another project's running state. Use nested suites instead.
- `mock set` / `mock fault` (CLI and steps) reject a state field or fault the spec doesn't declare, and unknown CLI flags are an error, so typos fail loudly.
- `down` never touches remote deps.
- After every run, dmock deletes the rows it inserted, terminates the workflows it started and resets the mock state it set. Pass `--keep` only to inspect a failure, then clean up by hand.
