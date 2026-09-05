package mcp

// docker_names.go — validating the identifiers a provisioning request hands to Docker.
//
// # What is actually at risk, and what is not
//
// Nothing here is shell injection. runDockerCommand builds an argument array and
// calls exec.CommandContext, so no shell parses any of these values and a name of
// `; rm -rf /` reaches Docker as one literal argument that Docker rejects.
//
// The risks that remain are real but different:
//
//   - **Argument smuggling.** A value starting with "-" is read by Docker as a flag,
//     not as the operand it was passed as. `--privileged` as a container name lands
//     in `docker run -d --name --privileged <image>`, and Docker's parser takes
//     --privileged as the flag it looks like. That is a container with host-level
//     capabilities, obtained without this package ever offering a privileged option.
//   - **Registry redirection.** An image tag is a reference, and a reference with a
//     host component points at a registry. `evil.example/x:latest` with skip_build
//     makes `docker run` pull and execute an attacker's image on this host.
//   - **Colliding with something already running.** A container name that resolves
//     to an existing container is a failed provision at best; an image tag that
//     overwrites an unrelated local image is worse, because the overwrite is silent
//     and the next `docker run` of that tag gets different code.
//
// So the values are validated against Docker's own grammar and then narrowed
// further: no leading dash anywhere, and an image reference may not name a registry
// host. Neither restriction costs a legitimate caller anything — a service is named
// like a service, and a locally built tag has no host component by definition.

import (
	"fmt"
	"regexp"
	"strings"
)

// dockerNamePattern is Docker's own container-name grammar.
//
// Copied from the daemon's validation rather than approximated: the first character
// is alphanumeric, and the rest may add underscore, period and hyphen. A name Docker
// would reject is better refused here, where the message can name the argument.
var dockerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// dockerImagePattern is the repository:tag form this orchestrator builds.
//
// Deliberately narrower than the full OCI reference grammar. It permits lower-case
// path segments separated by "/" plus an optional ":tag", and it does not permit a
// digest, a port, or a registry host — see imageReferenceLocal.
var dockerImagePattern = regexp.MustCompile(
	`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[a-zA-Z0-9_][a-zA-Z0-9_.-]*)?$`,
)

// maxDockerNameLength bounds a name.
//
// 128 is Docker's own limit for a container name. An image reference is allowed more
// because a repository path legitimately has several segments, but not unboundedly:
// a megabyte-long tag is not a naming convention, it is an attempt to find a buffer.
const (
	maxDockerNameLength  = 128
	maxDockerImageLength = 255
)

// validateContainerName checks a container name.
func validateContainerName(name string) error {
	return validateDockerIdentifier("container_name", name, maxDockerNameLength, dockerNamePattern,
		"letters, digits, then any of _ . -")
}

// validateServiceName checks a provisioning service name.
//
// The same grammar as a container name, because that is what it becomes: the default
// container is "breeze-"+name and the default image is "breeze-provisioned/"+name.
// A service name that is not a legal container name would produce a default that
// Docker refuses, and the error would name a value the caller never typed.
//
// It is also a directory name — generateProject creates one — so the traversal
// characters have to go too. The pattern excludes "/" and "\" already, and a leading
// "." is not permitted at all.
func validateServiceName(name string) error {
	if err := validateDockerIdentifier("name", name, maxDockerNameLength, dockerNamePattern,
		"letters, digits, then any of _ . -"); err != nil {
		return err
	}
	// A second reader of the same rule rather than a new one — and the one that
	// will still be correct if the pattern is ever loosened for Docker's sake
	// without anyone thinking about the filesystem half.
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("name %q cannot contain a path separator or \"..\": it is also the "+
			"directory the project is generated into", name)
	}
	return nil
}

// validateImageTag checks an image reference.
func validateImageTag(tag string) error {
	if err := validateDockerIdentifier("image_tag", tag, maxDockerImageLength, dockerImagePattern,
		"lower-case name segments separated by /, with an optional :tag"); err != nil {
		return err
	}
	return imageReferenceLocal(tag)
}

// imageReferenceLocal refuses a reference that names a registry.
//
// # Why this is refused rather than allowed
//
// Docker decides that the first path segment is a registry host when it contains a
// "." or a ":", or when it is literally "localhost". So `evil.example/x` is a pull
// from evil.example, and with skip_build set that image is then *run on this host* —
// the orchestrator would have executed an attacker-supplied container because a
// string in a JSON field looked like a tag.
//
// The legitimate use of image_tag is an operator's local naming convention, which has
// no host component. Someone who genuinely wants a remote image can pull it
// themselves; that is a deliberate act by a person with Docker access, which is
// exactly the distinction being drawn.
func imageReferenceLocal(tag string) error {
	slash := strings.Index(tag, "/")
	if slash < 0 {
		// No slash means no host component is possible — the whole value is a
		// repository name.
		return nil
	}

	if first := tag[:slash]; strings.ContainsAny(first, ".:") || first == "localhost" {
		return fmt.Errorf("image_tag %q names the registry %q. This orchestrator only builds and "+
			"runs local images, and pulling from a registry named in a tool argument would run "+
			"code this host did not build. Use a tag with no registry host",
			tag, first)
	}
	return nil
}

// validateDockerIdentifier is the shared shape of the three checks above.
//
// The leading-dash test is separate from the pattern and comes first, because it is
// the one whose consequence is not "Docker refuses this". A value beginning with "-"
// is read by Docker as a flag rather than as an operand, so `--privileged` passed as
// a container name becomes the flag it resembles. The patterns already exclude a
// leading dash; this exists to make the *reason* checkable and to produce a message
// about argument smuggling rather than about character classes.
func validateDockerIdentifier(
	field, value string,
	max int,
	pattern *regexp.Regexp,
	allowed string,
) error {
	if strings.TrimSpace(value) == "" {
		// Empty means "use the default", which every caller of these is entitled to.
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s %q has leading or trailing whitespace", field, value)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q begins with a dash, so Docker would read it as a flag rather "+
			"than as a name — `--privileged` passed here would become the flag it looks like",
			field, value)
	}
	if len(value) > max {
		return fmt.Errorf("%s is %d characters; the limit is %d", field, len(value), max)
	}
	if !pattern.MatchString(value) {
		return fmt.Errorf("%s %q is not a valid Docker name: %s", field, value, allowed)
	}
	return nil
}

// dockerEnvNamePattern is the shape of an environment-variable name.
//
// POSIX plus the leading-digit restriction, which is what every shell and every
// runtime actually enforces.
var dockerEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxDockerEnvValueLength bounds one environment value.
//
// Linux allows about 128 KiB for the whole environment, so a value approaching that
// makes the container fail to exec with a message about argument lists rather than
// about the value that caused it. 32 KiB is far above any legitimate configuration
// string and far below the point where the failure becomes unintelligible.
const maxDockerEnvValueLength = 32 << 10

// scopeEnvVar is the variable the provisioned entrypoint reads a token scope from.
//
// Declared here rather than beside tokenEnvVar because this file is what refuses it,
// and a constant whose only use is a refusal belongs with the refusal.
const scopeEnvVar = "BREEZE_MCP_SCOPE"

// validateDockerEnv checks the environment map a provisioning request supplies.
//
// # These are not shell-injectable, and that is not the whole story
//
// The values are passed as `-e KEY=VALUE` arguments to exec.CommandContext, so no
// shell splits them and a value containing `$(...)`, backticks or a newline arrives
// at the container verbatim. The container's entrypoint is `#!/bin/sh` and does
// expand variables — but only the specific ones it names (BREEZE_MCP_TOKEN,
// BREEZE_MCP_SCOPE, FLEET_AGGREGATOR_PORT), and an arbitrary caller-supplied
// variable is never referenced there.
//
// What is checked instead:
//
//   - **A name containing "="** would silently redefine a different variable, because
//     Docker splits the pair on the first "=". `FOO=BAR` as a key with value `BAZ`
//     becomes FOO=BAR=BAZ.
//   - **A leading dash on a name** is the argument-smuggling case again.
//   - **BREEZE_MCP_SCOPE** is honoured by the entrypoint and decides what the
//     container's control plane will serve. A caller who sets it is choosing that
//     container's capability set, which is the orchestrator's decision to make rather
//     than the request's. BREEZE_MCP_TOKEN is already dropped in prepare for the
//     adjacent reason; this closes the other half.
func validateDockerEnv(env map[string]string) error {
	for key, value := range env {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("docker.env has an empty variable name")
		}
		if strings.HasPrefix(key, "-") {
			return fmt.Errorf("docker.env name %q begins with a dash, which Docker would read "+
				"as a flag rather than as a variable name", key)
		}
		if !dockerEnvNamePattern.MatchString(key) {
			return fmt.Errorf("docker.env name %q is not a legal environment variable name: "+
				"letters, digits and underscore, not starting with a digit", key)
		}
		if strings.EqualFold(key, scopeEnvVar) {
			return fmt.Errorf("docker.env cannot set %s: it decides what the provisioned "+
				"container's control plane will serve, which is the orchestrator's decision "+
				"rather than the request's", scopeEnvVar)
		}
		if len(value) > maxDockerEnvValueLength {
			return fmt.Errorf("docker.env %s is %d bytes; the limit is %d, above which the "+
				"container fails to start with an error about argument lists rather than about "+
				"this value", key, len(value), maxDockerEnvValueLength)
		}
	}
	return nil
}

// validateDockerHost checks the host a published port is reported on.
//
// This value is not passed to Docker — runContainer always publishes on 127.0.0.1 —
// it is recorded in the registry and returned in URLs. So the risk is not an escape
// but a lie: a returned app_url pointing somewhere the service is not, which sends
// every later tool call to a host the caller did not choose.
func validateDockerHost(host string) error {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("docker.host %q begins with a dash", host)
	}
	if strings.ContainsAny(trimmed, "/\\ \t\r\n") {
		return fmt.Errorf("docker.host %q is not a hostname: it is used to build the URLs this "+
			"tool returns, so it must be a bare host, not a path or a URL", host)
	}
	return nil
}

// validateDockerOptions is the whole of a provisioning request's Docker half.
//
// One function so a new provisioning entry point cannot validate three of the four
// fields. provision_service and provision_fleet both call it; a third caller that
// forgot would be checking nothing, which is why there is nothing left to forget.
func validateDockerOptions(opts dockerOptions) error {
	if err := validateImageTag(opts.ImageTag); err != nil {
		return err
	}
	if err := validateContainerName(opts.ContainerName); err != nil {
		return err
	}
	if err := validateDockerEnv(opts.Env); err != nil {
		return err
	}
	return validateDockerHost(opts.Host)
}
