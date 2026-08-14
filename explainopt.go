package orm

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// EXPLAIN, and what the connected server can be asked for.
//
// EXPLAIN's option list has grown with every release: GENERIC_PLAN arrived in
// PostgreSQL 16, SERIALIZE and MEMORY in 17. Sending one to a server that does
// not have it is a syntax error, and this package supports 14 upwards — so the
// options a caller asks for are checked against the server before the statement
// is built, and refused with a message naming the version rather than passed
// through to become "syntax error at or near".
//
// Two combinations PostgreSQL itself refuses are also refused here: GENERIC_PLAN
// with ANALYZE, and the options that only mean something while a statement runs
// asked for without running one. Catching those before the round trip is not
// about saving the round trip; it is about the error saying which two options
// disagreed.

// ExplainOption asks EXPLAIN for something.
//
// The options are values rather than a struct so that the set can grow without
// changing a signature, and so that an option a server does not have can carry
// its own version requirement.
type ExplainOption interface {
	applyExplain(*explainConfig) error
}

// explainConfig accumulates what EXPLAIN was asked for.
type explainConfig struct {
	analyze   bool
	verbose   bool
	costs     *bool
	settings  bool
	buffers   *bool
	wal       bool
	timing    *bool
	summary   *bool
	generic   bool
	serialize string
	memory    bool
}

type explainOptFunc struct {
	name  string
	apply func(*explainConfig) error
}

func (o explainOptFunc) applyExplain(c *explainConfig) error { return o.apply(c) }

// ExplainVerbose asks for the output columns of every node and the schema
// qualification of every relation.
var ExplainVerbose ExplainOption = explainOptFunc{"VERBOSE", func(c *explainConfig) error {
	c.verbose = true
	return nil
}}

// ExplainCosts turns the planner's estimates on or off.
//
// They are on by default. Turning them off is what makes a plan comparable
// across machines, which is why a test suite sometimes wants it.
func ExplainCosts(on bool) ExplainOption {
	return explainOptFunc{"COSTS", func(c *explainConfig) error {
		c.costs = &on
		return nil
	}}
}

// ExplainSettings asks for the planner settings that differ from their defaults.
//
// They are context for a plan — a plan made with enable_seqscan off is not the
// plan the same query gets elsewhere — and nothing in this package changes them.
var ExplainSettings ExplainOption = explainOptFunc{"SETTINGS", func(c *explainConfig) error {
	c.settings = true
	return nil
}}

// ExplainBuffers asks for buffer accounting.
//
// Without ANALYZE it reports the planner's own buffer use, which is available
// from PostgreSQL 13 onwards and is usually not what somebody wants; with
// ANALYZE it reports each node's.
func ExplainBuffers(on bool) ExplainOption {
	return explainOptFunc{"BUFFERS", func(c *explainConfig) error {
		c.buffers = &on
		return nil
	}}
}

// ExplainWAL asks for write-ahead-log accounting, which needs ANALYZE.
var ExplainWAL ExplainOption = explainOptFunc{"WAL", func(c *explainConfig) error {
	c.wal = true
	return nil
}}

// ExplainTiming turns per-node timing on or off, which needs ANALYZE.
//
// Turning it off is worth doing when the timing itself is expensive, which on
// some kernels it is: the row counts are still collected.
func ExplainTiming(on bool) ExplainOption {
	return explainOptFunc{"TIMING", func(c *explainConfig) error {
		c.timing = &on
		return nil
	}}
}

// ExplainSummary turns the planning and execution time summary on or off.
//
// It is on for ANALYZE and off otherwise, which is PostgreSQL's default.
func ExplainSummary(on bool) ExplainOption {
	return explainOptFunc{"SUMMARY", func(c *explainConfig) error {
		c.summary = &on
		return nil
	}}
}

// ExplainGenericPlan asks for the plan PostgreSQL would make without knowing
// the parameter values.
//
// It is a different plan from the one a statement actually gets. PostgreSQL
// plans a parameterised statement against the values on the first executions and
// may switch to a generic plan later, so the two are both real and neither is
// the other — a report that showed one and called it the plan would be wrong
// about which.
//
// It requires PostgreSQL 16 and cannot be combined with ANALYZE, because there
// is nothing to execute a plan with no parameter values against.
var ExplainGenericPlan ExplainOption = explainOptFunc{"GENERIC_PLAN", func(c *explainConfig) error {
	c.generic = true
	return nil
}}

// ExplainMemory asks for the planner's memory use, which requires PostgreSQL 17.
var ExplainMemory ExplainOption = explainOptFunc{"MEMORY", func(c *explainConfig) error {
	c.memory = true
	return nil
}}

// ExplainSerialize asks what it costs to convert the result rows into their wire
// form, which requires PostgreSQL 17 and ANALYZE.
//
// mode is "text" or "binary"; "none" is the default and means not to measure.
func ExplainSerialize(mode string) ExplainOption {
	return explainOptFunc{"SERIALIZE", func(c *explainConfig) error {
		switch strings.ToLower(mode) {
		case "none", "text", "binary":
			c.serialize = strings.ToUpper(mode)
			return nil
		default:
			return fmt.Errorf("orm: SERIALIZE takes none, text or binary, and %q is none of them", mode)
		}
	}}
}

// ExplainCapabilities is what the connected server's EXPLAIN accepts.
//
// It exists so that the version comparisons happen once, in one place, against
// the server actually connected — rather than scattered through the code as
// guesses about what is deployed.
type ExplainCapabilities struct {
	// Version is the server's version number as PostgreSQL reports it:
	// 140012 for 14.12, 170005 for 17.5.
	Version int

	// GenericPlan is PostgreSQL 16 and later.
	GenericPlan bool
	// Serialize and Memory are PostgreSQL 17 and later.
	Serialize bool
	Memory    bool
	// WAL is PostgreSQL 13 and later, so it is true on everything supported.
	WAL bool
}

// Major returns the server's major version.
func (c ExplainCapabilities) Major() int { return c.Version / 10000 }

// String renders the version the way a person writes it.
func (c ExplainCapabilities) String() string {
	if c.Version == 0 {
		return "unknown"
	}
	major := c.Version / 10000
	minor := c.Version % 10000
	return fmt.Sprintf("%d.%d", major, minor)
}

func capabilitiesFor(version int) ExplainCapabilities {
	return ExplainCapabilities{
		Version:     version,
		GenericPlan: version >= 160000,
		Serialize:   version >= 170000,
		Memory:      version >= 170000,
		WAL:         version >= 130000,
	}
}

// Capabilities asks the connected server what its EXPLAIN accepts.
//
// The answer is cached per executor, because server_version_num does not change
// under a connection and asking on every EXPLAIN would double the round trips.
func Capabilities(ctx context.Context, ex Executor) (ExplainCapabilities, error) {
	if ex == nil {
		return ExplainCapabilities{}, fmt.Errorf("orm: no executor")
	}
	if c, ok := capabilityCache.Load(cacheKey(ex)); ok {
		return c.(ExplainCapabilities), nil
	}
	rows, err := ex.Query(ctx, `SELECT current_setting('server_version_num')::int`)
	if err != nil {
		return ExplainCapabilities{}, fmt.Errorf("orm: reading the server version: %w", err)
	}
	defer rows.Close()
	var version int
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ExplainCapabilities{}, fmt.Errorf("orm: reading the server version: %w", err)
		}
		return ExplainCapabilities{}, fmt.Errorf("orm: the server reported no version")
	}
	if err := rows.Scan(&version); err != nil {
		return ExplainCapabilities{}, fmt.Errorf("orm: reading the server version: %w", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ExplainCapabilities{}, fmt.Errorf("orm: reading the server version: %w", err)
	}
	c := capabilitiesFor(version)
	capabilityCache.Store(cacheKey(ex), c)
	return c, nil
}

// capabilityCache remembers each executor's server version.
//
// The key is the executor's identity, which for a pool is the pool. A pool whose
// connections reached two different servers would be a pool nobody could reason
// about anyway.
var capabilityCache sync.Map

func cacheKey(ex Executor) any { return ex }

// validate checks the requested options against each other and against the
// server, and returns the option list EXPLAIN should be given.
func (c *explainConfig) validate(caps ExplainCapabilities) ([]string, error) {
	// The combinations PostgreSQL itself refuses, refused here so the message
	// names the two options rather than a position in a string.
	if c.generic && c.analyze {
		return nil, fmt.Errorf("orm: GENERIC_PLAN asks for the plan PostgreSQL would make" +
			" without the parameter values, and ANALYZE runs the statement with them;" +
			" they cannot both be asked for")
	}
	if c.wal && !c.analyze {
		return nil, fmt.Errorf("orm: WAL reports what a statement wrote," +
			" so it needs ANALYZE — use ExplainAnalyze, which runs the statement")
	}
	if c.timing != nil && !c.analyze {
		return nil, fmt.Errorf("orm: TIMING measures a statement running," +
			" so it needs ANALYZE — use ExplainAnalyze, which runs the statement")
	}
	if c.serialize != "" && c.serialize != "NONE" && !c.analyze {
		return nil, fmt.Errorf("orm: SERIALIZE measures converting result rows to their wire form," +
			" so it needs ANALYZE — use ExplainAnalyze, which runs the statement")
	}

	// The options the connected server does not have.
	if c.generic && !caps.GenericPlan {
		return nil, &CapabilityError{Option: "GENERIC_PLAN", Needs: 16, Have: caps}
	}
	if c.serialize != "" && c.serialize != "NONE" && !caps.Serialize {
		return nil, &CapabilityError{Option: "SERIALIZE", Needs: 17, Have: caps}
	}
	if c.memory && !caps.Memory {
		return nil, &CapabilityError{Option: "MEMORY", Needs: 17, Have: caps}
	}
	if c.wal && !caps.WAL {
		return nil, &CapabilityError{Option: "WAL", Needs: 13, Have: caps}
	}

	// FORMAT JSON is not optional. The text format is written for people and
	// rearranged between releases; the JSON format is the one with a contract,
	// and it is the only one this package reads.
	opts := []string{"FORMAT JSON"}
	if c.analyze {
		opts = append(opts, "ANALYZE true")
	}
	if c.verbose {
		opts = append(opts, "VERBOSE true")
	}
	if c.costs != nil {
		opts = append(opts, "COSTS "+boolText(*c.costs))
	}
	if c.settings {
		opts = append(opts, "SETTINGS true")
	}
	if c.buffers != nil {
		opts = append(opts, "BUFFERS "+boolText(*c.buffers))
	}
	if c.wal {
		opts = append(opts, "WAL true")
	}
	if c.timing != nil {
		opts = append(opts, "TIMING "+boolText(*c.timing))
	}
	if c.summary != nil {
		opts = append(opts, "SUMMARY "+boolText(*c.summary))
	}
	if c.generic {
		opts = append(opts, "GENERIC_PLAN true")
	}
	if c.memory {
		opts = append(opts, "MEMORY true")
	}
	if c.serialize != "" {
		opts = append(opts, "SERIALIZE "+c.serialize)
	}
	return opts, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// CapabilityError reports an EXPLAIN option the connected server does not have.
//
// It is a typed error rather than a string so that a caller can degrade — ask
// for MEMORY, catch this, ask again without it — without matching on a message.
type CapabilityError struct {
	// Option is the EXPLAIN option that was asked for.
	Option string
	// Needs is the PostgreSQL major version that introduced it.
	Needs int
	// Have is what the connected server has.
	Have ExplainCapabilities
}

// Error names the EXPLAIN option that was asked for and the server version that
// does not have it.
func (e *CapabilityError) Error() string {
	return fmt.Sprintf("orm: EXPLAIN %s needs PostgreSQL %d and this server is %s",
		e.Option, e.Needs, e.Have)
}
