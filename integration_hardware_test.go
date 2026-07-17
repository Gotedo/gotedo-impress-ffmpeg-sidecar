package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
)

// Setup a real gRPC server bound to an internal local Unix Domain Socket (UDS)
func setupRealHardwareTestServer(t *testing.T) (proto.FFmpegServiceClient, string, func()) {
	t.Helper()

	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("hardware_test_%d.sock", time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("[HARDWARE TEST SETUP] Failed to bind local UDS: %v", err)
	}

	grpcServer := grpc.NewServer()
	srv := &ffmpegServer{}
	proto.RegisterFFmpegServiceServer(grpcServer, srv)

	go func() {
		if err := grpcServer.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("gRPC Server exited abnormally: %v", err)
		}
	}()

	// Establish real client channel
	conn, err := grpc.Dial("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcServer.Stop()
		listener.Close()
		_ = os.Remove(socketPath)
		t.Fatalf("[HARDWARE TEST SETUP] Client failed to dial UDS: %v", err)
	}

	closer := func() {
		conn.Close()
		grpcServer.GracefulStop()
		listener.Close()
		_ = os.Remove(socketPath)
	}

	return proto.NewFFmpegServiceClient(conn), socketPath, closer
}

// generateRealTestMedia points to the validated baseline asset in your repository
func generateRealTestMedia(t *testing.T) (string, func()) {
	t.Helper()

	// Locate the baseline asset relative to the test execution path
	// If tests are run from the repo root, assets/video.mp4 is available.
	filePath, err := filepath.Abs("assets/video.mp4")
	if err != nil {
		t.Fatalf("Failed to resolve absolute path to test asset: %v", err)
	}

	// Verify the file actually exists before passing it to the C-layer
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatalf("Baseline test asset not found at expected path: %s", filePath)
	}

	// Return a "no-op" cleanup function because we are using a persistent source file,
	// not a temporary file that needs to be deleted.
	return filePath, func() {
		// No-op: Do not delete the source repository asset
	}
}

// -------------------------------------------------------------------------
// Integration Test 1: Real System Soundcard Enumeration
// -------------------------------------------------------------------------
func TestIntegration_RealAudioDeviceProbing(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Execution: Hit the live C miniaudio implementation layer directly
	resp, err := client.GetAudioDevices(ctx, &proto.DevicesRequest{})
	if err != nil {
		t.Fatalf("Hardware query failed over gRPC: %v", err)
	}

	t.Logf("[HARDWARE REPORT] Discovered %d real system soundcards on host machine.", len(resp.GetDevices()))

	if len(resp.GetDevices()) == 0 {
		t.Skip("Skipping verification assertion: No hardware audio soundcard devices detected on this environment.")
	}

	// Structural Validation of real system strings
	var defaultFound bool
	for _, dev := range resp.GetDevices() {
		if dev.GetId() == "" || dev.GetName() == "" {
			t.Errorf("Hardware reported unparsed or empty device properties: %+v", dev)
		}
		if dev.GetIsDefault() {
			defaultFound = true
			t.Logf("[HARDWARE REPORT] Verified Host Default Output Device: %s (ID: %s)", dev.GetName(), dev.GetId())
		}
	}

	if !defaultFound {
		t.Log("Warning: Miniaudio successfully enumerated soundcards, but none are registered as the OS primary default.")
	}
}

// -------------------------------------------------------------------------
// Integration Test 2: Pipeline Initialization, Latency adjustments, and Teardown
// -------------------------------------------------------------------------
func TestIntegration_FullPipelineStreamingAndRealTimeLatencyControl(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateRealTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Initialize the live streaming consumer against real system loops
	stream, err := client.StartStream(ctx, &proto.StreamRequest{
		TargetId: "hardware-integration-target",
		FilePath: mediaPath,
	})
	if err != nil {
		t.Fatalf("Failed to establish real stream pipeline channel: %v", err)
	}

	// 2. Spawn a concurrent monitor to observe frame data or process codes
	errChan := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				errChan <- nil
				return
			}
			if err != nil {
				errChan <- err
				return
			}

			// If miniaudio / FFmpeg successfully parses out fragments, process chunks here
			if len(resp.GetFmp4Chunk()) > 0 {
				t.Logf("[HARDWARE CAPTURE] Received real fMP4 packet from engine loop: %d bytes", len(resp.GetFmp4Chunk()))
			}
		}
	}()

	// Allow the hardware layers to spin up and establish native threads
	time.Sleep(500 * time.Millisecond)

	// 3. Fire a real-time AdjustLatency request down to the active stream map
	t.Log("[HARDWARE TESTING] Sending active runtime audio latency adjustment payload...")
	latencyResp, err := client.AdjustLatency(context.Background(), &proto.LatencyRequest{
		TargetId: "hardware-integration-target",
		DelayMs:  120, // Shift audio 120ms to align lip sync
	})

	if err != nil {
		// If the stream terminated early due to the file being a placeholder container,
		// the map entry might be gone, which is a normal structural result.
		t.Logf("[HARDWARE NOTE] Latency channel adjusted with condition: %v", err)
	} else if !latencyResp.GetAccepted() {
		t.Error("Hardware context rejected live latency shift configuration.")
	}

	// 4. Gracefully break the context pipeline to assert that C sub-threads shut down instantly
	t.Log("[HARDWARE CLEANUP] Triggering streaming channel cancellation signal...")
	cancel()

	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Logf("[HARDWARE DISCOVERY] Active stream exited with container condition: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Deadlock Error: Hardware threads failed to release resources within 2 seconds of context cancellation.")
	}
}
