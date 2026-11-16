# d-mock guide

How to use `dmock` day to day. Run `dmock help guide` to print this from the binary alone; `dmock help` lists the other topics.

## What dmock does, in one paragraph

`dmock` runs integration tests on your machine or in CI. A **suite** is a folder with a `dmock.yaml`, which describes what has to be running:

- **deps:** databases, brokers and the like. Each is either a server that already exists (`remote`) or a container dmock starts.
- **mocks:** fake third-party APIs, served by Mockoon.
- **services:** the code under test, run as local processes or as containers with CPU/memory limits.

A `scenario.yaml` is a test: a list of steps (call an API, change a mock, send a Kafka message, start a Temporal workflow, query the DB, read a log), each followed by checks. dmock runs the steps, reports which checks failed and why, then deletes what the test created.

## Suite layout

A suite is a folder with `dmock.yaml`, `mocks/`, `configs/`, `scenarios/` (layout and every field:
`dmock help spec`).

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
dmock --json mock calls payments --key ID # 6. on failure, see what the service actually sent the mock
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
  - name: arrange — the API will approve
    mock: { name: payments, set: { key: "${vars.id}", state: { status: APPROVED } } }
  - name: act — create the order
    http: { method: POST, url: "http://localhost:8080/orders", json: { id: "${vars.id}" } }
    expect: { status: 201 }
  - name: assert — eventually approved
    http: { url: "http://localhost:8080/orders/${vars.id}" }
    retry: { timeout: 30s, every: 1s }
    expect: { json.status: APPROVED }
  - name: assert — the API was called exactly once
    mock: { name: payments, calls: { route: status, key: "${vars.id}" } }
    expect: { count: 1 }
```

More help, all built into the binary:

- `dmock help spec`: every YAML field: the suite layout, `dmock.yaml`, hooks, mock specs, scenarios, and every step's arguments and result.
- `dmock help examples`: patterns for the common questions (faults, retries, races, load inside pod limits).

## Mistakes that look like bugs but are not

These came up in real use. Check them before you blame the code under test.

1. **The mock never changes state.** The mock picks its state key from the request (`key:` in the mock spec), and that may not be the id you think. A real case: the test set state on the internal id `int-…`, but the service sent the external reference `EXT-…`. Run `dmock mock calls NAME` and look at `path`/`body` to see what was really sent, then set the state on that key.
2. **A check for "nothing happened" passes too early.** `retry` only repeats a step while it fails. A check like "status is still 5" passes at once, even if the thing that could change it hasn't run yet. Put a `sleep` longer than that delay before the check.
3. **A log check finds nothing.** Services often log enums by name (`SIGN_CONTRACT`), not by number (`5`). Grep the log by hand first (`dmock logs svc --grep ID`), then copy the exact text into the pattern.
4. **`unresolved reference ${env.X}`.** dmock never fills in defaults. Export the variable before `up` and `run`.
5. **Two workers on the same Temporal task queue split the work.** Only one of them should be up (`dmock up --only worker-fast`).
6. **A container service can't reach a mock or dep.** Inside a container, `localhost` is the container. Use `${host}` in its config template.
7. **A load test is refused.** `k6` won't run while a dep is `remote`, to protect shared servers. Switch deps to containers (`--mode db=container`) or pass `--allow-remote-load` if you mean it.
8. **An `exec` step passed although the command failed.** `exec` passes on any exit code unless you `expect: { exit: 0 }`. Its `stdout` keeps the trailing newline, so `save: { x: stdout }` breaks a later shell line: print with `printf`, or print JSON and save `json.x`.
9. **A `log` step can't see `up` output.** It reads only lines written since the scenario started. Check what `after_ready` printed with `dmock logs NAME`.
10. **A race test passes and proves nothing.** Two sequential requests aren't a race: use `parallel` so they arrive together, and `repeat` the scenario a few times. Check `count.<field>.<value>`, not `results.0`, since either branch may win.
11. **A step after an outage fails.** After `stack: { start: db }` the service may need a moment to reconnect: put `retry` on the next check. After the run, every part a `stack` step touched is put back as it was before the scenario: started again if it was up, stopped if the scenario started it.
12. **The payload is encoded or encrypted, so you reached for a raw Mockoon environment or a hand-written fake server.** Don't: you would lose `mock set`, `fault`, `calls` and cleanup. Keep it a d-mock mock:
    - fixed answers per state: `body_file` + `encode` (run once at start), one `variants` entry per state;
    - answers that must echo the request (an id, a signature over its nonce, a fresh timestamp), or requests you need to read: a `codec` with `decode`/`encode` commands, run per request.
    Your scenario talks encrypted too: `http` with `encode` (request) and `decode` (response).
13. **Per-entity state never matches on an encrypted request.** `key: "bodyRaw 'id'"` can't read a field out of ciphertext, so the key never equals your id and `mock set` on it never applies. Add a `codec` with `decode` (then `bodyRaw` reads the decrypted body), or key on a readable part (URL param, query, header). With no readable id at all, leave `key` out: every request shares one state, so run the scenarios that use that mock one at a time.
14. **`lint` warns that a service runs Mockoon.** A fake API started as a service `cmd` has none of d-mock's mock features. Move it under `mocks:` with a spec in `mocks/`.
15. **`after: "${matrix.delay}"` fails lint.** `after`, `retry.timeout` and `retry.every` are fixed durations, read before references are filled in. To vary timing per case, use a `sleep` step (it takes references) or one scenario per timing.
16. **`sql` refuses a write.** On a `remote` dep, every write must contain the run id. That's on purpose: a test may only touch rows it created. Seed with `sql.insert` (its rows are deleted automatically) or with an `exec` step that calls your seed tool.

## Rules dmock enforces (don't try to work around them)

- Suite files are code (they run commands); values are data. In a shell line (`exec` `cmd` as a string, a string `after_ready`, any `sh -c` script) every `${...}` becomes exactly one literal word, quoted for where it sits, so a value (a DSN with `&`, an answer from the system under test) can never run as shell syntax. A reference can't splice in extra flags; write them in the line, or use an argv list.
- `encode`, `decode` and `codec` commands are argv lists, never shell lines: a payload passes through them on stdin. They run from the suite (or spec) folder, with a 10 s timeout and a 16 MiB output limit.

- No DDL (`CREATE/ALTER/DROP/TRUNCATE/...`) in `sql` steps. Schemas belong in the dep's `after_ready` (your migration tool).
- Writes on remote databases must include the run id (in the statement or args, not in a comment). Anything that isn't a plain read counts as a write, including `CALL` and `SET`. One statement per step.
- A suite's `.dmock/` belongs to that suite. Two stack files in one folder (the old layout) share it, so only one can be up at a time; d-mock refuses to act on another project's running state. Use nested suites instead.
- `mock set` / `mock fault` (CLI and steps) reject a state field or fault the spec doesn't declare, and unknown CLI flags are an error, so typos fail loudly.
- `down` never touches remote deps.
- After every run, dmock puts back what `stack` steps changed (as it was before the scenario), deletes the rows it inserted, terminates the workflows it started and resets the mock state it set. Pass `--keep` only to inspect a failure, then clean up by hand.
