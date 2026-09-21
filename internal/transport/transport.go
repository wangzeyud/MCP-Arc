package transport

import "context"

// ClientTransporter is how MCP Arc communicates with an MCP client.
// Run reads messages from the client and invokes onMessage for each JSON-RPC
// payload, together with a `respond` function that writes a message back to
// that specific client (used for synthetic errors and for routing responses).
type ClientTransporter interface {
	// The respond callback takes a second bool, "done": true marks the terminal
	// frame for the request (e.g. the actual JSON-RPC response) so the transport
	// may close the client stream; false keeps a streaming response open (used for
	// request-scoped notifications over Streamable HTTP).
	//
	// onMessage may return a cleanup function that the transport calls when this
	// client message's connection closes (client disconnect or stream completion).
	// The proxy uses it to propagate a client disconnect upstream (cancel the
	// in-flight upstream request); a nil return means "nothing to clean up".
	Run(ctx context.Context, onMessage func(raw []byte, respond func([]byte, bool) error) func()) error
	// Broadcast writes a message to all connected clients (used for
	// upstream-initiated notifications that cannot be routed to one session).
	Broadcast(raw []byte) error
	Close() error
}

// UpstreamTransporter is how MCP Arc communicates with the upstream MCP server.
type UpstreamTransporter interface {
	Run(ctx context.Context, onMessage func(raw []byte)) error
	Write(raw []byte) error
	Close() error
}
