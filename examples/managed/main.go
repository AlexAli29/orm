// Command journal is a small program over a managed schema.
//
// The blog example is database-first: PostgreSQL owns the schema and the ORM
// proves the structs agree with it. This one is the other mode. The schema is
// declared in Go, a migration carries it to PostgreSQL, and the runtime is the
// same runtime — which is the point. Managed mode changes who decides what the
// schema is, and nothing else.
//
//	docker compose up -d
//	export JOURNAL_DSN='postgres://journal:journal@localhost:55434/journal?sslmode=disable'
//	go run ../../cmd/orm migrate --config orm.yaml
//	go run .
//
// See README.md for the whole sequence, including what to do after changing a
// declaration.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/examples/managed/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "journal:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("JOURNAL_DSN")
	if dsn == "" {
		return errors.New("set JOURNAL_DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parsing JOURNAL_DSN: %w", err)
	}
	// Every connection is taught this package's PostgreSQL types once, when it
	// is opened. Without it the article_status enum arrives as bytes pgx has no
	// codec for.
	cfg.AfterConnect = domain.RegisterTypes

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening the pool: %w", err)
	}
	defer pool.Close()

	db := domain.New(pool)
	if err := seed(ctx, db); err != nil {
		return err
	}
	if err := feed(ctx, db, os.Stdout); err != nil {
		return err
	}
	if err := stats(ctx, db, os.Stdout); err != nil {
		return err
	}
	return publish(ctx, db, os.Stdout)
}

// titles is a result shape: two expressions and the Go value that reads them.
// It is declared once and used by every query that wants that shape.
var titles = orm.Project2(
	domain.Articles.ID, domain.Articles.Title,
	func(id int64, title string) string { return fmt.Sprintf("%d %s", id, title) },
)

// stats is the grouped-aggregate endpoint: how many articles each author has,
// and how many of them are published.
//
// The counts are PostgreSQL's, taken in one statement over the groups. FILTER
// is what lets the second count see a subset of the rows without the first one
// seeing it too.
func stats(ctx context.Context, db *domain.DB, out io.Writer) error {
	// Grouping the authors' own rows alongside their articles' would need a
	// join, which this milestone does not have. The statistics are therefore
	// per author key, over the articles table where the aggregate belongs.
	perAuthor := orm.Project3(
		domain.Articles.AuthorID,
		orm.Count[domain.Article]().As("articles"),
		orm.Count[domain.Article]().
			Filter(domain.Articles.Status.Eq(domain.StatusPublished)).As("published"),
		func(author, all, published int64) string {
			return fmt.Sprintf("author %d: %d article(s), %d published", author, all, published)
		},
	)
	lines, err := orm.Select(db.Articles, perAuthor).
		GroupBy(domain.Articles.AuthorID).
		Having(orm.Count[domain.Article]().Gt(0)).
		OrderBy(domain.Articles.AuthorID.Asc()).
		All(ctx)
	if err != nil {
		return fmt.Errorf("reading the statistics: %w", err)
	}
	fmt.Fprintln(out)
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	return nil
}

// publish moves the drafts to published and reports what it changed, in one
// statement.
//
// The timestamp is computed by PostgreSQL rather than sent, and the rows come
// back from the UPDATE itself: there is no SELECT before it and none after.
func publish(ctx context.Context, db *domain.DB, out io.Writer) error {
	moved, err := orm.UpdateReturning(
		db.Articles.Update().
			Set(
				domain.Articles.Status.Set(domain.StatusPublished),
				domain.Articles.PublishedAt.SetExpr(
					orm.RawValue[domain.Article, *time.Time]("now()")),
			).
			Where(domain.Articles.Status.Eq(domain.StatusDraft)),
		titles,
	).All(ctx)
	if err != nil {
		return fmt.Errorf("publishing the drafts: %w", err)
	}
	fmt.Fprintln(out)
	for _, t := range moved {
		fmt.Fprintf(out, "published %s\n", t)
	}
	return nil
}

// seed writes one author and their articles, in one transaction.
//
// It is idempotent by the only means that is actually reliable: it asks first,
// and does nothing if the row is there.
func seed(ctx context.Context, db *domain.DB) error {
	existing, err := db.Authors.Query().Where(domain.Authors.Email.Eq("ada@example.com")).Exists(ctx)
	if err != nil {
		return fmt.Errorf("looking for the author: %w", err)
	}
	if existing {
		return nil
	}

	return db.Tx(ctx, func(tx *domain.DB) error {
		// An upsert that computes from both rows: the name comes from the row
		// the insert could not add, and nothing else about the existing one is
		// disturbed.
		author, err := tx.Authors.Insert(ctx, domain.Author{
			Email: "ada@example.com",
			Name:  "Ada",
		}, orm.OnConflict(domain.Authors.Email).DoUpdateSet(
			domain.Authors.Name.SetExpr(orm.Excluded(domain.Authors.Name)),
		))
		if err != nil {
			return fmt.Errorf("inserting the author: %w", err)
		}
		published := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
		for _, a := range []domain.Article{
			{AuthorID: author.ID, Title: "On notation", Body: "…", Status: domain.StatusPublished, PublishedAt: &published},
			{AuthorID: author.ID, Title: "On engines", Body: "…", Status: domain.StatusPublished, PublishedAt: ptr(published.AddDate(0, 1, 0))},
			{AuthorID: author.ID, Title: "Unfinished", Body: "…", Status: domain.StatusDraft},
		} {
			if _, err := tx.Articles.Insert(ctx, a); err != nil {
				return fmt.Errorf("inserting %q: %w", a.Title, err)
			}
		}
		return nil
	})
}

// feed prints each author with their published articles, newest first.
//
// This is the query the schema was declared for: the partial covering index on
// (author_id, published_at DESC) WHERE status = 'published' is exactly its
// shape, which is why declaring the schema in PostgreSQL's own vocabulary was
// worth doing.
func feed(ctx context.Context, db *domain.DB, out io.Writer) error {
	authors, err := db.Authors.Query().
		OrderBy(domain.Authors.Name.Asc()).
		With(domain.Authors.Articles.
			Where(domain.Articles.Status.Eq(domain.StatusPublished)).
			OrderBy(domain.Articles.PublishedAt.Desc())).
		All(ctx)
	if err != nil {
		return fmt.Errorf("reading the feed: %w", err)
	}

	for _, a := range authors {
		fmt.Fprintf(out, "%s <%s>\n", a.Name, a.Email)
		for _, article := range a.Articles.MustGet() {
			fmt.Fprintf(out, "    %s  %s\n", article.PublishedAt.Format("2006-01-02"), article.Title)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
