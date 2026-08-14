package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// The on-disk artifact.
//
// A migration has to survive being written down, read back on another machine
// and compared with what was applied months ago. That rules out anything whose
// meaning depends on the program that wrote it: the artifact is data, it names
// its own format version, and every operation it can hold is one this package
// can also apply to an in-memory state.
//
// It is JSON because JSON is the format that round-trips without a parser of
// our own and diffs legibly in review, which is where a migration is actually
// read. The checksum is computed over the operations rather than the bytes, so
// reindenting a file does not make an applied migration look modified — and
// changing an argument always does.
//
// One operation deliberately cannot be written down: a data migration written
// as a Go function. A function has no representation here, and inventing one
// that named a symbol this binary would have to resolve would be a promise the
// command-line tool cannot keep. Those migrations belong to a program that
// links the engine and passes its own Set.

// artifact is the document a migration file holds.
type artifact struct {
	Format     int      `json:"format"`
	ID         string   `json:"id"`
	DependsOn  []string `json:"dependsOn,omitempty"`
	Atomic     bool     `json:"atomic"`
	Operations []opJSON `json:"operations"`
}

// opJSON is one operation, tagged with the kind that decides how to read it.
type opJSON struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Render writes a migration as the bytes of its file.
//
// The output is deterministic: the same migration renders to the same bytes on
// every machine, which is what lets a repository check that a regenerated
// migration is the one already committed.
func Render(m *Migration) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	a := artifact{Format: m.format(), ID: m.ID, DependsOn: slices.Clone(m.DependsOn), Atomic: m.Atomic}
	for i, op := range m.Operations {
		enc, err := marshalOp(op)
		if err != nil {
			return nil, fmt.Errorf("migration %s, operation %d (%s): %w", m.ID, i+1, op.Describe(), err)
		}
		a.Operations = append(a.Operations, enc)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Schema expressions are SQL and routinely contain <, > and &. HTML
	// escaping would rewrite them into entities that PostgreSQL does not accept.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return nil, fmt.Errorf("rendering migration %s: %w", m.ID, err)
	}
	return buf.Bytes(), nil
}

// Parse reads a migration from the bytes of its file.
func Parse(data []byte) (*Migration, error) {
	var a artifact
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("reading a migration: %w", err)
	}
	m := &Migration{ID: a.ID, DependsOn: a.DependsOn, Atomic: a.Atomic, Format: a.Format}
	// The format is checked before the operations are read, so an artifact from
	// a newer tool reports that rather than reporting an operation kind this
	// build happens not to know.
	if v := m.format(); v > FormatVersion {
		return nil, &ErrUnsupportedFormat{ID: a.ID, Format: v, Supported: FormatVersion}
	}
	for i, enc := range a.Operations {
		op, err := unmarshalOp(enc)
		if err != nil {
			return nil, fmt.Errorf("migration %s, operation %d: %w", a.ID, i+1, err)
		}
		// An operation built in this process comes from the diff and its
		// identifiers came from a catalog or a declaration the scanner checked.
		// One read from a file did not: the file may have been hand-edited, and
		// an identifier it cannot render has to be refused here rather than
		// discovered as a panic when somebody runs the migration.
		if err := renderable(op); err != nil {
			return nil, fmt.Errorf("migration %s, operation %d (%s): %w", a.ID, i+1, op.Describe(), err)
		}
		m.Operations = append(m.Operations, op)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// renderable reports whether an operation can produce its SQL.
//
// SQL rendering quotes identifiers, and an identifier it cannot quote — empty,
// or carrying a NUL — is a bug when the operation was built in this process and
// bad input when it was read from a file. The renderer treats it as the former
// and panics; this converts it back into the latter, at the boundary where
// untrusted bytes become operations.
func renderable(op Operation) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("this operation cannot be turned into SQL: %v", p)
		}
	}()
	_, err = op.SQL()
	return err
}

// opKinds names every operation the artifact format can hold.
//
// The name is part of the format: renaming one would silently invalidate every
// migration already committed, so these strings are as much public API as the
// types they stand for.
const (
	kindCreateView         = "create_view"
	kindReplaceView        = "replace_view"
	kindDropView           = "drop_view"
	kindCreateMatView      = "create_materialized_view"
	kindDropMatView        = "drop_materialized_view"
	kindCreateTable        = "create_table"
	kindDropTable          = "drop_table"
	kindRenameTable        = "rename_table"
	kindAddColumn          = "add_column"
	kindDropColumn         = "drop_column"
	kindRenameColumn       = "rename_column"
	kindAlterColumn        = "alter_column"
	kindAddPrimaryKey      = "add_primary_key"
	kindDropPrimaryKey     = "drop_primary_key"
	kindAddUnique          = "add_unique"
	kindDropUnique         = "drop_unique"
	kindAddForeignKey      = "add_foreign_key"
	kindDropForeignKey     = "drop_foreign_key"
	kindAddCheck           = "add_check"
	kindDropCheck          = "drop_check"
	kindValidateConstraint = "validate_constraint"
	kindCreateIndex        = "create_index"
	kindDropIndex          = "drop_index"
	kindRenameIndex        = "rename_index"
	kindCreateEnum         = "create_enum"
	kindDropEnum           = "drop_enum"
	kindAddEnumValue       = "add_enum_value"
	kindRenameEnumValue    = "rename_enum_value"
	kindRenameEnum         = "rename_enum"
	kindCreateExtension    = "create_extension"
	kindRawSQL             = "raw_sql"
	kindStateOnly          = "state_only"
)

// marshalOp encodes one operation.
//
// The switch is exhaustive by construction, like the checksum's: an operation
// with no case produces an error rather than a file that silently lost it.
func marshalOp(op Operation) (opJSON, error) {
	kind := ""
	switch o := op.(type) {
	case CreateView:
		kind = kindCreateView
	case ReplaceView:
		kind = kindReplaceView
	case DropView:
		kind = kindDropView
	case CreateMaterializedView:
		kind = kindCreateMatView
	case DropMaterializedView:
		kind = kindDropMatView
	case CreateTable:
		kind = kindCreateTable
	case DropTable:
		kind = kindDropTable
	case RenameTable:
		kind = kindRenameTable
	case AddColumn:
		kind = kindAddColumn
	case DropColumn:
		kind = kindDropColumn
	case RenameColumn:
		kind = kindRenameColumn
	case AlterColumn:
		kind = kindAlterColumn
	case AddPrimaryKey:
		kind = kindAddPrimaryKey
	case DropPrimaryKey:
		kind = kindDropPrimaryKey
	case AddUnique:
		kind = kindAddUnique
	case DropUnique:
		kind = kindDropUnique
	case AddForeignKey:
		kind = kindAddForeignKey
	case DropForeignKey:
		kind = kindDropForeignKey
	case AddCheck:
		kind = kindAddCheck
	case DropCheck:
		kind = kindDropCheck
	case ValidateConstraint:
		kind = kindValidateConstraint
	case CreateIndex:
		kind = kindCreateIndex
	case DropIndex:
		kind = kindDropIndex
	case RenameIndex:
		kind = kindRenameIndex
	case CreateEnum:
		kind = kindCreateEnum
	case DropEnum:
		kind = kindDropEnum
	case AddEnumValue:
		kind = kindAddEnumValue
	case RenameEnumValue:
		kind = kindRenameEnumValue
	case RenameEnum:
		kind = kindRenameEnum
	case CreateExtension:
		kind = kindCreateExtension
	case RawSQL:
		kind = kindRawSQL
	case StateOnly:
		inner, err := marshalOp(o.Op)
		if err != nil {
			return opJSON{}, err
		}
		args, err := marshalArgs(inner)
		if err != nil {
			return opJSON{}, err
		}
		return opJSON{Op: kindStateOnly, Args: args}, nil
	case RunFunc:
		return opJSON{}, fmt.Errorf("the data migration %q is a Go function, which a migration file cannot hold;"+
			" write it as raw SQL, or keep the migration in a program that builds its own Set", o.Name)
	case unsupportedEnumChange:
		// The diff produces this to carry a refusal into a summary. Writing it
		// to a file would produce a migration nobody can run, so the refusal is
		// the one the operation itself gives — which says what to do instead.
		_, err := o.SQL()
		return opJSON{}, err
	default:
		return opJSON{}, fmt.Errorf("operation %T cannot be written to a migration file", op)
	}
	args, err := marshalArgs(op)
	if err != nil {
		return opJSON{}, err
	}
	return opJSON{Op: kind, Args: args}, nil
}

func marshalArgs(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// unmarshalOp decodes one operation.
func unmarshalOp(enc opJSON) (Operation, error) {
	read := func(v any) error {
		dec := json.NewDecoder(bytes.NewReader(enc.Args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("%s: %w", enc.Op, err)
		}
		return nil
	}
	switch enc.Op {
	case kindCreateView:
		var o CreateView
		return o, read(&o)
	case kindReplaceView:
		var o ReplaceView
		return o, read(&o)
	case kindDropView:
		var o DropView
		return o, read(&o)
	case kindCreateMatView:
		var o CreateMaterializedView
		return o, read(&o)
	case kindDropMatView:
		var o DropMaterializedView
		return o, read(&o)
	case kindCreateTable:
		var o CreateTable
		return o, read(&o)
	case kindDropTable:
		var o DropTable
		return o, read(&o)
	case kindRenameTable:
		var o RenameTable
		return o, read(&o)
	case kindAddColumn:
		var o AddColumn
		return o, read(&o)
	case kindDropColumn:
		var o DropColumn
		return o, read(&o)
	case kindRenameColumn:
		var o RenameColumn
		return o, read(&o)
	case kindAlterColumn:
		var o AlterColumn
		return o, read(&o)
	case kindAddPrimaryKey:
		var o AddPrimaryKey
		return o, read(&o)
	case kindDropPrimaryKey:
		var o DropPrimaryKey
		return o, read(&o)
	case kindAddUnique:
		var o AddUnique
		return o, read(&o)
	case kindDropUnique:
		var o DropUnique
		return o, read(&o)
	case kindAddForeignKey:
		var o AddForeignKey
		return o, read(&o)
	case kindDropForeignKey:
		var o DropForeignKey
		return o, read(&o)
	case kindAddCheck:
		var o AddCheck
		return o, read(&o)
	case kindDropCheck:
		var o DropCheck
		return o, read(&o)
	case kindValidateConstraint:
		var o ValidateConstraint
		return o, read(&o)
	case kindCreateIndex:
		var o CreateIndex
		return o, read(&o)
	case kindDropIndex:
		var o DropIndex
		return o, read(&o)
	case kindRenameIndex:
		var o RenameIndex
		return o, read(&o)
	case kindCreateEnum:
		var o CreateEnum
		return o, read(&o)
	case kindDropEnum:
		var o DropEnum
		return o, read(&o)
	case kindAddEnumValue:
		var o AddEnumValue
		return o, read(&o)
	case kindRenameEnumValue:
		var o RenameEnumValue
		return o, read(&o)
	case kindRenameEnum:
		var o RenameEnum
		return o, read(&o)
	case kindCreateExtension:
		var o CreateExtension
		return o, read(&o)
	case kindRawSQL:
		var o RawSQL
		return o, read(&o)
	case kindStateOnly:
		var inner opJSON
		if err := read(&inner); err != nil {
			return nil, err
		}
		op, err := unmarshalOp(inner)
		if err != nil {
			return nil, fmt.Errorf("state_only: %w", err)
		}
		return StateOnly{Op: op}, nil
	case "":
		return nil, fmt.Errorf("an operation has no %q field", "op")
	default:
		return nil, fmt.Errorf("unknown operation %q; it may come from a newer version of this tool", enc.Op)
	}
}

// Editable reports whether every operation in a migration can be written to a
// file, and names the first that cannot.
//
// It exists so that a command can refuse before it starts rather than after it
// has rendered half a summary.
func Editable(m *Migration) error {
	for i, op := range m.Operations {
		if _, err := marshalOp(op); err != nil {
			return fmt.Errorf("operation %d (%s): %w", i+1, op.Describe(), err)
		}
	}
	return nil
}

// sanitizeName turns a user-supplied migration name into an identifier safe to
// put in a file name.
//
// It is deliberately narrow: lower-case letters, digits and underscores, and
// nothing else. A name is a label for people, and letting one carry a path
// separator, a leading dot or a shell metacharacter would make the label decide
// where the file goes.
func sanitizeName(name string) string {
	var b strings.Builder
	lastUnderscore := true // suppresses a leading one
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case lastUnderscore:
			// Runs of anything else collapse into a single separator.
		default:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if len(out) > maxNameLength {
		out = strings.TrimRight(out[:maxNameLength], "_")
	}
	return out
}

// maxNameLength bounds the descriptive half of a migration ID. Long enough to
// say what a migration does, short enough that the file name stays readable in
// a directory listing and a review.
const maxNameLength = 60
