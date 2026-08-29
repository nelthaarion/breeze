// Package mcp implements a Model Context Protocol server for the Breeze
// toolchain.
//
// MCP is JSON-RPC 2.0 with a fixed method vocabulary, so this package is
// deliberately thin: rpc/ already speaks JSON-RPC, and rpc/stdio.go already
// frames it over a pipe. What is left is the vocabulary — initialize, tools/list,
// tools/call — and the tools themselves.
//
// Nothing here re-implements JSON-RPC. If a message is malformed, or names a
// method that does not exist, or has params of the wrong shape, the dispatcher in
// rpc/ has already answered it with the right code before any of this runs.
//
// # Why the generator is called in-process
//
// cmd/breeze-mcp could have shelled out to the breeze binary. It does not,
// because a subprocess only reports an exit status: a failure mid-way through
// scaffolding would arrive as "exit status 1" with the actual diagnostic mixed
// into stdout, and the caller would have to scrape it back out. Calling
// internal/generator directly keeps the error as an error.
package mcp

import "encoding/json"

// protocolVersion is the MCP revision this server implements.
//
// The handshake echoes it rather than negotiating: a peer asking for a different
// revision is told what it is talking to and can decide for itself. Pretending to
// support whatever was asked for would be worse than a visible mismatch.
const protocolVersion = "2024-11-05"

// serverName identifies this server in the handshake. Clients display it, and
// some key their per-server configuration on it, so it is effectively API.
const serverName = "breeze"

// initializeResult is the response to initialize.
//
// Capabilities is a nested object rather than a list of booleans because that is
// how MCP versions itself: a client reads the presence of a key, not its value,
// so declaring tools as an empty object is how a server says "I have tools"
// without promising anything about future sub-capabilities.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	// Tools is present, and empty, for the reason above. It is not omitempty:
	// omitting it would mean "no tools", which is the opposite of the truth.
	Tools struct{} `json:"tools"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolDescriptor is one entry in tools/list.
//
// InputSchema is JSON Schema. It is what makes a tool usable by a caller that
// has never seen this codebase: without it a model has to guess parameter names,
// and a guessed parameter is a silent no-op rather than an error.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// toolsListResult is the response to tools/list.
type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

// toolContent is one piece of a tool's output. MCP allows several kinds; every
// tool here produces text, because every tool here reports what it did to a
// filesystem.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCallResult is the response to tools/call.
//
// IsError deserves a note, because it is the part most easily got wrong. A tool
// that ran correctly and reported a failure — a generator refusing to overwrite a
// file, say — is not a protocol error: the call succeeded, and its outcome was
// "no". That belongs here, where the model can read the message and adjust.
//
// A JSON-RPC error is reserved for the cases where the request itself was wrong:
// an unknown tool name, or params that are not an object. The distinction matters
// because a model can recover from the former and can only give up on the latter.
type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// textResult builds a successful tool result.
func textResult(text string) toolCallResult {
	return toolCallResult{Content: []toolContent{{Type: "text", Text: text}}}
}

// errorResult builds a failed-but-well-formed tool result.
func errorResult(text string) toolCallResult {
	return toolCallResult{
		Content: []toolContent{{Type: "text", Text: text}},
		IsError: true,
	}
}

// toolCallParams is the params member of tools/call.
//
// Arguments is deliberately a RawMessage: each tool decodes its own arguments
// against its own schema, and decoding here would mean this type had to know
// every tool's shape.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
