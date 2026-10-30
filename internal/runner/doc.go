// Package runner runs one exec handler call as docs/handler-contract.md
// section 4 defines it. The Gateway's embedded agent and the node agent use
// the same runner, so a handler behaves the same on every executor.
//
// The runner starts the entry command as an argument list, never through a
// shell. It writes the request envelope to stdin, reads the response
// envelope from stdout with a size limit, and passes every stderr line to a
// log callback as the line ends. It enforces the call's deadline, carries
// out cancellation as section 7 defines it, creates and removes
// HANSE_WORK_DIR, and maps exit codes and malformed output to the shared
// error codes of section 5.
package runner
