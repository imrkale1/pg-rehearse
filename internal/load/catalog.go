package load

import (
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// samplePoolSize is how many existing values are sampled per column.
const samplePoolSize = 1000

// zipfS controls hot-key skew. With s=1.1 over 1000 keys the hottest key
// receives roughly 13% of lookups and the top 10 roughly 40%.
const zipfS = 1.1

type colInfo struct {
	Num        int
	Name       string
	Type       string // format_type()
	TypName    string // pg_type.typname
	TypCat     string // pg_type.typcategory
	Unique     bool   // single-column unique/primary key index
	FKTable    string // regclass text of referenced table, if single-column FK
	FKCol      string
	HasDefault bool
}

type tableInfo struct {
	OID       uint32
	Name      string // regclass::text (properly quoted, schema-qualified when needed)
	RelKind   string
	RelTuples float64
	Cols      []colInfo
	byName    map[string]*colInfo
}

// catalog resolves tables/columns and caches sampled value pools.
type catalog struct {
	pool   *pgxpool.Pool
	tables map[string]*tableInfo // key: "schema.name" or "name" as written; nil = not found
	values map[string][]any      // key: table.Name + "\x00" + col
	enums  map[string][]string   // typname -> labels
	fresh  map[string]*freshState
}

func newCatalog(pool *pgxpool.Pool) *catalog {
	return &catalog{pool: pool, tables: map[string]*tableInfo{}, values: map[string][]any{},
		enums: map[string][]string{}, fresh: map[string]*freshState{}}
}

func (c *catalog) table(ctx context.Context, schema, name string) (*tableInfo, error) {
	key := schema + "." + name
	if ti, ok := c.tables[key]; ok {
		return ti, nil
	}
	ident := pgx.Identifier{name}
	if schema != "" {
		ident = pgx.Identifier{schema, name}
	}
	ti := &tableInfo{byName: map[string]*colInfo{}}
	err := c.pool.QueryRow(ctx, `SELECT c.oid, c.oid::regclass::text, c.relkind::text, greatest(c.reltuples, 0)::float8
		FROM pg_class c WHERE c.oid = to_regclass($1)`, ident.Sanitize()).
		Scan(&ti.OID, &ti.Name, &ti.RelKind, &ti.RelTuples)
	if err == pgx.ErrNoRows {
		c.tables[key] = nil
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve table %s: %w", ident.Sanitize(), err)
	}
	rows, err := c.pool.Query(ctx, `SELECT a.attnum, a.attname, format_type(a.atttypid, a.atttypmod), t.typname, t.typcategory::text,
			(a.atthasdef OR a.attidentity <> '' OR a.attgenerated <> '')
		FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, ti.OID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ci colInfo
		if err := rows.Scan(&ci.Num, &ci.Name, &ci.Type, &ci.TypName, &ci.TypCat, &ci.HasDefault); err != nil {
			rows.Close()
			return nil, err
		}
		ti.Cols = append(ti.Cols, ci)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byNum := map[int]*colInfo{}
	for i := range ti.Cols {
		ti.byName[ti.Cols[i].Name] = &ti.Cols[i]
		byNum[ti.Cols[i].Num] = &ti.Cols[i]
	}
	rows, err = c.pool.Query(ctx, `SELECT i.indkey[0]::int FROM pg_index i
		WHERE i.indrelid = $1 AND i.indisunique AND i.indnkeyatts = 1 AND i.indexprs IS NULL`, ti.OID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err == nil && byNum[n] != nil {
			byNum[n].Unique = true
		}
	}
	rows.Close()
	rows, err = c.pool.Query(ctx, `SELECT con.conkey[1]::int, con.confrelid::regclass::text,
			(SELECT attname FROM pg_attribute WHERE attrelid = con.confrelid AND attnum = con.confkey[1])
		FROM pg_constraint con WHERE con.conrelid = $1 AND con.contype = 'f' AND array_length(con.conkey, 1) = 1`, ti.OID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n int
		var ft, fc string
		if err := rows.Scan(&n, &ft, &fc); err == nil && byNum[n] != nil {
			byNum[n].FKTable, byNum[n].FKCol = ft, fc
		}
	}
	rows.Close()
	c.tables[key] = ti
	return ti, nil
}

// tableByRegclass resolves a table already named by its regclass text (e.g. FK targets).
func (c *catalog) tableByRegclass(ctx context.Context, reg string) (*tableInfo, error) {
	for _, ti := range c.tables {
		if ti != nil && ti.Name == reg {
			return ti, nil
		}
	}
	var schema, name string
	if err := c.pool.QueryRow(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, reg).Scan(&schema, &name); err != nil {
		return nil, err
	}
	return c.table(ctx, schema, name)
}

// sample returns up to samplePoolSize existing non-null values of ti.col, shuffled.
func (c *catalog) sample(ctx context.Context, ti *tableInfo, col string) ([]any, error) {
	key := ti.Name + "\x00" + col
	if v, ok := c.values[key]; ok {
		return v, nil
	}
	qc := pgx.Identifier{col}.Sanitize()
	var vals []any
	var err error
	if ti.RelTuples > 20000 && (ti.RelKind == "r" || ti.RelKind == "m") {
		pct := 100 * 3 * samplePoolSize / ti.RelTuples
		if pct > 100 {
			pct = 100
		}
		vals, err = c.collect(ctx, fmt.Sprintf("SELECT %s FROM %s TABLESAMPLE SYSTEM (%s) WHERE %s IS NOT NULL LIMIT %d",
			qc, ti.Name, strconv.FormatFloat(pct, 'f', 4, 64), qc, samplePoolSize))
	}
	if err != nil || len(vals) < 50 {
		vals, err = c.collect(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL ORDER BY random() LIMIT %d",
			qc, ti.Name, qc, samplePoolSize))
		if err != nil {
			return nil, fmt.Errorf("sample %s.%s: %w", ti.Name, col, err)
		}
	}
	mrand.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
	c.values[key] = vals
	return vals, nil
}

func (c *catalog) collect(ctx context.Context, sql string) ([]any, error) {
	rows, err := c.pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vals := make([]any, 0, samplePoolSize)
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			return nil, err
		}
		vals = append(vals, v[0])
	}
	return vals, rows.Err()
}

func (c *catalog) enumLabels(ctx context.Context, typname string) ([]string, error) {
	if l, ok := c.enums[typname]; ok {
		return l, nil
	}
	rows, err := c.pool.Query(ctx, `SELECT e.enumlabel FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid
		WHERE t.typname = $1 ORDER BY e.enumsortorder`, typname)
	if err != nil {
		return nil, err
	}
	labels, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	c.enums[typname] = labels
	return labels, nil
}

// ---------- value generators ----------

// gen produces one parameter value. Generators are only ever called from the
// scheduling goroutine of their query, so they may use that query's *Rand.
type gen func() any

// zipfGen picks from vals with Zipf skew (vals[0] hottest).
func zipfGen(r *mrand.Rand, vals []any) gen {
	if len(vals) == 1 {
		v := vals[0]
		return func() any { return v }
	}
	z := mrand.NewZipf(r, zipfS, 1, uint64(len(vals)-1))
	return func() any { return vals[z.Uint64()] }
}

func uniformGen(r *mrand.Rand, vals []any) gen {
	return func() any { return vals[r.IntN(len(vals))] }
}

// freshState hands out values that do not collide with existing rows.
type freshState struct {
	base    int64
	counter atomic.Int64
	prefix  string
}

func (c *catalog) freshGen(ctx context.Context, ti *tableInfo, ci *colInfo) (gen, error) {
	key := ti.Name + "\x00" + ci.Name
	fs := c.fresh[key]
	if fs == nil {
		fs = &freshState{prefix: randPrefix()}
		switch ci.TypName {
		case "int2", "int4", "int8", "numeric", "float4", "float8":
			var mx *float64
			q := fmt.Sprintf("SELECT max(%s)::float8 FROM %s", pgx.Identifier{ci.Name}.Sanitize(), ti.Name)
			if err := c.pool.QueryRow(ctx, q).Scan(&mx); err != nil {
				return nil, err
			}
			if mx != nil {
				fs.base = int64(*mx)
			}
			// Leave headroom so the table's own sequence does not collide with us soon.
			fs.base += 1_000_000
		}
		c.fresh[key] = fs
	}
	switch ci.TypName {
	case "int8":
		return func() any { return fs.base + fs.counter.Add(1) }, nil
	case "int4":
		return func() any { return int32(fs.base + fs.counter.Add(1)) }, nil
	case "int2":
		return func() any { return int16(fs.base + fs.counter.Add(1)) }, nil
	case "numeric", "float4", "float8":
		return func() any { return float64(fs.base + fs.counter.Add(1)) }, nil
	case "text", "varchar", "bpchar", "citext", "name":
		limit := typmodLen(ci.Type)
		return func() any {
			s := fs.prefix + strconv.FormatInt(fs.counter.Add(1), 36)
			if strings.Contains(strings.ToLower(ci.Name), "email") {
				s += "@example.invalid"
			}
			if limit > 0 && len(s) > limit {
				s = s[len(s)-limit:]
			}
			return s
		}, nil
	case "uuid":
		return func() any { return randUUID() }, nil
	case "timestamptz", "timestamp":
		t0 := time.Now()
		return func() any { return t0.Add(time.Duration(fs.counter.Add(1)) * time.Microsecond) }, nil
	}
	return nil, fmt.Errorf("cannot generate unique values of type %s", ci.Type)
}

func randPrefix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("r%x-", b)
}

func randUUID() [16]byte {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// typmodLen extracts n from "character varying(n)" / "character(n)".
func typmodLen(formatType string) int {
	i := strings.IndexByte(formatType, '(')
	j := strings.IndexByte(formatType, ')')
	if i < 0 || j < i {
		return 0
	}
	n, _ := strconv.Atoi(formatType[i+1 : j])
	return n
}

var castTypnames = map[string]string{
	"int": "int4", "integer": "int4", "int4": "int4", "bigint": "int8", "int8": "int8",
	"smallint": "int2", "int2": "int2", "text": "text", "varchar": "varchar", "character varying": "varchar",
	"numeric": "numeric", "decimal": "numeric", "real": "float4", "float4": "float4",
	"double precision": "float8", "float8": "float8", "bool": "bool", "boolean": "bool",
	"date": "date", "timestamp": "timestamp", "timestamp without time zone": "timestamp",
	"timestamptz": "timestamptz", "timestamp with time zone": "timestamptz", "uuid": "uuid",
	"json": "json", "jsonb": "jsonb", "bytea": "bytea",
}

// typedGen produces plausible random values for a base type.
func (c *catalog) typedGen(ctx context.Context, r *mrand.Rand, typname, typcat, formatType string) (gen, error) {
	switch typname {
	case "int2":
		return func() any { return int16(r.IntN(32000)) }, nil
	case "int4":
		return func() any { return int32(r.IntN(1_000_000)) }, nil
	case "int8":
		return func() any { return r.Int64N(1_000_000_000) }, nil
	case "numeric", "float4", "float8":
		return func() any { return float64(r.IntN(100000)) / 100 }, nil
	case "text", "varchar", "bpchar", "citext", "name":
		limit := typmodLen(formatType)
		return func() any {
			n := 8 + r.IntN(9)
			if limit > 0 && n > limit {
				n = limit
			}
			b := make([]byte, n)
			for i := range b {
				b[i] = byte('a' + r.IntN(26))
			}
			return string(b)
		}, nil
	case "bool":
		return func() any { return r.IntN(2) == 0 }, nil
	case "timestamptz", "timestamp", "date":
		now := time.Now()
		return func() any { return now.Add(-time.Duration(r.Int64N(int64(30 * 24 * time.Hour)))) }, nil
	case "uuid":
		return func() any { return randUUID() }, nil
	case "json", "jsonb":
		return func() any { return "{}" }, nil
	case "bytea":
		return func() any {
			b := make([]byte, 16)
			for i := range b {
				b[i] = byte(r.Uint32())
			}
			return b
		}, nil
	}
	if typcat == "E" {
		labels, err := c.enumLabels(ctx, typname)
		if err != nil || len(labels) == 0 {
			return nil, fmt.Errorf("enum %s has no labels", typname)
		}
		return func() any { return labels[r.IntN(len(labels))] }, nil
	}
	return nil, fmt.Errorf("no generator for type %s", formatType)
}
