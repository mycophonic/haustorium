// Package truncation detects abruptly truncated audio by analyzing the tail of the PCM stream.
package truncation

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
	defaultWindowMs uint = 50
)

// Detect checks whether the final windowMs of audio suggests abrupt truncation.
func Detect(r io.ReadSeeker, format types.PCMFormat, windowMs uint) (*types.TruncationDetection, error) {
	if windowMs == 0 {
		windowMs = defaultWindowMs
	}

	bytesPerSample := int(
		format.BitDepth / 8,
	)
	tailSamples := format.SampleRate * int(
		windowMs,
	) / shared.MsPerSec * int(
		format.Channels,
	)
	tailBytes := int64(tailSamples * bytesPerSample)

	// Seek to end minus tail size
	_, err := r.Seek(-tailBytes, io.SeekEnd)
	if err != nil {
		// File shorter than tail window, seek to start
		_, err = r.Seek(0, io.SeekStart)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
		}
	}

	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", fault.ErrReadFailure, err)
	}

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

	var (
		sumSquares float64
		peak       float64
		count      uint64
	)

	completeSamples := (len(buf) / bytesPerSample) * bytesPerSample
	data := buf[:completeSamples]

	switch format.BitDepth {
	case types.Depth16:
		for i := 0; i < len(data); i += 2 {
			sample := int16(
				binary.LittleEndian.Uint16(data[i:]),
			)
			normalized := float64(sample) / maxVal

			sumSquares += normalized * normalized
			if abs := math.Abs(normalized); abs > peak {
				peak = abs
			}

			count++
		}
	case types.Depth24:
		for i := 0; i < len(data); i += 3 {
			sample := int32(data[i]) | int32(data[i+1])<<shared.Shift8 | int32(data[i+2])<<16
			if sample&shared.Mask24Sign != 0 {
				sample |= ^shared.Mask24Extend
			}

			normalized := float64(sample) / maxVal

			sumSquares += normalized * normalized
			if abs := math.Abs(normalized); abs > peak {
				peak = abs
			}

			count++
		}
	case types.Depth32:
		for i := 0; i < len(data); i += 4 {
			sample := int32(
				binary.LittleEndian.Uint32(data[i:]),
			)
			normalized := float64(sample) / maxVal

			sumSquares += normalized * normalized
			if abs := math.Abs(normalized); abs > peak {
				peak = abs
			}

			count++
		}
	default:
	}

	if count == 0 {
		return &types.TruncationDetection{
			IsTruncated:   false,
			FinalRmsDB:    shared.SilenceFloorDB,
			FinalPeakDB:   shared.SilenceFloorDB,
			SamplesInTail: 0,
		}, nil
	}

	rms := math.Sqrt(sumSquares / float64(count))
	rmsDB := shared.DBMultiplier * math.Log10(rms)
	peakDB := shared.DBMultiplier * math.Log10(peak)

	if math.IsInf(rmsDB, -1) {
		rmsDB = shared.SilenceFloorDB
	}

	if math.IsInf(peakDB, -1) {
		peakDB = shared.SilenceFloorDB
	}

	return &types.TruncationDetection{
		FinalRmsDB:    rmsDB,
		FinalPeakDB:   peakDB,
		SamplesInTail: count,
	}, nil
}
