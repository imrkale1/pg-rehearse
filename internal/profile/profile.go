// Package profile captures a privacy-safe statistical profile of a production
// Postgres database: schema DDL (generated from the catalogs, no pg_dump),
// table/column statistics from pg_stats, query rates from pg_stat_statements
// and recurring long transactions from pg_stat_activity. It never reads table
// rows.
//
// Requires PostgreSQL 13+ and the pg_stat_statements extension.
package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// ProfileVersion is the value written to model.Profile.Version.
const ProfileVersion = 1

// Defaults applied for zero-valued Options fields.
const (
	DefaultSampleInterval = 10 * time.Second
	DefaultTopQueries     = 50
	DefaultLongTxnMinSec  = 5.0
)

type Options struct {
	SampleInterval time.Duration     // gap between two pg_stat_statements snapshots (rates = delta/interval)
	TopQueries     int               // keep top-N by calls/sec
	SensitiveCols  []string          // extra "table.column" or "column" names to treat as PII
	FeatureMap     map[string]string // regex (case-insensitive) on query text -> feature label
	LongTxnMinSec  float64           // pg_stat_activity xacts older than this count as long
}

func (o Options) withDefaults() Options {
	if o.SampleInterval <= 0 {
		o.SampleInterval = DefaultSampleInterval
	}
	if o.TopQueries <= 0 {
		o.TopQueries = DefaultTopQueries
	}
	if o.LongTxnMinSec <= 0 {
		o.LongTxnMinSec = DefaultLongTxnMinSec
	}
	return o
}

// Collect builds a Profile. It blocks for about opts.SampleInterval while it
// samples pg_stat_statements and pg_stat_activity.
func Collect(ctx context.Context, pool *pgxpool.Pool, opts Options) (*model.Profile, error) {
	opts = opts.withDefaults()
	rules, err := compileFeatureMap(opts.FeatureMap)
	if err != nil {
		return nil, err
	}

	p := &model.Profile{Version: ProfileVersion, CapturedAt: time.Now().UTC()}
	if err := pool.QueryRow(ctx, "/* pg-rehearse:profile */ SHOW server_version").Scan(&p.ServerVersion); err != nil {
		return nil, fmt.Errorf("server version: %w", err)
	}

	relNames, err := readCatalog(ctx, pool, p, opts)
	if err != nil {
		return nil, err
	}
	res := newRelationResolver(relNames)

	pssSchema, err := checkPgStatStatements(ctx, pool)
	if err != nil {
		return nil, err
	}
	first, err := snapshotStatements(ctx, pool, pssSchema)
	if err != nil {
		return nil, err
	}

	obs := newLongTxnObserver(opts.LongTxnMinSec)
	pollCtx, stopPoll := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); obs.run(pollCtx, pool) }()

	timer := time.NewTimer(opts.SampleInterval)
	select {
	case <-ctx.Done():
		timer.Stop()
		stopPoll()
		wg.Wait()
		return nil, ctx.Err()
	case <-timer.C:
	}
	stopPoll()
	wg.Wait()

	second, err := snapshotStatements(ctx, pool, pssSchema)
	if err != nil {
		return nil, err
	}
	p.Queries = diffSnapshots(first, second, opts.TopQueries, rules, res)
	p.LongTxns = obs.summarize(second.at.Sub(first.at).Seconds(), res)
	if p.Queries == nil {
		p.Queries = []model.QueryProfile{}
	}
	return p, nil
}

// readCatalog fills SchemaSQL and Tables from one consistent read-only
// snapshot, and returns all relation names usable for query resolution.
func readCatalog(ctx context.Context, pool *pgxpool.Pool, p *model.Profile, opts Options) ([]string, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Make every name printed by format_type / pg_get_*def schema-qualified.
	if _, err := tx.Exec(ctx, "/* pg-rehearse:profile */ SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, err
	}
	if p.SchemaSQL, err = generateSchemaSQL(ctx, tx); err != nil {
		return nil, err
	}
	if p.Tables, err = collectTables(ctx, tx); err != nil {
		return nil, err
	}
	for i := range p.Tables {
		applyPrivacy(&p.Tables[i], opts.SensitiveCols)
	}
	rows, err := tx.Query(ctx, `/* pg-rehearse:profile */
SELECT format('%I.%I', n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m','f') AND `+userNamespaceFilter+` AND `+notExtensionMember("c.oid", "pg_class"))
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// format('%I') quotes mixed-case names; the resolver compares unquoted forms.
	for i, n := range names {
		names[i] = unquoteQualified(n)
	}
	return names, tx.Commit(ctx)
}

// unquoteQualified turns `"MySchema"."T"` into `MySchema.T` (as the lexer
// would produce for quoted identifiers).
func unquoteQualified(s string) string {
	toks := lexSQL(s)
	out := ""
	for _, t := range toks {
		if t.text == "." {
			out += "."
		} else {
			out += t.text
		}
	}
	return out
}

// Save writes p as indented JSON.
func Save(path string, p *model.Profile) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load reads a profile written by Save.
func Load(path string) (*model.Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p model.Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}
	return &p, nil
}
