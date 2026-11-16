package profile

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// collectTables reads table sizes and per-column pg_stats. It never reads
// table rows: only pg_class, pg_attribute and the pg_stats view.
// Partitioned parents (relkind 'p') hold no rows and are not listed; their
// partitions are ordinary tables and are.
func collectTables(ctx context.Context, tx pgx.Tx) ([]model.TableProfile, error) {
	rows, err := tx.Query(ctx, `/* pg-rehearse:profile */
SELECT c.oid, n.nspname, c.relname, greatest(c.reltuples, 0)::bigint, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND `+userNamespaceFilter+` AND `+notExtensionMember("c.oid", "pg_class")+`
ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	type tbl struct {
		oid uint32
		t   model.TableProfile
	}
	var tbls []tbl
	for rows.Next() {
		var x tbl
		if err := rows.Scan(&x.oid, &x.t.Schema, &x.t.Name, &x.t.RowEstimate, &x.t.TotalBytes); err != nil {
			rows.Close()
			return nil, err
		}
		tbls = append(tbls, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.TableProfile, 0, len(tbls))
	for _, x := range tbls {
		cols, err := collectColumns(ctx, tx, x.oid, x.t.Schema, x.t.Name)
		if err != nil {
			return nil, fmt.Errorf("columns of %s: %w", x.t.QualifiedName(), err)
		}
		x.t.Columns = cols
		out = append(out, x.t)
	}
	return out, nil
}

func collectColumns(ctx context.Context, tx pgx.Tx, relid uint32, schema, table string) ([]model.ColumnProfile, error) {
	rows, err := tx.Query(ctx, `/* pg-rehearse:profile */
SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
  s.null_frac, s.n_distinct, s.avg_width, s.correlation,
  s.most_common_vals::text, s.most_common_freqs::text, s.histogram_bounds::text
FROM pg_attribute a
LEFT JOIN pg_stats s ON s.schemaname = $2 AND s.tablename = $3 AND s.attname = a.attname AND NOT s.inherited
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, relid, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []model.ColumnProfile
	for rows.Next() {
		var c model.ColumnProfile
		var nullFrac, nDistinct, corr *float64
		var avgWidth *int32
		var mcv, mcf, hist *string
		if err := rows.Scan(&c.Name, &c.DataType, &c.Nullable, &nullFrac, &nDistinct, &avgWidth, &corr,
			&mcv, &mcf, &hist); err != nil {
			return nil, err
		}
		if nullFrac != nil {
			c.NullFrac = *nullFrac
		}
		if nDistinct != nil {
			c.NDistinct = *nDistinct
		}
		if avgWidth != nil {
			c.AvgWidth = int(*avgWidth)
		}
		if corr != nil {
			c.Correlation = *corr
		}
		if mcv != nil && mcf != nil {
			vals, err1 := ParseArrayLiteral(*mcv)
			freqs, err2 := parseFloatArray(*mcf)
			if err1 == nil && err2 == nil && len(vals) == len(freqs) {
				c.MostCommonVals, c.MostCommonFreqs = vals, freqs
			}
		}
		if hist != nil {
			if vals, err := ParseArrayLiteral(*hist); err == nil {
				c.HistogramBounds = vals
			}
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}
