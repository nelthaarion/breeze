package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate_grpc_test.go â€” tests for the gRPC generator.
//
// Tests use the new detection rules:
//   - Files must end in `_grpc.go`
//   - No `_GRPC` suffix needed on methods
//   - grpc_type comment sets the method kind (default: Unary)

// helper to write a go.mod + a single _grpc.go file and run generateGRPC.
func setupGRPCGen(t *testing.T, ifaceName, src string, args ...string) error {
	t.Helper()
	t.Chdir(t.TempDir())

	if err := os.WriteFile(
		"go.mod",
		[]byte("module example.com/test\n\ngo 1.24.3\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("handlers", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join("handlers", "service_grpc.go"),
		[]byte(src),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	return generateGRPC("example.com/test", ifaceName, args)
}

func TestGenerateGRPC_UnaryService(t *testing.T) {
	src := `package handlers

import "context"

type UserService interface {
	// grpc_type: Unary
	GetUser(ctx context.Context, req GetUserRequest) (*UserResponse, error)
}

type GetUserRequest struct {
	ID int64 ` + "`grpc:\"id\"`" + `
}

type UserResponse struct {
	Name string ` + "`grpc:\"name\"`" + `
}
`
	if err := setupGRPCGen(t, "UserService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestGenerateGRPC_DefaultUnary(t *testing.T) {
	// No grpc_type comment â†’ defaults to Unary.
	src := `package handlers

import "context"

type EchoService interface {
	Echo(ctx context.Context, req EchoRequest) (*EchoResponse, error)
}

type EchoRequest struct {
	Msg string ` + "`grpc:\"msg\"`" + `
}

type EchoResponse struct {
	Msg string ` + "`grpc:\"msg\"`" + `
}
`
	if err := setupGRPCGen(t, "EchoService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestGenerateGRPC_ServerSideStreaming(t *testing.T) {
	src := `package handlers

type StreamService interface {
	// grpc_type: ServerSideStreaming
	Subscribe(req SubRequest, stream SubServer) error
}

type SubRequest struct {
	Topic string ` + "`grpc:\"topic\"`" + `
}

type SubServer struct {
	Data string ` + "`grpc:\"data\"`" + `
}
`
	if err := setupGRPCGen(t, "StreamService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
	// Verify SubServer is generated as a message.
	pbContent, err := os.ReadFile("generated/StreamService/stream_service.pb.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pbContent), "type SubServer struct") {
		t.Error("SubServer struct not found in generated .pb.go")
	}
}

func TestGenerateGRPC_ClientSideStreaming(t *testing.T) {
	src := `package handlers

type UploadService interface {
	// grpc_type: ClientSideStreaming
	Upload(stream ChunkData) (*UploadResponse, error)
}

type ChunkData struct {
	Data []byte ` + "`grpc:\"data\"`" + `
}

type UploadResponse struct {
	Bytes int64 ` + "`grpc:\"bytes\"`" + `
}
`
	if err := setupGRPCGen(t, "UploadService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestGenerateGRPC_Bidirectional(t *testing.T) {
	src := `package handlers

type ChatService interface {
	// grpc_type: Bidirectional
	Chat(stream ChatMessage) error
}

type ChatMessage struct {
	Text string ` + "`grpc:\"text\"`" + `
}
`
	if err := setupGRPCGen(t, "ChatService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestGenerateGRPC_InterfaceNotFound(t *testing.T) {
	src := `package handlers

import "context"

type OtherService interface {
	Echo(ctx context.Context, req EchoRequest) (*EchoResponse, error)
}

type EchoRequest struct{ Msg string }
type EchoResponse struct{ Msg string }
`
	err := setupGRPCGen(t, "NonExistentService", src)
	if err == nil {
		t.Fatal("expected error for non-existent interface, got nil")
	}
	if !strings.Contains(err.Error(), "no interface named") {
		t.Errorf("expected 'no interface named' error, got: %v", err)
	}
}

func TestGenerateGRPC_RequiresGrpcGoFile(t *testing.T) {
	// Interface in a regular .go file (not _grpc.go) should NOT be found.
	t.Chdir(t.TempDir())

	if err := os.WriteFile(
		"go.mod",
		[]byte("module example.com/test\n\ngo 1.24.3\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("handlers", 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package handlers

import "context"

type UserService interface {
	GetUser(ctx context.Context, req GetUserRequest) (*UserResponse, error)
}

type GetUserRequest struct{ ID int64 }
type UserResponse struct{ Name string }
`
	// File is named user.go, NOT user_grpc.go.
	if err := os.WriteFile("handlers/user.go", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	err := generateGRPC("example.com/test", "UserService", nil)
	if err == nil {
		t.Fatal("expected error because interface is not in a _grpc.go file")
	}
}

func TestGenerateGRPC_ForceFlagAccepted(t *testing.T) {
	src := `package handlers

import "context"

type EchoService interface {
	Echo(ctx context.Context, req EchoRequest) (*EchoResponse, error)
}

type EchoRequest struct{ Msg string ` + "`grpc:\"msg\"`" + ` }
type EchoResponse struct{ Msg string ` + "`grpc:\"msg\"`" + ` }
`
	if err := setupGRPCGen(t, "EchoService", src, "--force"); err != nil {
		t.Fatalf("failed with --force: %v", err)
	}
}

func TestGenerateGRPC_MultipleMethods(t *testing.T) {
	src := `package handlers

import "context"

type MultiService interface {
	// grpc_type: Unary
	GetUser(ctx context.Context, req GetUserRequest) (*UserResponse, error)
	// grpc_type: Unary
	ListUsers(ctx context.Context, req ListRequest) (*ListResponse, error)
	// grpc_type: ServerSideStreaming
	Subscribe(req SubRequest, stream SubData) error
	// grpc_type: Bidirectional
	Chat(stream ChatMsg) error
}

type GetUserRequest struct{ ID int64 ` + "`grpc:\"id\"`" + ` }
type UserResponse struct{ Name string ` + "`grpc:\"name\"`" + ` }
type ListRequest struct{ Page int ` + "`grpc:\"page\"`" + ` }
type ListResponse struct{ Total int ` + "`grpc:\"total\"`" + ` }
type SubRequest struct{ Topic string ` + "`grpc:\"topic\"`" + ` }
type SubData struct{ Event string ` + "`grpc:\"event\"`" + ` }
type ChatMsg struct{ Text string ` + "`grpc:\"text\"`" + ` }
`
	if err := setupGRPCGen(t, "MultiService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestGenerateGRPC_StructTagOmit(t *testing.T) {
	src := `package handlers

import "context"

type UserService interface {
	GetUser(ctx context.Context, req GetUserRequest) (*UserResponse, error)
}

type GetUserRequest struct {
	ID       int64  ` + "`grpc:\"id\"`" + `
	Internal string ` + "`grpc:\"-\"`" + `
}

type UserResponse struct {
	Name string ` + "`grpc:\"name\"`" + `
}
`
	if err := setupGRPCGen(t, "UserService", src); err != nil {
		t.Fatalf("failed: %v", err)
	}
	// Verify the omitted field doesn't appear in .proto.
	proto, _ := os.ReadFile("generated/UserService/user_service.proto")
	if strings.Contains(string(proto), "Internal") {
		t.Error("grpc:\"-\" field appeared in .proto")
	}
}

func TestGenerateGRPC_MissingStructDefinition(t *testing.T) {
	src := `package handlers

import "context"

type UserService interface {
	GetUser(ctx context.Context, req MissingRequest) (*MissingResponse, error)
}
`
	err := setupGRPCGen(t, "UserService", src)
	if err == nil {
		t.Fatal("expected error for missing struct definitions")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}
