package main

import (
	"context"
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
		completed = completed || update.GetCompleted()
	}
	if progressCount < 2 {
		t.Fatalf("expected progress and completion updates, got %d update(s)", progressCount)
	}
	if !completed {
		t.Fatal("conversion stream ended without a completed update")
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
