package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/urfave/cli/v3"
)

const (
	severitySevere   = "severe"
	severityModerate = "moderate"
	severityMild     = "mild"
)

var errDigestArgs = errors.New("expected exactly one argument: path to report.jsonl")

func digestCommand() *cli.Command {
	return &cli.Command{
		Name:      "digest",
		Usage:     "Produce a summary digest from a haustorium JSONL report",
		ArgsUsage: "<report.jsonl>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "issue",
				Usage: "Show files affected by a specific issue type (e.g., clipping, noise-floor)",
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return errDigestArgs
			}

			return runDigest(cmd.Args().First(), cmd.String("issue"))
		},
	}
}

func runDigest(reportPath, issueFilter string) error {
	records, rawLines, err := readRecordsWithRaw(reportPath)
	if err != nil {
		return err
	}

	printDigest(records)

	if issueFilter != "" {
		printIssueDetail(records, rawLines, issueFilter)
	}

	return nil
}

func readRecordsWithRaw(path string) ([]digestRecord, [][]byte, error) {
	file, err := os.Open(path) //nolint:gosec // CLI tool opens user-specified report files
	if err != nil {
		return nil, nil, fmt.Errorf("opening report: %w", err)
	}
	defer file.Close()

	var (
		records []digestRecord
		lines   [][]byte
	)

	scanner := bufio.NewScanner(file)

	const maxLineSize = 1024 * 1024 // 1MB
	scanner.Buffer(make([]byte, 0, maxLineSize), maxLineSize)

	for scanner.Scan() {
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())
		lines = append(lines, line)

		var rec digestRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			records = append(records, digestRecord{Error: "parse error"})

			continue
		}

		records = append(records, rec)
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading report: %w", err)
	}

	return records, lines, nil
}

func printDigest(records []digestRecord) {
	total := len(records)
	errorCount := 0
	sevDist := map[string]int{severitySevere: 0, severityModerate: 0, severityMild: 0, "clean": 0}
	issueDist := map[int]int{}
	checkStats := map[string]*checkBreakdown{}

	for _, rec := range records {
		if rec.Error != "" || rec.Analysis == nil {
			errorCount++

			continue
		}

		// Worst severity.
		worst := rec.Analysis.Summary.WorstSeverity
		if worst == "" || worst == "no issue" {
			sevDist["clean"]++
		} else {
			sevDist[worst]++
		}

		// Issue count.
		issueDist[rec.Analysis.Summary.IssueCount]++

		// Per-check breakdown.
		for _, issue := range rec.Analysis.Issues {
			if !issue.Detected {
				continue
			}

			breakdown, ok := checkStats[issue.Check]
			if !ok {
				breakdown = &checkBreakdown{Check: issue.Check}
				checkStats[issue.Check] = breakdown
			}

			breakdown.Total++

			switch issue.Severity {
			case severitySevere:
				breakdown.Severe++
			case severityModerate:
				breakdown.Moderate++
			case severityMild:
				breakdown.Mild++
			default:
			}
		}
	}

	analyzed := total - errorCount

	fmt.Fprintln(os.Stdout, "=== Haustorium Report Digest ===")
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Total tracks:  %d\n", total)
	fmt.Fprintf(os.Stdout, "Failed:        %d\n", errorCount)
	fmt.Fprintf(os.Stdout, "Analyzed:      %d\n", analyzed)
	fmt.Fprintln(os.Stdout)

	fmt.Fprintln(os.Stdout, "--- Worst Severity ---")
	fmt.Fprintf(os.Stdout, "  Clean:     %d\n", sevDist["clean"])
	fmt.Fprintf(os.Stdout, "  Mild:      %d\n", sevDist[severityMild])
	fmt.Fprintf(os.Stdout, "  Moderate:  %d\n", sevDist[severityModerate])
	fmt.Fprintf(os.Stdout, "  Severe:    %d\n", sevDist[severitySevere])
	fmt.Fprintln(os.Stdout)

	fmt.Fprintln(os.Stdout, "--- Issues Per Track ---")

	maxIssues := 0
	for k := range issueDist {
		if k > maxIssues {
			maxIssues = k
		}
	}

	for i := range maxIssues + 1 {
		if count, ok := issueDist[i]; ok && count > 0 {
			fmt.Fprintf(os.Stdout, "  %d issues:  %d tracks\n", i, count)
		}
	}

	fmt.Fprintln(os.Stdout)

	fmt.Fprintln(os.Stdout, "--- Issues By Type ---")

	breakdowns := make([]*checkBreakdown, 0, len(checkStats))
	for _, bd := range checkStats {
		breakdowns = append(breakdowns, bd)
	}

	slices.SortFunc(breakdowns, func(a, b *checkBreakdown) int {
		return b.Total - a.Total
	})

	for _, bd := range breakdowns {
		fmt.Fprintf(os.Stdout, "  %s\n", bd.Check)
		fmt.Fprintf(
			os.Stdout,
			"    total: %d  severe: %d  moderate: %d  mild: %d\n",
			bd.Total,
			bd.Severe,
			bd.Moderate,
			bd.Mild,
		)
	}
}

//nolint:gochecknoglobals
var checkKeyMap = map[string]string{
	"clipping":           "clipping",
	"truncation":         "truncation",
	"fake-bit-depth":     "bit_depth",
	"fake-sample-rate":   "spectral",
	"lossy-transcode":    "spectral",
	"dc-offset":          "dc_offset",
	"fake-stereo":        "stereo",
	"phase-issues":       "stereo",
	"inverted-phase":     "stereo",
	"channel-imbalance":  "stereo",
	"silence-padding":    "silence",
	"hum":                "spectral",
	"noise-floor":        "spectral",
	"inter-sample-peaks": "true_peak",
	"loudness":           "loudness",
	"dynamic-range":      "loudness",
	"dropouts":           "dropouts",
}

type issueEntry struct {
	file       string
	severity   string
	summary    string
	confidence float64
	detail     map[string]any
}

func printIssueDetail(records []digestRecord, rawLines [][]byte, check string) {
	fmt.Fprintln(os.Stdout)

	var entries []issueEntry

	detailKey := checkKeyMap[check]

	for idx, rec := range records {
		if rec.Error != "" || rec.Analysis == nil {
			continue
		}

		for _, issue := range rec.Analysis.Issues {
			if !issue.Detected || issue.Check != check {
				continue
			}

			entry := issueEntry{
				file:       rec.File,
				severity:   issue.Severity,
				summary:    issue.Summary,
				confidence: issue.Confidence,
			}

			if entry.file == "" {
				entry.file = "(redacted)"
			}

			// Extract detail from raw JSONL line.
			if detailKey != "" && idx < len(rawLines) {
				entry.detail = extractDetailFromRaw(rawLines[idx], detailKey)
			}

			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		fmt.Fprintf(os.Stdout, "No tracks affected by %s\n", check)

		return
	}

	slices.SortFunc(entries, func(a, b issueEntry) int {
		return severityRank(a.severity) - severityRank(b.severity)
	})

	fmt.Fprintf(os.Stdout, "=== %s: %d tracks ===\n\n", check, len(entries))

	for _, entry := range entries {
		fmt.Fprintf(os.Stdout, "  %s\n", entry.file)
		fmt.Fprintf(os.Stdout, "    severity: %s  confidence: %.0f%%\n", entry.severity, entry.confidence*100)
		fmt.Fprintf(os.Stdout, "    %s\n", entry.summary)

		if entry.detail != nil {
			for key, val := range entry.detail {
				fmt.Fprintf(os.Stdout, "    %s: %s\n", key, formatDetailValue(val))
			}
		}

		fmt.Fprintln(os.Stdout)
	}
}

func extractDetailFromRaw(rawLine []byte, key string) map[string]any {
	var full struct {
		Analysis map[string]any `json:"analysis"`
	}

	if err := json.Unmarshal(rawLine, &full); err != nil {
		return nil
	}

	if full.Analysis == nil {
		return nil
	}

	if detail, ok := full.Analysis[key].(map[string]any); ok {
		return detail
	}

	return nil
}

func severityRank(severity string) int {
	switch severity {
	case severitySevere:
		return 0
	case severityModerate:
		return 1
	case severityMild:
		return 2
	default:
		return 3
	}
}

func formatDetailValue(value any) string {
	switch val := value.(type) {
	case []any:
		return fmt.Sprintf("%d entries", len(val))
	case string:
		return val
	default:
		return fmt.Sprintf("%v", value)
	}
}
