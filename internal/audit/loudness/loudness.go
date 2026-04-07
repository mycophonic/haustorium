// Package loudness implements EBU R128 loudness measurement and dynamic range analysis.
package loudness

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

// EBU R128 and K-weighting constants.
const (
	// K-weighting pre-filter (high shelf) parameters.
	preFilterCenterFreq = 1681.974450955533
	preFilterGainDb     = 3.999843853973347
	preFilterQ          = 0.7071752369554196
	preFilterVbExponent = 0.4996667741545416

	// K-weighting RLB (high pass) parameters.
	rlbCenterFreq = 38.13547087602444
	rlbQ          = 0.5003270373238773

	// Surround channel weight for Ls/Rs (~+1.5 dB).
	surroundWeight = 1.41

	// EBU R128 gating thresholds.
	absoluteGateLUFS   = -70
	relativeGateOffset = 10
	lraRelativeGate    = 20

	// EBU R128 LRA percentiles.
	lraLowPercentile  = 0.10
	lraHighPercentile = 0.95

	// Momentary window: 400 ms.
	momentaryWindowMs = 400
	// Short-term window: 3 seconds.
	shortTermWindowSec = 3
	// Hop size: 100 ms.
	hopMs = 100

	// DR calculation: top 20% of RMS values (1/5).
	drTop20Divisor = 5

	// DR score clamping range.
	drMinScore = 1
	drMaxScore = 20

	// dbDivisor is the power-to-dB conversion factor: 10 * log10(power).
	dbDivisor = 10

	// Surround channel index range.
	surroundChStart = 3
	surroundChEnd   = 4
	surroundMinCh   = 4
)

// drResult holds the output of dynamic range calculation.
type drResult struct {
	score  int
	value  float64
	peakDB float64
	rmsDB  float64
}

// Biquad filter coefficients.
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
}

// Biquad filter state.
type biquadState struct {
	z1, z2 float64
}

func (s *biquadState) process(b *biquad, in float64) float64 {
	out := b.b0*in + s.z1
	s.z1 = b.b1*in - b.a1*out + s.z2
	s.z2 = b.b2*in - b.a2*out

	return out
}

// K-weighting filter coefficients for common sample rates
// Pre-filter (high shelf) + RLB weighting (high pass).
func getKWeightingFilters(rate int) (pre, rlb biquad) {
	// Coefficients from ITU-R BS.1770-4
	// These are computed from the analog prototype transfer functions
	sampleRate := float64(rate)

	// Pre-filter (high shelf)
	// Models the acoustic effects of the head
	bilinearK := math.Tan(math.Pi * preFilterCenterFreq / sampleRate)
	headGainV := math.Pow(10, preFilterGainDb/float64(shared.DBMultiplier))
	vb := math.Pow(headGainV, preFilterVbExponent)

	gain := 1 + bilinearK/preFilterQ + bilinearK*bilinearK
	pre.b0 = (headGainV + vb*bilinearK/preFilterQ + bilinearK*bilinearK) / gain
	pre.b1 = 2 * (bilinearK*bilinearK - headGainV) / gain
	pre.b2 = (headGainV - vb*bilinearK/preFilterQ + bilinearK*bilinearK) / gain
	pre.a1 = 2 * (bilinearK*bilinearK - 1) / gain
	pre.a2 = (1 - bilinearK/preFilterQ + bilinearK*bilinearK) / gain

	// RLB weighting (high pass)
	bilinearK = math.Tan(math.Pi * rlbCenterFreq / sampleRate)

	gain = 1 + bilinearK/rlbQ + bilinearK*bilinearK
	rlb.b0 = 1 / gain
	rlb.b1 = -2 / gain
	rlb.b2 = 1 / gain
	rlb.a1 = 2 * (bilinearK*bilinearK - 1) / gain
	rlb.a2 = (1 - bilinearK/rlbQ + bilinearK*bilinearK) / gain

	return pre, rlb
}

// Channel weights for surround (we only handle stereo for now).
func getChannelWeight(channel, numChannels int) float64 {
	if numChannels <= 2 {
		return shared.FullScale
	}
	// For surround: L, R, C = 1.0; Ls, Rs = 1.41 (~+1.5dB)
	// LFE is excluded
	if channel >= surroundChStart && channel <= surroundChEnd && numChannels > surroundMinCh {
		return surroundWeight
	}

	return shared.FullScale
}

// drBlock holds peak and RMS for a 3-second analysis block.
type drBlock struct {
	peak float64
	rms  float64
}

// meter holds all state for the loudness/DR measurement.
type meter struct {
	numChannels int
	sampleRate  int
	pre, rlb    biquad
	preState    []biquadState
	rlbState    []biquadState

	// Window sizes in samples.
	momentarySize int
	shortTermSize int
	blockSize     int
	hopSize       int

	// Ring buffers for windowed measurements.
	momentaryBuf    []float64
	shortTermBuf    []float64
	momentaryPos    int
	shortTermPos    int
	momentarySum    float64
	shortTermSum    float64
	momentaryFilled int
	shortTermFilled int

	// DR calculation: 3s blocks.
	drBlocks     []drBlock
	blockSum     float64
	blockPeak    float64
	blockSamples int

	// Loudness windows.
	momentaryPowers []float64
	shortTermPowers []float64
	momentaryMax    float64
	shortTermMax    float64

	// Counters.
	sampleCount int
	totalFrames uint64

	// Per-frame scratch buffer (reused, avoids allocation).
	frameSamples []float64
}

func newMeter(sampleRate, numChannels int) *meter {
	pre, rlb := getKWeightingFilters(sampleRate)

	momentarySize := sampleRate * momentaryWindowMs / shared.MsPerSec
	shortTermSize := sampleRate * shortTermWindowSec

	return &meter{
		numChannels:   numChannels,
		sampleRate:    sampleRate,
		pre:           pre,
		rlb:           rlb,
		preState:      make([]biquadState, numChannels),
		rlbState:      make([]biquadState, numChannels),
		momentarySize: momentarySize,
		shortTermSize: shortTermSize,
		blockSize:     sampleRate * shortTermWindowSec,
		hopSize:       sampleRate * hopMs / shared.MsPerSec,
		momentaryBuf:  make([]float64, momentarySize),
		shortTermBuf:  make([]float64, shortTermSize),
		momentaryMax:  shared.SilenceFloorDB,
		shortTermMax:  shared.SilenceFloorDB,
		frameSamples:  make([]float64, numChannels),
	}
}

// processFrame applies K-weighting, accumulates loudness and DR data for one frame.
// The caller must fill m.frameSamples before calling this.
func (m *meter) processFrame() {
	var framePower, framePeak float64

	for channel, sample := range m.frameSamples {
		if abs := math.Abs(sample); abs > framePeak {
			framePeak = abs
		}

		filtered := m.preState[channel].process(&m.pre, sample)
		filtered = m.rlbState[channel].process(&m.rlb, filtered)

		weight := getChannelWeight(channel, m.numChannels)
		framePower += weight * filtered * filtered
	}

	m.updateDRBlock(framePower, framePeak)
	m.updateWindows(framePower)
}

// updateDRBlock accumulates power and peak into the current DR block.
func (m *meter) updateDRBlock(framePower, framePeak float64) {
	m.blockSum += framePower / float64(m.numChannels)

	if framePeak > m.blockPeak {
		m.blockPeak = framePeak
	}

	m.blockSamples++

	if m.blockSamples >= m.blockSize {
		rms := math.Sqrt(m.blockSum / float64(m.blockSamples))
		m.drBlocks = append(m.drBlocks, drBlock{m.blockPeak, rms})
		m.blockSum = 0
		m.blockPeak = 0
		m.blockSamples = 0
	}
}

// updateWindows updates the momentary and short-term ring buffers and computes windowed loudness.
func (m *meter) updateWindows(framePower float64) {
	// Update momentary window (ring buffer).
	old := m.momentaryBuf[m.momentaryPos]
	m.momentaryBuf[m.momentaryPos] = framePower
	m.momentarySum = m.momentarySum - old + framePower

	m.momentaryPos = (m.momentaryPos + 1) % m.momentarySize
	if m.momentaryFilled < m.momentarySize {
		m.momentaryFilled++
	}

	// Update short-term window (ring buffer).
	old = m.shortTermBuf[m.shortTermPos]
	m.shortTermBuf[m.shortTermPos] = framePower
	m.shortTermSum = m.shortTermSum - old + framePower

	m.shortTermPos = (m.shortTermPos + 1) % m.shortTermSize
	if m.shortTermFilled < m.shortTermSize {
		m.shortTermFilled++
	}

	m.sampleCount++
	m.totalFrames++

	// Every hop, calculate windowed loudness.
	if m.sampleCount%m.hopSize == 0 {
		m.recordWindowedLoudness()
	}
}

// recordWindowedLoudness computes and records momentary/short-term loudness at each hop.
func (m *meter) recordWindowedLoudness() {
	if m.momentaryFilled == m.momentarySize {
		momentaryLoudness := -shared.LufsOffset + dbDivisor*math.Log10(m.momentarySum/float64(m.momentarySize))
		m.momentaryPowers = append(m.momentaryPowers, m.momentarySum/float64(m.momentarySize))

		if momentaryLoudness > m.momentaryMax {
			m.momentaryMax = momentaryLoudness
		}
	}

	if m.shortTermFilled == m.shortTermSize {
		shortTermLoudness := -shared.LufsOffset + dbDivisor*math.Log10(m.shortTermSum/float64(m.shortTermSize))
		m.shortTermPowers = append(m.shortTermPowers, m.shortTermSum/float64(m.shortTermSize))

		if shortTermLoudness > m.shortTermMax {
			m.shortTermMax = shortTermLoudness
		}
	}
}

// finalize handles the final partial DR block and computes all results.
func (m *meter) finalize() *types.LoudnessResult {
	// Handle final partial DR block.
	if m.blockSamples > m.sampleRate { // at least 1 second
		rms := math.Sqrt(m.blockSum / float64(m.blockSamples))
		m.drBlocks = append(m.drBlocks, drBlock{m.blockPeak, rms})
	}

	integratedLUFS := calculateIntegratedLoudness(m.momentaryPowers)
	lra := calculateLoudnessRange(m.shortTermPowers)
	dr := calculateDR(m.drBlocks)

	return &types.LoudnessResult{
		IntegratedLUFS: integratedLUFS,
		ShortTermMax:   m.shortTermMax,
		MomentaryMax:   m.momentaryMax,
		LoudnessRange:  lra,
		DRScore:        dr.score,
		DRValue:        dr.value,
		PeakDB:         dr.peakDB,
		RmsDB:          dr.rmsDB,
		Frames:         m.totalFrames,
	}
}

// Analyze performs EBU R128 loudness measurement on PCM audio data.
func Analyze(reader io.Reader, format types.PCMFormat) (*types.LoudnessResult, error) {
	bytesPerSample := int(format.BitDepth / 8) //nolint:gosec // bit depth and channel count are small constants
	numChannels := int(format.Channels)        //nolint:gosec // bit depth and channel count are small constants
	frameSize := bytesPerSample * numChannels
	sampleRate := format.SampleRate

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

	measurement := newMeter(sampleRate, numChannels)

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			data := buf[:completeFrames]

			decodeLoudnessFrames(data, format.BitDepth, frameSize, numChannels, maxVal, measurement)
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}

	return measurement.finalize(), nil
}

// decodeLoudnessFrames decodes PCM frames and feeds them to the meter.
func decodeLoudnessFrames(data []byte, bitDepth types.BitDepth, frameSize, numChannels int, maxVal float64, m *meter) {
	switch bitDepth {
	case types.Depth16:
		for i := 0; i < len(data); i += frameSize {
			for ch := range numChannels {
				m.frameSamples[ch] = float64(
					int16(binary.LittleEndian.Uint16(data[i+ch*2:])), //nolint:gosec // PCM sample conversion
				) / maxVal
			}

			m.processFrame()
		}
	case types.Depth24:
		for i := 0; i < len(data); i += frameSize {
			for channel := range numChannels {
				offset := i + channel*3

				raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<16
				if raw&shared.Mask24Sign != 0 {
					raw |= ^shared.Mask24Extend
				}

				m.frameSamples[channel] = float64(raw) / maxVal
			}

			m.processFrame()
		}
	case types.Depth32:
		for i := 0; i < len(data); i += frameSize {
			for ch := range numChannels {
				m.frameSamples[ch] = float64(
					int32(binary.LittleEndian.Uint32(data[i+ch*4:])), //nolint:gosec // PCM sample conversion
				) / maxVal
			}

			m.processFrame()
		}
	default:
	}
}

func calculateIntegratedLoudness(powers []float64) float64 {
	if len(powers) == 0 {
		return shared.SilenceFloorDB
	}

	// First pass: absolute gate at -70 LUFS
	var (
		sum   float64
		count int
	)

	for _, p := range powers {
		lufs := -shared.LufsOffset + dbDivisor*math.Log10(p)
		if lufs > absoluteGateLUFS {
			sum += p
			count++
		}
	}

	if count == 0 {
		return shared.SilenceFloorDB
	}

	// Relative threshold: -10 LU below ungated mean
	ungatedMean := sum / float64(count)
	relativeThreshold := -shared.LufsOffset + dbDivisor*math.Log10(ungatedMean) - relativeGateOffset

	// Second pass: relative gate
	sum = 0
	count = 0

	for _, p := range powers {
		lufs := -shared.LufsOffset + dbDivisor*math.Log10(p)
		if lufs > relativeThreshold {
			sum += p
			count++
		}
	}

	if count == 0 {
		return shared.SilenceFloorDB
	}

	return -shared.LufsOffset + dbDivisor*math.Log10(sum/float64(count))
}

func calculateLoudnessRange(powers []float64) float64 {
	if len(powers) < 2 {
		return 0
	}

	// Convert to LUFS and filter by absolute gate
	var lufsValues []float64

	for _, p := range powers {
		lufs := -shared.LufsOffset + dbDivisor*math.Log10(p)
		if lufs > absoluteGateLUFS {
			lufsValues = append(lufsValues, lufs)
		}
	}

	if len(lufsValues) < 2 {
		return 0
	}

	// Relative gate at -20 LU below ungated mean
	var sum float64
	for _, l := range lufsValues {
		sum += l
	}

	mean := sum / float64(len(lufsValues))
	relativeThreshold := mean - lraRelativeGate

	var gated []float64

	for _, l := range lufsValues {
		if l > relativeThreshold {
			gated = append(gated, l)
		}
	}

	if len(gated) < 2 {
		return 0
	}

	// LRA = difference between 95th and 10th percentile
	slices.Sort(gated)
	low := gated[int(float64(len(gated))*lraLowPercentile)]
	high := gated[int(float64(len(gated))*lraHighPercentile)]

	return high - low
}

func calculateDR(blocks []drBlock) drResult {
	if len(blocks) == 0 {
		return drResult{0, 0, shared.SilenceFloorDB, shared.SilenceFloorDB}
	}

	// Sort blocks by peak (descending)
	peaksSorted := make([]float64, len(blocks))
	for i, b := range blocks {
		peaksSorted[i] = b.peak
	}

	slices.SortFunc(peaksSorted, func(a, b float64) int {
		if a > b {
			return -1
		}

		if a < b {
			return 1
		}

		return 0
	})

	// Use second-highest peak (avoid outliers)
	peakIdx := 1
	if len(peaksSorted) == 1 {
		peakIdx = 0
	}

	peak := peaksSorted[peakIdx]

	// Sort blocks by RMS (descending)
	rmsSorted := make([]float64, len(blocks))
	for i, b := range blocks {
		rmsSorted[i] = b.rms
	}

	slices.SortFunc(rmsSorted, func(a, b float64) int {
		if a > b {
			return -1
		}

		if a < b {
			return 1
		}

		return 0
	})

	// Average top 20% of RMS values
	top20Count := max(len(rmsSorted)/drTop20Divisor, 1)

	var rmsSum float64
	for i := range top20Count {
		rmsSum += rmsSorted[i]
	}

	rms := rmsSum / float64(top20Count)

	if rms == 0 {
		return drResult{0, 0, shared.SilenceFloorDB, shared.SilenceFloorDB}
	}

	// DR = 20 * log10(peak / rms)
	dynamicRange := float64(shared.DBMultiplier) * math.Log10(peak/rms)

	// Clamp to DR1-DR20
	score := min(max(int(math.Round(dynamicRange)), drMinScore), drMaxScore)

	peakDB := float64(shared.DBMultiplier) * math.Log10(peak)
	rmsDB := float64(shared.DBMultiplier) * math.Log10(rms)

	return drResult{score, dynamicRange, peakDB, rmsDB}
}
