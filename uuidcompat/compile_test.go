package uuidcompat_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// What the compiler accepts about a uuid column, and what it refuses.
//
// The typed API's whole claim is that a wrong comparison does not build. Testing
// the accepted half is easy — this file compiles, so it happened. Testing the
// refused half is where qualifications usually cheat: a snippet kept in testdata
// and never handed to a compiler proves that somebody wrote a snippet.
//
// So the negatives below are compiled, and the test fails if any of them builds.
// It also fails if one of them fails to build for the wrong reason, because a
// typo would otherwise look exactly like a type error.

// compilePositives is one function rather than many so that everything in it is
// in the same scope as the real API. If any line here stops compiling, the
// package stops compiling, which is the strongest form this evidence takes.
func compilePositives() {
	id := uuid.New()

	// Equality against a uuid column, and the nullable one.
	_ = domain.Users.ID.Eq(id)
	_ = domain.Users.OptionalID.Eq(id)
	_ = domain.Users.OptionalID.IsNull()

	// Membership.
	_ = domain.Users.ID.In(id, uuid.New())
	_ = domain.Orders.UserID.In(id)

	// Ordering: uuid has a SQL ordering, so the column offers one.
	_ = domain.Users.ID.Asc()
	_ = domain.Users.ID.Desc()
	_ = domain.Users.ID.Gt(id)
	_ = domain.Users.ID.Between(uuid.Nil, id)

	// Through a view and a materialized view.
	_ = domain.UserOrders.UserID.Eq(id)
	_ = domain.UserSummaries.UserID.Eq(id)

	// A domain over uuid is a uuid column.
	_ = domain.Tokens.TenantID.Eq(id)

	// A projection over uuid, and one that mixes it with the int8 aggregate.
	_ = orm.Project1(domain.Users.ID, func(u uuid.UUID) uuid.UUID { return u })
	_ = orm.Project2(domain.Orders.UserID, orm.Count[domain.Order](),
		func(u uuid.UUID, n int64) tally { return tally{userID: u, orders: n} })

	// Composed: the non-nullable lift and the outer-join lift.
	_ = orm.Of(domain.Users.ID)
	_ = orm.Opt(domain.Orders.ID)
	_ = orm.Eq(domain.Orders.UserID, domain.Users.ID)
}

// The negatives, each with the compiler error it is expected to produce.
//
// The expected fragment is deliberately about types rather than about wording,
// because a Go error message is not a stable interface — but "cannot use" and
// the offending type's name are what any version of the compiler says.
var compileNegatives = []struct {
	name string
	body string
	want string
}{
	{
		name: "Eq(string)",
		body: `_ = domain.Users.ID.Eq("2f1c8f8e-0000-0000-0000-000000000000")`,
		want: "uuid.UUID",
	},
	{
		name: "Eq(int)",
		body: `_ = domain.Users.ID.Eq(42)`,
		want: "uuid.UUID",
	},
	{
		name: "Eq([]byte)",
		body: `_ = domain.Users.ID.Eq([]byte{1, 2, 3})`,
		want: "uuid.UUID",
	},
	{
		name: "In(string...)",
		body: `_ = domain.Users.ID.In("a", "b")`,
		want: "uuid.UUID",
	},
	{
		name: "a uuid predicate reaching another entity's query",
		body: `_ = domain.Orders.Query().Where(domain.Users.ID.Eq(uuid.New()))`,
		want: "domain.Order",
	},
	{
		name: "the nullable side of an outer join read as non-nullable",
		body: `var _ orm.Expression[uuid.UUID, *uuid.UUID] = orm.Opt(domain.Orders.ID)`,
		want: "Expression",
	},
}

// Each negative is compiled on its own, and none of them may build.
func TestUUID_wrongComparisonsDoNotCompile(t *testing.T) {
	for _, c := range compileNegatives {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := `package negative

import (
	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

var _ = uuid.Nil
var _ = orm.Concurrently

func negative() {
	` + c.body + `
}
`
			// The file is written inside this module so that it compiles against
			// this module's generated code rather than against a copy.
			pkg := filepath.Join("negativeprobe", filepath.Base(dir))
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll("negativeprobe") })
			if err := os.WriteFile(filepath.Join(pkg, "negative.go"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("go", "build", "./"+filepath.ToSlash(pkg))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("this compiled, and it must not:\n%s", c.body)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("it failed to build, but not for the expected reason — a typo "+
					"would look the same. want a mention of %q:\n%s", c.want, out)
			}
		})
	}
}

// The positives are compiled by this package existing, and referenced here so
// that the function is not dead code the linter would remove.
func TestUUID_typedUUIDAPICompiles(t *testing.T) {
	compilePositives()
	if got := len(compileNegatives); got < 4 {
		t.Fatalf("only %d compile-negative fixtures; the evidence is thinner than "+
			"the contract it stands for", got)
	}
}
