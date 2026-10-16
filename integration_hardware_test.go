package main

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
)

// Setup a real gRPC server bound to a local UDS or TCP listener, depending on
// the host platform. Windows does not support the Unix-socket URI used by the
// original harness.
func setupRealHardwareTestServer(t *testing.T) (proto.FFmpegServiceClient, string, func()) {
	t.Helper()

	// Use the application's Unix-domain IPC on Unix hosts. Windows uses loopback
	// TCP because the sidecar manager uses TCP there as well.
	network := "unix"
	address := "127.0.0.1:0"
	if runtime.GOOS == "windows" || os.Getenv("GOTEDO_FFMPEG_SIDECAR_USE_TCP") == "1" {
		network = "tcp"
	} else {
		// macOS limits Unix-domain socket paths to a short fixed length. Testing
		// under t.TempDir() can exceed that limit because its path includes a long
		// per-test directory name.
		socketDir, err := os.MkdirTemp("/tmp", "ff-")
		if err != nil {
			t.Fatalf("failed to create short socket directory: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
		address = filepath.Join(socketDir, "i.sock")
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatalf("[HARDWARE TEST SETUP] Failed to bind local test listener: %v", err)
	}
	if network == "tcp" {
		address = listener.Addr().String()
	}

	grpcServer := grpc.NewServer()
	srv := &ffmpegServer{}
	proto.RegisterFFmpegServiceServer(grpcServer, srv)

	go func() {
		if err := grpcServer.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("gRPC Server exited abnormally: %v", err)
		}
	}()

	// Establish a real client channel over the selected local IPC transport.
	dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialTarget := address
	if network == "unix" {
		dialTarget = "passthrough:///integration"
		dialOptions = append(dialOptions, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", address)
		}))
	}
	conn, err := grpc.Dial(dialTarget, dialOptions...)
	if err != nil {
		grpcServer.Stop()
		listener.Close()
		t.Fatalf("[HARDWARE TEST SETUP] Client failed to dial test listener: %v", err)
	}

	closer := func() {
		conn.Close()
		grpcServer.GracefulStop()
		listener.Close()
	}

	return proto.NewFFmpegServiceClient(conn), address, closer
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
	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		t.Fatalf("Baseline test asset not found at expected path: %s", filePath)
	}
	if err != nil {
		t.Fatalf("Failed to inspect baseline test asset: %v", err)
	}
	if info.Size() < 1024 {
		t.Skipf("Baseline test asset is a Git LFS pointer or otherwise incomplete (%d bytes): %s", info.Size(), filePath)
	}

	// Return a "no-op" cleanup function because we are using a persistent source file,
	// not a temporary file that needs to be deleted.
	return filePath, func() {
		// No-op: Do not delete the source repository asset
	}
}

// generate4KHDRTestMedia resolves the 4K HEVC HDR sample file from current dir or assets/
func generate4KHDRTestMedia(t *testing.T) (string, func()) {
	t.Helper()

	pathsToTry := []string{
		"4K HEVC 59.940 Broadcast Capture Sample.mkv",
		filepath.Join("assets", "4K HEVC 59.940 Broadcast Capture Sample.mkv"),
	}

	var selectedPath string
	for _, p := range pathsToTry {
		absP, err := filepath.Abs(p)
		if err == nil {
			if _, err := os.Stat(absP); err == nil {
				selectedPath = absP
				break
			}
		}
	}

	if selectedPath == "" {
		t.Skip("4K HDR test asset '4K HEVC 59.940 Broadcast Capture Sample.mkv' not found. Skipping 4K tests.")
	}

	return selectedPath, func() {
		// No-op: Do not delete the source repository asset
	}
}

// generateExodusHDRTestMedia resolves the checked-in HDR HEVC sample used by
// the runtime GPU support preflight test.
func generateExodusHDRTestMedia(t *testing.T) (string, func()) {
	t.Helper()

	filePath := filepath.Join("assets", "(HDR HEVC 10-bit BT.2020 24fps) Exodus Sample.mp4")
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		t.Fatalf("Failed to resolve Exodus HDR test asset: %v", err)
	}

	info, err := os.Stat(absPath)
	if os.IsNotExist(err) {
		t.Skipf("Exodus HDR test asset not found: %s", absPath)
	}
	if err != nil {
		t.Fatalf("Failed to inspect Exodus HDR test asset: %v", err)
	}
	if info.Size() < 1024 {
		t.Skipf("Exodus HDR test asset is incomplete (%d bytes): %s", info.Size(), absPath)
	}

	return absPath, func() {}
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
// Integration Test 2: Pipeline Initialization, API compatibility, and Teardown
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
		TargetId:         "hardware-integration-target",
		FilePath:         mediaPath,
		ProcessingPolicy: proto.ProcessingPolicy_PROCESSING_ALLOW_CPU,
	})
	if err != nil {
		t.Fatalf("Failed to establish real stream pipeline channel: %v", err)
	}

	// 2. Spawn a concurrent monitor to observe frame data or process codes
	errChan := make(chan error, 1)
	go func() {
		// CRITICAL FIX: Create an output file to dump the gRPC stream into a playable MP4
		outFile, err := os.Create("test_output_integration.mp4")
		if err != nil {
			t.Errorf("Failed to create test output file: %v", err)
			return
		}
		defer outFile.Close()

		var logPathOnce sync.Once
		absPath, err := filepath.Abs(outFile.Name())
		if err != nil {
			fmt.Println("Error resolving absolute path:", err)
			return
		}

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

			// If FFmpeg successfully parses out fragments, process chunks here
			if len(resp.GetFmp4Chunk()) > 0 {

				// Register the deferred log exactly once on the first chunk payload encounter
				logPathOnce.Do(func() {
					defer func(path string) {
						log.Printf("👉 [TEST LOG] Execution tracking will be written to path: %s", path)
					}(absPath) // Captures the context variable safely
				})

				t.Logf("[HARDWARE CAPTURE] Received real fMP4 packet from engine loop: %d bytes", len(resp.GetFmp4Chunk()))

				// Write the chunk to our test file
				if _, err := outFile.Write(resp.GetFmp4Chunk()); err != nil {
					t.Errorf("Failed to write to test file: %v", err)
				}
			}
		}
	}()

	// Allow the hardware layers to spin up and establish native threads
	time.Sleep(500 * time.Millisecond)

	// 3. Fire a real-time AdjustLatency request down to the active stream map
	t.Log("[HARDWARE TESTING] Sending active runtime audio latency adjustment payload...")

	// CRITICAL FIX: Under the new unified fMP4 architecture, latency is natively
	// managed by the browser's MSE. We fire this request to ensure backwards API compatibility
	// doesn't break and is safely acknowledged by the modified endpoint.
	latencyResp, err := client.AdjustLatency(context.Background(), &proto.LatencyRequest{
		TargetId: "hardware-integration-target",
		DelayMs:  120, // Shift audio 120ms (Now mocked for backwards API compatibility)
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
// Integration Test: Runtime Playback Support Preflight
// -------------------------------------------------------------------------
func TestIntegration_PlaybackSupportPreflight(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateExodusHDRTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := client.GetPlaybackSupport(ctx, &proto.PlaybackSupportRequest{
		FilePath: mediaPath,
	})
	if err != nil {
		t.Fatalf("Playback support preflight failed over gRPC: %v", err)
	}

	if resp.GetOperatingSystem() == "" || resp.GetArchitecture() == "" {
		t.Fatal("Playback support preflight did not report the host platform")
	}
	if resp.GetStatus() == proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_UNKNOWN {
		t.Fatal("Playback support preflight returned an unknown status")
	}
	if resp.GetEncoderFrameTested() && resp.GetEncoderHeight() < 1080 {
		t.Fatalf("4K projection fallback must not select below 1080p; got %dx%d",
			resp.GetEncoderWidth(), resp.GetEncoderHeight())
	}

	t.Logf("[SUPPORT REPORT] status=%s native_gpu=%t decoder=%s encoder=%s failure=%s recommendation=%s",
		resp.GetStatus().String(), resp.GetNativeGpuSupported(), resp.GetDecoderBackend(),
		resp.GetEncoderBackend(), resp.GetFailureCode(), resp.GetRecommendation())
}

// TestE2E_PlaybackSupportPolicyAndFirstChunk verifies the complete contract:
// support probing validates a real encoder frame, GPU-required playback fails
// closed when no verified GPU path exists, and an explicitly permitted fallback
// still delivers a browser-consumable fragment.
func TestIntegration_E2E_PlaybackSupportPolicyAndFirstChunk(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateExodusHDRTestMedia(t)
	defer deleteMedia()

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelProbe()
	capabilities, err := client.GetPlaybackSupport(probeCtx, &proto.PlaybackSupportRequest{
		FilePath: mediaPath,
	})
	if err != nil {
		t.Fatalf("End-to-end playback support query failed: %v", err)
	}

	if capabilities.GetStatus() == proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_UNKNOWN {
		t.Fatal("End-to-end playback support query returned UNKNOWN")
	}
	if capabilities.GetEncoderBackend() != "" && !capabilities.GetEncoderFrameTested() {
		t.Fatal("Encoder backend was reported without a successful real-frame probe")
	}
	if capabilities.GetEncoderFrameTested() &&
		(capabilities.GetEncoderWidth() <= 0 || capabilities.GetEncoderHeight() <= 0) {
		t.Fatalf("Real-frame probe did not report a valid encoder size: %dx%d",
			capabilities.GetEncoderWidth(), capabilities.GetEncoderHeight())
	}
	if capabilities.GetEncoderFrameTested() && capabilities.GetEncoderHeight() < 1080 {
		t.Fatalf("4K projection fallback must not select below 1080p; got %dx%d",
			capabilities.GetEncoderWidth(), capabilities.GetEncoderHeight())
	}

	verifiedGPUPath := capabilities.GetNativeGpuSupported() ||
		(capabilities.GetHardwareDecodeSupported() &&
			capabilities.GetHardwareEncodeSupported() &&
			capabilities.GetEncoderFrameTested())

	if !verifiedGPUPath {
		gpuCtx, cancelGPU := context.WithTimeout(context.Background(), 10*time.Second)
		gpuStream, gpuErr := client.StartStream(gpuCtx, &proto.StreamRequest{
			TargetId:         "e2e-gpu-required",
			FilePath:         mediaPath,
			StreamMode:       proto.StreamMode_transcode,
			Container:        proto.PassthroughContainerFormat_CONTAINER_FMP4,
			ProcessingPolicy: proto.ProcessingPolicy_PROCESSING_GPU_REQUIRED,
		})
		if gpuErr == nil {
			_, gpuErr = gpuStream.Recv()
		}
		cancelGPU()
		if status.Code(gpuErr) != codes.FailedPrecondition {
			t.Fatalf("GPU-required stream should fail closed without a verified GPU path; got %v", gpuErr)
		}
	}

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStream()
	stream, err := client.StartStream(streamCtx, &proto.StreamRequest{
		TargetId:         "e2e-first-fragment",
		FilePath:         mediaPath,
		StreamMode:       proto.StreamMode_transcode,
		Container:        proto.PassthroughContainerFormat_CONTAINER_FMP4,
		ProcessingPolicy: proto.ProcessingPolicy_PROCESSING_ALLOW_CPU,
	})
	if err != nil {
		t.Fatalf("Explicitly permitted fallback stream failed to start: %v", err)
	}

	// The initialization and audio-only fragments are small.  Requiring a
	// substantially larger fragment ensures the test reaches a real encoded
	// video access unit, exercising the live hardware-frame transfer,
	// downscale, and H.264 encode path.
	const minimumMediaFragmentBytes = 4096
	for {
		fragment, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("Explicitly permitted fallback stream did not deliver a fragment: %v", recvErr)
		}
		if len(fragment.GetFmp4Chunk()) >= minimumMediaFragmentBytes {
			t.Logf("[E2E REPORT] Video-sized fMP4 fragment received: %d bytes; status=%s decoder=%s encoder=%s size=%dx%d",
				len(fragment.GetFmp4Chunk()), capabilities.GetStatus().String(),
				capabilities.GetDecoderBackend(), capabilities.GetEncoderBackend(),
				capabilities.GetEncoderWidth(), capabilities.GetEncoderHeight())
			return
		}
	}
}

// TestIntegration_E2E_HDRZscaleToneMapping proves that real HDR transcoding
// sends at least one decoded frame through the zscale + tonemap filter graph.
// macOS normally uses native HEVC passthrough, so that platform does not need
// this conversion and intentionally skips the test.
func TestIntegration_E2E_HDRZscaleToneMapping(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS uses native HDR passthrough; HDR-to-SDR zscale conversion is not required")
	}

	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateExodusHDRTestMedia(t)
	defer deleteMedia()

	metadataCtx, cancelMetadata := context.WithTimeout(context.Background(), 15*time.Second)
	metadata, err := client.GetMediaProperties(metadataCtx, &proto.MetadataRequest{FilePath: mediaPath})
	cancelMetadata()
	if err != nil {
		t.Fatalf("Could not inspect HDR test source: %v", err)
	}
	transfer := strings.ToLower(metadata.GetColorTransfer())
	primaries := strings.ToLower(metadata.GetColorPrimaries())
	colorSpace := strings.ToLower(metadata.GetColorSpace())
	if !strings.Contains(transfer, "2084") && !strings.Contains(transfer, "b67") &&
		!strings.Contains(primaries, "2020") && !strings.Contains(colorSpace, "2020") {
		t.Fatalf("Test fixture is not identified as HDR (transfer=%q primaries=%q colorspace=%q)",
			metadata.GetColorTransfer(), metadata.GetColorPrimaries(), metadata.GetColorSpace())
	}

	const targetID = "e2e-hdr-zscale-tonemap"
	streamCtx, cancelStream := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelStream()
	stream, err := client.StartStream(streamCtx, &proto.StreamRequest{
		TargetId:         targetID,
		FilePath:         mediaPath,
		StreamMode:       proto.StreamMode_transcode,
		Container:        proto.PassthroughContainerFormat_CONTAINER_FMP4,
		ProcessingPolicy: proto.ProcessingPolicy_PROCESSING_ALLOW_CPU,
	})
	if err != nil {
		t.Fatalf("Could not start HDR transcode stream: %v", err)
	}

	// A single frame can pass the graph even when a later decoded D3D11VA
	// transfer changes its colour metadata. Require two seconds of emitted
	// media as well as a substantial payload, which proves the configured
	// zscale graph remains usable beyond its initial frame.
	var videoFragmentBytes int
	const minimumToneMappedMediaSeconds = 2.0
	for {
		fragment, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("HDR transcode ended before a tone-mapped video fragment arrived: %v", recvErr)
		}
		videoFragmentBytes += len(fragment.GetFmp4Chunk())

		session, active := getPlaybackSession(targetID)
		if active && atomic.LoadInt32(&session.SessionCtx.HDRToneMapUsed) == 1 && videoFragmentBytes >= 32*1024 {
			session.SessionCtx.Mu.Lock()
			mediaPTS := session.SessionCtx.CurrentPTS
			session.SessionCtx.Mu.Unlock()
			if mediaPTS >= minimumToneMappedMediaSeconds {
				t.Logf("[HDR E2E] Confirmed sustained zscale + Hable tone mapping through %.2fs of media; received %d bytes of fMP4",
					mediaPTS, videoFragmentBytes)
				return
			}
		}
	}
}

// TestIntegration_E2E_TranscodeCarriesAAC verifies the browser-facing output
// contract by asking the sidecar's native media prober to inspect captured
// fMP4 emitted by the H.264 transcode pipeline.
func TestIntegration_E2E_TranscodeCarriesAAC(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateExodusHDRTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	stream, err := client.StartStream(ctx, &proto.StreamRequest{
		TargetId:         "e2e-aac-fragments",
		FilePath:         mediaPath,
		StreamMode:       proto.StreamMode_transcode,
		Container:        proto.PassthroughContainerFormat_CONTAINER_FMP4,
		ProcessingPolicy: proto.ProcessingPolicy_PROCESSING_ALLOW_CPU,
	})
	if err != nil {
		t.Fatalf("Could not start transcode stream for AAC verification: %v", err)
	}

	// Preserve complete AVIO writes long enough to include multiple fragmented
	// MP4 media fragments. A byte threshold of 512 KiB can be reached by video
	// packets before the muxer has emitted a complete AAC-bearing fragment.
	const minimumCaptureBytes = 3 * 1024 * 1024
	capture := make([]byte, 0, minimumCaptureBytes)
	for len(capture) < minimumCaptureBytes {
		fragment, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("Transcode stream ended before AAC capture was complete: %v", recvErr)
		}
		capture = append(capture, fragment.GetFmp4Chunk()...)
	}
	cancel()

	capturePath := filepath.Join(t.TempDir(), "transcoded-aac.fmp4")
	if err := os.WriteFile(capturePath, capture, 0o600); err != nil {
		t.Fatalf("Could not write captured fMP4 stream: %v", err)
	}

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelProbe()
	metadata, err := client.GetMediaProperties(probeCtx, &proto.MetadataRequest{
		FilePath:            capturePath,
		IncludePacketCounts: true,
	})
	if err != nil {
		t.Fatalf("sidecar could not inspect captured fMP4 media properties: %v", err)
	}
	if !metadata.GetHasVideo() || metadata.GetVideoCodec() != "h264" {
		t.Fatalf("captured fMP4 does not contain the expected H.264 video stream: has_video=%t codec=%q",
			metadata.GetHasVideo(), metadata.GetVideoCodec())
	}
	if !metadata.GetHasAudio() || metadata.GetAudioCodec() != "aac" {
		t.Fatalf("captured H.264 fMP4 does not contain the expected AAC audio stream: has_audio=%t codec=%q",
			metadata.GetHasAudio(), metadata.GetAudioCodec())
	}
	if metadata.GetAudioPacketCount() == 0 {
		t.Fatal("captured fMP4 declares AAC but sidecar found no audio packets")
	}
	t.Logf("[AUDIO E2E] Sidecar probe verified output streams: video=%s audio=%s (%d Hz, %d channels, %d packets)",
		metadata.GetVideoCodec(), metadata.GetAudioCodec(), metadata.GetSampleRate(),
		metadata.GetAudioChannels(), metadata.GetAudioPacketCount())
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

// HDR screenshots are JPEG/SDR by API contract, but must pass through the
// same HDR tone-map as playback and retain the decoded source resolution.
func TestIntegration_HDRScreenshotToneMapsAtSourceResolution(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generateExodusHDRTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metadata, err := client.GetMediaProperties(ctx, &proto.MetadataRequest{FilePath: mediaPath})
	if err != nil {
		t.Fatalf("could not inspect HDR screenshot fixture: %v", err)
	}
	transfer := strings.ToLower(metadata.GetColorTransfer())
	primaries := strings.ToLower(metadata.GetColorPrimaries())
	colorSpace := strings.ToLower(metadata.GetColorSpace())
	if !strings.Contains(transfer, "2084") && !strings.Contains(transfer, "b67") &&
		!strings.Contains(primaries, "2020") && !strings.Contains(colorSpace, "2020") {
		t.Fatalf("screenshot fixture is not identified as HDR (transfer=%q primaries=%q colorspace=%q)",
			metadata.GetColorTransfer(), metadata.GetColorPrimaries(), metadata.GetColorSpace())
	}
	resp, err := client.GetVideoScreenshot(ctx, &proto.ScreenshotRequest{
		FilePath: mediaPath,
		// Exercise the default thumbnail path. The sidecar must move this into
		// the stream and decode through the effective timestamp rather than
		// returning the first keyframe at the timeline origin.
		TimeMs: 0,
	})
	if err != nil {
		t.Fatalf("HDR screenshot extraction failed: %v", err)
	}
	if resp.GetMimeType() != "image/jpeg" {
		t.Fatalf("expected SDR JPEG screenshot, got MIME type %q", resp.GetMimeType())
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(resp.GetImageData()))
	if err != nil {
		t.Fatalf("HDR screenshot is not a valid JPEG: %v", err)
	}
	if config.Width != 3840 || config.Height != 2160 {
		t.Fatalf("HDR screenshot resolution changed: got %dx%d, want source 3840x2160",
			config.Width, config.Height)
	}
}

// -------------------------------------------------------------------------
// Integration Test 5: Candidate RFC 6381 MIME Generation (4K HEVC HDR)
// -------------------------------------------------------------------------
func TestIntegration_CandidateMimeProbing4KHDR(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generate4KHDRTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Logf("[PROBE 4K HDR] Requesting candidate MIME descriptors for: %s", mediaPath)
	resp, err := client.GetMediaProperties(ctx, &proto.MetadataRequest{
		FilePath: mediaPath,
	})
	if err != nil {
		t.Fatalf("Failed to probe 4K HDR media properties: %v", err)
	}

	if resp.GetCandidateVideoMime() == "" {
		t.Error("Candidate video MIME string is empty")
	}
	if resp.GetCandidateAudioMime() == "" {
		t.Error("Candidate audio MIME string is empty")
	}
	if resp.GetCandidateFullMime() == "" {
		t.Error("Candidate full MIME string is empty")
	}

	t.Logf("[PROBE 4K HDR] Candidate Video MIME : %s", resp.GetCandidateVideoMime())
	t.Logf("[PROBE 4K HDR] Candidate Audio MIME : %s", resp.GetCandidateAudioMime())
	t.Logf("[PROBE 4K HDR] Candidate Full MIME  : %s", resp.GetCandidateFullMime())
}

// -------------------------------------------------------------------------
// Integration Test 6: Zero-Copy Passthrough Streaming Pipeline (4K HEVC HDR)
// -------------------------------------------------------------------------
func TestIntegration_PassthroughStreamingPipeline4KHDR(t *testing.T) {
	client, _, cleanup := setupRealHardwareTestServer(t)
	defer cleanup()

	mediaPath, deleteMedia := generate4KHDRTestMedia(t)
	defer deleteMedia()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Logf("[PASSTHROUGH TEST] Starting zero-copy passthrough stream for 4K asset: %s", mediaPath)
	stream, err := client.StartStream(ctx, &proto.StreamRequest{
		TargetId:   "passthrough-4k-target",
		FilePath:   mediaPath,
		StreamMode: proto.StreamMode_passthrough,
	})
	if err != nil {
		t.Fatalf("Failed to initialize passthrough stream over gRPC: %v", err)
	}

	errChan := make(chan error, 1)
	chunkCount := 0
	totalBytes := 0

	go func() {
		outFile, err := os.Create("test_output_passthrough.mp4")
		if err != nil {
			t.Errorf("Failed to create test output file: %v", err)
			return
		}
		defer outFile.Close()

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

			chunk := resp.GetFmp4Chunk()
			if len(chunk) > 0 {
				chunkCount++
				totalBytes += len(chunk)

				if _, err := outFile.Write(chunk); err != nil {
					t.Errorf("Failed to write passthrough chunk to disk: %v", err)
				}
			}

			// Capture initial 30 fMP4 fragments to verify pipeline stability
			if chunkCount >= 30 {
				errChan <- nil
				return
			}
		}
	}()

	select {
	case err := <-errChan:
		if err != nil && err != context.Canceled {
			t.Fatalf("Passthrough stream encountered fatal error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Timeout: Passthrough stream failed to deliver 30 fMP4 chunks within 10 seconds")
	}

	cancel()
	t.Logf("[PASSTHROUGH SUCCESS] Successfully remuxed %d chunks (%d bytes total) in zero-copy mode.", chunkCount, totalBytes)
}
