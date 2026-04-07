// Package clipping detects inter-sample and hard clipping in PCM audio.
package clipping

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/farcloser/primordium/fault"

	"github.com/farcloser/haustorium/internal/audit/shared"
	"github.com/farcloser/haustorium/internal/types"
)

const (
	max16 = 1<<15 - 1 // 32767
	min16 = -1 << 15  // -32768
	max24 = 1<<23 - 1 // 8388607
	min24 = -1 << 23  // -8388608
	max32 = 1<<31 - 1 // 2147483647
	min32 = -1 << 31  // -2147483648
)

// clipState tracks per-channel consecutive clipped samples during detection.
type clipState struct {
	result      *types.ClippingDetection
	consecutive []uint64
	numChannels int
	sampleIndex int
}

// Detect scans PCM data for consecutive clipped samples at the bit-depth ceiling.
func Detect(r io.Reader, format types.PCMFormat) (*types.ClippingDetection, error) {
	bytesPerSample := int(format.BitDepth / 8)         //nolint:gosec // bit depth and channel count are small constants
	frameSize := bytesPerSample * int(format.Channels) //nolint:gosec // bit depth and channel count are small constants
	buf := make([]byte, frameSize*4096)

	numChannels := int(format.Channels) //nolint:gosec // channel count is small
	state := &clipState{
		result: &types.ClippingDetection{
			Channels: make([]types.ChannelClipping, numChannels),
		},
		consecutive: make([]uint64, numChannels),
		numChannels: numChannels,
	}

	for {
		n, err := r.Read(buf)
		if n > 0 {
			completeSamples := (n / bytesPerSample) * bytesPerSample
			data := buf[:completeSamples]

			switch format.BitDepth {
			case types.Depth16:
				state.process16(data)
			case types.Depth24:
				state.process24(data)
			case types.Depth32:
				state.process32(data)
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

	// Flush trailing clips for all channels
	for channel := range numChannels {
		state.flushConsecutive(channel)
	}

	return state.result, nil
}

func (s *clipState) process16(data []byte) {
	for i := 0; i < len(data); i += 2 {
		channel := s.sampleIndex % s.numChannels
		sample := int16(binary.LittleEndian.Uint16(data[i:])) //nolint:gosec // PCM sample conversion
		s.result.Samples++
		s.sampleIndex++

		s.trackSample(channel, sample == max16 || sample == min16)
	}
}

func (s *clipState) process24(data []byte) {
	for i := 0; i < len(data); i += 3 {
		channel := s.sampleIndex % s.numChannels

		sample := int32(data[i]) | int32(data[i+1])<<shared.Shift8 | int32(data[i+2])<<16
		if sample&shared.Mask24Sign != 0 {
			sample |= ^shared.Mask24Extend
		}

		s.result.Samples++
		s.sampleIndex++

		s.trackSample(channel, sample == max24 || sample == min24)
	}
}

func (s *clipState) process32(data []byte) {
	for i := 0; i < len(data); i += 4 {
		channel := s.sampleIndex % s.numChannels
		sample := int32(binary.LittleEndian.Uint32(data[i:])) //nolint:gosec // PCM sample conversion
		s.result.Samples++
		s.sampleIndex++

		s.trackSample(channel, sample == max32 || sample == min32)
	}
}

// trackSample updates the consecutive clip counter for a channel and flushes when a non-clip is found.
func (s *clipState) trackSample(channel int, clipped bool) {
	if clipped {
		s.consecutive[channel]++

		return
	}

	s.flushConsecutive(channel)
	s.consecutive[channel] = 0
}

// flushConsecutive records a clipping event if the channel has accumulated enough consecutive clips.
func (s *clipState) flushConsecutive(channel int) {
	run := s.consecutive[channel]
	if run < 2 {
		return
	}

	s.result.Channels[channel].Events++
	s.result.Channels[channel].ClippedSamples += run

	if run > s.result.Channels[channel].LongestRun {
		s.result.Channels[channel].LongestRun = run
	}

	s.result.Events++
	s.result.ClippedSamples += run

	if run > s.result.LongestRun {
		s.result.LongestRun = run
	}
}
