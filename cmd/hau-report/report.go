//nolint:wrapcheck
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/farcloser/haustorium"
	"github.com/farcloser/haustorium/internal/integration/ffmpeg"
	"github.com/farcloser/haustorium/internal/integration/ffprobe"
	"github.com/farcloser/haustorium/internal/output"
	"github.com/farcloser/haustorium/internal/types"
)

const (
	outputFile = "haustorium-report.jsonl"

	secondsPerMinute     = 60
	microsecondsPerMilli = 1000.0

	errFmtQuotedWrap = "%q: %w"
	errFmtIntWrap    = "%d: %w"
)

var (
	errNotDirectory      = errors.New("not a directory")
	errNoAudioFiles      = errors.New("no .flac or .m4a files found")
	errNoAudioStream     = errors.New("no audio streams found")
	errInvalidSampleRate = errors.New("invalid sample rate")
	errInvalidChannels   = errors.New("invalid channel count")
	errInvalidBitDepth   = errors.New("must be 16, 24, or 32")
	errReportArgs        = errors.New("expected exactly one argument: folder path")
)

func reportCommand() *cli.Command {
	return &cli.Command{
		Name:      "report",
		Usage:     "Scan a music collection and write a haustorium JSONL report",
		ArgsUsage: "<folder>",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "redact-path",
				Usage: "Strip file paths from the report",
			},
			&cli.StringFlag{
				Name:    "source",
				Aliases: []string{"S"},
				Usage:   "Override audio source type for all files: digital, vinyl, live (default: auto-detect from path)",
			},
			&cli.IntFlag{
				Name:    "workers",
				Aliases: []string{"j"},
				Usage:   "Number of concurrent workers",
				Value:   runtime.NumCPU(),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return errReportArgs
			}

			folder := cmd.Args().First()
			redact := cmd.Bool("redact-path")
			sourceOverride := cmd.String("source")
			workers := cmd.Int("workers")

			workers = max(workers, 1)

			return runReport(ctx, folder, redact, sourceOverride, workers)
		},
	}
}

func runReport(
	ctx context.Context,
	folder string,
	redact bool,
	sourceOverride string,
	workers int,
) error { //nolint:flag-parameter // redact is a simple toggle from CLI flag
	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		return fmt.Errorf(errFmtQuotedWrap, folder, errNotDirectory)
	}

	// Collect audio files.
	files, err := collectAudioFiles(folder)
	if err != nil {
		return fmt.Errorf("scanning folder: %w", err)
	}

	if len(files) == 0 {
		return fmt.Errorf(errFmtQuotedWrap, folder, errNoAudioFiles)
	}

	fmt.Fprintf(os.Stderr, "Found %d files to analyze (%d workers)\n", len(files), workers)

	// Process files concurrently.
	startTime := time.Now()
	results := make([]Record, len(files))

	var progress atomic.Int64

	sem := make(chan struct{}, workers)

	var waitGroup sync.WaitGroup

	for idx, filePath := range files {
		waitGroup.Add(1)

		go func(idx int, filePath string) {
			defer waitGroup.Done()

			sem <- struct{}{}

			defer func() { <-sem }()

			results[idx] = processFile(ctx, filePath, sourceOverride)

			done := progress.Add(1)
			fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", done, len(files), filePath)
		}(idx, filePath)
	}

	waitGroup.Wait()

	// Write results in file order.
	out, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer out.Close()

	enc := json.NewEncoder(out)
	failed := 0

	var totalProbe, totalDecode, totalAnalyze time.Duration

	for idx := range results {
		record := &results[idx]

		if record.Error != "" {
			failed++
		}

		if record.Timing != nil {
			totalProbe += millisToDuration(record.Timing.ProbeMs)
			totalDecode += millisToDuration(record.Timing.DecodeMs)
			totalAnalyze += millisToDuration(record.Timing.AnalyzeMs)
		}

		if redact {
			record.File = ""
			record.Probe = redactProbe(record.Probe)
		}

		if err := enc.Encode(record); err != nil {
			slog.Error("writing record", "file", files[idx], "error", err)
		}
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("closing output file: %w", err)
	}

	// Compress.
	if err := compressFile(outputFile); err != nil {
		slog.Error("compressing report", "error", err)
	}

	elapsed := time.Since(startTime)
	minutes := int(elapsed.Minutes())
	seconds := int(elapsed.Seconds()) % secondsPerMinute

	fmt.Fprintf(os.Stderr, "\nDone: %d files in %dm %ds (%d failed)\n", len(files), minutes, seconds, failed)
	fmt.Fprintf(os.Stderr, "Report written to %s (and %s.gz)\n", outputFile, outputFile)

	// Timing breakdown.
	analyzed := len(files) - failed

	fmt.Fprint(os.Stderr, "\n--- Timing ---\n")
	fmt.Fprintf(os.Stderr, "  Wall clock:  %s\n", elapsed.Truncate(time.Millisecond))
	fmt.Fprintf(os.Stderr, "  ffprobe:     %s (cumulative)\n", totalProbe.Truncate(time.Millisecond))
	fmt.Fprintf(os.Stderr, "  ffmpeg:      %s (cumulative)\n", totalDecode.Truncate(time.Millisecond))
	fmt.Fprintf(os.Stderr, "  analysis:    %s (cumulative)\n", totalAnalyze.Truncate(time.Millisecond))

	if analyzed > 0 {
		fmt.Fprintf(os.Stderr, "  avg/file:    %s (probe: %s, decode: %s, analyze: %s)\n",
			(totalProbe+totalDecode+totalAnalyze)/time.Duration(analyzed),
			totalProbe/time.Duration(analyzed),
			totalDecode/time.Duration(analyzed),
			totalAnalyze/time.Duration(analyzed),
		)
	}

	// Print digest summary.
	fmt.Fprintln(os.Stderr)

	return runDigest(outputFile, "")
}

func processFile(ctx context.Context, filePath, sourceOverride string) Record {
	fileStart := time.Now()
	timing := &RecordTiming{}

	// Determine source type.
	source, err := detectSource(filePath, sourceOverride)
	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("invalid source: %v", err)}
	}

	// Probe.
	probeStart := time.Now()

	probeResult, err := ffprobe.Probe(ctx, filePath)

	timing.ProbeMs = durationMs(time.Since(probeStart))

	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("probe failed: %v", err), Timing: timing}
	}

	// Find first audio stream.
	stream, err := findAudioStream(probeResult)
	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("no audio stream: %v", err), Timing: timing}
	}

	// Build PCM format.
	pcmFormat, err := buildPCMFormat(stream)
	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("format error: %v", err), Timing: timing}
	}

	// Extract PCM.
	decodeStart := time.Now()

	file, err := os.Open(filePath) //nolint:gosec // CLI tool opens user-specified audio files
	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("open failed: %v", err), Timing: timing}
	}
	defer file.Close()

	var pcmBuf bytes.Buffer

	extractFormat := &types.PCMFormat{BitDepth: types.Depth32}

	if err = ffmpeg.ExtractStream(ctx, file, &pcmBuf, 0, extractFormat); err != nil {
		timing.DecodeMs = durationMs(time.Since(decodeStart))

		return Record{File: filePath, Error: fmt.Sprintf("extraction failed: %v", err), Timing: timing}
	}

	timing.DecodeMs = durationMs(time.Since(decodeStart))

	// Build reader factory.
	pcmData := pcmBuf.Bytes()
	factory := func() (io.Reader, error) {
		return bytes.NewReader(pcmData), nil
	}

	// Run analysis.
	analyzeStart := time.Now()

	opts := haustorium.OptionsForSource(source)
	opts.Checks = haustorium.ChecksAll

	result, err := haustorium.Analyze(factory, pcmFormat, opts)

	timing.AnalyzeMs = durationMs(time.Since(analyzeStart))
	timing.TotalMs = durationMs(time.Since(fileStart))

	if err != nil {
		return Record{File: filePath, Error: fmt.Sprintf("analysis failed: %v", err), Timing: timing}
	}

	// Build record.
	record := Record{
		File:     filePath,
		Analysis: output.ResultToMap(result),
		Timing:   timing,
	}

	// Serialize probe data (strips tags/disposition since Go structs don't include them).
	probeJSON, err := json.Marshal(probeResult)
	if err == nil {
		record.Probe = probeJSON
	} else {
		record.ProbeError = "probe serialization failed"
	}

	return record
}

func durationMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / microsecondsPerMilli
}

func millisToDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

func detectSource(filePath, sourceOverride string) (haustorium.Source, error) {
	if sourceOverride != "" {
		return haustorium.ParseSource(sourceOverride)
	}

	dir := filepath.Dir(filePath)
	lower := strings.ToLower(dir)

	if strings.Contains(lower, "vinyl") {
		return haustorium.SourceVinyl, nil
	}

	return haustorium.SourceDigital, nil
}

func findAudioStream(result *ffprobe.Result) (*ffprobe.Stream, error) {
	for i := range result.Streams {
		if result.Streams[i].CodecType == "audio" {
			return &result.Streams[i], nil
		}
	}

	return nil, errNoAudioStream
}

func buildPCMFormat(stream *ffprobe.Stream) (types.PCMFormat, error) {
	sampleRate, err := strconv.Atoi(stream.SampleRate)
	if err != nil || sampleRate <= 0 {
		return types.PCMFormat{}, fmt.Errorf(errFmtQuotedWrap, stream.SampleRate, errInvalidSampleRate)
	}

	if stream.Channels <= 0 {
		return types.PCMFormat{}, fmt.Errorf(errFmtIntWrap, stream.Channels, errInvalidChannels)
	}

	return types.PCMFormat{
		SampleRate:       sampleRate,
		BitDepth:         types.Depth32,
		Channels:         uint(stream.Channels),
		ExpectedBitDepth: resolveExpectedBitDepth(stream),
	}, nil
}

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

const (
	bitDepth16 = 16
	bitDepth24 = 24
	bitDepth32 = 32
)

func toBitDepth(bits int) (types.BitDepth, error) {
	switch bits {
	case bitDepth16:
		return types.Depth16, nil
	case bitDepth24:
		return types.Depth24, nil
	case bitDepth32:
		return types.Depth32, nil
	default:
		return 0, fmt.Errorf(errFmtIntWrap, bits, errInvalidBitDepth)
	}
}

func collectAudioFiles(root string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".flac" || ext == ".m4a" {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.Sort(files)

	return files, nil
}

func compressFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // reading our own output file
	if err != nil {
		return err
	}

	gzFile, err := os.Create(path + ".gz") //nolint:gosec // path is constructed from our own output file constant
	if err != nil {
		return err
	}
	defer gzFile.Close()

	gzWriter := gzip.NewWriter(gzFile)

	if _, err := gzWriter.Write(data); err != nil {
		return err
	}

	return gzWriter.Close()
}

func redactProbe(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}

	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return raw
	}

	// Strip format.filename.
	if format, ok := probe["format"].(map[string]any); ok {
		delete(format, "filename")
	}

	redacted, err := json.Marshal(probe)
	if err != nil {
		return raw
	}

	return redacted
}
