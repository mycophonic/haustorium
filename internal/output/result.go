// Package output provides shared result serialization for haustorium JSON output.
package output

import (
	"fmt"

	"github.com/farcloser/haustorium"
	"github.com/farcloser/haustorium/internal/types"
)

// ResultToMap converts an analysis result into the canonical map structure
// used for JSON and JSONL serialization.
func ResultToMap(result *haustorium.Result) map[string]any {
	meta := map[string]any{
		"summary": map[string]any{
			"issue_count":    result.IssueCount,
			"worst_severity": result.WorstSeverity.String(),
		},
	}

	// Issues.
	issues := make([]any, 0, len(result.Issues))
	for _, issue := range result.Issues {
		issues = append(issues, map[string]any{
			"check":      issue.Check.String(),
			"detected":   issue.Detected,
			"severity":   issue.Severity.String(),
			"summary":    issue.Summary,
			"confidence": issue.Confidence,
		})
	}

	meta["issues"] = issues

	// Raw analyzer results.
	if r := result.Clipping; r != nil {
		meta["clipping"] = ClippingToMap(r)
	}

	if r := result.Truncation; r != nil {
		meta["truncation"] = map[string]any{
			"final_rms_db":    r.FinalRmsDB,
			"final_peak_db":   r.FinalPeakDB,
			"samples_in_tail": r.SamplesInTail,
		}
	}

	if r := result.BitDepth; r != nil {
		meta["bit_depth"] = map[string]any{
			"claimed":   int(r.Claimed),   //nolint:gosec // audio format values are small constants
			"effective": int(r.Effective), //nolint:gosec // audio format values are small constants
			"is_padded": r.IsPadded,
			"samples":   r.Samples,
		}
	}

	if r := result.Spectral; r != nil {
		meta["spectral"] = SpectralToMap(r)
	}

	if r := result.DCOffset; r != nil {
		meta["dc_offset"] = map[string]any{
			"offset":    r.Offset,
			"offset_db": r.OffsetDB,
			"channels":  r.Channels,
			"samples":   r.Samples,
		}
	}

	if reader := result.Stereo; reader != nil {
		meta["stereo"] = map[string]any{
			"correlation":     reader.Correlation,
			"difference_db":   reader.DifferenceDB,
			"mono_sum_db":     reader.MonoSumDB,
			"stereo_rms_db":   reader.StereoRmsDB,
			"cancellation_db": reader.CancellationDB,
			"left_rms_db":     reader.LeftRmsDB,
			"right_rms_db":    reader.RightRmsDB,
			"imbalance_db":    reader.ImbalanceDB,
			"frames":          reader.Frames,
		}
	}

	if r := result.Silence; r != nil {
		meta["silence"] = SilenceToMap(r)
	}

	if reader := result.TruePeak; reader != nil {
		meta["true_peak"] = map[string]any{
			"true_peak_db":       reader.TruePeakDB,
			"sample_peak_db":     reader.SamplePeakDB,
			"isp_count":          reader.ISPCount,
			"isp_max_db":         reader.ISPMaxDB,
			"isp_density_peak":   reader.ISPDensityPeak,
			"isp_density_avg":    reader.ISPDensityAvg,
			"isps_above_half_db": reader.ISPsAboveHalfdB,
			"isps_above_1db":     reader.ISPsAbove1dB,
			"isps_above_2db":     reader.ISPsAbove2dB,
			"worst_density_sec":  reader.WorstDensitySec,
			"frames":             reader.Frames,
		}
	}

	if reader := result.Loudness; reader != nil {
		meta["loudness"] = map[string]any{
			"integrated_lufs": reader.IntegratedLUFS,
			"short_term_max":  reader.ShortTermMax,
			"momentary_max":   reader.MomentaryMax,
			"loudness_range":  reader.LoudnessRange,
			"dr_score":        reader.DRScore,
			"dr_value":        reader.DRValue,
			"peak_db":         reader.PeakDB,
			"rms_db":          reader.RmsDB,
			"frames":          reader.Frames,
		}
	}

	if r := result.Dropout; r != nil {
		meta["dropouts"] = DropoutToMap(r)
	}

	return meta
}

// ClippingToMap converts clipping detection results to a map.
func ClippingToMap(result *types.ClippingDetection) map[string]any {
	channels := make([]any, 0, len(result.Channels))
	for i, ch := range result.Channels {
		channels = append(channels, map[string]any{
			"channel":         i,
			"events":          ch.Events,
			"clipped_samples": ch.ClippedSamples,
			"longest_run":     ch.LongestRun,
		})
	}

	return map[string]any{
		"events":          result.Events,
		"clipped_samples": result.ClippedSamples,
		"longest_run":     result.LongestRun,
		"samples":         result.Samples,
		"channels":        channels,
	}
}

// SpectralToMap converts spectral analysis results to a map.
func SpectralToMap(result *types.SpectralResult) map[string]any {
	meta := map[string]any{
		"claimed_rate":      result.ClaimedRate,
		"is_upsampled":      result.IsUpsampled,
		"is_transcode":      result.IsTranscode,
		"has_50hz_hum":      result.Has50HzHum,
		"has_60hz_hum":      result.Has60HzHum,
		"hum_level_db":      result.HumLevelDB,
		"noise_floor_db":    result.NoiseFloorDB,
		"spectral_centroid": result.SpectralCentroid,
		"frames":            result.Frames,
	}

	if result.IsUpsampled {
		meta["effective_rate"] = result.EffectiveRate
		meta["upsample_cutoff"] = result.UpsampleCutoff
		meta["upsample_sharpness"] = result.UpsampleSharpness
	}

	if result.IsTranscode || result.TranscodeConfidence > 0 {
		meta["transcode_cutoff"] = result.TranscodeCutoff
		meta["transcode_sharpness"] = result.TranscodeSharpness
		meta["likely_codec"] = result.LikelyCodec
		meta["transcode_confidence"] = result.TranscodeConfidence
		meta["cutoff_consistency_hz"] = result.CutoffConsistency
		meta["has_ultrasonic_content"] = result.HasUltrasonicContent
	}

	if len(result.BandEnergy) > 0 {
		bands := make([]any, 0, len(result.BandEnergy))
		for i, e := range result.BandEnergy {
			entry := map[string]any{
				"energy_db": e,
			}
			if i < len(result.BandFreqs) {
				entry["freq_hz"] = result.BandFreqs[i]
			}

			bands = append(bands, entry)
		}

		meta["band_energy"] = bands
	}

	return meta
}

// SilenceToMap converts silence detection results to a map.
func SilenceToMap(result *types.SilenceResult) map[string]any {
	segments := make([]any, 0, len(result.Segments))
	for _, seg := range result.Segments {
		segments = append(segments, map[string]any{
			"start_sec":    seg.StartSec,
			"end_sec":      seg.EndSec,
			"duration_sec": seg.DurationSec,
			"rms_db":       seg.RmsDB,
		})
	}

	return map[string]any{
		"total_duration": result.TotalDuration,
		"leading_sec":    result.LeadingSec,
		"trailing_sec":   result.TrailingSec,
		"total_silence":  result.TotalSilence,
		"frames":         result.Frames,
		"segments":       segments,
	}
}

// DropoutToMap converts dropout detection results to a map.
func DropoutToMap(result *types.DropoutResult) map[string]any {
	events := make([]any, 0, len(result.Events))
	for _, entry := range result.Events {
		event := map[string]any{
			"time_sec": entry.TimeSec,
			"channel":  entry.Channel,
			"type":     entry.Type.String(),
			"severity": fmt.Sprintf("%.4f", entry.Severity),
		}
		if entry.Type == types.EventZeroRun {
			event["duration_ms"] = entry.DurationMs
		}

		events = append(events, event)
	}

	return map[string]any{
		"delta_count":    result.DeltaCount,
		"zero_run_count": result.ZeroRunCount,
		"dc_jump_count":  result.DCJumpCount,
		"worst_db":       result.WorstDB,
		"frames":         result.Frames,
		"events":         events,
	}
}
