package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func conversionTestRequest(source, output string) *proto.VideoConversionRequest {
	return &proto.VideoConversionRequest{
		JobId: "integration-test", SourcePath: source, OutputPath: output,
		VideoBitrateKbps: 1200, AudioBitrateKbps: 96,
		EncoderPreset: "ultrafast", HdrHandling: "tone_map",
	}
}

func TestIntegration_ConvertVideoThroughRPC(t *testing.T) {
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()

	source, cleanup := generateRealTestMedia(t)
	defer cleanup()
	output := filepath.Join(t.TempDir(), "prepared.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stream, err := client.ConvertVideo(ctx, conversionTestRequest(source, output))
	if err != nil {
		t.Fatalf("ConvertVideo RPC failed to start: %v", err)
	}
	lastProgress := -1.0
	completed := false
	progressCount := 0
	encoderName := ""
	hardwareAccelerated := false
	for {
		update, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("ConvertVideo RPC failed: %v", recvErr)
		}
		if math.IsNaN(update.GetProgress()) || math.IsInf(update.GetProgress(), 0) || update.GetProgress() < 0 || update.GetProgress() > 100 {
			t.Fatalf("invalid conversion progress: %v", update.GetProgress())
		}
		if update.GetProgress()+0.001 < lastProgress {
			t.Fatalf("conversion progress moved backward: %v after %v", update.GetProgress(), lastProgress)
		}
		lastProgress = update.GetProgress()
		progressCount++
		if update.GetEncoderName() != "" {
			encoderName = update.GetEncoderName()
			hardwareAccelerated = update.GetHardwareAccelerated()
		}
		completed = completed || update.GetCompleted()
	}
	if progressCount < 2 {
		t.Fatalf("expected progress and completion updates, got %d update(s)", progressCount)
	}
	if !completed {
		t.Fatal("conversion stream ended without a completed update")
	}
	if encoderName == "" {
		t.Fatal("conversion did not report the selected H.264 encoder")
	}
	t.Logf("conversion encoder=%s hardware_accelerated=%t", encoderName, hardwareAccelerated)
	if hardwareAccelerated && encoderName == "libx264" {
		t.Fatal("conversion reported CPU x264 as hardware accelerated")
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("converted MP4 was not created: %v", err)
	}
	if info.Size() < 1024 {
		t.Fatalf("converted MP4 is unexpectedly small: %d bytes", info.Size())
	}
	metadata, err := client.GetMediaProperties(ctx, &proto.MetadataRequest{FilePath: output})
	if err != nil {
		t.Fatalf("sidecar could not probe converted MP4: %v", err)
	}
	if !metadata.GetHasVideo() || metadata.GetVideoCodec() != "h264" || metadata.GetAudioCodec() != "aac" {
		t.Fatalf("unexpected output codecs: video=%q audio=%q", metadata.GetVideoCodec(), metadata.GetAudioCodec())
	}
	if metadata.GetDurationMs() <= 0 {
		t.Fatalf("converted output has no duration: %d", metadata.GetDurationMs())
	}
}

func TestIntegration_ConvertVideoRetryReselectsHardwareEncoder(t *testing.T) {
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()
	source, cleanup := generateRealTestMedia(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var firstEncoder string
	var firstHardware bool
	for attempt := 1; attempt <= 2; attempt++ {
		request := conversionTestRequest(source, filepath.Join(t.TempDir(), fmt.Sprintf("retry-%d.mp4", attempt)))
		request.JobId = fmt.Sprintf("integration-retry-%d", attempt)
		stream, err := client.ConvertVideo(ctx, request)
		if err != nil {
			t.Fatalf("conversion attempt %d failed to start: %v", attempt, err)
		}
		encoderName := ""
		hardwareAccelerated := false
		completed := false
		for {
			update, recvErr := stream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				t.Fatalf("conversion attempt %d failed: %v", attempt, recvErr)
			}
			if update.GetEncoderName() != "" {
				encoderName = update.GetEncoderName()
				hardwareAccelerated = update.GetHardwareAccelerated()
			}
			completed = completed || update.GetCompleted()
		}
		if !completed || encoderName == "" {
			t.Fatalf("attempt %d did not complete with a reported encoder: completed=%t encoder=%q", attempt, completed, encoderName)
		}
		if attempt == 1 {
			firstEncoder, firstHardware = encoderName, hardwareAccelerated
			continue
		}
		if encoderName != firstEncoder || hardwareAccelerated != firstHardware {
			t.Fatalf("retry changed encoder selection: first=%q hardware=%t retry=%q hardware=%t",
				firstEncoder, firstHardware, encoderName, hardwareAccelerated)
		}
		t.Logf("retry reused encoder=%s hardware_accelerated=%t", encoderName, hardwareAccelerated)
	}
}

func TestIntegration_ConvertVideoRejectsInvalidRequests(t *testing.T) {
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.ConvertVideo(ctx, &proto.VideoConversionRequest{})
	if err != nil {
		t.Fatalf("invalid request should fail through the RPC stream: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty request, got %v", err)
	}

	request := conversionTestRequest(filepath.Join(t.TempDir(), "missing.mkv"), filepath.Join(t.TempDir(), "out.mp4"))
	stream, err = client.ConvertVideo(ctx, request)
	if err != nil {
		t.Fatalf("missing-source request should start its RPC stream: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for missing source, got %v", err)
	}
}

func TestIntegration_ConvertVideoFallsBackToCPU(t *testing.T) {
	t.Setenv("GOTEDO_TEST_MODE", "1")
	t.Setenv("GOTEDO_FFMPEG_FORCE_CPU_ENCODER", "1")
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()
	source, cleanup := generateRealTestMedia(t)
	defer cleanup()
	output := filepath.Join(t.TempDir(), "prepared-cpu.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stream, err := client.ConvertVideo(ctx, conversionTestRequest(source, output))
	if err != nil {
		t.Fatalf("ConvertVideo RPC failed to start: %v", err)
	}
	completed := false
	encoderName := ""
	hardwareAccelerated := true
	for {
		update, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("CPU fallback conversion failed: %v", recvErr)
		}
		if update.GetEncoderName() != "" {
			encoderName = update.GetEncoderName()
			hardwareAccelerated = update.GetHardwareAccelerated()
		}
		completed = completed || update.GetCompleted()
	}
	t.Logf("conversion encoder=%s hardware_accelerated=%t", encoderName, hardwareAccelerated)
	if !completed || encoderName != "libx264" || hardwareAccelerated {
		t.Fatalf("expected completed CPU x264 conversion, got completed=%t encoder=%q hardware=%t", completed, encoderName, hardwareAccelerated)
	}
}

func TestIntegration_ConvertHEVCHDRVideoThroughRPC(t *testing.T) {
	if os.Getenv("FFMPEG_CONVERSION_E2E") != "1" {
		t.Skip("set FFMPEG_CONVERSION_E2E=1 to run the full checked-in HEVC HDR conversion")
	}
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()
	source, cleanup := generateExodusHDRTestMedia(t)
	defer cleanup()
	output := filepath.Join(t.TempDir(), "hdr-tonemapped.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	stream, err := client.ConvertVideo(ctx, conversionTestRequest(source, output))
	if err != nil {
		t.Fatalf("ConvertVideo RPC failed to start: %v", err)
	}
	completed := false
	for {
		update, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("HEVC HDR conversion failed: %v", recvErr)
		}
		completed = completed || update.GetCompleted()
	}
	if !completed {
		t.Fatal("HEVC HDR conversion ended without a completed update")
	}
	metadata, err := client.GetMediaProperties(ctx, &proto.MetadataRequest{FilePath: output})
	if err != nil {
		t.Fatalf("could not probe converted HDR output: %v", err)
	}
	if metadata.GetVideoCodec() != "h264" || metadata.GetColorTransfer() == "smpte2084" || metadata.GetColorTransfer() == "arib-std-b67" {
		t.Fatalf("HDR tone mapping did not produce SDR H.264: codec=%q transfer=%q", metadata.GetVideoCodec(), metadata.GetColorTransfer())
	}
}

func TestIntegration_CancelLongHEVCConversion(t *testing.T) {
	if os.Getenv("FFMPEG_CONVERSION_E2E") != "1" {
		t.Skip("set FFMPEG_CONVERSION_E2E=1 to run conversion cancellation against the checked-in HEVC sample")
	}
	client, _, closeServer := setupRealHardwareTestServer(t)
	defer closeServer()
	source, cleanup := generateExodusHDRTestMedia(t)
	defer cleanup()
	output := filepath.Join(t.TempDir(), "cancelled.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.ConvertVideo(ctx, conversionTestRequest(source, output))
	if err != nil {
		t.Fatalf("ConvertVideo RPC failed to start: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		cancel()
		t.Fatalf("conversion did not announce its initial progress before cancellation: %v", err)
	}
	cancel()
	_, err = stream.Recv()
	if status.Code(err) != codes.Canceled {
		t.Fatalf("expected cancellation to stop the native conversion loop, got %v", err)
	}
	cleanupDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(output); os.IsNotExist(statErr) {
			break
		} else if statErr != nil {
			t.Fatalf("could not inspect cancelled conversion output: %v", statErr)
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatalf("cancelled conversion unexpectedly left final output at %s", output)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
