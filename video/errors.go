package video

import "errors"

// Sentinel errors returned by this package.
//
// These describe what went wrong internally. They are never written to the
// wire verbatim: [statusFor] maps them onto a status code and a fixed,
// non-revealing reason phrase, so a client learns "404 Not Found" while the
// log records "resolve /videos/../secret: escapes root".
//
// Match them with errors.Is; the resolver and range parser wrap them with
// %w and add detail that is safe for logs but not for responses.
var (
	// ErrNotFound is returned when the requested file does not exist, or
	// exists but must be treated as absent (hidden files, disallowed
	// extensions, directories with listing disabled). Collapsing these
	// into one error is deliberate: a probing client must not be able to
	// tell "denied" from "missing" and map out the filesystem.
	ErrNotFound = errors.New("video: file not found")

	// ErrForbiddenPath is returned when a request path escapes the mount
	// root, or resolves through a symlink that leaves it. This is a
	// security event and is logged as such; the client still sees 404.
	ErrForbiddenPath = errors.New("video: path outside root")

	// ErrDirectory is returned when the resolved target is a directory.
	ErrDirectory = errors.New("video: path is a directory")

	// ErrUnsupportedType is returned when a file's extension is not in
	// the mount's allow-list.
	ErrUnsupportedType = errors.New("video: unsupported media type")

	// ErrInvalidRange is returned for a syntactically malformed Range
	// header. Per RFC 9110 §14.2 a malformed Range is ignored rather than
	// rejected, so this yields a normal 200, not a 4xx.
	ErrInvalidRange = errors.New("video: malformed range header")

	// ErrRangeNotSatisfiable is returned when a Range is well-formed but
	// no part of it overlaps the file. This maps to 416.
	ErrRangeNotSatisfiable = errors.New("video: range not satisfiable")

	// ErrInvalidConfig is returned by [Mount] when the configuration
	// cannot produce a working mount. It fails at setup time, never at
	// request time.
	ErrInvalidConfig = errors.New("video: invalid configuration")

	// ErrSignatureRequired is returned when signed URLs are enforced and
	// the request carries no signature.
	ErrSignatureRequired = errors.New("video: signature required")

	// ErrInvalidSignature is returned when a signature is present but
	// does not verify against the secret, path, or binding claims.
	ErrInvalidSignature = errors.New("video: invalid signature")

	// ErrSignatureExpired is returned when a signature verified but its
	// expiry has passed.
	ErrSignatureExpired = errors.New("video: signature expired")
)

// statusFor maps an internal error onto the status code the client sees.
//
// Everything that reveals filesystem shape — traversal attempts, hidden
// files, blocked extensions, directories — collapses to 404. Signature
// failures are 403 because the client is being told "your credential is
// bad", which reveals nothing about the filesystem.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, ErrRangeNotSatisfiable):
		return 416
	case errors.Is(err, ErrSignatureRequired),
		errors.Is(err, ErrInvalidSignature),
		errors.Is(err, ErrSignatureExpired):
		return 403
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrForbiddenPath),
		errors.Is(err, ErrDirectory),
		errors.Is(err, ErrUnsupportedType):
		return 404
	default:
		return 500
	}
}
