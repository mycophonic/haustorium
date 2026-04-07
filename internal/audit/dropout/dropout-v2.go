package dropout

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

// scannerV2 adds cross-channel correlation to filter out intentional transients.
type scannerV2 struct {
	scanner

	// Per-frame delta candidates (not yet committed as events).
	deltaCandidates []deltaCandidate
}

type deltaCandidate struct {
	channel int
	prev    float64
	cur     float64
	delta   float64
	frame   uint64
}

func newScannerV2(opts Options, sampleRate float64, numChannels int) *scannerV2 {
	return &scannerV2{
		scanner:         *newScanner(opts, sampleRate, numChannels),
		deltaCandidates: make([]deltaCandidate, 0, numChannels),
	}
}

// processSampleV2 runs detection but defers delta events for cross-channel check.
func (s *scannerV2) processSampleV2(channel int, sample float64) {
	if !s.firstSample {
		// Delta detection - store as candidate, don't emit yet.
		delta := math.Abs(sample - s.prevSample[channel])
		if delta > s.opts.DeltaThreshold &&
			isDeltaDropout(s.prevSample[channel], sample, s.opts.DeltaNearZero) {
			s.deltaCandidates = append(s.deltaCandidates, deltaCandidate{
				channel: channel,
				prev:    s.prevSample[channel],
				cur:     sample,
				delta:   delta,
				frame:   s.totalFrames,
			})
		}

		// Zero run detection - same as original.
		s.checkZeroRun(channel, sample)
	}

	s.updateDCOffset(channel, sample)
	s.updateRMS(channel, sample)

	s.prevSample[channel] = sample
}

// endFrameV2 processes delta candidates with cross-channel correlation.
func (s *scannerV2) endFrameV2(numChannels int) {
	if len(s.deltaCandidates) > 0 {
		s.processDeltas(numChannels)
		s.deltaCandidates = s.deltaCandidates[:0] // reset for next frame
	}

	s.totalFrames++
	s.firstSample = false
}

// processDeltas checks if delta candidates are correlated across channels.
// If multiple channels have similar deltas at the same frame, it's likely
// intentional (music), not a dropout.
func (s *scannerV2) processDeltas(numChannels int) {
	candidates := s.deltaCandidates

	// Single channel: probably a real dropout.
	if len(candidates) == 1 {
		candidate := candidates[0]
		s.result.Events = append(s.result.Events, types.Event{
			Frame:    candidate.frame,
			TimeSec:  float64(candidate.frame) / s.sampleRate,
			Channel:  candidate.channel,
			Type:     types.EventDelta,
			Severity: candidate.delta,
		})
		s.result.DeltaCount++

		return
	}

	// Multiple channels: check correlation.
	// For stereo, if both channels jump in the same direction with similar magnitude,
	// it's almost certainly intentional music.
	if numChannels == 2 && len(candidates) == 2 {
		candidate0, candidate1 := candidates[0], candidates[1]

		// Same direction? (both positive-going or both negative-going)
		dir0 := candidate0.cur - candidate0.prev
		dir1 := candidate1.cur - candidate1.prev
		sameDirection := (dir0 > 0) == (dir1 > 0)

		// Similar magnitude? (within 50% of each other)
		maxDelta := math.Max(candidate0.delta, candidate1.delta)
		minDelta := math.Min(candidate0.delta, candidate1.delta)
		similarMagnitude := minDelta > maxDelta*correlationThreshold

		if sameDirection && similarMagnitude {
			// Correlated transient across both channels = music, not dropout.
			return
		}
	}

	// For >2 channels or uncorrelated stereo: check how many channels are similar.
	// If majority are correlated, discard all. Otherwise emit the outliers.
	if numChannels > 2 {
		s.processMultiChannelDeltas(candidates, numChannels)

		return
	}

	// Uncorrelated: emit all as potential dropouts.
	s.emitDeltaCandidates(candidates)
}

// processMultiChannelDeltas handles delta correlation for more than 2 channels.
func (s *scannerV2) processMultiChannelDeltas(candidates []deltaCandidate, numChannels int) {
	// Group by direction.
	positive := make([]deltaCandidate, 0)
	negative := make([]deltaCandidate, 0)

	for _, c := range candidates {
		if c.cur-c.prev > 0 {
			positive = append(positive, c)
		} else {
			negative = append(negative, c)
		}
	}

	// If all or most go same direction, it's music.
	majorityThreshold := (numChannels + 1) / 2
	if len(positive) >= majorityThreshold || len(negative) >= majorityThreshold {
		// Emit only the outliers (minority direction).
		var outliers []deltaCandidate
		if len(positive) < len(negative) {
			outliers = positive
		} else if len(negative) < len(positive) {
			outliers = negative
		}
		// If equal split, no clear outlier - discard all as ambiguous music.

		s.emitDeltaCandidates(outliers)

		return
	}

	// Uncorrelated: emit all as potential dropouts.
	s.emitDeltaCandidates(candidates)
}

// emitDeltaCandidates appends delta events for the given candidates.
func (s *scannerV2) emitDeltaCandidates(candidates []deltaCandidate) {
	for _, candidate := range candidates {
		s.result.Events = append(s.result.Events, types.Event{
			Frame:    candidate.frame,
			TimeSec:  float64(candidate.frame) / s.sampleRate,
			Channel:  candidate.channel,
			Type:     types.EventDelta,
			Severity: candidate.delta,
		})
		s.result.DeltaCount++
	}
}

// finalizeV2 is identical to finalize but on scannerV2.
func (s *scannerV2) finalizeV2() *types.DropoutResult {
	return s.finalize()
}

// decodeFramesV2_16 decodes 16-bit PCM frames and feeds them to the V2 scanner.
func decodeFramesV2_16(data []byte, frameSize, numChannels int, maxVal float64, scan *scannerV2) {
	for i := 0; i < len(data); i += frameSize {
		for ch := range numChannels {
			sample := float64(
				int16(binary.LittleEndian.Uint16(data[i+ch*2:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			scan.processSampleV2(ch, sample)
		}

		scan.endFrameV2(numChannels)
	}
}

// decodeFramesV2_24 decodes 24-bit PCM frames and feeds them to the V2 scanner.
func decodeFramesV2_24(data []byte, frameSize, numChannels int, maxVal float64, scan *scannerV2) {
	for i := 0; i < len(data); i += frameSize {
		for channel := range numChannels {
			offset := i + channel*3

			raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<shift16
			if raw&shared.Mask24Sign != 0 {
				raw |= ^shared.Mask24Extend
			}

			sample := float64(raw) / maxVal
			scan.processSampleV2(channel, sample)
		}

		scan.endFrameV2(numChannels)
	}
}

// decodeFramesV2_32 decodes 32-bit PCM frames and feeds them to the V2 scanner.
func decodeFramesV2_32(data []byte, frameSize, numChannels int, maxVal float64, scan *scannerV2) {
	for i := 0; i < len(data); i += frameSize {
		for ch := range numChannels {
			sample := float64(
				int32(binary.LittleEndian.Uint32(data[i+ch*4:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			scan.processSampleV2(ch, sample)
		}

		scan.endFrameV2(numChannels)
	}
}

// DetectV2 scans PCM audio data for dropout events with cross-channel correlation.
func DetectV2(reader io.Reader, format types.PCMFormat, opts Options) (*types.DropoutResult, error) {
	applyDefaults(&opts)

	bytesPerSample := int(format.BitDepth / shared.Shift8) //nolint:gosec // bit depth is a small constant
	numChannels := int(format.Channels)                    //nolint:gosec // channel count is a small constant
	frameSize := bytesPerSample * numChannels
	sampleRate := float64(format.SampleRate)
	maxVal := resolveMaxVal(format.BitDepth)

	buf := make([]byte, frameSize*readBufFrames)
	scan := newScannerV2(opts, sampleRate, numChannels)

	decoder := buildDecoderV2(format.BitDepth, frameSize, numChannels, maxVal, scan)

	if err := readLoop(reader, buf, frameSize, decoder); err != nil {
		return nil, err
	}

	return scan.finalizeV2(), nil
}

// buildDecoderV2 returns a frameDecoder for the given bit depth using the V2 scanner.
func buildDecoderV2(depth types.BitDepth, frameSize, numChannels int, maxVal float64, scan *scannerV2) frameDecoder {
	switch depth {
	case types.Depth16:
		return func(data []byte) { decodeFramesV2_16(data, frameSize, numChannels, maxVal, scan) }
	case types.Depth24:
		return func(data []byte) { decodeFramesV2_24(data, frameSize, numChannels, maxVal, scan) }
	case types.Depth32:
		return func(data []byte) { decodeFramesV2_32(data, frameSize, numChannels, maxVal, scan) }
	default:
		return func([]byte) {}
	}
}
