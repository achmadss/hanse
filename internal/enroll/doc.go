// Package enroll issues one-time join tokens and node credentials for the
// agent protocol (docs/agent-protocol.md §2). A join token enrolls one node
// once and expires; the node then authenticates with a credential that
// belongs to that node's ID alone.
//
// The store keeps only the SHA-256 hash of each value and compares hashes in
// constant time (docs/architecture.md §15). A plain value exists only in the
// response of the call that created it, so it can be printed or written
// exactly once.
package enroll
