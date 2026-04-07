//nolint:wrapcheck
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/urfave/cli/v3"

	"github.com/farcloser/haustorium"
	"github.com/farcloser/haustorium/internal/integration/ffmpeg"
	"github.com/farcloser/haustorium/internal/integration/ffprobe"
	"github.com/farcloser/haustorium/internal/types"
)

var (
	errProcessArgs       = errors.New("expected exactly one argument: file path")
	errStreamNotFound    = errors.New("audio stream not found")
	errInvalidSampleRate = errors.New("invalid sample rate from probe")
	errInvalidChannels   = errors.New("invalid channel count from probe")
)

func processCommand() *cli.Command {
	return &cli.Command{
		Name:      "process",
		Usage:     "Extract PCM from an audio file and analyze for quality issues",
		ArgsUsage: "<file>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "checks",
				Aliases: []string{"C"},
				Usage:   "Comma-separated checks or presets: all, defects, loudness, clipping, truncation, fake-bit-depth, fake-sample-rate, lossy-transcode, dc-offset, fake-stereo, phase-issues, inverted-phase, channel-imbalance, silence-padding, hum, noise-floor, inter-sample-peaks, dynamic-range, dropouts",
				Value:   "all",
			},
			&cli.IntFlag{
				Name:  "stream",
				Usage: "Audio stream index (0-based)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "source",
				Aliases: []string{"S"},
				Usage:   "Audio source type adjusting detection thresholds: digital, vinyl, live",
				Value:   "digital",
			},
			&cli.StringFlag{
				Name:    "format",
				Aliases: []string{"f"},
				Usage:   "Output format: console, json, markdown",
				Value:   "console",
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"D"},
				Usage:   "Include all raw analyzer data in output",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return fmt.Errorf("%w: got %d", errProcessArgs, cmd.NArg())
			}

			filePath := cmd.Args().First()
			streamIndex := cmd.Int("stream")

			checks, err := parseChecks(cmd.String("checks"))
			if err != nil {
				return err
			}

			// Probe the file for audio properties.
			probeResult, err := ffprobe.Probe(ctx, filePath)
			if err != nil {
				return fmt.Errorf("probing file: %w", err)
			}

			stream, err := findAudioStream(probeResult, streamIndex)
			if err != nil {
				return err
			}

			format, err := buildPCMFormat(stream)
			if err != nil {
				return err
			}

			// Extract PCM (32-bit) from the file via ffmpeg.
			file, openErr := os.Open(filePath) //nolint:gosec // CLI tool opens user-specified audio files
			if openErr != nil {
				return fmt.Errorf("opening file: %w", openErr)
			}
			defer file.Close()

			var pcmBuf bytes.Buffer

			extractFormat := &types.PCMFormat{BitDepth: types.Depth32}

			if err = ffmpeg.ExtractStream(ctx, file, &pcmBuf, streamIndex, extractFormat); err != nil {
				return fmt.Errorf("extracting PCM: %w", err)
			}

			// Build reader factory from extracted PCM.
			pcmData := pcmBuf.Bytes()
			factory := func() (io.Reader, error) {
				return bytes.NewReader(pcmData), nil
			}

			// Run analysis.
			source, sourceErr := haustorium.ParseSource(cmd.String("source"))
			if sourceErr != nil {
				return sourceErr
			}

			opts := haustorium.OptionsForSource(source)
			opts.Checks = checks

			result, err := haustorium.Analyze(factory, format, opts)
			if err != nil {
				return fmt.Errorf("analysis failed: %w", err)
			}

			return outputResult(filePath, result, cmd.String("format"), cmd.Bool("debug"))
		},
	}
}

func findAudioStream(result *ffprobe.Result, streamIndex int) (*ffprobe.Stream, error) {
	audioCount := 0

	for i := range result.Streams {
		if result.Streams[i].CodecType == "audio" {
			if audioCount == streamIndex {
				return &result.Streams[i], nil
			}

			audioCount++
		}
	}

	return nil, fmt.Errorf(
		"audio stream index %d not found (file has %d audio streams): %w",
		streamIndex,
		audioCount,
		errStreamNotFound,
	)
}

func buildPCMFormat(stream *ffprobe.Stream) (types.PCMFormat, error) {
	sampleRate, err := strconv.Atoi(stream.SampleRate)
	if err != nil || sampleRate <= 0 {
		return types.PCMFormat{}, fmt.Errorf("%q: %w", stream.SampleRate, errInvalidSampleRate)
	}

	if stream.Channels <= 0 {
		return types.PCMFormat{}, fmt.Errorf("%d: %w", stream.Channels, errInvalidChannels)
	}

	return types.PCMFormat{
		SampleRate:       sampleRate,
		BitDepth:         types.Depth32,
		Channels:         uint(stream.Channels),
		ExpectedBitDepth: resolveExpectedBitDepth(stream),
	}, nil
}

// resolveExpectedBitDepth determines the original bit depth from ffprobe data.
// For lossless codecs (FLAC, ALAC), bits_per_raw_sample is most reliable.
// For PCM containers (WAV, AIFF), bits_per_sample is authoritative.
// For lossy codecs, no meaningful bit depth exists. Defaults to Depth32
// (matching extraction bit depth, which disables the fake-bit-depth check).
func resolveExpectedBitDepth(stream *ffprobe.Stream) types.BitDepth {
	if stream.BitsPerRawSample != "" {
		if bits, err := strconv.Atoi(stream.BitsPerRawSample); err == nil {
			if bd, err := toBitDepth(bits); err == nil {
				return bd
			}
		}
	}

	if stream.BitsPerSample > 0 {
		if bd, err := toBitDepth(stream.BitsPerSample); err == nil {
			return bd
		}
	}

	return types.Depth32
}
