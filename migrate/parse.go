package migrate

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// Migration represents a single migration file pair (.up.sql and .down.sql).
type Migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string
}

var migrationFileRe = regexp.MustCompile(`^(\d{4,})_([a-z0-9_]+)\.(up|down)\.sql$`)

// DiscoverMigrations discovers all migration files from an fs.FS and returns
// them sorted by version in ascending order. It returns an error if:
// - a migration has an .up.sql without a matching .down.sql (or vice versa)
// - two migrations share the same version number
func DiscoverMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to read migration directory: %w", err)
	}

	// Group files by version and name
	type fileGroup struct {
		version int
		name    string
		up      string
		down    string
	}
	groups := make(map[string]*fileGroup)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		match := migrationFileRe.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}

		versionStr := match[1]
		name := match[2]
		direction := match[3]

		version, err := strconv.Atoi(versionStr)
		if err != nil {
			return nil, fmt.Errorf("invalid version in %q: %w", entry.Name(), err)
		}

		key := fmt.Sprintf("%04d_%s", version, name)
		if groups[key] == nil {
			groups[key] = &fileGroup{version: version, name: name}
		}

		content, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("failed to read %q: %w", entry.Name(), err)
		}

		if direction == "up" {
			groups[key].up = string(content)
		} else {
			groups[key].down = string(content)
		}
	}

	// Validate that each migration has both up and down, and collect into slice
	migrations := make([]Migration, 0, len(groups))
	versionsSeen := make(map[int]string)

	for key, group := range groups {
		if group.up == "" {
			return nil, fmt.Errorf("migration %q has .down.sql but no .up.sql", key)
		}
		if group.down == "" {
			return nil, fmt.Errorf("migration %q has .up.sql but no .down.sql", key)
		}

		if existing, ok := versionsSeen[group.version]; ok {
			return nil, fmt.Errorf(
				"duplicate version %04d: %q and %q",
				group.version,
				existing,
				key,
			)
		}
		versionsSeen[group.version] = key

		migrations = append(migrations, Migration{
			Version: group.version,
			Name:    group.name,
			UpSQL:   group.up,
			DownSQL: group.down,
		})
	}

	// Sort by version ascending
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	return migrations, nil
}

// nextVersion and toSlug used to live here: a version allocator and a
// CamelCase→snake_case converter for naming a *new* migration pair. Neither was
// reachable — nothing in this package or any importer called them, only their own
// tests did.
//
// They were dead because this package does not create migration files. It
// discovers and runs the ones already on disk; `breeze makemigration` writes them,
// and that lives in internal/generator with its own toSlug. Two functions naming
// the same artefact would be a problem if both ran, and this one never did — its
// version was also the wrong one, inserting an underscore before every capital, so
// "HTTPServer" came out as "h_t_t_p_server" against the generator's "http_server".
//
// If this package ever needs to write a migration, the generator's toSlug is the
// implementation to lift out — it handles acronym runs and has the test cases to
// prove it.
