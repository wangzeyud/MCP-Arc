package proxy

import "encoding/json"

// ---------------------------------------------------------------------------
// MCP protocol touchpoints — the ONLY place in the governance layer that knows
// anything MCP-specific.
//
// MCP Arc is built on top of the transport layer (stdio / SSE), not inside the
// protocol: requests and responses are matched by JSON-RPC id, never parsed for
// protocol meaning. Everything the governance features need is expressed with
// the single method name below plus the shape of its params.
//
// If the spec renames the method, reshapes params, or changes capability
// negotiation, this file is the only thing that has to change.
// ---------------------------------------------------------------------------

// MethodToolsCall is the only MCP method Arc needs to recognise in order to
// mask, audit, rate-limit and replay tool calls.
const MethodToolsCall = "tools/call"

// toolCall reports whether msg is a tools/call request and, if so, extracts the
// tool name and its arguments object. It never fails on unexpected shapes: an
// unrecognised message simply is not a tool call and is forwarded untouched.
func toolCall(msg map[string]any) (name string, args map[string]any, ok bool) {
	if m, _ := msg["method"].(string); m != MethodToolsCall {
		return "", nil, false
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return "", nil, true
	}
	name, _ = params["name"].(string)
	args, _ = params["arguments"].(map[string]any)
	return name, args, true
}

// newToolCallRequest builds a tools/call JSON-RPC request carrying the given id.
// Used by replay so it reuses exactly the same shape as a live client call.
func newToolCallRequest(id any, toolName string, args map[string]any) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  MethodToolsCall,
		"params":  map[string]any{"name": toolName, "arguments": args},
	})
}
