// Package rpc implements JSON-RPC 2.0 directly on gnet's event loop.
//
// It is a peer of the HTTP layer in the root package, not a passenger on it.
// Requests are framed out of the raw connection buffer, dispatched, and
// answered with gnet's own Conn.Write / Conn.AsyncWrite. There is no net/http
// server, no net/rpc, no http.Handler adapter and no reflection-driven method
// discovery anywhere in the path — the same reasons the HTTP layer does not use
// them apply here unchanged.
//
// # Framing
//
// JSON-RPC 2.0 specifies a message format and says nothing about how messages
// are delimited on a byte stream. This package frames by structural
// completeness: it reads one complete JSON value at a time, tracking bracket
// depth while respecting string literals and escapes. That accepts every
// framing a client is likely to use — newline-delimited, whitespace-separated,
// or values packed back to back with no separator at all — without the client
// having to declare which one it picked, and without a length-prefix header
// that is not part of the specification.
//
// A value that is only partially present is left in the connection's own gnet
// context and completed by a later read, exactly as the HTTP layer reassembles
// a split request. SetMaxMessageBytes bounds how much a single message may
// accumulate before the connection is dropped, so a client cannot pin memory
// by opening a brace and going quiet.
//
// # Registration
//
// Methods are registered the way routes are, so nothing new has to be learned:
//
//	reg := rpc.NewRegistry()
//	reg.Use(logging)                       // mirrors Router.Use
//	reg.Register("sum", sum)               // mirrors Router.Handle
//	reg.RegisterBlocking("db.query", query) // mirrors Router.HandleBlocking
//
//	srv := rpc.NewServer(reg)
//	srv.SetPool(breeze.NewEventLoopWorkerPool(runtime.NumCPU()))
//	log.Fatal(srv.Run(9000, true))
//
// A HandlerFunc takes a *rpc.Context and reads params off it, the same shape as
// a breeze.HandlerFunc taking a *breeze.Context. Middleware composes through
// Context.Next, and the [global..., method..., handler] chain is flattened once
// at registration rather than rebuilt per call.
//
// # Blocking work
//
// A handler that blocks must not run on an event loop, because it stalls every
// connection pinned to that loop. Register those with RegisterBlocking and give
// the server a worker pool; everything else runs inline on the loop that read
// the bytes and is answered with a direct write. This is the same trade-off,
// with the same defaults, as Breeze.SetInlineExecution.
//
// # Lifetime of Context.Params
//
// Context.Params is raw JSON pointing into the connection's read buffer, which
// gnet reuses for the next read. It is valid for the duration of the handler.
// A batch containing any blocking method is copied into owned memory before it
// leaves the event loop, so a handler always sees valid bytes — but anything
// kept past the handler's return must be copied first, the same rule and the
// same failure mode as breeze.SetZeroCopyHeaders. Context.Bind copies as part
// of decoding, so bound structs are always safe to keep.
package rpc
