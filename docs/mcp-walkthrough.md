# Breeze MCP walkthrough

`breeze-mcp` exposes the Breeze toolchain to an AI agent over the Model Context
Protocol. This document is the operator's guide to pointing a client at it — one
instance or many.

> **Status.** This is the structural guide to the transports, the address kinds
> and the client configuration. The runnable end-to-end example
> (`cmd/mcp-example`) is deliberately not built yet; the sections marked
> *deferred* below name what it will demonstrate, so the shape of this document
> does not have to change when it arrives.

## The four address kinds

Everything below depends on keeping four separate addresses separate. They are
never the same port, and no tool call, return value or example here is ambiguous
about which one it means.

| Kind | What listens there | Who dials it | Named by |
|---|---|---|---|
| **Control address** | a `breeze-mcp` process in `--mode generator` | the MCP client, as a configured server | `control_port` / `control_token` |
| **App address** | the generated Breeze application itself (`app.Run(port, …)`, or its Docker-mapped equivalent) | Category C/D tools, as an *argument* | `app_port` |
| **App-MCP address** | the application's own embedded `app-runtime` endpoint | the MCP client, as a second configured server | `app_mcp_port` |
| **Aggregator address** | the Fleet Aggregator's own API/WS endpoint | Category D tools, as an *argument* | the aggregator entry in a fleet registry |

The first and the third are both MCP, which is exactly why they must not be
conflated: the control address serves the generator-level toolchain over that
container's source tree, so whoever holds its token can rewrite the project. The
app-MCP address serves read-only introspection of the running process and cannot
change anything — it has no mutating tool registered. Point an agent at the third to
ask what a service is doing; it needs the first only to change what the service *is*.

The distinction that causes the most confusion is between the first two, so it is
worth stating directly:

- A tool **call** travels *through* a control-plane connection. When an agent
  invokes `breeze_add` against the server named `users-service`, the JSON-RPC
  message goes to `users-host:2000` — that service's `breeze-mcp` control
  address. The agent never types that address into a tool argument; it is
  configured once, as a server URL.
- A tool **argument** such as `service_url` or `aggregator_url` points at
  something else entirely: the running application's own address, or the
  aggregator's. `breeze_get_routes(service_url: "http://users-host:8080")`
  reaches the *application* on its app port, and it does so via whichever
  control-plane connection the agent happened to make the call through.

So one question — "what routes is the users service serving?" — involves two
addresses at once: `users-host:2000` carries the call, and
`http://users-host:8080` is what the call is about. Conflating them produces the
most misleading failure available, because a `breeze-mcp` control port answers an
app-port request with a well-formed MCP rejection rather than a connection error.

The aggregator address matters as soon as Fleet is involved. The service hosting the
Fleet Aggregator can have all four: its own control port, its own app port, its
aggregator port, and — if `enable_app_mcp` was requested — its app-MCP port.
`breeze_get_trace(aggregator_url: …)` takes the aggregator's, and nothing infers it
from the others.

## Two kinds of server: `--mode`

Before anything else, every MCP server Breeze starts must be told which of two
kinds it is. There is no default, and construction fails without one.

| | `--mode generator` | `--mode app-runtime` |
|---|---|---|
| Answers | "help me build and change this project" | "help me understand what this instance is doing right now" |
| Tools | the full toolchain — generate, plan, verify, provision, plus every read-only tool | live introspection only |
| Mutating tools | registered | **not registered at all** |
| Belongs | a developer's machine, a build agent | inside a deployed process |

The distinction is structural, not configuration. An `app-runtime` server does not
check a permission before running `breeze_generate` — it has no `breeze_generate` to
run. No token scope, no misconfiguration and no argument trick reaches one, because
there is nothing to reach.

Why no default: choosing `generator` would mean a deployed app that forgot the flag
silently exposes project generation and Docker provisioning to whoever holds its
token. Choosing `app-runtime` would mean a developer's server silently lacks the
tools they are trying to use, reported as "unknown tool" for a tool the docs promise.
The first is dangerous, the second is confusing, so neither is assumed.

The handshake reports it back, so an agent can confirm what it reached rather than
inferring from the binary's name:

```json
{
  "protocolVersion": "2024-11-05",
  "capabilities": { "tools": { "listChanged": false } },
  "serverInfo": { "name": "breeze", "version": "v0.1.0" },
  "breezeServerKind": "generator"
}
```

The same handshake from an embedded endpoint:

```json
{
  "protocolVersion": "2024-11-05",
  "capabilities": { "tools": { "listChanged": false } },
  "serverInfo": { "name": "breeze", "version": "v0.1.0" },
  "breezeServerKind": "app-runtime"
}
```

`breezeServerKind` is a Breeze extension, not part of the MCP specification — hence
the vendor prefix. It is populated from the same `Mode` value that decided which
tools were registered, so it cannot disagree with `tools/list`.

## What a token may do: `--scope`

`--mode` decides what a server *has*. `--scope` decides what a *credential* reaches.
They are independent layers and both are worth using.

Every tool belongs to exactly one of eight categories:

| Category | What it does |
|---|---|
| `generation` | writes project files — `breeze_new`, `breeze_generate`, `breeze_add` |
| `introspection` | reads what exists in a project |
| `planning` | previews changes, holds change sets open |
| `knowledge` | maintains and searches `llms.txt`, suggests next steps |
| `verification` | runs the Go toolchain — and therefore project code |
| `runtime` | reads live state from a running service over HTTP — routes, errors, logs, performance, and `breeze_diagnose_service`, which returns every subsystem's own diagnostic report |
| `fleet` | reads a Fleet Aggregator: traces, topology, contract violations |
| `provisioning` | drives Docker: builds images, starts and removes containers |

A token minted for a CI job that only reads traces needs one of them:

```bash
breeze-mcp --mode generator --port 2000 --scope fleet
# breeze-mcp: mode generator (full toolchain: generates and changes projects)
# breeze-mcp: token scope fleet
```

Several are comma-separated, and the order does not matter:

```bash
breeze-mcp --mode generator --port 2000 --scope fleet,runtime
```

Omitting `--scope` grants every category, which is what every existing command line
already did. Scoping is a hardening step taken deliberately; making it mandatory would
break working deployments to protect a loopback default that is already the trust
boundary. What is *not* silent is the risky combination — a generator-mode server on a
non-loopback bind with an unscoped token says so at startup, and stops saying it once
you have acted on it.

### Categories rather than tool names

A token minted with 40 tool names would be stale the moment a tool was added, and
whoever minted it would have to know the whole inventory. Granting `fleet` does not
require knowing that `breeze_explain_incident` exists.

One category per tool, never two. A tool in two categories would mean either grant
reaches it, which makes the narrower grant a lie.

### How an agent finds out

At handshake time, with no extra call:

```json
{
  "protocolVersion": "2024-11-05",
  "capabilities": { "tools": { "listChanged": false } },
  "serverInfo": { "name": "breeze", "version": "v0.1.0" },
  "breezeServerKind": "generator",
  "breezeCapabilities": {
    "granted": ["fleet", "runtime"],
    "known": ["fleet", "generation", "introspection", "knowledge",
              "planning", "provisioning", "runtime", "verification"],
    "scoped": true
  }
}
```

Both lists are always present. `granted` alone cannot tell an agent whether a tool it
cannot find was never built or was withheld from its token — and that difference decides
whether the right move is to give up or to ask for a wider credential. `scoped`
distinguishes an unscoped token from one deliberately minted with all eight.

This is sound because scope is fixed for a token's lifetime: it is set when the token is
minted and nothing changes it mid-session, so a snapshot taken at handshake cannot go
stale. There is deliberately no `list_features` tool — an agent that has handshaken
already has this, and a third way to ask one question is the one most likely to drift.

### Checking a token by hand

For a person with curl, or for tooling that would rather not implement an MCP
handshake to learn one fact:

```bash
curl -H "Authorization: Bearer $BREEZE_MCP_TOKEN" \
  http://127.0.0.1:2000/mcp/features
```

```json
{
  "server_kind": "generator",
  "granted": ["fleet", "runtime"],
  "known": ["fleet", "generation", "introspection", "knowledge",
            "planning", "provisioning", "runtime", "verification"],
  "scoped": true,
  "tools": ["breeze_diagnose_service", "breeze_explain_incident",
            "breeze_get_contract_violations",
            "breeze_get_logs", "breeze_get_performance", "breeze_get_recent_errors",
            "breeze_get_routes", "breeze_get_topology", "breeze_get_trace",
            "breeze_get_traces", "breeze_query_openapi", "breeze_simulate_request"],
  "note": "This endpoint is a convenience for humans and external tooling. …"
}
```

It requires the same bearer token as `/mcp` — the report describes a credential's
privileges, which is not something an anonymous caller should be able to enumerate — and
it answers `GET` only. Network mode only: on stdio there is no HTTP surface to mount it
on, and the handshake carries the same information there anyway (`--scope` applies on
stdio too; see "One instance: stdio"). A test compares this response against the
handshake payload and against `tools/list`, so the three cannot disagree.

### What a refused call looks like

Out-of-scope tools are absent from `tools/list`. A client working from a cached list, or
an agent guessing, will call one anyway. The refusal is a **tool result**, not a
JSON-RPC error:

```json
{
  "isError": true,
  "structuredContent": {
    "tool": "provision_service",
    "refused": true,
    "reason": "outside this token's granted capabilities",
    "requires": "provisioning",
    "granted": ["fleet", "runtime"],
    "known": ["fleet", "generation", "introspection", "knowledge",
              "planning", "provisioning", "runtime", "verification"],
    "retry_will_succeed": false
  }
}
```

`-32602` would tell a model its request was malformed, which is false and invites it to
reformat and try again indefinitely. This says what capability is needed, what the token
has, and that retrying will not help — so the agent reports the missing grant to a human
instead of looping.

### Scope and mode together

```bash
# a deployed app: no mutating tools exist, and this token only reads traces
breeze-mcp --mode app-runtime --port 2000 --scope fleet
```

An app-runtime server with an unscoped token still cannot generate — there is nothing
registered to reach. A generator server scoped to `{fleet}` still cannot provision — the
tool exists, and this credential does not reach it. The two layers are reported
separately (`breezeServerKind` and `breezeCapabilities`) precisely so a missing tool can
be attributed to the right one.
## Where a tool may reach on disk: `--workspace`

`--mode` decides what tools exist. `--scope` decides which of them a credential reaches.
Neither says *where* they may operate, and that is a separate question because most of
these tools take a path.

```bash
breeze-mcp --mode generator                               # confined to the CWD (default)
breeze-mcp --mode generator --workspace /srv/projects      # one tree
breeze-mcp --mode generator --workspace /srv/a,/srv/b      # several
breeze-mcp --mode generator --allow-any-path               # no confinement
```

### Why this is not just tidiness

`breeze_verify_project` and `breeze_run_benchmarks` run `go test` in the directory they
are given. `go test` compiles and executes the code it finds there. So an unconfined
server handed `{"path": "/etc"}` — or any directory containing a Go module — ran that
module's code under the server's own identity and returned its output as a tool result.
No injection was involved; the tool did exactly what it was asked.

Confinement is therefore not a per-tool check. `resolvePath` is the only way any tool in
the package turns a caller's path into one it will touch, so a new tool cannot forget it:
there is no second way to get a usable path.

### What is refused

- A path outside every root, whether reached absolutely (`/etc`) or by traversal
  (`../../..`).
- A path that resolves outside through a **symlink**. Both the roots and the candidate are
  symlink-resolved before comparison, because a prefix test is not a containment test — a
  link inside the workspace pointing at `/` satisfies it trivially.
- A path through a **Windows directory junction**. `filepath.EvalSymlinks` returns a
  junction's own path and reports success, so where it points cannot be established; a
  path whose containment cannot be established is refused rather than assumed.

### What is allowed

- A path that does not exist yet, which is what `breeze_new` is given. Its nearest
  existing ancestor is resolved and checked instead, because that is where the new entry
  will actually land.
- A root declared through a symlink. `/tmp` is a symlink to `/private/tmp` on macOS, so
  both spellings have to name the same workspace.

### Generator flags are confined separately

`breeze_generate` forwards its `flags` object to the generator's own FlagSet verbatim —
deliberately, so `internal/generator` stays the authority on which flags each kind
accepts. That means `{"flags": {"dir": "../../.."}}` reaches `--dir`. The workspace
confines the directory a generator *runs in*; the generator itself refuses a path-like
flag that escapes the project, and a test derives the list of such flags from the feature
registry rather than trusting one.

The `breeze generate` kinds are a second registry with their own FlagSets, covered by
their own test. `generate view --views` was found unguarded by it: that flag both wrote
the HTML template and was embedded in the generated `router.View` call, so the running
application would have read templates from outside the project as well.

### What a provisioned container may reach

`provision_service` starts a second `breeze-mcp` inside each container it creates. That
one is a full generator-mode server, and the container's filesystem is not a boundary —
`/etc`, `/usr`, the Go toolchain and a mounted Docker socket are all inside it. So the
generated entrypoint passes `--workspace /workspace`, the directory the project source is
copied into, and a test pins that flag and both Dockerfiles' `WORKDIR` to one constant.

Nothing in a provisioning request can widen what that container reaches:

- **No host access can be requested.** The `docker` object decodes strictly, so
  `{"docker": {"privileged": true}}`, `volumes`, `network_mode`, `cap_add`, `devices`,
  `security_opt`, `pid_mode`, `entrypoint` and the rest are *refused* rather than
  ignored. A silently dropped option looks to the caller like an honoured one.
- **No host access can be emitted.** Every `docker` invocation passes through one
  function that checks the complete argv for mount, privilege, namespace and entrypoint
  flags. The orchestrator has never built such a command; the check is what keeps that
  true when a field is added later, and a test fails on any call that bypasses it.
- **The entrypoint does not word-split its arguments.** It builds a positional list and
  runs `breeze-mcp "$@"`, so a `BREEZE_MCP_SCOPE` containing a space cannot inject a
  second flag — `runtime --allow-any-path` would previously have become two arguments.

There is no opt-in for a host mount. A provisioned container carries its own copy of the
project, which is why one never came up; an operator who genuinely needs one has
`docker run`, which is a deliberate act by someone who already holds Docker access rather
than a JSON field an agent can populate.

### No tool builds a shell command

Every `exec.Command` in `internal/mcp` and `internal/generator` passes an argument array,
so no shell splits a caller-supplied value: `; rm -rf /` as a service name arrives at
Docker as one literal argument, which Docker rejects. A test parses both packages and
fails on an exec whose program is a shell or which passes `-c`.

That is pinned rather than reviewed because losing it is not a local change. A single
`exec.Command("sh", "-c", …)` makes every identifier check in the package irrelevant at
once, since those values are only safe as argv elements.

### Turning it off

`--allow-any-path` exists for the deployment where the process boundary is already the
security boundary: a container whose whole filesystem is disposable. It is mutually
exclusive with `--workspace` — "confine to these roots, and also to nothing" is not a
statement — and the startup banner reports either state, including a warning line when
confinement is off.



## One instance: stdio

With `--mode` and no `--port`, `breeze-mcp` speaks stdio and the client launches it
as a subprocess. There is no port and no token: the process boundary is the trust
boundary.

```bash
go build -o breeze-mcp ./cmd/breeze-mcp
```

```json
{
  "mcpServers": {
    "breeze": {
      "command": "/path/to/breeze-mcp",
      "args": ["--mode=generator"]
    }
  }
}
```

Nothing about this changed when network mode was added, apart from `--mode` becoming
required.

`--scope` works here too, and means something slightly different: stdio has no token to
restrict, so a scope is not a credential boundary but the operator saying what this
subprocess should offer at all — "launch me a read-only Breeze":

```json
{
  "mcpServers": {
    "breeze-readonly": {
      "command": "/path/to/breeze-mcp",
      "args": ["--mode=generator", "--scope=fleet,runtime"]
    }
  }
}
```

The handshake reports `breezeCapabilities` on stdio exactly as it does over HTTP, which
matters more here: `GET /mcp/features` needs an HTTP surface, and there isn't one.

### Running it by hand

`breeze-mcp --mode generator` in a terminal appears to do nothing. That is correct — it is
waiting for a client on stdin — but zero output is indistinguishable from a hang, so a
terminal gets a short guide on stderr instead:

```
breeze-mcp: MCP server ready on stdin/stdout. This is not an interactive command.

  It is now waiting for JSON-RPC 2.0 messages on stdin and will answer on stdout.
  Nothing else will be printed until a client speaks to it — the silence is the
  server working, not a hang. Ctrl+C to stop.

  mode        generator (full toolchain: generates and changes projects)
  transport   stdio — no port, no token; the process boundary is the trust boundary
  tools       40
  scope       all capabilities (unscoped)
  workspace   /srv/projects
  …
```

**A piped stdin gets none of it.** The guide is written only when stdin is a terminal, so
an editor launching this as a subprocess sees exactly what it saw before: nothing on
stderr, and nothing but JSON-RPC on stdout. That is not a preference — one human-readable
line on stdout is one malformed MCP message to the peer, and the guide would be explaining
a session it had just corrupted.

To check the server works without an editor, pipe one message in:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | breeze-mcp --mode generator
```

## One instance: network

With `--port`, the same tools are served over MCP Streamable HTTP on a control
address:

```bash
breeze-mcp --mode generator --port 2000
# breeze-mcp: generated control token (shown once): 9f3c…
# breeze-mcp: set BREEZE_MCP_TOKEN to keep it across restarts
# breeze-mcp: control endpoint http://127.0.0.1:2000/mcp (MCP Streamable HTTP, bearer token required)
# breeze-mcp: mode generator (full toolchain: generates and changes projects)
# breeze-mcp: token scope all capabilities (unscoped)
# breeze-mcp: capability report at http://127.0.0.1:2000/mcp/features
# breeze-mcp: filesystem tools confined to /srv/projects
```

The same server is reachable from the CLI you already have, with the identical
flags, defaults and startup banner — one implementation, two entrypoints:

```bash
breeze start mcp-server --mode generator --port 2000
```

Four things to know before deploying it:

- **The bind is loopback by default.** `--host 0.0.0.0` widens it, and is the only
  thing that does. What is exposed is a code generator with filesystem access, so
  widening it is a deliberate act.
- **A bearer token is mandatory on every request**, including the handshake.
  Supply it with `--token` or `$BREEZE_MCP_TOKEN` for a reproducible deployment;
  otherwise one is generated and printed once, to stderr.
- **Filesystem tools are confined to a workspace**, which defaults to the working
  directory. `--workspace` names one or more roots; `--allow-any-path` removes the
  confinement and is mutually exclusive with it. See "Where a tool may reach on
  disk" above — this is the flag that decides whether `{"path": "/etc"}` runs
  `go test` in /etc.
- **It is stdio or network, not both.** One process serves one transport. The
  package comment in `cmd/breeze-mcp/main.go` explains why.

```json
{
  "mcpServers": {
    "breeze": {
      "url": "http://127.0.0.1:2000/mcp",
      "headers": { "Authorization": "Bearer 9f3c…" }
    }
  }
}
```

### Getting the token

Started from a terminal, the banner is followed by what to do with the value it just
printed:

```
  Using the token

    The token above is shown once and is not stored. Copy it now, or restart
    with BREEZE_MCP_TOKEN set to a value you control:

      export BREEZE_MCP_TOKEN=<token>          # on Windows: $env:BREEZE_MCP_TOKEN="<token>"

    Check it works — this needs no MCP session and answers what this server can do:

      curl -H "Authorization: Bearer $BREEZE_MCP_TOKEN" http://127.0.0.1:2000/mcp/features
    …
```

Three things worth knowing about that:

- **The token is printed once and never stored.** There is no file to read it back from and
  no flag to reprint it. If it scrolls away, restart with `$BREEZE_MCP_TOKEN` set to a value
  you chose — which is what a reproducible deployment should do anyway.
- **A supplied token is never echoed.** When it came from `--token` or the environment, you
  already have it, and stderr is what a container runtime captures into a log. Only a
  *generated* token appears, exactly once.
- **`/mcp/features` is the cheapest way to verify one.** It needs no MCP handshake and no
  session, so a wrong token is a 401 immediately rather than a confusing failure three
  messages into a session.

Like the stdio guide, this block is terminal-only. A container log gets the banner — which
is fact, and worth keeping — without the instructions, which are not.

## Seeing what it is doing: `--log`

A server without `--log` prints its banner and then nothing. That is the right
default — on stdio, stdout is the protocol stream and stderr belongs to whichever
editor launched the process — but it means a running server answers none of
"did the agent call anything", "is a tool failing", or "is something trying my
control port".

`--log` writes one line to stderr per event:

```bash
breeze-mcp --mode generator --port 2000 --log
# breeze-mcp: session initialized
# breeze-mcp: tool breeze_verify_project ok 4.21s args=path,run_tests
# breeze-mcp: tool breeze_get_logs error 91ms args=service_url,token
# breeze-mcp: tool breeze_new refused (needs generation capability)
# breeze-mcp: tool breeze_generat unknown
# breeze-mcp: refused 401 missing or invalid bearer token from 10.1.2.3:53551
```

Six kinds of line, and each answers something the others do not:

| Line | Answers |
|---|---|
| `tool NAME ok/error DURATION` | which tools ran, how long they took, which failed |
| `tool NAME PANICKED` | a failure in this server rather than in the project |
| `tool NAME unknown` | the agent is calling a name that does not exist — often a wrong `--mode` |
| `tool NAME refused (needs X capability)` | the tool exists and this token's `--scope` excludes it |
| `refused STATUS REASON from ADDR` | a request rejected before dispatch |
| `session initialized` | a client completed a handshake |

**The last one is the security line.** A wrong token never reaches a tool, so
nothing in the dispatch path can see it — without this, a run of guesses against
the control port leaves no trace whatsoever. The address is included because a
refusal nobody can attribute is a refusal nobody can act on.

**`unknown` and `refused` are separate on purpose.** A client cannot tell them
apart, deliberately: a refusal that enumerated what a caller may not have would
be an invitation. In a log the opposite is wanted — "the agent says the tool is
missing" has two very different fixes, and this is what distinguishes *wrong mode*
from *wrong token*.

### Argument names, never values

`args=` lists the argument *keys* a call supplied. It never lists a value, at any
verbosity, and there is no flag that changes that.

The reason is concrete: several tools take a credential as an ordinary argument —
`token` and `password` on the fleet and live tools, `service_token` on
provisioning, `token` on `breeze_simulate_request`. stderr is the stream a
container runtime captures and a supervisor ships elsewhere, so a log that
formatted values would copy those secrets into a file that outlives the process.

This is enforced structurally rather than by a redaction list. The event type in
`internal/mcp/observe.go` has no field that could hold a value: every field is a
constant chosen in that package, a number, a tool name, or a list of argument
names. A redacting formatter would need a list of sensitive key names, and that
list is wrong the moment somebody adds a field — with the failure being a secret
in a log, discovered by whoever reads the log. `TestAToolCallLogsNoArgumentValue`
asserts the property over a real dispatch, and
`TestLogNeverContainsAnArgumentValue` asserts it at the formatter.

Two related omissions, for the same reason:

- **A rejected `Origin` is not logged**, though it *is* echoed to the caller — an
  operator needs to see which string to allowlist. It stays out of the log because
  it is caller-supplied, and logging it would let whoever is being refused choose
  what gets written into a file somebody else reads.
- **A panic value is not logged.** It goes to the caller who asked for the tool.
  It is composed at the panic site and can contain anything that code had in hand —
  a path, a captured output, an argument. The line names the tool, which is what a
  log is for.

## In-process: the app serves its own control plane

A generated application can serve an MCP control endpoint from its own process,
beside its own traffic — no separate `breeze-mcp`:

```go
app := breeze.New(router, pool)

server, token, err := mcp.StartInProcess(app, mcp.InProcessConfig{
    Mode:  mcp.ModeAppRuntime,               // required; see "Two kinds of server"
    Port:  2000,                             // control address
    Token: os.Getenv("BREEZE_MCP_TOKEN"),
})
if err != nil {
    log.Fatal(err)                           // a port conflict is fatal at startup
}
log.Printf("mcp: %s (token %s)", server.URL(), token)
go server.Serve()

app.Run(3000, true)                          // app address
```

`Mode` has no default here either. `ModeAppRuntime` is what a deployed application
wants, and it is the value that makes the mutating tools structurally absent rather
than merely filtered out. `ModeGenerator` is for a development container that really
does own its own source tree.

Same security posture as the standalone binary, with no exceptions: a bearer
token on every request including the handshake, `Origin` validated, loopback
unless `Host` says otherwise.

### It serves a subset, on purpose

16 of the 40 tools. The excluded 24 are the ones that generate, plan, verify or
provision, and they are excluded for two independent reasons:

- **Concurrency.** They `chdir` and replace `os.Stdout` under a process-wide
  lock. Inside a live application that changes the working directory of the
  server currently serving requests, so every relative path the app resolves
  during the call — a static root, a template directory, a log file — resolves
  somewhere else.
- **No source tree.** A deployed binary was built from a module cache, not a
  clone. Its own source is not on disk, so those tools would have nothing to
  operate on.

`mcp.Tools()` and `mcp.ExcludedTools()` report both lists at runtime.
`InProcessConfig.AllowWorkspaceTools` restores the excluded set — for a
development container where the app runs from its own clone and serves no real
traffic. A deployed app should not set it.

### Scoping an embedded token

`InProcessConfig.Scope` narrows what the embedded endpoint's token reaches, the same
way `--scope` does for the standalone binary:

```go
scope, err := mcp.NewScope(mcp.CapFleet)     // traces and topology, nothing else
if err != nil {
    log.Fatal(err)
}

server, token, err := mcp.StartInProcess(app, mcp.InProcessConfig{
    Mode:  mcp.ModeAppRuntime,
    Port:  2000,
    Token: os.Getenv("BREEZE_MCP_TOKEN"),
    Scope: scope,
})
```

Worth doing even in `ModeAppRuntime`, where nothing that writes is registered at all.
Mode is a property of the deployment; scope is a property of the credential. An
app-runtime embed with an unscoped token still hands one caller live logs, traces,
simulated requests and the OpenAPI document alike, and a CI job that only reads traces
has no use for the rest.

The zero value is unscoped, so an existing embed is unaffected. `mcp.Capabilities()`
lists the categories, and `GET /mcp/features` works on an embedded endpoint too — which
is often exactly where an operator wants it, debugging a deployed application.

### The endpoint reports on itself

`StartInProcess` registers a `diag` probe under `mcp`, so
`breeze_diagnose_service` includes the endpoint answering the call:

```json
{
  "subsystem": "mcp",
  "status": "ok",
  "summary": "MCP endpoint on 127.0.0.1:2000 in app-runtime mode, 4 of 9 tool(s) reachable",
  "detail": {
    "address": "127.0.0.1:2000",
    "endpoint": "/mcp",
    "mode": "app-runtime",
    "tools": 9,
    "reachable_tools": 4,
    "scoped": true,
    "granted_capabilities": ["fleet"],
    "withheld_by_scope": ["breeze_diagnose_service", "breeze_get_logs", "…"],
    "workspace_tools": false,
    "origin_check_disabled": false
  },
  "notes": ["5 tool(s) are registered but withheld by this token's scope, …"]
}
```

`reachable_tools` against `tools` is the field worth knowing about. A client calling a
scoped-out tool gets the same `no such tool` as for one that does not exist — mode
decided what is registered, scope decided what the credential reaches, and this is the
only place both are visible at once. `withheld_by_scope` names them.

The probe also raises a note for the three states that produce no error anywhere else:
`AllowWorkspaceTools` left on (this process will `chdir` into and rewrite its own tree
while serving requests), `ModeGenerator` in a container with no source tree (the full
toolchain registers and every mutating tool fails at its first file operation), and an
unscoped token. Plus `AllowedOrigins: ["*"]` and a non-loopback bind.

An endpoint whose every tool is withheld reports `degraded` rather than `ok`: the
server is running and answering, and a caller who can call nothing has a working
endpoint it cannot use, which is a configuration fault rather than an absent feature.

See [`diag.md`](./diag.md) for the registry, and note that `mcp` and `auto-mcp` are two
different subsystems — the section below is the distinction.

### In-process versus Auto-MCP

Breeze has two MCP endpoints in a running application, and they answer different
questions. A project may want both; they need different ports, and
`StartInProcess` refuses the port `EnableMCP` took.

| | `app.EnableMCP(addr)` — Auto-MCP | `mcp.StartInProcess(app, cfg)` |
|---|---|---|
| Exposes | this app's `MCPTool`-tagged **business routes** | framework **introspection** of this instance |
| Tool list | whatever the author tagged | fixed, 15 read-only tools |
| An agent uses it to | *call* the service — place an order, look up a customer | *understand* the service — live routes, errors, logs, traces |
| Writes | yes, whatever the tagged routes do | nothing |

Choose Auto-MCP to make an application agent-callable. Choose in-process to make
a running instance agent-inspectable. Neither substitutes for the other.

## Many instances: one client, N control planes

This is the "an agent centrally controls N containers" arrangement, and it needs
no new server-side machinery: MCP clients already support several named servers,
and each named server is one `breeze-mcp` control address.

```json
{
  "mcpServers": {
    "users-service": {
      "url": "http://users-host:2000/mcp",
      "headers": { "Authorization": "Bearer <users control_token>" }
    },
    "orders-service": {
      "url": "http://orders-host:2001/mcp",
      "headers": { "Authorization": "Bearer <orders control_token>" }
    }
  }
}
```

Every URL above is a **control address**. Each has its own token: a token
authorises one instance, and one instance manages one machine's workspace.

An agent then works per-service by naming the server, and reaches *running*
services by argument:

- "Add a route to the users service" → a `breeze_add` call through
  `users-service`. No address in the arguments.
- "What is the orders service actually serving?" →
  `breeze_get_routes(service_url: "http://orders-host:8080")`, sent through
  whichever control plane is convenient. The `8080` here is the orders
  application's **app port** and has no relationship to the `2001` above.

### Ports to expose per container

*(Deferred: `cmd/fleet-example/docker-compose.yml` is not extended for this yet.)*
When it is, each service will need **at least two distinct published ports** rather
than one:

- its `breeze-mcp` control port, for the agent's control plane;
- its own app port, which is what it publishes today.

The service hosting the Fleet Aggregator needs a third, for the aggregator
endpoint. A service that also embeds its own `app-runtime` endpoint needs a fourth —
that is what `enable_app_mcp` allocates, and `provision_service` returns it as
`app_mcp_port` / `app_mcp_url`. Reusing one port for two of these is not a
simplification: for the control and app-MCP ports in particular it would silently
merge a server that can rewrite the project with one that is supposed to be
read-only.

## Many instances: provisioned rather than configured

*(Deferred: the end-to-end walkthrough is not built yet.)*

The configuration above assumes the containers already exist and someone wrote
their addresses down. The Category H tools invert that: a single **orchestrator**
`breeze-mcp` instance — one explicitly given Docker access — provisions services
and *reports* their addresses.

`provision_service` returns `control_port`, `control_token` and `app_port`
explicitly, never a bare `port`. The `control_token` is returned exactly once, at
provision time: `list_provisioned_services` never includes it, and no other tool
can retrieve it. Lose it and the only recovery is `deprovision_service` followed
by re-provisioning.

`provision_fleet` additionally reports the Fleet Aggregator's own address for the
service hosting it, labelled separately from that service's `control_port` and
`app_port`.

See `internal/mcp/tools_provision.go` for the tool reference; this section gains a
worked walkthrough alongside `cmd/mcp-example`.

