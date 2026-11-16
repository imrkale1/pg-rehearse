// Package rehearse orchestrates a full dress rehearsal of a migration:
// build (or reuse) a synthetic production-shaped template database, clone it,
// drive production-rate traffic, apply the migration mid-load, measure stalls,
// then suggest and re-test a safer migration.
package rehearse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-rehearse/pg-rehearse/internal/fixer"
	"github.com/pg-rehearse/pg-rehearse/internal/load"
	"github.com/pg-rehearse/pg-rehearse/internal/locks"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
	"github.com/pg-rehearse/pg-rehearse/internal/synth"
)

type Options struct {
	Profile      *model.Profile
	ShadowDSN    string // DSN of the shadow server (any database; we create our own)
	MigrationSQL string

	Scale          float64
	Seed           int64
	Rebuild        bool          // force rebuilding the synthetic template
	Warmup         time.Duration // baseline traffic before the migration starts
	Observe        time.Duration // traffic kept running after the migration finishes
	RateMultiplier float64
	MaxConns       int
	TestFixes      bool

	// StallThreshold: a request slower than this counts as "stalled".
	// 0 = auto: max(5 x baseline p99, 200ms) per query.
	StallThreshold time.Duration

	Log io.Writer
}

func (o *Options) defaults() {
	if o.Scale <= 0 {
		o.Scale = 1
	}
	if o.Warmup <= 0 {
		o.Warmup = 8 * time.Second
	}
	if o.Observe <= 0 {
		o.Observe = 8 * time.Second
	}
	if o.RateMultiplier <= 0 {
		o.RateMultiplier = 1
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 64
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
}

// Thresholds for the verdict.
const (
	dangerousStall = time.Second
	riskyStall     = 200 * time.Millisecond
)

func Run(ctx context.Context, opts Options) (*model.Report, error) {
	opts.defaults()
	if opts.Profile == nil {
		return nil, fmt.Errorf("rehearse: profile is required")
	}
	logf := func(format string, a ...any) { fmt.Fprintf(opts.Log, "pg-rehearse: "+format+"\n", a...) }

	tpl, err := ensureTemplate(ctx, opts, logf)
	if err != nil {
		return nil, err
	}

	logf("rehearsing original migration")
	orig, err := rehearseOnce(ctx, opts, tpl, opts.MigrationSQL, "orig", logf)
	if err != nil {
		return nil, err
	}
	rep := &model.Report{Original: *orig, TrafficNote: trafficNote(opts)}

	if orig.Verdict == model.VerdictSafe && len(dangerLint(orig.Footprint.Lint)) == 0 {
		return rep, nil
	}
	rep.Fixes = fixer.Suggest(opts.MigrationSQL, &orig.Footprint)
	if len(rep.Fixes) == 0 || !opts.TestFixes {
		return rep, nil
	}
	fixed := fixer.Apply(opts.MigrationSQL, rep.Fixes)
	logf("re-testing safer migration")
	fr, err := rehearseOnce(ctx, opts, tpl, fixed, "fixed", logf)
	if err != nil {
		return nil, fmt.Errorf("re-testing fix: %w", err)
	}
	rep.FixedResult = fr
	return rep, nil
}

func dangerLint(fs []model.LintFinding) []model.LintFinding {
	var out []model.LintFinding
	for _, f := range fs {
		if f.Severity == model.SeverityDanger {
			out = append(out, f)
		}
	}
	return out
}

func trafficNote(opts Options) string {
	var rps float64
	for _, q := range opts.Profile.Queries {
		rps += q.CallsPerSec
	}
	rps *= opts.RateMultiplier
	note := fmt.Sprintf("%.0f queries/s across %d query shapes", rps, len(opts.Profile.Queries))
	if n := len(opts.Profile.LongTxns); n > 0 {
		note += fmt.Sprintf(", %d long-running transaction(s)", n)
	}
	if opts.Scale != 1 {
		note += fmt.Sprintf(", data at %g× production size", opts.Scale)
	}
	return note
}

// ---------- template / clone management ----------

func adminConn(ctx context.Context, dsn string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, dsn)
}

func dsnForDB(dsn, db string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))),
		Path:   "/" + db,
	}
	if cfg.TLSConfig == nil {
		u.RawQuery = "sslmode=disable"
	}
	return u.String(), nil
}

func templateName(opts Options) string {
	b, _ := json.Marshal(struct {
		Schema string
		Tables []model.TableProfile
		Scale  float64
		Seed   int64
	}{opts.Profile.SchemaSQL, opts.Profile.Tables, opts.Scale, opts.Seed})
	h := sha256.Sum256(b)
	return "pgr_tpl_" + hex.EncodeToString(h[:6])
}

func dbExists(ctx context.Context, c *pgx.Conn, name string) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok)
	return ok, err
}

func dropDB(ctx context.Context, c *pgx.Conn, name string) error {
	_, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	return err
}

func ensureTemplate(ctx context.Context, opts Options, logf func(string, ...any)) (string, error) {
	name := templateName(opts)
	c, err := adminConn(ctx, opts.ShadowDSN)
	if err != nil {
		return "", fmt.Errorf("connect shadow server: %w", err)
	}
	defer c.Close(ctx)

	exists, err := dbExists(ctx, c, name)
	if err != nil {
		return "", err
	}
	if exists && !opts.Rebuild {
		logf("reusing synthetic template %s", name)
		return name, nil
	}
	if exists {
		if err := dropDB(ctx, c, name); err != nil {
			return "", err
		}
	}
	building := name + "_building"
	_ = dropDB(ctx, c, building)
	if _, err := c.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{building}.Sanitize()); err != nil {
		return "", fmt.Errorf("create template db: %w", err)
	}
	dsn, err := dsnForDB(opts.ShadowDSN, building)
	if err != nil {
		return "", err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return "", err
	}
	start := time.Now()
	logf("building synthetic copy (scale %g) — no production rows are read", opts.Scale)
	err = synth.Build(ctx, pool, opts.Profile, synth.Options{Scale: opts.Scale, Seed: opts.Seed, BatchRows: 10000})
	pool.Close()
	if err != nil {
		_ = dropDB(ctx, c, building)
		return "", fmt.Errorf("build synthetic copy: %w", err)
	}
	if _, err := c.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{building}.Sanitize()+" RENAME TO "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", err
	}
	logf("synthetic copy ready in %s", time.Since(start).Round(time.Millisecond))
	return name, nil
}

func cloneTemplate(ctx context.Context, opts Options, tpl, suffix string) (string, func(), error) {
	c, err := adminConn(ctx, opts.ShadowDSN)
	if err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("pgr_run_%s_%d", suffix, time.Now().UnixNano()%1e9)
	_, err = c.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE "+pgx.Identifier{tpl}.Sanitize())
	if err != nil {
		c.Close(ctx)
		return "", nil, fmt.Errorf("clone template: %w", err)
	}
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = dropDB(cctx, c, name)
		c.Close(cctx)
	}
	return name, cleanup, nil
}

// ---------- one rehearsal ----------

var nonTxRe = regexp.MustCompile(`(?i)\b(CONCURRENTLY|VACUUM)\b|\bALTER\s+TYPE\b.*\bADD\s+VALUE\b`)

func rehearseOnce(ctx context.Context, opts Options, tpl, migration, label string, logf func(string, ...any)) (*model.RehearsalResult, error) {
	db, cleanup, err := cloneTemplate(ctx, opts, tpl, label)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	dsn, err := dsnForDB(opts.ShadowDSN, db)
	if err != nil {
		return nil, err
	}

	// 1. Static + lock footprint (BEGIN ... ROLLBACK, so the clone is unchanged).
	apool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	fp, err := locks.Analyze(ctx, apool, migration)
	apool.Close()
	if err != nil {
		return nil, fmt.Errorf("lock analysis: %w", err)
	}

	// 2. Traffic.
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pcfg.MaxConns = int32(opts.MaxConns)
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	longStart := opts.Warmup - 1500*time.Millisecond
	if longStart < 0 {
		longStart = 0
	}
	runner, err := load.NewRunner(pool, load.Config{
		Queries:           opts.Profile.Queries,
		LongTxns:          opts.Profile.LongTxns,
		RateMultiplier:    opts.RateMultiplier,
		MaxConns:          opts.MaxConns,
		LongTxnStartAfter: longStart,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare traffic: %w", err)
	}

	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	type loadOut struct {
		res *model.LoadResult
		err error
	}
	done := make(chan loadOut, 1)
	go func() {
		r, err := runner.Run(loadCtx)
		done <- loadOut{r, err}
	}()

	logf("[%s] baseline traffic for %s", label, opts.Warmup)
	select {
	case <-time.After(opts.Warmup):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 3. Apply the migration on its own connection, mid-traffic.
	logf("[%s] applying migration under load", label)
	migStart := time.Now()
	migErr := applyMigration(ctx, dsn, migration)
	migEnd := time.Now()

	logf("[%s] observing for %s", label, opts.Observe)
	select {
	case <-time.After(opts.Observe):
	case <-ctx.Done():
	}
	stopLoad()
	out := <-done
	if out.err != nil && out.err != context.Canceled {
		return nil, fmt.Errorf("traffic: %w", out.err)
	}

	res := &model.RehearsalResult{
		Migration:           migration,
		Footprint:           *fp,
		MigrationDurationMs: ms(migEnd.Sub(migStart)),
	}
	if migErr != nil {
		res.MigrationError = migErr.Error()
	}
	res.Impacts = impacts(out.res, opts.Profile.Queries, migStart, migEnd.Add(opts.Observe), opts.StallThreshold)
	for _, im := range res.Impacts {
		if im.StallMs > res.MaxStallMs {
			res.MaxStallMs = im.StallMs
		}
	}
	res.Verdict = verdict(res)
	logf("[%s] verdict %s (max stall %.0fms, migration %.0fms)", label, res.Verdict, res.MaxStallMs, res.MigrationDurationMs)
	return res, nil
}

func applyMigration(ctx context.Context, dsn, migration string) error {
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	stmts := locks.SplitStatements(migration)
	if nonTxRe.MatchString(migration) {
		for _, s := range stmts {
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", firstLine(s), err)
			}
		}
		return nil
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return tx.Commit(ctx)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func impacts(lr *model.LoadResult, qs []model.QueryProfile, from, to time.Time, fixed time.Duration) []model.Impact {
	if lr == nil {
		return nil
	}
	byQ := map[int64][]model.Sample{}
	for _, s := range lr.Samples {
		byQ[s.QueryID] = append(byQ[s.QueryID], s)
	}
	var out []model.Impact
	for _, q := range qs {
		samples := byQ[q.QueryID]
		if len(samples) == 0 {
			continue
		}
		var base []time.Duration
		var during []model.Sample
		errs := 0
		for _, s := range samples {
			end := s.Start.Add(s.Latency)
			switch {
			case end.Before(from):
				if s.Err == "" {
					base = append(base, s.Latency)
				}
			case s.Start.Before(to):
				during = append(during, s)
				if s.Err != "" {
					errs++
				}
			}
		}
		bp99 := percentile(base, 0.99)
		thr := fixed
		if thr == 0 {
			thr = 5 * bp99
			if thr < riskyStall {
				thr = riskyStall
			}
		}
		var maxD time.Duration
		for _, s := range during {
			if s.Latency > maxD {
				maxD = s.Latency
			}
		}
		var stall time.Duration
		for _, w := range load.StallWindows(during, q.QueryID, thr) {
			if d := w.End.Sub(w.Start); d > stall {
				stall = d
			}
		}
		out = append(out, model.Impact{
			QueryID: q.QueryID, Query: q.Query, Feature: q.Feature,
			BaselineP99Ms: ms(bp99), DuringMaxMs: ms(maxD), StallMs: ms(stall), Errors: errs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StallMs > out[j].StallMs })
	return out
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(p * float64(len(s)-1))
	return s[idx]
}

func verdict(r *model.RehearsalResult) model.Verdict {
	stall := time.Duration(r.MaxStallMs * float64(time.Millisecond))
	errs := 0
	for _, im := range r.Impacts {
		errs += im.Errors
	}
	switch {
	case stall >= dangerousStall || errs > 0:
		return model.VerdictDangerous
	case stall >= riskyStall || r.MigrationError != "":
		return model.VerdictRisky
	default:
		return model.VerdictSafe
	}
}
