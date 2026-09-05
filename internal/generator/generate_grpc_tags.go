package generator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// generate_grpc_tags.go â€” Phase 3: struct tag analysis.
//
// This file implements scanning of Go source files for struct types
// referenced by _GRPC methods (request and response types). For each
// struct, it parses the `grpc:"..."` struct tag to control the generated
// protobuf field name and field number.
//
// Tag semantics:
//
//      grpc:"name"      â€” controls the generated protobuf field name. Defaults
//                         to lower-camel-case of the Go field name if absent.
//      grpc:"-"         â€” the field is omitted from the generated protobuf
//                         message entirely (like json:"-").
//      grpc:"name,1"    â€” field name "name" with explicit field number 1.
//                         Field numbers are assigned automatically in
//                         declaration order starting at 1 if not specified.
//
// Type mapping (Go â†’ protobuf):
//
//      string           â†’ string
//      bool             â†’ bool
//      int32            â†’ int32
//      int, int64       â†’ int64
//      uint32           â†’ uint32
//      uint, uint64     â†’ uint64
//      float32          â†’ float
//      float64           â†’ double
//      []byte           â†’ bytes
//      time.Time        â†’ google.protobuf.Timestamp
//      []T              â†’ repeated T
//      map[string]T     â†’ map<string, T>
//      named struct T   â†’ T (message reference)
//      *T               â†’ T (treated as optional)
//
// Types not in this table cause generation to fail with a clear error.

// grpcField describes a single field on a struct that will become a
// protobuf message field.
type grpcField struct {
	GoName      string // Go field name, e.g. "UserName"
	ProtoName   string // protobuf field name, e.g. "user_name" (from tag or lower-camel)
	GoType      string // Go type string, e.g. "string", "[]byte", "GetUserRequest"
	ProtoType   string // protobuf type string, e.g. "string", "bytes", "GetUserRequest"
	FieldNumber int    // protobuf field number (1-based, assigned in declaration order)
	Repeated    bool   // true for slices (repeated in protobuf)
	IsMessage   bool   // true if the field is a message reference (nested struct)
	Omit        bool   // true if grpc:"-" â€” field is skipped
}

// grpcStruct describes a Go struct type that will become a protobuf message.
type grpcStruct struct {
	Name     string      // struct name, e.g. "GetUserRequest"
	GoFile   string      // source file, e.g. "handlers/user.go"
	Fields   []grpcField // non-omitted fields (grpc:"-" fields are excluded)
	UsesTime bool        // true if any field uses time.Time (needs google.protobuf.Timestamp import)
}

// scanStructs finds struct type definitions for the given type names by
// walking Go source files in the project root + handlers/ directory.
//
// Returns a map of type name â†’ *grpcStruct. If a referenced type is not
// found, returns an error naming the missing type.
func scanStructs(typeNames []string) (map[string]*grpcStruct, error) {
	if len(typeNames) == 0 {
		return map[string]*grpcStruct{}, nil
	}

	// Build a set of types we need to find.
	needed := make(map[string]bool, len(typeNames))
	for _, n := range typeNames {
		needed[n] = true
	}

	found := make(map[string]*grpcStruct)
	dirs := []string{".", "handlers"}
	fset := token.NewFileSet()

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", dir, err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			fn := entry.Name()
			if !strings.HasSuffix(fn, ".go") || strings.HasSuffix(fn, "_test.go") {
				continue
			}

			path := filepath.Join(dir, fn)
			node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				continue
			}

			scanFileForStructs(node, needed, found, path)

			// Stop early if we've found everything.
			if len(found) == len(needed) {
				return found, nil
			}
		}
	}

	// Check for missing types.
	var missing []string
	for n := range needed {
		if _, ok := found[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("struct type(s) not found: %s â€” define them in a .go file in the project root or handlers/ directory", strings.Join(missing, ", "))
	}

	return found, nil
}

// scanFileForStructs inspects a parsed Go file for struct type definitions
// whose names are in `needed`. Found structs are added to `found`.
func scanFileForStructs(file *ast.File, needed map[string]bool, found map[string]*grpcStruct, path string) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if !needed[typeSpec.Name.Name] {
				continue
			}
			if _, alreadyFound := found[typeSpec.Name.Name]; alreadyFound {
				continue
			}

			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue // not a struct
			}

			s := parseStruct(typeSpec.Name.Name, structType, path)
			found[typeSpec.Name.Name] = s
		}
	}
}

// parseStruct converts an AST struct type into a grpcStruct, parsing
// grpc:"..." tags on each field and mapping Go types to protobuf types.
func parseStruct(name string, structType *ast.StructType, file string) *grpcStruct {
	s := &grpcStruct{
		Name:   name,
		GoFile: file,
	}

	fieldNum := 0
	for _, field := range structType.Fields.List {
		// Skip fields with no names (embedded fields).
		if len(field.Names) == 0 {
			continue
		}

		goType := exprString(field.Type)
		protoType, isMessage, repeated, err := mapGoTypeToProto(goType)
		if err != nil {
			// Unknown type â€” skip the field but record nothing.
			// Phase 4 will make this an error.
			continue
		}

		for _, fieldName := range field.Names {
			// Parse the grpc tag.
			tag := ""
			if field.Tag != nil {
				// field.Tag.Value is a raw string literal like `"json:\"id\" grpc:\"user_id\""`
				tag = field.Tag.Value
			}
			grpcTag := parseStructTag(tag, "grpc")

			// grpc:"-" â†’ omit this field.
			if grpcTag == "-" {
				continue
			}

			fieldNum++

			// Determine the protobuf field name.
			protoName := grpcTag
			if protoName == "" {
				protoName = lowerCamel(fieldName.Name)
			}

			// Check for explicit field number: grpc:"name,N"
			explicitNum := 0
			if grpcTag != "" {
				if parts := strings.SplitN(grpcTag, ",", 2); len(parts) == 2 {
					protoName = parts[0]
					if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
						explicitNum = n
					}
				}
			}

			num := fieldNum
			if explicitNum > 0 {
				num = explicitNum
			}

			f := grpcField{
				GoName:      fieldName.Name,
				ProtoName:   protoName,
				GoType:      goType,
				ProtoType:   protoType,
				FieldNumber: num,
				Repeated:    repeated,
				IsMessage:   isMessage,
			}
			s.Fields = append(s.Fields, f)

			if goType == "time.Time" || strings.Contains(goType, "time.Time") {
				s.UsesTime = true
			}
		}
	}

	return s
}

// parseStructTag extracts the value of `name:"..."` from a raw Go struct
// tag string. Returns "" if the tag is not present.
func parseStructTag(rawTag, name string) string {
	// rawTag is a quoted string like `"json:\"id\" grpc:\"user_id\""`
	// Strip the surrounding quotes.
	if len(rawTag) < 2 {
		return ""
	}
	if rawTag[0] == '"' {
		rawTag = rawTag[1 : len(rawTag)-1]
	}

	// Use reflect.StructTag to parse properly.
	// We can't use reflect directly on the AST, but we can replicate
	// the parsing: look for name:"value" patterns.
	tag := structTag(rawTag)
	return tag.Get(name)
}

// structTag is a minimal implementation of reflect.StructTag for parsing
// struct tags from AST source. This avoids importing reflect on the AST
// (which would require a runtime value).
type structTag string

// Get returns the value associated with key in the tag string. The format
// is `key:"value" key2:"value2"`. Returns "" if the key is not present.
func (t structTag) Get(key string) string {
	// This mirrors reflect.StructTag.Get's logic.
	for len(t) > 0 {
		// Skip leading spaces.
		i := 0
		for i < len(t) && t[i] == ' ' {
			i++
		}
		t = t[i:]
		if len(t) == 0 {
			break
		}

		// Scan to colon.
		i = 0
		for i < len(t) && t[i] > ' ' && t[i] != ':' && t[i] != '"' && t[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(t) || t[i] != ':' || t[i+1] != '"' {
			break
		}
		keyName := string(t[:i])
		t = t[i+2:]

		// Scan quoted string to find value.
		i = 0
		for i < len(t) && t[i] != '"' {
			if t[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(t) {
			break
		}

		qvalue := string(t[:i])
		t = t[i+1:]

		if keyName == key {
			return qvalue
		}
	}
	return ""
}

// mapGoTypeToProto converts a Go type string to its protobuf equivalent.
// Returns (protoType, isMessage, repeated, error).
//
// isMessage is true for named struct references (they become protobuf
// message references). repeated is true for slices. Pointer types are
// treated as their underlying type (protobuf 3 has no pointers).
func mapGoTypeToProto(goType string) (protoType string, isMessage bool, repeated bool, err error) {
	// Handle slices first: []T â†’ repeated T
	if strings.HasPrefix(goType, "[]") {
		inner := goType[2:]
		pt, msg, _, e := mapGoTypeToProto(inner)
		if e != nil {
			return "", false, false, e
		}
		return pt, msg, true, nil
	}

	// Handle pointers: *T â†’ T (protobuf 3 has no pointers)
	if strings.HasPrefix(goType, "*") {
		return mapGoTypeToProto(goType[1:])
	}

	// Handle maps: map[K]V â†’ map<K, V>
	if strings.HasPrefix(goType, "map[") {
		// Parse map[K]V â€” find the matching ].
		depth := 1
		i := 4
		for i < len(goType) && depth > 0 {
			switch goType[i] {
			case '[':
				depth++
			case ']':
				depth--
			}
			if depth > 0 {
				i++
			}
		}
		if i >= len(goType) {
			return "", false, false, fmt.Errorf("malformed map type: %s", goType)
		}
		keyType := goType[4:i]
		valType := goType[i+1:]
		kp, _, _, e := mapGoTypeToProto(keyType)
		if e != nil {
			return "", false, false, e
		}
		vp, _, _, e := mapGoTypeToProto(valType)
		if e != nil {
			return "", false, false, e
		}
		return fmt.Sprintf("map<%s, %s>", kp, vp), false, false, nil
	}

	// Primitive types.
	switch goType {
	case "string":
		return "string", false, false, nil
	case "bool":
		return "bool", false, false, nil
	case "int32":
		return "int32", false, false, nil
	case "int", "int64":
		return "int64", false, false, nil
	case "uint32":
		return "uint32", false, false, nil
	case "uint", "uint64":
		return "uint64", false, false, nil
	case "float32":
		return "float", false, false, nil
	case "float64":
		return "double", false, false, nil
	case "byte":
		return "bytes", false, false, nil
	case "[]byte":
		return "bytes", false, true, nil
	case "time.Time":
		return "google.protobuf.Timestamp", false, false, nil
	case "interface{}", "any":
		return "", false, false, fmt.Errorf("interface{} is not supported in protobuf messages")
	}

	// Qualified types: pkg.Type â†’ Type (assume it's a message reference)
	if idx := strings.LastIndex(goType, "."); idx >= 0 {
		base := goType[idx+1:]
		return base, true, false, nil
	}

	// Anything else is assumed to be a named struct (message reference).
	// Phase 4 will validate that the struct actually exists.
	return goType, true, false, nil
}

// lowerCamel converts a PascalCase or CamelCase identifier to lowerCamelCase.
// e.g. "UserName" â†’ "userName", "ID" â†’ "id", "HTTPServer" â†’ "httpServer".
func lowerCamel(s string) string {
	if s == "" {
		return s
	}
	// Lowercase the first rune. If the first two are both uppercase,
	// lowercase the first (e.g. "ID" â†’ "id", "HTTPServer" â†’ "httpServer").
	runes := []rune(s)
	runes[0] = toLowerRune(runes[0])
	return string(runes)
}

// toLowerRune lowercases a single ASCII rune.
func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

// collectStructTypes gathers all struct type names referenced by the
// validated methods of an interface. These are the types that need to be
// scanned for grpc tags.
//
// For streaming methods, the StreamType is also a message struct (the
// element type sent/received over the stream) â€” it must be collected
// and generated as a protobuf message, not treated as an opaque interface.
func collectStructTypes(iface *grpcInterface) []string {
	seen := make(map[string]bool)
	for _, m := range iface.Methods {
		if m.ReqType != "" {
			seen[m.ReqType] = true
		}
		if m.RespType != "" {
			seen[m.RespType] = true
		}
		// Streaming methods have a StreamType that is also a struct
		// (e.g. SubServer). It must be generated as a protobuf message.
		if m.StreamType != "" {
			seen[m.StreamType] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out
}
