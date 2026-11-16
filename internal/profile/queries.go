package profile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// ErrNoPgStatStatements is returned (wrapped) when pg_stat_statements is not usable.
var ErrNoPgStatStatements = errors.New("pg_stat_statements is not available")

type stmtCounter struct {
	query   string
	calls   int64
	totalMs float64
}

type stmtSnapshot struct {
	at    time.Time
	stats map[int64]stmtCounter
}

// checkPgStatStatements returns the quoted schema the extension lives in.
func checkPgStatStatements(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var schemas []string
	if err := pool.QueryRow(ctx, `/* pg-rehearse:profile */
SELECT coalesce(array_agg(pg_catalog.quote_ident(n.nspname)), '{}') FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_stat_statements'`).Scan(&schemas); err != nil {
		return "", fmt.Errorf("check pg_stat_statements: %w", err)
	}
	if len(schemas) == 0 {
		return "", fmt.Errorf("%w: run `CREATE EXTENSION pg_stat_statements;` in this database "+
			"(the server must also have shared_preload_libraries = 'pg_stat_statements', which requires a restart)",
			ErrNoPgStatStatements)
	}
	return schemas[0], nil
}

// snapshotStatements reads cumulative counters for the current database,
// summed per queryid across users.
func snapshotStatements(ctx context.Context, pool *pgxpool.Pool, schema string) (*stmtSnapshot, error) {
	rows, err := pool.Query(ctx, `/* pg-rehearse:profile */
SELECT s.queryid, s.query, s.calls, s.total_exec_time
FROM `+schema+`.pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
  AND s.queryid IS NOT NULL AND s.query IS NOT NULL`)
	if err != nil {
		if strings.Contains(err.Error(), "shared_preload_libraries") {
			return nil, fmt.Errorf("%w: %v; add shared_preload_libraries = 'pg_stat_statements' to postgresql.conf, "+
				"restart the server, then run `CREATE EXTENSION pg_stat_statements;`", ErrNoPgStatStatements, err)
		}
		return nil, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	defer rows.Close()
	snap := &stmtSnapshot{at: time.Now(), stats: map[int64]stmtCounter{}}
	for rows.Next() {
		var id, calls int64
		var q string
		var total float64
		if err := rows.Scan(&id, &q, &calls, &total); err != nil {
			return nil, err
		}
		c := snap.stats[id]
		if c.query == "" {
			c.query = q
		}
		c.calls += calls
		c.totalMs += total
		snap.stats[id] = c
	}
	return snap, rows.Err()
}

type featureRule struct {
	re    *regexp.Regexp
	label string
}

// compileFeatureMap compiles FeatureMap patterns (case-insensitive), sorted by
// pattern for deterministic first-match-wins evaluation.
func compileFeatureMap(m map[string]string) ([]featureRule, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rules := make([]featureRule, 0, len(keys))
	for _, k := range keys {
		re, err := regexp.Compile("(?i)" + k)
		if err != nil {
			return nil, fmt.Errorf("feature map pattern %q: %w", k, err)
		}
		rules = append(rules, featureRule{re: re, label: m[k]})
	}
	return rules, nil
}

func featureFor(rules []featureRule, query string) string {
	for _, r := range rules {
		if r.re.MatchString(query) {
			return r.label
		}
	}
	return ""
}

// diffSnapshots turns two cumulative snapshots into per-second rates.
// Entries absent from the first snapshot (new, or evicted and re-added) count
// from zero; a counter that went backwards (pg_stat_statements_reset) is
// treated the same way.
func diffSnapshots(a, b *stmtSnapshot, topN int, rules []featureRule, res *relationResolver) []model.QueryProfile {
	elapsed := b.at.Sub(a.at).Seconds()
	if elapsed <= 0 {
		elapsed = 1e-3
	}
	var out []model.QueryProfile
	for id, cb := range b.stats {
		ca := a.stats[id]
		dCalls := cb.calls - ca.calls
		dTotal := cb.totalMs - ca.totalMs
		if dCalls < 0 || dTotal < 0 {
			dCalls, dTotal = cb.calls, cb.totalMs
		}
		if dCalls <= 0 || !isApplicationQuery(cb.query) {
			continue
		}
		out = append(out, model.QueryProfile{
			QueryID:     id,
			Query:       cb.query,
			CallsPerSec: float64(dCalls) / elapsed,
			MeanMs:      dTotal / float64(dCalls),
			Tables:      res.resolve(cb.query),
			Feature:     featureFor(rules, cb.query),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CallsPerSec != out[j].CallsPerSec {
			return out[i].CallsPerSec > out[j].CallsPerSec
		}
		return out[i].QueryID < out[j].QueryID
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}
