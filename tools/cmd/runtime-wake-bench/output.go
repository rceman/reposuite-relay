// Machine-readable and human output. Only sanitized data is written:
// timings, resource numbers, versions, fingerprints — never PIDs,
// absolute work paths, or raw native identities.
package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
)

func writeCSV(path string, res Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"scenario", "iteration", "processStartMs", "initializeMs",
		"attachMs", "totalMs", "pssRootMiB", "pssTreeMiB", "rssRootMiB", "rssTreeMiB",
		"shutdownMs", "err"}); err != nil {
		return err
	}
	for name, samples := range res.Scenarios {
		for _, s := range samples {
			if err := w.Write([]string{name, fmt.Sprint(s.Iteration),
				fmt.Sprintf("%.2f", s.ProcessStart), fmt.Sprintf("%.2f", s.Initialize),
				fmt.Sprintf("%.2f", s.Attach), fmt.Sprintf("%.2f", s.Total),
				fmt.Sprintf("%.2f", s.PSSRootMiB), fmt.Sprintf("%.2f", s.PSSTreeMiB),
				fmt.Sprintf("%.2f", s.RSSRootMiB), fmt.Sprintf("%.2f", s.RSSTreeMiB),
				fmt.Sprintf("%.2f", s.Shutdown), s.Err}); err != nil {
				return err
			}
		}
	}
	return w.Error()
}

func writeJSON(path string, res Result) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func fieldsOf(samples []Sample, f func(Sample) float64) []float64 {
	var ok []float64
	for _, s := range samples {
		if s.Err == "" {
			ok = append(ok, f(s))
		}
	}
	return ok
}

func printSummary(res Result) {
	fmt.Println()
	fmt.Println("=== RUNTIME WAKE BENCHMARK ===")
	for k, v := range res.Env {
		fmt.Printf("env %-14s %s\n", k+":", v)
	}
	for name, reason := range res.Skipped {
		fmt.Printf("%-22s SKIPPED: %s\n", name, reason)
	}
	for name, samples := range res.Scenarios {
		fmt.Printf("\n--- %s ---\n", name)
		for _, row := range []struct {
			label string
			vals  []float64
		}{
			{"processStartMs", fieldsOf(samples, func(s Sample) float64 { return s.ProcessStart })},
			{"initializeMs", fieldsOf(samples, func(s Sample) float64 { return s.Initialize })},
			{"attachMs", fieldsOf(samples, func(s Sample) float64 { return s.Attach })},
			{"totalMs", fieldsOf(samples, func(s Sample) float64 { return s.Total })},
			{"pssTreeMiB", fieldsOf(samples, func(s Sample) float64 { return s.PSSTreeMiB })},
			{"rssTreeMiB", fieldsOf(samples, func(s Sample) float64 { return s.RSSTreeMiB })},
			{"shutdownMs", fieldsOf(samples, func(s Sample) float64 { return s.Shutdown })},
		} {
			st := summarize(row.vals)
			if st.N == 0 {
				continue
			}
			fmt.Printf("%-15s n=%-3d min=%8.1f p50=%8.1f p90=%8.1f p95=%8.1f mean=%8.1f max=%8.1f sd=%7.1f\n",
				row.label, st.N, st.Min, st.P50, st.P90, st.P95, st.Mean, st.Max, st.StdDev)
		}
	}
}

// summaryLine formats one metric row for the committed doc.
func summaryLine(label string, vals []float64) string {
	st := summarize(vals)
	if st.N == 0 {
		return ""
	}
	return fmt.Sprintf("| %-15s | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %.1f | %.1f |",
		label, st.N, st.Min, st.P50, st.P90, st.P95, st.Mean, st.Max, st.StdDev)
}
