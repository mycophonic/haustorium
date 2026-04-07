//nolint:wrapcheck // too dumb
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/farcloser/haustorium"
	"github.com/farcloser/haustorium/internal/types"
)

var (
	errInvalidArgCount = errors.New("expected exactly one argument: file path or \"-\" for stdin")
	errUnknownCheck    = errors.New("unknown check")
)

func analyzeCommand() *cli.Command {
	return &cli.Command{
		Name:      "analyze",
		Usage:     "Analyze raw PCM audio for quality issues",
		ArgsUsage: "<file | ->",
		Flags: []cli.Flag{
			// PCMFormat flags.
			&cli.IntFlag{
				Name:     "sample-rate",
				Aliases:  []string{"s"},
				Usage:    "Sample rate in Hz (e.g., 44100, 48000, 96000)",
				Required: true,
			},
			&cli.IntFlag{
				Name:    "bit-depth",
				Aliases: []string{"b"},
				Usage:   "Bit depth (16, 24, or 32)",
				Value:   bitDepth32,
			},
			&cli.IntFlag{
				Name:    "channels",
				Aliases: []string{"c"},
				Usage:   "Number of channels (1 = mono, 2 = stereo)",
				Value:   2,
			},
			&cli.IntFlag{
				Name:  "expected-bit-depth",
				Usage: "Expected bit depth for authenticity check (defaults to --bit-depth value)",
			},

			// Check selection.
			&cli.StringFlag{
				Name:    "checks",
				Aliases: []string{"C"},
				Usage:   "Comma-separated checks or presets: all, defects, loudness, clipping, truncation, fake-bit-depth, fake-sample-rate, lossy-transcode, dc-offset, fake-stereo, phase-issues, inverted-phase, channel-imbalance, silence-padding, hum, noise-floor, inter-sample-peaks, dynamic-range, dropouts",
				Value:   "all",
			},

			// Source type.
			&cli.StringFlag{
				Name:    "source",
				Aliases: []string{"S"},
				Usage:   "Audio source type adjusting detection thresholds: digital, vinyl, live",
				Value:   "digital",
			},

			// Output format.
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
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return fmt.Errorf("%w: got %d", errInvalidArgCount, cmd.NArg())
			}

			// Parse PCM format.
			format, err := parsePCMFormat(cmd)
			if err != nil {
				return err
			}

			// Parse checks.
			checks, err := parseChecks(cmd.String("checks"))
			if err != nil {
				return err
			}

			source, err := haustorium.ParseSource(cmd.String("source"))
			if err != nil {
				return err
			}

			opts := haustorium.OptionsForSource(source)
			opts.Checks = checks

			// Build reader factory.
			inputPath := cmd.Args().First()

			factory, cleanup, err := readerFactory(inputPath)
			if err != nil {
				return err
			}
			defer cleanup()

			// Run analysis.
			result, err := haustorium.Analyze(factory, format, opts)
			if err != nil {
				return fmt.Errorf("analysis failed: %w", err)
			}

			return outputResult(inputPath, result, cmd.String("format"), cmd.Bool("debug"))
		},
	}
}

func parsePCMFormat(cmd *cli.Command) (types.PCMFormat, error) {
	sampleRate := cmd.Int("sample-rate")
	rawBitDepth := cmd.Int("bit-depth")
	channels := cmd.Int("channels")
	expectedBitDepth := cmd.Int("expected-bit-depth")

	bitDepth, err := toBitDepth(rawBitDepth)
	if err != nil {
		return types.PCMFormat{}, fmt.Errorf("--bit-depth: %w", err)
	}

	ebd := bitDepth
	if expectedBitDepth > 0 {
		ebd, err = toBitDepth(expectedBitDepth)
		if err != nil {
			return types.PCMFormat{}, fmt.Errorf("--expected-bit-depth: %w", err)
		}
	}

	return types.PCMFormat{
		SampleRate:       sampleRate,
		BitDepth:         bitDepth,
		Channels:         uint(channels), //nolint:gosec // validated positive value
		ExpectedBitDepth: ebd,
	}, nil
}

var errInvalidBitDepth = errors.New("must be 16, 24, or 32")

const (
	bitDepth16 = 16
	bitDepth24 = 24
	bitDepth32 = 32
)

func toBitDepth(v int) (types.BitDepth, error) {
	switch v {
	case bitDepth16:
		return types.Depth16, nil
	case bitDepth24:
		return types.Depth24, nil
	case bitDepth32:
		return types.Depth32, nil
	default:
		return 0, errInvalidBitDepth
	}
}

//nolint:gochecknoglobals
var checkNames = map[string]haustorium.Check{
	"clipping":           haustorium.CheckClipping,
	"truncation":         haustorium.CheckTruncation,
	"fake-bit-depth":     haustorium.CheckFakeBitDepth,
	"fake-sample-rate":   haustorium.CheckFakeSampleRate,
	"lossy-transcode":    haustorium.CheckLossyTranscode,
	"dc-offset":          haustorium.CheckDCOffset,
	"fake-stereo":        haustorium.CheckFakeStereo,
	"phase-issues":       haustorium.CheckPhaseIssues,
	"inverted-phase":     haustorium.CheckInvertedPhase,
	"channel-imbalance":  haustorium.CheckChannelImbalance,
	"silence-padding":    haustorium.CheckSilencePadding,
	"hum":                haustorium.CheckHum,
	"noise-floor":        haustorium.CheckNoiseFloor,
	"inter-sample-peaks": haustorium.CheckInterSamplePeaks,
	"loudness":           haustorium.CheckLoudness,
	"dynamic-range":      haustorium.CheckDynamicRange,
	"dropouts":           haustorium.CheckDropouts,
	// Presets.
	"all":     haustorium.ChecksAll,
	"defects": haustorium.ChecksDefects,
}

func parseChecks(raw string) (haustorium.Check, error) {
	var result haustorium.Check

	for name := range strings.SplitSeq(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		check, ok := checkNames[name]
		if !ok {
			return 0, fmt.Errorf("%q: %w", name, errUnknownCheck)
		}

		result |= check
	}

	if result == 0 {
		return haustorium.ChecksAll, nil
	}

	return result, nil
}

// readerFactory returns a factory that produces fresh readers for multi-pass analysis.
// For files, it re-opens the file each time. For stdin, it buffers the entire input.
func readerFactory(source string) (haustorium.ReaderFactory, func(), error) {
	if source == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, func() {}, fmt.Errorf("reading stdin: %w", err)
		}

		factory := func() (io.Reader, error) {
			return bytes.NewReader(data), nil
		}

		return factory, func() {}, nil
	}

	// Verify the file exists upfront.
	if _, err := os.Stat(source); err != nil {
		return nil, func() {}, fmt.Errorf("cannot access %s: %w", source, err)
	}

	factory := func() (io.Reader, error) {
		return os.Open(source) //nolint:gosec // CLI tool opens user-specified audio files
	}

	return factory, func() {}, nil
}
