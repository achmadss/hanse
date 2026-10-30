// Package herrors defines the one shared set of error codes used by the
// Gateway API, the runtimes, and the CLI, as described in
// docs/architecture.md §14. Each code has a stable snake_case wire value,
// a fixed HTTP status, and a fixed CLI exit code. Errors never contain
// secrets: every encoding path redacts registered secret values and the
// values of registered secret detail keys.
//
// The Go type is named herrors (the directory stays internal/errors) so the
// code can sit next to the standard library's errors package.
package herrors
