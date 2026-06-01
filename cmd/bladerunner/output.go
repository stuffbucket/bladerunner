package main

import (
	"encoding/json"
	"os"
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
