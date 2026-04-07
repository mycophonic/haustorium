package types

type BitDepth uint

const (
	Depth16 BitDepth = 16
	Depth24 BitDepth = 24
	Depth32 BitDepth = 32
)

// PCMFormat of the original input before PCM extraction (except BitDepth, from the PCM, vs. ExpectedBitDepth,
// from the original media).
type PCMFormat struct {
	SampleRate       int
	BitDepth         BitDepth
	Channels         uint
	ExpectedBitDepth BitDepth
}

// BitDepthAuthenticity contains results returned by the bitdepth analyzer.
type BitDepthAuthenticity struct {
	Claimed   BitDepth // what the file says it is
	Effective BitDepth // what it actually is
	IsPadded  bool     // Effective < Claimed
	Samples   uint64   // total samples analyzed
}

// ChannelClipping contains per channel clipping detection results.
type ChannelClipping struct {
	Events         uint64
	ClippedSamples uint64
	LongestRun     uint64
}

// ClippingDetection contains overall clipping detection results.
type ClippingDetection struct {
	Events         uint64
	ClippedSamples uint64
	LongestRun     uint64
	Samples        uint64
	Channels       []ChannelClipping
}

/*
Truncation Detection Heuristics

The Result provides raw measurements. Here are suggested interpretation guidelines:

## By Final RMS Level

| FinalRmsDB    | Interpretation                | Confidence |
|---------------|-------------------------------|------------|
| < -60 dB      | Silence. Legitimate ending.   | High       |
| -60 to -50 dB | Near-silence. Likely OK.      | Medium     |
| -50 to -40 dB | Ambiguous. Room noise? Fade?  | Low        |
| -40 to -30 dB | Suspicious. Possible truncate.| Medium     |
| > -30 dB      | Truncated. Audio cut mid-play.| High       |

## By Genre

| Genre              | Expected End Behavior         | Suggested Threshold |
|--------------------|-------------------------------|---------------------|
| Classical          | Long decay, room reverb       | -55 dB              |
| Electronic/EDM     | Often hard cuts by design     | -35 dB              |
| Rock/Pop           | Fade out or ring out          | -45 dB              |
| Live recordings    | Crowd noise, room tone        | -50 dB              |
| Podcasts/Spoken    | Usually clean silence         | -55 dB              |

## Combining RMS and Peak

| RmsDB   | PeakDB  | Interpretation                              |
|---------|---------|---------------------------------------------|
| < -60   | < -60   | Clean silence. OK.                          |
| < -60   | > -40   | Spike at end (click/glitch). Investigate.   |
| > -40   | > -40   | Sustained signal at end. Truncated.         |
| > -30   | > -20   | Definitely truncated mid-audio.             |

## Default Recommendation

For general-purpose detection without genre context:

    if FinalRmsDB > -40 && FinalPeakDB > -35 {
        // Likely truncated
    }

This catches obvious truncations while avoiding false positives on
intentional hard endings common in electronic music.
*/

// TruncationDetection contains truncation results.
type TruncationDetection struct {
	IsTruncated   bool
	FinalRmsDB    float64 // RMS of final window in dB
	FinalPeakDB   float64 // Peak of final window in dB
	SamplesInTail uint64
}

/*
DC Offset Interpretation

| Offset (abs) | OffsetDB | Interpretation                    |
|--------------|----------|-----------------------------------|
| < 0.001      | < -60 dB | Clean. No issues.                 |
| 0.001-0.01   | -60 to -40 dB | Minor. Usually inaudible.    |
| 0.01-0.05    | -40 to -26 dB | Noticeable. Should fix.      |
| > 0.05       | > -26 dB | Severe. Causes clicks, wastes headroom. |

Per-channel offsets can identify which channel has the problem.
Positive offset = waveform shifted up.
Negative offset = waveform shifted down.
*/

// DCOffsetResult contains DC offset results.
type DCOffsetResult struct {
	Offset   float64   // overall normalized offset (-1.0 to 1.0)
	OffsetDB float64   // overall offset as dB (more negative = less offset)
	Channels []float64 // per-channel offset, normalized
	Samples  uint64
}

/*
Stereo Analysis Interpretation

## Quick Diagnosis

| Correlation | DifferenceDB | Diagnosis                    |
|-------------|--------------|------------------------------|
| > 0.95      | < -60 dB     | Fake stereo (identical L/R)  |
| < -0.95     | —            | Inverted phase (L = -R)      |
| 0.5 to 0.95 | > -20 dB     | Normal stereo                |
| < 0.5       | —            | Wide/decorrelated stereo     |

## Fake Stereo Detection

| DifferenceDB | Interpretation                          |
|--------------|-----------------------------------------|
| < -80 dB     | Identical channels. Definitely fake.    |
| -80 to -60   | Near-identical. Fake or very narrow.    |
| -60 to -40   | Minimal separation. Suspicious.         |
| > -40 dB     | Real stereo content present.            |

## Inverted Phase Detection

| Correlation | Interpretation                          |
|-------------|-----------------------------------------|
| < -0.99     | Fully inverted. One channel flipped.    |
| -0.99 to -0.9 | Mostly inverted. Serious problem.     |
| -0.9 to -0.5 | Significant out-of-phase content.      |
| > -0.5      | Not inverted.                           |

## Phase Cancellation (Mono Compatibility)

| CancellationDB | Interpretation                          |
|----------------|-----------------------------------------|
| < 1 dB         | Mono-safe. Minimal cancellation.        |
| 1-3 dB         | Minor loss in mono. Usually acceptable. |
| 3-6 dB         | Noticeable. Will sound hollow in mono.  |
| > 6 dB         | Severe. Major elements disappear.       |

## Channel Imbalance

| ImbalanceDB (abs) | Interpretation                       |
|-------------------|--------------------------------------|
| < 0.5 dB          | Balanced. Normal.                    |
| 0.5-1.0 dB        | Slight imbalance. Usually fine.      |
| 1.0-2.0 dB        | Noticeable. May be intentional.      |
| 2.0-3.0 dB        | Significant. Likely a problem.       |
| > 3.0 dB          | Severe. Equipment issue or bad mix.  |

Sign: positive = left louder, negative = right louder.

## Decision Tree

    if Correlation > 0.98 && DifferenceDB < -60 {
        // Fake stereo
    } else if Correlation < -0.95 {
        // Inverted phase
    } else if CancellationDB > 3 {
        // Phase issues (mono compatibility problem)
    } else if math.Abs(ImbalanceDB) > 2 {
        // Channel imbalance
    } else {
        // OK
    }
*/

// StereoResult contains stereo results.
type StereoResult struct {
	Correlation    float64 // 1.0 = identical, 0 = uncorrelated, -1.0 = inverted
	DifferenceDB   float64 // RMS of (L-R) in dB; very negative = identical channels
	MonoSumDB      float64 // RMS of (L+R) in dB; very negative = inverted phase
	StereoRmsDB    float64 // RMS of original stereo signal
	CancellationDB float64 // StereoRmsDB - MonoSumDB; positive = cancellation when summed
	LeftRmsDB      float64 // RMS of left channel
	RightRmsDB     float64 // RMS of right channel
	ImbalanceDB    float64 // LeftRmsDB - RightRmsDB; positive = left louder
	Frames         uint64
}

/*
Silence Detection Interpretation

## Common Patterns

| Pattern                     | Meaning                              |
|-----------------------------|--------------------------------------|
| LeadingSec > 2              | Pre-roll silence, possibly from rip  |
| TrailingSec > 5             | Hidden track or excessive padding    |
| Multiple mid-track segments | Multi-movement work or compilation   |
| Segment at ~3:30            | Often a hidden track after "silence" |

## Threshold Guidelines

| ThresholdDb | Catches                              |
|-------------|--------------------------------------|
| -70 dB      | Only true digital silence            |
| -60 dB      | Standard. Catches quiet passages.    |
| -50 dB      | Aggressive. May catch soft intros.   |
| -40 dB      | Very aggressive. Room noise level.   |

## Minimum Duration Guidelines

| MinDurationMs | Use Case                             |
|---------------|--------------------------------------|
| 100           | Detect brief gaps (glitches)         |
| 500           | Detect intentional pauses            |
| 1000          | Track boundaries (default)           |
| 3000          | Only major gaps / hidden tracks      |

## Use Cases

Track splitting:
    opts := silence.Options{ThresholdDb: -55, MinDurationMs: 1500}

Hidden track detection:
    opts := silence.Options{ThresholdDb: -60, MinDurationMs: 10000}

Glitch/dropout hunting:
    opts := silence.Options{ThresholdDb: -70, MinDurationMs: 50}
*/

// SilenceSegment represents a silence segment.
type SilenceSegment struct {
	StartSample uint64
	EndSample   uint64
	StartSec    float64
	EndSec      float64
	DurationSec float64
	RmsDB       float64 // actual level during this segment
}

// SilenceResult aggregates all silence segments and provide high level result.
type SilenceResult struct {
	Segments      []SilenceSegment
	TotalSilence  float64 // total silence duration in seconds
	LeadingSec    float64 // silence at start
	TrailingSec   float64 // silence at end
	TotalDuration float64 // total file duration in seconds
	Frames        uint64
}

/*
Spectral Analysis Interpretation

## Sample Rate Authenticity (Upsampling)

| IsUpsampled | EffectiveRate | Meaning                              |
|-------------|---------------|--------------------------------------|
| false       | 0             | Genuine hi-res (or inconclusive)     |
| true        | 44100         | CD upsampled to 88.2/96/176.4/192k   |
| true        | 48000         | 48k upsampled to 96/192k             |
| true        | 88200         | 88.2k upsampled to 176.4k            |
| true        | 96000         | 96k upsampled to 192k                |

| Sharpness (dB/oct) | Interpretation                       |
|--------------------|--------------------------------------|
| < 20               | Natural rolloff, genuine             |
| 20-40              | Suspicious, possibly filtered        |
| > 40               | Brick wall, definitely upsampled     |
| > 60               | Extreme brick wall, cheap upsampler  |

Energy relative to 1-10kHz reference:

| Band     | Genuine Hi-Res | Upsampled CD |
|----------|----------------|--------------|
| 20 kHz   | -5 to -15 dB   | -5 to -15 dB |
| 22 kHz   | -10 to -20 dB  | < -60 dB     |
| 24 kHz   | -10 to -25 dB  | < -60 dB     |
| 30 kHz   | -15 to -30 dB  | < -60 dB     |

Caveats

- Solo instruments / voice may have little HF content naturally
- Some music is mastered with steep low-pass filters
- Old analog recordings have natural HF rolloff
- A "genuine" result doesn't guarantee audible benefit

Combine with listening tests for final judgment.

## Lossy Transcode Detection

| TranscodeCutoff | LikelyCodec      | Notes                    |
|-----------------|------------------|--------------------------|
| ~15.5 kHz       | AAC 128          | iTunes default era       |
| ~16 kHz         | MP3 128          | Common piracy bitrate    |
| ~18 kHz         | MP3 192 / AAC    | "Good enough" bitrate    |
| ~19 kHz         | MP3 256 / AAC    | Near-transparent         |
| ~20 kHz         | MP3 320          | Max MP3 bitrate          |

TranscodeSharpness > 30 dB/octave = confident detection
TranscodeSharpness > 50 dB/octave = obvious brick wall

## Hum Detection

| HumLevelDB | Interpretation                       |
|------------|--------------------------------------|
| < 10 dB    | Clean or negligible                  |
| 10-20 dB   | Audible hum present                  |
| 20-30 dB   | Significant contamination            |
| > 30 dB    | Severe, equipment malfunction        |

50Hz = European mains, turntable motors
60Hz = North American mains

## Noise Floor

| NoiseFloorDB | Interpretation                       |
|--------------|--------------------------------------|
| < -40 dB     | Excellent, clean recording           |
| -40 to -30   | Good, typical studio recording       |
| -30 to -20   | Elevated noise, older recording      |
| -20 to -10   | High noise, lo-fi or tape hiss       |
| > -10 dB     | Very noisy, possible problem         |

## Spectral Centroid

| Centroid Hz | Character                            |
|-------------|--------------------------------------|
| < 1500      | Dark, bassy                          |
| 1500-2500   | Warm, balanced                       |
| 2500-4000   | Bright, present                      |
| > 4000      | Very bright, potentially harsh       |

## Decision Tree

    if IsUpsampled {
        // Fake hi-res sample rate
    }
    if IsTranscode {
        // Lossy source re-encoded to lossless
    }
    if Has50HzHum || Has60HzHum {
        // Ground loop or equipment issue
    }
    if NoiseFloorDB > -20 {
        // Investigate source quality
    }
*/

// SpectralResult contains the result of spectral analysis.
type SpectralResult struct {
	// Sample rate authenticity
	ClaimedRate       int
	EffectiveRate     int // detected original rate; 0 = genuine
	IsUpsampled       bool
	UpsampleCutoff    float64 // Hz where brick wall detected
	UpsampleSharpness float64 // dB/octave at cutoff

	// Lossy transcode detection
	IsTranscode          bool
	TranscodeCutoff      float64 // Hz; 0 if not detected
	TranscodeSharpness   float64
	LikelyCodec          string  // "MP3 128", "MP3 320", "AAC 128", etc.
	TranscodeConfidence  float64 // 0.0-1.0; reduced when cutoff looks like mastering LPF
	CutoffConsistency    float64 // stddev of cutoff frequency across windows; low = mastering filter
	HasUltrasonicContent bool    // true if any content exists above the detected cutoff

	// Hum detection
	Has50HzHum bool
	Has60HzHum bool
	HumLevelDB float64 // level of worst hum relative to signal

	// Noise floor
	NoiseFloorDB float64 // HF noise level relative to 1-10kHz

	// Tonal character
	SpectralCentroid float64 // Hz; higher = brighter

	// Raw data for debugging/display
	BandEnergy []float64
	BandFreqs  []float64

	Frames uint64
}

/*
True Peak / Inter-Sample Peak Interpretation

## True Peak vs Sample Peak

| Difference | Interpretation                           |
|------------|------------------------------------------|
| < 0.3 dB   | Normal. No significant ISPs.             |
| 0.3-1.0 dB | Minor ISPs. Usually inaudible.           |
| 1.0-2.0 dB | Moderate ISPs. May clip some DACs.       |
| > 2.0 dB   | Severe ISPs. Will clip most DACs.        |

## True Peak Level (for streaming/broadcast)

| TruePeakDB | Compliance                               |
|------------|------------------------------------------|
| < -2.0 dBTP| Spotify, YouTube, Apple Music safe       |
| < -1.0 dBTP| Most streaming platforms safe            |
| < 0 dBTP   | No ISPs, but no headroom for conversion  |
| > 0 dBTP   | ISPs present. Will clip.                 |

## ISP Count

| ISPCount   | Interpretation                           |
|------------|------------------------------------------|
| 0          | Clean. No inter-sample overs.            |
| 1-100      | Occasional peaks. Minor issue.           |
| 100-1000   | Frequent ISPs. Mastering problem.        |
| > 1000     | Pervasive. Likely brickwall limited.     |

## ISP Max Overshoot

| ISPMaxDB   | Severity                                 |
|------------|------------------------------------------|
| 0-0.5 dB   | Mild clipping on sensitive DACs          |
| 0.5-1.0 dB | Audible clipping on most DACs            |
| 1.0-2.0 dB | Significant distortion                   |
| > 2.0 dB   | Severe distortion                        |

## Relationship to Clipping Detection

- Clipping (sample domain): catches 0dBFS flattops
- ISP (reconstructed): catches peaks BETWEEN samples

A file can have:
- Clipping but no ISPs (hard limited, flat tops)
- ISPs but no clipping (peaks between samples exceed 0dB)
- Both (common in loudness war masters)
- Neither (properly mastered)

## Broadcast Standards

| Standard     | True Peak Limit |
|--------------|-----------------|
| EBU R128     | -1.0 dBTP       |
| ATSC A/85    | -2.0 dBTP       |
| Spotify      | -1.0 dBTP       |
| Apple Music  | -1.0 dBTP       |
| YouTube      | -1.0 dBTP       |
*/

// TruePeakResult contains the peak analysis.
type TruePeakResult struct {
	TruePeakDB   float64 // max reconstructed level; > 0 = ISP present
	SamplePeakDB float64 // max original sample level
	ISPCount     uint64  // number of inter-sample peaks > 0 dBFS
	ISPMaxDB     float64 // worst ISP overshoot above 0 dBFS
	Frames       uint64

	// Enhanced ISP analysis
	ISPDensityPeak  float64 // worst-case ISPs per second (1-second window)
	ISPDensityAvg   float64 // average ISPs per second across file
	ISPsAboveHalfdB uint64  // count of ISPs with >0.5dB overshoot
	ISPsAbove1dB    uint64  // count of ISPs with >1.0dB overshoot
	ISPsAbove2dB    uint64  // count of ISPs with >2.0dB overshoot
	WorstDensitySec float64 // timestamp (seconds) of peak density window
}

/*
Loudness Analysis Interpretation

## Integrated Loudness (LUFS)

| IntegratedLUFS | Context                                 |
|----------------|----------------------------------------|
| -23 to -18     | Broadcast/streaming target range       |
| -16 to -14     | Typical modern pop/rock master         |
| -12 to -10     | Loud/compressed master                 |
| -9 to -6       | Extremely loud (loudness war casualty) |

## Streaming Targets

| Platform       | Target LUFS | Normalization          |
|----------------|-------------|------------------------|
| Spotify        | -14         | Turns down loud tracks |
| Apple Music    | -16         | Sound Check enabled    |
| YouTube        | -14         | Always normalized      |
| Amazon Music   | -14         | Default on             |
| Tidal          | -14         | Loudness normalization |

## Loudness Range (LRA)

| LRA (LU) | Interpretation                          |
|----------|----------------------------------------|
| < 5      | Very compressed, little dynamics        |
| 5-10     | Moderate dynamics, typical pop/rock     |
| 10-15    | Good dynamics, well-mastered            |
| 15-25    | Wide dynamics, classical/jazz           |
| > 25     | Extreme dynamics, may need limiting     |

## Dynamic Range (DR Score)

| DR Score | Interpretation                          |
|----------|----------------------------------------|
| DR1-DR4  | Severely crushed. Loudness war victim.  |
| DR5-DR7  | Compressed. Typical modern loud master. |
| DR8-DR10 | Moderate. Acceptable for most genres.   |
| DR11-DR14| Good dynamics. Well-mastered.           |
| DR15+    | Excellent dynamics. Audiophile grade.   |

## Relationship Between Metrics

- LUFS = perceived loudness (K-weighted, gated)
- LRA = loudness variation over time
- DR = crest factor (peak-to-RMS ratio)

A track can be:
- Loud (low LUFS) but dynamic (high DR) = limited peaks, preserved transients
- Quiet (high LUFS) but flat (low DR) = unusual, possibly broken
- Loud and flat (low LUFS, low DR) = typical loudness war master

## Common Patterns

| LUFS | DR | Meaning                                  |
|------|-----|----------------------------------------|
| -14  | 8   | Modern streaming-optimized master       |
| -10  | 5   | Loudness war casualty                   |
| -18  | 12  | Older or audiophile-targeted master     |
| -23  | 14  | Broadcast-compliant, dynamic            |
*/

// LoudnessResult contains Peak, RMS, etc.
type LoudnessResult struct {
	// EBU R128 LUFS
	IntegratedLUFS float64 // overall loudness (gated)
	ShortTermMax   float64 // max 3s window
	MomentaryMax   float64 // max 400ms window
	LoudnessRange  float64 // LRA in LU

	// Dynamic Range
	DRScore int     // DR1-DR20 scale (crest factor based)
	DRValue float64 // raw DR value before rounding
	PeakDB  float64 // peak level used
	RmsDB   float64 // RMS level used

	Frames uint64
}

/*
Dropout/Glitch Detection Interpretation

## Event Types

| Type      | Cause                                    |
|-----------|------------------------------------------|
| delta     | Sudden sample jump (buffer underrun, bad edit) |
| zero_run  | Digital silence (DAT dropout, USB glitch) |
| dc_jump   | Sudden offset shift (bad splice, hardware) |

## Delta Severity

| Severity | Interpretation                           |
|----------|------------------------------------------|
| 0.5-0.7  | Moderate discontinuity, may be audible   |
| 0.7-0.9  | Large jump, likely audible click         |
| > 0.9    | Near full-scale jump, definite pop       |

## Zero Run Duration

| DurationMs | Interpretation                          |
|------------|----------------------------------------|
| 1-5 ms     | Micro-dropout, may be inaudible         |
| 5-20 ms    | Short dropout, audible as click/gap     |
| 20-100 ms  | Obvious gap, clearly audible            |
| > 100 ms   | Major dropout, missing audio            |

## DC Jump Severity

| Severity | Interpretation                           |
|----------|------------------------------------------|
| 0.1-0.2  | Minor offset shift                       |
| 0.2-0.5  | Significant shift, audible thump         |
| > 0.5    | Major DC event, loud pop                 |

## Threshold Tuning

| Scenario                | DeltaThreshold | ZeroRunMinMs |
|-------------------------|----------------|--------------|
| Strict (find everything)| 0.3            | 0.5          |
| Default (balanced)      | 0.5            | 1.0          |
| Relaxed (obvious only)  | 0.7            | 5.0          |

## Common Patterns

| Pattern                      | Likely Cause                |
|------------------------------|----------------------------|
| Single delta, one channel    | Bad edit point             |
| Zero runs, both channels     | Buffer underrun, USB glitch|
| Zero runs, one channel       | DAT/tape dropout           |
| DC jumps throughout          | Hardware issue, bad ADC    |
| Deltas at regular intervals  | Clock sync issue           |

## Relationship to Other Analyses

- Dropouts often co-occur with clipping (both symptoms of bad recording)
- Zero runs ≠ silence segments (zeros are exact 0, silence is low level)
- DC jumps may cause clipping detection false positives
*/

// An Event is a dropout event.
type Event struct {
	Frame      uint64
	TimeSec    float64
	Channel    int
	Type       EventType
	Severity   float64 // magnitude of discontinuity (0-1 normalized)
	DurationMs float64 // for zero runs
}

// An EventType qualifies a dropout event.
type EventType int

const (
	EventDelta   EventType = iota // sudden large jump
	EventZeroRun                  // run of zeros (digital dropout)
	EventDCJump                   // sudden DC offset change
)

func (e EventType) String() string {
	switch e {
	case EventDelta:
		return "delta"
	case EventZeroRun:
		return "zero_run"
	case EventDCJump:
		return "dc_jump"
	}

	return "unknown"
}

// DropoutResult aggregates all dropout events.
type DropoutResult struct {
	Events       []Event
	DeltaCount   int     // sudden jumps
	ZeroRunCount int     // zero runs
	DCJumpCount  int     // DC offset jumps
	WorstDB      float64 // severity of worst event in dB
	Frames       uint64
}
