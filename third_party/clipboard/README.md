# github.com/atotto/clipboard (local copy)

This is `github.com/atotto/clipboard` v0.1.4, licensed under BSD 3-clause (see `LICENSE` beside this file). The `go.mod` in the repository root replaces the upstream module with this directory, so every package that imports the clipboard, including bubbles and huh, gets this copy.

The only changes from upstream are these:

- In `clipboard_unix.go`, the search for the clipboard tools moved from `init` to the first `ReadAll` or `WriteAll`, which runs it once through `sync.Once`. The tool chosen is the same.
- In `clipboard.go`, the comment on `Unsupported` says when it is set.

The `cmd/` directory and the upstream tests were not copied.

Why: upstream searches PATH for the clipboard tools in a package `init`, so every program that links the package does the search before `main` runs. On WSL the PATH has 95 entries, most of them slow Windows directories, and that search took about 1.9 s. Every `berthy` command, even `berthy version`, took about 2 s to start. No upstream release fixes this: v0.1.4 is the latest.

What guards it: `TestStartupDoesNotSearchPATH` in `cmd/berthy` fails if anything linked into `berthy` searches PATH before `main` runs.

When to delete it: when upstream stops searching PATH in `init`, or when bubbles and huh stop importing the clipboard. Then delete this directory and the `replace` line in `go.mod`, run `go mod tidy`, and run the test: it says whether that was right.
