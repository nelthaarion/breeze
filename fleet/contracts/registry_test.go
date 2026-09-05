package contracts

import (
	"testing"

	"github.com/nelthaarion/breeze/v2/scalar"
)

func TestOperationNormalizesBreezeRoute(t *testing.T) {
	r := NewSchemaRegistry(nil)
	r.docs["orders"] = schemaEntry{hash: "one", doc: scalar.OpenAPI{Paths: map[string]scalar.PathItem{"/orders/{id}": {"post": scalar.Operation{Summary: "charge"}}}}}
	op, ok := r.Operation("orders", "/orders/:id", "POST")
	if !ok || op.Summary != "charge" {
		t.Fatalf("op=%+v ok=%v", op, ok)
	}
}
