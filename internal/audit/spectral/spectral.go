package spectral

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"gonum.org/v1/gonum/dsp/fourier"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

const (
	defaultFFTSize    = 8192
	defaultWindowsMax = 100

	// defaultNoiseFlatnessCutoff is the Wiener entropy threshold for HF noise classification.
	defaultNoiseFlatnessCutoff = 0.4

	// Spectral analysis frequency constants.
	refBandStartHz = 1000
	refBandEndHz   = 10000
	nfBandStartHz  = 14000
	nfBandEndHz    = 18000

	// Brick wall detection parameters.
	brickWallBelowOffset = 1500
	brickWallAboveOffset = 1500
	brickWallNarrow      = 500
	brickWallSpan        = 1000

	// Detection thresholds.
	upsampleDropThreshold       = 20
	upsampleSharpnessThreshold  = 40
	transcodeDropThreshold      = 15
	transcodeSharpnessThreshold = 30
	transcodeProximity          = 2000
	brickWallMinDrop            = 10
	humSpikeThreshold           = 15
	humSharpnessMin             = 6

	// Hann window coefficient.
	hannCoeff = 0.5

	// Band energy margin factor.
	bandMarginLow  = 0.9
	bandMarginHigh = 1.1

	// Percentile bin adjacency.
	humAdjacentRange = 5
	humBinGuard      = 2

	// Mains hum fundamental frequencies.
	humFreq50Hz = 50
	humFreq60Hz = 60

	// Harmonic count for hum detection.
	humHarmonicCount = 6

	// firstHarmonic is the starting harmonic multiplier.
	firstHarmonic = 1.0

	// Minimum sample rate that could be upsampled.
	minUpsampleRate = 44100

	// shift16 is the bit shift for 24-bit high-byte decoding.
	shift16 = 16

	// readBufFrames is the number of frames per read buffer.
	spectralReadBufFrames = 4096
)

// Options configures spectral analysis.
type Options struct {
	FFTSize    int // default 8192
	WindowsMax int // max windows to analyze; 0 = all (default 100)

	// NoiseFlatnessCutoff is the spectral flatness threshold below which HF energy
	// is considered tonal content rather than noise. Flatness is the Wiener entropy
	// (geometric mean / arithmetic mean): 1.0 = white noise (flat), 0.0 = pure tone.
	// Below this cutoff, the noise floor level is capped at -40 dB to avoid false
	// positives on dark recordings. Default 0.4. Used only by AnalyzeV2.
	NoiseFlatnessCutoff float64
}

// DefaultOptions returns the default spectral analysis options.
func DefaultOptions() Options {
	return Options{
		FFTSize:             defaultFFTSize,
		WindowsMax:          defaultWindowsMax,
		NoiseFlatnessCutoff: defaultNoiseFlatnessCutoff,
	}
}

//nolint:gochecknoglobals // constant lookup table for lossy transcode cutoff frequencies
var transcodeCutoffs = []struct {
	freq  float64
	codec string
}{
	{15500, "AAC 128"},
	{16000, "MP3 128"},
	{17500, "MP3 160"},
	{18000, "MP3 192 / AAC 192"},
	{19000, "MP3 256 / AAC 256"},
	{20000, "MP3 320"},
	{20500, "Opus 128"},
}

//nolint:gochecknoglobals // constant lookup table for upsample Nyquist frequencies
var upsampleNyquists = []struct {
	rate    int
	nyquist float64
}{
	{44100, 22050},
	{48000, 24000},
	{88200, 44100},
	{96000, 48000},
}

//nolint:gochecknoglobals // constant lookup table for band energy analysis frequencies
var bandFrequencies = []float64{100, 500, 1000, 2000, 4000, 8000, 12000, 16000, 20000, 22050, 24000, 30000, 40000}

// Analyze performs spectral analysis on PCM audio data.
func Analyze(reader io.Reader, format types.PCMFormat, opts Options) (*types.SpectralResult, error) {
	if opts.FFTSize == 0 {
		opts.FFTSize = defaultFFTSize
	}

	if opts.WindowsMax == 0 {
		opts.WindowsMax = defaultWindowsMax
	}

	fftSize := opts.FFTSize

	// Phase 1: Read entire stream into mono-mixed samples.
	samples, err := readMonoMixed(reader, format)
	if err != nil {
		return nil, err
	}

	totalFrames := uint64(len(samples))

	if len(samples) < fftSize {
		return &types.SpectralResult{
			ClaimedRate: format.SampleRate,
			Frames:      totalFrames,
		}, nil
	}

	// Phase 2: Compute evenly spaced window positions.
	positions := windowPositions(len(samples), fftSize, opts.WindowsMax)

	if len(positions) == 0 {
		return &types.SpectralResult{
			ClaimedRate: format.SampleRate,
			Frames:      totalFrames,
		}, nil
	}

	// Phase 3: Process FFT windows.
	window := makeHannWindow(fftSize)
	binCount := fftSize/2 + 1
	magnitudeSum := make([]float64, binCount)
	fft := fourier.NewFFT(fftSize)
	fftIn := make([]float64, fftSize)

	for _, pos := range positions {
		for i := range fftSize {
			fftIn[i] = samples[pos+i] * window[i]
		}

		coeffs := fft.Coefficients(nil, fftIn)

		for i, c := range coeffs {
			magnitudeSum[i] += math.Sqrt(real(c)*real(c) + imag(c)*imag(c))
		}
	}

	windowsProcessed := len(positions)

	// Average magnitude spectrum.
	avgMagnitude := make([]float64, binCount)
	for i := range avgMagnitude {
		avgMagnitude[i] = magnitudeSum[i] / float64(windowsProcessed)
	}

	binHz := float64(format.SampleRate) / float64(fftSize)
	nyquist := float64(format.SampleRate) / 2

	magDB := toDB(avgMagnitude)

	// Reference level: 1-10 kHz average.
	refLevel := bandAverage(magDB, refBandStartHz, refBandEndHz, binHz)

	result := &types.SpectralResult{
		ClaimedRate: format.SampleRate,
		Frames:      totalFrames,
	}

	// === Sample rate authenticity ===
	if format.SampleRate > minUpsampleRate {
		detectUpsampling(result, magDB, binHz, nyquist, refLevel)
	}

	// === Lossy transcode detection ===
	detectTranscode(result, magDB, binHz, nyquist, refLevel)

	// === Hum detection ===
	detectHum(result, magDB, binHz, refLevel)

	// === Noise floor ===
	detectNoiseFloor(result, magDB, binHz, nyquist, refLevel)

	// === Spectral centroid ===
	result.SpectralCentroid = calculateCentroid(avgMagnitude, binHz)

	// === Band energy for debugging ===
	result.BandEnergy, result.BandFreqs = calculateBandEnergy(magDB, binHz, nyquist, refLevel)

	return result, nil
}

// decodeMono16 decodes 16-bit PCM frames into mono-mixed samples.
func decodeMono16(data []byte, frameSize, numChannels int, maxVal float64) []float64 {
	samples := make([]float64, 0, len(data)/frameSize)

	for i := 0; i < len(data); i += frameSize {
		var sum float64
		for ch := range numChannels {
			sum += float64(
				int16(binary.LittleEndian.Uint16(data[i+ch*2:])), //nolint:gosec // PCM sample conversion
			) / maxVal
		}

		samples = append(samples, sum/float64(numChannels))
	}

	return samples
}

// decodeMono24 decodes 24-bit PCM frames into mono-mixed samples.
func decodeMono24(data []byte, frameSize, numChannels int, maxVal float64) []float64 {
	samples := make([]float64, 0, len(data)/frameSize)

	for i := 0; i < len(data); i += frameSize {
		var sum float64

		for ch := range numChannels {
			offset := i + ch*3

			raw := int32(data[offset]) | int32(data[offset+1])<<shared.Shift8 | int32(data[offset+2])<<shift16
			if raw&shared.Mask24Sign != 0 {
				raw |= ^shared.Mask24Extend
			}

			sum += float64(raw) / maxVal
		}

		samples = append(samples, sum/float64(numChannels))
	}

	return samples
}

// decodeMono32 decodes 32-bit PCM frames into mono-mixed samples.
func decodeMono32(data []byte, frameSize, numChannels int, maxVal float64) []float64 {
	samples := make([]float64, 0, len(data)/frameSize)

	for i := 0; i < len(data); i += frameSize {
		var sum float64
		for ch := range numChannels {
			sum += float64(
				int32(binary.LittleEndian.Uint32(data[i+ch*4:])), //nolint:gosec // PCM sample conversion
			) / maxVal
		}

		samples = append(samples, sum/float64(numChannels))
	}

	return samples
}

// readMonoMixed reads the entire PCM stream and returns mono-mixed samples.
func readMonoMixed(reader io.Reader, format types.PCMFormat) ([]float64, error) {
	bytesPerSample := int(format.BitDepth / shared.Shift8) //nolint:gosec // bit depth is a small constant
	numChannels := int(format.Channels)                    //nolint:gosec // channel count is a small constant
	frameSize := bytesPerSample * numChannels

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

	readBuf := make([]byte, frameSize*spectralReadBufFrames)

	var samples []float64

	for {
		n, err := reader.Read(readBuf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			data := readBuf[:completeFrames]

			switch format.BitDepth {
			case types.Depth16:
				samples = append(samples, decodeMono16(data, frameSize, numChannels, maxVal)...)
			case types.Depth24:
				samples = append(samples, decodeMono24(data, frameSize, numChannels, maxVal)...)
			case types.Depth32:
				samples = append(samples, decodeMono32(data, frameSize, numChannels, maxVal)...)
			default:
			}
		}

		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}

	return samples, nil
}

// windowPositions returns evenly spaced FFT window start positions.
// If the track has fewer possible windows than maxWindows, all are returned.
// Otherwise, maxWindows positions are distributed evenly across the track.
func windowPositions(totalSamples, fftSize, maxWindows int) []int {
	available := totalSamples - fftSize
	if available < 0 {
		return nil
	}

	hopSize := fftSize / 2
	totalPossible := available/hopSize + 1

	if totalPossible <= maxWindows {
		positions := make([]int, 0, totalPossible)
		for pos := 0; pos+fftSize <= totalSamples; pos += hopSize {
			positions = append(positions, pos)
		}

		return positions
	}

	positions := make([]int, maxWindows)
	if maxWindows == 1 {
		positions[0] = available / 2

		return positions
	}

	for i := range maxWindows {
		positions[i] = available * i / (maxWindows - 1)
	}

	return positions
}

func makeHannWindow(size int) []float64 {
	window := make([]float64, size)
	for i := range window {
		window[i] = hannCoeff * (1 - math.Cos(2*math.Pi*float64(i)/float64(size-1)))
	}

	return window
}

func toDB(magnitude []float64) []float64 {
	decibels := make([]float64, len(magnitude))
	for i, m := range magnitude {
		if m > 0 {
			decibels[i] = float64(shared.DBMultiplier) * math.Log10(m)
		} else {
			decibels[i] = shared.SilenceFloorDB
		}
	}

	return decibels
}

func bandAverage(magDB []float64, startHz, endHz, binHz float64) float64 {
	startBin := int(startHz / binHz)
	endBin := int(endHz / binHz)

	if startBin < 0 {
		startBin = 0
	}

	if endBin >= len(magDB) {
		endBin = len(magDB) - 1
	}

	if startBin > endBin {
		return shared.SilenceFloorDB
	}

	var sum float64
	for i := startBin; i <= endBin; i++ {
		sum += magDB[i]
	}

	return sum / float64(endBin-startBin+1)
}

func detectBrickWall(magDB []float64, checkFreq, binHz float64) (drop, sharpness float64) {
	belowLevel := bandAverage(magDB, checkFreq-brickWallBelowOffset, checkFreq-brickWallNarrow, binHz)
	aboveLevel := bandAverage(magDB, checkFreq+brickWallNarrow, checkFreq+brickWallAboveOffset, binHz)

	drop = belowLevel - aboveLevel

	if drop > brickWallMinDrop {
		freqBelow := checkFreq - brickWallSpan
		freqAbove := checkFreq + brickWallSpan
		octaves := math.Log2(freqAbove / freqBelow)
		sharpness = drop / octaves
	}

	return drop, sharpness
}

func detectUpsampling(result *types.SpectralResult, magDB []float64, binHz, nyquist, _ float64) {
	var (
		bestSharpness float64
		bestCutoff    float64
		bestRate      int
	)

	for _, sampleRate := range upsampleNyquists {
		if sampleRate.nyquist >= nyquist {
			continue
		}

		drop, sharpness := detectBrickWall(magDB, sampleRate.nyquist, binHz)

		if drop > upsampleDropThreshold && sharpness > bestSharpness {
			bestSharpness = sharpness
			bestCutoff = sampleRate.nyquist
			bestRate = sampleRate.rate
		}
	}

	if bestSharpness > upsampleSharpnessThreshold {
		result.IsUpsampled = true
		result.EffectiveRate = bestRate
		result.UpsampleCutoff = bestCutoff
		result.UpsampleSharpness = bestSharpness
	}
}

func detectTranscode(result *types.SpectralResult, magDB []float64, binHz, nyquist, _ float64) {
	// Only check if claimed sample rate is 44.1/48k (or if upsampled from there)
	// Transcode detection looks for cutoffs below 22kHz
	var (
		bestSharpness float64
		bestCutoff    float64
		bestCodec     string
	)

	for _, transcodeInfo := range transcodeCutoffs {
		if transcodeInfo.freq >= nyquist {
			continue
		}
		// Don't flag upsample cutoff as transcode
		if result.IsUpsampled && math.Abs(transcodeInfo.freq-result.UpsampleCutoff) < transcodeProximity {
			continue
		}

		drop, sharpness := detectBrickWall(magDB, transcodeInfo.freq, binHz)

		if drop > transcodeDropThreshold && sharpness > bestSharpness {
			bestSharpness = sharpness
			bestCutoff = transcodeInfo.freq
			bestCodec = transcodeInfo.codec
		}
	}

	if bestSharpness > transcodeSharpnessThreshold {
		result.IsTranscode = true
		result.TranscodeCutoff = bestCutoff
		result.TranscodeSharpness = bestSharpness
		result.LikelyCodec = bestCodec
	}
}

func detectHum(result *types.SpectralResult, magDB []float64, binHz, _ float64) {
	// Check 50Hz and harmonics (100, 150, 200, 250, 300 Hz)
	hum50 := detectHumFrequency(magDB, humFreq50Hz, binHz)
	// Check 60Hz and harmonics (120, 180, 240, 300, 360 Hz)
	hum60 := detectHumFrequency(magDB, humFreq60Hz, binHz)

	if hum50 > humSpikeThreshold {
		result.Has50HzHum = true
		result.HumLevelDB = hum50
	}

	if hum60 > humSpikeThreshold {
		result.Has60HzHum = true
		if hum60 > result.HumLevelDB {
			result.HumLevelDB = hum60
		}
	}
}

func detectHumFrequency(magDB []float64, fundamental, binHz float64) float64 {
	var maxSpike float64

	for harmonic := firstHarmonic; harmonic <= humHarmonicCount; harmonic++ {
		freq := fundamental * harmonic
		bin := int(freq / binHz)

		if bin <= humBinGuard || bin >= len(magDB)-humBinGuard {
			continue
		}

		spike := measureHumSpike(magDB, bin)
		if spike > maxSpike {
			maxSpike = spike
		}
	}

	return maxSpike
}

// measureHumSpike measures the spectral spike at a given bin relative to its surroundings.
func measureHumSpike(magDB []float64, bin int) float64 {
	peakLevel := magDB[bin]

	// Average of surrounding bins (±5 bins, excluding ±1).
	var surroundSum float64

	surroundCount := 0

	for idx := bin - humAdjacentRange; idx <= bin+humAdjacentRange; idx++ {
		if idx >= 0 && idx < len(magDB) && (idx < bin-1 || idx > bin+1) {
			surroundSum += magDB[idx]
			surroundCount++
		}
	}

	surroundAvg := surroundSum / float64(surroundCount)

	spike := peakLevel - surroundAvg

	// Peak sharpness check: real mains hum is a razor-sharp spectral line
	// concentrated in 1-2 FFT bins, while musical bass content (synth, kick)
	// spreads energy across many bins.
	if bin >= 1 && bin < len(magDB)-1 {
		adjacentAvg := (magDB[bin-1] + magDB[bin+1]) / 2
		sharpness := peakLevel - adjacentAvg

		if sharpness < humSharpnessMin {
			return 0
		}
	}

	return spike
}

func detectNoiseFloor(result *types.SpectralResult, magDB []float64, binHz, _, refLevel float64) {
	hfLevel := bandAverage(magDB, nfBandStartHz, nfBandEndHz, binHz)
	result.NoiseFloorDB = hfLevel - refLevel
}

func calculateCentroid(magnitude []float64, binHz float64) float64 {
	var (
		weightedSum float64
		totalMag    float64
	)

	for i, mag := range magnitude {
		freq := float64(i) * binHz
		weightedSum += freq * mag
		totalMag += mag
	}

	if totalMag == 0 {
		return 0
	}

	return weightedSum / totalMag
}

func calculateBandEnergy(magDB []float64, binHz, nyquist, refLevel float64) (energy, freqs []float64) {
	for _, freq := range bandFrequencies {
		if freq >= nyquist {
			break
		}

		level := bandAverage(magDB, freq*bandMarginLow, freq*bandMarginHigh, binHz)
		energy = append(energy, level-refLevel)
		freqs = append(freqs, freq)
	}

	return energy, freqs
}
