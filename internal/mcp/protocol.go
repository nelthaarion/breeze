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

	// BreezeServerKind is "generator" or "app-runtime".
	//
	// A Breeze extension, not part of the MCP specification, and named with a
	// vendor prefix for exactly that reason: an unprefixed key risks colliding with
	// a future spec field, and a client seeing one would not know whose it was.
	//
	// It exists because an agent has to know which kind of server it reached before
	// it plans anything, and every other way of telling is a guess. Naming
	// convention does not survive a renamed binary; the tool list does not
	// distinguish an app-runtime server from a generator whose token is narrowly
	// scoped; and the port certainly does not. This is the authoritative answer,
	// populated from the same Mode value that decided which tools were registered,
	// so it cannot disagree with what tools/list returns.
	//
	// Absent from a server built by NewServer directly — the internal
	// "everything" builder used by tests — which is honest: that server has no
	// mode, and reporting one would be inventing an answer.
	BreezeServerKind ServerMode `json:"breezeServerKind,omitempty"`

	// BreezeCapabilities reports what the calling token may do.
	//
	// Part 8's primary mechanism, and the reason there is no feature-listing *tool*:
	// an agent learns its own permissions at handshake time, with no extra call. That
	// is sound only because scope is fixed for the lifetime of a token — it is set
	// when the token is minted and nothing changes it mid-session — so a snapshot
	// taken here cannot go stale.
	//
	// Also a Breeze extension, hence the prefix.
	BreezeCapabilities *capabilityReport `json:"breezeCapabilities,omitempty"`
}

// capabilityReport is the capability half of the handshake.
//
// Both lists are present, always. Granted alone cannot tell an agent whether a tool it
// cannot find was never built or was withheld from its token, and that difference
// decides whether the right response is to give up or to ask for a wider credential.
type capabilityReport struct {
	// Granted is what this token may do, sorted. For an unscoped token it is every
	// known category — what the token can actually do, rather than what was typed.
	Granted []string `json:"granted"`

	// Known is every category this server understands, sorted, so Granted can be read
	// as a subset of something.
	Known []string `json:"known"`

	// Scoped records whether the token was narrowed at all. Without it, an unscoped
	// token and one deliberately minted with every category look identical — and an
	// operator auditing a deployment wants to know which they are looking at.
	Scoped bool `json:"scoped"`
}

// serverCapabilities declares what this server can do.
//
// # What is absent, and why that is a decision
//
// MCP has four server capability keys: tools, resources, prompts and logging.
// Only tools is declared here, and the other three are absent deliberately
// rather than unimplemented by accident:
//
//   - resources and prompts would be a second and third vocabulary over the same
//     underlying data this server already exposes as tools. A resource is a thing
//     a client reads by URI; every readable thing here — a project's facts, a
//     running service's routes, an idiom — is already a tool that takes arguments
//     and returns structured content. Publishing them twice would mean two code
//     paths that can disagree about the same answer.
//   - logging (logging/setLevel plus notifications/message) is genuinely
//     redundant here rather than merely unbuilt. On stdio, stderr is the
//     diagnostic channel and is *required* to be: stdout is the protocol stream,
//     so a log line written there is a malformed message. The operator already
//     has every diagnostic this server produces, on the stream conventionally
//     read for it, without a protocol round trip to enable it.
//
// A client reads the presence of a key, not its value, so absence is the correct
// and complete way to say "do not ask me for these". This comment exists so that
// absence is legible as an answer rather than as a gap.
type serverCapabilities struct {
	// Tools is present because there are tools. It is not omitempty: omitting it
	// would mean "no tools", which is the opposite of the truth.
	Tools toolsCapability `json:"tools"`
}

// toolsCapability describes the tools capability's sub-capabilities.
//
// listChanged is declared false rather than omitted. The tool table is built once
// in NewServer and never mutated afterwards — addTool panics on a duplicate and
// nothing removes an entry — so this server will never send
// notifications/tools/list_changed. Saying so explicitly means a client caches
// tools/list without also having to subscribe to an update it would never get.
type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
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
// tool here produces text, because text is the one kind every client renders.
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

	// StructuredContent carries the machine-readable form of the same answer.
	//
	// Every tool that reports more than "it worked" fills it in, and the text
	// content is then the indented JSON of the same value. That duplication is
	// deliberate: a client pinned to the 2024-11-05 revision only knows how to
	// read content[].text, so putting the JSON there keeps the data reachable,
	// while a newer client reads structuredContent and skips parsing text whose
	// shape it would otherwise have to guess. What no tool does is forward a
	// generator's stdout verbatim — captured output is a side effect to be
	// summarised, not a result.
	StructuredContent any `json:"structuredContent,omitempty"`

	IsError bool `json:"isError,omitempty"`
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

// structuredResult builds a successful tool result carrying data in both forms.
//
// summary is a single human line — what happened, and how much of it. It is
// prepended to the JSON rather than replacing it so that a transcript stays
// readable without the payload becoming prose.
func structuredResult(summary string, data any) toolCallResult {
	res := toolCallResult{StructuredContent: data}
	res.Content = []toolContent{{Type: "text", Text: renderStructured(summary, data)}}
	return res
}

// structuredErrorResult is structuredResult for an outcome of "no".
//
// The payload is still attached: a caller that has just been refused needs the
// detail of the refusal more than a caller that succeeded needs the detail of
// the success.
func structuredErrorResult(summary string, data any) toolCallResult {
	res := structuredResult(summary, data)
	res.IsError = true
	return res
}

// renderStructured formats the text half of a structured result.
func renderStructured(summary string, data any) string {
	if data == nil {
		return summary
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		// Reaching here means a tool put an unserialisable value in its own
		// result type, which is a bug in this package rather than bad input.
		return summary + "\nresult could not be encoded: " + err.Error()
	}
	if summary == "" {
		return string(payload)
	}
	return summary + "\n" + string(payload)
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
