# Breeze — Changelog

All changes made to the Breeze framework.

---

## [Unreleased]

Six work items were developed concurrently and each had written its own
`## [Unreleased]` heading, so the file had six sections at the same level all
claiming to be the next release. They are now `###` subsections under this one
heading, in reverse chronological order, which is what a reader collapsing the file
to `##` needs in order to see one unreleased block and eight releases.

### Graceful shutdown: `Breeze.Stop`

#### Added

- **`Breeze.Stop(ctx context.Context) error`** — a running server can now be
  stopped. `Run` wrapped `gnet.Run`, which blocks for the life of the process and
  discarded the one handle that can end it, so there was no way to shut a Breeze
  server down at all. That blocked moving a consumer's socket tests onto Breeze's
  WebSocket support: all of them tear down between runs through the `gnet.Engine`
  handle, and Breeze had no equivalent.

  The shape is `net/http.Server.Shutdown`'s, deliberately — a context for the
  deadline, graceful first and forced after:

  1. New connections are refused from the first instruction, before anything is
     torn down.
  2. Every active WebSocket connection is sent a Close frame with **1001 (going
     away)** and its handler's `OnClose` is queued on that connection's ordered
     dispatch queue.
  3. Work already dispatched to the worker pool — blocking routes, WebSocket
     handler callbacks — is given until `ctx` is done to finish.
  4. The engine is torn down, which closes the listener and force-closes whatever
     is still connected, and `Stop` waits for `Run` to return.

  Returns `nil` on a clean stop, `ctx.Err()` when step 3 ran out of time (the
  teardown still happens — that is the forced half), `ErrNotRunning` when there
  was no engine to stop, and `ErrShutdownIncomplete` when gnet's own teardown did
  not finish. Idempotent: later calls report the first call's result without
  repeating any of it, and a call that overlaps the first waits for it rather than
  starting a second teardown.

- **`Breeze.OnBoot`** now keeps the `gnet.Engine` gnet hands it. That handle is
  the whole mechanism; `gnet.Run` never returned it and Breeze never asked for it.
  Per-engine rather than gnet's package-level `Stop(addr)`, which stops "the last
  engine registered for this address" and is deprecated upstream for exactly that
  reason — two Breeze instances in one process now stop independently.

- **`Breeze.OnOpen`** closes connections that arrive during a shutdown. gnet
  cannot close a listener without tearing the engine down in the same step, so
  without this "stops accepting" would only become true at the very end, after the
  graceful wait it is supposed to precede. The upgrade handler refuses with **503**
  for the same reason: an upgrade can arrive on a keep-alive connection accepted
  before the shutdown began, and registering it after `Stop` swept the registry
  would leave that handler hearing about the close only from the force-close, as
  1006, after `Stop` had already returned.

- **`ErrNotRunning`, `ErrServerStopped`, `ErrShutdownIncomplete`.**

#### Changed

- **`Run` returns after `Stop`, rather than blocking forever.** The usage the
  feature exists for —

  ```go
  go func() { _ = app.Run(port, true) }()
  // ...
  _ = app.Stop(ctx)
  ```

  — needs `Run`'s goroutine to actually exit, not merely `Stop` to return. `Stop`
  waits on `Run`'s own exit rather than on `Engine.Stop`'s return, because that
  call polls a flag on a half-second ticker: waiting for it would put 500 ms into
  every shutdown, and it answers a weaker question than the one callers care about.

- **`Run` refuses to start a stopped instance** (`ErrServerStopped`). A `*Breeze`
  gets one lifetime: after `Stop` the WebSocket registry, hub and dispatch queues
  are torn down, and rebinding the port would serve traffic with half its state
  already collected.

- **A drain the worker pool refuses now runs on a goroutine** instead of being
  dropped. `wsDispatchQueue` sets `running` before dispatching its drain, so a
  dropped drain left the connection with no consumer for the rest of its life —
  every later message, and the close event, queued behind a worker that never
  started. A pool that has been shut down refuses silently and an `OverflowReject`
  pool refuses when full, so neither needs an application bug to reach. Ordering is
  unaffected: what guarantees it is that exactly one drain exists, not where it
  runs, which is why the no-pool path has always been a goroutine.

- **A request the pool refuses no longer leaks its pooled `Context`.** `dispatch`
  uses `SubmitErr` and puts the `Context` and the shutdown counter back when the
  task will not run.

#### Tests

- `shutdown_test.go` runs a real server on a real port for each case, because the
  property is not "Stop returned" but "the server is gone": the port is free,
  `Run`'s goroutine has exited, and every handler was told.
- The three cases the issue asked for: `TestStopWithNoConnectionsClosesTheListener`,
  `TestStopWaitsForInFlightRequests` (a blocking handler, verified still running
  when `Stop` is called and finished when it returns, with the client's 200
  received), and `TestStopClosesActiveWebSocketsAndFiresOnClose` (four connections;
  `OnClose` fired for each with **1001** before `Stop` returned, and each peer saw
  1001 on the wire rather than the 1006 a bare socket close produces).
- `TestStopDeliversQueuedMessagesBeforeOnClose` is the ordering guarantee under a
  shutdown, repeated, and asserts a message was actually delivered so it cannot
  pass by delivering none. Verified against the failure it exists for: removing
  the drain wait fails it and `TestStopWaitsForInFlightRequests` on the first
  iteration.
- `TestStopReturnsContextErrorWhenTheDeadlinePasses` covers graceful-then-force,
  `TestStopIsIdempotent` and `TestConcurrentStopPerformsOneShutdown` the repeat and
  racing callers, `TestStopDuringStartup` a `Stop` that beats gnet's boot,
  `TestStopAffectsOnlyItsOwnInstance` two servers where only one is stopped,
  `TestStopRefusesNewConnectionsImmediately` and
  `TestStopRefusesUpgradesOnExistingConnections` the two ways a connection could
  still slip in mid-shutdown, and `TestStopNotifiesWebSocketHandlersWithAClosedPool`
  the caller who shuts the pool down first — verified against the failure it exists
  for: with the drain dispatched by `Submit` instead, it times out after 10 s.

#### Notes

- `Stop` does not shut down `Breeze.Pool`. The pool is supplied by the caller and
  may be shared with subsystems that outlive the HTTP server, so follow `Stop`
  with `Pool.Shutdown(ctx)` when the pool is the server's alone.
- Inline requests are not counted as in-flight, and do not need to be: an inline
  handler runs on the event-loop goroutine inside `OnTraffic`, and the shutdown
  signal is delivered to that same goroutine as a queued task, so it cannot be
  processed until the handler has returned. Counting them would have put two
  atomic read-modify-writes on the one request path in Breeze that has none.

### Inbound WebSocket messages were delivered out of order

#### Fixed

- **A connection's messages could reach its handler in the wrong order.**
  `dispatchMessage` submitted each inbound message to the worker pool as its own
  task, so two messages from one connection could be picked up by two workers and
  run in either order. Ten sequentially numbered messages sent back-to-back came
  back as `[0 4 1 2 3 5 8 6 7 9]` on one run and correctly ordered on the next,
  which is why it survived: any single run had a fair chance of looking fine.

  A handler parsing a stream cannot recover from this. It is also the one place
  Breeze's two WebSocket directions disagreed — `websocket_client.go`'s read pump
  has always delivered in order, and the gnet transport's per-connection loop does
  too.

  Delivery now goes through a per-connection single-consumer queue
  (`wsDispatchQueue`). A `running` flag means exactly one drain task exists per
  connection at a time, and it pops from the front, so order is a property of the
  structure rather than of scheduling.

  **A per-connection mutex would not have fixed this.** It makes deliveries
  non-overlapping and says nothing about which of two already-queued messages
  acquires the lock first. That is the same coincidence that caused the bug.

  A drain occupies a pool worker only while it has something to deliver, so this
  is a queue plus a flag rather than a goroutine per connection: ten thousand idle
  WebSocket connections keep no goroutines.

- **`OnClose` could overtake messages still queued ahead of it.** It was its own
  pool task. An application that releases per-connection state in `OnClose` would
  then be handed a message with that state already gone. Close now travels the
  same queue, so it lands after everything delivered before it.

#### Security

- **The queue is bounded** at 256 events per connection, and a connection that
  overruns it is closed with 1009 rather than having messages dropped. It is a
  buffer a peer fills and a handler drains, so unbounded it is a memory
  exhaustion vector; dropping instead of closing would silently hand a handler a
  stream with holes in it.

- **A panicking handler no longer strands its connection.** One drain now
  delivers many messages, so a panic escaping it would leave `running` true with
  nothing consuming — every later message on that connection queued behind a
  consumer that no longer exists, and a connection that goes silent rather than
  losing one message. `wsDispatchQueue.call` recovers per event, which the old
  pool-task-per-message design got from `WorkerPool.runTask` for free.

#### Tests

- `TestInboundMessagesArriveInOrder` sends 32 tagged messages back-to-back and
  repeats the exchange 30 times, because a single run does not distinguish fixed
  from lucky. Verified against the bug: reinstating the old dispatch fails it on
  iteration 1. `TestInboundOrderHoldsWithASlowHandler` repeats it with a 1 ms
  handler, which is where the reordering was easiest to produce.
- `TestOnCloseArrivesAfterEveryMessage` and
  `TestOrderedDispatchDoesNotStrandAConnectionOnPanic` cover the two failure modes
  above. All four pass under `-race -count=5`.

### Outbound WebSocket: `breeze.DialWS`

#### Added

- **`breeze.DialWS(url, WSClientConfig)`** — a Breeze process can now dial a
  WebSocket server. The inbound half was complete; there was no way to open a
  connection, which blocked moving a P2P networking layer onto Breeze since
  every node in one both accepts and dials.

  Full RFC 6455 client: the `Sec-WebSocket-Key`/`Accept` exchange with
  verification, mandatory frame masking, binary and text frames, ping/pong,
  fragmented-frame reassembly, and the close handshake. `wss://` through
  `crypto/tls`, matching `client/client.go` — gnet has no TLS of its own.

- **`WSConn.OnMessage`, `OnClose`, `Ping`, `Pong`, `Recv`, `Subprotocol`,
  `IsClient`** for a dialled connection. Callbacks are registered after the dial
  rather than passed in the config, so a handler can name the connection it
  belongs to — the right way round for a peer table keyed on the connection.

#### Changed

- **`DialWS` returns the same `*WSConn` the inbound side hands a `WSHandler`.**
  That was the point of building it rather than a separate client type: a
  dispatch loop holding accepted and dialled peers in one table would otherwise
  need two of every code path, with nothing stopping it getting one wrong.

  `WSConn.conn` changed from `gnet.Conn` to `wsRawConn`, a three-method interface
  (`AsyncWrite`, `Close`, `RemoteAddr`) that `gnet.Conn` already satisfies. No
  inbound behaviour changed. Three methods rather than gnet's forty because
  everything else on that interface is inbound-only, and requiring it would mean
  the outbound adapter faking a read path it does not use.

- Frame encoding split by role. RFC 6455 §5.3 requires a client to mask every
  frame it sends and §5.1 forbids a server from masking any, so
  `buildWSFrameMasked` joins `buildWSFrame` and `WSConn.client` selects. Masking
  keys come from `crypto/rand`: the key exists to stop a hostile intermediary
  steering a proxied stream, which a predictable sequence would not do.

#### Not included, deliberately

- **Reconnection and backoff.** A redial policy needs peer scoring, whether the
  address is still in the validator set, and how long to wait. A connection
  primitive can answer none of those. `OnClose` reports 1006 for a drop with no
  close frame and the peer's code otherwise, which is the signal a redial loop
  needs to tell an orderly shutdown from a network failure.

- **gnet for the outbound path.** `client/client.go` dials on gnet for one I/O
  model in both directions; this does not, and the reason is stated in
  `websocket_client.go`. gnet's client mode delivers reads to an engine-owned
  handler, and Breeze's engine handler dispatches by file descriptor through
  HTTP/WebSocket state — so an outbound connection would have to be registered
  into a *running server*, and `DialWS` could not be called by a process that
  does not serve. A P2P node dials before, and often without, listening.

  The cost, stated because a consumer sizing per-connection resource use needs
  it: one goroutine blocked in a read plus one 4 KiB `bufio.Reader` per dialled
  connection. At tens to low hundreds of peers that is a few hundred KiB. At tens
  of thousands it would be the wrong design, and the fix is gnet's `Enroll`;
  `DialWS` returns a `*WSConn` either way, so the signature survives that change.

#### Security

- **A dialled connection does not join the server's `WSHub`.** The hub is the
  registry of connections this server accepted, and `Broadcast` walks it — an
  outbound connection in there would be sent server-side traffic and counted
  among this server's own clients.

- **Reserved handshake headers cannot be overridden.** `Host`, `Upgrade`,
  `Connection`, `Sec-WebSocket-Key`, `-Version` and `-Protocol` are dropped from
  `Config.Header`; a caller-supplied `Sec-WebSocket-Key` would defeat the accept
  check that exists to catch a peer which is not a WebSocket endpoint. CR and LF
  are stripped from every header name and value, because a newline there is
  request splitting.

- **The handshake response is bounded** at 100 headers. A peer that never sends
  the terminating blank line would otherwise grow the header map until the
  deadline.

- **A frame's declared length is checked before allocating**, and a fragmented
  message's reassembled total is checked as well — the per-frame limit alone is
  the number an attacker does not control, since many small fragments reach the
  same total.

- **A peer selecting an unoffered subprotocol is refused**, rather than the
  connection continuing to speak a protocol this side never agreed to.

- **One deadline covers the handshake and is then cleared.** A P2P connection is
  idle for most of its life, so a lingering read deadline would close healthy
  peers; liveness is `Ping`'s job instead.

### `breeze-mcp` run by hand: a guide, and how to get the token

#### Fixed

- **`breeze-mcp --mode generator` printed nothing and blocked forever.** Correct for
  the transport — stdio waits for a client — and useless to a person: zero bytes on
  both streams is indistinguishable from a hang, a deadlock, or a binary that failed
  silently. The commonest first contact with the command was a command that looked
  broken.

  A terminal now gets a short block on stderr: that it is not interactive, that the
  silence is the server working, the mode and tool count it will serve, its scope and
  workspace, the editor config to point at it, and the one-line pipe that proves it
  answers.

- **A network server printed its token and left you to work out the rest.** The
  banner has always shown a generated token once, which is the security-correct
  behaviour and not the same thing as telling somebody what to do with it. A terminal
  now gets `export BREEZE_MCP_TOKEN=…`, the `Authorization: Bearer` header, the
  client config, and the `curl …/mcp/features` call that verifies a token without
  needing an MCP session.

#### Security

- **The guide is terminal-only, and that is the whole design.** An editor launches
  this as a subprocess with pipes; stdout is then the protocol stream, so one
  human-readable line there is one malformed MCP message to the peer — the guide
  would corrupt the session it was explaining. `interactiveStdin` gates both blocks
  on stdin being a character device, so a piped stdin behaves exactly as before:
  nothing on stderr, JSON-RPC only on stdout.

  A flag was the alternative and is worse: it would have to be discovered before it
  could help, and the person who needs this is the person who does not yet know the
  flags.

- **The stdio guide names no token.** stdio authenticates nothing — the process
  boundary is the trust boundary — so mentioning one would send an operator looking
  for a value that does not exist. It does not print `$BREEZE_MCP_TOKEN` either, even
  when set, since that value belongs to the other transport.

- **The network guide never reprints the token.** The banner shows a generated one
  exactly once and `TestBannerPrintsAGeneratedTokenExactlyOnce` holds that count; a
  second copy in a usage example would widen the window for a truncated log to keep
  it, for nothing. Every example reads the environment variable instead — also the
  form that keeps a token out of `ps` and `docker inspect`. A *supplied* token is not
  echoed at all, matching what the banner already does.

- **Unconfined is reported as a warning, not a row.** With `--allow-any-path`,
  `breeze_verify_project` will run `go test` in any directory it is handed. In a
  terminal that is the fact most worth seeing, so it is not printed as an ordinary
  line beside `mode` and `tools`.

- `interactiveStdin` returns false for anything it cannot positively identify as a
  terminal, including an `*os.File` that is a pipe. The two directions are
  asymmetric: a false negative costs a missing guide, a false positive writes text
  into somebody's protocol tooling.

### Auto-MCP example, and a bind that never bound

#### Fixed

- **`app.EnableMCP("127.0.0.1:2001")` never listened, and said nothing about it.**
  The address was handed to `gnet.Run`, which parses it as a URL and rejects one
  without a scheme — so every documented form of the call failed. It failed
  *inside the goroutine*, after `EnableMCP` had already returned `nil`, so the only
  evidence was one `log.Printf` line: no error to the caller, no failed startup,
  and an MCP endpoint that simply was not there. Every tagged route was silently
  unreachable.

  Found by writing `cmd/automcp-example`: the example used the address form the
  documentation shows, and nothing answered on the port.

  The fix is `mcpListenAddr`, which splits the two forms apart, because the two
  consumers want different ones and each rejects the other:

  - `gnet.Run` needs the scheme (`tcp://127.0.0.1:2001`).
  - `net.SplitHostPort` — which is how `mcp.StartInProcess` detects a port
    collision with Auto-MCP — rejects the scheme-qualified form for having too many
    colons. Recording that form would have made the collision check answer "no
    conflict" for every address that works, and two MCP servers would have shared a
    port with each answering some of the other's requests.

  So the plain `host:port` is recorded and reported, and the scheme is added only
  for the bind. An address that already carries a scheme passes through untouched: a
  caller naming a Unix socket means it, and a socket cannot collide with a TCP port.
  A malformed address is now returned as an error rather than logged, which is what
  `EnableMCP` already promises for configuration mistakes.

  `TestEnableMCPRecordsAnAddressTheConflictCheckCanRead` asserts the recorded form
  parses, so the two halves cannot drift apart again.

#### Added

- **`cmd/automcp-example`** — one order service, three routes, chosen so they can be
  compared: `POST /orders` tagged and open, `GET /orders/:id` tagged and behind JWT,
  `GET /internal/metrics` documented and deliberately untagged.

  The handler does real work — a generated id, a catalogue price the caller never
  sends, and `total_cents = unit_cents × quantity` — because a schema that matches
  the route proves nothing if the handler behind it is a stub.

  Four tests, each its own function so a failure names which guarantee broke:
  schema parity against the generated OpenAPI document (both directions, including
  field descriptions), a call whose result reflects logic that could not be echoed,
  an untagged route that is neither listed nor reachable by guessing, and auth
  enforcement asserted as a *comparison* against the router's own dispatch path
  rather than as a hardcoded 401.

  The last one is the interesting assertion: matching status *and body* is only
  possible if the same middleware ran. A separate MCP refusal path that happened to
  use the same status code would pass a naive test.

### `--log`: a running MCP server that says what it is doing

#### Added

- **`breeze-mcp --log`** writes one stderr line per tool call, refused call,
  unknown tool, panic, handshake and rejected request. Before this, a running
  server printed its banner and then nothing — so "did the agent call anything",
  "is a tool failing" and "is something probing the control port" had no answer
  short of a packet capture, on a port whose tools write files and start containers.

- **The refusal line is the one that did not exist anywhere.** A wrong token never
  reaches a tool, so no amount of dispatch-level logging could see it: a
  token-guessing run against the control port left no trace at all. The line carries
  the peer address, because a refusal nobody can attribute is a refusal nobody can
  act on.

- **`unknown` and `refused` are distinct kinds.** On the wire they are deliberately
  close — a refusal should not enumerate what a caller may not have. In a log the
  opposite is wanted: "the agent says the tool is missing" resolves either to a wrong
  `--mode` or a wrong `--scope`, and those have different fixes.

#### Security

- **Argument names are logged; values are not, at any verbosity.** `token` and
  `password` on the fleet and live tools, `service_token` on provisioning, `token`
  on simulate — credentials arrive as ordinary tool arguments, and stderr is what a
  container runtime captures and a supervisor ships elsewhere.

  Enforced structurally rather than by a redaction list: `mcp.Event` has no field
  that could hold a value. Every field is a constant declared in `internal/mcp`, a
  number, a tool name, or a list of argument names. A `Log(fmt.Sprintf(...))`
  interface would have made every future call site a place where a value could be
  interpolated by accident; this makes that a compile error.

  A redaction list was the alternative and is worse: it is wrong the moment somebody
  adds a field, and the failure is a secret in a log discovered later by whoever
  reads the log.

- **A rejected `Origin` is not logged**, though it is still echoed to the caller so
  an operator can see which string to allowlist. Logging it would let whoever is
  being refused choose what gets written into somebody else's log file.

- **A panic value is not logged.** It reaches the caller who asked for the tool; it
  is composed at the panic site and can hold a path, a captured output, or an
  argument. The line names the tool, which is what a log is for.

- **Off by default.** On stdio, stdout is the protocol stream — one human-readable
  line there is one malformed MCP message to the peer — and stderr belongs to
  whichever editor launched the process. Both are somebody else's channel.
  `TestAStdioServerWithoutLoggingWritesOnlyProtocol` asserts every byte of stdout is
  a JSON-RPC message.

- The formatter holds a mutex. The network transport serves each request on its own
  `net/http` goroutine, `io.Writer` promises nothing about concurrent use, and two
  interleaved `Fprintf` calls produce one corrupted line — which for a log being
  scraped is worse than no line, because it looks like data.

### Allocation Pass: Render and Event Dispatch

#### Performance

Every number below is from `benchstat` over 6–8 runs on the same machine, with the
benchmark that produced it named. The two benchmark files these needed — the root
package's and `dashboard/`'s — were new; the render path and the dashboard
middleware had no benchmarks at all, so the first task was making the cost
measurable rather than arguable. (Both are now `bench_test.go`; see *Repository
structure and code cleanliness* below.)

- **A full page render allocates 60 times instead of 118, and takes 10.5µs instead
  of 331µs.** (`BenchmarkZZRenderViewFull`: −49% allocs, −68% bytes, −96.8% time.
  Partial/SPA renders: −49% allocs, −77% bytes, −98.4% time.) Four separate
  things were being paid per request:

  - `breezeRuntime()` concatenated a `<script>` wrapper around the 20KB minified
    SPA bundle on **every full page load** — a 20KB allocation and copy to produce
    a string that differs only in which of two embedded constants it wraps. Both
    variants are now built at most once by `sync.OnceValue`.
    (`BenchmarkZZRuntimeString`: 9.5µs and 20KB → 3.5ns and zero.)

  - `collectTemplateSources` read the view file and globbed *and read every
    component file* on every render, then JSON-encoded the result — cache warm or
    not, production or dev. The finished `<script id="__breeze_tmpl__">` tag is now
    cached per view alongside the parsed template set, with the same devMode
    escape hatch. (`BenchmarkZZTemplateScriptCached` vs `…Script`: 1.2µs/9 allocs
    → 16.6ns/0.)

  - The injected block was built by concatenating four strings and then splicing
    that into the page, which copied the runtime once into `injection`, again into
    the buffer, and repeatedly as the buffer doubled to fit it. `execView` now
    computes the exact final length and appends each part once into a slice sized
    for it.

  - Template execution allocated a fresh `bytes.Buffer` per render and per
    `{{component}}` tag, growing each from zero. Those buffers are pooled
    (`renderBufPool`); the response body is still a fresh, exactly-sized slice,
    because gnet's async write path returns before the bytes reach the socket and
    a pooled body would be overwritten by the next render.

  `execView` now returns `[]byte` instead of writing into a caller-supplied
  buffer. It is unexported, and the exported surface — `RenderView`,
  `RenderComponent`, `RenderJSON`, `Router.View`, `Context.Render` — is unchanged.

- **Synchronous event dispatch allocates nothing.** (`BenchmarkEmit`: 3 allocs/168 B
  → 0/0 at every listener count; −52% time at one listener, −7% at 100, −2.3% at
  1000. `Recorder/disabled` −34%, `Metrics/disabled` −36%,
  `ContextMetadata/unused` −60%.) Two causes, both invisible without an escape
  analysis dump:

  - `emitCtx` passed the event to `b.finish` as an `any`, which boxes — and
    therefore allocates — for any payload wider than a pointer. Only the recorder
    and the observer read it, both are off by default, and both are already gated
    by an atomic flag. `payloadFor` checks those flags first and passes nil when
    nobody will look, so the dispatch no longer allocates a copy of the event to
    discard.

  - The middleware chain's closure captured `ran` and `stopped` by reference,
    which forced both onto the heap. Escape analysis is per-function, so the cost
    was paid on every dispatch merely because the closure existed in that
    function — including the overwhelmingly common case of no middleware at all.
    The closure moved to `runMiddlewareChain`, and the two variables are now
    ordinary stack locals in `emitCtx`.

  The middleware path kept its own necessary allocations and still improved
  (−20% allocs at one middleware, −11% at five) because it no longer boxes the
  payload either.

- **The dashboard's live-event queue no longer allocates when nobody is watching.**
  (`BenchmarkZZPushEventIdle`: 1 alloc/208 B → 0/0, −81% time.) `pushEvent` took
  `payload any`, so the caller boxed a `RequestRecord` — allocating, since it does
  not fit in a pointer word — *before* the function could check whether any
  WebSocket client was connected and drop it. It is now generic, so boxing happens
  at the append, on the path that actually keeps the value. This is on the
  slow-request path of every instrumented server, dashboard open or not.

#### Not changed, deliberately

- **`text/template` reflection dominates what is left of a render.** After the
  above, ~66% of remaining allocations in `BenchmarkZZRenderViewFull` are
  `reflect.Value.call` and friends inside `text/template`'s executor, reached
  through `map[string]any` page data. Removing that means not using
  `html/template`, which is a different framework, not an optimisation.

- **`SetHeader`'s map sizing.** `DefaultSecurityMiddleware` sets twelve headers
  into a map created with capacity 2, so it rehashes three times while filling.
  Raising the capacity to 8 was measured and produced no reliable improvement in
  bytes or allocations, and the timing runs were too noisy on this machine to
  claim anything — the whole benchmark is dominated by the fixture. Reverted
  rather than committed on a plausible-sounding argument.

- **`snapshotMessage` (25 allocs) and `flush` (`map[string]any` + `json.Marshal`).**
  Both run on the hub's own goroutine at 1Hz and 10Hz respectively, not on the
  request path. Pre-sizing them would trade clarity for allocations nobody is
  waiting on.

- **`EmitAsync` still allocates per listener** (one closure and one scheduled
  task each). That is what asynchronous means here; the Context is deliberately
  not pooled on that path because there is no point at which the last listener is
  known to have finished.

---

### Response headers set by middleware were discarded by the handler

`Context`'s three body methods — `JSON`, `WriteString`, `HTML` — assigned
`r.Headers = hdrsJSON`, one of three package-level shared maps, discarding whatever
was already on the response.

Every middleware that sets a response header does so **before** `ctx.Next()`, because
that is the only place it can: after `Next` returns, the handler has already written
the response. So `CORSMiddleware` and `SecurityMiddleware` computed up to eighteen
headers per request and the handler's `return ctx.JSON(...)` threw all of them away.

Nothing errored. `curl -i` showed a correct 200 with a correct body and no
`Access-Control-Allow-Origin`; a browser reported a CORS failure with no indication
that the server had computed the header and dropped it. Every `SecurityMiddleware`
header — CSP, HSTS, `X-Frame-Options` — was absent from every JSON response, which is
a live clickjacking and transport-security exposure rather than a missing hardening
nicety.

**Fixed** by routing all three through one `(*HTTPResponse).setContentType`, which
merges instead of assigning:

- `headersShared` is the discriminator, and it answers exactly the right question. It
  is true only when nothing has called `SetHeader`, so an untouched response still
  gets the shared map and its precomputed wire block — **the fast path is unchanged,
  at zero allocations and no map range at serialization time** — and only a response
  someone actually wrote a header to takes the merging branch.
- A handler's explicit `Content-Type` now wins, tracked by a new `ctypePinned` flag on
  the response rather than by looking for the key in the map. The map cannot answer
  that question: a body method leaves a `Content-Type` behind too, so
  `WriteString` then `JSON` on a middleware-touched response would find `text/plain`
  present, keep it, and send a JSON body labelled as text. Pinned means the caller
  chose it — `application/problem+json` from `ctx.JSON` is the case this makes
  possible — and unpinned means a previous body method's default, which is replaced.
- Any existing `Content-Type` is deleted before the new one is written, matched
  case-insensitively. The map is serialized verbatim, so a caller's `content-type` and
  a body method's `Content-Type` are two distinct keys to Go and both would be sent;
  RFC 9110 §5.3 lets a recipient treat that as malformed.
- `ServeStatic` pins its type too: it comes from the file extension, which is the most
  specific answer available.

Regression tests in `middlewares/header_preservation_test.go` drive a real chain
(middleware, then handler, in that order), which is the only way the bug was
observable — asserting `SetHeader` then `JSON` on a bare `Context` proves the
mechanism but not the thing that broke.

---

### Repository structure and code cleanliness

An audit of the repository against itself — structure, naming, dead code and
documentation drift — followed by fixing every finding. The audit is kept at
[`docs/repository-audit.md`](./docs/repository-audit.md) because its findings cite
paths and line numbers, so a future reader can check whether a rule is still earning
its place; the convention it produced is
[`docs/repository-structure.md`](./docs/repository-structure.md).

#### The finding that mattered most: the linter had never run

`.github/workflows/ci.yml` gated its lint step on
`hashFiles('.golangci.yml') != ''`, for a file that did not exist. The step had been
skipped on every run since it was added.

- **New `.golangci.yml`** (v2 format): `staticcheck` + `unused`, `-ST1000`, and no
  per-path exclusions at all — see the next section for the two it started with and
  why neither survived.
- **CI now runs golangci-lint unconditionally**, builds and vets the **nested kafka
  module** — previously invisible to all CI — and runs a Markdown link check.
- **New `internal/tools/linkcheck`**: fence- and code-span-aware, so Go generics like
  `Inspect[T](bus)` are not read as broken links. Exits non-zero on a broken one.

#### The linter configuration has no exclusions, and that is the point

Both exclusions this file originally shipped with are gone, replaced by fixes:

- **ST1012 on `events/errors.go`** — `events.Stop` now carries a `//lint:ignore` with
  its reason at the declaration. The name is deliberate: the value exists to be
  returned by a listener, so the call site reads `return events.Stop`, a statement
  about control flow in a position where `return events.ErrStop` would claim something
  failed. It is also exported API and documented under that name.
- **ST1005 on all of `internal/generator/`** — only one message actually tripped it,
  and it tripped on a trailing `."` that the sentence did not need. A blanket
  `text: ST1005` over a 40-file package would have suppressed every future instance
  too, which is the difference between an exception and a blind spot.

#### The MCP endpoint can now diagnose itself

`breeze_diagnose_service` reads the `diag` registry, so before this the endpoint
*answering the call* was the one subsystem missing from its own report.
`mcp.StartInProcess` now registers a probe under `mcp` — alongside `auto-mcp`, the
other MCP endpoint — reporting address, mode, tool counts and scope.

The three states it surfaces are the ones an agent cannot diagnose from outside,
because none of them produces an error anywhere:

- **A scope withheld a tool.** A client calling one gets the same `no such tool` as
  for a tool that does not exist. `reachable_tools` versus `tools`, plus
  `withheld_by_scope`, is the only place the two layers are visible at once — mode
  decides what is registered, scope decides what the credential reaches.
- **Generator mode in a container with no source tree.** It registers the full
  toolchain and every mutating tool fails at its first file operation. Nothing at
  startup knows whether the source is there, so nothing warns.
- **`AllowWorkspaceTools` on in production.** The process will `chdir` into and
  rewrite its own tree while serving requests.

Notes also fire for an unscoped token, `AllowedOrigins: ["*"]`, and a non-loopback
bind. The probe is registered by `StartInProcess` rather than from an `init`, for the
same reason the WebSocket hub's is: before the endpoint exists there is nothing to
report, and a probe answering "off" for a feature the application never asked for is a
row in every diagnostics read that no reader wants.

`mcp/diag_test.go` asserts it both ways — directly through `diag.Get`, and end to end
through a handshaken MCP session calling `breeze_diagnose_service`, which is the path
that would catch a break at any of the four joints between the probe and the agent.

#### Two real bugs, both invisible because of the above

- **`fleet/aggregator/blastradius.go`** — `ComputeBlastRadius` took a `Config`,
  normalised it with `withDefaults()`, and then read no field of it (SA4006). Every
  threshold is applied by `Incidents` before it calls here. The parameter is gone;
  the doc comment says why, because a normalisation that reads as if it mattered is
  worse than its absence.
- **`observability/observability_test.go`** — `TestDefaultCollectorIsStable`
  asserted `Default() != Default()`, identical expressions on both sides (SA4000), so
  it passed whether or not the singleton worked. It now binds both calls and adds a
  16-goroutine race check, which is the half that would actually catch a `Default()`
  built on a nil check instead of `sync.Once`.
#### Structure

- `cmd/main.go` → **`cmd/api-example/`**; `example_template/` →
  **`cmd/templates-example/`**; `events/events-app/` → **`cmd/events-example/`**. Every
  runnable example now lives under `cmd/`, one directory per example.
- `cmd/event-validator/` — a `package main` harness that ran 10 million emits and
  printed unconditional `PASS:` lines, never run by CI and never under `-race` —
  **replaced by `events/stress_test.go`**. Two tests now cover the global bus under
  registration churn and self-unsubscribe during concurrent dispatch, with the
  delivery count asserted rather than eyeballed.
- **`README.md` in each of the seven examples**: what it demonstrates, how to run it,
  what to look for. Package doc comments added to `cmd/events-example` and
  `cmd/templates-example`.
- **Seven new subsystem references**: [`binding.md`](./docs/binding.md),
  [`middlewares.md`](./docs/middlewares.md), [`diag.md`](./docs/diag.md),
  [`rpc.md`](./docs/rpc.md), [`scalar.md`](./docs/scalar.md),
  [`migrate.md`](./docs/migrate.md) and [`client.md`](./docs/client.md), plus
  [`docs/README.md`](./docs/README.md) as the index.
- **Benchmark files unified on `bench_test.go`.** The root package's three
  (`router_bench_test.go`, `zzperf_bench_test.go`, `zzrender_bench_test.go`) are now
  one file; `dashboard/` and `middlewares/` renamed theirs. The `Benchmark**ZZ**…`
  *function* names are unchanged — they are the identifiers every recorded baseline in
  this file is keyed to, and renaming them would make a `benchstat` comparison report
  "no benchmarks" rather than a regression. The two chain-composition tests that had
  been living in the router's benchmark file moved to `router_chain_test.go`.
- **13 committed build artifacts removed** from the index (`build.log`, `*.log`,
  `c_*.json`, and a file literally named `$($f.FullName)`), with `.gitignore` rules
  and a comment explaining why.
- **CodeQL** no longer excludes `example_template/**`, a path that no longer exists.
  An exclusion for a moved path is worse than none: it silently stops matching and
  nobody notices the coverage came back.
- `Dockerfile`, `docker-compose.yml` and `README.md` updated for the moved example
  target (`./cmd` → `./cmd/api-example`).

#### Duplication reconciled

Seven helpers existed in two or three copies each, and several had **diverged** —
the dangerous kind:

- **`humanBytes`** printed `KiB/MiB/GiB` in `internal/mcp` and `KB/MB/GB` in
  `middlewares` and `video`, while all three divided by 1024 and fed the same
  diagnostics page. Now one **`diag.HumanBytes`**, binary units, with a test at the
  boundaries the copies disagreed on. Each copy carried a comment explaining it could
  not be shared because the alternative was a forbidden import — each was right about
  the import and wrong about the conclusion: `diag` is the leaf all three already
  depend on.
- **`msOf`** truncated to microseconds in `events` and kept nanosecond noise in
  `workflow`, and both fed the same dashboard. Now one **`diag.Milliseconds`**.
- **`toSlug`** turned `HTTPServer` into `http_server` in the generator and
  `h_t_t_p_server` in `migrate`, and both named migration files. The `migrate` copy
  was **dead** — that package discovers and runs migrations, it does not write them —
  and is gone, with a note at its former site.
- **`firstLine`** truncated to 160 characters in `internal/mcp` and not at all in
  `scalar`. The MCP one is now **`bodyExcerpt`**, named for what it produces, and both
  doc comments state why they must not be interchanged.
- **`contains`** ×2 → `slices.Contains`. **`itoa`** ×2 → `strconv.Itoa`; the `oauth2`
  copy did not handle negatives and was correct only because its one call site cannot
  pass one.

#### Dead code

Fifteen unreachable symbols deleted, each with the comment that documented it:
`dashboard` `jsonStringField`, `currentMaxProcs`, `templatesDir`, `procPid`,
`requestsPerSec`; `fleet` `tags.recorder`; `internal/generator` `zeroLiteral`,
`blockMarkers` and four dead proto→Go type mappers; `(*changeSetStore).open`,
`(*portAllocator).inUse`, `(*mount).resolve`, `panickyObserver.calls`;
`migrate.nextVersion`. Also 3× S1021 in `binding/bind_test.go`.

#### Comments corrected

Six comments confidently described code that had been deleted. `dashboard/cpu.go`
documented a `runtime.NumGoroutine()`-based approximation for a function that reads
`/proc/self/stat`; `dashboard/collector.go` claimed a field was "updated by sampler"
when the sampler never touched it; `dashboard/middleware.go` cited "PHASE 1-9 of the
runtime audit", a document not in this repository, and now states the actual
mechanism — pooled request buffers, and a mis-attributed record rather than a crash.

#### Refactors, zero behavioural change

The bar for this section was that no test's expected output changed. If one had, the
refactor changed behaviour and would have been reverted.

- **`dashboard.registerRoutes`** (196 lines) split into `registerAuthRoutes`,
  `registerPageRoutes` and `registerAPIRoutes`. Split by *dependency*, not length:
  the view routes need the extracted template directory and cannot be registered
  without it, the API routes need neither, and the WebSocket route needs the `*Breeze`
  rather than the `*Router`. That is what makes the template-extraction failure branch
  obviously correct — the JSON API stays up when templates cannot be written.
- **`dashboard.jsonError`** replaces six hand-rolled
  `ctx.JSON(map[string]string{"error": …})` sites. Modelled on
  `aggregator.writeJSON`, and deliberately single-key: the sites that add a second
  field — the unreachable aggregator's URL, the list of registered subsystems — build
  their own map rather than widening this one for the exception.
- Receiver naming: `*Breeze` `b`→`s` across the ten methods in
  `websocket_engine.go` (21 methods already used `s`); `*route` `rt`→`r` in
  `router.go` (six already used `r`).
- `breeze.go:OnTraffic` and `video/handler.go:serve` are **left long** — measured hot
  paths where splitting risks the inlining and allocation behaviour the numbers in
  this file verify. Recorded in `repository-structure.md`'s deviation list rather than
  left for someone to rediscover and "fix".

#### Corrections to this file and the README

- `README.md`: `21`→`23` features (verified against `breeze add --list`), a dead ToC
  anchor removed, two `###` siblings un-nested from under the WebSocket section, four
  bare `docs/mcp-walkthrough.md` mentions turned into links, a pointer to
  `docs/README.md`, the deprecated `NewWorkerPool` in the quick-start replaced with
  `NewEventLoopWorkerPool`, and a new **Importable packages** table — `binding`,
  `client`, `scalar`, `migrate`, `rpc`, `observability`, `fleet`, `dashboard` and
  `mcp` appeared **zero times** in it before.
- `cmd/templates-example/main.go` was the last caller of the deprecated
  `breeze.NewWorkerPool` outside `workerpool.go`.
- This file: six concurrent `## [Unreleased]` sections consolidated under one heading,
  `## Bug Fixes` demoted to `###` under v1.3.1 where it belongs, the `## [v.1.4.1]`
  typo fixed, and `## File Inventory` deleted — it described a zip-distribution layout
  ("Replace your entire `breeze/` directory") and listed files as "ORIGINAL
  (unchanged)" that had changed many times since.

### Security Hardening

#### Security

- **MCP filesystem tools are confined to a workspace.** Every tool that takes a
  path — `breeze_new`, `breeze_add`, `breeze_generate`, `breeze_verify_project`,
  `breeze_run_benchmarks`, `breeze_check_idioms`, the planning tools, the change-set
  tools, the knowledge tools — resolved it with `filepath.Abs` and used it. So
  `{"path": "/etc"}` and `{"path": "C:\\Users\\someone\\else"}` were honoured.

  Two of those do not merely read. `breeze_verify_project` and
  `breeze_run_benchmarks` run `go test` in the directory they are given, and
  `go test` compiles and executes what it finds. An agent that could name any path
  could therefore run any code already on the host, under the server's identity, and
  read its stdout back as a tool result. No injection was needed — the feature did
  it on request.

  The default is now the working directory. `--workspace` takes one or more roots
  and `--allow-any-path` removes the confinement; they are mutually exclusive,
  because "confine to these roots, and also to nothing" is not a statement. The
  startup banner reports which is in force either way, with a warning line when it
  is off.

  The check is centralised rather than per call site. `resolvePath` in
  `internal/mcp/confine.go` is the only way any tool obtains a usable path, so a new
  tool cannot be added without it — a per-site list would have to stay complete, and
  the failure mode of an incomplete list is silent: the tool that was missed works
  exactly as before.

  That failure mode was not hypothetical. `breeze_explain_project`,
  `breeze_suggest_next_steps` and `breeze_diff_config` share a read path that chdirs
  into the directory it is given and reads `go.mod`, the features file, the route
  registry and `models/`, and it was still unconfined after every other tool was
  fixed. `breeze_diff_config` was the worse of the two shapes: it returned
  `"valid": true` with an empty change list and put the refusal in a note, so an
  agent reading the structured result would have concluded the comparison succeeded.
  The confinement now lives in `inProject` — the function that performs the chdir —
  so a future reader of a project cannot reach the filesystem without it. A test
  drives all 16 path-taking tools against an out-of-workspace path and asserts that
  each is refused *because of the workspace*, and a second test reads every tool's
  own JSON Schema to catch a new path argument that was never added to the first
  test's list.

  Both the roots and the candidate are symlink-resolved before comparison. A prefix
  test is not a containment test — a symlink inside the workspace pointing at `/`
  passes one trivially, and that is the standard way out of a directory jail. A
  Windows directory *junction* is refused by name rather than resolved:
  `filepath.EvalSymlinks` returns a junction's own path and reports success, so
  where it points cannot be established, and a path whose containment cannot be
  established is refused rather than assumed. A path that does not exist yet is
  allowed — that is what `breeze_new` is given — by resolving its nearest existing
  ancestor, which is where the new entry will actually land.

- **Generator path flags no longer escape the project.** `breeze_generate` forwards
  its `flags` object to the selected generator's `FlagSet` verbatim — deliberately,
  so `internal/generator` stays the authority on which flags each kind accepts. That
  also meant `{"flags": {"dir": "../../../../etc"}}` reached `--dir` and the
  generator wrote there. The workspace confines the directory a generator runs *in*;
  only the generator can confine what it then does with a path.

  `validatePathFlag` now guards `--dir` on `generate model` and `makemigration`,
  `--root` on `static` and `video`, `--dir` on `i18n`,
  `--views`/`--components`/`--layout` on `templates`, and `--file` on
  `breeze routes`. Absolute paths are refused outright rather than
  resolved-and-compared: one that happens to point inside the project is a value
  nobody types, so accepting it would add a second shape to validate for no benefit.

  A test derives the list of path-like flags from the feature registry rather than
  trusting one, so a new feature with a `--root` cannot inherit the hole silently.

  A second test covers the `breeze generate` kinds, which are a separate registry
  with their own `FlagSet`s — and it found `generate view --views` still unguarded
  after every feature flag was fixed. That one wrote its HTML template wherever it
  was pointed *and* embedded the path in the generated `router.View` call, so the
  running application would then have read templates from outside the project too.

- **No MCP tool can construct a shell command.** Every `exec.Command` in
  `internal/mcp` and `internal/generator` already passed an argument array, so no
  shell splits a caller-supplied value and `; rm -rf /` as a service name reaches
  Docker as one literal argument it rejects. That was true by inspection; it is now
  true by test. `TestNoToolStartsAShell` parses both packages and fails on an exec
  whose program is a shell, or which passes `-c` or `/c` anywhere.

  The property is worth pinning because its loss is not local. One
  `exec.Command("sh", "-c", ...)` written for a pipeline makes every identifier check
  in `docker_names.go` irrelevant at once, since those values were only ever safe as
  argv elements.

- **Provisioning cannot ask for host access, and cannot emit it either.** The
  orchestrator never granted a container a bind mount, `--privileged`, host
  networking or a shared namespace — there was no field for it. But the property held
  by absence, which is the weakest way for a security property to hold: a field added
  later, or a flag appended to `runContainer` for a plausible reason, would have
  granted host access with nothing objecting.

  Both directions are now closed. `dockerOptions` decodes with
  `DisallowUnknownFields`, so `{"docker": {"privileged": true}}` is refused with a
  message saying no such capability exists rather than being silently dropped — a
  dropped option looks to the caller like an honoured one, and leaves the audit
  question "can a tool request a privileged container" with the unsatisfying answer
  "it can ask, it just does not get one". And `dockerClient.exec` is now the single
  door every docker invocation passes through, checking the complete argv against the
  host-access flags: mounts, `--privileged`, `--cap-add`, `--device`,
  `--security-opt`, `--network`/`--pid`/`--ipc`/`--uts`, `--entrypoint`, `--user`,
  `--restart`, `--pull`. A test reads this package's source and fails on any call to
  the underlying runner that bypasses it.

  The strictness pays for itself outside security too: `wait_seconds` misspelled
  `wait_second` was previously ignored, so provisioning waited thirty seconds for an
  application the caller had explicitly told it not to wait for.

- **A provisioned container confines its own control plane.** `--workspace` confines
  the orchestrator and says nothing about the second `breeze-mcp` that provisioning
  starts *inside* each container — which runs in generator mode with the same tool
  set, in a filesystem holding `/etc`, `/usr`, the Go toolchain, and a Docker socket
  if one was mounted. The generated entrypoint now passes
  `--workspace /workspace`, the directory the project is copied into, and a test
  pins the flag and both Dockerfile variants' `WORKDIR` to the same constant so they
  cannot drift into a server confined to a directory the project is not in.

  The entrypoint also stopped word-splitting its own arguments. It built a
  `MCP_ARGS` string and expanded it unquoted, so a `BREEZE_MCP_SCOPE` of
  `runtime --allow-any-path` would have become two arguments and started that control
  plane unconfined. It now builds a positional list and invokes `breeze-mcp "$@"`.

- **Every filesystem call in `internal/mcp` is pinned to `resolvePath`.** Driving the
  16 path-taking tools proves those tools are confined; it cannot prove the absence of
  a seventeenth route to the filesystem, since a tool reading a path it derived rather
  than received would pass by not being in the list.
  `TestNoToolReachesTheFilesystemOutsideResolvePath` reads the package's own source
  and requires every `os.ReadFile`/`WriteFile`/`MkdirAll`/`Chdir`-family call to be in
  a function that either resolves the path itself or is named in an explicit list of
  functions whose caller already did — each with a written reason. The list is the
  audit; adding to it is a deliberate act rather than an omission.

- **Docker provisioning identifiers are validated against argument smuggling and
  registry redirection.** Nothing here was shell injection — `runDockerCommand`
  builds an argument array — but two other things were possible. A value beginning
  with `-` is read by Docker as a flag, so `--privileged` as a `container_name`
  landed in `docker run -d --name --privileged <image>` and Docker's parser took it
  as the flag it looks like: host-level capabilities, from a package that never
  offered a privileged option. And an `image_tag` naming a registry host
  (`evil.example/x`) combined with `skip_build` made the orchestrator pull and run an
  attacker's image on this host.

  `validateDockerOptions` is one function covering all four fields, so a third
  provisioning entry point cannot validate three of them. `docker.env` additionally
  refuses `BREEZE_MCP_SCOPE`, which the provisioned entrypoint honours and which
  therefore decides the new container's capability set — the orchestrator's decision
  rather than the request's, and the same reason `BREEZE_MCP_TOKEN` is already
  dropped in `prepare`.

- **`breeze_run_benchmarks` bounds what a benchmark request can ask for.** The
  `filter` reached `go test` as an argv element, so `-exec=/bin/sh` was read as Go's
  own flag — and `-exec` replaces the program that runs the compiled test binary.
  That is arbitrary execution with no shell anywhere in the path. A leading dash is
  now refused, with a message that names `-exec` rather than a character class.

  `benchtime` is bounded at 5 minutes and 100,000,000 iterations. `-benchtime 100h`
  is a request for a run that cannot finish inside the 10-minute benchmark timeout,
  so the effect was ten minutes of pinned CPU per call, repeatable — a denial of
  service on the host rather than a measurement. The duration limit is half the
  timeout so one maximal benchmark still leaves room for the rest of the suite, and a
  test asserts that relationship instead of leaving it in a comment.

- **Change sets are bounded.** `breeze_begin_change_set` has no required arguments —
  `project_path` defaults to the working directory — and each call copies the project
  into a temporary directory held until commit or discard, with nothing expiring it.
  That made a full project copy per call the cheapest denial of service in the
  package. At most 32 sets may be open and one set may hold at most 100 staged calls;
  both are checked before the expensive part, so a refusal prevents the copy rather
  than cleaning up after it.

- **`provision_fleet` bounds its services list.** Each entry is a generated project,
  a `docker build` with a 15-minute timeout, three or four allocated ports and a
  running container, provisioned sequentially. The limit is 12 — above any fleet this
  repository's own examples describe — and it is checked before the first port is
  allocated, so a rejected request has not already built anything.

- **The dashboard's API Explorer can no longer be used to fetch arbitrary URLs.**
  `POST /dashboard/api/api-explorer` took a URL and the *server* fetched it: a
  relative path was resolved against the request's `Host` header, and an absolute
  URL was passed through untouched.

  A request the server makes is not a request the caller could have made — it
  comes from inside the deployment, so it reached what the deployment reaches:
  `http://169.254.169.254/latest/meta-data/iam/security-credentials/` (cloud role
  credentials), `http://postgres:5432` and any other name that resolves only
  inside the cluster, and every port on the host including admin interfaces bound
  to loopback precisely because loopback was assumed unreachable. The response
  body came back verbatim in the JSON, so this was a read primitive rather than a
  blind one. The dashboard's own auth does not gate it: that auth is a no-op when
  `Username` or `Password` is empty, which is exactly what
  `breeze add dashboard --no-auth` generates.

  The target is now constructed rather than accepted. It is always `127.0.0.1` on
  the port the application is listening on; the caller's URL contributes only a
  path and query. An absolute URL is still accepted so the snippet panel's output
  can be pasted back, but by comparison — the host must be a loopback spelling and
  the port must be ours, and the request is rebuilt locally regardless, so no
  caller-supplied authority survives into the dial.

  This is a whitelist because a blacklist cannot work here: a hostname the
  attacker controls can resolve anywhere, and can resolve differently on the
  second lookup than the first, so checking the name is not checking the
  destination. Redirects are no longer followed either — a route on the service
  that reflects a parameter into a `Location` would otherwise reopen the hole
  through the front door. The redirect response is returned as-is, which is also
  what someone debugging a redirect wanted to see.

- **`middleware.JWTAuthMiddleware` refuses an empty `AccessSecret`**, and refuses
  `EnableRefreshToken` with no `RefreshSecret`.

  An empty string is a valid HMAC key. A middleware constructed without a secret
  did not reject every token, which is the intuitive reading and what the
  generated code used to claim — it *accepted* every token an attacker signed with
  `""`, carrying whatever claims they chose. `RequiredRoles: []string{"admin"}`
  was bypassable by anyone, because the key was public knowledge. Nothing in the
  request or the logs looked wrong.

  It panics at construction rather than returning an error or failing closed per
  request. A constructor's error is the kind that gets assigned to `_` in a setup
  function, and refusing every request turns a misconfiguration into an
  unexplained outage. This fails at startup, in the developer's terminal, naming
  the field — the same choice `oauth2`'s `mustNormalize` makes for the same
  reason. A test signs a token with `""` against the framework's own JWT library
  to demonstrate the forgery the refusal prevents.

- **`breeze add jwt` now exits when its secret variable is unset** instead of
  logging a warning and serving. The generated `setupJwt` called
  `log.Println("warning: … will reject every token")`, which was both wrong about
  the behaviour and too quiet about the consequence.

- **The `jwt` diagnostic probe reports how verification is configured, and no key
  material.** Algorithm, secret *lengths*, context key, required roles and which
  hooks are custom. It holds a copied `jwtFacts` value rather than a pointer to
  `JWTOptions`, so there is no path from a report back to a secret — this probe's
  output is served by `GET /dashboard/api/diagnostics`, whose auth may be a no-op.

  A secret under 32 bytes is reported as `degraded`, not as a note. HS256 with a
  key shorter than its hash output (RFC 7518 §3.2) is recoverable offline from a
  single captured token — no interaction with the service, no rate limit, no log
  entry — and a recovered key mints tokens with any claims. That is the same end
  state as the empty secret the constructor now refuses, a few CPU-hours later.

#### Added

- `breeze-mcp --workspace <dirs>` and `--allow-any-path`, also accepted by
  `breeze start mcp-server`. `--workspace` takes a comma-separated list of roots.
  They are mutually exclusive, the default is confine-to-CWD, and the startup banner
  states which applies.

- `Breeze.ListenPort()` reports the port passed to `Run`, or 0 before it is
  called. A subsystem can be handed the application without being handed its
  bootstrap; the API Explorer needed the real listener address, because deriving
  it from the `Host` header would have meant a caller-supplied header decided
  where the server connected — the same hole in a different field.

---

### Route Descriptions Reach Agents

#### Changed

- **A route's documentation now reaches the dashboard and the MCP tools, not just
  the OpenAPI document.** `Title`, `Description` and `Tags` from a `scalar.Doc`
  wrapper were only ever read by `scalar.Generate()`. Every other consumer — the
  Routes page, the API Explorer, `breeze_inspect_routes` — showed a method and a
  pattern, so the sentence the developer wrote about what an endpoint does was
  invisible to the two audiences most in need of it.

  `dashboard.RouteStat` gains `Summary`, `Description`, `Tags` and `Documented`,
  joined against the Scalar registry at read time by `describeRoute`. The join is
  deliberately not done at registration: copying summaries into the route
  accumulator would put documentation in a hot-path struct and freeze a route's
  description at its first request.

- **The API Explorer fills the input schemas it already declared.** `Summary` and
  `Tags` were declared on `APIExplorerRoute` and never populated; `explorerInputs`
  now derives body, query, param and header groups from the route's `RouteDoc`,
  with field types and per-field descriptions from `description:"…"` struct tags.
  The pattern-derived fallback is retained for undocumented routes, since a bare
  `:id` typed as string is still better than nothing.

- **`documented` is reported even when false.** A route absent from the Scalar
  registry is also absent from the generated OpenAPI document — so it is invisible
  to every client generator and every agent reading the spec. The dashboard and
  `breeze_inspect_routes` are in a position to say so and nothing else is, so the
  field is always sent and the MCP routes report carries a note explaining the
  consequence.

---

### Handler Error Returns

#### Breaking

- `breeze.HandlerFunc` is now `func(*Context) error`. Every handler and every
  middleware — including ones written against earlier versions — must return an
  error, and `ctx.Next()` now returns the error the rest of the chain produced.

  Before, a handler that failed had nowhere to say so. It wrote a status itself,
  or it logged and returned, and nothing above it could tell the difference
  between a request that succeeded and one that gave up. There was no single
  place to add error reporting, because there was no single thing being
  reported.

  **Migrating a handler.** Add the return type; return `nil` where it used to
  fall off the end, or return the body call directly:

  ```go
  // before
  router.Handle(breeze.GET, "/orders/:id", func(ctx *breeze.Context) {
      ctx.JSON(order)
  })

  // after
  router.Handle(breeze.GET, "/orders/:id", func(ctx *breeze.Context) error {
      return ctx.JSON(order)
  })
  ```

  **Migrating a middleware.** Propagate `ctx.Next()`. A middleware that
  discards it turns every downstream failure into a silent success:

  ```go
  // before
  func mw(ctx *breeze.Context) {
      ctx.Next()
  }

  // after
  func mw(ctx *breeze.Context) error {
      return ctx.Next()
  }
  ```

  A middleware with work to do after the handler holds the error and returns it
  once its own instrumentation has run, so a failing request is still logged or
  traced:

  ```go
  func mw(ctx *breeze.Context) error {
      start := time.Now()
      err := ctx.Next()
      record(time.Since(start)) // still runs when the request failed
      return err
  }
  ```

  A middleware that refuses a request writes its own response and returns
  `nil` — the refusal *is* the answer, so there is nothing for the error handler
  to do. Use `ctx.Abort()` as before to stop the chain.

- `Context.WriteString`, `Context.JSON`, `Context.HTML` and `Context.Render`
  now return an error, and `TemplateEngine.RenderView`/`RenderComponent` do
  too. The bodies they wrote before are still written, so ignoring the return
  leaves existing behaviour unchanged; propagating it gets the cause into the
  log instead of only into one user's browser.

#### Added

- `Breeze.ErrorHandler` — one hook, called once per failed request, that turns a
  returned error into a response. Leave it nil for the framework default.
- `breeze.HTTPError` with `NewHTTPError(status, message)` and
  `WrapHTTPError(status, message, err)`. This is how a handler says *404* rather
  than *something went wrong*: the framework cannot infer a status from an
  arbitrary error value, and guessing would turn a deliberate 404 into a 500 or
  the reverse. `Message` is sent to the client; the wrapped cause is logged,
  visible to `errors.Is`/`errors.As`, and never transmitted.
- The default error handler writes RFC 9457 problem+json, matching the shape
  `Bind` already produces so a client needs one error parser rather than two. A
  `*binding.ValidationError` keeps its field-level 422 — so the common
  `if err := ctx.Bind(&in); err != nil { return err }` does not downgrade into a
  generic 500 — an `*HTTPError` produces its own status, and anything else is
  logged to stderr and answered with a generic 500. An arbitrary error's text
  routinely names hosts, credentials and query fragments, so it is not sent to a
  remote caller unless the application chooses to.
- An error always produces a response. A custom `ErrorHandler` that writes
  nothing is corrected to a 500 rather than trusted, because the alternative is
  a connection that receives nothing at all.

#### Changed

- Auto-MCP tool results now carry `handler_error` when the route's chain
  returned an error, and the error is resolved to a response before the result
  is built — so an MCP tool call and an HTTP request to the same failing route
  report the same status and the same body.
- `Router.View`, `Router.Fragment` and `Router.EnableReRender` propagate render
  errors instead of discarding them. A template that fails to parse is a
  deployment fault, and it now reaches the operator's log.
- `breeze generate handler|middleware|resource` and both `breeze new` templates
  emit the new signature.

---

### Subsystem Diagnostics

#### Added

- **`diag` — a diagnostic registry every subsystem reports into.** One call now
  answers "what is every part of this process actually doing": `GET
  /dashboard/api/diagnostics`, or the `breeze_diagnose_service` MCP tool.

  Before this, a fact a subsystem knew was reachable only by holding a typed
  handle to that subsystem. The event bus knew its listener counts, the fleet
  tracer knew how many spans it had dropped, the template engine knew whether it
  was re-parsing on every render — and a diagnostic tool would have had to be
  handed every handle the application constructed. Nothing had the whole picture,
  so nothing could report it. An agent asking "why are my responses not
  compressed" had no way to find out that the middleware was never installed.

  Probes are registered for: events, observability, workflow, video (per mount,
  and per prefix as `video:<prefix>`), docs/scalar, router, workerpool, auto-mcp,
  websocket, templates, i18n, static, jsonrpc, migrate, oauth2, compression,
  etag, ratelimit, cors, security, jwt, locale, logging, recovery, fleet
  (including the no-op tracer, whose report is the answer to "why are there no
  traces"), and the dashboard itself.

  A test asserts the completeness both ways: every `breeze add` feature has a
  probe, and every probe's name is either a feature or a listed framework
  subsystem. `tuning` is the one exemption — it is two application setters, and
  both are reported inside the router probe's detail.

- **`diag` is a zero-dependency leaf package**, importable from any layer, so an
  application's own subsystems can join the same registry:

  ```go
  diag.Register("billing", func() diag.Report {
      if !gateway.Configured() {
          return diag.Off("no payment gateway is configured; call billing.Configure(key)")
      }
      return diag.OK(fmt.Sprintf("%d charge(s) settled", settled.Load()), nil)
  })
  ```

  It has to be a leaf. The import graph is `breeze → binding, rpc, scalar,
  internal/mcp`; `dashboard → breeze, events, observability, video`; `fleet →
  dashboard`; `workflow → events, observability`. A registry in the root package
  would be unusable by events, workflow, observability or scalar, and one in
  `dashboard` would be unusable by nearly everything.

- **Four statuses, not a boolean.** `off` means not installed, or installed and
  deliberately disabled — and it is not a fault. Collapsing it into "unhealthy"
  is what makes a dashboard lie: "installed and idle", "installed and unhappy"
  and "never wired up" are three different answers wanting three different next
  actions. Every `off` summary names the call that would turn it on. `unknown` is
  reserved for a probe that could not answer, which includes one that panicked —
  contained per probe, so one broken subsystem cannot hide the reports that would
  explain it.

- **`breeze_diagnose_service`** — the 40th MCP tool. It reads the whole registry,
  sorts degraded first (then unknown, then ok, then off — the order someone
  debugging reads in), summarises the statuses, and carries every subsystem's
  notes through verbatim. Optional `subsystem` reads one probe instead of running
  them all; optional `status` filters the list. Classified as `runtime`, so an
  existing `--scope runtime` token reaches it with no change, and served by the
  in-process endpoint.

#### Performance

- **Nothing is added to a request path.** Probes run only when someone reads
  them. Registration is one slice append per subsystem at startup, so a process
  that never reads its diagnostics pays one pointer per registered subsystem and
  not one instruction per request. The registry itself is copy-on-write behind an
  `atomic.Pointer`, so reading contends with nothing — including with another
  reader on a process that is already in trouble.

- **The counted facts are gated.** A few numbers cannot be recovered after the
  fact and must be counted as they happen: compression ratios, ETag hit rates,
  rate-limit rejections, static-file hits. Those go through `diag.Counter`, which
  reads one process-wide `atomic.Bool` before touching anything. With counting off
  the cost is a load of a global that is written approximately never — it sits in
  every core's cache in shared state, generates no coherence traffic, and the
  branch predicts perfectly. Unlike the increment it protects, it does not get
  worse as cores are added.

  Counting is off by default. `dashboard.Install` and `mcp.StartInProcess` enable
  it, since a process with either has already accepted per-request
  instrumentation; `diag.EnableCounters()` turns it on at runtime with no restart,
  and the reports then cover the window from that moment rather than pretending to
  cover the process's life.

  Unconditional atomics are used only where the unit of work already does I/O —
  workflow execution and step, video responses, JSON-RPC unknown-method calls,
  recovered panics, and the OAuth2 flow counters. In each of those the atomic is
  unmeasurable beside a file read or a round trip to a third party, and the number
  is one that must be trustworthy on a process that never enabled counting.

- **A zero is never ambiguous.** Every counter-backed report carries `counting`,
  so "nothing happened" cannot be read as "nothing was measured". Reports whose
  numbers are exact regardless — recovered panics, unknown JSON-RPC methods,
  OAuth2 logins — say so explicitly.

#### Changed

- `Router.ServeStatic` records its prefix and root for the `static` probe. A
  wildcard route gives no way to recover the directory behind it, and "which
  directory is this serving from" is the whole question when a static mount 404s
  for a file the developer can see on disk. The probe also reports the resolved
  absolute path and whether the directory exists — a missing root answers 404 for
  every request under its prefix with no error at startup.
- `middleware.RecoveryMiddleware` counts recovered panics and keeps the most
  recent value. A recovered panic returns a bare 500 and prints a stack trace to
  stdout; the middleware doing its job is precisely why nothing else in the
  process reports it.
- `rpc.NewServer` registers a probe reporting the method table. A JSON-RPC
  listener has no routes and its calls do not reach the dashboard, so a server
  whose methods were registered on a different `Registry` answers `-32601` to
  everything and looks like a client bug from the client's side. An empty method
  table is reported as degraded.
- `migrate.Runner.Up`, `Down` and `Status` record their outcome, and the probe
  reports it. A failed migration at startup leaves a process running and serving
  against a half-applied schema; `breeze migrate` prints to a terminal that is
  gone by the time anyone asks. The probe deliberately does not query — answering
  "how many are pending" needs a round trip, and a probe that queries hangs when
  the database is the problem, which is when the endpoint gets read.
- `oauth2` counts logins started against callbacks completed, per provider, and
  reports each provider's resolved redirect URL. Logins started with zero
  callbacks is the signature of every redirect-URL mismatch there is, and from
  inside the process nothing else looks like it: the login handler ran correctly,
  the browser left, and nothing came back.
- `mcp.StartInProcess` calls `diag.EnableCounters()`, so an embedded endpoint gets
  counted diagnostics without the dashboard — which is the normal shape for an
  app-runtime embed.

---

### Native Fleet Tracing

#### Fleet

- Added W3C Trace Context parsing/formatting, bounded baggage and custom span
  tags, root-only fixed-rate sampling, and lightweight always-sample-errors.
- Added non-blocking per-service tracers with bounded drop-oldest buffers,
  asynchronous batch export, heartbeats, timeout/retry discipline, and disabled
  zero-overhead fast paths.
- Added HTTP and in-process events transports plus isolated gnet/WebSocket
  compatibility adapters without adding a mandatory dependency; gRPC is
  explicitly planned rather than partially advertised.
- Added a standalone bounded in-memory Fleet Aggregator with authenticated span
  and heartbeat ingestion, service registry, TTL eviction, orphan/skew-aware
  trace assembly, topology percentiles, deterministic root-cause summaries,
  blast radius, and a multiplexed event stream.
- Added asynchronous live OpenAPI contract validation with bounded deduped
  violations and source-side sensitive-payload redaction.
- Added `go run ./cmd/fleet-aggregator` and the full Fleet guide at
  `docs/fleet-tracing.md`.

#### Dashboard

- Added capability-gated Fleet View as the 15th page. An empty
  `FleetAggregatorURL` keeps the nav and capability hidden; a configured but
  unreachable aggregator produces an explicit degraded state without affecting
  any existing dashboard API.
- Added server-side Fleet API proxying so aggregator credentials stay out of the
  browser, plus trace filtering support on dashboard logs.

#### JSON-RPC 2.0 (`rpc/`)

- Added a full JSON-RPC 2.0 implementation as a **peer protocol on gnet**, not a
  route on the HTTP router. It sits on the same event-loop primitives the HTTP
  layer uses, so it does not go through `net/http` and adds no reflection to the
  dispatch path.
- Covers single requests, notifications, batches, the five standard error codes
  (-32700, -32600, -32601, -32602, -32603) and the reserved
  application-defined range, with exact `id` echo including large-integer
  precision.
- Handlers register as `reg.Register(method, handler)`, mirroring how HTTP routes
  are registered, and compose middleware the same way. `RegisterBlocking` moves a
  method off the event loop for handlers that block.
- Frames JSON values off the raw byte stream, so pipelined, packed and
  split-across-reads messages all decode; a message cap closes runaway
  connections instead of buffering without bound.
- Added a stdio transport (`rpc.NewStdioServer`, `rpc.NewStdioServerOS`) over the
  same dispatcher, for peers that speak JSON-RPC over a pipe rather than a
  socket.

##### Benchmarks

Measured on a 12-core Windows box, `-benchtime=300ms`. Short runs, so treat
these as the right order of magnitude rather than settled figures:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `RegistryLookup` | **38.2** | **0** | **0** |
| `AppendErrorResponse` | 64.3 | **0** | **0** |
| `AppendResponse` | 114.2 | 2 | 1 |
| `NextValue` (framing only) | 144.4 | **0** | **0** |
| `HandleNotification` | 327.5 | 160 | 4 |
| `HandleSingleNoParams` | 477.7 | 216 | 7 |
| `HandleSingle` | 1331 | 329 | 15 |
| `OnTrafficSingle` (full event-loop path) | 2391 | 321 | 14 |
| `StdioSingle` | 1396 | 248 | 8 |

Batch dispatch is linear in N, which is the property worth pinning — a batch
should not degrade superlinearly:

| Batch size | ns/op | ns per request | allocs/op |
|---|---|---|---|
| N=1 | 1462 | 1462 | 20 |
| N=10 | 10,956 | 1096 | 164 |
| N=100 | 104,967 | 1050 | 1604 |
| N=1000 | 1,491,056 | 1491 | 16,010 |

The three pieces that are hot on every message — framing, method lookup, and
error serialisation — are all allocation-free. The remaining allocations are in
`encoding/json` unmarshalling of params and the result value, which is where the
next round of tightening belongs if the numbers ever justify it. Nothing has been
optimised speculatively beyond that point.

#### MCP server (`cmd/breeze-mcp`, `internal/mcp`)

- Added a Model Context Protocol server exposing Breeze's own tooling to an MCP
  client over stdio. It is built on the `rpc` dispatcher above rather than a
  second JSON-RPC implementation.
- Implements `initialize`, `notifications/initialized`, `tools/list` and
  `tools/call`, with five tools in this initial version: `breeze_new`,
  `breeze_generate`, `breeze_add`, `breeze_features` and `breeze_routes`. Each
  one calls the real generator rather than reimplementing it, so a tool call and
  the equivalent CLI invocation produce the same files.
- Generator progress is captured rather than written through, because stdout is
  the protocol stream on this transport and printed output would corrupt it.
  Diagnostics go to stderr.

#### MCP network transport and fleet provisioning

- Added MCP Streamable HTTP alongside stdio: `breeze-mcp --port 2000` serves the
  same registry, the same schemas and the same dispatcher over HTTP. No-flag
  behaviour is unchanged and covered by a regression test that asserts stdout
  carries nothing but JSON-RPC and stderr nothing at all.
- One process serves one transport, deliberately. The generators chdir and
  capture `os.Stdout` under a process-wide lock, so serving both at once would
  let a network tool call contend with stdio's own protocol stream.
- Verified against the published transport specification rather than from memory
  (2025-06-18, 2025-11-25 and 2026-07-28). What is implemented is Streamable HTTP
  for the handshake-based revisions, since this server's vocabulary is
  handshake-based; the 2026-07-28 revision's compatibility rules are followed for
  the mechanisms it removed — GET and DELETE answer 405, and a client announcing
  2026-07-28 is told this server is handshake-based rather than half-served.
- Network mode is authenticated and loopback-bound by default. A bearer token is
  required on every request including `initialize`, `Origin` is validated with an
  explicit 403, and `--host` is the only thing that widens the bind.
- Added Category H, four Docker-aware provisioning tools: `provision_service`,
  `list_provisioned_services`, `deprovision_service` and `provision_fleet`. They
  reuse the existing generation and Fleet-wiring paths — `generator.New` and
  `generator.ApplyConfig` — rather than reimplementing either, which is asserted
  by tests that read the generated tree.
- Provisioning returns `control_port`, `control_token` and `app_port` explicitly,
  never a bare `port`, plus the Fleet Aggregator's own separate address where one
  is hosted. The token is issued once, at provision time, is passed to the
  container by environment variable rather than argv, and is never stored — so
  `list_provisioned_services` cannot report it.
- One port allocator covers control, app and aggregator ports together, so no two
  addresses of any kind collide within an orchestrator's lifetime. The registry
  persists to a JSON file beside the binary and restores the allocator's
  reservations on restart, so a restarted orchestrator can still safely tear down
  what it created.

#### MCP in-process endpoint

- Added `mcp.StartInProcess(app, cfg)` / `mcp.ServeInProcess`: a generated
  application can serve an MCP control endpoint from its own process, beside its
  own traffic, with no separate `breeze-mcp` process. It wraps the same network
  server the binary uses, so the two cannot drift on defaults — bearer token
  required on every request, `Origin` validated, loopback unless told otherwise.
- It serves 15 of the 39 tools. The excluded 24 are excluded for two independent
  reasons, either of which is sufficient: they mutate process-global state
  (`chdir`, `os.Stdout`) under a lock, which would change the working directory
  of the server currently serving requests; and they need a source tree, which a
  `go build`-deployed binary does not have. `mcp.Tools()` and
  `mcp.ExcludedTools()` report both lists at runtime, and a test asserts the
  classification against the registry in both directions so a tool added later
  cannot default silently into either group.
- `breeze_routes` is classified workspace-only despite being read-only, because
  it runs through the generator and so takes the capture lock. Its live
  equivalent `breeze_get_routes` is in the in-process set.
- `InProcessConfig.AllowWorkspaceTools` restores the excluded set for a
  development container that runs from its own clone. It is a real risk, not a
  formality: the chdir window cannot be closed from inside the process, because
  the working directory is one shared value.
  `TestWorkspaceToolInProcessUnderLoad` documents that consequence rather than
  claiming safety.
- This is distinct from Auto-MCP. `app.EnableMCP(addr)` exposes the application's
  own `MCPTool`-tagged business routes so an agent can *call* the service;
  in-process exposes framework introspection so an agent can *understand* a
  running instance. Both can run at once, on different ports — `EnableMCP` now
  records its address and `StartInProcess` refuses that port with an error naming
  both features.
- Verified `-race` clean with app traffic and tool calls interleaved.

#### MCP server mode is required, with no default (breaking)

- **Breaking:** every MCP server construction path now requires a mode, and there
  is no default. `--mode generator` or `--mode app-runtime` on `breeze-mcp` and
  `breeze start mcp-server`; `Mode:` on `mcp.InProcessConfig`,
  `internal/mcp.NetworkConfig` and `internal/mcp.InProcessConfig`. Construction
  fails with an error naming both values if it is unset.
  - Migration: add `--mode=generator` to an existing `breeze-mcp` invocation or
    client `args`, and `Mode: mcp.ModeAppRuntime` to an existing
    `StartInProcess` config. Nothing else changes.
  - Why no default: `generator` would mean a deployed application that omitted the
    flag silently exposes project generation and Docker provisioning to whoever
    holds its token — a privilege escalation nobody selected. `app-runtime` would
    mean a developer's server silently lacks the tools they are using, reported as
    "unknown tool" for a documented tool, which reads as a broken build. One is
    dangerous and the other is confusing, so neither is assumed.
- The two modes differ **structurally**, not by configuration. A
  `mode: "app-runtime"` server has no generating, planning, verification or
  provisioning tool in its registry at all, so `tools/list` cannot advertise one
  and `tools/call` reports an unknown name. There is no scope check to
  misconfigure and no code path from an app-runtime server to a generator.
  `TestAppRuntimeRegistryExcludesMutatingTools` asserts it both by name and by
  walking the whole registry, so a tool added later is caught too.
- `initialize` now returns `breezeServerKind`, populated from the same `Mode` that
  decided what was registered — so it cannot disagree with `tools/list`. It carries
  a vendor prefix because it is a Breeze extension rather than a spec field. An
  agent reads it instead of inferring the server kind from a binary's name, a port
  number, or the shape of the tool list.
- `NewNetworkServer` refuses a `Server` built for one mode handed to a transport
  configured for the other, and `NewInProcess` refuses `AllowWorkspaceTools` in
  app-runtime mode rather than silently ignoring it — a caller who set it believes
  they enabled something.
- Added `portPurposeAppMCP`, a fourth port purpose, for a provisioned service that
  also exposes its own app-runtime endpoint (`docker.enable_app_mcp`, off by
  default). It comes from the same single allocator as the control, app and
  aggregator ports, in the same `allocateN` call so a partial failure releases what
  it took; it is persisted, re-reserved on restart, and released on deprovision.
  `provision_service` returns it as `app_mcp_port` / `app_mcp_url`.
  - It is deliberately *not* the control port. The control port serves the
    generator-level toolchain over that container's source tree; this one serves
    read-only introspection of the running process. Sharing one number would hand
    every agent that only wanted to read the ability to rewrite the project.

#### `breeze start mcp-server`

- Added `breeze start mcp-server`, the same MCP server reached from the CLI that is
  already installed. An agent working on a Breeze project has the `breeze` binary;
  requiring it to locate a second executable first is a discovery problem whose
  failure mode ("breeze-mcp: not found") reads as "MCP is unsupported".
- It is a second entrypoint, not a second implementation. Flag parsing, defaults,
  the loopback bind, token generation, the stdio-or-network rule and the startup
  banner all moved into `internal/mcpcmd` and both commands are a call into it.
  `cmd/breeze-mcp/main.go` shrank from ~194 lines to ~83.
- **Fixed:** the subcommand printed no startup banner. In network mode an operator
  had no way to see the generated token or confirm the endpoint address — the
  earlier shared-code refactor had covered server construction but left the
  announcement inline in `cmd/breeze-mcp`. Both now go through one `announce`.
  `TestEntrypointsPrintTheSameBanner` compares the whole normalised banner for six
  flag combinations, rather than checking that some output exists, because a weaker
  assertion would also have passed a subcommand printing something different.

#### Per-token capability scoping

- Added `--scope`, a second permission layer beside `--mode`. Mode decides what a
  server *has*; scope decides what a *credential* reaches. Both apply, and neither
  replaces the other: a `{fleet}` token on a generator server cannot provision, and an
  unscoped token on an app-runtime server still cannot generate because there is
  nothing registered to reach.
- Every one of the 39 tools is classified into exactly one of eight categories —
  `generation`, `introspection`, `planning`, `knowledge`, `verification`, `runtime`,
  `fleet`, `provisioning`. One category per tool, never two: a tool in two categories
  would mean either grant reaches it, which makes the narrower grant a lie.
  `TestEveryToolHasACapability` asserts the table and the registry agree in *both*
  directions, so a tool added later cannot go unclassified and a stale entry cannot
  name a tool that no longer exists.
  - Categories rather than tool names, because a token minted with 39 names would be
    stale the moment a tool was added, and whoever minted it would need the whole
    inventory.
  - An unclassified tool is withheld from every scoped token rather than granted. The
    failure mode of withholding is a missing capability, which is visible; of granting
    is an unreviewed tool reachable by a credential meant to be narrow.
- `initialize` now returns `breezeCapabilities` with `granted`, `known` and `scoped`.
  Both lists are always present: `granted` alone cannot tell an agent whether a tool it
  cannot find was never built or was withheld, and that difference decides whether to
  give up or ask for a wider credential. `scoped` distinguishes an unscoped token from
  one deliberately minted with all eight.
  - This is the primary mechanism, and the reason there is deliberately **no**
    `list_features` tool: an agent that has handshaken already has the answer, and a
    third way to ask one question is the one most likely to drift. It is sound because
    scope is fixed for a token's lifetime, so a handshake snapshot cannot go stale.
- `tools/list` omits out-of-scope tools, and `tools/call` refuses them as a **structured
  tool result** carrying `requires`, `granted`, `known` and `retry_will_succeed: false`
  — not a `-32602`. The request was well-formed and the tool does exist; the answer is
  "no". A parameter error would tell a model its call was malformed, which is false and
  invites it to reformat and retry indefinitely.
- Added `GET /mcp/features` for a person with curl, or tooling that would rather not
  implement a handshake to learn one fact. Same bearer token as `/mcp` — the report
  describes a credential's privileges — `GET` only, network mode only.
  `TestFeaturesEndpointAgreesWithTheHandshake` compares it against both the
  `initialize` payload and `tools/list`, so the three cannot disagree.
- `InProcessConfig.Scope` scopes an embedded endpoint, exported alongside
  `mcp.NewScope`, `mcp.ParseScope`, `mcp.UnscopedScope`, `mcp.Capabilities` and the
  eight `mcp.Cap*` constants so an application never needs the internal package.
- Omitting `--scope` grants everything, so every existing command line and embed is
  unaffected. Making it mandatory would break working deployments to protect a loopback
  default that is already the trust boundary. The risky combination is not silent: a
  generator-mode server on a non-loopback bind with an unscoped token is warned at
  startup, and the warning names `--scope` as the fix — then stops once a scope is in
  force, because a warning that fires after it has been acted on gets filtered out.
- The startup banner reports the scope either way, since "all capabilities" is a
  security-relevant fact and an operator who passed `--scope` needs to see it took.
- `--scope` applies on stdio as well, where it is not a credential boundary — the process
  boundary already is one — but the operator declaring what this subprocess should offer
  at all, which an editor configuration can legitimately want. Leaving it out would make
  the flag parse and do nothing on the transport most people use, and would make
  `breezeCapabilities` untrue on the one transport where `/mcp/features` cannot exist.

#### Provisioned containers build from local source

Five bugs, all in the same place and all invisible until a container actually ran.

- **Provisioning could not build from a development orchestrator at all.** The generated
  Dockerfile ran `go install  github.com/nelthaarion/breeze/v2/cmd/breeze-mcp@<pin>` and let
  the project's own go.mod resolve Breeze from the proxy. A working tree ahead of the
  newest tag — which it is, continuously — meant `module found, but does not contain
  package`, for `fleet/` and for `cmd/breeze-mcp` itself. An orchestrator could not
  provision a container running the code it was built from.
  - Fixed by copying the module's source into the build context under `breeze-src/` and
    adding a `replace`, whenever that source is on disk. Both binaries are then compiled
    from the tree that provisioned them. A released binary with no local clone falls back
    to the proxy and the version pin, which is correct there.
  - Compiled inside the image rather than copied in from the host: the orchestrator may
    be Windows or arm64 while the image is linux/amd64, and a copied executable produces
    a container whose control plane cannot exec.
  - Only what `go build` reads is copied — `.go` files, `go.mod`, `go.sum` and the
    `//go:embed` assets. `.git` and friends are skipped, because a build context is
    hashed and sent to the daemon whole.
- **The builder image was pinned to `golang:1.25-alpine`.** Two go.mod files in the
  context carry a `go` directive, and the golang images set `GOTOOLCHAIN=local`, so a
  toolchain upgrade on the host broke every provision with `go.mod requires go >= 1.26.4
  (running go 1.25.14)`. The tag is now derived from `runtime.Version()`, so the builder
  and the directive are the same number by construction.
- **The provisioned control plane never started.** Its entrypoint ran `breeze-mcp --port
  … --host 0.0.0.0` with no `--mode` — required since the mode work — so the process
  exited immediately. The container still reported healthy, because the probe targets the
  application, and the control port simply refused connections with nothing anywhere
  explaining why. The entrypoint now passes `--mode generator` and honours
  `BREEZE_MCP_SCOPE` for a scoped control token.
- **`provision_fleet` published an aggregator port nothing listened on.** The generator
  emits a *tracer*, never an aggregator, so the port was mapped into a container where no
  aggregator existed. Every service exported spans into a closed connection and the fleet
  reported an empty topology with no error to explain it. The image now also builds
  `cmd/fleet-aggregator`, and the entrypoint starts it only when `FLEET_AGGREGATOR_PORT`
  is set — on the one service `aggregator.hosted_by` names.
- **Services were pointed at the aggregator by an address they could not reach.**
  `FLEET_WRITE_URL` carried `127.0.0.1:<port>`, which inside a container is that
  container. Export is asynchronous and best-effort, so nothing failed: the fleet ran
  correctly and recorded nothing. Tracers now use `host.docker.internal`, and every
  container gets `--add-host host.docker.internal:host-gateway` so the name resolves on
  Linux as well as Docker Desktop. The reported `aggregator_url` stays host-side, because
  that is the address the caller connects to — the two are deliberately different
  spellings of one listener.
- **Fleet tools now accept a mount URL as well as an origin.** Every other place that
  names an aggregator names its mount: `fleet.TracerConfig.AggregatorURL`,
  `dashboard.Config.FleetAggregatorURL` and `provision_fleet`'s own returned
  `aggregator_url` are all `http://host:9000/fleet`. The tools took an origin plus a
  separate `base_path`, so pasting the value one already had produced
  `/fleet/fleet/api/…` and a 404 reporting that the aggregator feature was not installed
  — when it was, at the address given.

`TestLiveProvisionServiceReachableOnBothAddresses` and
`TestLiveProvisionFleetTracesFlowBetweenServices` now pass against a real daemon. They
were the only tests that could have caught any of this, which is why they exist.

#### Dashboard serves the minified bundles by default

- The dashboard layout linked `dashboard.css` and `dashboard.js` even though the
  minified pair was already generated and committed — about 55KB of comments and
  indentation per page load, paid by whoever opened the dashboard. It now links
  `dashboard.min.css` and `dashboard.min.js`, with `Config.DevMode` selecting the
  readable pair, matching what `TemplateConfig.DevMode` already did for the SPA
  runtime.
- Both files stay served under `/assets` either way, so a developer can still open
  the readable copy by URL. `TestLayoutReferencesAssetsThroughData` fails if a
  filename is hardcoded in the template again.

#### Test correction: SPA runtime invariants

- `TestSPARuntimeInvariants` asserted source-text properties of the SPA runtime
  against `breezeRuntime()`, which serves the **minified** bundle outside
  DevMode. esbuild renames locals and collapses whitespace, so all 19 snippets
  were absent from what the test read, and the failure said the runtime had
  regressed when the runtime was fine. It now reads the embedded `spa.js`
  directly — the file a regressing edit would touch — and
  `TestSPAMinifiedMatchesSource` remains what proves the shipped bundle came
  from that source, so no coverage is lost.
- Replaced the test's backtick check, which could never fire: the runtime is an
  embedded file rather than a Go raw string literal, so a backtick in it is
  harmless. The real hazard at that boundary is a `</script>` sequence, since
  `breezeRuntime()` wraps the bundle in a `<script>` tag; that is what the check
  now asserts.

#### CLI generator

- Extracted the generator out of `package main` into `internal/generator` with a
  small exported API, so the CLI and the MCP server drive the same code. CLI
  behaviour and flags are unchanged.
- The generator is driven by one unified `ProjectConfig` schema, settable from a
  YAML config file or from `--dotted.path` flags that resolve against the same
  struct, so both paths produce an identical configuration.

#### Documentation viewer: Scalar only

Swagger UI generation was **removed**, and Scalar is now the only documentation
viewer the framework ships. This is a change of direction: the earlier plan was
to keep Swagger UI available as an opt-in choice.

The reason for the change is that two viewers over one generated spec meant two
HTML generators, two sets of asset/CDN decisions and two things to keep working,
to render a document that is identical in both. Scalar renders the OpenAPI 3.1
output the `scalar` package already produces, so the second viewer was cost
without capability.

**Migration.** `middleware.SwaggerOptions` and `middleware.SwaggerMiddleware`
remain as deprecated aliases of `ScalarOptions`/`ScalarMiddleware`, so existing
code keeps compiling. What changes is what gets served: the UI path renders
Scalar. The spec endpoint is unaffected — `/openapi.json` still serves the same
document, so any external tool that consumes the spec directly (including
Swagger UI hosted elsewhere) keeps working unchanged.

#### Removed

- Removed `dashboard/spa.go`, `dashboard/spajavascript.go` and
  `dashboard/spa_css.go` (~1,400 lines). These held an unused
  JavaScript/CSS single-page dashboard: `dashboard.SPA()` had no callers
  anywhere in the repository, and the dashboard is served from the
  server-rendered templates in `dashboard/templates/` instead. Nothing
  referenced the removed symbols, so there is no migration.

---

## [v1.8.0] — Throughput

### Inline Handler Execution

Non-blocking handlers now run **directly on the `gnet` event-loop goroutine**
that read the request, instead of being dispatched through the worker pool.

`gnet` already runs one event loop per core and pins each connection to one,
so the parallelism was there before the pool got involved. Every request was
being pushed through a single `chan poolTask` guarded by one mutex that every
loop contended for — which re-serialised work that had already been spread
across cores, and charged two goroutine handoffs plus an `AsyncWrite` poller
wakeup for the privilege. For a handler that serialises a small JSON object,
that scheduling *was* most of the work being done.

**⚠️ Behaviour change.** A handler on the inline path must not block: the
event-loop goroutine it occupies is also serving every other connection
pinned to that reactor, so a SQL query or a file read stalls all of them.
Routes that block must be registered with the new **`Router.HandleBlocking`**,
which dispatches through the worker pool exactly as before.

```go
router.Handle(breeze.GET, "/users/:id", getUser)          // in-memory → inline
router.HandleBlocking(breeze.POST, "/orders", createOrder) // I/O → worker pool
```

Every I/O-bound route the framework ships was converted: static files, video
streaming, template `View`/`Fragment`/re-render, the whole dashboard, the
OpenAPI/Scalar endpoints, the OAuth2 login/callback/logout flow, and the
WebSocket upgrade. `Breeze.SetInlineExecution(false)` restores the previous
behaviour globally for applications that would rather not audit route by
route.

### Request Path

- **The per-event copy of the request bytes is gone.** `OnTraffic` used to
  copy every buffer out of `gnet` before parsing. It now parses `gnet`'s own
  read buffer in place and copies only when a partial request must be carried
  across events. Verified against `gnet`'s `Conn.Next` and `Conn.write`: the
  read buffer stays valid for the whole `OnTraffic` call because `write`
  never touches it.
- **`HTTPRequest` and its header map are pooled**, joining `Context`,
  `HTTPResponse`, and the route-parameter maps.
- **Response bytes are appended into a pooled wire buffer** on the inline
  path. `Conn.Write` is synchronous and has finished with the caller's slice
  by the time it returns — it either completes the syscall or copies the
  remainder into the connection's outbound buffer — which is what makes
  pooling those bytes safe. `AsyncWrite` does *not* copy, so the pooled
  dispatch path still serialises into a fresh slice.
- **`Bytes()` no longer walks a map** for the common case; a raw header blob
  is appended directly.
- **400/404/500 responses are precomputed** and written synchronously rather
  than built and pushed through `AsyncWrite` from inside the event loop.
- **Header keys are lowercased in place.** The old `toLowerASCII` scanned for
  an uppercase byte and returned a zero-copy view when it found none,
  allocating only otherwise — but that fast path never fired. Every header a
  real client puts on the wire is capitalised (`Host`, `User-Agent`, `Accept`,
  `Connection`, …), so the allocating branch was taken for essentially every
  header of every request, and it allocated **twice**: a scratch buffer and
  then the string conversion. A four-header `GET` paid eight heap allocations
  before it reached the router; a browser's eleven headers paid twenty-two.
  Keys are now lowercased in place inside the buffer the parser already owns,
  which costs one pass over bytes already in L1 and allocates nothing.
- **The WebSocket lookup is off the HTTP path.** `OnTraffic` opened with
  `wsConns.Load(c.Fd())`. `sync.Map` keys are `interface{}`, and the runtime
  only keeps preallocated boxes for integers 0–255 — so on any server busy
  enough for the file descriptors to reach the thousands, that conversion
  **heap-allocated once per request**, plus a hash probe, to answer a question
  that is "no" for every request on an HTTP-only server. The map is now behind
  an `atomic.Int64` count of upgraded connections, so the probe happens only
  once at least one WebSocket connection exists. `cleanupWS` decrements via
  `LoadAndDelete`, so the double-call path (a `Close` frame *and* `OnClose`)
  cannot drift the count negative.

### Zero-Copy Headers (opt-in)

With the above in place, an inline request had exactly one allocation left: the
copy of its header block into memory the request owns, which `Path` and every
header key and value point into. **`Breeze.SetZeroCopyHeaders(true)`** removes
that copy, so a request is parsed, routed, handled and answered with **zero
heap allocations by the framework**.

A few hundred bytes per request is not much on its own. At high request rates it
is a continuous stream of short-lived garbage, and the mark work for it lands as
GC assist on the very event-loop goroutines that are supposed to be serving
connections.

The flag is **off by default**, because it narrows the lifetime of every string
on the request: they become views into the connection's read buffer, which is
reused for the next read. Handlers are unaffected — a request bound for a worker
goroutine is re-parsed into owned memory first (`promoteRequest`), so `ctx.Req`
is always fully valid *inside* a handler. What changes is that keeping one of
those strings *after* the handler returns is no longer allowed without
`strings.Clone`. The failure mode is not a crash: the bytes stay readable, so a
retained string silently becomes a fragment of a later request — and as a map
key it stays in the bucket its original hash chose, so every subsequent lookup
misses.

Promotion is only paid by routes that need it. A blocking route is about to do
disk or network I/O, so a second pass over a few hundred bytes still in L1 does
not register; and when the buffer is one Breeze concatenated itself (a request
split across events), those bytes are already Go-owned and nothing is promoted
at all.

Three retention sites in the framework's own code were fixed so the shipped
middlewares hold to the new contract:

- `middlewares/cache.go` — the ETag cache used `ctx.Req.Path` directly as a key
  in a long-lived map. Now cloned.
- `dashboard/middleware.go` — the collector captured `Path`, `User-Agent`,
  the client IP and the selected header set into a `RequestRecord` that goes
  into a ring buffer and is marshalled later, possibly on the hub's goroutine.
  Now cloned. This is the collector's slow path by construction, so the copies
  only land on requests that are already being watched or already slow.
- `dashboard/collector.go` — `trackUniqueIP` inserted a string sliced out of
  `X-Forwarded-For` as a key in the unique-IP set. Now cloned on insert only,
  so an IP already seen stays allocation-free.

### Routing

- **O(1) exact-path lookup.** Routes are bucketed per HTTP method, and each
  bucket carries a map of static paths, so a non-parameterised route resolves
  in one hash probe instead of an ordered scan with a path split. Measured:
  **15ns against 92ns**.

- **Map eligibility is per-path, not per-bucket.** The map may only answer when
  the ordered scan would have reached the same route, because matching is
  first-registered-wins. The first version enforced that by closing the map as
  soon as any `:param` or wildcard route joined the bucket. Safe, but far too
  blunt: a single `ServeStatic` call registers a wildcard, and from that point
  every static route in the bucket fell back to the scan for the life of the
  process. In the sample app that was every hot `GET` — `/users`, `/ws/stats`,
  `/` — all paying 92ns instead of 15ns because `/files/*` happened to be
  registered before them.

  A dynamic route can only shadow a static route registered *after* it, so the
  precise test is whether any route already in the bucket also matches the
  candidate's path. That costs O(routes²) once at startup and nothing at
  request time. `/files/*` does not match `/users`, so `/users` keeps the map;
  `/users/me` registered after `/users/:id` correctly does not.

  `router_index_test.go` pins both directions, including a property test that
  re-runs every lookup with the map removed and asserts the scan agrees.

### Dashboard Fast Path

The dashboard middleware runs ahead of its own "is anyone watching" check, so
whatever it does there is paid by every request the server ever handles. Three
things it did there did not belong on a request path:

- **`trackDailyCount` formatted a date and took a global mutex per request.**
  `time.Now().UTC().Format("2006-01-02")` allocates a string and walks the
  layout every call, and the lock funnelled every event loop through one cache
  line to perform a single increment — the worst shape a lock can have, and a
  hard ceiling on how far throughput can scale with cores. Today's tally now
  lives in a pair of atomics keyed by UTC day number; the formatting and the
  map write happen once per day, at the rollover. Readers fold the live counter
  back in, so `TodayCount`, `DailyCounts` and the persisted state are unchanged.

- **`trackUniqueIP` took a write lock per request.** Behind any proxy —
  i.e. anywhere `X-Forwarded-For` is set — every request took a global
  exclusive mutex to re-learn an IP already in the set. The already-seen case
  now resolves under a read lock, with the write lock reserved for a genuinely
  new IP and re-checked once taken.

- **`requestsToday` counted the wrong thing, twice.** It was incremented on
  every request and never reset at a day boundary, so it held requests since
  process start — exactly what `requestsTotal` already held — while costing a
  second atomic read-modify-write in the same cache line as it. Removed; the
  sampler reports `TodayCount()`, which answers the question the field was
  named for.

- **Cache-line padding on the hot counters.** `requestsTotal` and the new
  `dayCount` are both bumped on every request from every event loop, so each
  now occupies its own line. Same reasoning as the worker pool's counters
  below; the cold ones (sampler-driven, per-session) still share.

### Worker Pool

- **Cache-line padding on the metrics counters.** Five adjacent
  `atomic.Int64` fields shared a single 64-byte line, so incrementing any one
  of them invalidated that line on every other core holding it. `submitted`
  and `queued` are both bumped on every successful `Submit`, and `Submit` was
  being called concurrently by every event loop at once — two guaranteed
  line-stealing read-modify-writes per request. Each counter now occupies its
  own line.

### Benchmarks

The root package's `bench_test.go` (then `zzperf_bench_test.go`) pairs each changed
stage against the approach it replaced, so a regression shows up as a number rather
than an argument. Measured on a 12-core Windows box, `-benchtime=2s`:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `ParsePublic` (exported, allocating) | 499.6 | 544 | 4 |
| `ParsePooled` (default) | 330.9 | 112 | 1 |
| `ParsePooledZeroCopy` | **275.6** | **0** | **0** |
| `LookupStaticMap` | **15.3** | 0 | 0 |
| `LookupOrderedScan` | 91.5 | 0 | 0 |
| `SerializeAlloc` (AsyncWrite path) | 72.0 | 128 | 1 |
| `SerializePooled` (inline path) | **31.9** | **0** | **0** |
| `PipelineDispatch` (old whole path) | 1892 | 1252 | 10 |
| `PipelineInline` | 1212 | 579 | 5 |
| `PipelineInlineZeroCopy` | **1196** | 466 | 4 |

Parse, route and serialize together are now **~325ns and zero allocations**.
Every allocation `PipelineInlineZeroCopy` still reports belongs to the handler.

#### Where the remaining time actually goes

That last point is the one worth acting on, so it has its own benchmarks. The
same two-field JSON payload, three ways:

| Handler style | ns/op | B/op | allocs/op |
|---|---|---|---|
| `json.Marshal(map[string]string{…})` | 777.8 | 512 | 8 |
| `json.Marshal(struct{…})` | 167.8 | 32 | 1 |
| hand-appended into a pooled buffer | **15.6** | **0** | **0** |

Marshalling a **map** costs more than twice the framework's entire request
path. `encoding/json` caches an encoder per type, so a struct writes its fields
directly, while a map is allocated, hashed, reflected over, and has its keys
sorted before a byte is written — for identical output. Prefer a struct in any
handler that is on a hot path; drop to hand-appending only where it measurably
matters.

#### A caution about end-to-end numbers

Local `bombardier` runs cannot resolve changes of this size, and it is worth
knowing that before drawing conclusions from one:

- The load generator competes with the server for the same cores. Dropping the
  server from `GOMAXPROCS=12` to `6` *raised* throughput (38.2k → 39.2k req/s),
  and two concurrent clients summed to the same total as one (44.2k vs 46.6k).
  The box saturates as a whole, well below what the framework can do.
- Run-to-run variance swamps the signal. Five consecutive runs of one unchanged
  binary measured 43.1k, 38.1k, 33.2k, 33.3k, 32.4k req/s — a **1.33x spread**,
  declining as the machine heats up. The first run of a session is always the
  fastest, so comparing a fresh baseline against a rebuilt candidate reliably
  invents a regression that is not there.
- For reference, `net/http` serving a precomputed body on the same box measured
  32.4k req/s against Breeze's 37.1k — three very different servers landing
  within 15% of each other is the harness reporting its own ceiling, not theirs.
- gnet has no `epoll`/`kqueue` on Windows and falls back to
  goroutine-per-connection, so these numbers say nothing about Linux behaviour
  either way.

Use the Go microbenchmarks above to evaluate request-path changes. For
end-to-end figures, drive the load from a separate machine.

---

## [v1.7.0] — Video Streaming

### New Features

#### Byte-Range Video Streaming (`video`)

A media handler that lets browsers **seek**, which is the whole difficulty:
a `<video>` element never downloads a file in one go — it reads the
container header, jumps to wherever the viewer clicked, and reads forward
from there. Each jump is a separate request carrying a `Range` header.
Ignore it and the video still plays, which is what makes the bug expensive
to find: the scrubber is simply dead.

- **`video.Mount(router, video.Config{Root: "./media"})`** registers `GET`
  and `HEAD` on `/videos/*filepath`. Correct `206 Partial Content` with
  `Content-Range`, `416` for unsatisfiable ranges, and `200` for malformed
  ones (which RFC 9110 requires be ignored rather than rejected).
- **Bounded memory**: one pooled chunk in flight per response (256 KiB
  default), so ten thousand viewers cost ten thousand chunks rather than
  ten thousand copies of the file. Open-ended `Range: bytes=0-` — which
  players routinely send — is capped at `MaxChunkSize` (4 MiB) so an
  opening request cannot pin an entire movie in memory.
- **Traversal defence ordered deliberately**: percent-decode *first* (or
  `%2e%2e%2f` walks past a literal-dot check), reject NUL and backslash,
  reject any `..` segment outright rather than cleaning it away — cleaning
  is safe but silently rewrites an attack into a legitimate-looking lookup,
  so neither the logs nor `Authorize` ever learn it happened — hide
  dotfiles, allow-list extensions, and only then touch the disk, proving
  containment against the symlink-resolved real path.
- **404, never 403**, for everything about a file's existence, so the
  filesystem cannot be mapped by watching which refusals differ. The real
  reason goes to `OnError`, never to the wire.
- **Optional signed, expiring URLs** (`video.Sign`) verified *before* any
  filesystem access, so an unauthenticated flood costs no disk I/O.
  Constant-time comparison, with the expiry inside the signed payload so it
  cannot be extended by editing the query string.
- **Conditional requests answered before the file is opened**, so a
  revalidation costs a stat instead of a transfer. `If-None-Match` takes
  precedence over `If-Modified-Since`, and `If-Range` is honoured so a
  resumed download cannot be silently corrupted.
- **Publishes `StreamServed` and an `observability.Signal`.** A viewer who
  closes the tab is reported as **cancelled, not failed** — in video that
  is the most common way a request ends, and counting those as errors would
  make a healthy server look like an outage.

Structurally this could not be a thin wrapper: `HTTPResponse.Bytes()`
always emits `Content-Length: len(Body)` and always writes `Body`, so a
`206` covering one slice of a file, a bodyless `304`, and a multi-write
stream whose head goes out before the bytes exist are all outside what it
can express. The package therefore takes over the connection and serialises
its own head.

#### Dashboard: Video page (`dashboard`)

- **`Collector.AttachVideo(bus) func()`** aggregates `StreamServed` **by
  file** — throughput, seeks, disconnects and errors per title. Separate
  from `AttachEvents` on purpose, so a dashboard with no media allocates
  nothing; every tracker method tolerates a nil receiver.
- Streaming breaks the one-row-per-request model the Live Requests feed
  assumes: a single viewer emits hundreds of range requests for one file,
  so that page shows the same filename repeatedly, interleaved with
  unrelated traffic — and it cannot report throughput at all, because
  bandwidth is a property of a stream, not of any one request.
- Rates use a fixed 10-second window rather than the span between samples,
  which would read two requests 3 ms apart as tens of MB/s of "sustained"
  throughput. `304`s are excluded so a well-cached file does not look slow.
  The file table is bounded with idle-first eviction, so path-probing
  cannot push a live stream off the page.

### Maintenance

- Toolchain moved to **Go 1.25.13**, which clears five standard-library
  advisories (`net/url`, `html/template`, `crypto/tls`, `encoding/asn1`,
  `net/http`). Both CI workflows resolve their toolchain with
  `go-version-file: go.mod`, so the `go` directive is the single source of
  truth — and it is also what `govulncheck` reads to decide which stdlib to
  analyse.
- `Dockerfile` `GO_VERSION` raised from `1.24`, which had fallen below the
  `go.mod` requirement. That failure is quiet rather than loud: the
  toolchain simply downloads the needed version, and the image stops
  matching the base layer it advertises.
- `golang.org/x/sync` → v0.22.0, `golang.org/x/sys` → v0.47.0.

---

## [v1.6.0] — Event System & Workflow Orchestration


### New Features

#### Event System (`events`)

A new internal communication layer for the framework: a typed,
allocation-conscious publish/subscribe system that lets any subsystem
(router, middleware, OAuth2, plugins, dashboard, scheduler, websocket)
observe and extend behaviour without importing each other. Zero external
dependencies.

- **Events are plain structs** — no interface, no embedding, no
  registration step. `type UserCreated struct { UserID uint64 }` is a
  complete event.
- **Generic, reflection-free dispatch**: `On`, `Once`, `Off`, `Before`,
  `After`, `Emit`. `reflect.Type` is used only as a map key to identify
  an event type — never to read fields, build values, or call handlers.
  A dispatch resolves to a strongly typed slice via one type assertion
  and calls each handler through a direct func value.
- **Two forms of every operation**: a package-level function on the
  `Default` bus, plus an explicit-bus form (`events.Emit(e)` /
  `events.EmitBus(myBus, e)`) for applications that want isolation. Go
  forbids generic methods, which is why the typed operations are
  functions taking a `*Bus`. Type-explicit variants (`OnType[T]`,
  `OnceTypeBus[T]`, …) are available where inference is unwanted.
- **Lock-free dispatch**: registration takes a mutex and publishes an
  immutable snapshot via `atomic.Pointer`; dispatch does a single atomic
  load and takes no lock. A listener added mid-dispatch does not affect
  the dispatch already in flight.
- **Ordering computed at registration, never during `Emit`**:
  before-hooks, then listeners by descending priority, then after-hooks.
  `Subscription.Priority` / `.Where` / `.Unsubscribe` configure a
  listener through a chainable handle.
- **Flow control**: returning `events.Stop` halts propagation;
  `ctx.Cancel()` stops the remaining listeners; `Config.ContinueOnError`
  chooses between failing fast and aggregating into a `*MultiError`.
- **Async emission** — `EmitAsync`, `EmitAsyncWait`, `EmitAsyncCtx` —
  with a selectable `AsyncMode` (`AsyncGoroutine` or `AsyncWorkerPool`),
  configurable `Workers` / `QueueSize`, and an `AsyncOverflow` policy
  (`OverflowBlock`, `OverflowSpawn`, `OverflowDrop`) for a full queue.
- **Panic recovery** with a configurable `PanicMode` and `PanicHandler`,
  so a panicking listener cannot take down the application.
- **Middleware** around every dispatch via `Use(...Middleware)`.
- **Introspection for the dashboard**: `List`, `CountEvents`,
  `CountListeners`, `HasEvent`, `SetName`/`GetName`, `Inspect` /
  `InspectAll` (registered listeners, priorities, execution order) and
  per-event `Metrics` (dispatch, listener, failure, panic, stopped and
  filtered counts; total/average/min/max duration; last dispatch time),
  per event type or aggregated for the bus via `TotalMetrics`.
- **Optional ring-buffer recorder** (`EnableRecorder`,
  `EnableRecorderWithPayload`, `History`, `DisableRecorder`) for
  debugging recent dispatches, off by default.
- **Built-in framework events** covering application lifecycle, router,
  HTTP, authentication, OAuth, plugins, websocket, scheduler and
  configuration reload, so modules can hook framework internals without
  patching them.
- **Tested**: unit, concurrency and race coverage for concurrent
  registration and dispatch, priorities, filters, once listeners,
  cancellation, panic recovery, middleware and the recorder. Passes
  `go test -race`; `go vet` clean; benchmarks included. Coverage 97.1%.

See `events/README.md` for full documentation.

#### Workflow Orchestration (`workflow`)

Durable, in-process orchestration for multi-step business processes,
with no broker and no required database.

- **Declarative steps with a validated DAG** — sequential by default;
  `WithDependsOn` opts into parallelism. Cycles, duplicate names,
  unknown dependencies and nil handlers are rejected at registration,
  and the graph is topologically sorted once rather than per execution.
- **Retries** with `BackoffExponential` and jitter, plus `NonRetryable`
  to end a doomed attempt sequence early while preserving the original
  error's identity under `errors.Is`.
- **Compensation (Saga)**: rollback runs in reverse over the steps that
  actually succeeded. A failing compensation handler does not stop the
  others — the remaining side effects still need undoing — and emits
  `WorkflowCompensationFailed`.
- **Timeouts and cancellation** per attempt and per execution, including
  cancellable retry backoff so shutdown never waits out a long delay.
- **Event triggers**: `workflow.OnType[UserRegistered](def)` starts an
  execution from a bus event, asynchronously, without blocking the
  emitter.
- **Durability and resume**: a nine-method `Store` interface with no
  driver or ORM behind it (`MemoryStore` by default). `Resume` continues
  interrupted executions and does not re-run completed steps.
- **Idempotency keys**, which is what makes at-least-once event delivery
  safe to trigger workflows with.
- **Observability**: every execution publishes framework events and one
  observability signal carrying each step as a span, so workflows render
  through the same dashboard pipeline as HTTP requests with no
  workflow-specific code.
- **Tested**: `go test -race` clean, `go vet` clean, benchmarks
  included. Coverage 95.3%.

See `workflow/README.md` for full documentation.

#### Live workflow view (dashboard)

The observability signal for an execution is written when it *ends*, so
it can only describe history — a workflow that is retrying with backoff
or waiting on a slow dependency is exactly the one worth watching, and
the signal cannot show it.

- The dashboard now assembles an in-flight view from step events as they
  arrive, exposed as `live` in the `/events` API payload. No
  configuration: attaching the dashboard to a bus is enough.
- Every step of the plan is present from the first frame, so the whole
  chain is visible immediately instead of materialising node by node.
  A step is `pending`, `running`, `done`, `failed`, `retrying` or
  `compensated`.
- A retryable failure reads as `retrying`, not `failed` — a step has no
  verdict until its final attempt. After a successful rollback,
  completed steps become `compensated` rather than staying `done`, so
  the UI never claims an effect that has since been undone.
- In-flight state is kept out of the observability ring buffer, where a
  half-finished execution would masquerade as history. Tracking is
  bounded (64 executions, finished ones evicted first) and finished
  executions are swept ~30s after they end.

---

## [v1.5.0] — OAuth2 / Social Login

### New Features

#### OAuth2 Authentication (`middlewares/oauth2`)

A new, zero-config OAuth2 / OpenID Connect middleware package that adds
social login with only `ClientID` and `ClientSecret` required — every other
setting has a secure default.

- **Four providers out of the box**: Google, GitHub, Microsoft, and Discord.
  Each is a self-registering `ProviderDriver` (via `init()`) that normalizes
  the provider's profile into a common `User` (stable `ID`, `Email`, `Name`,
  `Username`, `Avatar`, `Provider`) and handles its own quirks — Google
  offline-access consent, GitHub private-email fallback via `/user/emails` +
  numeric id, Microsoft `common` tenant with scope echo on the token request,
  and Discord avatar-hash → CDN URL expansion.
- **Six composable middlewares**: `Login`, `Callback`, `Auth`, `Optional`,
  `Refresh`, and `Logout`, plus context accessors `CurrentUser`,
  `CurrentToken`, `IsAuthenticated` (with `UserFrom` / `TokenFrom` aliases).
- **Secure by default**:
  - **PKCE** (S256) enabled for every provider.
  - **CSRF protection** — the `state` is stored in a signed, short-lived,
    single-use cookie and compared in constant time; the flow cookie is
    cleared on the callback so a state can never be replayed.
  - **Signed, HttpOnly, `SameSite=Lax` cookies**; `Secure` auto-enables for
    `https://` origins.
  - **Open-redirect guard** — `?redirect=` overrides accept same-site paths
    only (absolute and protocol-relative URLs are rejected).
  - **Algorithm-pinned HS256 JWTs** to prevent `alg=none` / algorithm-confusion
    attacks.
  - **Bounded provider calls** — every outbound token/userinfo request runs
    under a timeout so a slow provider can't pin a worker goroutine.
- **Two session modes** (`Config.SessionMode`): `SessionModeCookie` (default,
  signed HMAC cookie) and `SessionModeJWT`. Every write **rotates** the
  session so a pre-login cookie can't be reused post-login.
- **Transparent token refresh** — the `Refresh` middleware renews an expiring
  access token using the stored refresh token and rotates the session cookie,
  layered in front of `Auth`.
- **Shared pooled HTTP client** across all provider calls for connection reuse,
  overridable via `Config.HTTPClient` (injected in tests).
- **Tested**: the suite drives the full `Login → Callback → Auth` flow against
  an in-process mock provider, plus PKCE, cookie signing, JWT tamper-rejection,
  state expiry, config defaults, open-redirect protection, and concurrent
  registry access. Passes `go test -race`; `go vet` clean; benchmarks included.

See `middlewares/oauth2/README.md` for full documentation.

---

## [v1.4.1] — Performance Improvements & gRPC Codegen


### New Features

#### gRPC Code Generation (`breeze generate grpc`)

A new CLI generator scans the project for `_grpc.go` files, treats every
interface declared in them as a gRPC service, and generates the
corresponding server/client scaffolding, adapters, and boilerplate:

- Each method's call style is driven by a `grpc_type` comment annotation
  (`Unary`, `ServerSideStreaming`, `ClientSideStreaming`, `Bidirectional`)
  rather than a naming convention.
- New CLI usage: `breeze generate grpc <InterfaceName> [--force]`.
- Implemented across five new files: `generate_grpc.go` (detection +
  parsing), `generate_grpc_adapters.go`, `generate_grpc_files.go`,
  `generate_grpc_tags.go`, and a matching test suite
  (`generate_grpc_test.go`).

#### Editable Database Browser (dashboard)

The dashboard's Database Browser can now perform writes, not just reads:

- New `DBWriter` interface (kept separate from `DBInspector` so existing
  read-only integrations don't break) with `InsertRow` / `UpdateRow` /
  `DeleteRow`, plus a sentinel `ErrRowNotFound` error.
- New `Config.AllowWrites` gate — defaults to `false`, so writes must be
  explicitly opted into.
- New HTTP endpoints: `POST /rows`, `PUT /rows/:pk`, `DELETE /rows/:pk`,
  with support for composite primary keys via a new `parsePK` helper.
- Table cache is invalidated automatically after any write.
- `TableData` now reports a `writable` flag so the UI can conditionally
  show edit controls.
- Inline edit / delete / new-row UI added to the Database Browser
  frontend.
- NULL cells are no longer overwritten with an empty string when a cell
  is blurred without being edited.

### Performance Improvements

- **Worker pool overhaul** (`workerpool.go`, new `pool.go`): the pool now
  supports a configurable `OverflowPolicy` (`OverflowBlock`,
  `OverflowReject`, `OverflowSpawn`) instead of always spawning a
  goroutine on backpressure, plus a two-phase shutdown to avoid
  send-on-closed-channel races. `*Context`, `*HTTPResponse`, and route
  parameter maps are now pooled via `sync.Pool` (see `pool.go`) to cut
  per-request allocations.
- **Router**: `Router.findRoute` builds the middleware chain (global +
  route middlewares + handler) in a single allocation instead of the
  previous double-build through a `finalHandler` closure, and route
  parameter maps are drawn from a pool instead of freshly allocated on
  every match.
- **Response serialization**: HTTP status lines are now generated from a
  precomputed lookup table instead of per-request string building, and
  the response buffer is preallocated using an estimated size instead of
  growing incrementally.
- **Context**: response header maps are now shared/copy-on-write
  (`headersShared`) to avoid an allocation when a handler never mutates
  headers, and header/body writes (`WriteString`, `JSON`, `HTML`,
  `Status`) route through a shared `ensureResponse` helper. Added
  `ctx.GetHeader`.
- **Compression middleware**: broader rework of the post-`Next()`
  pipeline (building on the ordering fix from the previous release) to
  reduce redundant work on the hot path.

### Bug Fixes

- **`dashboard/auth.go`** — Dashboard Basic Auth / login previously used a
  random per-call salt when hashing the password, so a PBKDF2 hash of a
  correct password could never match the stored hash and valid
  credentials were always rejected. Fixed by hashing against a fixed,
  package-level salt (fixes #27).
- **`dashboard/mask.go`** — Secret masking only matched plain
  `key=value` / `key:value` tokens, so JSON-style log lines such as
  `"authorization":"Bearer xyz"` were never redacted and could leak into
  the dashboard's log buffer. `maskLine` now recognizes quoted JSON keys
  and values and redacts them while preserving surrounding JSON
  punctuation (fixes #28).

### Documentation / Housekeeping

- Removed the "Support the Project" donation section from `README.md`.
- Added design spec and implementation plan docs for the editable
  Database Browser feature (later removed from the tree — see "Removed
  docs" commit).
- Documented `DBWriter` and the editable Database Browser in
  `dashboard/README.md`.

---

## [v1.3.1]

### New Features

### Developer Dashboard (`/dashboard`)

A production-grade, native developer dashboard inspired by Laravel Telescope,
Horizon, and Grafana. Designed specifically for Breeze.

**Pages (13 total):**

- Overview — real-time cards (RPS, latency, error rate, goroutines, heap,
  CPU, cache hit ratio, queue jobs) + 4 live charts
- Routes Explorer — every registered route with per-route latency stats
- API Explorer — native API client (no Scalar redirect) with one-click
  code generation in curl / Go / JavaScript / Python / C# / PHP
- Live Requests — every incoming request with method/status/route/user
  filters, slow-request highlighting
- Database Browser — paginated table browser with column metadata and
  foreign-key relationships (read-only)
- ORM Query Monitor — every SQL with args, duration, rows, file:line,
  slow-query highlighting, expandable rows
- Cache Monitor — driver, keys, hits/misses, hit rate, clear/cache-prefix
- Queue Monitor — pending/running/completed/failed with retry button
- Scheduler — task name, cron, last/next run, status, run/fail counts
- Logs — five tabs (App / HTTP / Errors / Panics / Warnings) with search
- Health — configurable probes with green/yellow/red indicators
- Performance — Go runtime metrics (goroutines, GC, heap, stack, CPU,
  network) with 4 live charts
- Developer Timeline — per-request hierarchical profiler with
  expandable steps and metadata (the headline feature)

**Architecture:**

- Single WebSocket connection multiplexes all live updates (no polling)
- 1Hz metrics sampler pushes runtime stats into a 10-minute ring buffer
- Self-contained SPA: HTML + CSS + JS inlined into a single response
  (zero external dependencies, zero asset pipeline)
- Custom canvas charts (no Chart.js / D3 / npm)
- HTTP Basic Auth with constant-time password comparison (SHA-256 +
  subtle.ConstantTimeCompare)
- Secret masking for Authorization, Cookie, API-Key, Token, Password headers
  and key=value patterns in log lines
- Zero-overhead fast path: when enabled: false, the middleware returns
  immediately after ctx.Next() — no allocations, no locks
- Ring buffers bound memory for every collector (requests, queries, logs,
  timelines, metrics)

**Configuration:**

```yaml
dashboard:
  enabled: true
  timeline: true
  queries: true
  metrics: true
  requests: true
  base_path: "/dashboard"
  username: "admin"
  password: "s3cret"
  max_requests: 1000
  max_queries: 500
  max_logs: 1000
  slow_query_ms: 100
  slow_request_ms: 500
```

**Installation:**

```go
coll := dashboard.Install(app, router, dashboard.DefaultConfig())
router.Use(coll.Middleware())
```

See `dashboard/README.md` for full documentation.

---

### Bug Fixes

#### 1. `types.go` — HTTP Method Typo

**Bug:** `OPTION Method = "OPTION"` — missing the trailing `S`.
RFC 9110 defines the method as `OPTIONS` (7 characters).

**Impact:** All CORS preflight requests (`OPTIONS /path`) failed to match
the constant, causing 404s on every cross-origin browser request.

**Fix:** `OPTIONS Method = "OPTIONS"`

---

#### 2. `request.go` — `internMethod` Never Matched OPTIONS

**Bug:** `internMethod` had a case-6 branch checking for `"OPTION"` (6 bytes),
which is not a real HTTP method. Real OPTIONS requests are 7 bytes
(`"OPTIONS"`), so they fell through to `Method(string(b))` — an allocation.
Worse, the returned `Method("OPTIONS")` never matched the `OPTION` constant.

**Fix:** Removed the 6-byte branch. Added a case-7 branch using the same
zero-alloc byte-comparison pattern:

```go
case 7:
    if b[0] == 'O' && b[1] == 'P' && b[2] == 'T' &&
       b[3] == 'I' && b[4] == 'O' && b[5] == 'N' && b[6] == 'S' {
        return OPTIONS
    }
```

---

#### 3. `websocket_engine.go` — Use-After-Put in Close Frame

**Bug:** In the `wsOpClose` handler:

```go
code, reason := parseClosePayload(frame.payload)
wsFramePool.Put(frame)                                    // returned to pool
echo := buildWSFrame(wsOpClose, frame.payload)            // reads stale data!
```

After `wsFramePool.Put(frame)`, another goroutine calling `parseWSFrame`
could grab the same `*wsFrame` and overwrite `frame.payload`. The subsequent
`buildWSFrame` call would read corrupted data — a use-after-free in pooled
memory.

**Fix:** Reorder — use `frame.payload` first, then return to pool:

```go
code, reason := parseClosePayload(frame.payload)
echo := buildWSFrame(wsOpClose, frame.payload)
wsFramePool.Put(frame)
```

---

### 3b. `websocket.go` — RFC 6455 Control Frame Validation

**Bug:** `parseWSFrame` did not enforce RFC 6455 §5.5 requirements for
control frames (Close, Ping, Pong):

1. Control frames MUST have a payload ≤ 125 bytes (no extended length
   encoding allowed).
2. Control frames MUST NOT be fragmented (FIN must be 1).

A malicious client could send an oversized or fragmented control frame,
which the parser would accept — potentially causing excessive memory
allocation or confusing the defragmentation logic.

**Fix:** Added validation early in `parseWSFrame`, after the opcode and
initial payload length are parsed:

```go
isControl := opcode >= wsOpClose
if isControl {
    if payLen > wsMaxControlPayload {
        return nil, -1 // control frame payload exceeds 125 bytes
    }
    if !fin {
        return nil, -1 // control frames must not be fragmented
    }
}
```

Also added a defensive invariant check using `wsMaxFrameHeader` (14 bytes =
2 + 8 + 4) to validate the parsed header size never exceeds the maximum:

```go
if offset > wsMaxFrameHeader {
    return nil, -1
}
```

This also silences the `unusedfunc` warnings for `wsMaxControlPayload` and
`wsMaxFrameHeader` — both constants are now referenced in the parser.

---

#### 4. `context.go` — Added Typed Store (Set/Get/MustGet)

**Why:** Needed for the JWT fix (#8 below). The existing `params` field is
`map[string]string`, which can't hold structured data like `jwt.MapClaims`.

**Added:**
```go
type Context struct {
    // ... existing fields ...
    store map[string]any  // lazy-initialized, nil until first Set
}

func (ctx *Context) Set(key string, val any)
func (ctx *Context) Get(key string) (any, bool)
func (ctx *Context) MustGet(key string) any
```

**Performance:** The `store` field is `nil` until the first `Set` call —
zero allocation for requests that don't use it. Same pattern as Gin/Echo/Fiber.

---

#### 5. `middlewares/compression.go` — Pre-Next Ordering Bug

**Bug:** The middleware checked `ctx.Res` **before** calling `ctx.Next()`.
At that point `ctx.Res` is always `nil` (handler hasn't run), so the
middleware short-circuited and **compression never ran** — the entire
feature was dead code.

**Fix:** Call `ctx.Next()` first, then post-process the response.

**Additional improvements:**
- Early-return on empty `Accept-Encoding`
- Early-return if `Content-Encoding` is already set (prevent double-compress)
- Added `Vary: Accept-Encoding` header for proper cache behavior
- Properly check `Close()` return value

---

#### 6. `middlewares/cache.go` — ETag Ordering + Query Key Collision

**Bug 1 (ordering):** Same as compression — checked `ctx.Res` before
`ctx.Next()`, so ETag generation never ran.

**Bug 2 (key collision):** The cache key was `ctx.Req.Path` only, so
`/api/users?page=1` and `/api/users?page=2` shared the same ETag entry,
causing false 304s.

**Fix:**
- Call `ctx.Next()` first, then compute ETag from the fresh response body
- Include query string in the cache key (only allocates when a query exists)
- Use `RLock` for the If-None-Match pre-check (concurrent 304 checks)
- Pre-check: skip the handler entirely on a known ETag match

---

#### 7. `middlewares/cors.go` — Missing Abort() on OPTIONS

**Bug:** On OPTIONS preflight, the middleware called `return` without
`ctx.Abort()`, leaving `ctx.index` at its current position. If any code
later called `ctx.Next()` on the same context, the chain would resume past
the CORS short-circuit.

**Fix:**
```go
if ctx.Req.Method == breeze.OPTIONS {
    ctx.Status(204)
    ctx.Abort()
    return
}
```

---

#### 8. `middlewares/rate_limiter.go` — Lock Held Across Next()

**Bug (critical performance):** The middleware held `mu.Lock()` with
`defer rl.mu.Unlock()` across `ctx.Next()`:

```go
rl.mu.Lock()
defer rl.mu.Unlock()
// ... counter update ...
ctx.Next()  // ← handler runs under lock!
```

This serialized **every request** through a single mutex, completely
defeating the WorkerPool's concurrency. A 16-core server would process
requests one at a time.

**Fix:**
- Do the map lookup + counter update under the lock
- Release the lock before `ctx.Next()`
- Pre-compute the 429 message at construction time (avoid `fmt.Sprintf`
  on every rejected request)

**Impact:** Before — lock held for entire handler duration (ms to seconds).
After — lock held for map ops only (microseconds).

---

#### 9. `middlewares/jwt.go` — Claims Stored as Unparseable String

**Bug:** The middleware stored JWT claims via:

```go
ctx.SetParam(opts.UserContextKey, fmt.Sprintf("%v", claims))
```

`fmt.Sprintf("%v", map[string]any{...})` produces a Go-specific
representation like `map[exp:1234 role:admin user_id:42]`. Downstream
handlers could not parse this back into structured data.

**Fix:** Use the new typed store:

```go
ctx.Set(opts.UserContextKey, claims)
```

Handlers retrieve claims with a type assertion:

```go
claims, ok := ctx.Get("user").(jwt.MapClaims)
```

---

<!-- The "File Inventory" section that stood here has been deleted.

     It described a zip-distribution layout — "Replace your entire breeze/
     directory with these files" — from before this project was a Go module. It
     listed a `breeze-final/` tree that has never existed in the repository, and
     labelled files "ORIGINAL (unchanged)" that have changed many times since.

     A changelog entry describing a historical state is fine; an inventory
     claiming to describe the current file set is not, because a reader has no way
     to tell it is stale. `go doc ./...` and the repository itself are the
     inventory now, and docs/repository-structure.md is the map. -->
