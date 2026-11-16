package profile

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

const longTxnPollEvery = 250 * time.Millisecond

type txnKey struct {
	pid   int32
	start time.Time
}

type txnObs struct {
	query    string
	maxAge   float64
	tables   map[string]bool
	firstAge float64
}

// longTxnObserver polls pg_stat_activity for other client backends whose
// transaction is older than minSec.
type longTxnObserver struct {
	minSec float64
	seen   map[txnKey]*txnObs
}

func newLongTxnObserver(minSec float64) *longTxnObserver {
	return &longTxnObserver{minSec: minSec, seen: map[txnKey]*txnObs{}}
}

// run polls until ctx is done. Errors from individual polls are ignored
// (best effort); the window length is measured by the caller.
func (o *longTxnObserver) run(ctx context.Context, pool *pgxpool.Pool) {
	t := time.NewTicker(longTxnPollEvery)
	defer t.Stop()
	for {
		o.poll(ctx, pool)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (o *longTxnObserver) poll(ctx context.Context, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx, `/* pg-rehearse:profile */
SELECT a.pid, a.xact_start, coalesce(a.query, ''),
  extract(epoch FROM (pg_catalog.clock_timestamp() - a.xact_start))::float8,
  coalesce((SELECT array_agg(DISTINCT pg_catalog.format('%I.%I', n.nspname, c.relname))
            FROM pg_catalog.pg_locks l
            JOIN pg_catalog.pg_class c ON c.oid = l.relation
            JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
            WHERE l.pid = a.pid AND l.locktype = 'relation' AND l.granted
              AND c.relkind IN ('r','p','v','m')
              AND n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')), '{}')
FROM pg_catalog.pg_stat_activity a
WHERE a.pid <> pg_catalog.pg_backend_pid()
  AND a.datname = pg_catalog.current_database()
  AND a.backend_type = 'client backend'
  AND a.xact_start IS NOT NULL
  AND pg_catalog.clock_timestamp() - a.xact_start > $1 * interval '1 second'
  AND coalesce(a.query, '') NOT LIKE '%pg-rehearse:%'`, o.minSec)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k txnKey
		var q string
		var age float64
		var tables []string
		if err := rows.Scan(&k.pid, &k.start, &q, &age, &tables); err != nil {
			return
		}
		obs := o.seen[k]
		if obs == nil {
			obs = &txnObs{tables: map[string]bool{}, firstAge: age}
			o.seen[k] = obs
		}
		if q != "" {
			obs.query = q
		}
		if age > obs.maxAge {
			obs.maxAge = age
		}
		for _, t := range tables {
			obs.tables[t] = true
		}
	}
}

// summarize groups observed transactions by normalized query text.
//
// PerHour estimate: a transaction of total duration D is visible to a sampler
// watching a window of W seconds (only once older than M = minSec) iff it
// starts within an interval of length W + D - M. With N distinct transactions
// of a group observed, arrival rate λ ≈ N / (W + D - M), so PerHour = 3600·λ.
// D is the longest age observed (a lower bound if still running at the end of
// the window), and the denominator is floored at W. With short windows the
// estimate is coarse (N is usually 1); collect over longer intervals for
// better numbers.
func (o *longTxnObserver) summarize(windowSec float64, res *relationResolver) []model.LongTxn {
	type group struct {
		query  string
		n      int
		maxAge float64
		tables map[string]bool
	}
	groups := map[string]*group{}
	var order []string
	for _, obs := range o.seen {
		key := normalizeForGrouping(obs.query)
		g := groups[key]
		if g == nil {
			g = &group{query: obs.query, tables: map[string]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		g.n++
		if obs.maxAge > g.maxAge {
			g.maxAge = obs.maxAge
		}
		for t := range obs.tables {
			g.tables[t] = true
		}
		for _, t := range res.resolve(obs.query) {
			g.tables[t] = true
		}
	}
	out := make([]model.LongTxn, 0, len(groups))
	for _, key := range order {
		g := groups[key]
		denom := windowSec + g.maxAge - o.minSec
		if denom < windowSec {
			denom = windowSec
		}
		if denom <= 0 {
			denom = 1
		}
		tables := make([]string, 0, len(g.tables))
		for t := range g.tables {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		out = append(out, model.LongTxn{
			Query:       g.query,
			DurationSec: g.maxAge,
			Tables:      tables,
			PerHour:     3600 * float64(g.n) / denom,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DurationSec > out[j].DurationSec })
	return out
}
