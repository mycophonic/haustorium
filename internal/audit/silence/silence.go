// Package silence detects silence segments in PCM audio streams.
package silence

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

const (
	defaultThresholdDB   = -60.0
	defaultMinDurationMs = 1000
	defaultWindowMs      = 50

	// dbBase is the base for dB-to-linear conversion: 10^(dB/20).
	dbBase = 10
)

// Options configures silence detection parameters.
type Options struct {
	ThresholdDB   float64 // below this = silence (default -60)
	MinDurationMs int     // minimum silence to report (default 1000)
	WindowMs      int     // RMS window size (default 50)
}

// DefaultOptions returns the default silence detection options.
func DefaultOptions() Options {
	return Options{
		ThresholdDB:   defaultThresholdDB,
		MinDurationMs: defaultMinDurationMs,
		WindowMs:      defaultWindowMs,
	}
}

// silenceState tracks the state of an ongoing silence region.
type silenceState struct {
	active bool
	start  uint64
	sumSq  float64
	count  uint64
}

// windowState tracks the RMS window accumulator.
type windowState struct {
	sumSq float64
	count int
}

// decodeFrameSumSq decodes one PCM frame and returns the average sum-of-squares across channels.
func decodeFrameSumSq(data []byte, frameOffset int, bitDepth types.BitDepth, numChannels int, maxVal float64) float64 {
	var frameSumSq float64

	switch bitDepth {
	case types.Depth16:
		for ch := range numChannels {
			sample := float64(
				int16(binary.LittleEndian.Uint16(data[frameOffset+ch*2:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			frameSumSq += sample * sample
		}
	case types.Depth24:
		for ch := range numChannels {
			offset := frameOffset + ch*3

			raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<shared.Shift16
			if raw&shared.Mask24Sign != 0 {
				raw |= ^shared.Mask24Extend
			}

			sample := float64(raw) / maxVal
			frameSumSq += sample * sample
		}
	case types.Depth32:
		for ch := range numChannels {
			sample := float64(
				int32(binary.LittleEndian.Uint32(data[frameOffset+ch*4:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			frameSumSq += sample * sample
		}
	default:
		return 0
	}

	return frameSumSq / float64(numChannels)
}

// processWindow evaluates a completed RMS window and updates silence tracking.
func processWindow(
	ws *windowState,
	ss *silenceState,
	currentFrame uint64,
	threshold float64,
	minSilenceFrames uint64,
	sampleRate int,
	segments *[]types.SilenceSegment,
) {
	if ws.count == 0 {
		return
	}

	rms := math.Sqrt(ws.sumSq / float64(ws.count))
	isSilent := rms < threshold

	switch {
	case isSilent && !ss.active:
		ss.active = true
		ss.start = currentFrame - uint64(ws.count) //nolint:gosec // value is non-negative by construction
		ss.sumSq = ws.sumSq
		ss.count = uint64(ws.count) //nolint:gosec // value is non-negative by construction
	case isSilent && ss.active:
		ss.sumSq += ws.sumSq
		ss.count += uint64(ws.count) //nolint:gosec // value is non-negative by construction
	case !isSilent && ss.active:
		silenceEnd := currentFrame - uint64(ws.count) //nolint:gosec // value is non-negative by construction
		emitSilenceSegment(ss, silenceEnd, minSilenceFrames, sampleRate, segments)
		ss.active = false
	default:
	}

	ws.sumSq = 0
	ws.count = 0
}

// emitSilenceSegment appends a silence segment if it meets the minimum duration.
func emitSilenceSegment(
	ss *silenceState,
	silenceEnd, minSilenceFrames uint64,
	sampleRate int,
	segments *[]types.SilenceSegment,
) {
	silenceFrames := silenceEnd - ss.start
	if silenceFrames < minSilenceFrames {
		return
	}

	silenceRms := math.Sqrt(ss.sumSq / float64(ss.count))

	silenceDB := float64(shared.DBMultiplier) * math.Log10(silenceRms)
	if math.IsInf(silenceDB, -1) {
		silenceDB = shared.SilenceFloorDB
	}

	*segments = append(*segments, types.SilenceSegment{
		StartSample: ss.start,
		EndSample:   silenceEnd,
		StartSec:    float64(ss.start) / float64(sampleRate),
		EndSec:      float64(silenceEnd) / float64(sampleRate),
		DurationSec: float64(silenceFrames) / float64(sampleRate),
		RmsDB:       silenceDB,
	})
}

// Detect identifies silence segments in PCM audio data.
func Detect(r io.Reader, format types.PCMFormat, opts Options) (*types.SilenceResult, error) {
	if opts.ThresholdDB == 0 {
		opts.ThresholdDB = defaultThresholdDB
	}

	if opts.MinDurationMs == 0 {
		opts.MinDurationMs = defaultMinDurationMs
	}

	if opts.WindowMs == 0 {
		opts.WindowMs = defaultWindowMs
	}

	bytesPerSample := int(format.BitDepth / 8)         //nolint:gosec // bit depth and channel count are small constants
	frameSize := bytesPerSample * int(format.Channels) //nolint:gosec // bit depth and channel count are small constants
	numChannels := int(format.Channels)                //nolint:gosec // bit depth and channel count are small constants

	// Window size in frames
	windowFrames := max(format.SampleRate*opts.WindowMs/shared.MsPerSec, 1)

	minSilenceFrames := uint64(
		format.SampleRate,
	) * uint64(
		opts.MinDurationMs,
	) / uint64(shared.MsPerSec)

	buf := make([]byte, frameSize*4096)

	var maxVal float64

	switch format.BitDepth {
	case types.Depth16:
		maxVal = shared.MaxValue16
	case types.Depth24:
		maxVal = shared.MaxValue24
	case types.Depth32:
		maxVal = shared.MaxValue32
	default:
	}

	threshold := math.Pow(dbBase, opts.ThresholdDB/float64(shared.DBMultiplier))

	var (
		segments     []types.SilenceSegment
		currentFrame uint64
	)

	ws := &windowState{}
	ss := &silenceState{}

	for {
		n, err := r.Read(buf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			data := buf[:completeFrames]

			for i := 0; i < len(data); i += frameSize {
				ws.sumSq += decodeFrameSumSq(data, i, format.BitDepth, numChannels, maxVal)
				ws.count++
				currentFrame++

				if ws.count >= windowFrames {
					processWindow(ws, ss, currentFrame, threshold, minSilenceFrames, format.SampleRate, &segments)
				}
			}
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}

	// Process remaining window
	if ws.count > 0 {
		processWindow(ws, ss, currentFrame, threshold, minSilenceFrames, format.SampleRate, &segments)
	}

	// Handle trailing silence
	if ss.active {
		emitSilenceSegment(ss, currentFrame, minSilenceFrames, format.SampleRate, &segments)
	}

	return buildSilenceResult(segments, currentFrame, format.SampleRate), nil
}

// buildSilenceResult constructs the final SilenceResult from accumulated segments.
func buildSilenceResult(segments []types.SilenceSegment, currentFrame uint64, sampleRate int) *types.SilenceResult {
	var totalSilence float64
	for _, seg := range segments {
		totalSilence += seg.DurationSec
	}

	var leadingSec, trailingSec float64

	totalDuration := float64(currentFrame) / float64(sampleRate)

	if len(segments) > 0 {
		if segments[0].StartSample == 0 {
			leadingSec = segments[0].DurationSec
		}

		last := segments[len(segments)-1]
		if last.EndSample == currentFrame {
			trailingSec = last.DurationSec
		}
	}

	return &types.SilenceResult{
		Segments:      segments,
		TotalSilence:  totalSilence,
		LeadingSec:    leadingSec,
		TrailingSec:   trailingSec,
		TotalDuration: totalDuration,
		Frames:        currentFrame,
	}
}
