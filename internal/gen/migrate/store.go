package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The migrations directory.
//
// A migration's identity is its ID, and the file name is that ID plus an
// extension — not a second, independent name. Anything else invites the failure
// where a file is renamed, the ID stays, and two machines disagree about which
// migration is which; so a file whose name and ID differ is refused rather than
// interpreted.

// FileExtension is the file extension of a migration artifact.
const FileExtension = ".json"

// Store is a directory of migration files.
type Store struct{ dir string }

// NewStore names a migrations directory. Nothing is read until it is asked for,
// so a project that has never written a migration can still be described.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir returns the directory the store reads and writes.
func (s *Store) Dir() string { return s.dir }

// Files returns the migration files present, in file-name order.
//
// A missing directory is not an error: a project that has not written its first
// migration is a project mid-setup, and the answer to "which migrations are
// there" is "none".
func (s *Store) Files() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the migrations directory %s: %w", s.dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), FileExtension) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out, nil
}

// Load reads every migration in the directory and validates them as a set.
//
// An empty directory produces an empty set rather than an error, so the first
// makemigrations of a project takes the same path as the hundredth.
func (s *Store) Load() (*Set, error) {
	names, err := s.Files()
	if err != nil {
		return nil, err
	}
	migrations := make([]*Migration, 0, len(names))
	for _, name := range names {
		path := filepath.Join(s.dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		m, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := validID(m.ID); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if want := strings.TrimSuffix(name, FileExtension); m.ID != want {
			return nil, fmt.Errorf("%s declares the ID %q; a migration's file name is its ID, and a file that says otherwise"+
				" means two machines would disagree about which migration is which", name, m.ID)
		}
		migrations = append(migrations, m)
	}
	return NewSet(migrations)
}

// Path returns the file a migration ID lives in.
func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+FileExtension) }

// validID refuses an ID that would not survive being a file name.
//
// The ID is the file name, so an ID that is not one is a migration that either
// lands somewhere other than the migrations directory or lands where the loader
// will not look — and a migration written and then never seen is a hole in a
// history nobody notices until a fresh database is built from it.
func validID(id string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return errors.New("a migration ID cannot be empty")
	case id != filepath.Base(id) || strings.ContainsRune(id, '/') || strings.ContainsRune(id, filepath.Separator):
		return fmt.Errorf("migration ID %q contains a path separator, and an ID is a name rather than a location", id)
	case strings.HasPrefix(id, "."):
		return fmt.Errorf("migration ID %q begins with a dot, which would hide the file from the loader", id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("migration ID %q contains a NUL byte", id)
	case strings.HasSuffix(id, FileExtension):
		return fmt.Errorf("migration ID %q already ends in %s; the extension is added, not written", id, FileExtension)
	}
	return nil
}

// Write renders a migration and writes it to its file.
//
// Everything that can fail — rendering, checksumming, encoding — happens before
// the file exists, and the bytes land through a temporary file and a rename.
// A migration is a permanent record; a half-written one would be a permanent
// record of nothing, and one that failed to checksum would be discovered by the
// next command rather than by this one.
func (s *Store) Write(m *Migration) (string, error) {
	if err := validID(m.ID); err != nil {
		return "", err
	}
	data, err := Render(m)
	if err != nil {
		return "", err
	}
	// Rendering and parsing back proves the artifact says what the migration
	// meant before anything is committed to disk.
	back, err := Parse(data)
	if err != nil {
		return "", fmt.Errorf("migration %s does not survive being written down: %w", m.ID, err)
	}
	want, err := m.Checksum()
	if err != nil {
		return "", err
	}
	got, err := back.Checksum()
	if err != nil {
		return "", err
	}
	if want != got {
		return "", fmt.Errorf("migration %s changes meaning when written down (%s -> %s);"+
			" this is a bug in the artifact format", m.ID, short(want), short(got))
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", fmt.Errorf("creating the migrations directory %s: %w", s.dir, err)
	}
	path := s.Path(m.ID)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists; a migration is never rewritten in place", filepath.Base(path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(s.dir, "."+m.ID+".*")
	if err != nil {
		return "", fmt.Errorf("creating a temporary file in %s: %w", s.dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return "", fmt.Errorf("setting the mode of %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return "", fmt.Errorf("placing %s: %w", path, err)
	}
	return path, nil
}

// NextID builds the ID the next migration in a set should have.
//
// It is a sequence number and a name, and nothing else. A timestamp would make
// two developers on one branch produce two migrations that both claim to be
// next and sort by whose clock was ahead; the sequence makes the collision
// visible instead — two migrations numbered 0004 are obviously the same
// position in history, which is a conversation rather than a silent merge.
func NextID(set *Set, name string) string {
	n := 1
	if set != nil {
		for _, m := range set.Migrations() {
			if seq, ok := sequenceOf(m.ID); ok && seq >= n {
				n = seq + 1
			}
		}
	}
	name = sanitizeName(name)
	if name == "" {
		name = "auto"
	}
	return fmt.Sprintf("%04d_%s", n, name)
}

// sequenceOf reads the leading number of an ID such as 0004_add_status.
func sequenceOf(id string) (int, bool) {
	digits := 0
	for digits < len(id) && id[digits] >= '0' && id[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(id[:digits])
	if err != nil {
		return 0, false
	}
	return n, true
}
