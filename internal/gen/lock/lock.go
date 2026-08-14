// Package lock records what generated code was generated from.
//
// The problem it solves is that generated code and the mapping it came from
// drift apart silently. Somebody adds a column, changes a type, drops a unique
// constraint a relation depended on — and the committed .gen.go files still
// compile, still run, and are now describing a database that no longer exists.
// The compiler cannot notice, because nothing about the change is a Go type
// error.
//
// So generation writes down a fingerprint of the mapping it proved, and check
// compares the mapping it proves now against it. That reduces the question to
// one comparison of two hex strings, which is cheap enough to run in CI on
// every commit.
//
// What is fingerprinted is the mapping, not the catalog. A column nobody mapped
// can be added and dropped all day without invalidating anything, because it
// contributes nothing to generated output. What does invalidate: a mapped
// column's type, its nullability, whether the database supplies its value, the
// primary key, the foreign key and uniqueness a relation was proved from, an
// enum's labels, and the relations themselves.
package lock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// Name is the file generation writes beside the configuration.
const Name = "orm.lock"

// Version is the lock format's own version.
//
// It is separate from any tool version on purpose. A CLI release that changes
// nothing about what is generated must not make every project's committed code
// report as stale; only a change to what the fingerprint covers does that, and
// that change is this number.
const Version = 1

// File is the lock's contents.
type File struct {
	// Version is the format's version, not the tool's.
	Version int `json:"version"`
	// Mapping is the fingerprint of the reconciled mapping the generated code
	// was produced from.
	Mapping string `json:"mapping_sha256"`
}

// Fingerprint reduces a proved mapping to a hex digest.
//
// The digest is over a canonical rendering rather than over the mapping's
// memory: no map is ranged over, every list is written in an order the mapping
// already fixes or that is sorted here, and nothing machine-specific — a path,
// a host, an OID, a timestamp — appears at all. Two checkouts of one commit
// reconciled against one schema produce the same digest on any machine.
func Fingerprint(m *model.Mapping) string {
	sum := sha256.Sum256([]byte(canonical(m)))
	return hex.EncodeToString(sum[:])
}

// canonical renders the semantic content of a mapping as text.
//
// Text rather than a struct hashed field by field, because the rendering is the
// specification: what a reader can see in it is exactly what invalidates
// generated code, and anything absent from it deliberately does not.
func canonical(m *model.Mapping) string {
	var b strings.Builder
	fmt.Fprintf(&b, "orm-mapping/%d\n", Version)
	if m == nil {
		return b.String()
	}

	entities := slices.Clone(m.Entities)
	// Entity order is the scanner's, which follows the configured package order
	// and then declaration order. Sorting anyway means a package reordered in
	// the configuration does not read as a change to the mapping.
	slices.SortFunc(entities, func(a, c *model.EntityMapping) int {
		return strings.Compare(entityKey(a), entityKey(c))
	})
	for _, em := range entities {
		writeEntity(&b, em)
	}
	return b.String()
}

func entityKey(em *model.EntityMapping) string {
	return em.Entity.PkgPath + "." + em.Entity.Name
}

func writeEntity(b *strings.Builder, em *model.EntityMapping) {
	fmt.Fprintf(b, "entity %s table %s.%s\n", entityKey(em), em.Table.Schema, em.Table.Name)

	// The primary key is what a relation to this entity uses to tell an absent
	// row from a present one, so losing or reordering it changes generated code.
	fmt.Fprintf(b, "  pk %s\n", strings.Join(columnNames(em.Table.PK), ","))

	// Column order is the entity's field order, which is the contract between
	// the select list and the scanner. It is not sorted: reordering fields
	// really does change generated code.
	for _, cm := range em.Cols {
		writeColumn(b, cm)
	}
	for _, rel := range em.Rels {
		writeRelation(b, rel)
	}
	writeConcurrentRefresh(b, em)
}

// writeConcurrentRefresh records the index fact a materialized view's generated
// code is built from.
//
// The generated constructor carries the name of the unique index that lets
// REFRESH CONCURRENTLY run, so that the runtime can refuse before sending a
// statement the server would reject. That name is read out of the schema at
// generation time, which makes it an input to generation — and an input the
// fingerprint did not cover.
//
// The consequence was a false statement about the database. Adding a
// qualifying index through a migration left the generated code saying there was
// none, orm check --generated reported "Generated current" because the mapping
// had not moved, and Refresh(Concurrently) refused a refresh PostgreSQL would
// have accepted. Nothing told the developer to regenerate, because nothing
// knew.
//
// Only materialized views are written here. A table's generated code does not
// depend on its indexes, so including them would move every existing lock
// without anything having changed about what was generated.
func writeConcurrentRefresh(b *strings.Builder, em *model.EntityMapping) {
	if em.Entity == nil || em.Entity.Kind != model.RelMaterializedView {
		return
	}
	fmt.Fprintf(b, "  concurrent-refresh %s\n", em.ConcurrentRefreshIndex())
}

func writeColumn(b *strings.Builder, cm model.ColMapping) {
	col := cm.Column
	fmt.Fprintf(b, "  col %s go %s pg %s", col.Name, cm.Field.Type.Src, typeName(col.Type))
	// Each of these decides something a caller can see: whether the column can
	// hold NULL, whether an insert may leave it out, and whether it may be
	// written at all.
	if !col.Nullable() {
		b.WriteString(" notnull")
	}
	if col.HasDefault {
		b.WriteString(" default")
	}
	if col.Identity != 0 {
		b.WriteString(" identity")
	}
	if col.Generated != 0 {
		b.WriteString(" generated")
	}
	b.WriteByte('\n')
	writeTypeDetail(b, col.Type)
}

// writeTypeDetail records what a type is made of, for the kinds whose contents
// reach generated code.
func writeTypeDetail(b *strings.Builder, t *model.PGType) {
	for ; t != nil; t = t.Elem {
		switch t.Kind {
		case model.PGEnum:
			// The labels are a value a caller writes, so gaining or losing one
			// changes what the generated constants must be.
			fmt.Fprintf(b, "    enum %s [%s]\n", typeName(t), strings.Join(t.Labels, " "))
		case model.PGDomain:
			fmt.Fprintf(b, "    domain %s over %s notnull=%t\n", typeName(t), typeName(t.Elem), t.DomainNotNull)
		case model.PGArray:
			fmt.Fprintf(b, "    array %s of %s\n", typeName(t), typeName(t.Elem))
		default:
			return
		}
	}
}

func writeRelation(b *strings.Builder, rel model.RelMapping) {
	fmt.Fprintf(b, "  rel %s %s -> %s.%s via %s side %d\n",
		rel.Field.Name, rel.Cardinality, rel.Target.Entity.PkgPath, rel.Target.Entity.Name,
		rel.FK.Name, rel.FKSide)
	// The key pairing is what the loader matches on, in the order the
	// constraint declares it. Order is meaning here, so it is never sorted.
	for i := range rel.KeyCols {
		fmt.Fprintf(b, "    key %s -> %s mapped=%t\n",
			rel.KeyCols[i].Column.Name, rel.TargetCols[i].Column.Name, rel.KeyCols[i].Mapped())
	}
}

func columnNames(cols []*model.PGColumn) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}

// typeName is a type's schema-qualified name. The OID is deliberately absent:
// it differs between two databases holding the same schema, and generated code
// never contains one.
func typeName(t *model.PGType) string {
	if t == nil {
		return "?"
	}
	return t.Schema + "." + t.Name
}

// Path is where the lock lives for a given configuration file.
func Path(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), Name)
}

// Read loads a lock file. A missing file is reported as such rather than as an
// error, because the first run of a project has not written one yet.
func Read(path string) (*File, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	return &f, true, nil
}

// Write records a fingerprint, replacing whatever was there.
//
// It lands through a temporary file and a rename, like the generated code it
// describes, so the lock is never a half-written file and never describes a
// generation that did not finish.
func Write(path, fingerprint string) error {
	data, err := json.MarshalIndent(File{Version: Version, Mapping: fingerprint}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+Name+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// State is what a check found out about generated code.
type State int

const (
	// Missing means no lock file, which is what a project looks like before it
	// has ever generated. It is not a failure on its own.
	Missing State = iota
	// Current means the generated code matches the mapping.
	Current
	// Stale means it does not.
	Stale
	// Unknown means the lock exists but was written by a format this tool does
	// not understand.
	Unknown
)

// Compare reports how a lock file relates to a fingerprint.
func Compare(f *File, present bool, fingerprint string) State {
	switch {
	case !present:
		return Missing
	case f.Version != Version:
		return Unknown
	case f.Mapping == fingerprint:
		return Current
	default:
		return Stale
	}
}
