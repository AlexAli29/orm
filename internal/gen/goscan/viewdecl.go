package goscan

import (
	"fmt"
	"go/ast"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/model"
	"golang.org/x/tools/go/packages"
)

// Reading a managed view's definition and dependencies.
//
// Both spellings of a definition are supported, because they answer different
// needs and neither covers the other:
//
//	//orm:definition "SELECT id, email FROM users WHERE active"
//	//orm:definition ./sql/active_users.sql
//
// The inline form matches what //orm:check already does with SQL and keeps a
// three-line view where a reader can see it. The file form exists because a
// real view definition is thirty lines of SQL, and thirty lines of SQL inside
// Go comments is unreadable, unformattable and unreviewable — it would be a
// worse escape hatch than the one it replaced. A quoted argument is inline; an
// unquoted one is a path.
//
// Nothing here reads the SQL. Dependencies are declared, never inferred: a
// definition may name a relation inside a CTE, behind a function, through a
// quoted identifier or not at all, and a text search that guessed would be
// wrong in whichever direction happened to be least safe.

// viewDecl reads the definition, dependencies and creation policy written
// beside a view directive.
func (s *scanner) viewDecl(pkg *packages.Package, doc *ast.CommentGroup, pos model.Position) *model.ViewDecl {
	out := &model.ViewDecl{Pos: pos}
	pkgDir := s.packageDir(pkg)

	seenDefinition := false
	for _, c := range doc.List {
		text := strings.TrimRight(c.Text, " \t")
		switch {
		case strings.HasPrefix(text, defnDirective):
			if seenDefinition {
				s.fail(fmt.Errorf("%s: %s is declared twice; one relation has one definition",
					s.position(pkg, c.Pos()), defnDirective))
				return out
			}
			seenDefinition = true
			s.readDefinition(pkg, c, out, pkgDir)

		case strings.HasPrefix(text, dependsDirective):
			arg := strings.TrimSpace(strings.TrimPrefix(text, dependsDirective))
			if arg == "" {
				s.fail(fmt.Errorf("%s: %s needs a schema-qualified relation name",
					s.position(pkg, c.Pos()), dependsDirective))
				continue
			}
			for _, name := range strings.FieldsFunc(arg, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				ref, err := model.ParseTableRef(name)
				if err != nil {
					s.fail(fmt.Errorf("%s: %s: %w", s.position(pkg, c.Pos()), dependsDirective, err))
					continue
				}
				out.DependsOn = append(out.DependsOn, ref)
			}

		case strings.HasPrefix(text, withNoDataDirect):
			out.WithNoData = true
		case strings.HasPrefix(text, withDataDirective):
			out.WithNoData = false
		}
	}
	return out
}

// readDefinition reads one //orm:definition directive.
func (s *scanner) readDefinition(pkg *packages.Package, c *ast.Comment, out *model.ViewDecl, pkgDir string) {
	pos := s.position(pkg, c.Pos())
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(c.Text, " \t"), defnDirective))
	if arg == "" {
		s.fail(fmt.Errorf("%s: %s needs either quoted SQL or a path to a .sql file", pos, defnDirective))
		return
	}

	// A quoted argument is the SQL itself. Anything else is a path.
	if arg[0] == '"' || arg[0] == '`' {
		sql, rest, err := takeInlineSQL(arg)
		if err != nil {
			s.fail(fmt.Errorf("%s: %s: %w", pos, defnDirective, err))
			return
		}
		if strings.TrimSpace(rest) != "" {
			s.fail(fmt.Errorf("%s: %s takes one definition; %q follows the closing quote",
				pos, defnDirective, strings.TrimSpace(rest)))
			return
		}
		if strings.TrimSpace(sql) == "" {
			s.fail(fmt.Errorf("%s: %s is empty", pos, defnDirective))
			return
		}
		out.Mode, out.SQL = model.DefinitionInlineSQL, sql
		return
	}

	// A path. It is resolved against the package directory and must stay inside
	// it: a definition reached through .. would make the schema depend on what
	// happens to sit beside the checkout, and an absolute path would put a
	// machine's layout into the lock.
	if filepath.IsAbs(arg) {
		s.fail(fmt.Errorf("%s: %s: %q is an absolute path. A definition path is relative to "+
			"the package directory, so that the same source produces the same schema on every machine",
			pos, defnDirective, arg))
		return
	}
	clean := filepath.Clean(filepath.FromSlash(arg))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		s.fail(fmt.Errorf("%s: %s: %q leaves the package directory. A definition belongs with the "+
			"declaration that names it", pos, defnDirective, arg))
		return
	}
	if pkgDir == "" {
		s.fail(fmt.Errorf("%s: %s: the package's directory could not be resolved, so %q cannot be read",
			pos, defnDirective, arg))
		return
	}
	body, err := os.ReadFile(filepath.Join(pkgDir, clean))
	if err != nil {
		s.fail(fmt.Errorf("%s: %s: reading %s: %w", pos, defnDirective, arg, err))
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		s.fail(fmt.Errorf("%s: %s: %s is empty", pos, defnDirective, arg))
		return
	}
	// The path is stored as written, in slash form, so the lock reads the same
	// on every operating system.
	out.Mode, out.SQL, out.File = model.DefinitionFileSQL, string(body), filepath.ToSlash(clean)
}

// takeInlineSQL reads an inline definition from the front of a directive.
//
// Both Go quoting forms are accepted, and the backtick form is the one to
// reach for. SQL is full of single quotes and double-quoted identifiers, and a
// definition written in a Go interpreted string has to escape every one of
// them — which turns a readable SELECT into a line nobody can check by eye. A
// raw string escapes nothing, so what is in the comment is what PostgreSQL
// sees.
//
// Neither form can hold a newline: a // comment ends at one. That is a real
// limit rather than an oversight, and it is why //orm:definition also takes a
// path — see the package comment on this file.
func takeInlineSQL(s string) (sql, rest string, err error) {
	if s[0] == '`' {
		end := strings.IndexByte(s[1:], '`')
		if end < 0 {
			return "", "", fmt.Errorf("unterminated definition: %s", s)
		}
		return s[1 : 1+end], s[2+end:], nil
	}
	return unquotePrefix(s)
}

// packageDir returns the directory a package's files live in.
func (s *scanner) packageDir(pkg *packages.Package) string {
	if len(pkg.GoFiles) == 0 {
		return ""
	}
	return filepath.Dir(pkg.GoFiles[0])
}
