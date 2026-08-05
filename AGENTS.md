# AGENTS.md

Obey these instructions. This file gives instructions and references only. For
descriptions of the design, read `docs/cartridge-runtime/design.md` and
`README.md`.

Written in ASD-STE100 Simplified Technical English.

---

## 1. Before you write code

1. Search for a helper that already exists. Use `grep` or `rg` on the full tree.
2. Read section 3. Find the package that owns the operation.
3. Call the owner package. Do not write your own copy.
4. If no owner package gives what you need, stop. Tell the user. Do not write
   the code in your own package.

---

## 2. Commands

Run these commands from the repository root.

| Command | Use it to |
|---|---|
| `make build` | Build the binary to `./bin/br` |
| `make sign` | Sign the binary. Do this before you start a VM |
| `make check` | Run format, vet, lint and test. Run this before each commit |
| `make test` | Run all tests |
| `make test-linux` | Run all tests on Linux in a container. Docker must be in operation |
| `make lint` | Run golangci-lint |
| `make security` | Run govulncheck and trivy |
| `make smoke-cartridge` | Test a cartridge on real hardware |
| `make smoke-holder` | Test that a holder stays alive after its parent stops |
| `make test-traps` | Test that the smoke cleanup traps fire on EXIT, INT, TERM and HUP. No hardware |
| `make clonedetect` | Find duplicated concepts across packages |
| `./scripts/mutation-test.sh` | Run gremlins mutation tests |

Rules:

1. Use `make test`. Do not use `go test` alone. The Makefile sets `GOCACHE`.
2. To run one test, use `go test ./internal/vm/ -run TestName -v`.
3. Sign the binary before you start a VM. An unsigned binary cannot use
   Virtualization.framework.
4. Run `make clonedetect` before you add a helper. Read section 3 first. The
   report shows the owner package for a rule that is already in the tree.
5. Run `make test-linux` before you push. CI runs the tests on Linux. A test
   that passes on macOS can fail there.

---

## 3. Owner packages

Each operation below has one owner package. Call the owner. Do not write a
second copy in a different package.

| Operation | Owner package | Do not |
|---|---|---|
| BSD device names | `internal/diskarb` | Do not read or cut a `/dev/` prefix in your own code |
| Atomic file writes | `internal/util.WriteFileAtomic` | Do not write your own temporary-file-and-rename code |
| Boot and shutdown stages | `internal/bootstage` | Do not write your own stage file |
| Liveness of an instance | `internal/instance` | Do not test for a socket file with `os.Stat` |
| Host port reservation | `internal/portalloc` | Do not call `net.Listen` for a host port |
| Cartridge DMG operations | `internal/cartridge` | Do not call `hdiutil` from another package |
| DiskArbitration | `internal/diskarb` | Do not add a second cgo bridge |
| VM lifecycle | `internal/vmhost` | Do not start a VM from `package main` |
| Paths and file names | `internal/config` | Do not write a path as a string literal |
| Outbound HTTP budgets | `internal/httpfetch` | Do not use `http.DefaultClient`, and do not build your own `http.Client` |

Two more rules:

1. Do not add a second lock design. Use `flock`, as `internal/cartridge` does.
2. Do not import cobra, systray or Cocoa into `internal/vmhost`. A holder
   process imports this package.

---

## 4. Platform rules

1. Put darwin code in a `*_darwin.go` file. Add the `//go:build darwin` tag.
2. Give each darwin file an `*_other.go` file. Give it the same exported names.
3. Make the `*_other.go` file return an unsupported error.
4. Build for both platforms. Run `go build ./...` and
   `GOOS=linux go build ./...`. Both must pass.
5. Keep `runtime.LockOSThread()` in `main.go`. Virtualization.framework needs
   the main thread.
6. Call `(*Host).Run` from the main thread only. Call `(*Host).Drain` from any
   goroutine.
7. Do not add a GitHub-hosted macOS runner (`macos-latest`, `macos-14`) to any
   workflow. They are ad-hoc, cost about ten times a Linux minute, and this
   project has watched one fail to allocate twice in a day — failing a required
   check for reasons that had nothing to do with the code.
8. Send macOS work to `stuffbucket/macos-builder` instead. It is a private repo
   that owns the only self-hosted macOS runner and every Apple signing secret;
   this repo holds neither and dispatches to it (`.github/workflows/macos-build.yml`).
   Baking a macOS guest image belongs there, because it needs
   Virtualization.framework on real hardware.
9. Do not move darwin build, test or lint work to a Linux runner. The VM and
   DiskArbitration bridges are cgo and need the macOS SDK to compile or
   analyse, so `GOOS=darwin` from Linux cannot cover them.

---

## 5. Code rules

1. Do not add a Go module dependency. Do not change `go.mod` or `go.sum`.
2. Wrap each error with `%w`.
3. Write a doc comment for each exported name.
4. Write a test for each exported function and method. Put the test in the
   external test package, for example `package cartridge_test`. The test must
   import the package and call the name. This covers the export behavior and
   the import behavior together: the name is reachable from outside, and it
   operates when a different package calls it.
5. An exported struct field is different. Do not write a call test for it.
   Write a round-trip test for the type that holds it: write the value out,
   read it back, and compare. A field with a `json:` tag is an on-disk or
   on-wire format that a different version or a different process reads, so the
   export behavior is the write and the import behavior is the read. Section
   9, point 3 tells you not to delete these fields; this rule tells you how to
   hold them.
6. An exported constant needs no test of its own. The test of the code that
   uses it is sufficient.
7. A comment that makes a claim about a different component is not sufficient.
   Write a test that holds the claim. Examples of this class of claim: what the
   guest emits, what the holder publishes, what the menubar reads, what
   `hdiutil` reports, what CI builds. A claim that no test holds becomes wrong
   in silence.
8. Use a named constant. Do not write a number in the code.
9. Keep the cyclomatic complexity of each function at 25 or less.
10. Read `.golangci.yml` before you disable a lint rule.
11. Correct the code first. If you must add a `//nolint` comment, write the
    reason on the same line. The `nolintlint` rule needs a reason.
12. Write American spellings. The `misspell` linter rejects "behaviour",
    "recognised" and their relatives.

New rules go at the END of this list. `.golangci.yml` cites these by number,
so inserting one silently repoints every citation below it.

---

## 6. Test rules

1. Write a test for each correction. Make the test fail first. Then write the
   correction.
2. Tell the user that you saw the test fail before your correction.
3. Use `t.TempDir()` for temporary directories. For a unix socket, use
   `os.MkdirTemp("", "short")`. A long path breaks the socket.
4. Put hardware tests behind `testing.Short()`.
5. Remove a test only if you delete the code that it tests. If a test fails,
   correct the code. Do not delete the test.

---

## 7. Git rules

1. Use conventional commits. Use `feat`, `fix`, `refactor`, `test`, `build`,
   `chore`, `docs`, `perf` or `ci`.
2. Keep the commit subject to 50 characters or fewer.
3. Do not write the name of an AI tool in a commit message.
4. Do not use an emoji in a commit message.
5. Do not use `--no-verify`. Correct the failure.
6. Do not run `git reset --hard`. Do not run `git checkout -- <path>`. Do not
   run `git clean -fd`. These commands delete work.
7. Run `git status` before each operation that changes a branch or the history.
8. If the tree has changes that you did not make, stop. Tell the user.

---

## 8. Data safety

These operations can delete the work of a user. Obey these rules.

1. Check that no VM is in operation before you delete a disk file.
2. Check that a disk image is not attached before you unlink it. Use
   `internal/cartridge`.
3. Ask the guest to stop with ACPI. Then wait for the stopped state. Do not
   call `vz.Stop()` first.
4. Sync a disk image to disk after the VM stops. Do this before you detach.
5. Do not delete an instance record that another process uses.

---

## 9. Code that looks dead but is live

Do not delete these. A static tool cannot see them.

1. Each function with an `//export` comment. C code calls it. Look in
   `internal/diskarb` and `cmd/bladerunner/settings_window_darwin.go`.
2. Each name that only an `*_other.go` file needs. It keeps the Linux build
   correct.
3. Each field that the code writes to a JSON file on disk. A later version
   reads it. `instance.Entry.ProtocolVersion` is an example.
4. Each name that only a test uses. Delete the test first, or keep both.

To find dead code, install the tool. Then run the two commands and compare the
results.

```
go install golang.org/x/tools/cmd/deadcode@latest

deadcode ./...        # the program cannot reach these
deadcode -test ./...  # the tests cannot reach these either
```

The difference between the two lists gives you the names that only a test uses.

---

## 10. Stop and tell the user

Stop your work and tell the user in these conditions:

1. You need a shared part that no owner package in section 3 gives.
2. You must change a file that another agent owns.
3. A test fails and you cannot correct it in your own files.
4. You find a second copy of an operation, and the two copies do not agree.
5. An operation can delete the work of a user.
6. `make check` fails and the cause is outside your files.
