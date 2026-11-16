package synth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Options controls Build.
type Options struct {
	Scale     float64 // 1.0 = full production row counts; 0.01 = 1%. <= 0 means 1.
	Seed      int64
	BatchRows int // rows per COPY batch; <= 0 means 50000
}

// Logf receives progress messages (per-table row counts and rates). It is a
// no-op by default.
var Logf = func(format string, args ...any) {}

const defaultBatchRows = 50000

// Build applies p.SchemaSQL to the (empty) target database and fills every
// table with synthetic rows matching the profile's row counts and column
// distributions. The same Seed always yields the same data.
func Build(ctx context.Context, pool *pgxpool.Pool, p *model.Profile, opts Options) error {
	if p == nil {
		return errors.New("synth: nil profile")
	}
	if opts.Scale <= 0 || math.IsNaN(opts.Scale) {
		opts.Scale = 1
	}
	if opts.BatchRows <= 0 {
		opts.BatchRows = defaultBatchRows
	}
	start := time.Now()
	if err := applySchema(ctx, pool, p.SchemaSQL); err != nil {
		return err
	}
	cat, err := readCatalog(ctx, pool)
	if err != nil {
		return fmt.Errorf("synth: %w", err)
	}
	b := &builder{pool: pool, cat: cat, p: p, opts: opts}
	if err := b.plan(ctx); err != nil {
		return fmt.Errorf("synth: %w", err)
	}
	if err := b.prepare(ctx); err != nil {
		return fmt.Errorf("synth: %w", err)
	}
	var total int64
	for _, t := range b.order {
		n, err := b.load(ctx, t)
		if err != nil {
			return fmt.Errorf("synth: %w", err)
		}
		total += n
	}
	if err := b.fillDeferred(ctx); err != nil {
		return fmt.Errorf("synth: %w", err)
	}
	if err := b.finish(ctx); err != nil {
		return fmt.Errorf("synth: %w", err)
	}
	el := time.Since(start)
	Logf("synth: %d rows in %d tables, %s (%.0f rows/s overall)", total, len(b.order), el.Round(time.Millisecond), float64(total)/math.Max(el.Seconds(), 1e-9))
	return nil
}

// applySchema runs the schema DDL through the simple query protocol.
func applySchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	var keep []string
	for _, line := range strings.Split(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) { // psql meta-commands (\restrict, \connect, ...)
			continue
		}
		keep = append(keep, line)
	}
	sql := strings.TrimSpace(strings.Join(keep, "\n"))
	if sql == "" {
		return nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("synth: apply schema: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		return fmt.Errorf("synth: apply schema: %s", pgErrString(err))
	}
	if _, err := conn.Exec(ctx, "RESET ALL"); err != nil {
		return fmt.Errorf("synth: apply schema: %w", err)
	}
	return nil
}

type builder struct {
	pool *pgxpool.Pool
	cat  *catalog
	p    *model.Profile
	opts Options

	root     map[*tableInfo]*tableInfo // partition -> partitioned root
	targets  []*tableInfo              // tables we COPY into (no partitions), sorted by name
	prof     map[*tableInfo]map[string]*model.ColumnProfile
	target   map[*tableInfo]int64
	order    []*tableInfo
	deferred []*fkInfo
	dropFKs  []*fkInfo
	dropNN   []*colInfo
	nnTable  map[*colInfo]*tableInfo
	seqs     map[*colInfo]seqInfo
}

type seqInfo struct {
	name        string
	start, step int64
}

func (b *builder) rootOf(t *tableInfo) *tableInfo {
	if r := b.root[t]; r != nil {
		return r
	}
	return t
}

func (b *builder) plan(ctx context.Context) error {
	b.root = map[*tableInfo]*tableInfo{}
	b.prof = map[*tableInfo]map[string]*model.ColumnProfile{}
	b.target = map[*tableInfo]int64{}
	b.nnTable = map[*colInfo]*tableInfo{}
	b.seqs = map[*colInfo]seqInfo{}

	rows, err := b.pool.Query(ctx, `SELECT c.oid::int8, pg_partition_root(c.oid)::oid::int8 FROM pg_class c WHERE c.relispartition`)
	if err != nil {
		return fmt.Errorf("read partitions: %w", err)
	}
	for rows.Next() {
		var part, root int64
		if err := rows.Scan(&part, &root); err != nil {
			rows.Close()
			return fmt.Errorf("read partitions: %w", err)
		}
		pt, rt := b.cat.byOID[uint32(part)], b.cat.byOID[uint32(root)]
		if pt != nil && rt != nil {
			b.root[pt] = rt
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read partitions: %w", err)
	}

	for _, t := range b.cat.tables {
		if !t.isPartition {
			b.targets = append(b.targets, t)
		}
	}
	b.targets = sortedTables(b.targets)

	// Row estimates and column profiles.
	est := map[*tableInfo]int64{}
	partEst := map[*tableInfo]int64{}
	for i := range b.p.Tables {
		tp := &b.p.Tables[i]
		t := b.cat.byName[tp.QualifiedName()]
		if t == nil {
			Logf("synth: profile table %s not found in schema, skipped", tp.QualifiedName())
			continue
		}
		r := b.rootOf(t)
		cols := map[string]*model.ColumnProfile{}
		for j := range tp.Columns {
			cols[tp.Columns[j].Name] = &tp.Columns[j]
		}
		if r == t {
			b.prof[t] = cols
			est[t] = max(tp.RowEstimate, 0)
		} else {
			if _, ok := b.prof[r]; !ok || len(b.prof[r]) == 0 {
				if len(cols) > 0 {
					b.prof[r] = cols
				}
			}
			partEst[r] += max(tp.RowEstimate, 0)
		}
	}
	for _, t := range b.targets {
		e := est[t]
		if e <= 0 {
			e = partEst[t]
		}
		b.target[t] = int64(math.Round(float64(e) * b.opts.Scale))
	}

	// Dependency graph and FK-topological order.
	if err := b.order_(); err != nil {
		return err
	}

	// A NOT NULL foreign key needs at least one parent row.
	for changed, iter := true, 0; changed && iter < len(b.targets)+1; iter++ {
		changed = false
		for _, t := range b.targets {
			if b.target[t] == 0 {
				continue
			}
			for _, fk := range t.fks {
				pt := b.rootOf(fk.parent)
				if !fk.allNullable() && b.target[pt] == 0 && pt != t {
					b.target[pt] = 1
					changed = true
				}
			}
		}
	}
	// A unique NOT NULL foreign key cannot have more rows than its parent.
	for _, t := range b.order {
		for _, fk := range t.fks {
			if !fk.deferred && !fk.allNullable() && b.fkUnique(t, fk) && b.target[t] > b.target[b.rootOf(fk.parent)] {
				Logf("synth: table %s capped to %d rows by unique foreign key %s", t.qname(), b.target[b.rootOf(fk.parent)], fk.name)
				b.target[t] = b.target[b.rootOf(fk.parent)]
			}
		}
	}

	// Sequences of identity / serial columns.
	for _, t := range b.targets {
		for _, c := range t.cols {
			if !c.sequenceBacked() || (c.typ.kind != kInt && c.typ.kind != kNumeric) {
				continue
			}
			name, err := sequenceFor(ctx, b.pool, t, c)
			if err != nil {
				return err
			}
			if name == "" {
				continue
			}
			si := seqInfo{name: name, start: 1, step: 1}
			if err := b.pool.QueryRow(ctx, `SELECT seqstart, seqincrement FROM pg_sequence WHERE seqrelid = $1::regclass`, name).Scan(&si.start, &si.step); err != nil {
				return fmt.Errorf("table %s column %s: read sequence %s: %w", t.qname(), c.name, name, err)
			}
			if si.step == 0 {
				si.step = 1
			}
			b.seqs[c] = si
		}
	}
	return nil
}

// order_ computes b.order (parents first) and marks the foreign keys that must
// be filled after the load (self references and edges closing a cycle).
func (b *builder) order_() error {
	idx := map[*tableInfo]int{}
	for i, t := range b.targets {
		idx[t] = i
	}
	n := len(b.targets)
	adj := make([][]int, n) // child -> parents
	for i, t := range b.targets {
		for _, fk := range t.fks {
			if j, ok := idx[b.rootOf(fk.parent)]; ok && j != i {
				adj[i] = append(adj[i], j)
			}
		}
	}
	// Tarjan SCC.
	comp := make([]int, n)
	for i := range comp {
		comp[i] = -1
	}
	index := make([]int, n)
	low := make([]int, n)
	on := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int
	counter, ncomp := 0, 0
	var strong func(v int)
	strong = func(v int) {
		index[v], low[v] = counter, counter
		counter++
		stack = append(stack, v)
		on[v] = true
		for _, w := range adj[v] {
			if index[w] < 0 {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp[w] = ncomp
				if w == v {
					break
				}
			}
			ncomp++
		}
	}
	for v := 0; v < n; v++ {
		if index[v] < 0 {
			strong(v)
		}
	}
	// Defer self references and, inside a cycle, edges to a table that sorts
	// at or after the child (targets are name-sorted, so idx is that order).
	for i, t := range b.targets {
		for _, fk := range t.fks {
			pt := b.rootOf(fk.parent)
			j, ok := idx[pt]
			if !ok {
				continue
			}
			if j == i || (comp[i] == comp[j] && j > i) {
				fk.deferred = true
				b.deferred = append(b.deferred, fk)
			}
		}
	}
	// Kahn over the remaining edges with a deterministic (name) tie-break.
	indeg := make([]int, n)
	children := make([][]int, n)
	for i, t := range b.targets {
		seen := map[int]bool{}
		for _, fk := range t.fks {
			if fk.deferred {
				continue
			}
			j, ok := idx[b.rootOf(fk.parent)]
			if !ok || j == i || seen[j] {
				continue
			}
			seen[j] = true
			indeg[i]++
			children[j] = append(children[j], i)
		}
	}
	done := make([]bool, n)
	for len(b.order) < n {
		progress := false
		for i := 0; i < n; i++ {
			if done[i] || indeg[i] > 0 {
				continue
			}
			done[i] = true
			progress = true
			b.order = append(b.order, b.targets[i])
			for _, c := range children[i] {
				indeg[c]--
			}
			break
		}
		if !progress {
			return errors.New("foreign key graph: unresolved cycle")
		}
	}
	return nil
}

// fkUnique reports whether the FK columns of t are covered exactly by a unique index.
func (b *builder) fkUnique(t *tableInfo, fk *fkInfo) bool {
	if len(fk.cols) == 1 {
		return fk.cols[0].unique
	}
	want := colSet(fk.cols)
	for _, set := range t.uniqueSets {
		if colSet(set) == want {
			return true
		}
	}
	return false
}

func colSet(cols []*colInfo) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.name
	}
	sort.Strings(names)
	return strings.Join(names, "\x00")
}

// prepare removes what slows the bulk load down (FKs, secondary indexes, user
// triggers) or what the load order cannot satisfy (CHECKs are re-added per
// table after violating rows are removed; deferred NOT NULL FK columns).
func (b *builder) prepare(ctx context.Context) error {
	for _, t := range b.targets {
		if b.target[t] == 0 {
			continue
		}
		var stmts []string
		if t.hasUserTrig {
			stmts = append(stmts, "ALTER TABLE "+t.ident+" DISABLE TRIGGER USER")
		}
		for _, fk := range t.fks {
			b.dropFKs = append(b.dropFKs, fk)
			stmts = append(stmts, "ALTER TABLE "+t.ident+" DROP CONSTRAINT "+pgx.Identifier{fk.name}.Sanitize())
		}
		for _, ck := range t.checks {
			stmts = append(stmts, "ALTER TABLE "+t.ident+" DROP CONSTRAINT "+pgx.Identifier{ck.name}.Sanitize())
		}
		for _, ix := range t.dropIdx {
			stmts = append(stmts, "DROP INDEX "+ix.name)
		}
		for _, fk := range t.fks {
			if !fk.deferred {
				continue
			}
			for _, c := range fk.cols {
				if c.notNull {
					c.notNull = false
					b.dropNN = append(b.dropNN, c)
					b.nnTable[c] = t
					stmts = append(stmts, "ALTER TABLE "+t.ident+" ALTER COLUMN "+pgx.Identifier{c.name}.Sanitize()+" DROP NOT NULL")
				}
			}
		}
		for _, s := range stmts {
			if _, err := b.pool.Exec(ctx, s); err != nil {
				return fmt.Errorf("table %s: prepare (%s): %s", t.qname(), s, pgErrString(err))
			}
		}
	}
	return nil
}

// ---------- loading one table ----------

type tableLoad struct {
	t      *tableInfo
	rows   int64
	cols   []*colInfo
	gens   []gen
	dedupe [][]int // output indexes of multi-column unique sets to enforce
}

func (b *builder) colProfile(t *tableInfo, c *colInfo) *model.ColumnProfile {
	if m := b.prof[t]; m != nil {
		return m[c.name]
	}
	return nil
}

func (b *builder) newLoad(ctx context.Context, t *tableInfo) (*tableLoad, error) {
	rows := b.target[t]
	tl := &tableLoad{t: t, rows: rows}
	claimed := map[*colInfo]bool{}
	outIdx := map[*colInfo]int{}
	addCol := func(c *colInfo) int {
		i := len(tl.cols)
		tl.cols = append(tl.cols, c)
		outIdx[c] = i
		claimed[c] = true
		return i
	}
	fkOf := map[*colInfo]*fkInfo{}
	for _, fk := range t.fks {
		for _, c := range fk.cols {
			if fkOf[c] == nil || (fkOf[c].deferred && !fk.deferred) {
				fkOf[c] = fk
			}
		}
	}
	for _, c := range t.cols {
		if claimed[c] || c.generated != "" {
			continue
		}
		if fk := fkOf[c]; fk != nil {
			var idxs []int
			for _, fc := range fk.cols {
				if !claimed[fc] {
					idxs = append(idxs, addCol(fc))
				}
			}
			if len(idxs) != len(fk.cols) {
				return nil, fmt.Errorf("table %s: overlapping foreign keys on column %s are not supported", t.qname(), c.name)
			}
			if fk.deferred {
				tl.gens = append(tl.gens, &nullGen{idxs: idxs})
				continue
			}
			g, err := b.newFKGen(ctx, t, fk, rows, idxs)
			if err != nil {
				return nil, err
			}
			tl.gens = append(tl.gens, g)
			continue
		}
		if si, ok := b.seqs[c]; ok {
			tl.gens = append(tl.gens, &seqGen{idx: addCol(c), start: si.start, step: si.step})
			continue
		}
		i := len(tl.cols)
		g, err := newScalarGen(t, c, b.colProfile(t, c), i, rows, b.opts.Seed)
		if err != nil {
			return nil, err
		}
		if g == nil {
			continue // database default
		}
		addCol(c)
		tl.gens = append(tl.gens, g)
	}
	// Multi-column unique sets that are not implied by a unique column.
	for _, set := range t.uniqueSets {
		implied := false
		var idxs []int
		for _, c := range set {
			i, ok := outIdx[c]
			if !ok {
				implied = true // a database default fills it; we cannot dedupe
				break
			}
			if c.unique || b.seqs[c].name != "" {
				implied = true
				break
			}
			idxs = append(idxs, i)
		}
		if !implied {
			tl.dedupe = append(tl.dedupe, idxs)
		}
	}
	return tl, nil
}

func (b *builder) newFKGen(ctx context.Context, t *tableInfo, fk *fkInfo, rows int64, idxs []int) (*fkGen, error) {
	keys, err := b.parentKeys(ctx, fk)
	if err != nil {
		return nil, err
	}
	g := &fkGen{idxs: idxs, unique: b.fkUnique(t, fk)}
	cp := b.colProfile(t, fk.cols[0])
	if cp != nil && fk.allNullable() {
		g.nullFrac = clamp01(cp.NullFrac)
	}
	if len(keys) == 0 {
		if !fk.allNullable() && rows > 0 {
			return nil, fmt.Errorf("table %s column %s: foreign key %s references empty table %s", t.qname(), fk.cols[0].name, fk.name, fk.parent.qname())
		}
		return g, nil
	}
	// Deterministic shuffle so that "hot" parents are spread over the key space.
	perm := make([][]string, len(keys))
	copy(perm, keys)
	r := newRand(b.opts.Seed, fnv64("fk:"+t.qname()+"."+fk.name), 0)
	r.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	n := len(perm)
	var freqs []float64
	if cp != nil {
		if d := resolveDistinct(cp.NDistinct, rows); d > 0 && d < int64(n) && !g.unique {
			n = int(d)
		}
		if !cp.Sensitive && len(cp.MostCommonFreqs) == len(cp.MostCommonVals) {
			freqs = cp.MostCommonFreqs
		}
	}
	g.keys = perm[:n]
	g.picker = newSkewPicker(n, freqs, g.nullFrac, defaultSkew)
	return g, nil
}

// parentKeys returns the referenced key tuples of fk's parent (text form),
// in a deterministic order.
func (b *builder) parentKeys(ctx context.Context, fk *fkInfo) ([][]string, error) {
	p := fk.parent
	ck := colKey(fk.refCols)
	if k, ok := p.keys[ck]; ok {
		return k, nil
	}
	sel := make([]string, len(fk.refCols))
	where := make([]string, len(fk.refCols))
	for i, c := range fk.refCols {
		id := pgx.Identifier{c.name}.Sanitize()
		sel[i] = id + "::text"
		where[i] = id + " IS NOT NULL"
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s", strings.Join(sel, ", "), p.ident, strings.Join(where, " AND "), colList(fk.refCols))
	keys, err := queryTuples(ctx, b.pool, q, len(fk.refCols))
	if err != nil {
		return nil, fmt.Errorf("table %s: read keys of %s for foreign key %s: %w", fk.child.qname(), p.qname(), fk.name, err)
	}
	p.keys[ck] = keys
	return keys, nil
}

func queryTuples(ctx context.Context, pool *pgxpool.Pool, q string, n int) ([][]string, error) {
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		tup := make([]string, n)
		ptrs := make([]any, n)
		for i := range tup {
			ptrs[i] = &tup[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, tup)
	}
	return out, rows.Err()
}

// load fills table t and returns the number of rows it ends up with.
func (b *builder) load(ctx context.Context, t *tableInfo) (int64, error) {
	start := time.Now()
	tl, err := b.newLoad(ctx, t)
	if err != nil {
		return 0, err
	}
	var inserted int64
	if tl.rows > 0 && len(tl.cols) > 0 {
		inserted, err = b.copyTable(ctx, tl)
		if err != nil {
			return 0, err
		}
	} else if tl.rows > 0 {
		// Every column comes from a default.
		ct, err := b.pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT FROM generate_series(1, %d)", t.ident, tl.rows))
		if err != nil {
			return 0, fmt.Errorf("table %s: insert defaults: %s", t.qname(), pgErrString(err))
		}
		inserted = ct.RowsAffected()
	}
	// CHECK constraints: drop violators, then restore the constraint (validated).
	for _, ck := range t.checks {
		if inserted > 0 && ck.expr != "" {
			ct, err := b.pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE NOT (%s)", t.ident, ck.expr))
			if err != nil {
				return 0, fmt.Errorf("table %s: check %s: %s", t.qname(), ck.name, pgErrString(err))
			}
			if n := ct.RowsAffected(); n > 0 {
				Logf("synth: table %s: removed %d rows violating check %s", t.qname(), n, ck.name)
				inserted -= n
			}
		}
		if b.target[t] > 0 {
			if _, err := b.pool.Exec(ctx, "ALTER TABLE "+t.ident+" ADD CONSTRAINT "+pgx.Identifier{ck.name}.Sanitize()+" "+ck.def); err != nil {
				return 0, fmt.Errorf("table %s: restore check %s: %s", t.qname(), ck.name, pgErrString(err))
			}
		}
	}
	t.rows = inserted
	el := time.Since(start)
	if inserted > 0 {
		Logf("synth: %s: %d rows in %s (%.0f rows/s)", t.qname(), inserted, el.Round(time.Millisecond), float64(inserted)/math.Max(el.Seconds(), 1e-9))
	}
	return inserted, nil
}

func (b *builder) workers(tl *tableLoad) int {
	if len(tl.dedupe) > 0 {
		return 1 // dedupe must see batches in order to stay deterministic
	}
	w := int(b.pool.Config().MaxConns)
	w = min(w, 4)
	nb := int((tl.rows + int64(b.opts.BatchRows) - 1) / int64(b.opts.BatchRows))
	return max(min(w, nb), 1)
}

func (b *builder) copyTable(ctx context.Context, tl *tableLoad) (int64, error) {
	t := tl.t
	names := make([]string, len(tl.cols))
	for i, c := range tl.cols {
		names[i] = pgx.Identifier{c.name}.Sanitize()
	}
	copySQL := fmt.Sprintf("COPY %s (%s) FROM STDIN", t.ident, strings.Join(names, ", "))
	batch := int64(b.opts.BatchRows)
	nb := (tl.rows + batch - 1) / batch
	var dd *deduper
	if len(tl.dedupe) > 0 {
		dd = newDeduper(tl.dedupe)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int64)
	var (
		mu       sync.Mutex
		firstErr error
		total    int64
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for w := 0; w < b.workers(tl); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := b.pool.Acquire(ctx)
			if err != nil {
				fail(fmt.Errorf("table %s: acquire connection: %w", t.qname(), err))
				return
			}
			defer conn.Release()
			var buf []byte
			for bi := range jobs {
				from := bi * batch
				to := min(from+batch, tl.rows)
				buf = genBatch(buf[:0], tl, b.opts.Seed, bi, from, to, dd)
				ct, err := conn.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(buf), copySQL)
				if err != nil {
					fail(fmt.Errorf("table %s: copy rows %d-%d: %s", t.qname(), from, to, pgErrString(err)))
					return
				}
				mu.Lock()
				total += ct.RowsAffected()
				mu.Unlock()
			}
		}()
	}
feed:
	for bi := int64(0); bi < nb; bi++ {
		select {
		case jobs <- bi:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// genBatch appends rows [from, to) of the table in COPY text format. Every
// batch has its own random stream, so the output does not depend on which
// worker produced it.
func genBatch(buf []byte, tl *tableLoad, seed, batch, from, to int64, dd *deduper) []byte {
	r := newRand(seed, fnv64("rows:"+tl.t.qname()), uint64(batch))
	out := make([]value, len(tl.cols))
	for row := from; row < to; row++ {
		ok := true
		for attempt := 0; ; attempt++ {
			for _, g := range tl.gens {
				g.fill(r, row, out)
			}
			if dd == nil || dd.add(out) {
				break
			}
			if attempt >= 20 {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		buf = appendCopyRow(buf, out)
	}
	return buf
}

func appendCopyRow(buf []byte, out []value) []byte {
	for i := range out {
		if i > 0 {
			buf = append(buf, '\t')
		}
		if out[i].null {
			buf = append(buf, `\N`...)
			continue
		}
		buf = appendCopyEscaped(buf, out[i].b)
	}
	return append(buf, '\n')
}

// appendCopyEscaped escapes a field for COPY text format.
func appendCopyEscaped(buf, s []byte) []byte {
	for _, c := range s {
		switch c {
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\t':
			buf = append(buf, '\\', 't')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		default:
			buf = append(buf, c)
		}
	}
	return buf
}

// deduper enforces multi-column unique sets. Rows with a NULL in a set never
// collide (Postgres default NULLS DISTINCT).
type deduper struct {
	sets [][]int
	seen []map[string]struct{}
	key  []byte
}

func newDeduper(sets [][]int) *deduper {
	d := &deduper{sets: sets}
	for range sets {
		d.seen = append(d.seen, map[string]struct{}{})
	}
	return d
}

func (d *deduper) add(out []value) bool {
	keys := make([]string, len(d.sets))
	for si, set := range d.sets {
		d.key = d.key[:0]
		hasNull := false
		for _, i := range set {
			if out[i].null {
				hasNull = true
				break
			}
			d.key = append(d.key, out[i].b...)
			d.key = append(d.key, 0)
		}
		if hasNull {
			continue
		}
		k := string(d.key)
		if _, dup := d.seen[si][k]; dup {
			return false
		}
		keys[si] = k
	}
	for si, k := range keys {
		if k != "" {
			d.seen[si][k] = struct{}{}
		}
	}
	return true
}

// ---------- deferred foreign keys (self references, cycles) ----------

func (b *builder) fillDeferred(ctx context.Context) error {
	for _, fk := range b.deferred {
		t := fk.child
		if t.rows == 0 {
			continue
		}
		if err := b.fillOne(ctx, fk); err != nil {
			return fmt.Errorf("table %s column %s: fill foreign key %s: %s", t.qname(), fk.cols[0].name, fk.name, pgErrString(err))
		}
	}
	return nil
}

func (b *builder) fillOne(ctx context.Context, fk *fkInfo) error {
	t := fk.child
	self := b.rootOf(fk.parent) == t
	// Row identity: the primary key when it is disjoint from the FK, else ctid.
	idCols := t.pkCols
	for _, c := range idCols {
		for _, fc := range fk.cols {
			if c == fc {
				idCols = nil
			}
		}
	}
	var idSel, idOrder, idTmp, idJoin []string
	if len(idCols) > 0 {
		for _, c := range idCols {
			id := pgx.Identifier{c.name}.Sanitize()
			idSel = append(idSel, id+"::text")
			idOrder = append(idOrder, id)
			idTmp = append(idTmp, id)
			idJoin = append(idJoin, "c."+id+" = s."+id)
		}
	} else {
		idSel = []string{"ctid::text"}
		idOrder = []string{"ctid"}
		idTmp = []string{"ctid AS _pgr_tid"}
		idJoin = []string{"c.ctid = s._pgr_tid"}
	}
	nid := len(idSel)
	sel := append([]string(nil), idSel...)
	if self {
		for _, c := range fk.refCols {
			sel = append(sel, pgx.Identifier{c.name}.Sanitize()+"::text")
		}
	}
	tuples, err := queryTuples(ctx, b.pool, fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", strings.Join(sel, ", "), t.ident, strings.Join(idOrder, ", ")), len(sel))
	if err != nil {
		return err
	}
	nullFrac := 0.0
	if cp := b.colProfile(t, fk.cols[0]); cp != nil && fk.allNullable() {
		nullFrac = clamp01(cp.NullFrac)
	}
	notNull := len(b.nnTable) > 0 && func() bool {
		for _, c := range fk.cols {
			if b.nnTable[c] != nil {
				return true
			}
		}
		return false
	}()
	if notNull {
		nullFrac = 0
	}
	r := newRand(b.opts.Seed, fnv64("deferred:"+t.qname()+"."+fk.name), 0)

	var buf []byte
	out := make([]value, nid+len(fk.cols))
	var g *fkGen
	if !self {
		idxs := make([]int, len(fk.cols))
		for i := range idxs {
			idxs[i] = nid + i
		}
		if g, err = b.newFKGen(ctx, t, fk, t.rows, idxs); err != nil {
			return err
		}
	}
	for i, tup := range tuples {
		for j := 0; j < nid; j++ {
			out[j] = value{b: []byte(tup[j])}
		}
		if self {
			nullRow := nullFrac > 0 && r.Float64() < nullFrac
			p := treeParent(r, i, defaultSkew)
			if p < 0 {
				if !notNull {
					continue
				}
				p = i
			}
			if nullRow {
				continue
			}
			ref := tuples[p][nid:]
			for j := range fk.cols {
				out[nid+j] = value{b: []byte(ref[j])}
			}
		} else {
			g.fill(r, int64(i), out)
			if out[nid].null {
				continue
			}
		}
		buf = appendCopyRow(buf, out)
	}
	if len(buf) == 0 {
		return nil
	}
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	fkNames := make([]string, len(fk.cols))
	set := make([]string, len(fk.cols))
	for i, c := range fk.cols {
		id := pgx.Identifier{c.name}.Sanitize()
		fkNames[i] = id
		set[i] = id + " = s." + id
	}
	tmpCols := append(append([]string(nil), idTmp...), fkNames...)
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE _pgr_fk ON COMMIT DROP AS SELECT %s FROM %s LIMIT 0", strings.Join(tmpCols, ", "), t.ident)); err != nil {
		return err
	}
	copyCols := append(append([]string(nil), idOrder...), fkNames...)
	if len(idCols) == 0 {
		copyCols[0] = "_pgr_tid"
	}
	if _, err := tx.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(buf), fmt.Sprintf("COPY _pgr_fk (%s) FROM STDIN", strings.Join(copyCols, ", "))); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "ANALYZE _pgr_fk"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s AS c SET %s FROM _pgr_fk s WHERE %s", t.ident, strings.Join(set, ", "), strings.Join(idJoin, " AND "))); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- finishing ----------

func (b *builder) finish(ctx context.Context) error {
	for _, c := range b.dropNN {
		t := b.nnTable[c]
		c.notNull = true
		if _, err := b.pool.Exec(ctx, "ALTER TABLE "+t.ident+" ALTER COLUMN "+pgx.Identifier{c.name}.Sanitize()+" SET NOT NULL"); err != nil {
			return fmt.Errorf("table %s column %s: restore NOT NULL: %s", t.qname(), c.name, pgErrString(err))
		}
	}
	// Secondary indexes, in parallel.
	var idx []struct {
		t  *tableInfo
		ix indexInfo
	}
	for _, t := range b.targets {
		if b.target[t] == 0 {
			continue
		}
		for _, ix := range t.dropIdx {
			idx = append(idx, struct {
				t  *tableInfo
				ix indexInfo
			}{t, ix})
		}
	}
	if len(idx) > 0 {
		jobs := make(chan int)
		errs := make([]error, len(idx))
		var wg sync.WaitGroup
		for w := 0; w < min(int(b.pool.Config().MaxConns), 4, len(idx)); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if _, err := b.pool.Exec(ctx, idx[i].ix.def); err != nil {
						errs[i] = fmt.Errorf("table %s: recreate index %s: %s", idx[i].t.qname(), idx[i].ix.name, pgErrString(err))
					}
				}
			}()
		}
		for i := range idx {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	for _, fk := range b.dropFKs {
		t := fk.child
		if _, err := b.pool.Exec(ctx, "ALTER TABLE "+t.ident+" ADD CONSTRAINT "+pgx.Identifier{fk.name}.Sanitize()+" "+fk.def); err != nil {
			return fmt.Errorf("table %s column %s: restore foreign key %s: %s", t.qname(), fk.cols[0].name, fk.name, pgErrString(err))
		}
	}
	for _, t := range b.targets {
		if b.target[t] > 0 && t.hasUserTrig {
			if _, err := b.pool.Exec(ctx, "ALTER TABLE "+t.ident+" ENABLE TRIGGER USER"); err != nil {
				return fmt.Errorf("table %s: enable triggers: %s", t.qname(), pgErrString(err))
			}
		}
		for _, c := range t.cols {
			si, ok := b.seqs[c]
			if !ok || t.rows == 0 {
				continue
			}
			agg := "max"
			if si.step < 0 {
				agg = "min"
			}
			q := fmt.Sprintf("SELECT setval($1::regclass, m) FROM (SELECT %s(%s)::int8 AS m FROM %s) s WHERE m IS NOT NULL", agg, pgx.Identifier{c.name}.Sanitize(), t.ident)
			if _, err := b.pool.Exec(ctx, q, si.name); err != nil {
				return fmt.Errorf("table %s column %s: setval %s: %s", t.qname(), c.name, si.name, pgErrString(err))
			}
		}
	}
	for _, t := range b.targets {
		if _, err := b.pool.Exec(ctx, "ANALYZE "+t.ident); err != nil {
			return fmt.Errorf("table %s: analyze: %s", t.qname(), pgErrString(err))
		}
	}
	return nil
}

// pgErrString renders a Postgres error with its detail and context, which name
// the offending column/row for COPY failures.
func pgErrString(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		s := pe.Message
		if pe.Detail != "" {
			s += " (" + pe.Detail + ")"
		}
		if pe.ColumnName != "" {
			s += " [column " + pe.ColumnName + "]"
		}
		if pe.Where != "" {
			s += " [" + pe.Where + "]"
		}
		return s
	}
	return err.Error()
}
