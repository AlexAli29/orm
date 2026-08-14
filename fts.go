package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Full text search.
//
// PostgreSQL's search is built on two types rather than on string matching. A
// tsvector is a document that has already been parsed into normalised lexemes
// with their positions; a tsquery is a search expression that has been parsed
// the same way. The @@ operator asks whether one matches the other, and every
// useful property — stemming, stop words, phrase distance, ranking — comes from
// the fact that both sides were parsed by the same text search configuration.
//
// So neither maps to string here. [TSVector] and [TSQuery] are named types, and
// that is the whole reason: a column holding a parsed document is not a column
// holding words, and comparing one to a string is a mistake worth refusing at
// compile time.

// TSVector is a PostgreSQL tsvector: a parsed, normalised document.
//
// The text is PostgreSQL's own output format, which is what a row carries it
// as. Building one in Go by hand is possible and rarely what you want —
// [ToTSVector] asks PostgreSQL to parse a document with a configuration, which
// is the only way to get lexemes that will match a query parsed the same way.
type TSVector string

// TSQuery is a PostgreSQL tsquery: a parsed search expression.
//
// Build one with [ToTSQuery], [PlainToTSQuery], [PhraseToTSQuery] or
// [WebSearchToTSQuery] rather than by writing the text, so that PostgreSQL
// parses it under a configuration you named.
type TSQuery string

// TextSearchConfig names a PostgreSQL text search configuration.
//
// It is a type rather than a string because the name reaches SQL as a regconfig
// argument, and PostgreSQL resolves it against the catalog. Passing it as a
// bind parameter is what keeps it a value: nothing here concatenates it into
// the statement, so a configuration name from a request cannot become syntax.
type TextSearchConfig string

// The configurations PostgreSQL ships with. A project with its own is named
// with [SearchConfig].
const (
	English TextSearchConfig = "english"
	Simple  TextSearchConfig = "simple"
)

// SearchConfig names a text search configuration this package does not list.
//
// The name is still a value rather than syntax — it is bound and cast to
// regconfig, so PostgreSQL resolves it and reports an unknown one as its own
// error.
func SearchConfig(name string) TextSearchConfig { return TextSearchConfig(name) }

// regconfig binds a configuration name as a value cast to PostgreSQL's
// regconfig, which is what the two-argument search functions take.
func regconfig(c TextSearchConfig) expr.Node {
	return expr.Cast{X: expr.Arg{Value: string(c)}, Type: "regconfig"}
}

// ToTSVector parses a document into a tsvector under a configuration.
//
//	orm.ToTSVector(orm.English, Posts.Title)
//
// The result is non-nullable for a non-nullable document: to_tsvector of a
// value is a value, even when the value produces no lexemes at all — an empty
// tsvector is a tsvector.
func ToTSVector[E any](cfg TextSearchConfig, doc Selectable[E, string]) Expression[TSVector, *TSVector] {
	return Expression[TSVector, *TSVector]{node: expr.Call{
		Func: "to_tsvector", Args: []expr.Node{regconfig(cfg), doc.selectItem().Node},
	}}
}

// ToTSVectorNull parses a nullable document, which stays nullable.
func ToTSVectorNull[E any](cfg TextSearchConfig, doc Selectable[E, *string]) Expression[*TSVector, *TSVector] {
	return Expression[*TSVector, *TSVector]{
		node: expr.Call{
			Func: "to_tsvector", Args: []expr.Node{regconfig(cfg), doc.selectItem().Node},
		},
		nullSafe: true,
	}
}

// The query constructors.
//
// The four differ in how they read the text, and the difference is the whole
// reason to pick one:
//
//	ToTSQuery           the text is already tsquery syntax, with & | ! and <->
//	PlainToTSQuery      the words are ANDed; punctuation is ignored
//	PhraseToTSQuery     the words must appear adjacent, in order
//	WebSearchToTSQuery  the text is what a search box gets: quotes, or, -word
//
// All four take the search text as a bind parameter. None of it becomes syntax,
// so text a user typed is text a user typed.

// ToTSQuery parses text that is already in tsquery syntax.
func ToTSQuery(cfg TextSearchConfig, text string) Expression[TSQuery, *TSQuery] {
	return tsQuery("to_tsquery", cfg, text)
}

// PlainToTSQuery parses plain text, ANDing the words it finds.
func PlainToTSQuery(cfg TextSearchConfig, text string) Expression[TSQuery, *TSQuery] {
	return tsQuery("plainto_tsquery", cfg, text)
}

// PhraseToTSQuery parses plain text into a phrase, so the words must be
// adjacent and in order.
func PhraseToTSQuery(cfg TextSearchConfig, text string) Expression[TSQuery, *TSQuery] {
	return tsQuery("phraseto_tsquery", cfg, text)
}

// WebSearchToTSQuery parses the text a search box produces: quoted phrases, the
// word or, and a leading minus to exclude. It never fails on syntax, which is
// what makes it the right one for input you did not write.
func WebSearchToTSQuery(cfg TextSearchConfig, text string) Expression[TSQuery, *TSQuery] {
	return tsQuery("websearch_to_tsquery", cfg, text)
}

func tsQuery(fn string, cfg TextSearchConfig, text string) Expression[TSQuery, *TSQuery] {
	return Expression[TSQuery, *TSQuery]{node: expr.Call{
		Func: fn, Args: []expr.Node{regconfig(cfg), expr.Arg{Value: text}},
	}}
}

// Matches builds document @@ query.
//
// Both sides are parsed values rather than text, which is why this takes a
// [TSQuery] rather than a string: matching a tsvector against a word nobody
// parsed would compare lexemes to characters.
func Matches[A, B any](doc Optional[A, *TSVector], query Optional[B, *TSQuery]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "@@", Left: doc.optItem().Node, Right: query.optItem().Node,
	}}
}

// TSRank scores how well a document matches a query.
//
// PostgreSQL returns real — a 32-bit float — rather than double precision, and
// this says so: a score read back as float64 would be claiming precision the
// server never sent.
//
// Both arguments are the non-nullable forms, because ts_rank of a NULL is NULL:
// a vector that can be absent — a nullable column, or one read through an outer
// join — needs [TSRankNull], whose result can hold the answer.
func TSRank[A, B any](doc Selectable[A, TSVector], query Selectable[B, TSQuery]) Expression[float32, *float32] {
	return rank("ts_rank", doc.selectItem().Node, query.selectItem().Node)
}

// TSRankNull scores a match where either side may be NULL, and is nullable
// because PostgreSQL's answer then is NULL rather than zero.
func TSRankNull[A, B any](doc Optional[A, *TSVector], query Optional[B, *TSQuery]) Expression[*float32, *float32] {
	return rankNull("ts_rank", doc.optItem().Node, query.optItem().Node)
}

// TSRankCD scores a match by cover density, which accounts for how close the
// matching lexemes are to one another.
func TSRankCD[A, B any](doc Selectable[A, TSVector], query Selectable[B, TSQuery]) Expression[float32, *float32] {
	return rank("ts_rank_cd", doc.selectItem().Node, query.selectItem().Node)
}

// TSRankCDNull is [TSRankCD] where either side may be NULL.
func TSRankCDNull[A, B any](doc Optional[A, *TSVector], query Optional[B, *TSQuery]) Expression[*float32, *float32] {
	return rankNull("ts_rank_cd", doc.optItem().Node, query.optItem().Node)
}

func rank(fn string, doc, query expr.Node) Expression[float32, *float32] {
	return Expression[float32, *float32]{node: expr.Call{Func: fn, Args: []expr.Node{doc, query}}}
}

func rankNull(fn string, doc, query expr.Node) Expression[*float32, *float32] {
	return Expression[*float32, *float32]{
		node:     expr.Call{Func: fn, Args: []expr.Node{doc, query}},
		nullSafe: true,
	}
}

// TSWeight is the label setweight attaches to every lexeme of a vector.
//
// The four are ranked A > B > C > D, and ts_rank reads them through its weight
// array. They are a closed set because the label becomes a one-character string
// PostgreSQL validates, and offering an open one would offer a runtime error.
type TSWeight string

// The four weights PostgreSQL defines.
const (
	WeightA TSWeight = "A"
	WeightB TSWeight = "B"
	WeightC TSWeight = "C"
	WeightD TSWeight = "D"
)

// SetWeight labels every lexeme of a vector, which is how a title is made to
// count for more than a body once the two are combined.
//
//	orm.Concat2TSVector(
//	    orm.SetWeight(orm.ToTSVector(orm.English, Posts.Title), orm.WeightA),
//	    orm.SetWeight(orm.ToTSVector(orm.English, Posts.Body), orm.WeightB),
//	)
func SetWeight[E any](v Selectable[E, TSVector], w TSWeight) Expression[TSVector, *TSVector] {
	return Expression[TSVector, *TSVector]{node: expr.Call{
		Func: "setweight", Args: []expr.Node{v.selectItem().Node, expr.Arg{Value: string(w)}},
	}}
}

// Concat2TSVector combines two vectors with PostgreSQL's ||, keeping the
// weights each already carries and shifting the second one's positions.
func Concat2TSVector[A, B any](a Selectable[A, TSVector], b Selectable[B, TSVector]) Expression[TSVector, *TSVector] {
	return Expression[TSVector, *TSVector]{node: expr.Infix{
		Op: "||", Left: a.selectItem().Node, Right: b.selectItem().Node,
	}}
}

// The tsquery combinators.
//
// PostgreSQL composes queries with operators on the parsed values, so the logic
// happens in the server rather than in Go string concatenation — which is what
// keeps precedence, stemming and phrase distance the server's business.

// AndTSQuery builds a && b: both must match.
func AndTSQuery[A, B any](a Selectable[A, TSQuery], b Selectable[B, TSQuery]) Expression[TSQuery, *TSQuery] {
	return tsCombine("&&", a, b)
}

// OrTSQuery builds a || b: either may match.
func OrTSQuery[A, B any](a Selectable[A, TSQuery], b Selectable[B, TSQuery]) Expression[TSQuery, *TSQuery] {
	return tsCombine("||", a, b)
}

// FollowedByTSQuery builds a <-> b: the two must match adjacent lexemes, in
// order.
func FollowedByTSQuery[A, B any](a Selectable[A, TSQuery], b Selectable[B, TSQuery]) Expression[TSQuery, *TSQuery] {
	return tsCombine("<->", a, b)
}

// NotTSQuery builds !!a: the query must not match.
func NotTSQuery[A any](a Selectable[A, TSQuery]) Expression[TSQuery, *TSQuery] {
	return Expression[TSQuery, *TSQuery]{node: expr.Prefix{
		Op: "!!", X: a.selectItem().Node,
	}}
}

func tsCombine[A, B any](op string, a Selectable[A, TSQuery], b Selectable[B, TSQuery]) Expression[TSQuery, *TSQuery] {
	return Expression[TSQuery, *TSQuery]{node: expr.Infix{
		Op: op, Left: a.selectItem().Node, Right: b.selectItem().Node,
	}}
}

// TSQueryText is the text form of a parsed query, which is what a diagnostic
// wants: it shows how PostgreSQL read the search text.
func TSQueryText[E any](q Selectable[E, TSQuery]) Expression[string, *string] {
	return Expression[string, *string]{node: expr.Cast{
		X: q.selectItem().Node, Type: "text",
	}}
}

// String renders the weight, so that an invalid one is visible in a message
// rather than in a server error.
func (w TSWeight) String() string { return string(w) }

// Valid reports whether the weight is one PostgreSQL defines.
func (w TSWeight) Valid() bool {
	switch w {
	case WeightA, WeightB, WeightC, WeightD:
		return true
	default:
		return false
	}
}
