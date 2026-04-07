//nolint:wrapcheck // too dumb
package haustorium

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/farcloser/haustorium/internal/audit/bitdepth"
	"github.com/farcloser/haustorium/internal/audit/clipping"
	"github.com/farcloser/haustorium/internal/audit/dcoffset"
	"github.com/farcloser/haustorium/internal/audit/dropout"
	"github.com/farcloser/haustorium/internal/audit/loudness"
	"github.com/farcloser/haustorium/internal/audit/silence"
	"github.com/farcloser/haustorium/internal/audit/spectral"
	"github.com/farcloser/haustorium/internal/audit/stereo"
	"github.com/farcloser/haustorium/internal/audit/truepeak"
	"github.com/farcloser/haustorium/internal/audit/truncation"
	"github.com/farcloser/haustorium/internal/types"
)

// ErrUnknownSource is returned when a source string cannot be parsed.
var ErrUnknownSource = errors.New("unknown source")

// String literal constants to avoid repetition.
const (
	unknownStr         = "unknown"
	discontinuitiesFmt = "%d discontinuities (%d jumps, %d zero runs, %d DC shifts; worst: %.1f dB)"
)

// Band threshold constants for severity configuration.
const (
	bandThreshold5    = 5
	bandThreshold6    = 6
	bandThreshold8    = 8
	bandThreshold10   = 10
	bandThreshold12   = 12
	bandThreshold13   = 13
	bandThreshold15   = 15
	bandThreshold20   = 20
	bandThreshold25   = 25
	bandThreshold26   = 26
	bandThreshold30   = 30
	bandThreshold35   = 35
	bandThreshold40   = 40
	bandThreshold50   = 50
	bandThreshold100  = 100
	bandThreshold1000 = 1000
)

// Negative band thresholds (dB values).
const (
	bandThresholdNeg10 = -10
	bandThresholdNeg13 = -13
	bandThresholdNeg20 = -20
	bandThresholdNeg25 = -25
	bandThresholdNeg26 = -26
	bandThresholdNeg30 = -30
	bandThresholdNeg35 = -35
	bandThresholdNeg40 = -40
	bandThresholdNeg60 = -60
)

// Detection thresholds used in interpretResults.
const (
	fakeStereoCorrThreshold    = 0.98
	fakeStereoDBThreshold      = bandThresholdNeg60
	invertedPhaseThreshold     = -0.95
	baseSampleRateMax          = 48000
	excellentDynamicsThreshold = bandThreshold12
)

// Confidence values for issue reporting.
const (
	confidenceFull      = 1.0
	confidenceHigh      = 0.95
	confidenceGood      = 0.9
	confidenceModerate  = 0.85
	confidenceFair      = 0.8
	confidenceLow       = 0.5
	dropoutDeltaDefault = 0.5
	dropoutDeltaVinyl   = 0.7
)

/*
Usage:

result, err := haustorium.Analyze(factory, format, haustorium.DefaultOptions())
if result.HasClipping {
    fmt.Println("Clipping detected!")
}

// Defects only
opts := haustorium.DefaultOptions()
opts.Checks = haustorium.ChecksDefects
result, err := haustorium.Analyze(factory, format, opts)

// Custom bands
opts := haustorium.DefaultOptions()
opts.Truncation = haustorium.Bands{Mild: -35, Moderate: -25, Severe: -15}
opts.ChannelImbalance = haustorium.Bands{Mild: 1.5, Moderate: 3.0, Severe: 5.0}
result, err := haustorium.Analyze(factory, format, opts)

// Source-aware (adjusts bands for vinyl/live characteristics)
opts := haustorium.OptionsForSource(haustorium.SourceVinyl)
result, err := haustorium.Analyze(factory, format, opts)

// Iterate issues
for _, issue := range result.Issues {
    if issue.Detected {
        fmt.Printf("[%s] %s\n", issue.Severity, issue.Summary)
    }
}

// Inspect raw data
if result.Stereo != nil {
    fmt.Printf("Correlation: %.3f\n", result.Stereo.Correlation)
}

*/

// Check represents a high-level audio quality check.
type Check int

// CheckClipping and related constants enumerate individual audio quality checks.
const (
	CheckClipping Check = 1 << iota
	CheckTruncation
	CheckFakeBitDepth
	CheckFakeSampleRate
	CheckLossyTranscode
	CheckDCOffset
	CheckFakeStereo
	CheckPhaseIssues
	CheckInvertedPhase
	CheckChannelImbalance
	CheckSilencePadding
	CheckHum
	CheckNoiseFloor
	CheckInterSamplePeaks
	CheckLoudness
	CheckDynamicRange
	CheckDropouts

	// ChecksDefects is a preset combining all defect-detection checks.
	ChecksDefects = CheckClipping | CheckTruncation | CheckFakeBitDepth |
		CheckFakeSampleRate | CheckLossyTranscode | CheckDCOffset |
		CheckFakeStereo | CheckPhaseIssues | CheckInvertedPhase |
		CheckChannelImbalance | CheckSilencePadding | CheckHum |
		CheckNoiseFloor | CheckInterSamplePeaks | CheckDropouts

	// ChecksLoudness is a preset combining loudness-related checks.
	ChecksLoudness = CheckLoudness | CheckDynamicRange | CheckInterSamplePeaks

	// ChecksAll is a preset combining all available checks.
	ChecksAll = ChecksDefects | ChecksLoudness
)

func (c Check) String() string {
	switch c {
	case CheckClipping:
		return "clipping"
	case CheckTruncation:
		return "truncation"
	case CheckFakeBitDepth:
		return "fake-bit-depth"
	case CheckFakeSampleRate:
		return "fake-sample-rate"
	case CheckLossyTranscode:
		return "lossy-transcode"
	case CheckDCOffset:
		return "dc-offset"
	case CheckFakeStereo:
		return "fake-stereo"
	case CheckPhaseIssues:
		return "phase-issues"
	case CheckInvertedPhase:
		return "inverted-phase"
	case CheckChannelImbalance:
		return "channel-imbalance"
	case CheckSilencePadding:
		return "silence-padding"
	case CheckHum:
		return "hum"
	case CheckNoiseFloor:
		return "noise-floor"
	case CheckInterSamplePeaks:
		return "inter-sample-peaks"
	case CheckLoudness:
		return "loudness"
	case CheckDynamicRange:
		return "dynamic-range"
	case CheckDropouts:
		return "dropouts"
	case ChecksDefects:
		return "defects-preset"
	case ChecksLoudness:
		return "loudness-preset"
	case ChecksAll:
		return "all-checks-preset"
	}

	return unknownStr
}

// Severity indicates how bad a detected issue is.
type Severity int

// SeverityNone and related constants define the severity levels for detected issues.
const (
	SeverityNone Severity = iota
	SeverityMild
	SeverityModerate
	SeveritySevere
)

func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "no issue"
	case SeverityMild:
		return "mild"
	case SeverityModerate:
		return "moderate"
	case SeveritySevere:
		return "severe"
	}

	return unknownStr
}

// Issue represents a detected problem.
type Issue struct {
	Check      Check
	Detected   bool
	Severity   Severity
	Summary    string  // human-readable summary
	Confidence float64 // 0.0-1.0
}

// Bands defines severity thresholds for a check. Direction is implicit:
// if Mild < Severe, higher values are worse (ascending, e.g. dB offset).
// If Mild > Severe, lower values are worse (descending, e.g. DR score).
type Bands struct {
	Mild     float64
	Moderate float64
	Severe   float64
}

// Match returns the severity for a value.
// Returns (SeverityNone, false) when the value is below detection (the Mild threshold).
func (b Bands) Match(value float64) (Severity, bool) {
	if b.Mild <= b.Severe {
		// Ascending: higher = worse.
		if value >= b.Severe {
			return SeveritySevere, true
		}

		if value >= b.Moderate {
			return SeverityModerate, true
		}

		if value >= b.Mild {
			return SeverityMild, true
		}
	} else {
		// Descending: lower = worse (e.g. DR score).
		if value <= b.Severe {
			return SeveritySevere, true
		}

		if value <= b.Moderate {
			return SeverityModerate, true
		}

		if value <= b.Mild {
			return SeverityMild, true
		}
	}

	return SeverityNone, false
}

// Options configures the analysis.
type Options struct {
	Checks Check // which checks to run (default: ChecksAll)

	// Severity bands per check (zero value = use defaults).
	Clipping         Bands
	Truncation       Bands
	DCOffset         Bands
	ChannelImbalance Bands
	PhaseIssues      Bands
	SilencePadding   Bands
	Hum              Bands
	NoiseFloor       Bands
	ISP              Bands
	DynamicRange     Bands
	Dropouts         Bands

	// Analyzer thresholds (not severity bands).
	TranscodeSharpnessDB  float64 // default 30
	UpsampleSharpnessDB   float64 // default 40
	DropoutDeltaThreshold float64 // default 0.5
}

// DefaultOptions returns DefaultDigitalOptions.
func DefaultOptions() Options {
	return DefaultDigitalOptions()
}

// DefaultDigitalOptions returns options for clean digital recordings.
func DefaultDigitalOptions() Options {
	return Options{
		Checks:           ChecksAll,
		Clipping:         Bands{Mild: 1, Moderate: bandThreshold10, Severe: bandThreshold100},
		Truncation:       Bands{Mild: bandThresholdNeg40, Moderate: bandThresholdNeg30, Severe: bandThresholdNeg20},
		DCOffset:         Bands{Mild: bandThresholdNeg40, Moderate: bandThresholdNeg26, Severe: bandThresholdNeg13},
		ChannelImbalance: Bands{Mild: 1, Moderate: 2, Severe: 3},
		PhaseIssues:      Bands{Mild: 3, Moderate: bandThreshold6, Severe: bandThreshold10},
		SilencePadding:   Bands{Mild: 2, Moderate: bandThreshold5, Severe: bandThreshold10},
		Hum:              Bands{Mild: bandThreshold10, Moderate: bandThreshold20, Severe: bandThreshold30},
		NoiseFloor:       Bands{Mild: bandThresholdNeg30, Moderate: bandThresholdNeg20, Severe: bandThresholdNeg10},
		ISP:              Bands{Mild: 1, Moderate: bandThreshold100, Severe: bandThreshold1000},
		DynamicRange:     Bands{Mild: bandThreshold8, Moderate: bandThreshold6, Severe: 4},
		Dropouts:         Bands{Mild: 1, Moderate: bandThreshold5, Severe: bandThreshold20},

		TranscodeSharpnessDB:  bandThreshold30,
		UpsampleSharpnessDB:   bandThreshold40,
		DropoutDeltaThreshold: dropoutDeltaDefault,
	}
}

// DefaultVinylOptions returns options for vinyl rips.
// Higher tolerance for noise, hum, DC offset, silence padding, dropouts,
// and channel imbalance (early stereo mixes used hard panning).
func DefaultVinylOptions() Options {
	opts := DefaultDigitalOptions()
	opts.Truncation = Bands{Mild: bandThresholdNeg30, Moderate: bandThresholdNeg20, Severe: bandThresholdNeg10}
	opts.DCOffset = Bands{Mild: bandThresholdNeg26, Moderate: bandThresholdNeg13, Severe: 0}
	opts.ChannelImbalance = Bands{Mild: 3, Moderate: bandThreshold6, Severe: bandThreshold10}
	opts.SilencePadding = Bands{Mild: bandThreshold5, Moderate: bandThreshold10, Severe: bandThreshold20}
	opts.Hum = Bands{Mild: bandThreshold20, Moderate: bandThreshold30, Severe: bandThreshold40}
	opts.NoiseFloor = Bands{Mild: bandThresholdNeg20, Moderate: bandThresholdNeg10, Severe: 0}
	opts.Dropouts = Bands{Mild: bandThreshold5, Moderate: bandThreshold15, Severe: bandThreshold40}
	opts.DropoutDeltaThreshold = dropoutDeltaVinyl

	return opts
}

// DefaultLiveOptions returns options for live recordings.
// Higher tolerance for ambient noise, PA hum, silence padding, and DC offset.
func DefaultLiveOptions() Options {
	opts := DefaultDigitalOptions()
	opts.Truncation = Bands{Mild: bandThresholdNeg30, Moderate: bandThresholdNeg20, Severe: bandThresholdNeg10}
	opts.DCOffset = Bands{Mild: bandThresholdNeg30, Moderate: bandThresholdNeg20, Severe: bandThresholdNeg10}
	opts.SilencePadding = Bands{Mild: bandThreshold5, Moderate: bandThreshold10, Severe: bandThreshold20}
	opts.Hum = Bands{Mild: bandThreshold15, Moderate: bandThreshold25, Severe: bandThreshold35}
	opts.NoiseFloor = Bands{Mild: bandThresholdNeg20, Moderate: bandThresholdNeg10, Severe: 0}

	return opts
}

// Source represents the audio source type, which adjusts detection thresholds
// to account for characteristics inherent to the medium.
type Source int

// SourceDigital and related constants define the audio source types.
const (
	SourceDigital Source = iota // Clean digital recording (default).
	SourceVinyl                 // Vinyl rip. Higher noise, hum, DC offset tolerance.
	SourceLive                  // Live recording. Ambient noise, PA hum tolerance.
)

func (s Source) String() string {
	switch s {
	case SourceDigital:
		return "digital"
	case SourceVinyl:
		return "vinyl"
	case SourceLive:
		return "live"
	}

	return unknownStr
}

// ParseSource converts a string to a Source value.
func ParseSource(severity string) (Source, error) {
	switch severity {
	case "digital", "":
		return SourceDigital, nil
	case "vinyl":
		return SourceVinyl, nil
	case "live":
		return SourceLive, nil
	default:
		return 0, fmt.Errorf("%w: %q (valid: digital, vinyl, live)", ErrUnknownSource, severity)
	}
}

// OptionsForSource returns the default Options for the given source type.
func OptionsForSource(source Source) Options {
	switch source {
	case SourceVinyl:
		return DefaultVinylOptions()
	case SourceLive:
		return DefaultLiveOptions()
	case SourceDigital:
		return DefaultDigitalOptions()
	}

	return DefaultDigitalOptions()
}

// Result contains all analysis results.
type Result struct {
	// High-level issues (what the user asked for)
	Issues []Issue

	// Quick access booleans
	HasClipping         bool
	HasTruncation       bool
	HasFakeBitDepth     bool
	HasFakeSampleRate   bool
	HasLossyTranscode   bool
	HasDCOffset         bool
	HasFakeStereo       bool
	HasPhaseIssues      bool
	HasInvertedPhase    bool
	HasChannelImbalance bool
	HasSilencePadding   bool
	HasHum              bool
	HasHighNoiseFloor   bool
	HasInterSamplePeaks bool
	HasDropouts         bool
	IsBrickwalled       bool

	// Summary
	IssueCount    int
	WorstSeverity Severity

	// Raw analysis results (for inspection, nil if not requested)
	Clipping   *types.ClippingDetection
	Truncation *types.TruncationDetection
	BitDepth   *types.BitDepthAuthenticity
	Spectral   *types.SpectralResult
	DCOffset   *types.DCOffsetResult
	Stereo     *types.StereoResult
	Silence    *types.SilenceResult
	TruePeak   *types.TruePeakResult
	Loudness   *types.LoudnessResult
	Dropout    *types.DropoutResult
}

// ReaderFactory provides fresh readers for multiple passes.
type ReaderFactory func() (io.Reader, error)

// Analyze performs comprehensive audio analysis.
func Analyze(factory ReaderFactory, format types.PCMFormat, opts Options) (*Result, error) {
	if opts.Checks == 0 {
		opts = DefaultOptions()
	}

	applyDefaults(&opts)

	result := &Result{}

	if err := runAnalyzers(result, factory, format, opts); err != nil {
		return nil, err
	}

	// Interpret results
	interpretResults(result, opts)

	return result, nil
}

// analyzerNeeds determines which low-level analyzers are required based on the requested checks.
type analyzerNeeds struct {
	clipping   bool
	truncation bool
	bitDepth   bool
	spectral   bool
	dcOffset   bool
	stereoFld  bool
	silenceFld bool
	truePeak   bool
	loudnessFl bool
	dropout    bool
}

func resolveNeeds(checks Check) analyzerNeeds {
	return analyzerNeeds{
		clipping:   checks&CheckClipping != 0,
		truncation: checks&CheckTruncation != 0,
		bitDepth:   checks&CheckFakeBitDepth != 0,
		spectral:   checks&(CheckFakeSampleRate|CheckLossyTranscode|CheckHum|CheckNoiseFloor) != 0,
		dcOffset:   checks&CheckDCOffset != 0,
		stereoFld:  checks&(CheckFakeStereo|CheckPhaseIssues|CheckInvertedPhase|CheckChannelImbalance) != 0,
		silenceFld: checks&CheckSilencePadding != 0,
		truePeak:   checks&CheckInterSamplePeaks != 0,
		loudnessFl: checks&(CheckLoudness|CheckDynamicRange) != 0,
		dropout:    checks&CheckDropouts != 0,
	}
}

func runAnalyzers(
	result *Result,
	factory ReaderFactory,
	format types.PCMFormat,
	opts Options,
) error {
	needs := resolveNeeds(opts.Checks)

	if err := runEarlyAnalyzers(result, factory, format, needs); err != nil {
		return err
	}

	if err := runMidAnalyzers(result, factory, format, needs); err != nil {
		return err
	}

	return runLateAnalyzers(result, factory, format, opts, needs)
}

func runEarlyAnalyzers(
	result *Result,
	factory ReaderFactory,
	format types.PCMFormat,
	needs analyzerNeeds,
) error {
	if needs.clipping {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Clipping, err = clipping.Detect(r, format)
		if err != nil {
			return err
		}
	}

	if needs.truncation {
		r, err := factory()
		if err != nil {
			return err
		}

		if rs, ok := r.(io.ReadSeeker); ok {
			result.Truncation, err = truncation.Detect(rs, format, bandThreshold50)
			if err != nil {
				return err
			}
		}
	}

	if needs.bitDepth {
		r, err := factory()
		if err != nil {
			return err
		}

		result.BitDepth, err = bitdepth.Authenticity(r, format)
		if err != nil {
			return err
		}
	}

	return nil
}

func runMidAnalyzers(
	result *Result,
	factory ReaderFactory,
	format types.PCMFormat,
	needs analyzerNeeds,
) error {
	if needs.spectral {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Spectral, err = spectral.AnalyzeV2(r, format, spectral.DefaultOptions())
		if err != nil {
			return err
		}
	}

	if needs.dcOffset {
		r, err := factory()
		if err != nil {
			return err
		}

		result.DCOffset, err = dcoffset.Detect(r, format)
		if err != nil {
			return err
		}
	}

	if needs.stereoFld && format.Channels == 2 {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Stereo, err = stereo.Analyze(r, format)
		if err != nil {
			return err
		}
	}

	if needs.silenceFld {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Silence, err = silence.Detect(r, format, silence.DefaultOptions())
		if err != nil {
			return err
		}
	}

	return nil
}

func runLateAnalyzers(
	result *Result,
	factory ReaderFactory,
	format types.PCMFormat,
	opts Options,
	needs analyzerNeeds,
) error {
	if needs.truePeak {
		r, err := factory()
		if err != nil {
			return err
		}

		result.TruePeak, err = truepeak.Detect(r, format)
		if err != nil {
			return err
		}
	}

	if needs.loudnessFl {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Loudness, err = loudness.Analyze(r, format)
		if err != nil {
			return err
		}
	}

	if needs.dropout {
		r, err := factory()
		if err != nil {
			return err
		}

		result.Dropout, err = dropout.DetectV2(r, format, dropout.Options{
			DeltaThreshold: opts.DropoutDeltaThreshold,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func applyDefaults(opts *Options) {
	defaults := DefaultOptions()
	zeroBands := Bands{}

	if opts.Clipping == zeroBands {
		opts.Clipping = defaults.Clipping
	}

	if opts.Truncation == zeroBands {
		opts.Truncation = defaults.Truncation
	}

	if opts.DCOffset == zeroBands {
		opts.DCOffset = defaults.DCOffset
	}

	if opts.ChannelImbalance == zeroBands {
		opts.ChannelImbalance = defaults.ChannelImbalance
	}

	if opts.PhaseIssues == zeroBands {
		opts.PhaseIssues = defaults.PhaseIssues
	}

	if opts.SilencePadding == zeroBands {
		opts.SilencePadding = defaults.SilencePadding
	}

	if opts.Hum == zeroBands {
		opts.Hum = defaults.Hum
	}

	if opts.NoiseFloor == zeroBands {
		opts.NoiseFloor = defaults.NoiseFloor
	}

	if opts.ISP == zeroBands {
		opts.ISP = defaults.ISP
	}

	if opts.DynamicRange == zeroBands {
		opts.DynamicRange = defaults.DynamicRange
	}

	if opts.Dropouts == zeroBands {
		opts.Dropouts = defaults.Dropouts
	}

	if opts.TranscodeSharpnessDB == 0 {
		opts.TranscodeSharpnessDB = defaults.TranscodeSharpnessDB
	}

	if opts.UpsampleSharpnessDB == 0 {
		opts.UpsampleSharpnessDB = defaults.UpsampleSharpnessDB
	}

	if opts.DropoutDeltaThreshold == 0 {
		opts.DropoutDeltaThreshold = defaults.DropoutDeltaThreshold
	}
}

func interpretResults(result *Result, opts Options) {
	interpretClipping(result, opts)
	interpretTruncation(result, opts)
	interpretFakeBitDepth(result, opts)
	interpretSpectralChecks(result, opts)
	interpretDCOffset(result, opts)
	interpretStereoChecks(result, opts)
	interpretSilencePadding(result, opts)
	interpretHum(result, opts)
	interpretNoiseFloor(result, opts)
	interpretISP(result, opts)
	interpretLoudness(result, opts)
	interpretDynamicRange(result, opts)
	interpretDropouts(result, opts)

	// Calculate summary stats.
	for _, issue := range result.Issues {
		if issue.Detected {
			result.IssueCount++
		}

		if issue.Severity > result.WorstSeverity {
			result.WorstSeverity = issue.Severity
		}
	}
}

func interpretClipping(result *Result, opts Options) {
	if result.Clipping == nil || opts.Checks&CheckClipping == 0 {
		return
	}

	events := float64(result.Clipping.Events)
	severity, detected := opts.Clipping.Match(events)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "No clipping detected"
	case SeverityMild, SeverityModerate:
		summary = fmt.Sprintf("%d clipping events", result.Clipping.Events)
	case SeveritySevere:
		summary = fmt.Sprintf(
			"%d clipping events, longest run %d samples",
			result.Clipping.Events,
			result.Clipping.LongestRun,
		)
	default:
	}

	result.HasClipping = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckClipping,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretTruncation(result *Result, opts Options) {
	if result.Truncation == nil || opts.Checks&CheckTruncation == 0 {
		return
	}

	severity, detected := opts.Truncation.Match(result.Truncation.FinalRmsDB)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "Clean ending"
	case SeverityMild:
		summary = fmt.Sprintf("Possibly truncated (%.1f dB at end)", result.Truncation.FinalRmsDB)
	case SeverityModerate:
		summary = fmt.Sprintf("Likely truncated (%.1f dB at end)", result.Truncation.FinalRmsDB)
	case SeveritySevere:
		summary = fmt.Sprintf("Truncated mid-audio (%.1f dB at end)", result.Truncation.FinalRmsDB)
	default:
	}

	result.HasTruncation = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckTruncation,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFair,
	})
}

func interpretFakeBitDepth(result *Result, opts Options) {
	if result.BitDepth == nil || opts.Checks&CheckFakeBitDepth == 0 {
		return
	}

	// G115: BitDepth is uint; clamp to max int before converting to avoid overflow.
	effective := safeUintToInt(uint64(result.BitDepth.Effective))
	claimed := safeUintToInt(uint64(result.BitDepth.Claimed))
	detected := effective < claimed

	var (
		severity Severity
		summary  string
	)

	if detected {
		severity = SeveritySevere
		summary = fmt.Sprintf(
			"Fake %d-bit: actually %d-bit (zero-padded)",
			result.BitDepth.Claimed,
			result.BitDepth.Effective,
		)
	} else {
		severity = SeverityNone
		summary = fmt.Sprintf("Genuine %d-bit", result.BitDepth.Claimed)
	}

	result.HasFakeBitDepth = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckFakeBitDepth,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretSpectralChecks(result *Result, opts Options) {
	if result.Spectral == nil {
		return
	}

	interpretFakeSampleRate(result, opts)
	interpretLossyTranscode(result, opts)
}

func interpretFakeSampleRate(result *Result, opts Options) {
	if opts.Checks&CheckFakeSampleRate == 0 {
		return
	}

	detected := result.Spectral.IsUpsampled

	var (
		severity Severity
		summary  string
	)

	if detected {
		severity = SeveritySevere
		summary = fmt.Sprintf(
			"Fake %d Hz: upsampled from %d Hz",
			result.Spectral.ClaimedRate,
			result.Spectral.EffectiveRate,
		)
	} else {
		severity = SeverityNone
		summary = fmt.Sprintf("Genuine %d Hz", result.Spectral.ClaimedRate)
	}

	// Base sample rates (44100, 48000) have no standard lower rate to upsample from,
	// so the check is not applicable and we report 100% confidence in "genuine".
	confidence := confidenceFromSharpness(result.Spectral.UpsampleSharpness, opts.UpsampleSharpnessDB)
	if !detected && result.Spectral.ClaimedRate <= baseSampleRateMax {
		confidence = confidenceFull
	}

	result.HasFakeSampleRate = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckFakeSampleRate,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidence,
	})
}

func interpretLossyTranscode(result *Result, opts Options) {
	if opts.Checks&CheckLossyTranscode == 0 {
		return
	}

	detected := result.Spectral.IsTranscode

	var (
		severity   Severity
		summary    string
		confidence float64
	)

	if detected {
		severity = SeveritySevere
		summary = fmt.Sprintf(
			"Lossy transcode detected: likely %s (cutoff %.0f Hz)",
			result.Spectral.LikelyCodec,
			result.Spectral.TranscodeCutoff,
		)
		// Use the V2 confidence if available, otherwise fall back to sharpness-based.
		if result.Spectral.TranscodeConfidence > 0 {
			confidence = result.Spectral.TranscodeConfidence
		} else {
			confidence = confidenceFromSharpness(result.Spectral.TranscodeSharpness, opts.TranscodeSharpnessDB)
		}
	} else {
		severity = SeverityNone
		summary = "No lossy transcode detected"
		confidence = confidenceFull // high confidence it's NOT a transcode
	}

	result.HasLossyTranscode = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckLossyTranscode,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidence,
	})
}

func interpretDCOffset(result *Result, opts Options) {
	if result.DCOffset == nil || opts.Checks&CheckDCOffset == 0 {
		return
	}

	severity, detected := opts.DCOffset.Match(result.DCOffset.OffsetDB)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "No DC offset"
	case SeverityMild:
		summary = fmt.Sprintf("Minor DC offset (%.1f dB)", result.DCOffset.OffsetDB)
	case SeverityModerate:
		summary = fmt.Sprintf("DC offset present (%.1f dB)", result.DCOffset.OffsetDB)
	case SeveritySevere:
		summary = fmt.Sprintf("Severe DC offset (%.1f dB)", result.DCOffset.OffsetDB)
	default:
	}

	result.HasDCOffset = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckDCOffset,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretStereoChecks(result *Result, opts Options) {
	if result.Stereo == nil {
		return
	}

	interpretFakeStereo(result, opts)
	interpretPhaseIssues(result, opts)
	interpretInvertedPhase(result, opts)
	interpretChannelImbalance(result, opts)
}

func interpretFakeStereo(result *Result, opts Options) {
	if opts.Checks&CheckFakeStereo == 0 {
		return
	}

	detected := result.Stereo.Correlation > fakeStereoCorrThreshold &&
		result.Stereo.DifferenceDB < fakeStereoDBThreshold

	var (
		severity Severity
		summary  string
	)

	if detected {
		severity = SeverityModerate
		summary = fmt.Sprintf("Fake stereo: channels identical (correlation %.3f)", result.Stereo.Correlation)
	} else {
		severity = SeverityNone
		summary = "Real stereo content"
	}

	result.HasFakeStereo = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckFakeStereo,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretPhaseIssues(result *Result, opts Options) {
	if opts.Checks&CheckPhaseIssues == 0 {
		return
	}

	severity, detected := opts.PhaseIssues.Match(result.Stereo.CancellationDB)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "Mono-compatible"
	case SeverityMild:
		summary = fmt.Sprintf("Minor phase issues (%.1f dB cancellation)", result.Stereo.CancellationDB)
	case SeverityModerate:
		summary = fmt.Sprintf("Phase issues: %.1f dB lost in mono", result.Stereo.CancellationDB)
	case SeveritySevere:
		summary = fmt.Sprintf("Severe phase issues: %.1f dB cancellation in mono", result.Stereo.CancellationDB)
	default:
	}

	result.HasPhaseIssues = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckPhaseIssues,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretInvertedPhase(result *Result, opts Options) {
	if opts.Checks&CheckInvertedPhase == 0 {
		return
	}

	detected := result.Stereo.Correlation < invertedPhaseThreshold

	var (
		severity Severity
		summary  string
	)

	if detected {
		severity = SeveritySevere
		summary = fmt.Sprintf(
			"Inverted phase: one channel polarity flipped (correlation %.3f)",
			result.Stereo.Correlation,
		)
	} else {
		severity = SeverityNone
		summary = "Phase polarity OK"
	}

	result.HasInvertedPhase = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckInvertedPhase,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretChannelImbalance(result *Result, opts Options) {
	if opts.Checks&CheckChannelImbalance == 0 {
		return
	}

	imbalance := abs(result.Stereo.ImbalanceDB)
	severity, detected := opts.ChannelImbalance.Match(imbalance)

	var summary string

	side := "left"
	if result.Stereo.ImbalanceDB < 0 {
		side = "right"
	}

	switch severity {
	case SeverityNone:
		summary = "Channels balanced"
	case SeverityMild:
		summary = fmt.Sprintf("Slight imbalance: %s louder by %.1f dB", side, imbalance)
	case SeverityModerate:
		summary = fmt.Sprintf("Channel imbalance: %s louder by %.1f dB", side, imbalance)
	case SeveritySevere:
		summary = fmt.Sprintf("Severe imbalance: %s louder by %.1f dB", side, imbalance)
	default:
	}

	result.HasChannelImbalance = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckChannelImbalance,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretSilencePadding(result *Result, opts Options) {
	if result.Silence == nil || opts.Checks&CheckSilencePadding == 0 {
		return
	}

	worst := result.Silence.LeadingSec
	if result.Silence.TrailingSec > worst {
		worst = result.Silence.TrailingSec
	}

	severity, detected := opts.SilencePadding.Match(worst)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "No excessive silence padding"
	case SeverityMild, SeverityModerate, SeveritySevere:
		summary = fmt.Sprintf(
			"Silence padding: %.1fs leading, %.1fs trailing",
			result.Silence.LeadingSec,
			result.Silence.TrailingSec,
		)
	default:
	}

	result.HasSilencePadding = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckSilencePadding,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretHum(result *Result, opts Options) {
	if result.Spectral == nil || opts.Checks&CheckHum == 0 {
		return
	}

	detected := result.Spectral.Has50HzHum || result.Spectral.Has60HzHum

	var (
		severity Severity
		summary  string
	)

	if detected {
		var freqs string

		switch {
		case result.Spectral.Has50HzHum && result.Spectral.Has60HzHum:
			freqs = "50Hz and 60Hz"
		case result.Spectral.Has50HzHum:
			freqs = "50Hz"
		default:
			freqs = "60Hz"
		}

		severity, _ = opts.Hum.Match(result.Spectral.HumLevelDB)
		if severity == SeverityNone {
			// Detected but below band thresholds: default to mild.
			severity = SeverityMild
		}

		if severity == SeveritySevere {
			summary = fmt.Sprintf("Severe %s hum (%.1f dB)", freqs, result.Spectral.HumLevelDB)
		} else {
			summary = fmt.Sprintf("%s hum detected (%.1f dB)", freqs, result.Spectral.HumLevelDB)
		}
	} else {
		severity = SeverityNone
		summary = "No mains hum detected"
	}

	result.HasHum = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckHum,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceGood,
	})
}

func interpretNoiseFloor(result *Result, opts Options) {
	if result.Spectral == nil || opts.Checks&CheckNoiseFloor == 0 {
		return
	}

	severity, detected := opts.NoiseFloor.Match(result.Spectral.NoiseFloorDB)

	var summary string

	switch severity {
	case SeverityNone:
		summary = fmt.Sprintf("Clean recording (noise floor %.1f dB)", result.Spectral.NoiseFloorDB)
	case SeverityMild:
		summary = fmt.Sprintf("Slightly elevated noise floor (%.1f dB)", result.Spectral.NoiseFloorDB)
	case SeverityModerate:
		summary = fmt.Sprintf("Elevated noise floor (%.1f dB)", result.Spectral.NoiseFloorDB)
	case SeveritySevere:
		summary = fmt.Sprintf("High noise floor (%.1f dB)", result.Spectral.NoiseFloorDB)
	default:
	}

	result.HasHighNoiseFloor = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckNoiseFloor,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceModerate,
	})
}

func interpretISP(result *Result, opts Options) {
	if result.TruePeak == nil || opts.Checks&CheckInterSamplePeaks == 0 {
		return
	}

	ispCount := float64(result.TruePeak.ISPCount)
	severity, detected := opts.ISP.Match(ispCount)

	var summary string

	switch severity {
	case SeverityNone:
		summary = fmt.Sprintf("No inter-sample peaks (true peak %.1f dBTP)", result.TruePeak.TruePeakDB)
	case SeverityMild, SeverityModerate:
		summary = fmt.Sprintf("%d ISPs, max overshoot %.2f dB", result.TruePeak.ISPCount, result.TruePeak.ISPMaxDB)
	case SeveritySevere:
		summary = fmt.Sprintf(
			"Pervasive ISPs: %d events, max overshoot %.2f dB",
			result.TruePeak.ISPCount,
			result.TruePeak.ISPMaxDB,
		)
	default:
	}

	result.HasInterSamplePeaks = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckInterSamplePeaks,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretLoudness(result *Result, opts Options) {
	if result.Loudness == nil || opts.Checks&CheckLoudness == 0 {
		return
	}

	result.Issues = append(result.Issues, Issue{
		Check:    CheckLoudness,
		Detected: false, // informational
		Severity: SeverityNone,
		Summary: fmt.Sprintf(
			"Loudness: %.1f LUFS, range %.1f LU",
			result.Loudness.IntegratedLUFS,
			result.Loudness.LoudnessRange,
		),
		Confidence: confidenceFull,
	})
}

func interpretDynamicRange(result *Result, opts Options) {
	if result.Loudness == nil || opts.Checks&CheckDynamicRange == 0 {
		return
	}

	drScore := float64(result.Loudness.DRScore)
	severity, detected := opts.DynamicRange.Match(drScore)

	var summary string

	switch severity {
	case SeverityNone:
		if result.Loudness.DRScore >= excellentDynamicsThreshold {
			summary = fmt.Sprintf("Excellent dynamics (DR%d)", result.Loudness.DRScore)
		} else {
			summary = fmt.Sprintf("Good dynamics (DR%d)", result.Loudness.DRScore)
		}
	case SeverityMild:
		summary = fmt.Sprintf("Compressed (DR%d)", result.Loudness.DRScore)
	case SeverityModerate:
		summary = fmt.Sprintf("Heavily compressed (DR%d)", result.Loudness.DRScore)
	case SeveritySevere:
		summary = fmt.Sprintf("Brickwalled (DR%d)", result.Loudness.DRScore)
	default:
	}

	result.IsBrickwalled = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckDynamicRange,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceFull,
	})
}

func interpretDropouts(result *Result, opts Options) {
	if result.Dropout == nil || opts.Checks&CheckDropouts == 0 {
		return
	}

	total := float64(result.Dropout.DeltaCount + result.Dropout.ZeroRunCount + result.Dropout.DCJumpCount)
	severity, detected := opts.Dropouts.Match(total)

	var summary string

	switch severity {
	case SeverityNone:
		summary = "No dropouts or glitches"
	case SeverityMild, SeverityModerate, SeveritySevere:
		summary = fmt.Sprintf(
			discontinuitiesFmt,
			int(total),
			result.Dropout.DeltaCount,
			result.Dropout.ZeroRunCount,
			result.Dropout.DCJumpCount,
			result.Dropout.WorstDB,
		)
	default:
	}

	result.HasDropouts = detected
	result.Issues = append(result.Issues, Issue{
		Check:      CheckDropouts,
		Detected:   detected,
		Severity:   severity,
		Summary:    summary,
		Confidence: confidenceGood,
	})
}

// confidenceFromSharpness returns a confidence score based on whether the measured
// sharpness exceeds the given threshold.
func confidenceFromSharpness(measured, threshold float64) float64 {
	if measured > threshold {
		return confidenceHigh
	}

	return confidenceLow
}

// safeUintToInt converts a uint64 to int, clamping at math.MaxInt to prevent overflow.
func safeUintToInt(val uint64) int {
	if val > uint64(math.MaxInt) {
		return math.MaxInt
	}

	return int(val)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}

	return x
}
