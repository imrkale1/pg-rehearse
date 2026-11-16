// Command pg-rehearse rehearses Postgres migrations against a synthetic,
// production-shaped copy under realistic traffic before they reach production.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/pg-rehearse/pg-rehearse/internal/locks"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
	"github.com/pg-rehearse/pg-rehearse/internal/profile"
	"github.com/pg-rehearse/pg-rehearse/internal/rehearse"
	"github.com/pg-rehearse/pg-rehearse/internal/report"
)

var version = "dev"

// exitError carries a process exit code without printing usage.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root := &cobra.Command{
		Use:           "pg-rehearse",
		Short:         "Catch migration outages on the pull request, not in production",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(profileCmd(), rehearseCmd(), lintCmd())
	if err := root.ExecuteContext(ctx); err != nil {
		if e, ok := err.(exitError); ok {
			os.Exit(e.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func profileCmd() *cobra.Command {
	var (
		dsn, out  string
		interval  time.Duration
		top       int
		sensitive []string
		features  []string
		longMin   float64
	)
	c := &cobra.Command{
		Use:   "profile",
		Short: "Capture a privacy-safe statistics profile from a production database (read-only)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fm := map[string]string{}
			for _, f := range features {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					return fmt.Errorf("--feature must be regex=label, got %q", f)
				}
				fm[k] = v
			}
			pool, err := pgxpool.New(cmd.Context(), dsn)
			if err != nil {
				return err
			}
			defer pool.Close()
			fmt.Fprintf(os.Stderr, "pg-rehearse: sampling statistics for %s...\n", interval)
			p, err := profile.Collect(cmd.Context(), pool, profile.Options{
				SampleInterval: interval, TopQueries: top, SensitiveCols: sensitive,
				FeatureMap: fm, LongTxnMinSec: longMin,
			})
			if err != nil {
				return err
			}
			if err := profile.Save(out, p); err != nil {
				return err
			}
			var rps float64
			for _, q := range p.Queries {
				rps += q.CallsPerSec
			}
			fmt.Fprintf(os.Stderr, "pg-rehearse: wrote %s — %d tables, %d query shapes (%.0f q/s), %d long txns\n",
				out, len(p.Tables), len(p.Queries), rps, len(p.LongTxns))
			return nil
		},
	}
	c.Flags().StringVar(&dsn, "dsn", os.Getenv("PGR_PROD_DSN"), "production DSN (read-only user is enough)")
	c.Flags().StringVar(&out, "out", "profile.json", "output file")
	c.Flags().DurationVar(&interval, "interval", 60*time.Second, "sampling window for query rates")
	c.Flags().IntVar(&top, "top", 50, "keep the top-N query shapes")
	c.Flags().StringSliceVar(&sensitive, "sensitive", nil, "extra sensitive columns (table.column)")
	c.Flags().StringArrayVar(&features, "feature", nil, "map queries to features: regex=label (repeatable)")
	c.Flags().Float64Var(&longMin, "long-txn-min", 2, "transactions older than this many seconds count as long")
	_ = c.MarkFlagRequired("dsn")
	return c
}

func rehearseCmd() *cobra.Command {
	var (
		profPath, shadow, migPath, format, outPath, failOn string
		scale, rate                                       float64
		duration                                          time.Duration
		seed                                              int64
		rebuild, noFix                                    bool
		conns                                             int
	)
	c := &cobra.Command{
		Use:   "rehearse",
		Short: "Rehearse a migration on a synthetic copy under production-like traffic",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := profile.Load(profPath)
			if err != nil {
				return err
			}
			mig, err := os.ReadFile(migPath)
			if err != nil {
				return err
			}
			warm := duration * 4 / 10
			rep, err := rehearse.Run(cmd.Context(), rehearse.Options{
				Profile: p, ShadowDSN: shadow, MigrationSQL: string(mig),
				Scale: scale, Seed: seed, Rebuild: rebuild,
				Warmup: warm, Observe: duration - warm,
				RateMultiplier: rate, MaxConns: conns, TestFixes: !noFix,
				Log: os.Stderr,
			})
			if err != nil {
				return err
			}
			var b []byte
			switch format {
			case "markdown", "md":
				b = []byte(report.Markdown(rep))
			case "json":
				if b, err = report.JSON(rep); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown --format %q", format)
			}
			if outPath == "" || outPath == "-" {
				os.Stdout.Write(b)
			} else if err := os.WriteFile(outPath, b, 0o644); err != nil {
				return err
			}
			if failed(rep.Original.Verdict, failOn) {
				return exitError{2}
			}
			return nil
		},
	}
	c.Flags().StringVar(&profPath, "profile", "profile.json", "profile captured with `pg-rehearse profile`")
	c.Flags().StringVar(&shadow, "shadow-dsn", os.Getenv("PGR_SHADOW_DSN"), "DSN of a disposable Postgres server for the rehearsal")
	c.Flags().StringVar(&migPath, "migration", "", "migration SQL file")
	c.Flags().Float64Var(&scale, "scale", 1, "data size relative to production (0.1 = 10%)")
	c.Flags().Float64Var(&rate, "rate", 1, "traffic multiplier (2 = twice production rate)")
	c.Flags().DurationVar(&duration, "duration", 30*time.Second, "total traffic window (40% baseline, then migration)")
	c.Flags().Int64Var(&seed, "seed", 1, "random seed for synthetic data")
	c.Flags().BoolVar(&rebuild, "rebuild", false, "rebuild the cached synthetic template")
	c.Flags().BoolVar(&noFix, "no-fix", false, "do not suggest and re-test a safer migration")
	c.Flags().IntVar(&conns, "max-conns", 64, "connection pool size for simulated traffic")
	c.Flags().StringVar(&format, "format", "markdown", "markdown | json")
	c.Flags().StringVar(&outPath, "out", "-", "output file (- for stdout)")
	c.Flags().StringVar(&failOn, "fail-on", "dangerous", "exit 2 when verdict is at least: risky | dangerous | never")
	_ = c.MarkFlagRequired("migration")
	_ = c.MarkFlagRequired("shadow-dsn")
	return c
}

func failed(v model.Verdict, failOn string) bool {
	switch failOn {
	case "never":
		return false
	case "risky":
		return v != model.VerdictSafe
	default:
		return v == model.VerdictDangerous
	}
}

func lintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint <migration.sql>...",
		Short: "Static checks only (no database needed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			worst := 0
			for _, path := range args {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, f := range locks.Lint(string(b)) {
					fmt.Printf("%s: [%s] %s: %s\n", path, f.Severity, f.Rule, f.Message)
					if f.Suggestion != "" {
						fmt.Printf("    suggestion: %s\n", f.Suggestion)
					}
					if f.Severity == model.SeverityDanger {
						worst = 2
					}
				}
			}
			if worst > 0 {
				return exitError{worst}
			}
			return nil
		},
	}
}
