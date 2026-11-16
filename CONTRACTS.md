# Package contracts

Shared types live in `internal/model` (do not change without coordination).
Every package below exposes exactly these entry points (extra helpers are fine).
Postgres driver: `github.com/jackc/pgx/v5` (`pgxpool`). Go module: `github.com/pg-rehearse/pg-rehearse`.

## internal/profile — read production statistics (read-only, no customer rows)
```go
type Options struct {
    SampleInterval  time.Duration // gap between two pg_stat_statements snapshots (rates = delta/interval)
    TopQueries      int           // keep top-N by calls
    SensitiveCols   []string      // extra "table.column" or "column" names to treat as PII
    FeatureMap      map[string]string // regex on query text -> feature label, e.g. "orders" -> "checkout"
    LongTxnMinSec   float64       // pg_stat_activity xacts older than this count as long
}
func Collect(ctx context.Context, pool *pgxpool.Pool, opts Options) (*model.Profile, error)
func Save(path string, p *model.Profile) error
func Load(path string) (*model.Profile, error)
func IsSensitive(table, column string, extra []string) bool
```

## internal/synth — build a prod-shaped shadow database from a Profile
```go
type Options struct {
    Scale float64 // 1.0 = full production row counts; 0.01 = 1%
    Seed  int64
    BatchRows int
}
// Build applies p.SchemaSQL to the (empty) target database and fills every table with
// synthetic rows matching row counts and column distributions. Must respect FK order.
func Build(ctx context.Context, pool *pgxpool.Pool, p *model.Profile, opts Options) error
```

## internal/locks — what does a migration lock, and is it risky?
```go
// Analyze runs each statement of migrationSQL inside BEGIN ... ROLLBACK on the shadow DB,
// records pg_locks held by the session and detects table rewrites (relfilenode change).
// Statements that cannot run in a transaction (CREATE INDEX CONCURRENTLY) are analyzed statically.
func Analyze(ctx context.Context, pool *pgxpool.Pool, migrationSQL string) (*model.LockFootprint, error)
func Lint(migrationSQL string) []model.LintFinding
func SplitStatements(sql string) []string
```

## internal/fixer — rewrite risky migrations into safer equivalents
```go
func Suggest(migrationSQL string, fp *model.LockFootprint) []model.Fix
// Apply returns the full rewritten migration with all fixes applied.
func Apply(migrationSQL string, fixes []model.Fix) string
```

## internal/load — replay realistic traffic against the shadow DB
```go
type Config struct {
    Queries        []model.QueryProfile
    LongTxns       []model.LongTxn
    RateMultiplier float64 // 1.0 = production rate
    MaxConns       int
    LongTxnStartAfter time.Duration // start long txns this long after Run begins
}
type Runner struct { /* ... */ }
func NewRunner(pool *pgxpool.Pool, cfg Config) (*Runner, error) // samples real param values from the shadow DB
// Run generates load until ctx is cancelled and returns per-query stats + raw samples.
func (r *Runner) Run(ctx context.Context) (*model.LoadResult, error)
```

## internal/report — render results
```go
func Markdown(r *model.Report) string // the PR comment
func JSON(r *model.Report) ([]byte, error)
```

## internal/rehearse — orchestrator (owned by lead)
```go
func Run(ctx context.Context, opts Options) (*model.Report, error)
```
