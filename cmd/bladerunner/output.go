package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// jsonOutput is bound to the global --json persistent flag (see root.go). When
// true, commands emit machine-readable JSON instead of human-formatted output
// so that agents and scripts can consume them.
var jsonOutput bool

// jsonFieldStatus is the common "status" key used across command JSON results.
const jsonFieldStatus = "status"

// emitJSON writes v to stdout as indented JSON followed by a newline. Commands
// gather their result into a struct or map and call this when jsonOutput is set.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emitJSONError writes a structured error object to stdout so JSON consumers
// always get parseable output. The command should still return the error so the
// process exits non-zero.
func emitJSONError(err error) {
	_ = emitJSON(map[string]string{"error": err.Error()})
}

// rejectJSONForInteractive returns a non-nil error when --json is set on an
// interactive/streaming command (shell, exec, incus, events, logs) that cannot
// produce a structured envelope. It routes the message through emitJSONError so
// agents still get parseable output, then hands the error back for the RunE to
// return (so the process exits non-zero). Returns nil when --json is unset, so
// callers can guard with a single `if err := ...; err != nil { return err }`.
//
// instead names the command that DOES answer the question in JSON, when a
// better answer exists than the general one. `br ssh --json` is the case that
// forced it: it used to print SSH details and now opens a shell, so anyone
// scripting it needs pointing at `br ssh-config --json` rather than at status.
func rejectJSONForInteractive(name string, instead ...string) error {
	if !jsonOutput {
		return nil
	}
	alternatives := "'br status --json' or 'br ls --json'"
	if len(instead) > 0 {
		alternatives = "'" + strings.Join(instead, "' or '") + "'"
	}
	err := fmt.Errorf("--json is not supported for the interactive %q command; use %s for machine-readable state", name, alternatives)
	emitJSONError(err)
	return err
}
