// clonedetect is a separate module on purpose.
//
// A nested module is invisible to the parent module's "./..." package pattern,
// so `go build ./...`, `go vet ./...`, `go test ./...` and `golangci-lint run`
// in the repository root never load it. The Makefile's `fmt`/`fmt-check`
// targets walk the tree with `find`, so the sources here are still held to
// gofmt. The tool needs no dependency beyond the standard library, so the
// nested module adds no go.sum and no supply-chain surface.
module bladerunner.local/clonedetect

go 1.25
