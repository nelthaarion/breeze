package mcp

// docker_args.go — the last check before an argv reaches the docker binary.
//
// # What this is for, given that nothing here asks for host access
//
// This orchestrator never grants a container a host mount, a privileged flag, a
// shared namespace or host networking. runContainer builds exactly one shape —
//
//	run -d --name N [-e K=V ...] [-p 127.0.0.1:h:c ...] --add-host A:host-gateway IMAGE
//
// — and there is no field on dockerOptions that could add to it. So today the
// property holds.
//
// It holds by absence, which is the weakest way for a security property to hold. A
// field added to dockerOptions later, or a flag appended to runContainer for a
// plausible reason, would grant host access with nothing anywhere objecting. The
// checks in docker_names.go do not help: they validate the *values* a caller
// supplies, and this is about the *flags* this package emits.
//
// So the guard is here instead, at the point where an argv is complete and about to
// be executed. It is the same choke-point argument as confine.go: one place that
// everything passes through, rather than a list of call sites that has to stay
// complete. A future field granting a mount does not fail a review — it fails a test.
//
// # Why there is no opt-in
//
// Part 6 permits either locking this down or gating it behind a default-off flag. It
// is locked down, with no escape hatch, because nothing needs one.
//
// A provisioned container carries its own copy of the project: the Dockerfile does
// `COPY --from=build /src /workspace`, so the control instance inside it edits a tree
// the image already contains. That is the whole reason a host mount never came up.
// An unused opt-in is attack surface with no user, and the argument for adding one is
// always the same shape — "an operator might want it" — which is also the argument
// that would have made this hole in the first place. An operator who genuinely needs
// a mounted container has `docker run`, which is a deliberate act by a person who
// already holds Docker access rather than a JSON field an agent can populate.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// hostAccessFlags are the docker flags that let a container reach the host, or let a
// caller decide what runs inside one.
//
// Grouped by what each actually grants, because the list is otherwise just words and
// a reader deciding whether to extend it needs the principle rather than the entries.
var hostAccessFlags = map[string]string{
	// Filesystem. Any of these puts a host path inside the container, which is a
	// read or a write outside every boundary this server has — including the
	// workspace confinement, since that governs this process rather than a
	// container it starts.
	"-v":             "mounts a host path into the container",
	"--volume":       "mounts a host path into the container",
	"--mount":        "mounts a host path into the container",
	"--volumes-from": "inherits another container's mounts, including host ones",
	"--tmpfs":        "adds a mount this orchestrator did not plan",

	// Privilege. --privileged is the whole host; the others are the same thing
	// retail. --security-opt is included because that is how seccomp and AppArmor
	// get switched off, which is a privilege grant spelled as a restriction.
	"--privileged":         "grants the container host-level capabilities",
	"--cap-add":            "grants a Linux capability the default profile withholds",
	"--device":             "exposes a host device to the container",
	"--device-cgroup-rule": "widens the container's device access",
	"--security-opt":       "can disable the seccomp or AppArmor profile",
	"--userns":             "changes user-namespace isolation",
	"--user":               "chooses the uid inside the container",
	"-u":                   "chooses the uid inside the container",
	"--group-add":          "adds a supplementary group inside the container",
	"--sysctl":             "sets a kernel parameter",
	"--runtime":            "replaces the container runtime",
	"--cgroup-parent":      "places the container in a chosen cgroup",
	"--cgroupns":           "changes cgroup-namespace isolation",
	"--ulimit":             "changes a resource limit this orchestrator did not set",
	"--oom-kill-disable":   "lets the container survive the OOM killer at the host's expense",
	"--restart":            "makes the container outlive this orchestrator's registry",
	"--pull":               "can fetch an image this host did not build",

	// Namespaces. `--network host` is the interesting one — it puts the container on
	// the host's network stack, so every loopback service on this machine becomes
	// reachable from inside it, published ports and 127.0.0.1 bindings included.
	// The rest are the same escape through a different namespace.
	"--network": "chooses the network namespace, and `host` shares the host's own",
	"--net":     "chooses the network namespace, and `host` shares the host's own",
	"--pid":     "shares a pid namespace, exposing host processes",
	"--ipc":     "shares an ipc namespace",
	"--uts":     "shares the host's hostname namespace",

	// What runs. The provisioned entrypoint is what refuses to start without a
	// control token; replacing it or the command reaches around that.
	"--entrypoint": "replaces the entrypoint that requires a control token",
	"--init":       "changes what runs as pid 1",
}

// refuseHostAccess checks a complete docker argv.
//
// Every element is examined, not only the ones in flag position. That is deliberate:
// an operand that looks like a flag is the argument-smuggling case docker_names.go
// describes, and this is the point at which the distinction between "operand" and
// "flag" stops being this package's opinion and becomes Docker's parser's. A value
// that reaches here still resembling `--privileged` is refused whatever slot it was
// written into.
//
// Both `--flag value` and `--flag=value` are matched, because Docker accepts both and
// a check that knew only one would be a check that could be spelled around.
func refuseHostAccess(args []string) error {
	for _, arg := range args {
		name := arg
		if eq := strings.Index(arg, "="); eq > 0 {
			name = arg[:eq]
		}

		grants, denied := hostAccessFlags[name]
		if !denied {
			continue
		}
		return fmt.Errorf("refusing to run docker with %s: it %s. This orchestrator provisions "+
			"containers that carry their own copy of the project and need no access to this host, "+
			"so no tool argument can ask for any, and nothing in this package emits this flag. "+
			"Reaching this error means either a provisioning option was added without considering "+
			"what it grants, or a caller-supplied value reached a flag position: %q",
			name, grants, arg)
	}
	return nil
}

// hostAccessOptions are JSON field names a caller might use to ask for host access,
// mapped to what asking for it would mean.
//
// These are not fields on dockerOptions and never will be. They are listed so that a
// request containing one is refused with an explanation rather than with the generic
// unknown-field message — an agent told `privileged` is not a recognised option will
// try `privileged_mode` next, whereas one told this orchestrator grants no host
// access at all has learnt the actual rule.
var hostAccessOptions = map[string]string{
	"privileged":   "host-level capabilities",
	"volumes":      "a host path mounted into the container",
	"volume":       "a host path mounted into the container",
	"mounts":       "a host path mounted into the container",
	"mount":        "a host path mounted into the container",
	"binds":        "a host path mounted into the container",
	"network_mode": "a chosen network namespace, including the host's own",
	"network":      "a chosen network namespace, including the host's own",
	"cap_add":      "a Linux capability the default profile withholds",
	"capabilities": "a Linux capability the default profile withholds",
	"devices":      "a host device inside the container",
	"security_opt": "the seccomp or AppArmor profile disabled",
	"pid_mode":     "the host's pid namespace, exposing host processes",
	"ipc_mode":     "a shared ipc namespace",
	"user":         "a chosen uid inside the container",
	"entrypoint":   "a replaced entrypoint, bypassing the control-token check",
	"command":      "a replaced command, bypassing the control-token check",
	"docker_host":  "a different Docker daemon than this orchestrator's own",
}

// dockerOptionFields is what the docker object accepts, for the refusal message.
//
// Spelled out rather than derived by reflection over the struct tags: this string is
// read by whoever is being refused, and the order that helps them is the order the
// tool's description uses, not the order the fields happen to be declared in.
const dockerOptionFields = "host, image_tag, container_name, env, skip_build, " +
	"wait_seconds, enable_app_mcp"

// UnmarshalJSON decodes the docker object strictly.
//
// # Why strictly, when nothing else on this server is
//
// decodeArgs is deliberately lenient: MCP clients differ, and a tool that refused an
// extra field would be refusing calls that mean exactly what they say. This object is
// the exception, because the thing a caller would most plausibly add to it is
// `"privileged": true` — and leniency's answer to that is to drop it silently and
// provision an unprivileged container.
//
// Silently dropping is safe for the host and bad for everyone else. The caller
// believes they got what they asked for; the audit question "can a tool request a
// privileged container" has the unsatisfying answer "it can request one, it just does
// not get one"; and nothing is written down anywhere. Refusing makes the request
// visible: the call fails, with a message saying no such capability exists.
//
// The same strictness catches a typo — `wait_second` for `wait_seconds` — which was
// previously a silently ignored argument and a provision that waited when the caller
// asked it not to.
func (o *dockerOptions) UnmarshalJSON(data []byte) error {
	// An alias, so decoding it does not re-enter this method.
	type plain dockerOptions

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var decoded plain
	if err := dec.Decode(&decoded); err != nil {
		if field, ok := unknownJSONField(err); ok {
			if grants, denied := hostAccessOptions[strings.ToLower(field)]; denied {
				return fmt.Errorf("docker.%s is not an option this orchestrator has. It would ask "+
					"for %s, and no provisioning argument can grant a container any access to this "+
					"host: a provisioned container carries its own copy of the project and needs "+
					"none. Accepted fields are %s", field, grants, dockerOptionFields)
			}
			return fmt.Errorf("docker.%s is not a recognised option. Accepted fields are %s. "+
				"An unrecognised field is refused rather than ignored, because a silently dropped "+
				"option looks to the caller like one that was honoured", field, dockerOptionFields)
		}
		return err
	}

	*o = dockerOptions(decoded)
	return nil
}

// unknownJSONField extracts the field name from encoding/json's unknown-field error.
//
// The error is unexported and carries no structure, so its text is parsed. That is
// fragile in principle and safe in practice: the fallback is returning the raw error,
// which is still an accurate refusal — only the explanation is lost.
func unknownJSONField(err error) (string, bool) {
	const prefix = `json: unknown field "`

	text := err.Error()
	if !strings.HasPrefix(text, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(text, prefix)
	if end := strings.Index(name, `"`); end >= 0 {
		return name[:end], true
	}
	return "", false
}
