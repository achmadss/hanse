// Package bundle implements the handler delivery of
// docs/agent-protocol.md section 5: the content hash of a capability
// directory, the safe unpack of a downloaded archive, and the
// content-addressed cache that a node agent keeps between runs.
//
// The Gateway computes the same hash before it serves a bundle, so both
// sides share this package rather than keeping two copies of the
// algorithm.
package bundle
