package spectral

import (
	"io"
	"math"

	"gonum.org/v1/gonum/dsp/fourier"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

// V2-specific detection constants.
const (
	// Hum V2 variance threshold: coefficient of variation below this indicates hum.
	humMaxVariance = 0.3

	// Noise floor V2 constants.
	quietWindowFraction = 5     // denominator: use 1/5 (20%) quietest windows
	quietGateDBFS       = -50.0 // RMS gate for quiet-window selection
	noiseFlatnessCap    = -40   // dB cap when HF is tonal (not noise)
	nyquistGuardHz      = 500   // guard band below Nyquist for HF measurement

	// Transcode V2 confidence parameters.
	transcodeBaseConfidence     = 0.95
	transcodeMinConfidence      = 0.50
	transcodeConsistencyHz      = 50   // stddev below this suggests mastering filter
	transcodeConsistencyPenalty = 0.20 // max confidence reduction for consistency
	transcodeUltrasonicPenalty  = 0.40 // confidence reduction for ultrasonic content
	transcodeHFPenalty          = 0.10 // confidence reduction for high cutoff frequency
	transcodeHFThresholdHz      = 20000
	transcodeHFRangeHz          = 5000
	transcodeSharpnessPenalty   = 0.10 // confidence reduction for moderate sharpness
	transcodeV2SharpnessFloor   = 40   // sharpness below this is moderate

	// Cutoff consistency measurement.
	cutoffSearchRangeHz = 2000 // search +/- 2 kHz around target cutoff
	cutoffMinDropDB     = 5    // minimum drop to count as meaningful
	cutoffMinWindows    = 3    // minimum windows for consistency measurement

	// Ultrasonic content detection.
	ultrasonicOffsetHz   = 500 // offset above cutoff for ultrasonic check
	ultrasonicRelativeDB = -50 // threshold relative to reference level

	// Default coefficient of variation for no-data case.
	defaultCV = 1.0

	// Confidence clamping bounds.
	confidenceMin = 0.0
	confidenceMax = 1.0
)

// AnalyzeV2 adds temporal variance analysis to reduce false positives for hum
// and noise floor detection on legitimately dark or bass-heavy recordings.
func AnalyzeV2(reader io.Reader, format types.PCMFormat, opts Options) (*types.SpectralResult, error) {
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

	// Phase 3: Process FFT windows, keeping per-window data for variance analysis.
	windowMagnitudes, windowRMS, magnitudeSum := processFFTWindows(samples, positions, fftSize)
	windowsProcessed := len(positions)

	// Average magnitude spectrum.
	binCount := fftSize/2 + 1

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

	// === Lossy transcode detection V2 (with consistency analysis) ===
	detectTranscodeV2(result, windowMagnitudes, magDB, binHz, nyquist, refLevel)

	// === Hum detection V2 (with variance) ===
	detectHumV2(result, windowMagnitudes, binHz)

	// === Noise floor V2 (quiet-window HF + full-track reference + RMS gate) ===
	detectNoiseFloorV2(result, windowMagnitudes, windowRMS, magDB, binHz, nyquist, refLevel, opts)

	// === Spectral centroid ===
	result.SpectralCentroid = calculateCentroid(avgMagnitude, binHz)

	// === Band energy for debugging ===
	result.BandEnergy, result.BandFreqs = calculateBandEnergy(magDB, binHz, nyquist, refLevel)

	return result, nil
}

// processFFTWindows computes per-window magnitudes, RMS, and the magnitude sum.
func processFFTWindows(
	samples []float64, positions []int, fftSize int,
) (
	windowMagnitudes [][]float64,
	windowRMS []float64,
	magnitudeSum []float64,
) {
	binCount := fftSize/2 + 1
	magnitudeSum = make([]float64, binCount)
	window := makeHannWindow(fftSize)
	fft := fourier.NewFFT(fftSize)
	fftIn := make([]float64, fftSize)

	windowMagnitudes = make([][]float64, len(positions))
	windowRMS = make([]float64, len(positions))

	for windowIdx, pos := range positions {
		var rmsSum float64

		for i := range fftSize {
			fftIn[i] = samples[pos+i] * window[i]
			rmsSum += samples[pos+i] * samples[pos+i]
		}

		windowRMS[windowIdx] = math.Sqrt(rmsSum / float64(fftSize))

		coeffs := fft.Coefficients(nil, fftIn)

		windowMagnitudes[windowIdx] = make([]float64, binCount)

		for i, c := range coeffs {
			mag := math.Sqrt(real(c)*real(c) + imag(c)*imag(c))
			windowMagnitudes[windowIdx][i] = mag
			magnitudeSum[i] += mag
		}
	}

	return windowMagnitudes, windowRMS, magnitudeSum
}

// detectHumV2 checks for hum by analyzing temporal variance.
// Real hum is constant; musical content at 50/60 Hz varies with the performance.
func detectHumV2(result *types.SpectralResult, windowMagnitudes [][]float64, binHz float64) {
	hum50, variance50 := detectHumFrequencyV2(windowMagnitudes, humFreq50Hz, binHz)
	hum60, variance60 := detectHumFrequencyV2(windowMagnitudes, humFreq60Hz, binHz)

	// Hum = high level + low variance (coefficient of variation < humMaxVariance)
	// Music = high level + high variance
	if hum50 > humSpikeThreshold && variance50 < humMaxVariance {
		result.Has50HzHum = true
		result.HumLevelDB = hum50
	}

	if hum60 > humSpikeThreshold && variance60 < humMaxVariance {
		result.Has60HzHum = true
		if hum60 > result.HumLevelDB {
			result.HumLevelDB = hum60
		}
	}
}

// detectHumFrequencyV2 returns the spike level and coefficient of variation across windows.
func detectHumFrequencyV2(windowMagnitudes [][]float64, fundamental, binHz float64) (spike, coeffVar float64) {
	if len(windowMagnitudes) == 0 {
		return 0, defaultCV
	}

	// For each window, compute the max spike across harmonics.
	windowSpikes := make([]float64, len(windowMagnitudes))

	for windowIdx, mag := range windowMagnitudes {
		magDB := toDB(mag)
		windowSpikes[windowIdx] = measureMaxHumSpikeV2(magDB, fundamental, binHz)
	}

	// Compute mean and standard deviation of spikes across windows.
	mean, stdDev := meanStdDev(windowSpikes)

	// Coefficient of variation (stdDev / mean).
	// Low CV = consistent level = hum.
	// High CV = varying level = music.
	cv := defaultCV
	if mean > 0 {
		cv = stdDev / mean
	}

	return mean, cv
}

// measureMaxHumSpikeV2 finds the maximum hum spike across harmonics for a single window.
func measureMaxHumSpikeV2(magDB []float64, fundamental, binHz float64) float64 {
	var maxSpike float64

	for harmonic := firstHarmonic; harmonic <= humHarmonicCount; harmonic++ {
		freq := fundamental * harmonic
		bin := int(freq / binHz)

		if bin <= humAdjacentRange || bin >= len(magDB)-humAdjacentRange {
			continue
		}

		spike := measureHumSpikeV2(magDB, bin)
		if spike > maxSpike {
			maxSpike = spike
		}
	}

	return maxSpike
}

// measureHumSpikeV2 measures the spectral spike at a given bin with sharpness filtering.
func measureHumSpikeV2(magDB []float64, bin int) float64 {
	peakLevel := magDB[bin]

	var surroundSum float64

	surroundCount := 0

	for idx := bin - humAdjacentRange; idx <= bin+humAdjacentRange; idx++ {
		if idx >= 0 && idx < len(magDB) && (idx < bin-1 || idx > bin+1) {
			surroundSum += magDB[idx]
			surroundCount++
		}
	}

	if surroundCount == 0 {
		return 0
	}

	surroundAvg := surroundSum / float64(surroundCount)
	spikeLevel := peakLevel - surroundAvg

	// Peak sharpness: reject broad spectral bumps (synth bass, kick)
	// that are not genuine tonal spikes.
	if bin >= 1 && bin < len(magDB)-1 {
		adjacentAvg := (magDB[bin-1] + magDB[bin+1]) / 2
		if peakLevel-adjacentAvg < humSharpnessMin {
			return 0
		}
	}

	return spikeLevel
}

// meanStdDev computes the mean and standard deviation of a slice.
func meanStdDev(values []float64) (mean, stdDev float64) {
	if len(values) == 0 {
		return 0, 0
	}

	var sum float64
	for _, v := range values {
		sum += v
	}

	mean = sum / float64(len(values))

	var varianceSum float64

	for _, v := range values {
		d := v - mean
		varianceSum += d * d
	}

	stdDev = math.Sqrt(varianceSum / float64(len(values)))

	return mean, stdDev
}

// detectNoiseFloorV2 measures noise floor using quiet-window HF with full-track reference,
// gated by an absolute RMS threshold on the quiet windows.
//
// Strategy:
//   - HF energy (14-18 kHz) is measured from the quietest 20% of windows to expose the true
//     noise floor without signal masking.
//   - Reference level (1-10 kHz) comes from the full-track average for a stable baseline.
//   - RMS gate: if the quiet windows are not actually quiet (above -40 dBFS), they contain
//     signal, not noise. In that case, fall back to full-track HF measurement.
//   - Spectral flatness guard: suppress detection when HF energy is tonal (music, not noise).
func detectNoiseFloorV2(
	result *types.SpectralResult,
	windowMagnitudes [][]float64,
	windowRMS []float64,
	magDB []float64,
	binHz, nyquist, refLevel float64,
	opts Options,
) {
	if len(windowMagnitudes) == 0 {
		result.NoiseFloorDB = shared.SilenceFloorDB

		return
	}

	// HF band boundaries.
	binCount := len(windowMagnitudes[0])
	hfStart := int(nfBandStartHz / binHz)
	hfEnd := int(min(float64(nfBandEndHz), nyquist-nyquistGuardHz) / binHz)

	if hfStart >= binCount || hfEnd <= hfStart {
		result.NoiseFloorDB = shared.SilenceFloorDB

		return
	}

	// Find the quietest 20% of windows (or at least 1).
	quietCount := max(len(windowRMS)/quietWindowFraction, 1)
	quietIndices := findQuietestWindows(windowRMS, quietCount)

	useQuietWindows := isQuietWindowUsable(windowRMS, quietIndices)

	var hfDB float64

	if useQuietWindows {
		hfDB = measureQuietWindowHF(windowMagnitudes, quietIndices, hfStart, hfEnd, refLevel)
	} else {
		// Quiet windows still contain signal - fall back to full-track HF.
		hfLevel := bandAverage(magDB, nfBandStartHz, nfBandEndHz, binHz)
		hfDB = hfLevel - refLevel
	}

	result.NoiseFloorDB = hfDB

	// Spectral flatness guard: only flag if HF energy is spectrally flat (actual noise).
	flatness := measureQuietHFFlatness(windowMagnitudes, quietIndices, hfStart, hfEnd)
	if !useQuietWindows {
		flatness = measureFullTrackHFFlatness(windowMagnitudes, hfStart, hfEnd)
	}

	flatnessCutoff := opts.NoiseFlatnessCutoff
	if flatnessCutoff == 0 {
		flatnessCutoff = defaultNoiseFlatnessCutoff
	}

	if flatness < flatnessCutoff {
		// Not flat enough to be noise; likely just dark recording.
		// Cap the reported level below the mild threshold to avoid false positive.
		result.NoiseFloorDB = min(hfDB, noiseFlatnessCap)
	}
}

// isQuietWindowUsable checks if the quiet windows are genuinely quiet (below the RMS gate).
func isQuietWindowUsable(windowRMS []float64, quietIndices []int) bool {
	var quietRMSSum float64
	for _, wi := range quietIndices {
		quietRMSSum += windowRMS[wi]
	}

	avgQuietRMS := quietRMSSum / float64(len(quietIndices))

	quietRMSDB := shared.SilenceFloorDB
	if avgQuietRMS > 0 {
		quietRMSDB = float64(shared.DBMultiplier) * math.Log10(avgQuietRMS)
	}

	return quietRMSDB > quietGateDBFS
}

// measureQuietWindowHF measures HF energy from the quietest windows.
func measureQuietWindowHF(
	windowMagnitudes [][]float64, quietIndices []int,
	hfStart, hfEnd int, refLevel float64,
) float64 {
	var hfSum float64

	hfBins := hfEnd - hfStart

	for _, wi := range quietIndices {
		mag := windowMagnitudes[wi]

		var bandSum float64
		for i := hfStart; i < hfEnd && i < len(mag); i++ {
			bandSum += mag[i]
		}

		hfSum += bandSum / float64(hfBins)
	}

	avgHF := hfSum / float64(len(quietIndices))

	hfDB := shared.SilenceFloorDB
	if avgHF > 0 {
		hfDB = float64(shared.DBMultiplier)*math.Log10(avgHF) - refLevel
	}

	return hfDB
}

// measureQuietHFFlatness computes spectral flatness of the HF band from quiet windows.
func measureQuietHFFlatness(
	windowMagnitudes [][]float64, quietIndices []int,
	hfStart, hfEnd int,
) float64 {
	var flatnessSum float64

	for _, wi := range quietIndices {
		mag := windowMagnitudes[wi]
		flatnessSum += spectralFlatness(mag[hfStart:min(hfEnd, len(mag))])
	}

	return flatnessSum / float64(len(quietIndices))
}

// measureFullTrackHFFlatness computes spectral flatness of the HF band from all windows.
func measureFullTrackHFFlatness(
	windowMagnitudes [][]float64,
	hfStart, hfEnd int,
) float64 {
	binCount := len(windowMagnitudes[0])
	avgMag := make([]float64, binCount)

	for _, wm := range windowMagnitudes {
		for i := hfStart; i < hfEnd && i < len(wm); i++ {
			avgMag[i] += wm[i]
		}
	}

	wc := float64(len(windowMagnitudes))
	for i := hfStart; i < hfEnd && i < len(avgMag); i++ {
		avgMag[i] /= wc
	}

	return spectralFlatness(avgMag[hfStart:min(hfEnd, binCount)])
}

// findQuietestWindows returns indices of the N quietest windows by RMS.
func findQuietestWindows(windowRMS []float64, count int) []int {
	if count >= len(windowRMS) {
		indices := make([]int, len(windowRMS))
		for i := range indices {
			indices[i] = i
		}

		return indices
	}

	// Simple selection: copy and sort indices by RMS.
	type indexedRMS struct {
		index int
		rms   float64
	}

	indexed := make([]indexedRMS, len(windowRMS))
	for i, rms := range windowRMS {
		indexed[i] = indexedRMS{i, rms}
	}

	// Partial sort: find count smallest.
	// For simplicity, full sort then take first count.
	for idx := range count {
		minIdx := idx
		for j := idx + 1; j < len(indexed); j++ {
			if indexed[j].rms < indexed[minIdx].rms {
				minIdx = j
			}
		}

		indexed[idx], indexed[minIdx] = indexed[minIdx], indexed[idx]
	}

	result := make([]int, count)
	for idx := range count {
		result[idx] = indexed[idx].index
	}

	return result
}

// spectralFlatness computes the Wiener entropy: geometric mean / arithmetic mean.
// Returns 1.0 for white noise (flat spectrum), lower for tonal content.
func spectralFlatness(magnitudes []float64) float64 {
	if len(magnitudes) == 0 {
		return 0
	}

	var (
		arithmeticSum float64
		logSum        float64
	)

	count := 0

	for _, m := range magnitudes {
		if m > 0 {
			arithmeticSum += m
			logSum += math.Log(m)
			count++
		}
	}

	if count == 0 || arithmeticSum == 0 {
		return 0
	}

	arithmeticMean := arithmeticSum / float64(count)
	geometricMean := math.Exp(logSum / float64(count))

	return geometricMean / arithmeticMean
}

// detectTranscodeV2 detects lossy transcodes with enhanced analysis to reduce false positives.
//
// Key improvements over V1:
//   - Measures cutoff consistency across windows (mastering LPFs are rock-solid, codecs may vary)
//   - Checks for ultrasonic content above the cutoff (mastering may leave some, codecs don't)
//   - Adjusts confidence based on these factors
//
// A 20-21 kHz cutoff on 44.1kHz content is ambiguous: it could be a legitimate mastering
// low-pass filter (common practice) or a high-bitrate lossy codec (Opus 128, AAC 256).
// This function attempts to distinguish them.
func detectTranscodeV2(
	result *types.SpectralResult,
	windowMagnitudes [][]float64,
	magDB []float64,
	binHz, nyquist, refLevel float64,
) {
	// First, run the basic detection to find candidate cutoffs.
	detectTranscode(result, magDB, binHz, nyquist, refLevel)

	// If no transcode detected, nothing more to do.
	if !result.IsTranscode {
		result.TranscodeConfidence = 0

		return
	}

	// Start with high confidence, reduce based on evidence.
	confidence := transcodeBaseConfidence
	cutoffFreq := result.TranscodeCutoff

	// === Check 1: Cutoff consistency across windows ===
	cutoffStdDev := measureCutoffConsistency(windowMagnitudes, cutoffFreq, binHz)
	result.CutoffConsistency = cutoffStdDev

	// Very low stddev suggests mastering filter, not codec.
	if cutoffStdDev < transcodeConsistencyHz {
		reduction := transcodeConsistencyPenalty * (1 - cutoffStdDev/transcodeConsistencyHz)
		confidence -= reduction
	}

	// === Check 2: Ultrasonic content above cutoff ===
	hasUltrasonic := checkUltrasonicContent(magDB, cutoffFreq, binHz, nyquist, refLevel)
	result.HasUltrasonicContent = hasUltrasonic

	if hasUltrasonic {
		confidence -= transcodeUltrasonicPenalty
	}

	// === Check 3: Cutoff frequency penalty for high frequencies ===
	if cutoffFreq >= transcodeHFThresholdHz {
		reduction := transcodeHFPenalty + (cutoffFreq-transcodeHFThresholdHz)/transcodeHFRangeHz*transcodeHFPenalty
		confidence -= min(reduction, transcodeConsistencyPenalty)
	}

	// === Check 4: Sharpness analysis ===
	sharpness := result.TranscodeSharpness
	if sharpness < transcodeV2SharpnessFloor {
		confidence -= transcodeSharpnessPenalty
	}

	// Clamp confidence to valid range.
	confidence = max(confidenceMin, min(confidenceMax, confidence))

	// If confidence drops below threshold, un-flag as transcode.
	if confidence < transcodeMinConfidence {
		result.IsTranscode = false
		result.LikelyCodec = ""
	}

	result.TranscodeConfidence = confidence
}

// measureCutoffConsistency measures how consistent the cutoff frequency is across windows.
// Returns the standard deviation of detected cutoff frequencies.
// Low stddev = consistent (mastering filter), high stddev = variable (possibly codec).
func measureCutoffConsistency(windowMagnitudes [][]float64, targetCutoff, binHz float64) float64 {
	if len(windowMagnitudes) < cutoffMinWindows {
		return 0 // not enough windows to measure consistency
	}

	// For each window, find the frequency where energy drops most sharply
	// in the vicinity of the target cutoff.
	searchStart := targetCutoff - cutoffSearchRangeHz
	searchEnd := targetCutoff + cutoffSearchRangeHz
	startBin := max(1, int(searchStart/binHz))

	var cutoffs []float64

	for _, mag := range windowMagnitudes {
		magDB := toDB(mag)
		endBin := min(len(magDB)-2, int(searchEnd/binHz))

		if startBin >= endBin {
			continue
		}

		// Find the bin with the steepest drop.
		var (
			maxDrop    float64
			maxDropBin int
		)

		for bin := startBin; bin < endBin; bin++ {
			// Measure drop from bin to bin+2 (smoothed gradient).
			drop := magDB[bin] - magDB[bin+2]
			if drop > maxDrop {
				maxDrop = drop
				maxDropBin = bin
			}
		}

		if maxDrop > cutoffMinDropDB { // only count if there's a meaningful drop
			cutoffs = append(cutoffs, float64(maxDropBin)*binHz)
		}
	}

	if len(cutoffs) < cutoffMinWindows {
		return 0
	}

	// Calculate standard deviation.
	_, stdDev := meanStdDev(cutoffs)

	return stdDev
}

// checkUltrasonicContent checks if there's any meaningful content above the cutoff.
// Legitimate mastering may leave faint harmonics, dither, or room noise above 20 kHz.
// Lossy codecs create a hard wall with nothing above.
func checkUltrasonicContent(magDB []float64, cutoffFreq, binHz, nyquist, refLevel float64) bool {
	// Check energy in the band from cutoff+500 Hz to nyquist-500 Hz.
	checkStart := cutoffFreq + ultrasonicOffsetHz
	checkEnd := nyquist - ultrasonicOffsetHz

	if checkEnd <= checkStart {
		return false // no room to check
	}

	startBin := int(checkStart / binHz)
	endBin := int(checkEnd / binHz)

	if startBin >= len(magDB) || endBin <= startBin {
		return false
	}

	endBin = min(endBin, len(magDB)-1)

	// Calculate average energy above cutoff.
	var sum float64

	count := 0

	for i := startBin; i <= endBin; i++ {
		sum += magDB[i]
		count++
	}

	if count == 0 {
		return false
	}

	avgAboveCutoff := sum / float64(count)

	// Compare to reference level.
	// If ultrasonic energy is within range of reference, there's content.
	relativeLevel := avgAboveCutoff - refLevel

	// If there's meaningful content (not just noise floor), return true.
	return relativeLevel > ultrasonicRelativeDB
}
