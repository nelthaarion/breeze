package gnettransport

import "testing"

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "gnet" {
		t.Fatalf("name=%q", got)
	}
}
