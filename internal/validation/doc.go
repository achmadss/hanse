// Package validation checks request arguments against an operation's
// compiled JSON Schema and applies the Gateway's own `hanse:node` rules.
//
// The Gateway validates every submit and dry-run request here, after the
// capability and operation lookup and before executor resolution
// (docs/architecture.md section 4). A failure is one *herrors.Error with
// the invalid_arguments code, whose details hold one entry per problem:
// the JSON pointer of the offending value, the rule that failed, and a
// message for people.
//
// Messages never repeat an argument value, so a secret argument value
// cannot reach an error response, a log line, or an operation record.
package validation
