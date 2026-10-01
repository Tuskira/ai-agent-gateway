// This is a separate Go module from the gateway (../../go.mod) on purpose:
// it exists only so `make notices` has somewhere to live as real, type-checked
// Go code instead of a shell script, without adding anything to the gateway's
// own go.mod/go.sum. It has no third-party dependencies of its own -- it
// shells out to `go list` and to `go run github.com/google/go-licenses/v2@...`
// (itself isolated the same way) rather than importing either as a library.
module github.com/Tuskira/tusk-ai-secured-gateway/tools/notices

go 1.27
