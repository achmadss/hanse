// Package httprt runs http handlers, docs/handler-contract.md section 9.
// The Gateway sends the request envelope as a JSON POST to the handler's
// URL and reads the response envelope from the body. The Gateway makes the
// call itself, so the executor of an http operation is always the Gateway
// host node.
//
// The runtime speaks both call kinds of the handler contract: operation
// runs and option providers. It verifies the server's TLS certificate
// unless the configured CA file adds another trusted root, it never
// follows redirects, and it maps unexpected HTTP statuses and malformed
// bodies to the shared herrors codes.
package httprt
