package livesource

import (
	"errors"
	"net/url"
	"testing"
)

func TestCurrentIsNilUntilSetAndSetNilWithdraws(t *testing.T) {
	Set(nil)
	if Current() != nil {
		t.Fatal("a reader is published before anything set one")
	}

	Set(Func(func(path string, _ url.Values) (any, error) { return path, nil }))
	got, err := Current().Read("/logs", nil)
	if err != nil || got != "/logs" {
		t.Fatalf("Read = %v, %v; want the path echoed", got, err)
	}

	Set(nil)
	if Current() != nil {
		t.Fatal("Set(nil) did not withdraw the reader")
	}
}

func TestLastSetWins(t *testing.T) {
	t.Cleanup(func() { Set(nil) })

	Set(Func(func(string, url.Values) (any, error) { return "first", nil }))
	Set(Func(func(string, url.Values) (any, error) { return "second", nil }))
	if got, _ := Current().Read("/x", nil); got != "second" {
		t.Fatalf("Read = %v, want the later reader", got)
	}
}

func TestErrorCarriesTheStatus(t *testing.T) {
	var err error = &Error{Status: 404, Message: "no such timeline"}
	var e *Error
	if !errors.As(err, &e) || e.Status != 404 {
		t.Fatalf("errors.As lost the status: %v", err)
	}
	if got := err.Error(); got != "404: no such timeline" {
		t.Errorf("Error() = %q", got)
	}
}
