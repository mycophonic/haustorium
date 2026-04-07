//nolint:wrapcheck
package main

import (
	"fmt"
	"math"
	"os"

	"github.com/farcloser/primordium/format"

	"github.com/farcloser/haustorium"
	"github.com/farcloser/haustorium/internal/output"
)

const docsBaseURL = "https://github.com/farcloser/haustorium/blob/main/docs/issues"

// issueInfo maps checks to their HAU ID and category.
type issueInfo struct {
	hauID    string
	category string
}

//nolint:gochecknoglobals // configuration data, effectively const
var issueInfoMap = map[haustorium.Check]issueInfo{
	// Source authenticity
	haustorium.CheckFakeBitDepth:   {hauID: "HAU-002", category: "1. Source authenticity"},
	haustorium.CheckFakeSampleRate: {hauID: "HAU-003", category: "1. Source authenticity"},
	haustorium.CheckLossyTranscode: {hauID: "HAU-004", category: "1. Source authenticity"},
	haustorium.CheckFakeStereo:     {hauID: "HAU-005", category: "1. Source authenticity"},

	// Stereo field
	haustorium.CheckPhaseIssues:      {hauID: "HAU-006", category: "2. Stereo field"},
	haustorium.CheckInvertedPhase:    {hauID: "HAU-007", category: "2. Stereo field"},
	haustorium.CheckChannelImbalance: {hauID: "HAU-008", category: "2. Stereo field"},

	// Dynamics & levels
	haustorium.CheckClipping:         {hauID: "HAU-001", category: "3. Dynamics & levels"},
	haustorium.CheckInterSamplePeaks: {hauID: "HAU-009", category: "3. Dynamics & levels"},
	haustorium.CheckDynamicRange:     {hauID: "HAU-010", category: "3. Dynamics & levels"},
	haustorium.CheckLoudness:         {hauID: "HAU-011", category: "3. Dynamics & levels"},
	haustorium.CheckDCOffset:         {hauID: "HAU-012", category: "3. Dynamics & levels"},

	// Noise & interference
	haustorium.CheckHum:        {hauID: "HAU-013", category: "4. Noise & interference"},
	haustorium.CheckNoiseFloor: {hauID: "HAU-014", category: "4. Noise & interference"},

	// Digital artifacts
	haustorium.CheckDropouts:       {hauID: "HAU-015", category: "5. Digital artifacts"},
	haustorium.CheckTruncation:     {hauID: "HAU-016", category: "5. Digital artifacts"},
	haustorium.CheckSilencePadding: {hauID: "HAU-017", category: "5. Digital artifacts"},
}

// categoryOrder defines the display order for categories (numbered for sorting).
//
//nolint:gochecknoglobals // configuration data, effectively const
var categoryOrder = []string{
	"1. Source authenticity",
	"2. Stereo field",
	"3. Dynamics & levels",
	"4. Noise & interference",
	"5. Digital artifacts",
}

func outputResult(
	filePath string,
	result *haustorium.Result,
	formatName string,
	debug bool,
) error { //nolint:flag-parameter // debug is a simple toggle from CLI flag
	formatter, err := format.GetFormatter(formatName)
	if err != nil {
		return err
	}

	var meta map[string]any
	if debug {
		meta = output.ResultToMap(result)
	} else {
		meta = buildFriendlyOutput(result)
	}

	data := &format.Data{
		Object: filePath,
		Meta:   meta,
	}

	return formatter.PrintAll([]*format.Data{data}, os.Stdout)
}

// buildFriendlyOutput creates a user-friendly summary of the analysis results.
func buildFriendlyOutput(result *haustorium.Result) map[string]any {
	meta := map[string]any{
		"summary": fmt.Sprintf("%d issues found (worst: %s)", result.IssueCount, result.WorstSeverity),
	}

	// Group issues by category.
	categoryIssues := make(map[string][]any)

	for _, issue := range result.Issues {
		info, ok := issueInfoMap[issue.Check]
		if !ok {
			continue
		}

		marker := "  "
		if issue.Detected {
			marker = "!!"
		}

		docURL := fmt.Sprintf("%s/%s.md", docsBaseURL, info.hauID)
		line := fmt.Sprintf("%s [%s] %s: %s (%.0f%% confidence) - %s",
			marker, issue.Severity, issue.Check, issue.Summary, issue.Confidence*100, docURL)

		categoryIssues[info.category] = append(categoryIssues[info.category], line)
	}

	// Build ordered issues map.
	if len(categoryIssues) > 0 {
		issues := make(map[string]any)

		for _, cat := range categoryOrder {
			if catIssues, ok := categoryIssues[cat]; ok {
				issues[cat] = catIssues
			}
		}

		meta["issues"] = issues
	}

	// Key properties.
	props := buildProperties(result)
	if len(props) > 0 {
		meta["properties"] = props
	}

	return meta
}

func buildProperties(result *haustorium.Result) map[string]any {
	props := make(map[string]any)

	if r := result.Loudness; r != nil {
		props["loudness"] = fmt.Sprintf("%.1f LUFS (range: %.1f LU)", r.IntegratedLUFS, r.LoudnessRange)
		props["dynamic_range"] = fmt.Sprintf("DR%d", r.DRScore)
	}

	if r := result.TruePeak; r != nil {
		props["true_peak"] = fmt.Sprintf("%.1f dBTP", r.TruePeakDB)
	}

	if r := result.Spectral; r != nil {
		props["spectral_centroid"] = fmt.Sprintf("%.0f Hz", r.SpectralCentroid)
		props["noise_floor"] = fmt.Sprintf("%.1f dB", r.NoiseFloorDB)
	}

	if stereoResult := result.Stereo; stereoResult != nil {
		props["stereo_width"] = fmt.Sprintf(
			"%s (correlation: %.2f)",
			stereoWidthLabel(stereoResult.Correlation),
			stereoResult.Correlation,
		)
		if math.Abs(stereoResult.ImbalanceDB) > correlationNormal {
			props["channel_imbalance"] = fmt.Sprintf(
				"%.1f dB (%s louder)",
				math.Abs(stereoResult.ImbalanceDB),
				imbalanceSide(stereoResult.ImbalanceDB),
			)
		}
	}

	if r := result.BitDepth; r != nil {
		if r.Claimed != r.Effective {
			props["bit_depth"] = fmt.Sprintf("%d-bit (effective: %d-bit)", r.Claimed, r.Effective)
		} else {
			props["bit_depth"] = fmt.Sprintf("%d-bit", r.Claimed)
		}
	}

	return props
}

const (
	correlationMono   = 0.95
	correlationNarrow = 0.75
	correlationNormal = 0.5
	correlationWide   = 0.2
)

func stereoWidthLabel(correlation float64) string {
	switch {
	case correlation > correlationMono:
		return "Mono/Narrow"
	case correlation > correlationNarrow:
		return "Narrow"
	case correlation > correlationNormal:
		return "Normal"
	case correlation > correlationWide:
		return "Wide"
	default:
		return "Very Wide"
	}
}

func imbalanceSide(imbalanceDB float64) string {
	if imbalanceDB > 0 {
		return "left"
	}

	return "right"
}
