// Package model holds the data types shared by every pg-rehearse component.
// It is the contract between packages: change it only in coordination.
package model

import "time"

// ---------- Profile: privacy-safe statistics captured from production ----------

// Profile is a statistical snapshot of a production database. It must never
// contain customer rows. Values copied from pg_stats are only kept for columns
// that are NOT marked Sensitive.
type Profile struct {
	Version       int            `json:"version"`
	CapturedAt    time.Time      `json:"captured_at"`
	ServerVersion string         `json:"server_version"`
	SchemaSQL     string         `json:"schema_sql"` // schema-only DDL (tables, indexes, constraints, views)
	Tables        []TableProfile `json:"tables"`
	Queries       []QueryProfile `json:"queries"`
	LongTxns      []LongTxn      `json:"long_txns"`
}

type TableProfile struct {
	Schema      string          `json:"schema"`
	Name        string          `json:"name"`
	RowEstimate int64           `json:"row_estimate"`
	TotalBytes  int64           `json:"total_bytes"`
	Columns     []ColumnProfile `json:"columns"`
}

// QualifiedName returns schema.name.
func (t TableProfile) QualifiedName() string { return t.Schema + "." + t.Name }

type ColumnProfile struct {
	Name     string  `json:"name"`
	DataType string  `json:"data_type"` // format_type() output, e.g. "bigint", "text", "timestamp with time zone"
	Nullable bool    `json:"nullable"`
	NullFrac float64 `json:"null_frac"`
	// NDistinct follows pg_stats semantics: >0 absolute count, <0 negative fraction of rows, 0 unknown.
	NDistinct   float64 `json:"n_distinct"`
	AvgWidth    int     `json:"avg_width"`
	Correlation float64 `json:"correlation"`
	// Sensitive columns (PII heuristics or user config) never carry the value arrays below.
	Sensitive       bool      `json:"sensitive"`
	MostCommonVals  []string  `json:"most_common_vals,omitempty"`
	MostCommonFreqs []float64 `json:"most_common_freqs,omitempty"`
	HistogramBounds []string  `json:"histogram_bounds,omitempty"`
}

// QueryProfile is one normalized statement from pg_stat_statements ($1, $2 placeholders).
type QueryProfile struct {
	QueryID     int64    `json:"query_id"`
	Query       string   `json:"query"`
	CallsPerSec float64  `json:"calls_per_sec"`
	MeanMs      float64  `json:"mean_ms"`
	Tables      []string `json:"tables"`  // schema-qualified relations the query touches (best effort)
	Feature     string   `json:"feature"` // human label, e.g. "checkout"; from config or "" if unknown
}

// LongTxn describes a recurring long-running transaction (reports, batch jobs)
// observed in pg_stat_activity. These cause lock-queue outages.
type LongTxn struct {
	Query       string   `json:"query"`
	DurationSec float64  `json:"duration_sec"`
	Tables      []string `json:"tables"`
	PerHour     float64  `json:"per_hour"`
}

// ---------- Lock analysis ----------

type LockFootprint struct {
	Statements      []StatementLocks `json:"statements"`
	RewrittenTables []string         `json:"rewritten_tables"` // relfilenode changed => full table rewrite
	Lint            []LintFinding    `json:"lint"`
}

type StatementLocks struct {
	SQL        string         `json:"sql"`
	DurationMs float64        `json:"duration_ms"` // on an idle shadow DB
	Locks      []RelationLock `json:"locks"`
}

type RelationLock struct {
	Relation     string `json:"relation"` // schema-qualified
	Kind         string `json:"kind"`     // "table", "index", "view", "sequence", "other"
	Mode         string `json:"mode"`     // e.g. "AccessExclusiveLock"
	BlocksReads  bool   `json:"blocks_reads"`
	BlocksWrites bool   `json:"blocks_writes"`
}

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityDanger  Severity = "danger"
)

type LintFinding struct {
	Rule       string   `json:"rule"` // stable id, e.g. "create-index-not-concurrent"
	Severity   Severity `json:"severity"`
	Statement  string   `json:"statement"`
	Message    string   `json:"message"`
	Suggestion string   `json:"suggestion,omitempty"`
}

// ---------- Load generation ----------

type QueryStats struct {
	QueryID int64   `json:"query_id"`
	Query   string  `json:"query"`
	Feature string  `json:"feature"`
	Count   int     `json:"count"`
	Errors  int     `json:"errors"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
	MaxMs   float64 `json:"max_ms"`
}

// Sample is one executed query. Kept so the orchestrator can slice latency by time window.
type Sample struct {
	QueryID int64         `json:"query_id"`
	Start   time.Time     `json:"start"`
	Latency time.Duration `json:"latency"`
	Err     string        `json:"err,omitempty"`
}

type LoadResult struct {
	Start    time.Time    `json:"start"`
	End      time.Time    `json:"end"`
	PerQuery []QueryStats `json:"per_query"`
	Samples  []Sample     `json:"-"`
}

// ---------- Rehearsal result ----------

type Verdict string

const (
	VerdictSafe      Verdict = "safe"
	VerdictRisky     Verdict = "risky"
	VerdictDangerous Verdict = "dangerous"
)

// Impact is how one query (feature) behaved while the migration ran, vs. baseline.
type Impact struct {
	QueryID       int64   `json:"query_id"`
	Query         string  `json:"query"`
	Feature       string  `json:"feature"`
	BaselineP99Ms float64 `json:"baseline_p99_ms"`
	DuringMaxMs   float64 `json:"during_max_ms"`
	StallMs       float64 `json:"stall_ms"` // longest continuous window where requests were blocked
	Errors        int     `json:"errors"`
}

type RehearsalResult struct {
	Migration           string        `json:"migration"`
	Footprint           LockFootprint `json:"footprint"`
	MigrationDurationMs float64       `json:"migration_duration_ms"`
	MigrationError      string        `json:"migration_error,omitempty"`
	Impacts             []Impact      `json:"impacts"`
	MaxStallMs          float64       `json:"max_stall_ms"`
	Verdict             Verdict       `json:"verdict"`
}

// Fix is a rewritten, safer form of a migration.
type Fix struct {
	Rule        string `json:"rule"`
	Description string `json:"description"`
	Original    string `json:"original"`
	Rewritten   string `json:"rewritten"`
}

// Report is everything the PR comment needs.
type Report struct {
	Original    RehearsalResult  `json:"original"`
	Fixes       []Fix            `json:"fixes"`
	FixedResult *RehearsalResult `json:"fixed_result,omitempty"`
	TrafficNote string           `json:"traffic_note"` // e.g. "peak-hour profile, 1,240 req/s, 1 long report txn"
}
