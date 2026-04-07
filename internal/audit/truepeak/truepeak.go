// Package truepeak implements ITU-R BS.1770 true peak detection with 4x oversampling.
package truepeak

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

const (
	oversample   = 4  // 4x oversampling per ITU-R BS.1770
	tapsPerPhase = 12 // filter taps per phase
	totalTaps    = oversample * tapsPerPhase

	// ISP overshoot magnitude thresholds (dB above 0 dBFS).
	ispThresholdHalfdB = 0.5
	ispThreshold1dB    = 1.0
	ispThreshold2dB    = 2.0

	// Kaiser window parameter for polyphase filter generation.
	kaiserBeta = 5.0

	// Bessel function iteration limit.
	besselMaxIter = 25
)

// polyphaseCoeffsOnce ensures coefficients are computed exactly once.
var polyphaseCoeffsOnce sync.Once //nolint:gochecknoglobals // lazy-init guard for polyphase coefficients

// polyphaseCoeffsCache stores the computed polyphase filter coefficients.
var polyphaseCoeffsCache [oversample][tapsPerPhase]float64 //nolint:gochecknoglobals // cached polyphase coefficients

// getPolyphaseCoeffs returns the polyphase filter coefficients, computing them on first call.
func getPolyphaseCoeffs() *[oversample][tapsPerPhase]float64 {
	polyphaseCoeffsOnce.Do(func() {
		polyphaseCoeffsCache = computePolyphaseCoeffs()
	})

	return &polyphaseCoeffsCache
}

// computePolyphaseCoeffs generates polyphase filter coefficients for 4x oversampling.
// Lowpass at 0.25 normalized frequency (Nyquist of original signal) with Kaiser window.
func computePolyphaseCoeffs() [oversample][tapsPerPhase]float64 {
	var coeffs [oversample][tapsPerPhase]float64

	for phase := range oversample {
		for tap := range tapsPerPhase {
			count := tap*oversample + phase
			center := float64(totalTaps-1) / 2.0

			sample := float64(count) - center

			var sinc float64
			if math.Abs(sample) < 1e-10 {
				sinc = shared.FullScale
			} else {
				sinc = math.Sin(math.Pi*sample/float64(oversample)) / (math.Pi * sample / float64(oversample))
			}

			alpha := (float64(count) - center) / center
			if math.Abs(alpha) <= shared.FullScale {
				window := bessel0(kaiserBeta*math.Sqrt(1-alpha*alpha)) / bessel0(kaiserBeta)
				coeffs[phase][tap] = sinc * window * float64(oversample)
			}
		}
	}

	for phase := range oversample {
		var sum float64
		for tap := range tapsPerPhase {
			sum += coeffs[phase][tap]
		}

		for tap := range tapsPerPhase {
			coeffs[phase][tap] /= sum
		}
	}

	return coeffs
}

// Bessel function I0 (modified Bessel function of the first kind, order 0).
func bessel0(x float64) float64 {
	sum := shared.FullScale

	term := shared.FullScale
	for k := 1; k <= besselMaxIter; k++ {
		term *= (x * x) / (4.0 * float64(k) * float64(k))

		sum += term
		if term < 1e-12 {
			break
		}
	}

	return sum
}

// ispTracker accumulates inter-sample peak statistics.
type ispTracker struct {
	count          uint64
	maxDB          float64
	aboveHalfdB    uint64
	above1dB       uint64
	above2dB       uint64
	currentWindowN uint64
	windowCounts   []uint64
	windowStart    uint64
	samplesPerSec  uint64
}

// trackISP records a single inter-sample peak event.
func (t *ispTracker) trackISP(absInterp float64) {
	t.count++
	t.currentWindowN++

	overshoot := float64(shared.DBMultiplier) * math.Log10(absInterp)
	if overshoot > t.maxDB {
		t.maxDB = overshoot
	}

	if overshoot > ispThresholdHalfdB {
		t.aboveHalfdB++
	}

	if overshoot > ispThreshold1dB {
		t.above1dB++
	}

	if overshoot > ispThreshold2dB {
		t.above2dB++
	}
}

// advanceFrame advances the frame counter and rolls over 1-second windows.
func (t *ispTracker) advanceFrame(totalFrames uint64) {
	if totalFrames-t.windowStart >= t.samplesPerSec {
		t.windowCounts = append(t.windowCounts, t.currentWindowN)
		t.currentWindowN = 0
		t.windowStart = totalFrames
	}
}

// processSample handles one sample: updates sample peak, history, runs polyphase interpolation,
// and tracks ISPs. It returns the updated samplePeak and truePeak.
func processSample(
	sample float64,
	history []float64,
	coeffs *[oversample][tapsPerPhase]float64,
	samplePeak, truePeak float64,
	isp *ispTracker,
) (float64, float64) {
	absSample := math.Abs(sample)
	if absSample > samplePeak {
		samplePeak = absSample
	}

	copy(history[0:], history[1:])
	history[tapsPerPhase-1] = sample

	for phase := range oversample {
		var interp float64
		for tap := range tapsPerPhase {
			interp += history[tap] * coeffs[phase][tap]
		}

		absInterp := math.Abs(interp)
		if absInterp > truePeak {
			truePeak = absInterp
		}

		if absInterp > shared.FullScale {
			isp.trackISP(absInterp)
		}
	}

	return samplePeak, truePeak
}

// decode24BitSample decodes a 24-bit little-endian signed PCM sample from data at the given offset.
func decode24BitSample(data []byte, offset int) int32 {
	raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<16
	if raw&shared.Mask24Sign != 0 {
		raw |= ^shared.Mask24Extend
	}

	return raw
}

// Detect performs true peak detection on PCM audio using polyphase interpolation.
func Detect(r io.Reader, format types.PCMFormat) (*types.TruePeakResult, error) {
	coeffs := getPolyphaseCoeffs()

	bytesPerSample := int(format.BitDepth / 8) //nolint:gosec // bit depth and channel count are small constants
	numChannels := int(format.Channels)        //nolint:gosec // bit depth and channel count are small constants
	frameSize := bytesPerSample * numChannels

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

	history := make([][]float64, numChannels)
	for channel := range history {
		history[channel] = make([]float64, tapsPerPhase)
	}

	var (
		samplePeak  float64
		truePeak    float64
		totalFrames uint64
	)

	isp := &ispTracker{
		windowCounts:  []uint64{0},
		samplesPerSec: uint64(format.SampleRate), //nolint:gosec // sample rate is a small positive constant
	}

	for {
		n, err := r.Read(buf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			data := buf[:completeFrames]

			samplePeak, truePeak, totalFrames = processFrames(
				data, format.BitDepth, frameSize, numChannels, maxVal,
				history, coeffs, samplePeak, truePeak, totalFrames, isp,
			)
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}

	return buildResult(samplePeak, truePeak, totalFrames, isp), nil
}

// processFrames processes a buffer of complete PCM frames for all bit depths.
func processFrames(
	data []byte,
	bitDepth types.BitDepth,
	frameSize, numChannels int,
	maxVal float64,
	history [][]float64,
	coeffs *[oversample][tapsPerPhase]float64,
	samplePeak, truePeak float64,
	totalFrames uint64,
	isp *ispTracker,
) (float64, float64, uint64) {
	for i := 0; i < len(data); i += frameSize {
		for channel := range numChannels {
			sample := decodeSample(data, i, channel, bitDepth, maxVal)
			samplePeak, truePeak = processSample(sample, history[channel], coeffs, samplePeak, truePeak, isp)
		}

		totalFrames++
		isp.advanceFrame(totalFrames)
	}

	return samplePeak, truePeak, totalFrames
}

// decodeSample decodes a single PCM sample at the given frame offset and channel.
func decodeSample(data []byte, frameOffset, channel int, bitDepth types.BitDepth, maxVal float64) float64 {
	switch bitDepth {
	case types.Depth16:
		return float64(
			int16(binary.LittleEndian.Uint16(data[frameOffset+channel*2:])), //nolint:gosec // PCM sample conversion
		) / maxVal
	case types.Depth24:
		offset := frameOffset + channel*3

		return float64(decode24BitSample(data, offset)) / maxVal
	case types.Depth32:
		return float64(
			int32(binary.LittleEndian.Uint32(data[frameOffset+channel*4:])), //nolint:gosec // PCM sample conversion
		) / maxVal
	default:
		return 0
	}
}

// buildResult constructs the final TruePeakResult from accumulated measurements.
func buildResult(samplePeak, truePeak float64, totalFrames uint64, isp *ispTracker) *types.TruePeakResult {
	samplePeakDB := shared.SilenceFloorDB
	if samplePeak > 0 {
		samplePeakDB = float64(shared.DBMultiplier) * math.Log10(samplePeak)
	}

	truePeakDB := shared.SilenceFloorDB
	if truePeak > 0 {
		truePeakDB = float64(shared.DBMultiplier) * math.Log10(truePeak)
	}

	if isp.currentWindowN > 0 {
		isp.windowCounts = append(isp.windowCounts, isp.currentWindowN)
	}

	var (
		ispDensityPeak  float64
		worstDensitySec float64
	)

	for i, count := range isp.windowCounts {
		density := float64(count)
		if density > ispDensityPeak {
			ispDensityPeak = density
			worstDensitySec = float64(i)
		}
	}

	var ispDensityAvg float64

	if totalFrames > 0 {
		durationSec := float64(totalFrames) / float64(isp.samplesPerSec)
		if durationSec > 0 {
			ispDensityAvg = float64(isp.count) / durationSec
		}
	}

	return &types.TruePeakResult{
		TruePeakDB:   truePeakDB,
		SamplePeakDB: samplePeakDB,
		ISPCount:     isp.count,
		ISPMaxDB:     isp.maxDB,
		Frames:       totalFrames,

		ISPDensityPeak:  ispDensityPeak,
		ISPDensityAvg:   ispDensityAvg,
		ISPsAboveHalfdB: isp.aboveHalfdB,
		ISPsAbove1dB:    isp.above1dB,
		ISPsAbove2dB:    isp.above2dB,
		WorstDensitySec: worstDensitySec,
	}
}
