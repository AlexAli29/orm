package gendemo_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// COPY against InsertMany.
//
// Both write the same rows into the same table on the same connection, so what
// the numbers compare is the protocol rather than the machine. Nothing here
// fails a build: the point is to be able to see a regression, and to be able to
// say honestly how much faster COPY is rather than asserting that it is.

func benchRows(n int) []gendemo.Category {
	out := make([]gendemo.Category, n)
	for i := range out {
		out[i] = gendemo.Category{Name: fmt.Sprintf("category-%d", i)}
	}
	return out
}

// freshDB gives each iteration an empty database, so that neither path is
// measured against rows the previous one left behind.
func freshDB(b *testing.B) *gendemo.DB {
	b.Helper()
	dsn := testdb.Create(b, schemaFor(b))
	return gendemo.New(testdb.Connect(b, dsn))
}

// schemaFor reads the fixture DDL without the seeded rows.
func schemaFor(b *testing.B) string {
	b.Helper()
	ddl, err := os.ReadFile("schema.sql")
	if err != nil {
		b.Fatalf("reading schema.sql: %v", err)
	}
	return string(ddl)
}

func runIngestBench(b *testing.B, n int, ingest func(db *gendemo.DB, rows []gendemo.Category) error) {
	b.Helper()
	rows := benchRows(n)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		db := freshDB(b)
		b.StartTimer()
		if err := ingest(db, rows); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "rows/sec")
}

func BenchmarkIngest(b *testing.B) {
	if os.Getenv(testdb.EnvAdminDSN) == "" {
		b.Skipf("%s is not set", testdb.EnvAdminDSN)
	}
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("copy/%d", n), func(b *testing.B) {
			runIngestBench(b, n, func(db *gendemo.DB, rows []gendemo.Category) error {
				_, err := db.Categories.CopyFrom(b.Context(), rows)
				return err
			})
		})
		b.Run(fmt.Sprintf("insertMany/%d", n), func(b *testing.B) {
			runIngestBench(b, n, func(db *gendemo.DB, rows []gendemo.Category) error {
				_, err := db.Categories.InsertMany(b.Context(), rows)
				return err
			})
		})
	}
}
