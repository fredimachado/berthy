# github.com/rs/xid (local copy)

This is `github.com/rs/xid` v1.6.0, licensed under MIT (see `LICENSE` beside this file). The `go.mod` in the repository root replaces the upstream module with this directory, so every package that imports it, including minio-go, gets this copy.

The only change from upstream is this:

- In `id.go`, the machine ID is worked out at the first `New` or `NewWithTime`, once through `sync.Once`, not in a package variable initializer. The ID is the same.

The `.github/`, `b/` and test files were not copied.

Why: on macOS the upstream machine ID comes from `ioreg`, which `readPlatformMachineID` looks up in PATH (`exec.LookPath`) and runs as a process. Because that happens in the package initializer, every `berthy` command did it before `main` ran, whether or not it used an object store. On a PATH with 300 entries, that is 300 lookups; on any PATH it is a process start. berthy uses xid only through minio-go, only to name a replication rule.

What guards it: `TestStartupDoesNotSearchPATH` in `cmd/berthy` fails if anything linked into `berthy` searches PATH before `main` runs. It failed on macOS (`github.com/rs/xid allocates 1196 more at start with a 300-entry PATH than with one entry`) and not on Linux, where the machine ID is read from `/etc/machine-id`.

When to delete it: when upstream stops looking for `ioreg` in an initializer, or when minio-go stops using xid. Then delete this directory and the `replace` line in `go.mod`, run `go mod tidy`, and run the test: it says whether that was right.
