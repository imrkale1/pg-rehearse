package synth

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// kind is the generator family for a column's (base) type.
type kind int

const (
	kUnknown kind = iota
	kInt
	kNumeric
	kFloat
	kBool
	kText
	kUUID
	kTimestamp
	kTimestamptz
	kDate
	kTime
	kJSON
	kBytea
	kInterval
	kInet
	kEnum
	kArray
)

func (k kind) String() string {
	return [...]string{"unknown", "int", "numeric", "float", "bool", "text", "uuid", "timestamp",
		"timestamptz", "date", "time", "json", "bytea", "interval", "inet", "enum", "array"}[k]
}

// typeInfo is the resolved description of a column type (domains unwrapped).
type typeInfo struct {
	kind     kind
	name     string // base type name, e.g. int4
	intBits  int    // 16/32/64 for kInt
	maxLen   int    // varchar(n)/char(n); 0 = unbounded
	prec     int    // numeric precision (0 = unconstrained)
	scale    int    // numeric scale (-1 = unconstrained)
	labels   []string
	elem     *typeInfo // for arrays
	elemName string
}

type colInfo struct {
	num        int16
	name       string
	typeSQL    string // format_type output, usable in casts
	typ        typeInfo
	notNull    bool
	hasDefault bool
	defExpr    string
	identity   string // '', 'a', 'd'
	generated  string // '', 's'
	unique     bool   // single-column unique or primary key index
	pk         bool
}

func (c *colInfo) sequenceBacked() bool {
	return c.identity != "" || strings.Contains(c.defExpr, "nextval(")
}

type fkInfo struct {
	name     string
	def      string
	child    *tableInfo
	parent   *tableInfo
	cols     []*colInfo
	refCols  []*colInfo
	deferred bool // filled by UPDATE after all tables are loaded (self-reference / cycle)
}

func (f *fkInfo) allNullable() bool {
	for _, c := range f.cols {
		if c.notNull {
			return false
		}
	}
	return true
}

type checkInfo struct {
	name string
	def  string // pg_get_constraintdef
	expr string // pg_get_expr(conbin)
	cols []*colInfo
}

type indexInfo struct {
	name string // regclass text
	def  string
}

type tableInfo struct {
	oid         uint32
	schema      string
	name        string
	ident       string // quoted schema.name
	relkind     string
	isPartition bool
	cols        []*colInfo
	byNum       map[int16]*colInfo
	byName      map[string]*colInfo
	fks         []*fkInfo // outgoing
	checks      []checkInfo
	uniqueSets  [][]*colInfo // multi-column unique / pk indexes (plain columns, no predicate)
	pkCols      []*colInfo
	dropIdx     []indexInfo
	hasUserTrig bool

	rows int64
	keys map[string][][]string // cached parent key tuples by column list
}

func (t *tableInfo) qname() string { return t.schema + "." + t.name }

type catalog struct {
	tables []*tableInfo
	byOID  map[uint32]*tableInfo
	byName map[string]*tableInfo
}

type pgType struct {
	oid, base, elem uint32
	name, typtype   string
	category        string
	typmod          int32
}

func readCatalog(ctx context.Context, pool *pgxpool.Pool) (*catalog, error) {
	cat := &catalog{byOID: map[uint32]*tableInfo{}, byName: map[string]*tableInfo{}}

	// Types and enum labels (small, load them all).
	types := map[uint32]*pgType{}
	rows, err := pool.Query(ctx, `SELECT oid::int8, typname, typtype::text, typbasetype::int8, typelem::int8, typcategory::text, typtypmod FROM pg_type`)
	if err != nil {
		return nil, fmt.Errorf("read pg_type: %w", err)
	}
	for rows.Next() {
		var oid, base, elem int64
		t := &pgType{}
		if err := rows.Scan(&oid, &t.name, &t.typtype, &base, &elem, &t.category, &t.typmod); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read pg_type: %w", err)
		}
		t.oid, t.base, t.elem = uint32(oid), uint32(base), uint32(elem)
		types[t.oid] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_type: %w", err)
	}
	labels := map[uint32][]string{}
	rows, err = pool.Query(ctx, `SELECT enumtypid::int8, enumlabel FROM pg_enum ORDER BY enumtypid, enumsortorder`)
	if err != nil {
		return nil, fmt.Errorf("read pg_enum: %w", err)
	}
	for rows.Next() {
		var oid int64
		var l string
		if err := rows.Scan(&oid, &l); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read pg_enum: %w", err)
		}
		labels[uint32(oid)] = append(labels[uint32(oid)], l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_enum: %w", err)
	}

	// User tables.
	rows, err = pool.Query(ctx, `
SELECT c.oid::int8, n.nspname, c.relname, c.relkind::text, c.relispartition,
       EXISTS (SELECT 1 FROM pg_trigger tg WHERE tg.tgrelid = c.oid AND NOT tg.tgisinternal)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p')
  AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for rows.Next() {
		var oid int64
		t := &tableInfo{byNum: map[int16]*colInfo{}, byName: map[string]*colInfo{}, keys: map[string][][]string{}}
		if err := rows.Scan(&oid, &t.schema, &t.name, &t.relkind, &t.isPartition, &t.hasUserTrig); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: %w", err)
		}
		t.oid = uint32(oid)
		t.ident = pgx.Identifier{t.schema, t.name}.Sanitize()
		cat.tables = append(cat.tables, t)
		cat.byOID[t.oid] = t
		cat.byName[t.qname()] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	if len(cat.tables) == 0 {
		return cat, nil
	}
	oids := make([]int64, 0, len(cat.tables))
	for _, t := range cat.tables {
		oids = append(oids, int64(t.oid))
	}

	// Columns.
	rows, err = pool.Query(ctx, `
SELECT a.attrelid::int8, a.attnum, a.attname, format_type(a.atttypid, a.atttypmod), a.atttypid::int8, a.atttypmod,
       a.attnotnull, a.atthasdef, COALESCE(pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity::text, a.attgenerated::text
FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = ANY($1::int8[]::oid[]) AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	for rows.Next() {
		var rel, typOID int64
		var typmod int32
		c := &colInfo{}
		if err := rows.Scan(&rel, &c.num, &c.name, &c.typeSQL, &typOID, &typmod, &c.notNull, &c.hasDefault, &c.defExpr, &c.identity, &c.generated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read columns: %w", err)
		}
		c.typ = resolveType(types, labels, uint32(typOID), typmod, 0)
		t := cat.byOID[uint32(rel)]
		t.cols = append(t.cols, c)
		t.byNum[c.num] = c
		t.byName[c.name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	// Unique / primary key indexes on plain columns.
	rows, err = pool.Query(ctx, `
SELECT indrelid::int8, indkey::int2[], indisprimary
FROM pg_index WHERE indisunique AND indexprs IS NULL AND indpred IS NULL AND indrelid = ANY($1::int8[]::oid[])
ORDER BY indrelid, indexrelid`, oids)
	if err != nil {
		return nil, fmt.Errorf("read unique indexes: %w", err)
	}
	for rows.Next() {
		var rel int64
		var keys []int16
		var primary bool
		if err := rows.Scan(&rel, &keys, &primary); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read unique indexes: %w", err)
		}
		t := cat.byOID[uint32(rel)]
		var set []*colInfo
		for _, k := range keys {
			if c := t.byNum[k]; c != nil {
				set = append(set, c)
			}
		}
		if len(set) != len(keys) || len(set) == 0 {
			continue
		}
		if primary {
			t.pkCols = set
			for _, c := range set {
				c.pk = len(set) == 1
			}
		}
		if len(set) == 1 {
			set[0].unique = true
		} else {
			t.uniqueSets = append(t.uniqueSets, set)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unique indexes: %w", err)
	}

	// Foreign keys and CHECK constraints.
	rows, err = pool.Query(ctx, `
SELECT conname, contype::text, conrelid::int8, confrelid::int8, COALESCE(conkey, '{}'), COALESCE(confkey, '{}'),
       pg_get_constraintdef(oid), CASE WHEN contype = 'c' THEN pg_get_expr(conbin, conrelid) ELSE '' END
FROM pg_constraint
WHERE contype IN ('f','c') AND conrelid = ANY($1::int8[]::oid[]) AND conparentid = 0 AND conislocal
ORDER BY conrelid, conname`, oids)
	if err != nil {
		return nil, fmt.Errorf("read constraints: %w", err)
	}
	for rows.Next() {
		var name, typ, def, expr string
		var rel, frel int64
		var key, fkey []int16
		if err := rows.Scan(&name, &typ, &rel, &frel, &key, &fkey, &def, &expr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read constraints: %w", err)
		}
		t := cat.byOID[uint32(rel)]
		switch typ {
		case "f":
			p := cat.byOID[uint32(frel)]
			if p == nil {
				continue
			}
			fk := &fkInfo{name: name, def: def, child: t, parent: p}
			for i := range key {
				c, rc := t.byNum[key[i]], p.byNum[fkey[i]]
				if c == nil || rc == nil {
					continue
				}
				fk.cols = append(fk.cols, c)
				fk.refCols = append(fk.refCols, rc)
			}
			if len(fk.cols) > 0 {
				t.fks = append(t.fks, fk)
			}
		case "c":
			ck := checkInfo{name: name, def: def, expr: expr}
			for _, k := range key {
				if c := t.byNum[k]; c != nil {
					ck.cols = append(ck.cols, c)
				}
			}
			t.checks = append(t.checks, ck)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read constraints: %w", err)
	}

	// Secondary indexes that we drop during the load and rebuild afterwards.
	rows, err = pool.Query(ctx, `
SELECT i.indrelid::int8, i.indexrelid::regclass::text, pg_get_indexdef(i.indexrelid)
FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
WHERE i.indrelid = ANY($1::int8[]::oid[]) AND NOT i.indisunique AND NOT i.indisprimary AND NOT i.indisexclusion
  AND c.relkind = 'r'
  AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid)
  AND NOT EXISTS (SELECT 1 FROM pg_inherits h WHERE h.inhrelid = i.indexrelid OR h.inhparent = i.indexrelid)
ORDER BY i.indrelid, i.indexrelid`, oids)
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	for rows.Next() {
		var rel int64
		var ix indexInfo
		if err := rows.Scan(&rel, &ix.name, &ix.def); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read indexes: %w", err)
		}
		t := cat.byOID[uint32(rel)]
		t.dropIdx = append(t.dropIdx, ix)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	return cat, nil
}

func resolveType(types map[uint32]*pgType, labels map[uint32][]string, oid uint32, typmod int32, depth int) typeInfo {
	t := types[oid]
	if t == nil || depth > 8 {
		return typeInfo{kind: kUnknown}
	}
	if t.typtype == "d" {
		if typmod < 0 {
			typmod = t.typmod
		}
		return resolveType(types, labels, t.base, typmod, depth+1)
	}
	ti := typeInfo{name: t.name, scale: -1}
	if t.typtype == "e" {
		ti.kind = kEnum
		ti.labels = labels[oid]
		return ti
	}
	if t.category == "A" && t.elem != 0 {
		e := resolveType(types, labels, t.elem, typmod, depth+1)
		ti.kind = kArray
		ti.elem = &e
		ti.elemName = types[t.elem].name
		return ti
	}
	switch t.name {
	case "int2":
		ti.kind, ti.intBits = kInt, 16
	case "int4":
		ti.kind, ti.intBits = kInt, 32
	case "int8":
		ti.kind, ti.intBits = kInt, 64
	case "numeric":
		ti.kind = kNumeric
		if typmod >= 4 {
			ti.prec = int(((typmod - 4) >> 16) & 0xffff)
			ti.scale = int((typmod - 4) & 0xffff)
		}
	case "float4", "float8":
		ti.kind = kFloat
	case "bool":
		ti.kind = kBool
	case "text", "varchar", "bpchar", "name", "citext":
		ti.kind = kText
		if (t.name == "varchar" || t.name == "bpchar") && typmod >= 4 {
			ti.maxLen = int(typmod - 4)
		}
		if t.name == "name" {
			ti.maxLen = 63
		}
	case "uuid":
		ti.kind = kUUID
	case "timestamp":
		ti.kind = kTimestamp
	case "timestamptz":
		ti.kind = kTimestamptz
	case "date":
		ti.kind = kDate
	case "time", "timetz":
		ti.kind = kTime
	case "json", "jsonb":
		ti.kind = kJSON
	case "bytea":
		ti.kind = kBytea
	case "interval":
		ti.kind = kInterval
	case "inet", "cidr":
		ti.kind = kInet
	}
	return ti
}

var nextvalRe = regexp.MustCompile(`nextval\('((?:[^']|'')+)'`)

// sequenceFor returns the regclass text of the sequence feeding col, or "".
func sequenceFor(ctx context.Context, pool *pgxpool.Pool, t *tableInfo, c *colInfo) (string, error) {
	var seq *string
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence($1, $2)`, t.ident, c.name).Scan(&seq); err != nil {
		return "", fmt.Errorf("table %s column %s: find sequence: %w", t.qname(), c.name, err)
	}
	if seq != nil {
		return *seq, nil
	}
	if m := nextvalRe.FindStringSubmatch(c.defExpr); m != nil {
		return strings.ReplaceAll(m[1], "''", "'"), nil
	}
	return "", nil
}

func colList(cols []*colInfo) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = pgx.Identifier{c.name}.Sanitize()
	}
	return strings.Join(parts, ", ")
}

func colKey(cols []*colInfo) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = c.name
	}
	return strings.Join(parts, "\x00")
}

func sortedTables(ts []*tableInfo) []*tableInfo {
	out := append([]*tableInfo(nil), ts...)
	sort.Slice(out, func(i, j int) bool { return out[i].qname() < out[j].qname() })
	return out
}
