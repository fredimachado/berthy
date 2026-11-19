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

## Tests

- A test never touches the user's real Berthy config. Pass `--config` or set
  `BERTHY_CONFIG` to a temp file.
- A test that starts an operation waits for it before closing the store. A
  goroutine that outlives a request belongs to the controller, which drains it
  on shutdown.
- Write the failing test first, and check that it fails without the change.

## End-to-end harness (`test/e2e/`)

- `test/e2e/run.sh [--no-build] [--keep] [scenario...]` runs the scenarios
  against a real Swarm. A new user-facing feature gets a scenario.
- Scenarios build names from `$PROJECT` and `$ENVN`, never a literal `e2e`, so
  two runs with their own names, registry and ports can share one daemon.
- The harness builds the working tree. To check committed work while someone
  else edits the checkout, run it from a clean clone of HEAD.

## Design rules

- The CLI runs on Windows, macOS and Linux, and the agent runs on Linux.
  Anything passed between them, such as a manifest path, must mean the same on
  every OS: relative, forward slashes, no drive letter, no `..`.
- A plan must come out byte-identical when it is re-planned at approval time.
  Name services and image digests, never container ids.
- Each manifest key is defined once, in `internal/manifest/keys.go`. The guide
  reference, `berthy docs` and the JSON Schema are generated from it with
  `go generate`, and a test fails if they drift.
- A Docker call that waits on the server, such as a stop with a grace period,
  needs a deadline longer than that wait, not the client's short default.
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
- Released CLIs and agents refuse unknown manifest keys. A new key can't
  appear in berthy-examples or in instructions for released versions until a
  release ships it.

## Docs

- `docs/user-guide.md` repeats exact CLI output, such as the next steps that
  `berthy setup` prints. When a message changes, search `docs/` for its old
  text.
- berthy-web builds its Docs pages from `docs/user-guide.md` and
  `docs/spec-v1.md` on `main` every day, one page per heading. Renaming or
  renumbering a `##` or `###` heading, or an anchor a link points to, breaks
  the site build until berthy-web's split map is updated.
