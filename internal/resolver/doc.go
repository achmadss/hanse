// Package resolver chooses the executor for each operation request.
//
// It applies the precedence of docs/architecture.md section 5: the executor
// named in the request, then the operation's default_executor, then the
// capability's default_executor, and otherwise it rejects the request. The
// registry joins routes.yaml, so an operation's OperationRouting already
// carries its default with the capability default folded in: a
// Route.DefaultExecutor that is set there is used as the default, whether
// routes.yaml set it at operation or capability level.
//
// A chosen executor must be in the operation's eligible executor list and
// must be enabled and online. Resolution never falls back to another node:
// when the chosen executor is ineligible, disabled, or offline, the request
// fails with the error code of docs/architecture.md section 14.
package resolver
