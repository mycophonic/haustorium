package dropout

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

// Dropout-specific default option values.
const (
	defaultDeltaThreshold   = 0.6
	defaultMinDeltaNearZero = 0.01
	defaultZeroRunMinMs     = 1.0
	defaultZeroRunQuietDB   = -50.0
	defaultDCWindowMs       = 50.0
	defaultDCJumpThreshold  = 0.1

	// readBufFrames is the number of frames per read buffer.
	readBufFrames = 4096

	// correlationThreshold is the magnitude similarity ratio for stereo correlation.
	correlationThreshold = 0.5

	// shift16 is the bit shift for 24-bit high-byte decoding.
	shift16 = 16
)

// Options configures the dropout detector thresholds.
type Options struct {
	DeltaThreshold  float64 // normalized; default 0.6 (60% of full scale jump)
	DeltaNearZero   float64 // at least one side of a delta must be below this; default 0.01
	ZeroRunMinMs    float64 // minimum zero run to report; default 1.0ms
	ZeroRunQuietDB  float64 // RMS below this around a zero run = not a dropout; default -50
	DCWindowMs      float64 // window for DC average; default 50ms
	DCJumpThreshold float64 // DC change threshold; default 0.1
}

// DefaultOptions returns the default dropout detection options.
func DefaultOptions() Options {
	return Options{
		DeltaThreshold:  defaultDeltaThreshold,
		DeltaNearZero:   defaultMinDeltaNearZero,
		ZeroRunMinMs:    defaultZeroRunMinMs,
		ZeroRunQuietDB:  defaultZeroRunQuietDB,
		DCWindowMs:      defaultDCWindowMs,
		DCJumpThreshold: defaultDCJumpThreshold,
	}
}

// scanner holds all per-channel state for the dropout detector.
type scanner struct {
	opts           Options
	sampleRate     float64
	dcWindowSize   int
	minZeroSamples int
	result         *types.DropoutResult
	totalFrames    uint64
	firstSample    bool

	// Per-channel state.
	prevSample    []float64
	zeroStart     []int64
	zeroStartRms  []float64
	dcBuf         [][]float64
	dcPos         []int
	dcSum         []float64
	dcFilled      []int
	prevDC        []float64
	dcInitialized []bool
	sqBuf         [][]float64
	sqPos         []int
	sqSum         []float64
	sqFilled      []int
}

func newScanner(opts Options, sampleRate float64, numChannels int) *scanner {
	dcWindowSize := max(int(sampleRate*opts.DCWindowMs/shared.MsPerSec), 1)
	minZeroSamples := max(int(sampleRate*opts.ZeroRunMinMs/shared.MsPerSec), 1)

	scan := &scanner{
		opts:           opts,
		sampleRate:     sampleRate,
		dcWindowSize:   dcWindowSize,
		minZeroSamples: minZeroSamples,
		result:         &types.DropoutResult{},
		firstSample:    true,

		prevSample:    make([]float64, numChannels),
		zeroStart:     make([]int64, numChannels),
		zeroStartRms:  make([]float64, numChannels),
		dcBuf:         make([][]float64, numChannels),
		dcPos:         make([]int, numChannels),
		dcSum:         make([]float64, numChannels),
		dcFilled:      make([]int, numChannels),
		prevDC:        make([]float64, numChannels),
		dcInitialized: make([]bool, numChannels),
		sqBuf:         make([][]float64, numChannels),
		sqPos:         make([]int, numChannels),
		sqSum:         make([]float64, numChannels),
		sqFilled:      make([]int, numChannels),
	}

	for i := range scan.zeroStart {
		scan.zeroStart[i] = -1
	}

	for ch := range numChannels {
		scan.dcBuf[ch] = make([]float64, dcWindowSize)
		scan.sqBuf[ch] = make([]float64, dcWindowSize)
	}

	return scan
}

// processSample runs all detection logic for a single sample on a single channel.
func (s *scanner) processSample(channel int, sample float64) {
	if !s.firstSample {
		// Delta detection.
		delta := math.Abs(sample - s.prevSample[channel])
		if delta > s.opts.DeltaThreshold &&
			isDeltaDropout(s.prevSample[channel], sample, s.opts.DeltaNearZero) {
			s.result.Events = append(s.result.Events, types.Event{
				Frame:    s.totalFrames,
				TimeSec:  float64(s.totalFrames) / s.sampleRate,
				Channel:  channel,
				Type:     types.EventDelta,
				Severity: delta,
			})
			s.result.DeltaCount++
		}

		// Zero run detection.
		s.checkZeroRun(channel, sample)
	}

	s.updateDCOffset(channel, sample)
	s.updateRMS(channel, sample)

	s.prevSample[channel] = sample
}

// checkZeroRun detects zero-sample runs and emits events when they end.
func (s *scanner) checkZeroRun(channel int, sample float64) {
	if sample == 0 {
		if s.zeroStart[channel] < 0 {
			s.zeroStart[channel] = int64(s.totalFrames) //nolint:gosec // frame count fits in int64
			s.zeroStartRms[channel] = rmsDB(s.sqSum[channel], s.sqFilled[channel])
		}
	} else if s.zeroStart[channel] >= 0 {
		s.emitZeroRun(channel)
	}
}

// emitZeroRun checks and emits a zero-run event for the given channel, then resets the state.
func (s *scanner) emitZeroRun(channel int) {
	runLength := int64(s.totalFrames) - s.zeroStart[channel] //nolint:gosec // frame count fits in int64
	if runLength >= int64(s.minZeroSamples) && s.zeroStartRms[channel] >= s.opts.ZeroRunQuietDB {
		durationMs := float64(runLength) / s.sampleRate * shared.MsPerSec
		s.result.Events = append(s.result.Events, types.Event{
			Frame:      uint64(s.zeroStart[channel]), //nolint:gosec // value is non-negative by construction
			TimeSec:    float64(s.zeroStart[channel]) / s.sampleRate,
			Channel:    channel,
			Type:       types.EventZeroRun,
			Severity:   float64(runLength) / s.sampleRate,
			DurationMs: durationMs,
		})
		s.result.ZeroRunCount++
	}

	s.zeroStart[channel] = -1
}

// updateDCOffset tracks the running DC offset for a channel.
func (s *scanner) updateDCOffset(channel int, sample float64) {
	old := s.dcBuf[channel][s.dcPos[channel]]
	s.dcBuf[channel][s.dcPos[channel]] = sample
	s.dcSum[channel] = s.dcSum[channel] - old + sample

	s.dcPos[channel] = (s.dcPos[channel] + 1) % s.dcWindowSize
	if s.dcFilled[channel] < s.dcWindowSize {
		s.dcFilled[channel]++
	}

	if s.dcFilled[channel] == s.dcWindowSize {
		currentDC := s.dcSum[channel] / float64(s.dcWindowSize)
		if s.dcInitialized[channel] {
			dcDelta := math.Abs(currentDC - s.prevDC[channel])
			if dcDelta > s.opts.DCJumpThreshold {
				s.result.Events = append(s.result.Events, types.Event{
					Frame:    s.totalFrames,
					TimeSec:  float64(s.totalFrames) / s.sampleRate,
					Channel:  channel,
					Type:     types.EventDCJump,
					Severity: dcDelta,
				})
				s.result.DCJumpCount++
			}
		}

		s.prevDC[channel] = currentDC
		s.dcInitialized[channel] = true
	}
}

// updateRMS tracks the running sum-of-squares for RMS calculation.
func (s *scanner) updateRMS(channel int, sample float64) {
	oldSq := s.sqBuf[channel][s.sqPos[channel]]
	sq := sample * sample
	s.sqBuf[channel][s.sqPos[channel]] = sq
	s.sqSum[channel] = s.sqSum[channel] - oldSq + sq

	s.sqPos[channel] = (s.sqPos[channel] + 1) % s.dcWindowSize
	if s.sqFilled[channel] < s.dcWindowSize {
		s.sqFilled[channel]++
	}
}

// endFrame advances the frame counter and clears the first-sample flag.
func (s *scanner) endFrame() {
	s.totalFrames++
	s.firstSample = false
}

// flush emits any trailing zero runs still open at EOF.
func (s *scanner) flush() {
	for channel := range s.zeroStart {
		if s.zeroStart[channel] >= 0 {
			runLength := int64(s.totalFrames) - s.zeroStart[channel] //nolint:gosec // frame count fits in int64
			if runLength >= int64(s.minZeroSamples) && s.zeroStartRms[channel] >= s.opts.ZeroRunQuietDB {
				durationMs := float64(runLength) / s.sampleRate * shared.MsPerSec
				s.result.Events = append(s.result.Events, types.Event{
					Frame:      uint64(s.zeroStart[channel]),
					TimeSec:    float64(s.zeroStart[channel]) / s.sampleRate,
					Channel:    channel,
					Type:       types.EventZeroRun,
					Severity:   float64(runLength) / s.sampleRate,
					DurationMs: durationMs,
				})
				s.result.ZeroRunCount++
			}
		}
	}
}

// finalize computes the worst severity and sets the frame count on the result.
func (s *scanner) finalize() *types.DropoutResult {
	s.flush()

	var worstSeverity float64

	for _, e := range s.result.Events {
		if e.Type == types.EventDelta || e.Type == types.EventDCJump {
			if e.Severity > worstSeverity {
				worstSeverity = e.Severity
			}
		}
	}

	if worstSeverity > 0 {
		s.result.WorstDB = shared.DBMultiplier * math.Log10(worstSeverity)
	} else {
		s.result.WorstDB = shared.SilenceFloorDB
	}

	s.result.Frames = s.totalFrames

	return s.result
}

// rmsDB returns the current RMS level in dB from a running sum-of-squares.
func rmsDB(sqSum float64, sqFilled int) float64 {
	if sqFilled == 0 {
		return shared.SilenceFloorDB
	}

	rms := math.Sqrt(sqSum / float64(sqFilled))
	if rms > 0 {
		return shared.DBMultiplier * math.Log10(rms)
	}

	return shared.SilenceFloorDB
}

// isDeltaDropout returns true if a sample-to-sample jump looks like a real
// dropout rather than a normal musical transient. A dropout transitions
// between audible content and near-silence, so at least one of the two
// samples flanking the jump must be near zero.
func isDeltaDropout(prev, cur, nearZero float64) bool {
	return math.Abs(prev) < nearZero || math.Abs(cur) < nearZero
}

// applyDefaults fills in zero-valued option fields with their defaults.
func applyDefaults(opts *Options) {
	if opts.DeltaThreshold == 0 {
		opts.DeltaThreshold = defaultDeltaThreshold
	}

	if opts.DeltaNearZero == 0 {
		opts.DeltaNearZero = defaultMinDeltaNearZero
	}

	if opts.ZeroRunMinMs == 0 {
		opts.ZeroRunMinMs = defaultZeroRunMinMs
	}

	if opts.ZeroRunQuietDB == 0 {
		opts.ZeroRunQuietDB = defaultZeroRunQuietDB
	}

	if opts.DCWindowMs == 0 {
		opts.DCWindowMs = defaultDCWindowMs
	}

	if opts.DCJumpThreshold == 0 {
		opts.DCJumpThreshold = defaultDCJumpThreshold
	}
}

// resolveMaxVal returns the normalization divisor for the given bit depth.
func resolveMaxVal(depth types.BitDepth) float64 {
	switch depth {
	case types.Depth16:
		return shared.MaxValue16
	case types.Depth24:
		return shared.MaxValue24
	case types.Depth32:
		return shared.MaxValue32
	default:
		return 0
	}
}

// frameDecoder is a callback that decodes and processes one buffer of PCM frames.
type frameDecoder func(data []byte)

// readLoop reads PCM data from r in chunks and calls decode for each complete-frame buffer.
func readLoop(r io.Reader, buf []byte, frameSize int, decode frameDecoder) error {
	for {
		n, err := r.Read(buf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			decode(buf[:completeFrames])
		}

		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}
}

// decodeFrames16 decodes 16-bit PCM frames and feeds them to the scanner.
func decodeFrames16(data []byte, frameSize, numChannels int, maxVal float64, scan *scanner) {
	for i := 0; i < len(data); i += frameSize {
		for ch := range numChannels {
			sample := float64(
				int16(binary.LittleEndian.Uint16(data[i+ch*2:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			scan.processSample(ch, sample)
		}

		scan.endFrame()
	}
}

// decodeFrames24 decodes 24-bit PCM frames and feeds them to the scanner.
func decodeFrames24(data []byte, frameSize, numChannels int, maxVal float64, scan *scanner) {
	for i := 0; i < len(data); i += frameSize {
		for channel := range numChannels {
			offset := i + channel*3

			raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<shift16
			if raw&shared.Mask24Sign != 0 {
				raw |= ^shared.Mask24Extend
			}

			sample := float64(raw) / maxVal
			scan.processSample(channel, sample)
		}

		scan.endFrame()
	}
}

// decodeFrames32 decodes 32-bit PCM frames and feeds them to the scanner.
func decodeFrames32(data []byte, frameSize, numChannels int, maxVal float64, scan *scanner) {
	for i := 0; i < len(data); i += frameSize {
		for ch := range numChannels {
			sample := float64(
				int32(binary.LittleEndian.Uint32(data[i+ch*4:])), //nolint:gosec // PCM sample conversion
			) / maxVal
			scan.processSample(ch, sample)
		}

		scan.endFrame()
	}
}

// Detect scans PCM audio data for dropout events.
func Detect(reader io.Reader, format types.PCMFormat, opts Options) (*types.DropoutResult, error) {
	applyDefaults(&opts)

	bytesPerSample := int(format.BitDepth / shared.Shift8) //nolint:gosec // bit depth is a small constant
	numChannels := int(format.Channels)                    //nolint:gosec // channel count is a small constant
	frameSize := bytesPerSample * numChannels
	sampleRate := float64(format.SampleRate)
	maxVal := resolveMaxVal(format.BitDepth)

	buf := make([]byte, frameSize*readBufFrames)
	scan := newScanner(opts, sampleRate, numChannels)

	decoder := buildDecoder(format.BitDepth, frameSize, numChannels, maxVal, scan)

	if err := readLoop(reader, buf, frameSize, decoder); err != nil {
		return nil, err
	}

	return scan.finalize(), nil
}

// buildDecoder returns a frameDecoder for the given bit depth using the V1 scanner.
func buildDecoder(depth types.BitDepth, frameSize, numChannels int, maxVal float64, scan *scanner) frameDecoder {
	switch depth {
	case types.Depth16:
		return func(data []byte) { decodeFrames16(data, frameSize, numChannels, maxVal, scan) }
	case types.Depth24:
		return func(data []byte) { decodeFrames24(data, frameSize, numChannels, maxVal, scan) }
	case types.Depth32:
		return func(data []byte) { decodeFrames32(data, frameSize, numChannels, maxVal, scan) }
	default:
		return func([]byte) {}
	}
}
