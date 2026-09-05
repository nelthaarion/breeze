package generator

import (
	"os"
	"strings"
	"testing"
)

func withTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

func TestUpsertRouteBlockCreatesFile(t *testing.T) {
	withTempDir(t)

	err := upsertRouteBlock("myapp", "User", `router.Handle(breeze.GET, "/users", handlers.ListUsers)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(registryFileName)
	if err != nil {
		t.Fatalf("reading generated file: %v", err)
	}
	content := string(got)

	for _, want := range []string{
		"package main",
		`"myapp/handlers"`,
		"// breeze:route:User:start",
		`router.Handle(breeze.GET, "/users", handlers.ListUsers)`,
		"// breeze:route:User:end",
		"func RegisterGeneratedRoutes(router *breeze.Router)",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("generated file missing %q\n--- content ---\n%s", want, content)
		}
	}
}

func TestUpsertRouteBlockIsIdempotent(t *testing.T) {
	withTempDir(t)

	if err := upsertRouteBlock("myapp", "User", `router.Handle(breeze.GET, "/users", handlers.ListUsers)`); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsertRouteBlock("myapp", "User", `router.Handle(breeze.GET, "/users", handlers.ListUsersV2)`); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := os.ReadFile(registryFileName)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)

	if strings.Contains(content, "ListUsers)") && !strings.Contains(content, "ListUsersV2") {
		t.Fatal("expected block to be replaced with updated content")
	}
	if strings.Count(content, "breeze:route:User:start") != 1 {
		t.Fatalf("expected exactly one User block, got content:\n%s", content)
	}
}

func TestUpsertRouteBlockAppendsSecondResource(t *testing.T) {
	withTempDir(t)

	if err := upsertRouteBlock("myapp", "User", `router.Handle(breeze.GET, "/users", handlers.ListUsers)`); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsertRouteBlock("myapp", "Post", `router.Handle(breeze.GET, "/posts", handlers.ListPosts)`); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := os.ReadFile(registryFileName)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)

	for _, want := range []string{"breeze:route:User:start", "breeze:route:Post:start", "ListUsers", "ListPosts"} {
		if !strings.Contains(content, want) {
			t.Errorf("expected content to contain %q\n--- content ---\n%s", want, content)
		}
	}
}
