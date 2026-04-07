// Package stereo analyzes stereo field properties of PCM audio.
package stereo

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

// Analyze computes stereo correlation, channel balance, and mono-compatibility metrics.
func Analyze(reader io.Reader, format types.PCMFormat) (*types.StereoResult, error) {
	if format.Channels != 2 {
		return &types.StereoResult{
			Correlation:    0,
			DifferenceDB:   0,
			MonoSumDB:      0,
			StereoRmsDB:    0,
			CancellationDB: 0,
			LeftRmsDB:      0,
			RightRmsDB:     0,
			ImbalanceDB:    0,
			Frames:         0,
		}, nil
	}

	bytesPerSample := int(format.BitDepth / 8) //nolint:gosec // bit depth is a small constant
	frameSize := bytesPerSample * 2
	buf := make([]byte, frameSize*4096)

	var (
		sumL, sumR, sumLL, sumRR, sumLR   float64
		sumDiffSq, sumMonoSq, sumStereoSq float64
		frames                            uint64
	)

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

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			completeFrames := (n / frameSize) * frameSize
			data := buf[:completeFrames]

			switch format.BitDepth {
			case types.Depth16:
				for i := 0; i < len(data); i += 4 {
					left := float64(
						int16(binary.LittleEndian.Uint16(data[i:])),
					) / maxVal
					right := float64(
						int16(binary.LittleEndian.Uint16(data[i+2:])),
					) / maxVal

					sumL += left
					sumR += right
					sumLL += left * left
					sumRR += right * right
					sumLR += left * right

					diff := left - right
					sumDiffSq += diff * diff

					mono := (left + right) / 2
					sumMonoSq += mono * mono
					sumStereoSq += (left*left + right*right) / 2
					frames++
				}
			case types.Depth24:
				for idx := 0; idx < len(data); idx += 6 {
					leftRaw := int32(data[idx]) | int32(data[idx+1])<<8 | int32(data[idx+2])<<16
					if leftRaw&0x800000 != 0 {
						leftRaw |= ^0xFFFFFF
					}

					rightRaw := int32(data[idx+3]) | int32(data[idx+4])<<8 | int32(data[idx+5])<<16
					if rightRaw&0x800000 != 0 {
						rightRaw |= ^0xFFFFFF
					}

					left := float64(leftRaw) / maxVal
					right := float64(rightRaw) / maxVal

					sumL += left
					sumR += right
					sumLL += left * left
					sumRR += right * right
					sumLR += left * right

					diff := left - right
					sumDiffSq += diff * diff

					mono := (left + right) / 2
					sumMonoSq += mono * mono
					sumStereoSq += (left*left + right*right) / 2
					frames++
				}
			case types.Depth32:
				for i := 0; i < len(data); i += 8 {
					left := float64(
						int32(binary.LittleEndian.Uint32(data[i:])),
					) / maxVal
					right := float64(
						int32(binary.LittleEndian.Uint32(data[i+4:])),
					) / maxVal

					sumL += left
					sumR += right
					sumLL += left * left
					sumRR += right * right
					sumLR += left * right

					diff := left - right
					sumDiffSq += diff * diff

					mono := (left + right) / 2
					sumMonoSq += mono * mono
					sumStereoSq += (left*left + right*right) / 2
					frames++
				}
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

	if frames == 0 {
		return &types.StereoResult{
			Correlation:    0,
			DifferenceDB:   shared.SilenceFloorDB,
			MonoSumDB:      shared.SilenceFloorDB,
			StereoRmsDB:    shared.SilenceFloorDB,
			CancellationDB: 0,
			LeftRmsDB:      shared.SilenceFloorDB,
			RightRmsDB:     shared.SilenceFloorDB,
			ImbalanceDB:    0,
			Frames:         0,
		}, nil
	}

	count := float64(frames)

	// Pearson correlation
	numerator := count*sumLR - sumL*sumR
	denominator := math.Sqrt((count*sumLL - sumL*sumL) * (count*sumRR - sumR*sumR))

	var correlation float64
	if denominator > 0 {
		correlation = numerator / denominator
	}

	// RMS values
	diffRms := math.Sqrt(sumDiffSq / count)
	monoRms := math.Sqrt(sumMonoSq / count)
	stereoRms := math.Sqrt(sumStereoSq / count)
	leftRms := math.Sqrt(sumLL / count)
	rightRms := math.Sqrt(sumRR / count)

	diffDB := shared.DBMultiplier * math.Log10(diffRms)
	monoDB := shared.DBMultiplier * math.Log10(monoRms)
	stereoDB := shared.DBMultiplier * math.Log10(stereoRms)
	leftDB := shared.DBMultiplier * math.Log10(leftRms)
	rightDB := shared.DBMultiplier * math.Log10(rightRms)

	if math.IsInf(diffDB, -1) {
		diffDB = shared.SilenceFloorDB
	}

	if math.IsInf(monoDB, -1) {
		monoDB = shared.SilenceFloorDB
	}

	if math.IsInf(stereoDB, -1) {
		stereoDB = shared.SilenceFloorDB
	}

	if math.IsInf(leftDB, -1) {
		leftDB = shared.SilenceFloorDB
	}

	if math.IsInf(rightDB, -1) {
		rightDB = shared.SilenceFloorDB
	}

	return &types.StereoResult{
		Correlation:    correlation,
		DifferenceDB:   diffDB,
		MonoSumDB:      monoDB,
		StereoRmsDB:    stereoDB,
		CancellationDB: stereoDB - monoDB,
		LeftRmsDB:      leftDB,
		RightRmsDB:     rightDB,
		ImbalanceDB:    leftDB - rightDB,
		Frames:         frames,
	}, nil
}
