package gendemo_test

import (
	"net"
	"net/netip"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M12.2: PostgreSQL's network types.
//
// The claim is that inet and cidr are an address with a prefix length rather
// than a string that happens to look like one — so they round-trip through
// netip.Prefix, the operators are PostgreSQL's own, and the answers are
// PostgreSQL's.

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return p
}

func seedNetworks(t *testing.T, db *gendemo.DB) {
	t.Helper()
	mac, err := net.ParseMAC("08:00:2b:01:02:03")
	if err != nil {
		t.Fatalf("parsing a MAC: %v", err)
	}
	fallback := prefix(t, "10.0.0.1/32")
	rows := []gendemo.Network{
		{Label: "office v4", Subnet: prefix(t, "192.168.1.0/24"), Host: prefix(t, "192.168.1.10/32"),
			Fallback: &fallback, Hardware: mac},
		{Label: "datacentre v4", Subnet: prefix(t, "10.0.0.0/8"), Host: prefix(t, "10.1.2.3/32"),
			Hardware: mac},
		{Label: "office v6", Subnet: prefix(t, "2001:db8::/32"), Host: prefix(t, "2001:db8::1/128"),
			Hardware: mac},
	}
	if _, err := db.Networks.InsertMany(t.Context(), rows); err != nil {
		t.Fatalf("seeding networks: %v", err)
	}
}

// Values survive the round trip as addresses, with their prefix lengths, in
// both address families.
func TestNetwork_roundTrip(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)
	seedNetworks(t, db)

	got, err := db.Networks.Query().OrderBy(gendemo.Networks.Label.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d rows", len(got))
	}
	byLabel := map[string]gendemo.Network{}
	for _, n := range got {
		byLabel[n.Label] = n
	}

	office := byLabel["office v4"]
	if office.Subnet.String() != "192.168.1.0/24" {
		t.Errorf("a cidr round-tripped as %q", office.Subnet)
	}
	if office.Host.String() != "192.168.1.10/32" {
		t.Errorf("an inet round-tripped as %q", office.Host)
	}
	if office.Fallback == nil || office.Fallback.String() != "10.0.0.1/32" {
		t.Errorf("a nullable inet round-tripped as %v", office.Fallback)
	}
	if office.Hardware.String() != "08:00:2b:01:02:03" {
		t.Errorf("a macaddr round-tripped as %q", office.Hardware)
	}
	// A NULL inet stays nil rather than becoming the zero prefix.
	if dc := byLabel["datacentre v4"]; dc.Fallback != nil {
		t.Errorf("a SQL NULL became %v", dc.Fallback)
	}
	// IPv6 keeps its family.
	if v6 := byLabel["office v6"]; v6.Subnet.String() != "2001:db8::/32" || !v6.Host.Addr().Is6() {
		t.Errorf("an IPv6 network round-tripped as %q / %q", v6.Subnet, v6.Host)
	}
}

// The operators are PostgreSQL's, and their answers are compared with
// PostgreSQL's own.
func TestNetwork_operatorsMatchHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedNetworks(t, db)

	shape := orm.Project1(orm.Of(gendemo.Networks.Label), func(s string) string { return s })
	run := func(t *testing.T, p orm.Predicate[orm.Composed]) []string {
		t.Helper()
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Networks.Source()).
			Where(p).
			OrderBy(orm.Of(gendemo.Networks.Label).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got == nil {
			got = []string{}
		}
		return got
	}
	handwritten := func(t *testing.T, sql string) []string {
		t.Helper()
		rows, err := conn.Query(t.Context(), sql)
		if err != nil {
			t.Fatalf("handwritten query: %v", err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}

	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
		sql  string
	}{
		{"host contained by subnet",
			orm.ContainedBy(gendemo.Networks.Host, gendemo.Networks.Subnet),
			`SELECT label FROM networks WHERE host << subnet ORDER BY label`},
		{"host contained by or equal",
			orm.ContainedByOrEquals(gendemo.Networks.Host, gendemo.Networks.Subnet),
			`SELECT label FROM networks WHERE host <<= subnet ORDER BY label`},
		{"subnet contains a literal address",
			orm.ContainsNetwork(gendemo.Networks.Subnet, orm.Val(prefix(t, "10.1.2.3/32"))),
			`SELECT label FROM networks WHERE subnet >> '10.1.2.3/32'::inet ORDER BY label`},
		{"subnet contains or equals itself",
			orm.ContainsNetworkOrEquals(gendemo.Networks.Subnet, gendemo.Networks.Subnet),
			`SELECT label FROM networks WHERE subnet >>= subnet ORDER BY label`},
		{"overlapping networks",
			orm.NetworksOverlap(gendemo.Networks.Subnet, orm.Val(prefix(t, "10.0.0.0/16"))),
			`SELECT label FROM networks WHERE subnet && '10.0.0.0/16'::cidr ORDER BY label`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, want := run(t, tt.pred), handwritten(t, tt.sql)
			if len(got) != len(want) {
				t.Fatalf("the ORM returned %v, handwritten SQL returned %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("row %d: %q against %q", i, got[i], want[i])
				}
			}
			if len(got) == 0 {
				t.Error("no rows matched, so the operator proved nothing")
			}
		})
	}
}

// The functions return what PostgreSQL says they return.
func TestNetwork_functions(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedNetworks(t, db)

	one := func(t *testing.T, v orm.Selectable[orm.Composed, string]) string {
		t.Helper()
		got, err := orm.Compose(db.Executor(), orm.Project1(v, func(s string) string { return s })).
			From(gendemo.Networks.Source()).
			Where(orm.Cond(gendemo.Networks.Label.Eq("office v4"))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		return got
	}
	if got := one(t, orm.Host(gendemo.Networks.Host)); got != "192.168.1.10" {
		t.Errorf("host() = %q", got)
	}

	masked, err := orm.Compose(db.Executor(),
		orm.Project1(orm.MaskLen(gendemo.Networks.Subnet), func(v int32) int32 { return v })).
		From(gendemo.Networks.Source()).
		Where(orm.Cond(gendemo.Networks.Label.Eq("office v4"))).
		One(t.Context())
	if err != nil {
		t.Fatalf("masklen: %v", err)
	}
	if masked != 24 {
		t.Errorf("masklen() = %d", masked)
	}

	network, err := orm.Compose(db.Executor(),
		orm.Project1(orm.Network(gendemo.Networks.Host), func(p netip.Prefix) string { return p.String() })).
		From(gendemo.Networks.Source()).
		Where(orm.Cond(gendemo.Networks.Label.Eq("office v4"))).
		One(t.Context())
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	if network != "192.168.1.10/32" {
		t.Errorf("network() = %q", network)
	}

	// A nullable address keeps a nullable result.
	nullable, err := orm.Compose(db.Executor(),
		orm.Project1(orm.HostNull(gendemo.Networks.Fallback), func(s *string) *string { return s })).
		From(gendemo.Networks.Source()).
		Where(orm.Cond(gendemo.Networks.Label.Eq("datacentre v4"))).
		One(t.Context())
	if err != nil {
		t.Fatalf("host of a nullable address: %v", err)
	}
	if nullable != nil {
		t.Errorf("host(NULL) = %q, want NULL", *nullable)
	}

	// And the types this package claims are the types PostgreSQL reports.
	for _, tt := range []struct{ what, sql, want string }{
		{"host", `SELECT pg_typeof(host(host)) FROM networks LIMIT 1`, "text"},
		{"masklen", `SELECT pg_typeof(masklen(subnet)) FROM networks LIMIT 1`, "integer"},
		{"network", `SELECT pg_typeof(network(host)) FROM networks LIMIT 1`, "cidr"},
		{"containment", `SELECT pg_typeof(host << subnet) FROM networks LIMIT 1`, "boolean"},
		{"inet column", `SELECT pg_typeof(host) FROM networks LIMIT 1`, "inet"},
		{"cidr column", `SELECT pg_typeof(subnet) FROM networks LIMIT 1`, "cidr"},
		{"macaddr column", `SELECT pg_typeof(hardware) FROM networks LIMIT 1`, "macaddr"},
	} {
		t.Run("pg_typeof "+tt.what, func(t *testing.T) {
			if got := pgTypeOf(t, conn, tt.sql); got != tt.want {
				t.Errorf("pg_typeof = %q, this package claims %q", got, tt.want)
			}
		})
	}
}

// The network types travel through COPY on their own codecs rather than as
// text, and a nullable one keeps its NULL.
func TestNetwork_copy(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	mac, err := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("parsing a MAC: %v", err)
	}
	fb := prefix(t, "203.0.113.9/32")
	n, err := db.Networks.CopyFrom(t.Context(), []gendemo.Network{
		{Label: "copied v4", Subnet: prefix(t, "172.16.0.0/12"), Host: prefix(t, "172.16.5.4/32"),
			Fallback: &fb, Hardware: mac},
		{Label: "copied v6", Subnet: prefix(t, "2001:db8:1::/48"), Host: prefix(t, "2001:db8:1::5/128"),
			Hardware: mac},
	})
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != 2 {
		t.Fatalf("copied %d rows", n)
	}

	got, err := db.Networks.Query().
		Where(gendemo.Networks.Label.In("copied v4", "copied v6")).
		OrderBy(gendemo.Networks.Label.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read back %d rows", len(got))
	}
	if got[0].Subnet.String() != "172.16.0.0/12" || got[0].Fallback == nil {
		t.Errorf("the copied IPv4 row = %+v", got[0])
	}
	if got[1].Subnet.String() != "2001:db8:1::/48" || got[1].Fallback != nil {
		t.Errorf("the copied IPv6 row = %+v", got[1])
	}
	if got[0].Hardware.String() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("a copied macaddr = %q", got[0].Hardware)
	}
}

// Network expressions compose with M11: an outer join makes them nullable, and
// the nullability survives the composition.
func TestNetwork_outerJoinNullability(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedNetworks(t, db)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (77, 'no match')`)

	shape := orm.Project2(
		orm.Of(gendemo.Categories.ID), orm.Opt(gendemo.Networks.Subnet),
		func(id int64, p *netip.Prefix) *netip.Prefix { return p },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(gendemo.Networks.Source(), orm.Eq(gendemo.Networks.ID, gendemo.Categories.ID)).
		Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0] != nil {
		t.Errorf("a NOT NULL cidr read through a LEFT JOIN with no match = %v", got)
	}
}
