# Agent instructions

## Commit messages

- The first line is a short summary of what the commit does, in a few words
  (aim for 50 characters, never more than 72), in the imperative mood:
  "Add berthy key rotate", not a full explanation.
- Leave a blank line after it.
- Put the details in the body below: why the change was made, how it works,
  what proves it. The body can be as long as it needs to be.

## Before and after a push

- CI runs the tests on Linux, macOS and Windows. Run `go test ./...` on Windows
  as well as Linux before pushing.
- `gofmt` covers `third_party/` too, because CI checks it.
- `gofmt -l` on a Windows checkout lists nearly every file, because of CRLF
  line endings. Check formatting in WSL or trust CI's check.
- After pushing, watch the CI run until it is green. A red `main` comes before
  any other work.
- Push only after reading the test and e2e results yourself. A command that
  prints the log and then pushes, such as `run.sh | tee log && git push`,
  pushes a failure. An e2e scenario that fails only sometimes is a finding,
  not noise.
- A push to `main` cancels the CI run still in progress, so the earlier
  commit is tested only by the later run. A push can also start no run at all;
  start one with `gh workflow run ci.yml --ref main`.

## Tests

- A test never touches the user's real Berthy config or the real OS credential
  store. Pass `--config` or set `BERTHY_CONFIG` to a temp file.
  `agentclient.TokenStore` defaults to `credstore.None()`, the e2e harness
  exports `BERTHY_TOKEN_STORE=none`, and the one test that uses the real store
  runs only with `BERTHY_TEST_OS_STORE=1`, under a random service name it
  deletes.
- A test that starts an operation waits for it before closing the store. A
  goroutine that outlives a request belongs to the controller, which drains it
  on shutdown.
- Write the failing test first, and check that it fails without the change.
- A test never races the real clock or a tight deadline: no schedule that
  fires every minute, no deadline of a few seconds for work a loaded CI runner
  does, no sleep. Wait for the event itself.
- Code that gives up after a timeout is tested inside `testing/synctest`: the
  timer is fake, and `synctest.Wait()` replaces waiting long enough. A 1 ms
  timer is still the real clock.
- A fixture's cleanup never waits for an event the code under test might skip.
  Release it unconditionally and assert the event in the test body; otherwise
  a regression hangs the package until the 10-minute `go test` timeout instead
  of failing.
- A flaky test is a bug until shown otherwise. Find the cause instead of
  rerunning it or adding a sleep; several flakes here were real bugs. Run the
  tests with `-race` in WSL, because some races only show there.
- Windows and macOS file systems ignore case; Linux does not. A lookup that
  opens a file by a name the user typed, such as a time zone, can pass there
  and fail on Linux. Check the spelling against the canonical name instead.

## End-to-end harness (`test/e2e/`)

- `test/e2e/run.sh [--no-build] [--keep] [scenario...]` runs the scenarios
  against a real Swarm. A new user-facing feature gets a scenario.
- Scenarios build names from `$PROJECT` and `$ENVN`, never a literal `e2e`, so
  two runs with their own names, registry and ports can share one daemon.
- Refuse existing Docker fixture names and clean up only resources the current
  run can prove it created, even when setup fails partway through. Use a per-run
  ownership marker when a resource name can collide. Scenario 41 checks its
  volume's label before removal.
- The harness builds the working tree. To check committed work while someone
  else edits the checkout, run it from a clean clone of HEAD.

## Design rules

- The CLI runs on Windows, macOS and Linux, and the agent runs on Linux.
  Anything passed between them, such as a manifest path, must mean the same on
  every OS: relative, forward slashes, no drive letter, no `..`.
- PowerShell ends a string it pipes to a native program with CRLF. Input read
  from standard input, such as a token or a secret, accepts one trailing LF or
  CRLF and refuses any other line break.
- A plan must come out byte-identical when it is re-planned at approval time.
  Name services and image digests, never container ids.
  Planning and execution must interpret the same request identically. Reject
  ambiguous inputs such as repeated JSON fields that affect the action.
  Preserve decision fields when redacting metadata so approval comparison
  can detect a changed outcome.
- Each manifest key is defined once, in `internal/manifest/keys.go`. The guide
  reference, `berthy docs` and the JSON Schema are generated from it with
  `go generate`, and a test fails if they drift.
- A Docker call that waits on the server, such as a stop with a grace period,
  needs a deadline longer than that wait, not the client's short default.
- Require a complete, valid response before treating a resource or secret as
  absent. A Docker volume list with warnings cannot establish absence. The
  secret guide needs one decision per declared secret and a valid inventory
  before it prompts.
- `third_party/` holds vendored packages with patches (for example, `xid`
  reads the machine id lazily so startup never searches PATH). Don't replace
  them with upstream copies.
- A new field on `deploy.Request` goes in three places: the struct, the
  request hash in `Admit` (the same request id with another value is a
  conflict), and the allowlist in `state.publicJSON`. Without the last one,
  every request with the field is refused as an invalid admission.
- The API shows an operation's error only when it is a fixed text or
  `deploy.PublicMessage` accepts it; anything else becomes "operation failed".
  A refusal with variable text, such as an app name or digest, needs a strict
  pattern there.
- A new state column needs a numbered schema step that alters an existing
  database, not only the schema for a new one; otherwise an agent upgraded in
  place cannot open its state. Two branches that both bump the schema version
  renumber when one rebases onto the other.
- Swarm replica slots are not always 1..N: after a service is scaled by hand, a
  single replica can keep slot 2. Count healthy replicas, not slot numbers.
- A reader never returns `(0, nil)` at the end of its input, which makes its
  caller loop forever. A stream cut inside a frame ends with
  `io.ErrUnexpectedEOF`.
- A helper container whose output holds backup data or secrets sets its log
  driver to `none`, so Docker does not keep a copy of that output on disk.
- Released CLIs and agents refuse unknown manifest keys. A new key can't
  appear in berthy-examples or in instructions for released versions until a
  release ships it.
- A token the CLI finds in the OS credential store is the person's admin
  token. It is filed under the configuration entry's own `token_env`, and
  `berthy mcp` never reads it: the agent's approval gate holds only tokens
  marked `agent`, so an MCP server or a script's entry that fell back to it
  would bypass approvals.
- Redact the active bearer token and any entered secret before displaying
  agent or registry diagnostics, including preflight errors. Exercise
  version preflight with a release-stamped CLI because dev builds skip it.

## Docs

- `docs/user-guide.md` repeats exact CLI output, such as the next steps that
  `berthy setup` prints. When a message changes, search `docs/` for its old
  text.
- berthy-web builds its Docs pages from `docs/user-guide.md` and
  `docs/spec-v1.md` on `main` every day, one page per heading. Renaming or
  renumbering a `##` or `###` heading, or an anchor a link points to, breaks
  the site build until berthy-web's split map is updated.
