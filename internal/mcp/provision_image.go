package mcp

// provision_image.go — the container image a provisioned service runs.
//
// # What is in the image, and why two processes
//
// A provisioned service is two things at once: the Breeze application, and its own
// breeze-mcp control instance. They are one container rather than two because they
// share one workspace — the control instance's whole purpose is to generate, modify
// and verify the code the application is running, and a control plane looking at a
// different copy of the project than the one being served would be worse than
// useless.
//
// So the image has one entrypoint that starts both: breeze-mcp in the background on
// the container's control port, the application in the foreground on its app port.
// The application is the foreground process on purpose — when it exits the container
// exits, and `docker ps` then reports what an operator means by "is the service
// running". A container that stayed up because its control plane was alive while the
// application had crashed would report healthy and serve nothing.
//
// # Where breeze itself comes from
//
// A provisioned container needs the Breeze module twice: the generated application
// imports it, and the control instance *is* it. Both used to be resolved from the
// module proxy — `go install .../cmd/breeze-mcp@<pin>` for the binary, and the
// generated go.mod's own require for the library.
//
// That is correct for a released orchestrator and broken for a development one. The
// working tree is routinely ahead of the newest tag: a package added since the last
// release — `fleet/`, or `cmd/breeze-mcp` itself — is not in what the proxy serves, so
// the build fails with "module found, but does not contain package". An orchestrator
// cannot provision a container running the code it was built from.
//
// So when the module's source is on disk, it is copied into the build context and the
// generated go.mod is pointed at it with a `replace`. Both the application and the
// control binary are then compiled from exactly the tree that provisioned them, and
// neither needs the proxy at all.
//
// Compiling inside the image rather than copying a host-built binary in is what keeps
// this portable: the orchestrator may well run on Windows or arm64 while the image is
// linux/amd64, and a copied executable would produce a container whose control plane
// cannot exec. The build stage settles the target platform once, for both binaries.
//
// Without local source — a released binary installed with `go install` — it falls back
// to the proxy and the version pin, which is the right behaviour there: a tagged
// release does contain its own packages.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// buildImageTag is the golang image a provisioned build compiles in.
//
// # Why this is derived rather than pinned
//
// It used to be a literal `golang:1.25-alpine`, and that is a latent break. Two go.mod
// files inside the context both carry a `go` directive: the generated project's, which
// the generator sets from its own runtime version, and Breeze's own. The official golang
// images set GOTOOLCHAIN=local, so a builder older than either directive fails with
// "go.mod requires go >= 1.26.4 (running go 1.25.14)" — a failure that appears the day
// the orchestrator's toolchain is upgraded and nothing else changes.
//
// Deriving the tag from runtime.Version() keeps the builder in step with whatever
// produced the go directive, because they are then the same number by construction.
//
// GOTOOLCHAIN is deliberately left alone. Setting it to `auto` would paper over a
// mismatch by downloading a toolchain mid-build, reintroducing the network dependency
// the vendored path exists to remove — and doing it silently.
func buildImageTag() string {
	if m := goVersionRe.FindStringSubmatch(runtime.Version()); m != nil {
		// The exact patch tag. It exists on Docker Hub for every released version, and
		// being exact means the builder is never behind a directive derived from this
		// same value.
		return "golang:" + m[1] + "-alpine"
	}
	// A release candidate or devel build names no published image tag, so this falls
	// back to the minor line of Breeze's own go directive — the oldest toolchain this
	// module claims to build under.
	return fallbackBuildImage
}

// goVersionRe pulls "1.26.4" out of "go1.26.4". A release candidate ("go1.27rc1") or a
// devel build deliberately fails to match, since neither names a published image tag.
var goVersionRe = regexp.MustCompile(`^go(\d+\.\d+(?:\.\d+)?)$`)

// fallbackBuildImage is used when runtime.Version() is not a released version.
//
// A bare minor line rather than a patch, so it tracks the newest patch of that line —
// which is what a fallback wants.
const fallbackBuildImage = "golang:1.25-alpine"

// The ports *inside* a provisioned container. They are fixed, and the host-side
// ports the allocator hands out are mapped onto them.
//
// Fixed inside and dynamic outside is what keeps the image identical for every
// service: an image parameterised by port would have to be rebuilt to move a
// service, and the generated main.go hardcodes 3000 anyway. It also means these
// three numbers are the only place the container's internal layout is written down.
const (
	containerControlPort    = 2000
	containerAppPort        = 3000
	containerAggregatorPort = 9000

	// containerAppMCPPort is the app-runtime MCP endpoint the application embeds,
	// when it embeds one. Distinct from containerControlPort because they are
	// different servers with different capabilities: the control port serves the
	// generator-level toolchain over this container's source tree, this one serves
	// read-only introspection of the running process.
	containerAppMCPPort = 2100
)

// containerWorkspaceDir is where a provisioned container carries its project.
//
// One spelling, used by both Dockerfile variants as their WORKDIR and by the
// entrypoint as the control instance's --workspace. That coupling is the point: the
// control plane inside a provisioned container is a full generator-mode server, and
// what confines it to the project tree is this path. Were the two to drift — a WORKDIR
// moved without the flag following — confinement would still be in force but pointed
// somewhere else, and every tool call would be refused for a reason no message
// explains.
const containerWorkspaceDir = "/workspace"

// breezeModulePath, declared in idiomcheck.go, is also the module a provisioned
// container installs breeze-mcp from. It is reused rather than re-declared so that a
// module rename cannot leave the two disagreeing — the copy would then generate a
// Dockerfile installing from a path that no longer exists.

// provisionDockerfile renders the Dockerfile for a provisioned service.
//
// mcpVersion is the orchestrator's own version, used only in the no-local-source case.
// A development build reports "(devel)", which is not a resolvable module version, so
// the pin falls back to @latest — and says so in a comment in the generated file,
// because an operator reading it later needs to know why their two containers are not
// necessarily running the same tool set.
//
// vendored says whether the build context carries Breeze's own source under
// breeze-src/. When it does, both binaries are compiled from it and the proxy is not
// consulted; see the file comment for why that is the default whenever source exists.
func provisionDockerfile(mcpVersion string, vendored bool) string {
	if vendored {
		return provisionDockerfileFromSource()
	}
	return provisionDockerfileFromProxy(mcpVersion)
}

// provisionDockerfileFromSource builds both binaries from the copied-in module.
//
// The `replace` is written by rewriteGoModForVendoredBreeze before the build, so the
// application's own import of Breeze resolves to breeze-src/ too — one source of truth
// for the library and the control binary alike.
//
// `go mod tidy` runs inside the build rather than being skipped, and the comment in the
// generated Dockerfile says why: the replace covers Breeze, not Breeze's own published
// dependencies, and the generated go.sum has no entries for those.
func provisionDockerfileFromSource() string {
	return fmt.Sprintf(`# syntax=docker/dockerfile:1
# Generated by breeze-mcp provision_service. Two processes, one workspace: the
# Breeze application in the foreground, its breeze-mcp control instance behind it.
#
# Breeze is compiled from source copied into this context under breeze-src/, not
# fetched from the module proxy. The orchestrator that produced this file was running
# from a source tree, and the proxy's newest release does not necessarily contain the
# packages that tree has — so building from it is the only way this container runs the
# same code that provisioned it.

FROM %s AS build

WORKDIR /src
COPY . .

# The replace resolves Breeze itself from disk, but the generated go.sum has no entries
# for what Breeze *imports* — gnet, go-json, brotli and the rest. Those are published
# dependencies with nothing local to substitute, so this resolves them from the proxy.
# Without it the build fails with "missing go.sum entry", naming a third-party package
# and giving no hint that the local replace is what changed the requirement set.
RUN go mod tidy

# The application. Breeze comes from breeze-src/ via the replace above.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app .

# The control instance, from the same tree. Built here rather than copied from the host
# because the host may be a different OS or architecture than this image. Breeze's own
# go.sum came across with the source, so this needs no tidy of its own.
WORKDIR /src/breeze-src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/breeze-mcp ./cmd/breeze-mcp

# The Fleet Aggregator, from the same tree again. Built unconditionally and started only
# when the entrypoint is given a port: one image serves every service in a fleet, and
# which of them hosts the aggregator is a runtime decision the orchestrator makes.
#
# It has to be built rather than fetched for the same reason as breeze-mcp, and the
# reason it is built *here* at all: the generator emits a tracer, never an aggregator, so
# without this the published aggregator port maps to a container port nothing listens on
# — a fleet whose services all export spans into a closed connection.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fleet-aggregator ./cmd/fleet-aggregator

FROM alpine:3.22

RUN apk add --no-cache ca-certificates wget

WORKDIR /workspace

# The project source, not only the binary: the control instance's tools read and
# rewrite this tree, so the container serving the app must also contain it.
COPY --from=build /src /workspace
COPY --from=build /out/app /usr/local/bin/app
COPY --from=build /out/breeze-mcp /usr/local/bin/breeze-mcp
COPY --from=build /out/fleet-aggregator /usr/local/bin/fleet-aggregator
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The control port and the app port. Two numbers, never interchangeable: the first
# answers "can I generate here", the second "what is this service serving".
EXPOSE %d
EXPOSE %d

# The probe targets the app port, because the application is what this container
# exists to run. A probe against the control port would report a healthy container
# whose application had failed to start.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 \
    CMD wget -qO- http://127.0.0.1:%d/ >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
`, buildImageTag(), containerControlPort, containerAppPort, containerAppPort)
}

// provisionDockerfileFromProxy is the released-orchestrator path: no local source, so
// Breeze comes from the module proxy at a pinned version.
func provisionDockerfileFromProxy(mcpVersion string) string {
	pin := strings.TrimSpace(mcpVersion)
	note := "# Pinned to the orchestrator's own version, so this instance serves the same tools."
	if !strings.HasPrefix(pin, "v") {
		pin = "latest"
		note = "# The orchestrator is a development build, whose version is not a resolvable module\n" +
			"# version, so this pulls the latest release instead. A released orchestrator pins its\n" +
			"# own version here and the two then serve an identical tool set."
	}

	return fmt.Sprintf(`# syntax=docker/dockerfile:1
# Generated by breeze-mcp provision_service. Two processes, one workspace: the
# Breeze application in the foreground, its breeze-mcp control instance behind it.

FROM %s AS build

WORKDIR /src
COPY . .

# The application. Its go.mod already requires Breeze; nothing is added here.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app .

%s
RUN CGO_ENABLED=0 GOBIN=/out go install %s/cmd/breeze-mcp@%s

FROM alpine:3.22

RUN apk add --no-cache ca-certificates wget

WORKDIR /workspace

# The project source, not only the binary: the control instance's tools read and
# rewrite this tree, so the container serving the app must also contain it.
COPY --from=build /src /workspace
COPY --from=build /out/app /usr/local/bin/app
COPY --from=build /out/breeze-mcp /usr/local/bin/breeze-mcp
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The control port and the app port. Two numbers, never interchangeable: the first
# answers "can I generate here", the second "what is this service serving".
EXPOSE %d
EXPOSE %d

# The probe targets the app port, because the application is what this container
# exists to run. A probe against the control port would report a healthy container
# whose application had failed to start.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 \
    CMD wget -qO- http://127.0.0.1:%d/ >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
`, buildImageTag(), note, breezeModulePath, pin, containerControlPort, containerAppPort, containerAppPort)
}

// provisionEntrypoint renders the entrypoint script.
//
// BREEZE_MCP_TOKEN is read from the environment and never echoed. --host 0.0.0.0 is
// correct *inside* a container and is not the loopback default being abandoned: a
// container's own loopback is not reachable from the host at all, so a control
// instance bound to it would be unreachable through its published port. The host side
// of that publish is bound to 127.0.0.1 by runContainer, and the token is required on
// every request either way.
//
// --mode is passed explicitly, and generator is the right value: this control plane's
// whole purpose is to generate, modify and verify the source tree the container carries
// beside the application. Passing it is not optional — mode has no default, so the
// script would otherwise start a process that exits immediately, leaving a container
// that looks healthy (the probe targets the application) with a control port nothing
// answers on.
//
// --scope is honoured when BREEZE_MCP_SCOPE is set, so a provisioned control plane can
// be given a token narrower than its mode. Unset means unscoped, matching every other
// entrypoint into this server.
//
// FLEET_AGGREGATOR_PORT starts the aggregator in this container, and is set only for the
// one service in a fleet that hosts it. The generator emits a tracer and never an
// aggregator, so without this the port provision_fleet publishes and returns as
// aggregator_url maps to a container port nothing listens on — every service exports
// spans into a closed connection, and the fleet reports an empty topology with no error
// anywhere to explain it.
// --mode is required and has no default. This instance serves the project tree beside
// it, so it is a generator; an app-runtime server would have no generating tools at all.
//
// --workspace is what keeps that generator inside the project. The default is the
// working directory, which is /workspace here anyway, but it is passed explicitly for
// two reasons: it does not follow a later change to WORKDIR or to the entrypoint's cwd,
// and it makes a provisioned container's confinement visible in the file an operator
// reads when asking what this container can reach. The container's filesystem is not
// itself a boundary — /etc, /usr and a mounted Docker socket are all inside it.
//
// # The version floor this raises
//
// The flag is newer than --mode, so it moves the oldest breeze-mcp this entrypoint can
// start. That matters in exactly one configuration: a development orchestrator with no
// local Breeze source, where provisionDockerfileFromProxy installs @latest and the
// newest release may predate the flag. There the control plane would exit on an unknown
// flag while the application ran on — the same shape as the --mode regression.
//
// It is accepted rather than worked around, because the two configurations that are not
// exposed are the two that are used: a vendored build compiles breeze-mcp from the tree
// that generated this file, and a released orchestrator pins its own version. Probing
// `breeze-mcp --help` first would trade a loud startup failure in a rare development
// case for a container that silently runs unconfined, which is the worse of the two.
func provisionEntrypoint() string {
	return fmt.Sprintf(`#!/bin/sh
set -e

if [ -z "$BREEZE_MCP_TOKEN" ]; then
  # Refusing is the point. A control plane that generated its own token here would
  # have one nobody knows, and would look like a working instance that no client can
  # ever authenticate against.
  echo "entrypoint: BREEZE_MCP_TOKEN is not set; refusing to start an unreachable control plane" >&2
  exit 1
fi

# --mode is required and has no default. This instance serves the project tree beside
# it, so it is a generator; an app-runtime server would have no generating tools at all.
#
# --workspace confines every filesystem tool to the project. Without it the default is
# the working directory, which is the same directory — but stated here so that a
# container's reach is written in the file describing the container, and so a future
# change of WORKDIR cannot silently widen it.
#
# Built with "set --" rather than a string. A string would be word-split on use, and
# BREEZE_MCP_SCOPE is read from the environment: a value of "runtime --allow-any-path"
# would then become two arguments and start this control plane unconfined. "$@" keeps
# each element one argument, whatever it contains.
set -- --mode %s --port %d --host 0.0.0.0 --workspace %s

# Optional per-token scoping. Unset means every capability, which is what an
# unscoped token has always meant.
if [ -n "$BREEZE_MCP_SCOPE" ]; then
  set -- "$@" --scope "$BREEZE_MCP_SCOPE"
fi

breeze-mcp "$@" &

# The Fleet Aggregator, for the one service in a fleet that hosts it. FLEET_PORT is what
# the binary reads; the variable is named for what it is so it cannot be confused with
# this service's own app port.
if [ -n "$FLEET_AGGREGATOR_PORT" ]; then
  FLEET_PORT="$FLEET_AGGREGATOR_PORT" fleet-aggregator &
fi

# The application is exec'd, so it becomes PID 1: it receives the signals docker stop
# sends, which is what lets a Fleet tracer flush its buffered spans instead of being
# killed with them still in memory.
exec app
`, ModeGenerator, containerControlPort, containerWorkspaceDir)
}

// vendoredBreezeDir is where the module's source lands inside a build context.
//
// Inside the project directory, because the whole project directory is the context and
// Docker cannot COPY from outside it. The name is prefixed so it cannot collide with a
// directory the generator writes (handlers/, models/, views/ …).
const vendoredBreezeDir = "breeze-src"

// writeProvisionBuildFiles adds the Dockerfile and entrypoint to a generated project
// directory, which then becomes the build context.
//
// They are written into the project rather than into a separate context directory so
// that the tree the control instance serves is exactly the tree that was built —
// including the files describing how it was built.
//
// When Breeze's own source is on disk it is copied in as well, and go.mod is given a
// replace pointing at the copy. That makes the container build the framework from the
// tree that provisioned it rather than from the proxy's newest tag — see the file
// comment. A failure to copy is not fatal: it falls back to the proxy path and returns
// a note, because a provisioned service built from a release is still a working service,
// whereas refusing outright would turn a degraded outcome into no outcome.
func writeProvisionBuildFiles(projectDir, mcpVersion string) ([]string, error) {
	var notes []string

	vendored := false
	if source := breezeSourceRoot(); source != "" {
		if err := vendorBreezeSource(source, projectDir); err != nil {
			notes = append(notes, "the local Breeze source could not be copied into the build "+
				"context ("+err.Error()+"), so the image builds against the published module instead")
		} else if err := rewriteGoModForVendoredBreeze(projectDir); err != nil {
			notes = append(notes, "go.mod could not be pointed at the copied Breeze source ("+
				err.Error()+"), so the image builds against the published module instead")
			_ = os.RemoveAll(filepath.Join(projectDir, vendoredBreezeDir))
		} else {
			vendored = true
		}
	} else {
		// Worth saying: it explains a build that then needs the network, and why a
		// package added since the last release might be missing from the image.
		notes = append(notes, "no local Breeze source tree was found, so the image builds "+
			"against the published module; a package added since the last release will not be in it")
	}

	files := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"Dockerfile", provisionDockerfile(mcpVersion, vendored), 0o644},
		{"entrypoint.sh", provisionEntrypoint(), 0o755},
	}

	for _, file := range files {
		path := filepath.Join(projectDir, file.name)
		if err := os.WriteFile(path, []byte(file.content), file.mode); err != nil {
			return notes, fmt.Errorf("writing %s: %w", file.name, err)
		}
	}
	return notes, nil
}
