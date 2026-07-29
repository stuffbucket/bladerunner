package util

import (
	"encoding/json"
	"fmt"
	"io/fs"
)

// jsonIndent is the indentation every JSON document this package publishes is
// written with. It is one constant rather than a literal at each call site
// because the files are meant to be read with `cat`, and four hand-rolled
// copies of `"  "` is four chances for one of them to drift.
const jsonIndent = "  "

// WriteJSONAtomic encodes v as indented JSON and publishes it to path
// atomically, with the given mode.
//
// It exists because four packages independently wrote the same three steps —
// json.MarshalIndent, then WriteFileAtomic, then wrap the error — for files
// that are all read by a person or by another process: the disk manifest, the
// cartridge manifest, the startup report and the runtime metadata. Sharing the
// sequence keeps the indent and the atomicity guarantee in one place; a caller
// that spells it out again can quietly lose either.
//
// Marshaling failure leaves the destination untouched and creates nothing, so
// a value that cannot be encoded cannot destroy a good file. Beyond that the
// durability guarantee is WriteFileAtomic's: a concurrent reader sees either
// the previous document or the new one, never a partial file.
//
// The error is returned unwrapped by domain. Callers add their own context
// ("write cartridge manifest %s"), because the caller knows which file this is
// and this function does not.
func WriteJSONAtomic(path string, v any, perm fs.FileMode) error {
	b, err := json.MarshalIndent(v, "", jsonIndent)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	return WriteFileAtomic(path, b, perm)
}
