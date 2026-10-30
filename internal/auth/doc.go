// Package auth authenticates API requests with bearer tokens and
// applies per-token permissions. It owns the tokens file that
// `hanse-gateway token` manages: the file stores hashes of the tokens
// only, is written with 0600 permissions, and is compared in constant
// time. It also holds the transport gate that refuses plain HTTP on a
// non-loopback address without allow_insecure_http, and the TLS
// configuration builder. The API package wires the middleware in (#16).
package auth
