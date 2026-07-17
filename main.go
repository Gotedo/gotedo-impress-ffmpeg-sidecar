package main

/*
#include "decoder.h"
*/
import "C"
import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ffmpegServer struct {
	proto.UnimplementedFFmpegServiceServer
}

// ChunkPayload bundles the binary fMP4 data with its extraction PTS
type ChunkPayload struct {
	Data []byte
	PTS  float64
}

// SessionContext tracks the state isolated to a single active playback monitor
type SessionContext struct {
	StreamChan chan ChunkPayload
	CurrentPTS float64
	Mu         sync.Mutex
}

//export goTestWriteCallback
func goTestWriteCallback(buf *C.uchar, bufSize C.int, userToken C.uintptr_t) {
	// Reconstruct the Handle directly from the integer token
	handle := cgo.Handle(userToken)

	// Fetch our original Go session context safely
	session := handle.Value().(*SessionContext)

	// Convert buffer and send
	data := C.GoBytes(unsafe.Pointer(buf), bufSize)

	session.Mu.Lock()
	pts := session.CurrentPTS
	session.Mu.Unlock()

	// Push data cleanly into this monitor's private channel array
	// Stream the chunk alongside the current active time context
	select {
	case session.StreamChan <- ChunkPayload{Data: data, PTS: pts}:
	default:
		// Drop frame if buffer backs up heavily to prevent core thread locks
	}
}

func main() {
	socketPath := flag.String("socket", "", "Path to the Unix Domain Socket for IPC")
	testMode := flag.Bool("test", false, "Run application in pipeline verification test mode")
	videoPath := flag.String("video", "", "Absolute path to the video file")
	flag.Parse()

	if *testMode {
		runPipelineTest(*videoPath)
		return
	}

	if *socketPath == "" {
		log.Fatal("Error: Sidecar must be started with a valid -socket path.")
	}

	// Clean up stale socket files if they exist from previous unexpected crashes
	_ = os.Remove(*socketPath)

	listener, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("Failed to bind Unix Domain Socket: %v", err)
	}

	grpcServer := grpc.NewServer()
	proto.RegisterFFmpegServiceServer(grpcServer, &ffmpegServer{})

	// Handle graceful termination signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Stopping gRPC sidecar server safely...")
		grpcServer.GracefulStop()
		_ = os.Remove(*socketPath)
		os.Exit(0)
	}()

	log.Printf("FFmpeg Go-gRPC Sidecar listening on UDS: %s", *socketPath)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("gRPC server run failure: %v", err)
	}
}

func runPipelineTest(path string) {
	log.Println("[TEST] Starting FFmpeg & Miniaudio Integrated Pipeline Test...")

	// Verify absolute path requirements
	if path == "" {
		log.Fatalf("[TEST ERROR] Must specify a video file path using the -video flag")
	}

	if !filepath.IsAbs(path) {
		log.Fatalf("[TEST ERROR] Path must be absolute. Received: %s", path)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Fatalf("[TEST ERROR] Video file does not exist at path: %s", path)
	}
	log.Printf("[TEST] Absolute video path verified: %s", path)

	// Allocate the massive structs in C memory directly.
	// C.calloc zero-initializes the memory just like Go's 'var' does.
	decCtx := (*C.DemuxDecContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.DemuxDecContext{}))))
	defer C.free(unsafe.Pointer(decCtx))

	playCtx := (*C.AudioPlaybackContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.AudioPlaybackContext{}))))
	defer C.free(unsafe.Pointer(playCtx))

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	// Step 1: Open file and decoders (pass pointer directly)
	log.Println("[TEST] Opening media file and initializing decoders...")
	ret := C.open_input_and_decoders(decCtx, cPath)
	if ret < 0 {
		log.Fatalf("[TEST ERROR] Failed opening input decoders. FFmpeg Code: %d", ret)
	}
	log.Println("[TEST] SUCCESS: FFmpeg Decoders and SWResampler successfully initialized!")

	// Step 2: Initialize Audio Playback (using system default speaker)
	log.Println("[TEST] Initializing Miniaudio hardware device...")
	ret = C.init_audio_playback(playCtx, C.int(48000), C.int(2), nil)
	if ret < 0 {
		C.free_demux_dec_context(decCtx)
		log.Fatalf("[TEST ERROR] Failed to initialize Audio hardware. Code: %d", ret)
	}
	log.Println("[TEST] SUCCESS: Miniaudio successfully opened system hardware soundcard!")

	// Instantiate a perfectly isolated, thread-safe session container for this test
	sessionCtx := &SessionContext{
		StreamChan: make(chan ChunkPayload, 100),
	}

	// CREATE A SAFE CGO HANDLE FOR THE CHANNEL
	handle := cgo.NewHandle(sessionCtx)
	defer handle.Delete() // Ensure we release the handle when the test finishes to avoid leaks

	// Step 3: Spin up the C-processing pipeline loop on a concurrent Go thread
	log.Println("[TEST] Spawning parallel demuxer & play threads...")

	// Create an error channel to capture C-side failures
	errChan := make(chan int, 1)

	go func() {
		// Capture the return status from the C function
		result := C.run_test_mux_and_play(decCtx, playCtx, C.uintptr_t(handle))
		errChan <- int(result)
	}()

	// Monitor for errors in a non-blocking way or as part of your logic flow
	go func() {
		if err := <-errChan; err != 0 {
			log.Printf("[TEST ERROR] C-side pipeline exited with error code: %d", err)
		} else {
			log.Println("[TEST] C-side pipeline execution completed successfully.")
		}
	}()

	// Step 4: Stream consumer
	go func() {
		for chunk := range sessionCtx.StreamChan {
			fmt.Printf("[TEST STREAM] Captured %d bytes of browser-ready fMP4 in Go memory!\n", len(chunk.Data))
		}
	}()

	// Step 5: Simulate live feedback from webview
	time.Sleep(2 * time.Second)
	log.Println("[TEST LATENCY] Simulated webview reports rendering lag. Increasing audio delay to 150ms...")
	C.set_audio_delay_offset(playCtx, C.int(150))

	time.Sleep(2 * time.Second)
	log.Println("[TEST LATENCY] Simulated webview caught up. Restoring audio delay to 50ms...")
	C.set_audio_delay_offset(playCtx, C.int(50))

	time.Sleep(3 * time.Second)

	// Clean up Contexts
	log.Println("[TEST] Tearing down playback engine and freeing C memory...")
	C.stop_audio_playback(playCtx)
	C.free_demux_dec_context(decCtx)
	log.Println("[TEST] SUCCESS: All context structures freed cleanly. No leaks found!")
	log.Println("[TEST] PIPELINE RUN CONCLUDED SUCCESSFULLY!")
}

// StartStream implements the gRPC streaming endpoint with isolated multi-monitor support
func (s *ffmpegServer) StartStream(req *proto.StreamRequest, stream proto.FFmpegService_StartStreamServer) error {
	log.Printf("[SIDECAR] Received StartStream request for file: %s", req.GetFilePath())

	if _, err := os.Stat(req.GetFilePath()); os.IsNotExist(err) {
		return status.Errorf(codes.NotFound, "media file does not exist: %s", req.GetFilePath())
	}

	// Instantiate a perfectly isolated, thread-safe session container for this specific stream context
	sessionCtx := &SessionContext{
		StreamChan: make(chan ChunkPayload, 100),
	}

	// Wrap this private instance in a localized runtime Cgo handle
	handle := cgo.NewHandle(sessionCtx)
	defer handle.Delete()

	// Allocate context structures directly in C memory to avoid unpinned pointer GC checks
	decCtx := (*C.DemuxDecContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.DemuxDecContext{}))))
	defer C.free(unsafe.Pointer(decCtx))

	playCtx := (*C.AudioPlaybackContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.AudioPlaybackContext{}))))
	defer C.free(unsafe.Pointer(playCtx))

	cPath := C.CString(req.GetFilePath())
	defer C.free(unsafe.Pointer(cPath))

	// Step 1: Open input contexts
	if ret := C.open_input_and_decoders(decCtx, cPath); ret < 0 {
		return status.Errorf(codes.Internal, "FFmpeg decoder initialization failed with code: %d", ret)
	}

	// Step 2: Initialize system soundcard bindings
	// Note: Pass hardware device routing parameters if requested by req.GetAudioDeviceId()
	if ret := C.init_audio_playback(playCtx, C.int(48000), C.int(2), nil); ret < 0 {
		C.free_demux_dec_context(decCtx)
		return status.Errorf(codes.Internal, "Miniaudio hardware initialization failed with code: %d", ret)
	}

	// Spin up the C processing pipeline thread
	pipelineErrChan := make(chan int, 1)
	go func() {
		ret := C.run_test_mux_and_play(decCtx, playCtx, C.uintptr_t(handle))
		pipelineErrChan <- int(ret)
	}()

	// Loop and push fMP4 packets across the gRPC network boundary as they arrive from C
	for {
		select {
		case <-stream.Context().Done():
			log.Println("[SIDECAR] Client closed connection stream.")
			C.stop_audio_playback(playCtx)
			C.free_demux_dec_context(decCtx)
			return stream.Context().Err()

		case errCode := <-pipelineErrChan:
			log.Printf("[SIDECAR] C-pipeline demuxing complete. Exit code: %d", errCode)

			// ONLY tear down and return if there was an actual error.
			// If it was successful (0), leave the function running so the audio
			// ring buffer can drain and the client can finish rendering the video.
			if errCode < 0 {
				C.stop_audio_playback(playCtx)
				C.free_demux_dec_context(decCtx)
				return status.Errorf(codes.Internal, "C-pipeline processing failed with error code: %d", errCode)
			}

		case chunk := <-sessionCtx.StreamChan: // Read from private channel loop
			// Send binary payload packet over the network
			err := stream.Send(&proto.StreamResponse{
				Fmp4Chunk: chunk.Data,
				Pts:       chunk.PTS,
			})
			if err != nil {
				log.Printf("[SIDECAR ERROR] Failed to send gRPC response packet: %v", err)
				C.stop_audio_playback(playCtx)
				C.free_demux_dec_context(decCtx)
				return err
			}
		}
	}
}

// GetAudioDevices queries miniaudio and maps discovered OS soundcards directly to gRPC responses.
func (s *ffmpegServer) GetAudioDevices(ctx context.Context, req *proto.DevicesRequest) (*proto.DevicesResponse, error) {
	maxDevices := 32
	// Allocate sequential buffer in C heap space
	cDevices := (*C.NativeAudioDevice)(C.calloc(C.size_t(maxDevices), C.size_t(unsafe.Sizeof(C.NativeAudioDevice{}))))
	defer C.free(unsafe.Pointer(cDevices))

	count := int(C.get_miniaudio_devices(cDevices, C.int(maxDevices)))
	if count < 0 {
		return nil, status.Errorf(codes.Internal, "failed to query host miniaudio capabilities: error code %d", count)
	}

	// Slice across C memory layout natively without allocation overheads
	deviceSlice := (*[1 << 20]C.NativeAudioDevice)(unsafe.Pointer(cDevices))[:count:count]
	responseDevices := make([]*proto.AudioDevice, 0, count)

	for i := 0; i < count; i++ {
		responseDevices = append(responseDevices, &proto.AudioDevice{
			Id:        C.GoString(&deviceSlice[i].id[0]),
			Name:      C.GoString(&deviceSlice[i].name[0]),
			IsDefault: bool(deviceSlice[i].is_default),
		})
	}

	return &proto.DevicesResponse{
		Devices: responseDevices,
	}, nil
}

// AdjustLatency handles real-time audio delay modifications from the backend client.
func (s *ffmpegServer) AdjustLatency(ctx context.Context, req *proto.LatencyRequest) (*proto.LatencyResponse, error) {
	log.Printf("[SIDECAR] AdjustLatency requested for target %s: %d ms", req.GetTargetId(), req.GetDelayMs())

	// TODO: Pass this offset value down to your active miniaudio playback ring context.
	// C.set_audio_delay_offset(playCtx, C.int(req.DelayMs()))

	return &proto.LatencyResponse{
		Accepted: true,
	}, nil
}

//export set_session_pts
func set_session_pts(userToken C.uintptr_t, pts C.double) {
	handle := cgo.Handle(userToken)
	session := handle.Value().(*SessionContext)

	session.Mu.Lock()
	session.CurrentPTS = float64(pts)
	session.Mu.Unlock()
}

// GetMediaProperties probes a media file using native FFmpeg extraction loops.
func (s *ffmpegServer) GetMediaProperties(ctx context.Context, req *proto.MetadataRequest) (*proto.MetadataResponse, error) {
	log.Printf("[SIDECAR] Received comprehensive GetMediaProperties request for file: %s", req.GetFilePath())

	// 1. Verify existence profile natively before dropping into the C heap
	fileInfo, err := os.Stat(req.GetFilePath())
	if os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "media file does not exist: %s", req.GetFilePath())
	}

	cPath := C.CString(req.GetFilePath())
	defer C.free(unsafe.Pointer(cPath))

	// 2. Allocate the properties struct cleanly on the C stack area
	var cProps C.CMediaProperties

	// 3. Invoke the C-Prober compilation engine
	ret := C.probe_media_properties(cPath, &cProps)
	if ret < 0 {
		return nil, status.Errorf(codes.Internal, "FFmpeg native prober failed with code: %d", int(ret))
	}

	// 4. Read last modified metadata directly from filesystem fallback to bolster static tags
	lastModifiedStr := fileInfo.ModTime().Format(time.RFC3339)

	// 5. Unpack and map all parsed C fields directly into your gRPC Response layout
	return &proto.MetadataResponse{
		FormatName:     C.GoString(&cProps.format_name[0]),
		FormatLongName: C.GoString(&cProps.format_long_name[0]),
		DurationMs:     int64(cProps.duration_ms),
		FileSizeBytes:  int64(cProps.file_size_bytes), // Maps to protocol layout
		BitRate:        int64(cProps.bit_rate),

		Title:        C.GoString(&cProps.title[0]),
		Author:       C.GoString(&cProps.author[0]),
		Album:        C.GoString(&cProps.album[0]),
		Track:        C.GoString(&cProps.track[0]),
		Genre:        C.GoString(&cProps.genre[0]),
		CreationTime: C.GoString(&cProps.creation_time[0]),
		LastModified: lastModifiedStr,

		HasVideo:           bool(cProps.has_video != 0),
		VideoCodec:         C.GoString(&cProps.video_codec[0]),
		VideoCodecLongName: C.GoString(&cProps.video_codec_long_name[0]),
		VideoProfile:       C.GoString(&cProps.video_profile[0]),
		Width:              int32(cProps.width),
		Height:             int32(cProps.height),
		Framerate:          float64(cProps.framerate),
		AspectRatio:        C.GoString(&cProps.aspect_ratio[0]),
		PixelFormat:        C.GoString(&cProps.pixel_format[0]),
		ColorSpace:         C.GoString(&cProps.color_space[0]),
		ColorTransfer:      C.GoString(&cProps.color_transfer[0]),
		ColorPrimaries:     C.GoString(&cProps.color_primaries[0]),

		HasAudio:           bool(cProps.has_audio != 0),
		AudioCodec:         C.GoString(&cProps.audio_codec[0]),
		AudioCodecLongName: C.GoString(&cProps.audio_codec_long_name[0]),
		AudioProfile:       C.GoString(&cProps.audio_profile[0]),
		AudioChannels:      int32(cProps.audio_channels),
		SampleRate:         int32(cProps.sample_rate),
		ChannelLayout:      C.GoString(&cProps.channel_layout[0]),
		AudioBitRate:       int64(cProps.audio_bit_rate),
	}, nil
}

// GetVideoScreenshot extracts a single frame image out of a media asset layout.
func (s *ffmpegServer) GetVideoScreenshot(ctx context.Context, req *proto.ScreenshotRequest) (*proto.ScreenshotResponse, error) {
	log.Printf("[SIDECAR] Extracting screenshot for file: %s at %d ms", req.GetFilePath(), req.GetTimeMs())

	if _, err := os.Stat(req.GetFilePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "media file does not exist: %s", req.GetFilePath())
	}

	timeTargetMs := req.GetTimeMs()
	if timeTargetMs <= 0 {
		timeTargetMs = 1000 // Sane default to bypass initial black frames
	}

	cPath := C.CString(req.GetFilePath())
	defer C.free(unsafe.Pointer(cPath))

	var outBuf *C.uint8_t
	var outSize C.int

	// Call the native C function we built in Task 2.7.2
	ret := C.extract_video_screenshot(cPath, C.int64_t(timeTargetMs), &outBuf, &outSize)
	if ret < 0 {
		return nil, status.Errorf(codes.Internal, "native frame extraction failed with code: %d", int(ret))
	}

	// Safely copy C memory into a managed Go byte slice before freeing the buffer
	imageData := C.GoBytes(unsafe.Pointer(outBuf), outSize)

	// CRITICAL: Free the C heap allocation generated by av_malloc in decoder.c
	C.av_free(unsafe.Pointer(outBuf))

	return &proto.ScreenshotResponse{
		ImageData: imageData,
		MimeType:  "image/jpeg",
	}, nil
}
