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
	StreamChan      chan ChunkPayload
	CurrentPTS      float64
	Mu              sync.Mutex
	playbackSession *PlaybackSession
}

// Per-target playback session registry (enables safe concurrent playbacks)
type PlaybackSession struct {
	TargetID   string
	DecCtx     *C.DemuxDecContext
	SessionCtx *SessionContext
	Handle     cgo.Handle
	Cancel     context.CancelFunc
	// Track pause state for proper resume
	lastPausePTS float64
	isPaused     bool
	lastKnownPTS float64 // continuously updated from set_session_pts
	FilePath     string
	Mu           sync.Mutex
}

var (
	activePlaybacks = make(map[string]*PlaybackSession)
	playbacksMu     sync.RWMutex
)

// updateLastKnownPTS is called from set_session_pts / telemetry
func (s *PlaybackSession) updateLastKnownPTS(pts float64) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.lastKnownPTS = pts
}

// captureCurrentPTSForPause snapshots the most recent PTS pushed by
// the C pipeline via set_session_pts. This is the reliable way to
// know exactly where we are when the user presses pause.
func (s *PlaybackSession) captureCurrentPTSForPause() float64 {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.lastPausePTS = s.lastKnownPTS
	if s.lastPausePTS == 0 && s.SessionCtx != nil {
		s.lastPausePTS = s.SessionCtx.CurrentPTS
	}
	s.isPaused = true
	return s.lastPausePTS
}

// registerPlayback stores (or replaces) a playback session for a target.
// Safe to call even if a session already exists for that target.
func registerPlayback(targetID string, sess *PlaybackSession) {
	if _, exists := getPlayback(targetID); exists {
		log.Printf("[SIDECAR] Overwriting existing playback for target: %s", targetID)
	}

	unregisterPlayback(targetID) // ensure clean state first (idempotent)

	playbacksMu.Lock()
	activePlaybacks[targetID] = sess
	playbacksMu.Unlock()
}

// unregisterPlayback safely tears down a playback session.
// It is idempotent — safe to call multiple times for the same targetID.
func unregisterPlayback(targetID string) {
	playbacksMu.Lock()
	sess, exists := activePlaybacks[targetID]
	if !exists {
		playbacksMu.Unlock()
		return
	}

	// Remove from map immediately so any concurrent call sees "not exists"
	delete(activePlaybacks, targetID)
	playbacksMu.Unlock()

	// 1. CRITICAL FIX: Signal the C streaming loop to terminate cleanly
	if sess.DecCtx != nil {
		C.request_stop_on_dec_ctx(sess.DecCtx)
	}

	// Perform actual resource cleanup outside the lock
	if sess.Cancel != nil {
		sess.Cancel()
	}

	// Give the old C pipeline goroutine a moment to exit
	// (prevents use-after-free on the cgo.Handle and internal contexts)
	time.Sleep(250 * time.Millisecond)

	if sess.DecCtx != nil {
		C.free_demux_dec_context(sess.DecCtx)
		sess.DecCtx = nil
	}

	if sess.Handle != 0 {
		sess.Handle.Delete()
	}
}

// getPlayback returns the active session for a target (thread-safe)
func getPlayback(targetID string) (*PlaybackSession, bool) {
	playbacksMu.RLock()
	defer playbacksMu.RUnlock()
	sess, ok := activePlaybacks[targetID]
	return sess, ok
}

// stopPlayback stops audio playback and frees FFmpeg decoder contexts for a target.
// It does NOT delete the cgo.Handle or remove the session from the map.
// Use this for temporary stop/pause scenarios where you may want to resume later.
func stopPlayback(targetID string) {
	playbacksMu.Lock()
	sess, exists := activePlaybacks[targetID]
	if !exists || sess == nil {
		playbacksMu.Unlock()
		return
	}
	playbacksMu.Unlock()

	// Ensure the loop stops pushing data before we free underneath it
	if sess.DecCtx != nil {
		C.request_stop_on_dec_ctx(sess.DecCtx)
	}

	// Free decoder resources
	if sess.DecCtx != nil {
		C.free_demux_dec_context(sess.DecCtx)
		sess.DecCtx = nil
	}
}

//export goTestWriteCallback
func goTestWriteCallback(buf *C.uchar, bufSize C.int, userToken C.uintptr_t) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered from invalid cgo.Handle in goTestWriteCallback: %v", r)
		}
	}()

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
	// Allow the channel to block the C thread to establish backpressure.
	session.StreamChan <- ChunkPayload{Data: data, PTS: pts}
}

func main() {
	socketPath := flag.String("socket", "", "Path to the Unix Domain Socket for IPC")
	// Parse the incoming command-line arguments
	flag.Parse()

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

// StartStream implements the gRPC streaming endpoint with per-target session tracking
func (s *ffmpegServer) StartStream(req *proto.StreamRequest, stream proto.FFmpegService_StartStreamServer) error {
	targetID := req.GetTargetId()
	log.Printf("[SIDECAR] Received StartStream request for target=%s file: %s", targetID, req.GetFilePath())

	if _, err := os.Stat(req.GetFilePath()); os.IsNotExist(err) {
		return status.Errorf(codes.NotFound, "media file does not exist: %s", req.GetFilePath())
	}

	// PRE: Handle case where target already has an active playback
	if oldSess, exists := getPlayback(targetID); exists && oldSess != nil {
		log.Printf("[SIDECAR] Target %s already has active playback — replacing it", targetID)

		// Cancel the old streaming context so its handler exits cleanly
		if oldSess.Cancel != nil {
			oldSess.Cancel()
		}

		// Give the old goroutine a moment to exit its select loop
		time.Sleep(300 * time.Millisecond)

		// Fully unregister (stops audio, frees C memory, deletes handle)
		unregisterPlayback(targetID)
	}

	// 1. Create isolated Go session context + cgo handle
	sessionCtx := &SessionContext{
		StreamChan: make(chan ChunkPayload, 100),
	}

	// Wrap this private instance in a localized runtime Cgo handle
	handle := cgo.NewHandle(sessionCtx)
	// We will delete the handle inside unregisterPlayback

	// 2. Allocate C contexts (we keep explicit frees for early error paths)
	decCtx := (*C.DemuxDecContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.DemuxDecContext{}))))

	cPath := C.CString(req.GetFilePath())
	defer C.free(unsafe.Pointer(cPath))

	// 3. Open decoders
	if ret := C.open_input_and_decoders(decCtx, cPath); ret < 0 {
		C.free(unsafe.Pointer(decCtx))
		handle.Delete()
		return status.Errorf(codes.Internal, "FFmpeg decoder initialization failed with code: %d", ret)
	}

	// 4. Register session so ControlStream / AdjustLatency / Shutdown can find it
	_, cancel := context.WithCancel(stream.Context())
	sess := &PlaybackSession{
		TargetID:   targetID,
		DecCtx:     decCtx,
		SessionCtx: sessionCtx,
		Handle:     handle,
		Cancel:     cancel,
		FilePath:   req.GetFilePath(),
	}
	// Update the session context
	sessionCtx.Mu.Lock()
	sessionCtx.playbackSession = sess
	sessionCtx.Mu.Unlock()
	registerPlayback(targetID, sess)

	// 5. Start the C pipeline goroutine
	pipelineErrChan := make(chan int, 1)
	go func() {
		ret := C.run_streaming_mux_and_play(decCtx, C.uintptr_t(handle))
		pipelineErrChan <- int(ret)
	}()

	// 6. Main streaming loop
	for {
		select {
		case <-stream.Context().Done():
			log.Println("[SIDECAR] Client closed connection stream.")
			unregisterPlayback(targetID)
			return stream.Context().Err()

		case errCode := <-pipelineErrChan:
			log.Printf("[SIDECAR] C-pipeline for target %s finished with code: %d", targetID, errCode)

			if errCode < 0 {
				// Error path -> full cleanup
				unregisterPlayback(targetID)
				return status.Errorf(codes.Internal, "C-pipeline processing failed with error code: %d", errCode)
			}

			// Success path (errCode == 0): Do NOT free yet.
			// Leave contexts alive so the audio ring buffer can drain
			// and the client can finish rendering. We will clean up on client disconnect
			// or when the next request comes in for the same target.
			log.Printf("[SIDECAR] Pipeline ended successfully for target %s — waiting for client to finish (audio drain)", targetID)
			// We intentionally do NOT call unregisterPlayback here.
			// The session stays registered until client disconnect or new request overwrites it.

		case chunk := <-sessionCtx.StreamChan:
			err := stream.Send(&proto.StreamResponse{
				Fmp4Chunk: chunk.Data,
				Pts:       chunk.PTS,
			})
			if err != nil {
				log.Printf("[SIDECAR ERROR] Failed to send gRPC response: %v", err)
				unregisterPlayback(targetID)
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
	return &proto.LatencyResponse{Accepted: true}, nil
}

//export set_session_pts
func set_session_pts(userToken C.uintptr_t, pts C.double) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered from invalid cgo.Handle in set_session_pts: %v", r)
		}
	}()

	handle := cgo.Handle(userToken)
	session := handle.Value().(*SessionContext)

	session.Mu.Lock()
	session.CurrentPTS = float64(pts)
	session.Mu.Unlock()

	if session.playbackSession != nil {
		session.playbackSession.Mu.Lock()
		session.playbackSession.lastKnownPTS = float64(pts)
		session.playbackSession.Mu.Unlock()
	}
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

// Shutdown RPC - graceful exit requested by manager
func (s *ffmpegServer) Shutdown(ctx context.Context, req *proto.ShutdownRequest) (*proto.ShutdownResponse, error) {
	log.Println("[SIDECAR] Shutdown RPC received - initiating graceful exit")
	// Stop all active playbacks first
	playbacksMu.Lock()
	for targetID := range activePlaybacks {
		unregisterPlayback(targetID) // reuses the cleanup logic
	}
	playbacksMu.Unlock()

	go func() {
		time.Sleep(200 * time.Millisecond) // allow final gRPC responses
		os.Exit(0)
	}()
	return &proto.ShutdownResponse{Accepted: true}, nil
}

// ControlStream handles PLAY/PAUSE/SEEK/STOP for a specific target.
// STOP is fully supported. PLAY/PAUSE/SEEK currently return success
// (so the frontend doesn't break) but require future enhancements
// in the C demux/mux loop for true runtime control.
func (s *ffmpegServer) ControlStream(ctx context.Context, req *proto.ControlRequest) (*proto.ControlResponse, error) {
	targetID := req.GetTargetId()
	action := req.GetAction()
	sess, ok := getPlayback(targetID)

	if !ok || sess == nil {
		return &proto.ControlResponse{
			Success: false,
			Message: "no active playback for target",
		}, nil
	}

	switch action {
	case proto.ControlRequest_STOP:
		unregisterPlayback(targetID)
		log.Printf("[SIDECAR] STOP command executed for target: %s", targetID)

		return &proto.ControlResponse{
			Success: true,
			Message: "playback stopped",
		}, nil

	case proto.ControlRequest_PLAY:
		if sess.DecCtx != nil {
			C.set_dec_ctx_paused(sess.DecCtx, C.int(0))
		}
		log.Printf("[SIDECAR] RESUMED target: %s", targetID)

		return &proto.ControlResponse{
			Success: true,
			Message: fmt.Sprintf("Playback resumed. File: %s", sess.FilePath),
		}, nil

	case proto.ControlRequest_PAUSE:
		capturedPTS := sess.captureCurrentPTSForPause()

		// Signal the streaming loop to stop processing new packets (video + audio).
		// This is the key to "audio stops exactly when video is paused".
		if sess.DecCtx != nil {
			C.set_dec_ctx_paused(sess.DecCtx, C.int(1))
		}
		sess.isPaused = true
		log.Printf("[SIDECAR] PAUSED target=%s at %.2fs", targetID, capturedPTS)

		return &proto.ControlResponse{
			Success: true,
			Message: fmt.Sprintf("Playback paused at %.2fs: File: %s", capturedPTS, sess.FilePath),
		}, nil

	case proto.ControlRequest_SEEK:
		seekMs := int64(req.GetSeekSeconds()) * 1000
		if sess.DecCtx != nil {
			// Use the flag-based request so the streaming loop performs the seek
			// cleanly without racing with av_read_frame.
			C.request_seek_on_dec_ctx(sess.DecCtx, C.int64_t(seekMs))
		}
		log.Printf("[SIDECAR] SEEK to %d ms on target: %s", seekMs, targetID)

		return &proto.ControlResponse{
			Success: true,
			Message: fmt.Sprintf("Seek performed at %d: File: %s", seekMs, sess.FilePath),
		}, nil

	default:
		return &proto.ControlResponse{
			Success: false,
			Message: "unknown action",
		}, nil
	}
}

// StopStream is an explicit stop (alias to STOP control for convenience)
func (s *ffmpegServer) StopStream(ctx context.Context, req *proto.StreamControlRequest) (*proto.StreamControlResponse, error) {
	targetID := req.GetTargetId()
	unregisterPlayback(targetID)
	return &proto.StreamControlResponse{Success: true, Message: "stream stopped"}, nil
}
