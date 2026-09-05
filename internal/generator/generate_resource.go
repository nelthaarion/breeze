package generator

import (
	"flag"
	"fmt"
	"strings"
)

const middlewareImport = `middleware "github.com/nelthaarion/breeze/v2/middlewares"`
const scalarImport = `"github.com/nelthaarion/breeze/v2/scalar"`

func generateResource(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate resource", flag.ContinueOnError)
	pluralOverride := fs.String("plural", "", "override the pluralized resource name (e.g. --plural=people)")
	pathOverride := fs.String("path", "", "route prefix (default /<plural>, e.g. --path=/api/v1/users)")
	methods := parseMethodsFlag(fs)
	noValidate := fs.Bool("no-validate", false, "do not infer validate tags for string fields")
	force := fs.Bool("force", false, "overwrite an existing handler file")
	out := registerOutputFlags(fs)

	flagArgs, positional := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	fields, err := parseFields(positional)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return fmt.Errorf("usage: breeze generate resource <Name> field:type[:rules] [field:type ...]")
	}

	// Inferred rules, unless the caller opted out. Explicit rules always win â€”
	// withValidationDefaults only fills fields that carry none.
	if !*noValidate {
		fields = withValidationDefaults(fields)
	}

	plural := *pluralOverride
	if plural == "" {
		plural = pluralize(name)
	}
	pathBase, err := resolveRoutePath(*pathOverride, plural)
	if err != nil {
		return err
	}

	// --methods was accepted but ignored here, so `--methods=list,get` still
	// produced all five routes and all five handlers.
	actions, err := actionsFor(name, plural, splitMethods(*methods))
	if err != nil {
		return err
	}

	target, err := out.target("handlers", strings.ToLower(name))
	if err != nil {
		return err
	}

	if err := writeResourceHandlerFileTo(target, modulePath, name, plural, pathBase, fields, actions, *force); err != nil {
		return err
	}

	docArgs := make([]string, len(actions))
	for i, a := range actions {
		docArgs[i] = routeDoc(a, name, plural, pathBase+a.PathSuffix)
	}

	return registerActionRoutes(modulePath, name, pathBase, actions, docArgs, middlewareImport, scalarImport)
}

// writeResourceHandlerFile writes the handler file to the destination the
// defaults derive, which is what a caller with no overrides in hand wants.
//
// It exists alongside the ...To form because the two callers differ in what they
// know: generateResource has already parsed --filename/--package and must resolve
// the destination before writing anything, while a caller that only has a
// resource name should not have to reconstruct the default derivation to get the
// same answer. Both end up in the same writer.
func writeResourceHandlerFile(name, plural, pathBase string, fields []field, actions []action, force bool) error {
	target, err := (*outputFlags)(nil).target("handlers", strings.ToLower(name))
	if err != nil {
		return err
	}
	return writeResourceHandlerFileTo(target, "", name, plural, pathBase, fields, actions, force)
}

// writeResourceHandlerFile writes the handler file to the destination the
// generator resolved.
//
// The default-destination form is writeResourceHandlerFileForName below; this
// one takes the resolved target because generateResource has to resolve it
// before writing anything, so that a bad --package or a filename another feature
// owns fails before the route block is touched.
func writeResourceHandlerFileTo(target outputTarget, modulePath, name, plural, pathBase string,
	fields []field, actions []action, force bool) error {

	nameLower := strings.ToLower(name)

	// Which handlers get emitted decides which imports are used. Emitting an
	// import for a handler that --methods excluded is a compile error in the
	// generated project, so this is derived rather than fixed — and the writer
	// prunes anything the finished body does not reference, so a wrong condition
	// here cannot produce an unused import either.
	want := make(map[string]bool, len(actions))
	for _, a := range actions {
		want[a.Name] = true
	}
	// The write actions decode a body, and they do it through binding.Bind
	// rather than json.Unmarshal so the validate tags on the request struct are
	// actually enforced. Bind takes sources, so the same handler grows query or
	// path binding by adding one argument.
	needsBinding := want["create"] || want["update"]
	// errors.As is only reachable when there is a rule that can fail; without
	// tags the 422 branch would be dead code in the user's project.
	validated := needsBinding && usesValidation(fields)
	needsFmt := want["create"]

	imports := []string{`"sync"`, `"github.com/nelthaarion/breeze/v2"`}
	if validated {
		imports = append(imports, `"errors"`)
	}
	if needsFmt {
		imports = append(imports, `"fmt"`)
	}
	if usesTime(fields) {
		imports = append(imports, timeImport)
	}
	if needsBinding {
		imports = append(imports, `"github.com/nelthaarion/breeze/v2/binding"`)
	}

	var b strings.Builder

	if needsBinding {
		if want["create"] {
			writeStruct(&b, "Create"+name+"Request", fields)
		}
		if want["update"] {
			writeStruct(&b, "Update"+name+"Request", fields)
		}
	}
	writeResponseStruct(&b, name+"Response", fields)

	if want["list"] {
		fmt.Fprintf(&b, "type %sListResponse struct {\n", name)
		fmt.Fprintf(&b, "\t%s []%sResponse `json:\"%s\"`\n", plural, name, strings.ToLower(plural))
		b.WriteString("\tTotal int `json:\"total\"`\n")
		b.WriteString("}\n\n")
	}

	if want["get"] || want["update"] || want["delete"] {
		fmt.Fprintf(&b, "type %sPathParams struct {\n", name)
		b.WriteString("\tID string `json:\"id\" description:\"")
		b.WriteString(name)
		b.WriteString(" ID\"`\n")
		b.WriteString("}\n\n")
	}

	b.WriteString("// In-memory store for scaffolding only â€” replace with real persistence\n// before production use.\n")
	fmt.Fprintf(&b, "var (\n\t%sMu sync.RWMutex\n\t%sStore = []%sResponse{}\n", nameLower, nameLower, name)
	if want["create"] {
		fmt.Fprintf(&b, "\t%sNextID = 1\n", nameLower)
	}
	b.WriteString(")\n\n")

	if want["list"] {
		fmt.Fprintf(&b, "// List%s handles GET %s.\nfunc List%s(ctx *breeze.Context) error {\n", plural, pathBase, plural)
		fmt.Fprintf(&b, "\t%sMu.RLock()\n\tdefer %sMu.RUnlock()\n", nameLower, nameLower)
		fmt.Fprintf(&b, "\treturn ctx.JSON(%sListResponse{%s: %sStore, Total: len(%sStore)})\n}\n\n", name, plural, nameLower, nameLower)
	}

	if want["get"] {
		fmt.Fprintf(&b, "// Get%s handles GET %s/:id.\nfunc Get%s(ctx *breeze.Context) error {\n", name, pathBase, name)
		fmt.Fprintf(&b, "\tid := ctx.GetParam(\"id\")\n\t%sMu.RLock()\n\tdefer %sMu.RUnlock()\n", nameLower, nameLower)
		fmt.Fprintf(&b, "\tfor _, item := range %sStore {\n\t\tif item.ID == id {\n\t\t\treturn ctx.JSON(item)\n\t\t}\n\t}\n", nameLower)
		writeNotFound(&b, nameLower)
	}

	if want["create"] {
		fmt.Fprintf(&b, "// Create%s handles POST %s.\nfunc Create%s(ctx *breeze.Context) error {\n", name, pathBase, name)
		fmt.Fprintf(&b, "\tvar req Create%sRequest\n\tif err := binding.Bind(&req, binding.JSONBody(ctx.Req.Body)); err != nil {\n", name)
		writeBindFailure(&b, nameLower, validated)
		fmt.Fprintf(&b, "\t%sMu.Lock()\n\tid := fmt.Sprintf(\"%%d\", %sNextID)\n\t%sNextID++\n", nameLower, nameLower, nameLower)
		fmt.Fprintf(&b, "\titem := %sResponse{ID: id", name)
		for _, f := range fields {
			fmt.Fprintf(&b, ", %s: req.%s", f.Name, f.Name)
		}
		b.WriteString("}\n")
		fmt.Fprintf(&b, "\t%sStore = append(%sStore, item)\n\t%sMu.Unlock()\n\n", nameLower, nameLower, nameLower)
		b.WriteString("\tctx.Status(201)\n\treturn ctx.JSON(item)\n}\n\n")
	}

	if want["update"] {
		fmt.Fprintf(&b, "// Update%s handles PUT %s/:id.\nfunc Update%s(ctx *breeze.Context) error {\n", name, pathBase, name)
		fmt.Fprintf(&b, "\tvar req Update%sRequest\n\tif err := binding.Bind(&req, binding.JSONBody(ctx.Req.Body)); err != nil {\n", name)
		writeBindFailure(&b, nameLower, validated)
		b.WriteString("\tid := ctx.GetParam(\"id\")\n")
		fmt.Fprintf(&b, "\t%sMu.Lock()\n\tdefer %sMu.Unlock()\n", nameLower, nameLower)
		fmt.Fprintf(&b, "\tfor i, item := range %sStore {\n\t\tif item.ID == id {\n", nameLower)
		for _, f := range fields {
			fmt.Fprintf(&b, "\t\t\t%sStore[i].%s = req.%s\n", nameLower, f.Name, f.Name)
		}
		fmt.Fprintf(&b, "\t\t\treturn ctx.JSON(%sStore[i])\n\t\t}\n\t}\n", nameLower)
		writeNotFound(&b, nameLower)
	}

	if want["delete"] {
		fmt.Fprintf(&b, "// Delete%s handles DELETE %s/:id.\nfunc Delete%s(ctx *breeze.Context) error {\n", name, pathBase, name)
		b.WriteString("\tid := ctx.GetParam(\"id\")\n")
		fmt.Fprintf(&b, "\t%sMu.Lock()\n\tdefer %sMu.Unlock()\n", nameLower, nameLower)
		fmt.Fprintf(&b, "\tfor i, item := range %sStore {\n\t\tif item.ID == id {\n\t\t\t%sStore = append(%sStore[:i], %sStore[i+1:]...)\n\t\t\tctx.Status(204)\n\t\t\treturn nil\n\t\t}\n\t}\n",
			nameLower, nameLower, nameLower, nameLower)
		writeNotFound(&b, nameLower)
	}

	// A named type rather than map[string]string: marshalling a one-key map
	// costs several times what the equivalent struct does, and these error
	// paths are the ones that fire under load. The name is per-resource
	// because every resource writes into the same handlers package, and a
	// shared `errorResponse` would be redeclared by the second one.
	fmt.Fprintf(&b, "type %sError struct {\n\tError string `json:\"error\"`\n}\n", nameLower)

	return writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("resource"),
		Imports:    imports,
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      force,
	})
}

// writeNotFound and writeBindFailure emit the two error tails shared by the
// generated handlers, so the per-resource error type name is spelled once.
func writeNotFound(b *strings.Builder, nameLower string) {
	fmt.Fprintf(b, "\tctx.Status(404)\n\treturn ctx.JSON(%sError{Error: \"not found\"})\n}\n\n", nameLower)
}

// writeBindFailure emits the error branch of a binding.Bind call.
//
// The two failures are worth separating: malformed JSON is the client sending
// something that is not a request at all (400), while a validate rule that
// fails is a well-formed request the server understood and refused (422). The
// binding package draws that line by returning *ValidationError only in the
// second case, and carries an RFC 9457 problem+json rendering for it.
//
// When no field has rules the 422 branch is omitted, so the generated project
// has no unreachable code and no unused errors import.
func writeBindFailure(b *strings.Builder, nameLower string, validated bool) {
	if validated {
		b.WriteString("\t\tvar ve *binding.ValidationError\n")
		b.WriteString("\t\tif errors.As(err, &ve) {\n")
		b.WriteString("\t\t\tctx.Status(422)\n")
		b.WriteString("\t\t\treturn ctx.JSON(ve.ToProblemJSON())\n")
		b.WriteString("\t\t}\n")
	}
	fmt.Fprintf(b, "\t\tctx.Status(400)\n\t\treturn ctx.JSON(%sError{Error: \"invalid body\"})\n\t}\n\n", nameLower)
}

// writeStruct emits a request struct. A field's validate tag is included when
// it has one â€” that tag is the entire mechanism binding.Bind validates through,
// so omitting it is what made `generate resource` advertise validation and
// produce none.
func writeStruct(b *strings.Builder, typeName string, fields []field) {
	fmt.Fprintf(b, "type %s struct {\n", typeName)
	for _, f := range fields {
		if f.Validate != "" {
			fmt.Fprintf(b, "\t%s %s `json:\"%s\" validate:\"%s\"`\n", f.Name, f.Type, f.JSON, f.Validate)
			continue
		}
		fmt.Fprintf(b, "\t%s %s `json:\"%s\"`\n", f.Name, f.Type, f.JSON)
	}
	b.WriteString("}\n\n")
}

func writeResponseStruct(b *strings.Builder, typeName string, fields []field) {
	fmt.Fprintf(b, "type %s struct {\n", typeName)
	b.WriteString("\tID string `json:\"id\"`\n")
	for _, f := range fields {
		fmt.Fprintf(b, "\t%s %s `json:\"%s\"`\n", f.Name, f.Type, f.JSON)
	}
	b.WriteString("}\n\n")
}

// routeDoc renders the middleware.DocXXX(...) call for a single action,
// wiring up scalar.RouteDoc from the generated request/response types.
func routeDoc(a action, name, plural, path string) string {
	tags := fmt.Sprintf("[]string{%q}", plural)
	switch a.Name {
	case "list":
		return fmt.Sprintf(`middleware.DocGET(%q, scalar.RouteDoc{
	Title:        %q,
	Tags:         %s,
	Output:       handlers.%sListResponse{},
	OutputStatus: 200,
})`, path, "List "+plural, tags, name)
	case "get":
		return fmt.Sprintf(`middleware.DocGET(%q, scalar.RouteDoc{
	Title: %q,
	Tags:  %s,
	Input: []scalar.InputGroup{
		{Type: scalar.InputParams, Fields: handlers.%sPathParams{}},
	},
	Output: handlers.%sResponse{},
})`, path, "Get "+name+" by ID", tags, name, name)
	case "create":
		return fmt.Sprintf(`middleware.DocPOST(%q, scalar.RouteDoc{
	Title: %q,
	Tags:  %s,
	Input: []scalar.InputGroup{
		{Type: scalar.InputBody, Fields: handlers.Create%sRequest{}, Required: true},
	},
	Output:       handlers.%sResponse{},
	OutputStatus: 201,
})`, path, "Create "+name, tags, name, name)
	case "update":
		return fmt.Sprintf(`middleware.DocPUT(%q, scalar.RouteDoc{
	Title: %q,
	Tags:  %s,
	Input: []scalar.InputGroup{
		{Type: scalar.InputParams, Fields: handlers.%sPathParams{}},
		{Type: scalar.InputBody, Fields: handlers.Update%sRequest{}, Required: true},
	},
	Output: handlers.%sResponse{},
})`, path, "Update "+name, tags, name, name, name)
	case "delete":
		return fmt.Sprintf(`middleware.DocDELETE(%q, scalar.RouteDoc{
	Title: %q,
	Tags:  %s,
	Input: []scalar.InputGroup{
		{Type: scalar.InputParams, Fields: handlers.%sPathParams{}},
	},
	Output:            struct{}{},
	OutputStatus:      204,
	OutputDescription: %q,
})`, path, "Delete "+name, tags, name, name+" deleted")
	}
	return ""
}
