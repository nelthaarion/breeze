package mcp

// project_source.go — reading a project's Go source.
//
// explain_project reconstructs a configuration from the marker blocks, which is
// the right source for features: a feature *is* a block, so the block is the
// fact. Models and routes are not blocks. `breeze generate model` writes a whole
// file under models/, and nothing records afterwards that it was asked for — so a
// configuration rebuilt from blocks has an empty Models list no matter how many
// models the project contains.
//
// That matters because the advice worth giving is mostly about models: a model
// with no route, a route naming a model that is not there. Reading those from the
// reconstructed configuration would mean every such rule silently never fired,
// and a tool that reports "nothing to suggest" because it looked in the wrong
// place is worse than one that does not exist.
//
// So this file reads the source. Parsing is the only option available — the
// models package cannot be imported, because it belongs to a different module
// that is not built here — and it is the same approach the generator's own
// `breeze routes` takes over the route registry.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nelthaarion/breeze/v2/internal/generator"
)

// modelsDirName is where `breeze generate model` writes. It is a literal in the
// generator too (filepath.Join("models", ...)), and there is nothing exported to
// share, so it is repeated here rather than guessed at each call site.
const modelsDirName = "models"

// sourceModel is a model as it exists on disk.
type sourceModel struct {
	Name string `json:"name"`
	// Table is read from the generated `const <Name>Table = "..."`, which is the
	// generator's own record of the table name. Deriving it by pluralising the
	// struct name again would be a second implementation of that rule, and the
	// two would disagree for every model generated with --table.
	Table  string        `json:"table,omitempty"`
	File   string        `json:"file"`
	Fields []sourceField `json:"fields"`
}

type sourceField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Column     string `json:"column,omitempty"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
}

// readSourceModels parses every model in a project's models directory.
//
// It returns what it could read plus a note for every file it could not, rather
// than failing: a project with one unparseable file still has models worth
// describing, and a caller told "the models could not be read" cannot tell
// whether that means none exist.
func readSourceModels(root string) ([]sourceModel, []string) {
	dir := filepath.Join(root, modelsDirName)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// A project with no models directory has no models. That is an ordinary
		// state, not a fault, and it is reported as such.
		return nil, nil
	}

	var models []sourceModel
	var notes []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			notes = append(
				notes,
				filepath.Join(
					modelsDirName,
					name,
				)+" could not be parsed, so its models are not reported: "+err.Error(),
			)
			continue
		}

		tables := modelTableConstants(file)

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structType, ok := ts.Type.(*ast.StructType)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				models = append(models, sourceModel{
					Name:   ts.Name.Name,
					Table:  tables[ts.Name.Name],
					File:   filepath.Join(modelsDirName, name),
					Fields: structFields(structType),
				})
			}
		}
	}

	sort.SliceStable(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, notes
}

// modelTableConstants collects `const XTable = "x"` declarations, keyed by the X.
func modelTableConstants(file *ast.File) map[string]string {
	out := map[string]string{}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := strings.TrimSuffix(vs.Names[0].Name, "Table")
			if name == vs.Names[0].Name {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if table, err := strconv.Unquote(lit.Value); err == nil {
				out[name] = table
			}
		}
	}
	return out
}

func structFields(st *ast.StructType) []sourceField {
	var out []sourceField

	for _, field := range st.Fields.List {
		typeName := types.ExprString(field.Type)
		column, hasColumn := dbTag(field.Tag)

		// An embedded field has no name. Its type is the field for reporting
		// purposes, which is what makes an embedded relationship visible.
		if len(field.Names) == 0 {
			out = append(out, sourceField{Name: typeName, Type: typeName, Column: column})
			continue
		}

		for _, ident := range field.Names {
			if !ident.IsExported() {
				continue
			}
			out = append(out, sourceField{
				Name:   ident.Name,
				Type:   typeName,
				Column: column,
				// The generator always emits ID as the primary key and names its
				// column id. Treating either as the marker keeps this right for
				// a hand-edited model that renamed one of them.
				PrimaryKey: ident.Name == "ID" || (hasColumn && column == "id"),
			})
		}
	}
	return out
}

// dbTag reads the db:"..." tag a generated model carries on every field.
func dbTag(tag *ast.BasicLit) (string, bool) {
	if tag == nil {
		return "", false
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return "", false
	}
	value, ok := reflectStructTagLookup(raw, "db")
	if !ok {
		return "", false
	}
	// A db:"col,omitempty" style option is not something the generator emits,
	// but a hand-edited model may carry one and the column is still the first
	// part.
	if idx := strings.IndexByte(value, ','); idx >= 0 {
		value = value[:idx]
	}
	return value, value != ""
}

// reflectStructTagLookup is reflect.StructTag.Get without importing reflect.
//
// The framework's own convention is that reflection has no place in code that
// runs per request, and while this is a build-time tool rather than a handler,
// pulling reflect in for one string lookup would put the import in a package
// whose rule set includes "no reflect in handler code" — and a checker that
// imports the thing it warns about invites the argument that the rule is
// negotiable. The lookup itself is four lines.
func reflectStructTagLookup(tag, key string) (string, bool) {
	for tag != "" {
		// Skip leading space.
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		// Scan to colon.
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]

		// Scan quoted string.
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]

		if name == key {
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

// asModelConfigs converts parsed models into the generator's own type.
//
// This exists so that relationship detection stays the generator's answer:
// ModelConfig.ModelRefs is what decides which model has to be emitted before
// which, and reusing it means "is this field a relationship" has one definition.
// A second rule here — "the type starts with a capital", say — would disagree the
// first time a model had a field of some other declared type.
func asModelConfigs(models []sourceModel) []generator.ModelConfig {
	out := make([]generator.ModelConfig, 0, len(models))
	for _, m := range models {
		cfg := generator.ModelConfig{Name: m.Name, Table: m.Table}
		for _, f := range m.Fields {
			cfg.Fields = append(cfg.Fields, generator.FieldConfig{
				Name:       f.Name,
				Type:       f.Type,
				Column:     f.Column,
				PrimaryKey: f.PrimaryKey,
			})
		}
		out = append(out, cfg)
	}
	return out
}

// modelNameSet is the lookup ModelRefs expects.
func modelNameSet(models []generator.ModelConfig) map[string]bool {
	names := make(map[string]bool, len(models))
	for _, m := range models {
		names[m.Name] = true
	}
	return names
}

// baseTypeName strips the decorations a relationship field can carry, so that
// []*Customer, *Customer and Customer all resolve to Customer.
func baseTypeName(typeName string) string {
	typeName = strings.TrimPrefix(typeName, "[]")
	typeName = strings.TrimPrefix(typeName, "*")
	if idx := strings.LastIndexByte(typeName, '.'); idx >= 0 {
		typeName = typeName[idx+1:]
	}
	return typeName
}

// routeServesModel reports whether any route looks like it serves a model.
//
// This is a heuristic and is reported as one. The registry records a path, a
// handler and the marker block a route was written into; none of those names the
// model, because a route and a model are generated by separate commands and
// nothing links them afterwards. What is available is that `breeze generate
// resource Order` writes its routes into a block named for the resource and
// mounts them on a path derived from the same word, so the model's name appears
// in one or the other in every case the generator produces.
//
// The failure mode is therefore a false "no routes" for a model served by a
// hand-written handler under an unrelated path. That is the right direction to
// fail in: the finding is a warning that names what it looked at, and a caller
// can see it is wrong. The opposite bias — assuming a model is served — would
// suppress the finding that matters.
func routeServesModel(routes []generator.RouteEntry, modelName string) bool {
	needle := strings.ToLower(modelName)
	if needle == "" {
		return false
	}

	for _, r := range routes {
		if strings.Contains(strings.ToLower(r.Block), needle) {
			return true
		}
		if strings.Contains(strings.ToLower(r.Path), needle) {
			return true
		}
		if strings.Contains(strings.ToLower(r.Handler), needle) {
			return true
		}
	}
	return false
}
