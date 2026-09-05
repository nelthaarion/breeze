package wstransport

import (
	"testing"

	"github.com/nelthaarion/breeze/fleet"
)

// Compile-time proof that the rewrite still satisfies the interface the Tracer
// consumes. A missing or renamed method fails the build rather than surfacing at
// runtime as a transport that cannot be registered.
var _ fleet.Transport = (*Transport)(nil)

func TestInterfaceSatisfied(t *testing.T) {
	var tr fleet.Transport = New(Config{})
	if tr.Name() != "ws" {
		t.Fatalf("Name() = %q, want ws", tr.Name())
	}
}
