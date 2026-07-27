# CLAUDE.md

Obey these instructions. This file gives instructions only. For descriptions of
the design, read `docs/cartridge-runtime/design.md` and `README.md`.

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
| `make lint` | Run golangci-lint |
| `make security` | Run govulncheck and trivy |
| `make smoke-cartridge` | Test a cartridge on real hardware |
| `make smoke-holder` | Test that a holder stays alive after its parent stops |
| `./scripts/mutation-test.sh` | Run gremlins mutation tests |

Rules:

1. Use `make test`. Do not use `go test` alone. The Makefile sets `GOCACHE`.
2. To run one test, use `go test ./internal/vm/ -run TestName -v`.
3. Sign the binary before you start a VM. An unsigned binary cannot use
   Virtualization.framework.

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

---

## 5. Code rules

1. Do not add a Go module dependency. Do not change `go.mod` or `go.sum`.
2. Wrap each error with `%w`.
3. Write a doc comment for each exported name.
4. Use a named constant. Do not write a number in the code.
5. Keep the cyclomatic complexity of each function at 25 or less.
6. Read `.golangci.yml` before you disable a lint rule.
7. Correct the code first. If you must add a `//nolint` comment, write the
   reason on the same line. The `nolintlint` rule needs a reason.

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
