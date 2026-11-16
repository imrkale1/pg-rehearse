package report

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden files")

const indexSQL = "CREATE INDEX orders_created_at_idx ON orders (created_at);"

func dangerousOriginal() model.RehearsalResult {
	return model.RehearsalResult{
		Migration: "demo/migrations/002_index_orders_created.sql",
		Footprint: model.LockFootprint{
			Statements: []model.StatementLocks{{
				SQL:        indexSQL,
				DurationMs: 2140,
				Locks: []model.RelationLock{
					{Relation: "public.orders", Kind: "table", Mode: "ShareLock", BlocksWrites: true},
				},
			}},
			Lint: []model.LintFinding{{
				Rule:       "create-index-not-concurrent",
				Severity:   model.SeverityDanger,
				Statement:  indexSQL,
				Message:    "CREATE INDEX without CONCURRENTLY blocks writes to `orders` for the whole build.",
				Suggestion: "Use CREATE INDEX CONCURRENTLY outside a transaction.",
			}},
		},
		MigrationDurationMs: 3120,
		Impacts: []model.Impact{
			{QueryID: 1, Query: "INSERT INTO orders (user_id, status, total) VALUES ($1, $2, $3) RETURNING id", Feature: "checkout", BaselineP99Ms: 4.2, DuringMaxMs: 3104, StallMs: 3090},
			{QueryID: 2, Query: "INSERT INTO order_items (order_id, product_id, quantity, unit_price) VALUES ($1, $2, $3, $4)", Feature: "checkout", BaselineP99Ms: 2.1, DuringMaxMs: 3080, StallMs: 3050},
			{QueryID: 3, Query: "SELECT * FROM orders WHERE id = $1", Feature: "order page", BaselineP99Ms: 0.8, DuringMaxMs: 1.4},
			{QueryID: 4, Query: "SELECT id, status, total FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2", Feature: "order history", BaselineP99Ms: 2.5, DuringMaxMs: 6.1},
		},
		MaxStallMs: 3090,
		Verdict:    model.VerdictDangerous,
	}
}

func cases() map[string]*model.Report {
	safeImpacts := []model.Impact{
		{QueryID: 1, Query: "INSERT INTO orders ...", Feature: "checkout", BaselineP99Ms: 4.2, DuringMaxMs: 41},
		{QueryID: 3, Query: "SELECT * FROM orders WHERE id = $1", Feature: "order page", BaselineP99Ms: 0.8, DuringMaxMs: 1.9},
		{QueryID: 4, Query: "SELECT ... WHERE user_id = $1", Feature: "order history", BaselineP99Ms: 2.5, DuringMaxMs: 7.3},
	}
	concurrent := model.Fix{
		Rule:        "create-index-not-concurrent",
		Description: "Build the index with CONCURRENTLY so writes keep flowing (must run outside a transaction).",
		Original:    indexSQL,
		Rewritten:   "CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_created_at_idx ON orders (created_at);",
	}

	return map[string]*model.Report{
		"dangerous_fixed": {
			Original: dangerousOriginal(),
			Fixes:    []model.Fix{concurrent},
			FixedResult: &model.RehearsalResult{
				Migration:           "demo/migrations/002_index_orders_created.sql (fixed)",
				MigrationDurationMs: 4870,
				Impacts:             safeImpacts,
				Verdict:             model.VerdictSafe,
			},
			TrafficNote: "peak-hour profile, 1,240 req/s, 1 long report txn",
		},
		"safe": {
			Original: model.RehearsalResult{
				Migration: "demo/migrations/001_add_column_safe.sql",
				Footprint: model.LockFootprint{
					Statements: []model.StatementLocks{{
						SQL:        "ALTER TABLE orders ADD COLUMN gift_message text;",
						DurationMs: 1.3,
						Locks: []model.RelationLock{
							{Relation: "public.orders", Kind: "table", Mode: "AccessExclusiveLock", BlocksReads: true, BlocksWrites: true},
						},
					}},
				},
				MigrationDurationMs: 2.4,
				Impacts:             safeImpacts,
				Verdict:             model.VerdictSafe,
			},
			TrafficNote: "peak-hour profile, 1,240 req/s",
		},
		"migration_error": {
			Original: model.RehearsalResult{
				Migration: "demo/migrations/005_not_null.sql",
				Footprint: model.LockFootprint{
					Lint: []model.LintFinding{{
						Rule:       "set-not-null",
						Severity:   model.SeverityWarning,
						Statement:  "ALTER TABLE orders ALTER COLUMN user_id SET NOT NULL;",
						Message:    "SET NOT NULL scans the whole table while holding an ACCESS EXCLUSIVE lock.",
						Suggestion: "Add a CHECK (user_id IS NOT NULL) NOT VALID constraint, VALIDATE it, then SET NOT NULL.",
					}},
				},
				MigrationError: "ERROR: column \"user_id\" of relation \"orders\" contains null values (SQLSTATE 23502)",
				Verdict:        model.VerdictDangerous,
			},
			TrafficNote: "peak-hour profile, 1,240 req/s",
		},
		"fix_still_fails": {
			Original: model.RehearsalResult{
				Migration: "demo/migrations/004_alter_type.sql",
				Footprint: model.LockFootprint{
					Statements: []model.StatementLocks{{
						SQL:        "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);",
						DurationMs: 8400,
						Locks: []model.RelationLock{
							{Relation: "public.orders", Kind: "table", Mode: "AccessExclusiveLock", BlocksReads: true, BlocksWrites: true},
							{Relation: "public.orders_pkey", Kind: "index", Mode: "AccessExclusiveLock", BlocksReads: true, BlocksWrites: true},
							{Relation: "public.users", Kind: "table", Mode: "ShareRowExclusiveLock", BlocksWrites: true},
						},
					}},
					RewrittenTables: []string{"public.orders"},
					Lint: []model.LintFinding{
						{Rule: "alter-column-type", Severity: model.SeverityDanger, Message: "Changing a column type rewrites `orders` under an ACCESS EXCLUSIVE lock."},
						{Rule: "missing-lock-timeout", Severity: model.SeverityInfo, Message: "No lock_timeout is set; a blocked ALTER queues every query behind it.", Suggestion: "SET lock_timeout = '2s';"},
					},
				},
				MigrationDurationMs: 9800,
				Impacts: []model.Impact{
					{QueryID: 1, Feature: "checkout", BaselineP99Ms: 4.2, DuringMaxMs: 9810, StallMs: 9800, Errors: 12},
					{QueryID: 3, Feature: "order page", BaselineP99Ms: 0.8, DuringMaxMs: 9790, StallMs: 9780},
					{QueryID: 9, Query: "SELECT name || price FROM products WHERE id = $1", BaselineP99Ms: 0.5, DuringMaxMs: 0.9},
				},
				MaxStallMs: 9800,
				Verdict:    model.VerdictDangerous,
			},
			Fixes: []model.Fix{
				{
					Rule:        "lock-timeout",
					Description: "Fail fast instead of queueing traffic behind the lock.",
					Rewritten:   "SET lock_timeout = '2s';",
				},
				{
					Rule:        "alter-column-type",
					Description: "Add a new column, backfill in batches, then swap.",
					Original:    "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);",
					Rewritten:   "ALTER TABLE orders ADD COLUMN total_new numeric(14,2);\n-- backfill in batches, then:\nALTER TABLE orders RENAME COLUMN total TO total_old;\nALTER TABLE orders RENAME COLUMN total_new TO total;",
				},
			},
			FixedResult: &model.RehearsalResult{
				MigrationDurationMs: 2010,
				Impacts: []model.Impact{
					{QueryID: 1, Feature: "checkout", BaselineP99Ms: 4.2, DuringMaxMs: 1250, StallMs: 1200},
					{QueryID: 3, Feature: "order page", BaselineP99Ms: 0.8, DuringMaxMs: 1.1},
				},
				MaxStallMs: 1200,
				Verdict:    model.VerdictDangerous,
			},
			TrafficNote: "peak-hour profile, 1,240 req/s, 1 long report txn",
		},
	}
}

func TestMarkdownGolden(t *testing.T) {
	for name, r := range cases() {
		t.Run(name, func(t *testing.T) {
			got := Markdown(r)
			path := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `go test ./internal/report -update` to create it)", err)
			}
			if got != string(want) {
				t.Errorf("Markdown(%s) mismatch; run with -update to accept.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

func TestMarkdownEssentials(t *testing.T) {
	c := cases()
	checks := map[string][]string{
		"dangerous_fixed": {
			Marker,
			"🔴 This migration would have frozen **checkout** for **3.1s**",
			"Locks `orders` (blocks writes)",
			"✅ Re-tested: checkout max 41ms, no stalls",
			"<details>",
			"```sql\n-- create-index-not-concurrent\nCREATE INDEX CONCURRENTLY",
			"peak-hour profile, 1,240 req/s, 1 long report txn",
		},
		"safe":            {"🟢 Safe", "Locks `orders` (blocks reads and writes)"},
		"migration_error": {"💥 This migration failed", "contains null values"},
		"fix_still_fails": {
			"table `orders` is rewritten",
			"`users` (blocks writes)",
			"❌ Re-tested: the suggested version still froze **checkout** for **1.2s**",
			`\|`,
		},
	}
	for name, wants := range checks {
		md := Markdown(c[name])
		for _, w := range wants {
			if !strings.Contains(md, w) {
				t.Errorf("%s: missing %q in\n%s", name, w, md)
			}
		}
	}
	if strings.Contains(Markdown(c["safe"]), "<details>") {
		t.Error("safe report should not contain a fix section")
	}
	if strings.Contains(Markdown(c["fix_still_fails"]), "orders_pkey") {
		t.Error("index locks should be folded into their table")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	r := cases()["dangerous_fixed"]
	b, err := JSON(r)
	if err != nil {
		t.Fatal(err)
	}
	var back model.Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Original.MaxStallMs != r.Original.MaxStallMs || back.FixedResult == nil || len(back.Fixes) != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestFormatDuration(t *testing.T) {
	for ms, want := range map[float64]string{
		0: "0ms", 0.42: "0.4ms", 4: "4ms", 41.3: "41ms", 999: "999ms",
		1000: "1s", 3090: "3.1s", 59990: "1m00s", 65000: "1m05s",
	} {
		if got := FormatDuration(ms); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", ms, got, want)
		}
	}
}

func TestFenceEscapes(t *testing.T) {
	if got := fence("sql", "a ``` b"); !strings.HasPrefix(got, "````sql") {
		t.Errorf("fence did not lengthen: %q", got)
	}
}
