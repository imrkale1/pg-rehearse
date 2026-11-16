// Package load replays a production query mix against the shadow database.
//
// It is an open-loop driver: each query has an independent Poisson arrival
// process at its production rate, and arrivals are never delayed by slow
// responses. When the database stalls (for example an ALTER TABLE queued
// behind a long transaction), requests keep arriving and pile up exactly like
// real users would, and every request's latency is measured from its scheduled
// arrival time, so time spent waiting for a free connection is counted
// (no coordinated omission).
package load

import (
	"context"
	"errors"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Config controls a load run.
type Config struct {
	Queries           []model.QueryProfile
	LongTxns          []model.LongTxn
	RateMultiplier    float64       // 1.0 = production rate
	MaxConns          int           // size of the load pool (default 20)
	LongTxnStartAfter time.Duration // start long txns this long after Run begins

	// Optional knobs (zero values pick defaults).
	RequestTimeout time.Duration // per request, measured from scheduled arrival (default 30s)
	DrainTimeout   time.Duration // how long Run waits for in-flight requests after ctx ends (default 10s)
	MaxInFlight    int           // client-side backlog cap; beyond it arrivals are recorded as errors (default 100000)
	Seed           uint64        // RNG seed; 0 = time based
}

// SkippedQuery is a profile entry the runner refused to replay.
type SkippedQuery struct {
	QueryID int64
	Query   string
	Reason  string
}

// LongTxnResult describes what happened to one configured long transaction.
type LongTxnResult struct {
	Query string
	Mode  string // "query" (ran the recorded statement) or "tables" (SELECT over its tables)
	Start time.Time
	End   time.Time
	Err   string
}

type runQuery struct {
	prof  model.QueryProfile
	rate  float64 // arrivals per second
	gens  []gen
	rnd   *mrand.Rand
	mu    sync.Mutex
	samps []model.Sample
}

// Runner replays traffic. Create with NewRunner; Run may be called repeatedly but not concurrently.
type Runner struct {
	pool    *pgxpool.Pool
	cfg     Config
	queries []*runQuery
	skipped []SkippedQuery

	ltMu      sync.Mutex
	ltResults []LongTxnResult
}

// NewRunner infers parameter meaning for each query and samples real values from the shadow DB.
func NewRunner(pool *pgxpool.Pool, cfg Config) (*Runner, error) {
	if cfg.RateMultiplier <= 0 {
		cfg.RateMultiplier = 1
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 20
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 100000
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	r := &Runner{pool: pool, cfg: cfg}
	cat := newCatalog(pool)
	for i, qp := range cfg.Queries {
		if qp.CallsPerSec <= 0 {
			r.skip(qp, "zero call rate")
			continue
		}
		rnd := mrand.New(mrand.NewPCG(seed, uint64(i)+1))
		gens, reason, err := buildGens(ctx, pool, cat, rnd, qp.Query)
		if err != nil {
			return nil, fmt.Errorf("query %d: %w", qp.QueryID, err)
		}
		if reason != "" {
			r.skip(qp, reason)
			continue
		}
		rate := qp.CallsPerSec * cfg.RateMultiplier
		r.queries = append(r.queries, &runQuery{prof: qp, rate: rate, gens: gens, rnd: rnd})
	}
	return r, nil
}

func (r *Runner) skip(qp model.QueryProfile, reason string) {
	r.skipped = append(r.skipped, SkippedQuery{QueryID: qp.QueryID, Query: qp.Query, Reason: reason})
}

// Skipped lists queries that could not be parameterized confidently, with the reason.
func (r *Runner) Skipped() []SkippedQuery { return slices.Clone(r.skipped) }

// LongTxnResults reports what the long transactions of the last Run did.
func (r *Runner) LongTxnResults() []LongTxnResult {
	r.ltMu.Lock()
	defer r.ltMu.Unlock()
	return slices.Clone(r.ltResults)
}

// buildGens returns one generator per placeholder, or a non-empty skip reason.
// A non-nil error means the database itself failed (not the query).
func buildGens(ctx context.Context, pool *pgxpool.Pool, cat *catalog, rnd *mrand.Rand, sql string) ([]gen, string, error) {
	pq, err := parseQuery(sql)
	if err != nil {
		return nil, "unparseable SQL: " + err.Error(), nil
	}
	toks, _ := tokenize(sql)
	if len(toks) == 0 || toks[0].kind != tIdent {
		return nil, "empty or non-DML statement", nil
	}
	switch toks[0].text {
	case "select", "insert", "update", "delete", "with", "values", "table":
	default:
		return nil, "not a DML statement (" + strings.ToUpper(toks[0].text) + ")", nil
	}
	// Ask the server to parse it: validates tables exist and the param count.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, "", err
	}
	sd, perr := conn.Conn().PgConn().Prepare(ctx, "", sql, nil)
	conn.Release()
	if perr != nil {
		return nil, "server rejected query: " + perr.Error(), nil
	}
	if len(sd.ParamOIDs) != pq.MaxParam {
		return nil, fmt.Sprintf("server reports %d params, scanner found $%d", len(sd.ParamOIDs), pq.MaxParam), nil
	}

	// Resolve table references once.
	type rt struct {
		ref tableRef
		ti  *tableInfo
	}
	var tables []rt
	for _, tr := range pq.Tables {
		ti, err := cat.table(ctx, tr.Schema, tr.Name)
		if err != nil {
			return nil, "", err
		}
		if ti != nil {
			tables = append(tables, rt{tr, ti})
		}
	}
	var insertTI *tableInfo
	if pq.InsertTbl != nil {
		insertTI, _ = cat.table(ctx, pq.InsertTbl.Schema, pq.InsertTbl.Name)
	}
	resolve := func(p paramRef) (*tableInfo, *colInfo) {
		if p.Role == roleInsert && insertTI != nil {
			if p.InsertPos > 0 {
				if p.InsertPos <= len(insertTI.Cols) {
					return insertTI, &insertTI.Cols[p.InsertPos-1]
				}
				return nil, nil
			}
			if ci := insertTI.byName[p.Column]; ci != nil {
				return insertTI, ci
			}
		}
		for _, t := range tables {
			if p.Qualifier != "" && p.Qualifier != t.ref.Alias && !(t.ref.Alias == "" && p.Qualifier == t.ref.Name) {
				continue
			}
			if ci := t.ti.byName[p.Column]; ci != nil {
				return t.ti, ci
			}
		}
		return nil, nil
	}

	gens := make([]gen, pq.MaxParam)
	for i := 1; i <= pq.MaxParam; i++ {
		p, ok := pq.Params[i]
		if !ok {
			return nil, fmt.Sprintf("cannot infer meaning of $%d", i), nil
		}
		var g gen
		var reason string
		switch p.Kind {
		case pkLimit:
			choices := []int64{10, 20, 20, 25, 50, 100}
			g = func() any { return choices[rnd.IntN(len(choices))] }
		case pkOffset:
			g = func() any { return int64(rnd.IntN(6) * 20) }
		case pkCast:
			g, reason = castGen(ctx, cat, rnd, p.Cast, i)
		case pkColumn:
			ti, ci := resolve(p)
			if ci == nil {
				if p.Cast != "" {
					g, reason = castGen(ctx, cat, rnd, p.Cast, i)
					break
				}
				name := p.Column
				if p.Qualifier != "" {
					name = p.Qualifier + "." + name
				}
				if name == "" {
					name = fmt.Sprintf("insert position %d", p.InsertPos)
				}
				reason = fmt.Sprintf("$%d: cannot resolve column %s in catalog", i, name)
				break
			}
			g, reason, err = columnGen(ctx, cat, rnd, p, ti, ci)
			if err != nil {
				return nil, "", err
			}
		}
		if reason != "" {
			return nil, reason, nil
		}
		gens[i-1] = g
	}
	return gens, "", nil
}

func castGen(ctx context.Context, cat *catalog, rnd *mrand.Rand, cast string, i int) (gen, string) {
	tn, ok := castTypnames[cast]
	if !ok {
		return nil, fmt.Sprintf("$%d: only a cast to %q is known", i, cast)
	}
	g, err := cat.typedGen(ctx, rnd, tn, "", cast)
	if err != nil {
		return nil, fmt.Sprintf("$%d: %v", i, err)
	}
	return g, ""
}

func columnGen(ctx context.Context, cat *catalog, rnd *mrand.Rand, p paramRef, ti *tableInfo, ci *colInfo) (gen, string, error) {
	where := ti.Name + "." + ci.Name
	if (p.Role == roleInsert || p.Role == roleAssign) && ci.Unique {
		g, err := cat.freshGen(ctx, ti, ci)
		if err != nil {
			return nil, fmt.Sprintf("$%d (%s): %v", p.Index, where, err), nil
		}
		return g, "", nil
	}
	if p.Role == roleInsert && ci.FKTable != "" {
		parent, err := cat.tableByRegclass(ctx, ci.FKTable)
		if err != nil || parent == nil {
			return nil, fmt.Sprintf("$%d (%s): cannot resolve FK parent %s", p.Index, where, ci.FKTable), nil
		}
		vals, err := cat.sample(ctx, parent, ci.FKCol)
		if err != nil {
			return nil, "", err
		}
		if len(vals) == 0 {
			return nil, fmt.Sprintf("$%d (%s): FK parent %s is empty", p.Index, where, ci.FKTable), nil
		}
		return zipfGen(rnd, vals), "", nil
	}
	vals, err := cat.sample(ctx, ti, ci.Name)
	if err != nil {
		return nil, "", err
	}
	if len(vals) > 0 {
		if p.Role == roleLookup {
			return zipfGen(rnd, vals), "", nil
		}
		return uniformGen(rnd, vals), "", nil
	}
	g, err := cat.typedGen(ctx, rnd, ci.TypName, ci.TypCat, ci.Type)
	if err != nil {
		return nil, fmt.Sprintf("$%d (%s): %v", p.Index, where, err), nil
	}
	return g, "", nil
}

// Run generates load until ctx is cancelled and returns per-query stats + raw samples.
func (r *Runner) Run(ctx context.Context) (*model.LoadResult, error) {
	pcfg := r.pool.Config()
	pcfg.MaxConns = int32(r.cfg.MaxConns)
	pcfg.MinConns = 0
	lp, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("load pool: %w", err)
	}
	defer lp.Close()
	if err := warm(ctx, lp, r.cfg.MaxConns); err != nil {
		return nil, fmt.Errorf("warm load pool: %w", err)
	}

	r.ltMu.Lock()
	r.ltResults = nil
	r.ltMu.Unlock()
	for _, q := range r.queries {
		c := int(math.Min(q.rate*60, 1<<20))
		q.samps = make([]model.Sample, 0, max(c, 64))
	}

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	var schedWG, reqWG, ltWG sync.WaitGroup
	var inflight atomic.Int64
	start := time.Now()

	for _, q := range r.queries {
		schedWG.Add(1)
		go func() {
			defer schedWG.Done()
			r.schedule(ctx, drainCtx, lp, q, start, &reqWG, &inflight)
		}()
	}
	for _, lt := range r.cfg.LongTxns {
		ltWG.Add(1)
		go func() {
			defer ltWG.Done()
			r.runLongTxn(ctx, lt, start.Add(r.cfg.LongTxnStartAfter))
		}()
	}

	<-ctx.Done()
	end := time.Now()
	schedWG.Wait()
	done := make(chan struct{})
	go func() { reqWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(r.cfg.DrainTimeout):
		drainCancel()
		<-done
	}
	ltWG.Wait()

	res := &model.LoadResult{Start: start, End: end}
	total := 0
	for _, q := range r.queries {
		total += len(q.samps)
	}
	res.Samples = make([]model.Sample, 0, total)
	var lat []time.Duration
	for _, q := range r.queries {
		res.Samples = append(res.Samples, q.samps...)
		var st model.QueryStats
		st, lat = computeStats(q.prof, q.samps, lat)
		res.PerQuery = append(res.PerQuery, st)
	}
	slices.SortFunc(res.Samples, func(a, b model.Sample) int { return a.Start.Compare(b.Start) })
	return res, nil
}

func warm(ctx context.Context, lp *pgxpool.Pool, n int) error {
	conns := make([]*pgxpool.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := lp.Acquire(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				firstErr = err
				return
			}
			conns = append(conns, c)
		}()
	}
	wg.Wait()
	return firstErr
}

// schedule runs one query's Poisson arrival process until ctx ends.
func (r *Runner) schedule(ctx, drainCtx context.Context, lp *pgxpool.Pool, q *runQuery, start time.Time,
	reqWG *sync.WaitGroup, inflight *atomic.Int64) {
	meanGap := float64(time.Second) / q.rate
	next := start.Add(time.Duration(q.rnd.ExpFloat64() * meanGap))
	timer := time.NewTimer(0)
	<-timer.C
	maxIn := int64(r.cfg.MaxInFlight)
	for {
		if d := time.Until(next); d > 0 {
			timer.Reset(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return
		}
		now := time.Now()
		for !next.After(now) {
			sched := next
			next = next.Add(time.Duration(q.rnd.ExpFloat64() * meanGap))
			if inflight.Load() >= maxIn {
				q.record(model.Sample{QueryID: q.prof.QueryID, Start: sched, Latency: now.Sub(sched),
					Err: "client backlog overflow (MaxInFlight reached)"})
				continue
			}
			args := make([]any, len(q.gens))
			for i, g := range q.gens {
				args[i] = g()
			}
			inflight.Add(1)
			reqWG.Add(1)
			go func() {
				defer reqWG.Done()
				defer inflight.Add(-1)
				r.exec(drainCtx, lp, q, sched, args)
			}()
		}
	}
}

func (r *Runner) exec(drainCtx context.Context, lp *pgxpool.Pool, q *runQuery, sched time.Time, args []any) {
	reqCtx, cancel := context.WithDeadline(drainCtx, sched.Add(r.cfg.RequestTimeout))
	defer cancel()
	// A single statement in autocommit mode is its own short transaction.
	_, err := lp.Exec(reqCtx, q.prof.Query, args...)
	lat := time.Since(sched)
	s := model.Sample{QueryID: q.prof.QueryID, Start: sched, Latency: lat}
	if err != nil {
		switch {
		case errors.Is(reqCtx.Err(), context.DeadlineExceeded):
			s.Err = fmt.Sprintf("timeout after %s", r.cfg.RequestTimeout)
		case drainCtx.Err() != nil:
			s.Err = "unfinished when run ended"
		default:
			s.Err = err.Error()
		}
	}
	q.record(s)
}

func (q *runQuery) record(s model.Sample) {
	q.mu.Lock()
	q.samps = append(q.samps, s)
	q.mu.Unlock()
}

// runLongTxn opens a dedicated connection at startAt and holds a transaction
// (and therefore AccessShareLock on its tables) for lt.DurationSec.
func (r *Runner) runLongTxn(ctx context.Context, lt model.LongTxn, startAt time.Time) {
	res := LongTxnResult{Query: lt.Query}
	defer func() {
		res.End = time.Now()
		r.ltMu.Lock()
		r.ltResults = append(r.ltResults, res)
		r.ltMu.Unlock()
	}()
	select {
	case <-ctx.Done():
		res.Err = "run ended before start"
		return
	case <-time.After(time.Until(startAt)):
	}
	res.Start = time.Now()
	dur := time.Duration(lt.DurationSec * float64(time.Second))
	conn, err := pgx.ConnectConfig(ctx, r.pool.Config().ConnConfig.Copy())
	if err != nil {
		res.Err = "connect: " + err.Error()
		return
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(ctx)
	if err != nil {
		res.Err = err.Error()
		return
	}
	defer tx.Rollback(context.Background())

	ran := false
	if q := strings.TrimSpace(lt.Query); q != "" && !hasParams(q) {
		_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", max(dur.Milliseconds(), 1)))
		if err == nil {
			_, err = tx.Exec(ctx, q, pgx.QueryExecModeSimpleProtocol)
		}
		if err == nil {
			ran = true
			res.Mode = "query"
		} else {
			_ = tx.Rollback(ctx)
			if tx, err = conn.Begin(ctx); err != nil {
				res.Err = err.Error()
				return
			}
		}
	}
	if !ran {
		res.Mode = "tables"
		if len(lt.Tables) == 0 {
			res.Err = "query not runnable and no tables listed"
			return
		}
		for _, t := range lt.Tables {
			if _, err := tx.Exec(ctx, "SELECT 1 FROM "+pgx.Identifier(strings.Split(t, ".")).Sanitize()+" LIMIT 1"); err != nil {
				res.Err = "lock " + t + ": " + err.Error()
				return
			}
		}
	}
	if remain := dur - time.Since(res.Start); remain > 0 {
		if _, err := tx.Exec(ctx, "SELECT pg_sleep($1)", remain.Seconds()); err != nil {
			res.Err = "hold: " + err.Error()
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		res.Err = "commit: " + err.Error()
	}
}

func hasParams(sql string) bool {
	toks, err := tokenize(sql)
	if err != nil {
		return true
	}
	for _, t := range toks {
		if t.kind == tParam {
			return true
		}
	}
	return false
}
