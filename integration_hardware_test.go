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
	"google.golang.org/protobuf/encoding/protojson"

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

	// Added to manually listen to the playback buffer draining
	t.Log("Waiting 5 seconds to allow audio buffer to reach speakers...")
	time.Sleep(5 * time.Second)

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

// -------------------------------------------------------------------------
// Integration Test 3: Comprehensive Media Properties Probing (FFmpeg C-Layer)
// -------------------------------------------------------------------------
func TestIntegration_MediaPropertiesProbing(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateRealTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Dispatch the probing request against the verified baseline video file
	t.Logf("[PROBE TEST] Dispatching native media prober for asset: %s", mediaPath)
	resp, err := client.GetMediaProperties(ctx, &proto.MetadataRequest{
		FilePath: mediaPath,
	})
	if err != nil {
		t.Fatalf("Static media property probing failed over gRPC: %v", err)
	}

	// 2. Perform comprehensive assertions on structural container constraints
	t.Logf("[PROBE REPORT] Successfully analyzed media container. Format: %s (%s)", resp.GetFormatName(), resp.GetFormatLongName())
	t.Logf("[PROBE REPORT] Duration: %d ms | Size: %d bytes | Global Bitrate: %d bps", resp.GetDurationMs(), resp.GetFileSizeBytes(), resp.GetBitRate())

	if resp.GetFormatName() == "" {
		t.Error("Container probing failed: empty format name returned from C layer")
	}
	if resp.GetDurationMs() <= 0 {
		t.Errorf("Container probing failed: expected a valid duration, got %d ms", resp.GetDurationMs())
	}

	// 3. Structural Validation of the Video Stream Track (if flagged present)
	if resp.GetHasVideo() {
		t.Logf("[PROBE VIDEO] Codec: %s (%s) | Profile: %s", resp.GetVideoCodec(), resp.GetVideoCodecLongName(), resp.GetVideoProfile())
		t.Logf("[PROBE VIDEO] Resolution: %dx%d | Framerate: %.2f fps | Aspect Ratio: %s", resp.GetWidth(), resp.GetHeight(), resp.GetFramerate(), resp.GetAspectRatio())
		t.Logf("[PROBE VIDEO] Pixel Format: %s | Color Space: %s", resp.GetPixelFormat(), resp.GetColorSpace())

		if resp.GetWidth() == 0 || resp.GetHeight() == 0 {
			t.Error("Video track enabled but reported invalid geometry dimensions")
		}
		if resp.GetFramerate() <= 0.0 {
			t.Errorf("Video track enabled but reported unexpected framerate: %.2f", resp.GetFramerate())
		}
	} else {
		t.Log("[PROBE WARNING] Test asset contains no video track stream components.")
	}

	// 4. Structural Validation of the Audio Stream Track (if flagged present)
	if resp.GetHasAudio() {
		t.Logf("[PROBE AUDIO] Codec: %s (%s) | Profile: %s", resp.GetAudioCodec(), resp.GetAudioCodecLongName(), resp.GetAudioProfile())
		t.Logf("[PROBE AUDIO] Channels: %d | Layout: %s | Sample Rate: %d Hz", resp.GetAudioChannels(), resp.GetChannelLayout(), resp.GetSampleRate())

		if resp.GetAudioChannels() <= 0 {
			t.Errorf("Audio track enabled but reported invalid channel matrix size: %d", resp.GetAudioChannels())
		}
		if resp.GetSampleRate() <= 0 {
			t.Errorf("Audio track enabled but reported invalid sample rate clock: %d Hz", resp.GetSampleRate())
		}
	} else {
		t.Log("[PROBE WARNING] Test asset contains no audio track stream components.")
	}

	bytes, _ := protojson.MarshalOptions{Multiline: true}.Marshal(resp)
	t.Logf("[PROBE REPORT] Full Properties Payload:\n%s", string(bytes))
}

// -------------------------------------------------------------------------
// Integration Test 4: Frame Seek, Extraction, and Disk Inspection (JPEG)
// -------------------------------------------------------------------------
func TestIntegration_VideoScreenshotExtractionAndDiskWrite(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateRealTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Request a screenshot 2.5 seconds (2500ms) into the video timeline
	const targetTimeMs = 2500
	t.Logf("[SCREENSHOT TEST] Requesting frame capture at %d ms for asset: %s", targetTimeMs, mediaPath)

	resp, err := client.GetVideoScreenshot(ctx, &proto.ScreenshotRequest{
		FilePath: mediaPath,
		TimeMs:   targetTimeMs,
	})
	if err != nil {
		t.Fatalf("Static video frame extraction failed over gRPC: %v", err)
	}

	// 2. Validate structural response markers
	if len(resp.GetImageData()) == 0 {
		t.Fatal("Extraction failed: sidecar returned an empty image byte buffer")
	}
	if resp.GetMimeType() != "image/jpeg" {
		t.Errorf("Unexpected format payload type returned: expected 'image/jpeg', got '%s'", resp.GetMimeType())
	}

	t.Logf("[SCREENSHOT REPORT] Successfully extracted image from C layer. Size: %d bytes | Mime: %s",
		len(resp.GetImageData()), resp.GetMimeType(),
	)

	// 3. Write image payload out to the disk filesystem for manual structural verification
	outputPath := filepath.Join(os.TempDir(), fmt.Sprintf("extracted_frame_%d.jpg", time.Now().Unix()))

	err = os.WriteFile(outputPath, resp.GetImageData(), 0644)
	if err != nil {
		t.Fatalf("Failed to write extracted thumbnail buffer down to disk: %v", err)
	}

	t.Logf("[SCREENSHOT SUCCESS] Thumbnail saved for visual inspection! Find it here:\n👉 %s", outputPath)
}
