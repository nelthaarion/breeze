package contracts

import (
	"testing"
	"time"
)

func TestViolationStoreDedupesAndBounds(t *testing.T) {
	s := NewViolationStore(2, time.Minute)
	v := Violation{Caller: "a", Callee: "b", Route: "/x", Path: "/id", Rule: "required", Timestamp: 1}
	s.Add(v)
	v.Timestamp = 2
	g := s.Add(v)
	if g.Count != 2 || len(s.Snapshot()) != 1 {
		t.Fatalf("group=%+v snapshot=%+v", g, s.Snapshot())
	}
	s.Add(Violation{Caller: "c", Callee: "d", Route: "/y", Rule: "type", Timestamp: 3})
	s.Add(Violation{Caller: "e", Callee: "f", Route: "/z", Rule: "enum", Timestamp: 4})
	if len(s.Snapshot()) != 2 {
		t.Fatal("store not bounded")
	}
}
