package generator

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generateModel writes a struct into models/ and the paired SQL migration.
//
// The two are generated together because a model without a table is not useful
// and keeping them in step by hand is exactly the sort of thing that drifts.
// Column types come from the same field table the struct does, so the mapping is
// stated in one place.
func generateModel(modulePath, name string, args []string) error {
	fs := flag.NewFlagSet("generate model", flag.ContinueOnError)
	tableOverride := fs.String("table", "", "table name (default the pluralized snake_case name)")
	pluralOverride := fs.String("plural", "", "override the pluralized name (e.g. --plural=people)")
	migrationsDir := fs.String("dir", "migrations", "directory holding migration files")
	skipMigration := fs.Bool("no-migration", false, "write only the struct, no SQL migration")
	timestamps := fs.Bool("timestamps", true, "include created_at / updated_at columns")
	force := fs.Bool("force", false, "overwrite an existing model file")
	out := registerOutputFlags(fs)

	flagArgs, positional := splitFlagsAndPositional(fs, args)
	if err := parseFlags(fs, flagArgs); err != nil {
		return err
	}

	fields, err := parseFieldsNoRules("model", positional)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return fmt.Errorf("usage: breeze generate model %s field:type [field:type ...]", name)
	}
	// --dir names the migrations directory, which is created and written to. It is
	// checked here rather than at the MkdirAll below so a traversal attempt fails
	// before the model file is written — a refusal that leaves half the pair behind
	// is worse than one that leaves nothing.
	if err := validatePathFlag("dir", *migrationsDir); err != nil {
		return err
	}

	plural := *pluralOverride
	if plural == "" {
		plural = pluralize(name)
	}
	table := *tableOverride
	if table == "" {
		table = toSlug(plural)
	}

	// Resolved before anything is written, so a bad --package or a filename that
	// belongs to another feature fails before the migration pair is created.
	target, err := out.target("models", fileSlug(name))
	if err != nil {
		return err
	}

	// ── the struct ─────────────────────────────────────────────────────────────
	var imports []string
	// time is needed either by a time-typed field or by the timestamp columns.
	// It is listed conditionally here and pruned again by the writer, so the two
	// agree even if this condition is ever wrong.
	if usesTime(fields) || *timestamps {
		imports = append(imports, timeImport)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "// %s maps to the %q table.\n", name, table)
	fmt.Fprintf(&b, "type %s struct {\n", name)
	b.WriteString("\tID int64 `json:\"id\" db:\"id\"`\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "\t%s %s `json:\"%s\" db:\"%s\"`\n", f.Name, f.Type, f.JSON, columnName(f))
	}
	if *timestamps {
		b.WriteString("\tCreatedAt time.Time `json:\"created_at\" db:\"created_at\"`\n")
		b.WriteString("\tUpdatedAt time.Time `json:\"updated_at\" db:\"updated_at\"`\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// %sTable is the table name, so a query never hardcodes the string.\n", name)
	fmt.Fprintf(&b, "const %sTable = %q\n\n", name, table)

	// Column list as a const: every SELECT and INSERT needs it in the same
	// order, and building it per query is how column/value mismatches happen.
	cols := []string{"id"}
	for _, f := range fields {
		cols = append(cols, columnName(f))
	}
	if *timestamps {
		cols = append(cols, "created_at", "updated_at")
	}
	fmt.Fprintf(
		&b,
		"// %sColumns lists the columns in the order %s's fields are declared.\n",
		name,
		name,
	)
	fmt.Fprintf(&b, "var %sColumns = []string{\n", name)
	for _, c := range cols {
		fmt.Fprintf(&b, "\t%q,\n", c)
	}
	b.WriteString("}\n\n")

	// Scan target helper: the argument order that matches Columns, so a caller
	// cannot get the two out of step.
	fmt.Fprintf(&b, "// ScanDest returns pointers to every field in %sColumns order, for\n", name)
	b.WriteString("// use with sql.Row.Scan:\n//\n")
	fmt.Fprintf(&b, "//\tvar m models.%s\n", name)
	fmt.Fprintf(&b, "//\terr := row.Scan(m.ScanDest()...)\n")
	fmt.Fprintf(&b, "func (m *%s) ScanDest() []any {\n", name)
	b.WriteString("\treturn []any{\n")
	b.WriteString("\t\t&m.ID,\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "\t\t&m.%s,\n", f.Name)
	}
	if *timestamps {
		b.WriteString("\t\t&m.CreatedAt,\n")
		b.WriteString("\t\t&m.UpdatedAt,\n")
	}
	b.WriteString("\t}\n}\n")

	if err := writeGeneratedGoFile(generatedFile{
		Target:     target,
		Owner:      generateOwner("model"),
		Imports:    imports,
		Body:       b.String(),
		ModulePath: modulePath,
		Force:      *force,
	}); err != nil {
		return err
	}

	notes := []string{fmt.Sprintf("Import as:    \"%s/models\"", modulePath)}

	// ── the migration ──────────────────────────────────────────────────────────
	if !*skipMigration {
		// nextMigrationVersion reads the directory, so it has to exist first â€”
		// on a fresh project it does not.
		if err := os.MkdirAll(*migrationsDir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", *migrationsDir, err)
		}
		version, err := nextMigrationVersion(*migrationsDir)
		if err != nil {
			return err
		}
		slug := fmt.Sprintf("%04d_create_%s", version, table)

		var up strings.Builder
		fmt.Fprintf(&up, "-- Creates the %s table for models.%s.\n", table, name)
		fmt.Fprintf(&up, "CREATE TABLE %s (\n", table)
		// A portable auto-increment does not exist: SERIAL is Postgres, AUTOINCREMENT
		// is SQLite, AUTO_INCREMENT is MySQL. BIGINT plus a note is the honest
		// default rather than silently picking one engine.
		up.WriteString("\t-- Adjust to your engine: SERIAL (Postgres), INTEGER PRIMARY KEY\n")
		up.WriteString("\t-- AUTOINCREMENT (SQLite), BIGINT AUTO_INCREMENT (MySQL).\n")
		up.WriteString("\tid BIGINT PRIMARY KEY,\n")
		for _, f := range fields {
			fmt.Fprintf(&up, "\t%s %s NOT NULL,\n", columnName(f), sqlType(f))
		}
		if *timestamps {
			up.WriteString("\tcreated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,\n")
			up.WriteString("\tupdated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,\n")
		}
		// Trim the trailing comma from the last column.
		sql := strings.TrimSuffix(strings.TrimRight(up.String(), "\n"), ",")
		sql += "\n);\n"

		down := fmt.Sprintf("-- Reverses %s.up.sql.\nDROP TABLE IF EXISTS %s;\n", slug, table)

		upPath := filepath.Join(*migrationsDir, slug+".up.sql")
		downPath := filepath.Join(*migrationsDir, slug+".down.sql")
		if err := writeGeneratedTextFile(upPath, sql, *force); err != nil {
			return err
		}
		if err := writeGeneratedTextFile(downPath, down, *force); err != nil {
			return err
		}

		notes = append(notes,
			"Review the id column â€” the portable type is a placeholder, not your engine's.",
			"Apply it:     breeze migrate up",
		)
		if !migratorPresent() {
			notes = append(
				notes,
				"Run `breeze add migrator --driver=postgres` â€” migrate needs a runner with your SQL driver.",
			)
		}
	}

	printNotes(notes)
	return nil
}
