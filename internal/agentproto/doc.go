// Package agentproto defines the hanse.agent/v1 messages that the Gateway
// and the node agent exchange over one WebSocket connection
// (docs/agent-protocol.md section 4). It holds the message types, their
// JSON encoding, and the validation the protocol requires; it holds no
// transport code.
//
// Every message is one JSON object in one WebSocket text frame. Messages
// allow fields this version does not know, so a later v1 release can add
// one without breaking an older peer (docs/agent-protocol.md section 12).
package agentproto
