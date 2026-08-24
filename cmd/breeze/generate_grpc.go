package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// generate_grpc.go — gRPC generator with file-based detection + grpc_type metadata.
//
// Detection rules (revised):
//
//  1. The generator recursively scans the ENTIRE project for files whose
//     name ends in `_grpc.go` (e.g. `user_grpc.go`, `echo_grpc.go`).
//     These files are "gRPC interface files".
//
//  2. Every interface type declared in a `_grpc.go` file is treated as a
//     gRPC service. There is NO `_GRPC` suffix requirement on methods —
//     being in a `_grpc.go` file is sufficient.
//
//  3. Each method's gRPC call type is determined by a `grpc_type` comment
//     annotation on the method:
//
//       // grpc_type: Unary
//       GetUser(ctx context.Context, req GetUserRequest) (*UserResponse, error)
//
//       // grpc_type: ServerSideStreaming
//       Subscribe(req SubscribeRequest, stream SubscribeServer) error
//
//       // grpc_type: ClientSideStreaming
//       Upload(stream UploadServer) (*UploadResponse, error)
//
//       // grpc_type: Bidirectional
//       Chat(stream ChatServer) error
//
//     If no `grpc_type` comment is present, the method defaults to `Unary`.
//
//  4. Generation depends on grpc_type — each method is generated as the
//     appropriate gRPC shape (unary, server-stream, client-stream, bidi).
//
// Directories excluded from scanning: generated/, vendor/, .git/, and
// any directory starting with ".".

// grpcInterface describes an interface found in a _grpc.go file.
type grpcInterface struct {
	Name    string                 // interface name, e.g. "UserService"
	File    string                 // relative path to the .go file
	Pkg     string                 // Go package name of the file
	Methods []grpcMethod           // all methods on the interface
	Structs map[string]*grpcStruct // scanned struct types (request/response)
}

// grpcMethod describes a single method on a gRPC interface.
type grpcMethod struct {
	Name       string         // method name, e.g. "GetUser"
	Params     []string       // parameter type names
	Results    []string       // result type names
	Kind       grpcMethodKind // determined by grpc_type comment
	ReqType    string         // request type name
	RespType   string         // response type name
	StreamType string         // stream interface name
	GrpcType   string         // raw grpc_type value from comment ("" = default Unary)
}

// grpcMethodKind classifies a method by its gRPC call pattern.
type grpcMethodKind int

const (
	grpcInvalid grpcMethodKind = iota
	grpcUnary
	grpcServerStream
	grpcClientStream
	grpcBidiStream
)

func (k grpcMethodKind) String() string {
	switch k {
	case grpcUnary:
		return "unary"
	case grpcServerStream:
		return "server-stream"
	case grpcClientStream:
		return "client-stream"
	case grpcBidiStream:
		return "bidi-stream"
	default:
		return "invalid"
	}
}

// generateGRPC is the entry point for `breeze generate grpc <Name>`.
func generateGRPC(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate grpc", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite existing generated files")

	flagArgs, _ := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	// Scan all _grpc.go files in the project for the named interface.
	ifaces, err := scanGRPCInterfaces(name)
	if err != nil {
		return err
	}

	if len(ifaces) == 0 {
		return fmt.Errorf("no interface named %q found in any *_grpc.go file — define your interface in a file ending in _grpc.go (e.g. user_grpc.go)", name)
	}

	// Validate each method's signature based on its grpc_type.
	for i := range ifaces {
		for j := range ifaces[i].Methods {
			m := &ifaces[i].Methods[j]
			if err := validateGRPCMethod(ifaces[i].Name, m); err != nil {
				return err
			}
		}
	}

	// Scan for struct definitions referenced by the methods.
	for i := range ifaces {
		iface := &ifaces[i]
		typeNames := collectStructTypes(iface)
		structs, err := scanStructs(typeNames)
		if err != nil {
			return fmt.Errorf("%s: %w", iface.Name, err)
		}
		iface.Structs = structs
	}

	// Generate files.
	for i := range ifaces {
		iface := &ifaces[i]
		fmt.Printf("Generating gRPC files for %s (%d method(s)):\n", iface.Name, len(iface.Methods))
		for _, m := range iface.Methods {
			fmt.Printf("  %s — %s\n", m.Name, m.Kind)
		}
		if err := generateGRPCFiles(modulePath, iface, *force); err != nil {
			return err
		}
	}

	// Print next-steps hint.
	fmt.Printf("\nGeneration complete. %d service(s) generated under generated/.\n", len(ifaces))
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Install gRPC + protobuf dependencies:\n")
	fmt.Printf("     go get google.golang.org/grpc google.golang.org/protobuf\n")
	fmt.Printf("  2. Register the server adapter on your gRPC server:\n")
	fmt.Printf("     srv := grpc.NewServer()\n")
	for _, iface := range ifaces {
		fmt.Printf("     %s.Register%sServer(srv, %s.New%sGRPCServer(impl))\n",
			iface.Name, iface.Name, iface.Name, iface.Name)
	}
	fmt.Printf("\nNote: re-running `breeze generate grpc %s` is safe — pass --force\n", name)
	fmt.Printf("to overwrite existing generated files.\n")
	return nil
}

// ─── Interface scanning ──────────────────────────────────────────────────────

// scanGRPCInterfaces recursively walks the entire project for files ending
// in `_grpc.go`, parses them, and looks for an interface named `name`.
//
// Excludes: generated/, vendor/, .git/, directories starting with ".",
// and _test.go files.
func scanGRPCInterfaces(name string) ([]grpcInterface, error) {
	var found []grpcInterface
	fset := token.NewFileSet()

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable paths
		}
		if info.IsDir() {
			base := filepath.Base(path)
			// Skip generated output, vendor, hidden dirs, .git.
			if base == "generated" || base == "vendor" || base == ".git" ||
				(base != "." && strings.HasPrefix(base, ".")) {
				return filepath.SkipDir
			}
			return nil
		}

		fn := info.Name()
		// Only scan _grpc.go files (not _test.go).
		if !strings.HasSuffix(fn, "_grpc.go") || strings.HasSuffix(fn, "_test.go") {
			return nil
		}

		node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil // skip unparseable files
		}

		iface := scanFileForGRPCInterface(node, name, path)
		if iface != nil {
			found = append(found, *iface)
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("scanning project: %w", err)
	}
	return found, nil
}

// scanFileForGRPCInterface inspects a parsed Go file for an interface type
// declaration named `name`. All methods on the interface are collected.
// The grpc_type for each method is parsed from its doc comment.
func scanFileForGRPCInterface(file *ast.File, name, path string) *grpcInterface {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}

			ifaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}

			var methods []grpcMethod
			for _, field := range ifaceType.Methods.List {
				if len(field.Names) == 0 {
					continue // embedded interface
				}
				for _, methodName := range field.Names {
					// Parse grpc_type from the field's doc comment.
					grpcType := parseGrpcTypeComment(field.Doc)
					m := parseMethodSignature(methodName.Name, field.Type)
					m.GrpcType = grpcType
					// Determine Kind from grpc_type (default: Unary).
					m.Kind = grpcTypeToKind(grpcType)
					methods = append(methods, m)
				}
			}

			if len(methods) == 0 {
				return nil
			}

			return &grpcInterface{
				Name:    name,
				File:    path,
				Pkg:     file.Name.Name,
				Methods: methods,
			}
		}
	}
	return nil
}

// parseGrpcTypeComment extracts the grpc_type value from a method's doc
// comment. Looks for lines like:
//
//	// grpc_type: ServerSideStreaming
//	//grpc_type:Bidirectional
//
// Returns "" if no grpc_type comment is found (caller defaults to Unary).
func parseGrpcTypeComment(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	for _, comment := range doc.List {
		text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
		if strings.HasPrefix(strings.ToLower(text), "grpc_type:") {
			val := strings.TrimSpace(text[len("grpc_type:"):])
			// Strip quotes if present.
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// grpcTypeToKind converts a grpc_type string to a grpcMethodKind.
// Defaults to grpcUnary if empty or unrecognized.
func grpcTypeToKind(grpcType string) grpcMethodKind {
	switch strings.ToLower(strings.TrimSpace(grpcType)) {
	case "", "unary":
		return grpcUnary
	case "serversidestreaming", "server-stream", "serverstream", "server_stream":
		return grpcServerStream
	case "clientsidestreaming", "client-stream", "clientstream", "client_stream":
		return grpcClientStream
	case "bidirectional", "bidi", "bidi-stream", "bidistreaming":
		return grpcBidiStream
	default:
		return grpcUnary // unknown → default to unary
	}
}

// ─── Signature validation (type-driven) ──────────────────────────────────────

// validateGRPCMethod validates a method's signature based on its grpc_type.
// The grpc_type (already parsed) determines the expected shape:
//
//	Unary:                (ctx context.Context, req ReqT) (*RespT, error)
//	ServerSideStreaming:  (req ReqT, stream StreamT) error
//	ClientSideStreaming:  (stream StreamT) (*RespT, error)
//	Bidirectional:        (stream StreamT) error
func validateGRPCMethod(ifaceName string, m *grpcMethod) error {
	fail := func(rule string) error {
		return fmt.Errorf(
			"%s.%s: invalid signature for grpc_type=%q — %s\n"+
				"  got: %s(%s) (%s)\n"+
				"  expected shape: %s",
			ifaceName, m.Name, m.GrpcType, rule,
			m.Name, strings.Join(m.Params, ", "), strings.Join(m.Results, ", "),
			expectedShape(m.Kind))
	}

	switch m.Kind {
	case grpcUnary:
		if len(m.Params) != 2 {
			return fail("unary requires exactly 2 parameters: (ctx context.Context, req ReqT)")
		}
		if !isContextType(m.Params[0]) {
			return fail("unary: first parameter must be context.Context")
		}
		if !isNamedStruct(m.Params[1]) {
			return fail("unary: second parameter must be a named struct (the request type)")
		}
		m.ReqType = stripPointerType(m.Params[1])
		if len(m.Results) != 2 {
			return fail("unary requires exactly 2 results: (*RespT, error)")
		}
		if !isPointerToNamedStruct(m.Results[0]) {
			return fail("unary: first result must be *NamedStruct (the response type)")
		}
		m.RespType = stripPointerType(m.Results[0])
		if m.Results[1] != "error" {
			return fail("unary: last result must be error")
		}

	case grpcServerStream:
		if len(m.Params) != 2 {
			return fail("ServerSideStreaming requires exactly 2 parameters: (req ReqT, stream StreamT)")
		}
		if !isNamedStruct(m.Params[0]) {
			return fail("ServerSideStreaming: first parameter must be a named struct (the request type)")
		}
		m.ReqType = stripPointerType(m.Params[0])
		if !isStreamType(m.Params[1]) {
			return fail("ServerSideStreaming: second parameter must be a stream interface")
		}
		m.StreamType = m.Params[1]
		if len(m.Results) != 1 || m.Results[0] != "error" {
			return fail("ServerSideStreaming requires exactly 1 result: error")
		}

	case grpcClientStream:
		if len(m.Params) != 1 {
			return fail("ClientSideStreaming requires exactly 1 parameter: (stream StreamT)")
		}
		if !isStreamType(m.Params[0]) {
			return fail("ClientSideStreaming: parameter must be a stream interface")
		}
		m.StreamType = m.Params[0]
		if len(m.Results) != 2 {
			return fail("ClientSideStreaming requires exactly 2 results: (*RespT, error)")
		}
		if !isPointerToNamedStruct(m.Results[0]) {
			return fail("ClientSideStreaming: first result must be *NamedStruct (the response type)")
		}
		m.RespType = stripPointerType(m.Results[0])
		if m.Results[1] != "error" {
			return fail("ClientSideStreaming: last result must be error")
		}

	case grpcBidiStream:
		if len(m.Params) != 1 {
			return fail("Bidirectional requires exactly 1 parameter: (stream StreamT)")
		}
		if !isStreamType(m.Params[0]) {
			return fail("Bidirectional: parameter must be a stream interface")
		}
		m.StreamType = m.Params[0]
		if len(m.Results) != 1 || m.Results[0] != "error" {
			return fail("Bidirectional requires exactly 1 result: error")
		}
	}

	return nil
}

// expectedShape returns a human-readable description of the expected
// method signature for a given kind.
func expectedShape(kind grpcMethodKind) string {
	switch kind {
	case grpcUnary:
		return "(ctx context.Context, req ReqT) (*RespT, error)"
	case grpcServerStream:
		return "(req ReqT, stream StreamT) error"
	case grpcClientStream:
		return "(stream StreamT) (*RespT, error)"
	case grpcBidiStream:
		return "(stream StreamT) error"
	default:
		return "unknown"
	}
}

// ─── Type helpers ────────────────────────────────────────────────────────────

func isContextType(typeStr string) bool {
	return typeStr == "context.Context" || typeStr == "Context"
}

func isNamedStruct(typeStr string) bool {
	if typeStr == "" {
		return false
	}
	if strings.HasPrefix(typeStr, "*") || strings.HasPrefix(typeStr, "[]") || strings.HasPrefix(typeStr, "map[") {
		return false
	}
	switch typeStr {
	case "string", "int", "int32", "int64", "uint", "uint32", "uint64",
		"float32", "float64", "bool", "byte", "rune", "error",
		"interface{}", "any":
		return false
	}
	return true
}

func isPointerToNamedStruct(typeStr string) bool {
	if !strings.HasPrefix(typeStr, "*") {
		return false
	}
	return isNamedStruct(strings.TrimPrefix(typeStr, "*"))
}

func isStreamType(typeStr string) bool {
	// For user-defined stream types (structs like SubServer, ChunkData,
	// ChatMessage), we accept any named struct as a valid stream type.
	// The grpc_type comment already tells us this is a streaming method,
	// so the stream parameter just needs to be a named type.
	if typeStr == "" || typeStr == "interface{}" || typeStr == "any" {
		return true
	}
	// Accept any named struct (not primitives, pointers, slices, maps).
	return isNamedStruct(typeStr)
}

func stripPointerType(typeStr string) string {
	return strings.TrimPrefix(typeStr, "*")
}

// ─── Method signature parsing ────────────────────────────────────────────────

func parseMethodSignature(name string, typ ast.Expr) grpcMethod {
	fnType, ok := typ.(*ast.FuncType)
	if !ok {
		return grpcMethod{Name: name}
	}

	var params []string
	if fnType.Params != nil {
		for _, param := range fnType.Params.List {
			typeStr := exprString(param.Type)
			if len(param.Names) == 0 {
				params = append(params, typeStr)
			} else {
				for range param.Names {
					params = append(params, typeStr)
				}
			}
		}
	}

	var results []string
	if fnType.Results != nil {
		for _, result := range fnType.Results.List {
			typeStr := exprString(result.Type)
			if len(result.Names) == 0 {
				results = append(results, typeStr)
			} else {
				for range result.Names {
					results = append(results, typeStr)
				}
			}
		}
	}

	return grpcMethod{Name: name, Params: params, Results: results}
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.ArrayType:
		return "[]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func(...)"
	default:
		return fmt.Sprintf("%T", expr)
	}
}
