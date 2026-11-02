package postgres

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

var migrationFile = regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.(up|down)\.sql$`)

// TestMigrationNumbering guards against two branches claiming the same
// migration number: the registered versions must run 1..N with no gaps or
// duplicates, and migrations/ must hold exactly one up and one down file
// per version and nothing else.
func TestMigrationNumbering(t *testing.T) {
	registered := map[int64]bool{}
	for i, m := range migrations {
		if want := int64(i + 1); m.version != want {
			t.Errorf("migrations[%d] has version %d, want %d (versions must be sequential from 1 with no gaps or duplicates)", i, m.version, want)
		}
		if registered[m.version] {
			t.Errorf("version %d is registered twice", m.version)
		}
		registered[m.version] = true
		if m.sql == "" {
			t.Errorf("version %d has empty SQL", m.version)
		}
	}

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	type pair struct{ up, down string }
	files := map[int64]*pair{}
	for _, e := range entries {
		name := e.Name()
		match := migrationFile.FindStringSubmatch(name)
		if match == nil {
			t.Errorf("%s does not match NNNNNN_name.(up|down).sql", name)
			continue
		}
		v, _ := strconv.ParseInt(match[1], 10, 64)
		if !registered[v] {
			t.Errorf("%s is not registered in the migrations slice", name)
		}
		p := files[v]
		if p == nil {
			p = &pair{}
			files[v] = p
		}
		if match[2] == "up" {
			if p.up != "" {
				t.Errorf("version %d has two up files: %s and %s", v, p.up, name)
			}
			p.up = name
		} else {
			if p.down != "" {
				t.Errorf("version %d has two down files: %s and %s", v, p.down, name)
			}
			p.down = name
		}
	}

	for v := range registered {
		p := files[v]
		if p == nil || p.up == "" {
			t.Errorf("version %d has no up file", v)
		}
		if p == nil || p.down == "" {
			t.Errorf("version %d has no down file", v)
		}
	}
}
