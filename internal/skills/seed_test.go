package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/pkg/store"
)

// memSkillStore is a minimal in-memory store.SkillStore, just enough to
// exercise SeedSkills' create/no-op/update-on-change logic without a
// database.
type memSkillStore struct {
	mu       sync.Mutex
	byID     map[string]*store.Skill
	seq      int
	versions map[string][]store.SkillVersion
}

func newMemSkillStore() *memSkillStore {
	return &memSkillStore{byID: map[string]*store.Skill{}, versions: map[string][]store.SkillVersion{}}
}

func (s *memSkillStore) Create(_ context.Context, sk *store.Skill, files []store.SkillFile, createdBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.byID {
		if existing.TenantID == sk.TenantID && existing.Name == sk.Name {
			return store.ErrConflict
		}
	}
	s.seq++
	sk.ID = fmt.Sprintf("skill-%d", s.seq)
	sk.LatestVersion = 1
	cp := *sk
	s.byID[sk.ID] = &cp
	s.versions[sk.ID] = []store.SkillVersion{{SkillID: sk.ID, Version: 1, Files: append([]store.SkillFile(nil), files...), CreatedBy: createdBy}}
	return nil
}

func (s *memSkillStore) Get(_ context.Context, _, id string) (*store.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *sk
	return &cp, nil
}

func (s *memSkillStore) GetByName(_ context.Context, _, name string) (*store.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sk := range s.byID {
		if sk.Name == name {
			cp := *sk
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *memSkillStore) List(context.Context, string, store.SkillListOptions) ([]store.Skill, int, error) {
	return nil, 0, nil
}

func (s *memSkillStore) Update(_ context.Context, sk *store.Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[sk.ID]
	if !ok {
		return store.ErrNotFound
	}
	existing.Description = sk.Description
	existing.Enabled = sk.Enabled
	existing.Metadata = sk.Metadata
	existing.Arguments = sk.Arguments
	return nil
}

func (s *memSkillStore) SoftDelete(context.Context, string, string) error { return nil }

func (s *memSkillStore) AddVersion(_ context.Context, _, id string, files []store.SkillFile, createdBy string) (*store.SkillVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	skillMD, err := FindSkillMD(files)
	if err != nil {
		return nil, err
	}
	fm, err := ParseFrontmatter(skillMD, sk.Name)
	if err != nil {
		return nil, err
	}
	newVersion := sk.LatestVersion + 1
	v := store.SkillVersion{SkillID: id, Version: newVersion, Files: append([]store.SkillFile(nil), files...), CreatedBy: createdBy}
	s.versions[id] = append(s.versions[id], v)
	sk.LatestVersion = newVersion
	sk.Description = DescriptionFrom(fm)
	sk.Frontmatter = fm
	return &v, nil
}

func (s *memSkillStore) GetVersion(_ context.Context, skillID string, version int) (*store.SkillVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions[skillID] {
		if v.Version == version {
			cp := v
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *memSkillStore) ListVersions(_ context.Context, skillID string) ([]store.SkillVersion, error) {
	return s.versions[skillID], nil
}

var _ store.SkillStore = (*memSkillStore)(nil)

func writeSeedSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSeedSkills_EmptyDirDisablesSeed(t *testing.T) {
	s := newMemSkillStore()
	created, updated, err := SeedSkills(context.Background(), s, "", nil)
	if err != nil || created != 0 || updated != 0 {
		t.Fatalf("SeedSkills(\"\") = %d, %d, %v; want 0, 0, nil", created, updated, err)
	}
}

func TestSeedSkills_CreateThenNoop(t *testing.T) {
	root := t.TempDir()
	writeSeedSkill(t, root, "recon", "Reconnaissance helper.", "Body.")

	s := newMemSkillStore()
	created, updated, err := SeedSkills(context.Background(), s, root, nil)
	if err != nil {
		t.Fatalf("SeedSkills: %v", err)
	}
	if created != 1 || updated != 0 {
		t.Fatalf("first run: created=%d updated=%d, want 1, 0", created, updated)
	}
	sk, err := s.GetByName(context.Background(), "", "recon")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if sk.TenantID != "" || sk.Kind != "skill" || !sk.Enabled || sk.LatestVersion != 1 {
		t.Errorf("seeded skill = %+v", sk)
	}
	if sk.Description != "Reconnaissance helper." {
		t.Errorf("Description = %q", sk.Description)
	}

	// Re-running with unchanged content is a no-op.
	created, updated, err = SeedSkills(context.Background(), s, root, nil)
	if err != nil {
		t.Fatalf("SeedSkills (rerun): %v", err)
	}
	if created != 0 || updated != 0 {
		t.Errorf("rerun (unchanged): created=%d updated=%d, want 0, 0", created, updated)
	}
}

// racedSkillStore misses the skill on lookup, as a replica booting at the
// same moment as another one does, so Create then hits the unique name.
type racedSkillStore struct{ *memSkillStore }

func (s racedSkillStore) GetByName(context.Context, string, string) (*store.Skill, error) {
	return nil, store.ErrNotFound
}

func TestSeedSkills_ConcurrentCreateIsNotAnError(t *testing.T) {
	root := t.TempDir()
	writeSeedSkill(t, root, "recon", "Reconnaissance helper.", "Body.")
	s := newMemSkillStore()
	if _, _, err := SeedSkills(context.Background(), s, root, nil); err != nil {
		t.Fatalf("first replica: %v", err)
	}
	created, _, err := SeedSkills(context.Background(), racedSkillStore{s}, root, nil)
	if err != nil || created != 0 {
		t.Fatalf("second replica = created %d, err %v; want 0, nil", created, err)
	}
}

func TestSeedSkills_UpdatesOnContentChange(t *testing.T) {
	root := t.TempDir()
	writeSeedSkill(t, root, "recon", "Reconnaissance helper.", "Body.")

	s := newMemSkillStore()
	if _, _, err := SeedSkills(context.Background(), s, root, nil); err != nil {
		t.Fatalf("SeedSkills: %v", err)
	}

	// Change the SKILL.md content: a new version should be added and the
	// description refreshed.
	writeSeedSkill(t, root, "recon", "Reconnaissance helper, v2.", "Updated body.")
	created, updated, err := SeedSkills(context.Background(), s, root, nil)
	if err != nil {
		t.Fatalf("SeedSkills (changed): %v", err)
	}
	if created != 0 || updated != 1 {
		t.Fatalf("changed run: created=%d updated=%d, want 0, 1", created, updated)
	}
	sk, err := s.GetByName(context.Background(), "", "recon")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if sk.LatestVersion != 2 || sk.Description != "Reconnaissance helper, v2." {
		t.Errorf("after update, seeded skill = %+v", sk)
	}
}

func TestSeedSkills_SkipsNonBundleDirs(t *testing.T) {
	root := t.TempDir()
	writeSeedSkill(t, root, "recon", "A helper.", "Body.")
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-skill", "readme.md"), []byte("just notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newMemSkillStore()
	created, _, err := SeedSkills(context.Background(), s, root, nil)
	if err != nil {
		t.Fatalf("SeedSkills: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1 (only the real bundle)", created)
	}
	if _, err := s.GetByName(context.Background(), "", "not-a-skill"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the non-bundle directory must not have been seeded")
	}
}

func TestSeedSkills_CommandArguments(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "scan-host")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" +
		"name: scan-host\n" +
		"description: Scan a host.\n" +
		"metadata:\n" +
		"  kind: command\n" +
		"  arguments:\n" +
		"    - name: target\n" +
		"      description: the host\n" +
		"      required: true\n" +
		"---\n" +
		"scan {{target}}\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newMemSkillStore()
	created, _, err := SeedSkills(context.Background(), s, root, nil)
	if err != nil {
		t.Fatalf("SeedSkills: %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	sk, err := s.GetByName(context.Background(), "", "scan-host")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if sk.Kind != "command" || len(sk.Arguments) != 1 || sk.Arguments[0].Name != "target" || !sk.Arguments[0].Required {
		t.Errorf("seeded command = %+v", sk)
	}
}

func TestSeedSkills_InvalidBundleErrors(t *testing.T) {
	root := t.TempDir()
	// Frontmatter name doesn't match the directory name.
	writeSeedSkill(t, root, "recon", "x", "y")
	dir := filepath.Join(root, "recon")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: wrong\ndescription: x\n---\ny\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newMemSkillStore()
	if _, _, err := SeedSkills(context.Background(), s, root, nil); err == nil {
		t.Error("SeedSkills(mismatched frontmatter name) error = nil, want an error")
	}
}
