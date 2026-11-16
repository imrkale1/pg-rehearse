// Package report renders a rehearsal Report as a GitHub PR comment (Markdown)
// or as machine-readable JSON.
package report

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Marker is a hidden HTML comment placed at the top of every rendered comment.
// CI integrations search for it to update a single sticky PR comment in place.
const Marker = "<!-- pg-rehearse:report -->"

// maxImpactRows caps the impact table so the comment stays skimmable.
const maxImpactRows = 6

// JSON returns the report as indented JSON (with a trailing newline).
func JSON(r *model.Report) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Markdown renders the PR comment.
func Markdown(r *model.Report) string {
	if r == nil {
		return Marker + "\n### ⚪ pg-rehearse produced no result\n"
	}
	var b strings.Builder
	o := &r.Original
	b.WriteString(Marker + "\n")
	b.WriteString("### " + headline(o) + "\n\n")

	// Context line: which migration, under what traffic.
	var ctx []string
	if name := migrationName(o.Migration); name != "" {
		ctx = append(ctx, "`"+name+"`")
	}
	if o.MigrationError == "" && o.MigrationDurationMs > 0 {
		ctx = append(ctx, "ran in "+FormatDuration(o.MigrationDurationMs))
	}
	if r.TrafficNote != "" {
		ctx = append(ctx, "rehearsed under "+r.TrafficNote)
	}
	if len(ctx) > 0 {
		b.WriteString(strings.Join(ctx, " · ") + "\n\n")
	}

	if o.MigrationError != "" {
		b.WriteString("The migration failed on the production-shaped shadow database, so it would most likely fail in production too:\n\n")
		b.WriteString(fence("text", strings.TrimSpace(o.MigrationError)) + "\n")
	}

	writeImpactTable(&b, r)
	writeFootprint(&b, &o.Footprint)
	writeLint(&b, o.Footprint.Lint)
	writeFix(&b, r)

	b.WriteString("---\n")
	b.WriteString("<sub>🎭 <b>pg-rehearse</b> ran this migration against a synthetic, production-shaped copy of your database " +
		"while replaying realistic traffic. No production data was read or modified.</sub>\n")
	return b.String()
}

// headline is the one-line verdict at the top of the comment.
func headline(o *model.RehearsalResult) string {
	if o.MigrationError != "" {
		return "💥 This migration failed during rehearsal"
	}
	worst, ok := worstImpact(o)
	switch o.Verdict {
	case model.VerdictDangerous:
		if ok && worst.StallMs > 0 {
			return fmt.Sprintf("🔴 This migration would have frozen **%s** for **%s**", worst.Feature, FormatDuration(worst.StallMs))
		}
		if ok && worst.Errors > 0 {
			return fmt.Sprintf("🔴 This migration would have caused **%d failed requests** in **%s**", worst.Errors, worst.Feature)
		}
		return "🔴 This migration is dangerous to run in production"
	case model.VerdictRisky:
		if ok && worst.StallMs > 0 {
			return fmt.Sprintf("🟠 This migration would have stalled **%s** for **%s**", worst.Feature, FormatDuration(worst.StallMs))
		}
		if ok && worst.DuringMaxMs > worst.BaselineP99Ms {
			return fmt.Sprintf("🟠 This migration would have slowed **%s** to **%s** (normally %s p99)",
				worst.Feature, FormatDuration(worst.DuringMaxMs), FormatDuration(worst.BaselineP99Ms))
		}
		return "🟠 This migration is risky to run under load"
	default:
		return "🟢 Safe — no feature was blocked while this migration ran"
	}
}

// featureImpact is an Impact aggregated over all queries sharing a feature label.
type featureImpact struct {
	Feature       string
	BaselineP99Ms float64
	DuringMaxMs   float64
	StallMs       float64
	Errors        int
}

func featureName(im model.Impact) string {
	if im.Feature != "" {
		return im.Feature
	}
	q := strings.Join(strings.Fields(im.Query), " ")
	if q == "" {
		return fmt.Sprintf("query %d", im.QueryID)
	}
	const max = 40
	if r := []rune(q); len(r) > max {
		q = string(r[:max-1]) + "…"
	}
	return "`" + strings.ReplaceAll(q, "`", "'") + "`"
}

// byFeature merges impacts per feature, worst first.
func byFeature(impacts []model.Impact) []featureImpact {
	idx := map[string]int{}
	var out []featureImpact
	for _, im := range impacts {
		name := featureName(im)
		i, ok := idx[name]
		if !ok {
			idx[name] = len(out)
			out = append(out, featureImpact{Feature: name})
			i = len(out) - 1
		}
		f := &out[i]
		f.BaselineP99Ms = math.Max(f.BaselineP99Ms, im.BaselineP99Ms)
		f.DuringMaxMs = math.Max(f.DuringMaxMs, im.DuringMaxMs)
		f.StallMs = math.Max(f.StallMs, im.StallMs)
		f.Errors += im.Errors
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.StallMs != y.StallMs {
			return x.StallMs > y.StallMs
		}
		if x.Errors != y.Errors {
			return x.Errors > y.Errors
		}
		return slowdown(x) > slowdown(y)
	})
	return out
}

func slowdown(f featureImpact) float64 {
	if f.BaselineP99Ms <= 0 {
		return f.DuringMaxMs
	}
	return f.DuringMaxMs / f.BaselineP99Ms
}

func worstImpact(o *model.RehearsalResult) (featureImpact, bool) {
	fs := byFeature(o.Impacts)
	if len(fs) == 0 {
		return featureImpact{}, false
	}
	return fs[0], true
}

func writeImpactTable(b *strings.Builder, r *model.Report) {
	features := byFeature(r.Original.Impacts)
	if len(features) == 0 {
		return
	}
	var fixed map[string]featureImpact
	if fr := r.FixedResult; fr != nil && fr.MigrationError == "" && len(fr.Impacts) > 0 {
		fixed = map[string]featureImpact{}
		for _, f := range byFeature(fr.Impacts) {
			fixed[f.Feature] = f
		}
	}
	hasErrors := false
	for _, f := range features {
		if f.Errors > 0 {
			hasErrors = true
		}
	}
	if fr := r.FixedResult; fr != nil {
		for _, f := range fixed {
			if f.Errors > 0 {
				hasErrors = true
			}
		}
	}

	b.WriteString("| Feature | Normal p99 | Worst during migration | Stall |")
	if hasErrors {
		b.WriteString(" Errors |")
	}
	if fixed != nil {
		b.WriteString(" With safer version |")
	}
	b.WriteString("\n|---|--:|--:|--:|")
	if hasErrors {
		b.WriteString("--:|")
	}
	if fixed != nil {
		b.WriteString("--:|")
	}
	b.WriteString("\n")

	shown := features
	if len(shown) > maxImpactRows {
		shown = shown[:maxImpactRows]
	}
	for _, f := range shown {
		fmt.Fprintf(b, "| %s | %s | %s | %s |", cell(f.Feature), FormatDuration(f.BaselineP99Ms),
			FormatDuration(f.DuringMaxMs), stallCell(f.StallMs))
		if hasErrors {
			fmt.Fprintf(b, " %d |", f.Errors)
		}
		if fixed != nil {
			if g, ok := fixed[f.Feature]; ok {
				s := FormatDuration(g.DuringMaxMs)
				if g.StallMs > 0 {
					s += ", stalled " + FormatDuration(g.StallMs)
				}
				if g.Errors > 0 {
					s += fmt.Sprintf(", %d errors", g.Errors)
				}
				fmt.Fprintf(b, " %s |", s)
			} else {
				b.WriteString(" – |")
			}
		}
		b.WriteString("\n")
	}
	if n := len(features) - len(shown); n > 0 {
		fmt.Fprintf(b, "\n<sub>+ %d more %s not shown.</sub>\n", n, plural(n, "feature", "features"))
	}
	b.WriteString("\n")
}

func stallCell(ms float64) string {
	if ms <= 0 {
		return "–"
	}
	return "**" + FormatDuration(ms) + "**"
}

// relLock is the strongest blocking effect a migration has on one relation.
type relLock struct {
	name   string
	reads  bool
	writes bool
}

func writeFootprint(b *strings.Builder, fp *model.LockFootprint) {
	if len(fp.Statements) == 0 && len(fp.RewrittenTables) == 0 {
		return
	}
	idx := map[string]int{}
	var rels []relLock
	for _, st := range fp.Statements {
		for _, l := range st.Locks {
			if !l.BlocksReads && !l.BlocksWrites {
				continue
			}
			if l.Kind != "" && l.Kind != "table" && l.Kind != "view" {
				// Index/sequence locks follow their table; keep the sentence short.
				continue
			}
			name := shortRel(l.Relation)
			i, ok := idx[name]
			if !ok {
				idx[name] = len(rels)
				rels = append(rels, relLock{name: name})
				i = len(rels) - 1
			}
			rels[i].reads = rels[i].reads || l.BlocksReads
			rels[i].writes = rels[i].writes || l.BlocksWrites || l.BlocksReads
		}
	}

	var s string
	if len(rels) == 0 {
		s = "Takes no locks that block reads or writes"
	} else {
		parts := make([]string, len(rels))
		for i, r := range rels {
			what := "blocks writes"
			if r.reads {
				what = "blocks reads and writes"
			}
			parts[i] = fmt.Sprintf("`%s` (%s)", r.name, what)
		}
		s = "Locks " + strings.Join(parts, ", ")
	}
	if n := len(fp.RewrittenTables); n > 0 {
		names := make([]string, n)
		for i, t := range fp.RewrittenTables {
			names[i] = "`" + shortRel(t) + "`"
		}
		if n == 1 {
			s += " — table " + names[0] + " is rewritten"
		} else {
			s += " — tables " + joinAnd(names) + " are rewritten"
		}
	}
	b.WriteString("**Lock footprint:** " + s + ".\n\n")
}

func severityIcon(s model.Severity) string {
	switch s {
	case model.SeverityDanger:
		return "🔴"
	case model.SeverityWarning:
		return "🟠"
	default:
		return "ℹ️"
	}
}

func writeLint(b *strings.Builder, findings []model.LintFinding) {
	if len(findings) == 0 {
		return
	}
	rank := map[model.Severity]int{model.SeverityDanger: 0, model.SeverityWarning: 1, model.SeverityInfo: 2}
	fs := append([]model.LintFinding(nil), findings...)
	sort.SliceStable(fs, func(i, j int) bool { return rank[fs[i].Severity] < rank[fs[j].Severity] })

	b.WriteString("**Findings**\n\n")
	for _, f := range fs {
		fmt.Fprintf(b, "- %s `%s` — %s", severityIcon(f.Severity), f.Rule, oneLine(f.Message))
		if f.Suggestion != "" {
			fmt.Fprintf(b, " _%s_", oneLine(f.Suggestion))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeFix(b *strings.Builder, r *model.Report) {
	if len(r.Fixes) == 0 {
		if r.Original.Verdict != model.VerdictSafe && r.Original.MigrationError == "" {
			b.WriteString("No automatic rewrite is available for this migration; consider running it in a maintenance window or splitting it into smaller steps.\n\n")
		}
		return
	}

	b.WriteString(retestLine(r) + "\n\n")

	summary := "Suggested safer migration"
	if len(r.Fixes) > 1 {
		summary += fmt.Sprintf(" (%d changes)", len(r.Fixes))
	}
	fmt.Fprintf(b, "<details>\n<summary>%s</summary>\n\n", summary)
	for _, f := range r.Fixes {
		fmt.Fprintf(b, "- **%s** — %s\n", f.Rule, oneLine(f.Description))
	}
	b.WriteString("\n")
	var sql strings.Builder
	for i, f := range r.Fixes {
		if i > 0 {
			sql.WriteString("\n\n")
		}
		sql.WriteString("-- " + f.Rule + "\n")
		sql.WriteString(strings.TrimSpace(f.Rewritten))
	}
	b.WriteString(fence("sql", sql.String()))
	b.WriteString("\n</details>\n\n")
}

// retestLine summarises how the suggested rewrite behaved when rehearsed.
func retestLine(r *model.Report) string {
	fr := r.FixedResult
	if fr == nil {
		return "⚠️ A safer version is suggested below, but it was **not re-tested**."
	}
	if fr.MigrationError != "" {
		return "❌ Re-test failed: the suggested version errored — `" + oneLine(fr.MigrationError) + "`. Review it by hand."
	}
	focus := ""
	if w, ok := worstImpact(&r.Original); ok {
		focus = w.Feature
	}
	var focusMax float64
	found := false
	for _, f := range byFeature(fr.Impacts) {
		if f.Feature == focus {
			focusMax, found = f.DuringMaxMs, true
		}
	}
	worst, hasWorst := worstImpact(fr)

	switch {
	case fr.Verdict == model.VerdictDangerous || (hasWorst && worst.StallMs > 0 && fr.Verdict != model.VerdictSafe):
		if hasWorst && worst.StallMs > 0 {
			return fmt.Sprintf("❌ Re-tested: the suggested version still froze **%s** for **%s**. It needs a human look.",
				worst.Feature, FormatDuration(worst.StallMs))
		}
		return "❌ Re-tested: the suggested version is still dangerous. It needs a human look."
	case fr.Verdict == model.VerdictRisky:
		s := "⚠️ Re-tested: better, but still risky"
		if found {
			s += fmt.Sprintf(" — %s max %s", focus, FormatDuration(focusMax))
		}
		return s + "."
	default:
		s := "✅ Re-tested:"
		if found {
			s += fmt.Sprintf(" %s max %s,", focus, FormatDuration(focusMax))
		}
		s += " no stalls"
		if fr.MigrationDurationMs > 0 {
			s += fmt.Sprintf(" (migration took %s)", FormatDuration(fr.MigrationDurationMs))
		}
		return s + "."
	}
}

// FormatDuration renders milliseconds the way a human would say them:
// 0.4ms, 41ms, 3.1s, 1m05s.
func FormatDuration(ms float64) string {
	switch {
	case ms <= 0:
		return "0ms"
	case ms < 10:
		s := fmt.Sprintf("%.1f", ms)
		return strings.TrimSuffix(s, ".0") + "ms"
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	case ms < 60000:
		s := fmt.Sprintf("%.1f", ms/1000)
		if s == "60.0" {
			return "1m00s"
		}
		return strings.TrimSuffix(s, ".0") + "s"
	default:
		total := int(math.Round(ms / 1000))
		return fmt.Sprintf("%dm%02ds", total/60, total%60)
	}
}

func migrationName(m string) string {
	m = strings.TrimSpace(m)
	if m == "" || strings.ContainsAny(m, "\n;") || len(m) > 120 {
		return ""
	}
	return m
}

func shortRel(r string) string { return strings.TrimPrefix(r, "public.") }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cell(s string) string { return strings.ReplaceAll(oneLine(s), "|", `\|`) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// fence wraps body in a code fence long enough not to collide with backticks inside it.
func fence(lang, body string) string {
	ticks := "```"
	for strings.Contains(body, ticks) {
		ticks += "`"
	}
	return ticks + lang + "\n" + body + "\n" + ticks + "\n"
}
