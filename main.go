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
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type ffmpegServer struct {
	proto.UnimplementedFFmpegServiceServer
	conversionMu sync.Mutex
}

type conversionProgressContext struct {
	ctx                 context.Context
	stream              proto.FFmpegService_ConvertVideoServer
	encoderName         string
	hardwareAccelerated bool
	err                 error
}

// rescaleConversionPTS exposes the C timestamp normalizer to focused Go tests.
func rescaleConversionPTS(inputPTS int64, inputTBNum, inputTBDen, outputTBNum, outputTBDen int, lastOutputPTS int64) (int64, int64) {
	last := C.int64_t(lastOutputPTS)
	output := C.conversion_rescale_monotonic_pts(
		C.int64_t(inputPTS), C.int(inputTBNum), C.int(inputTBDen),
		C.int(outputTBNum), C.int(outputTBDen), &last,
	)
	return int64(output), int64(last)
}

// ChunkPayload bundles the binary fMP4 data with its extraction PTS
type ChunkPayload struct {
	Data []byte
	PTS  float64
}

// SessionContext tracks the state isolated to a single active playback monitor
type SessionContext struct {
	StreamChan      chan ChunkPayload
	Done            chan struct{}
	doneOnce        sync.Once
	CurrentPTS      float64
	IsEOF           int32 // int32 for atomic operations
	HDRToneMapUsed  int32 // set by C after an HDR frame exits the zscale/tonemap graph
	Mu              sync.Mutex
	playbackSession *PlaybackSession
}

// stop unblocks a C write callback when its gRPC client disconnects.  The
// callback normally blocks on StreamChan to apply real backpressure, so it
// needs an explicit escape path before its cgo.Handle is deleted.
func (s *SessionContext) stop() {
	if s != nil && s.Done != nil {
		s.doneOnce.Do(func() { close(s.Done) })
	}
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
	activePlaybacks    = make(map[string]*PlaybackSession)
	playbacksMu        sync.RWMutex
	playbackProbeMu    sync.Mutex
	playbackProbeCache = make(map[string]cachedPlaybackProbe)
)

const playbackProbeCacheTTL = 30 * time.Second

type cachedPlaybackProbe struct {
	size       int64
	modifiedAt int64
	expiresAt  time.Time
	support    C.CPlaybackSupport
}

// cachedPlaybackSupport returns a still-valid plan without performing another
// GPU probe. StartStream uses it after the UI preflight so opening a stream
// does not immediately retry every adapter/backend that has just failed.
func cachedPlaybackSupport(filePath string, info os.FileInfo) (C.CPlaybackSupport, bool) {
	var empty C.CPlaybackSupport
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return empty, false
	}
	key := filepath.Clean(absPath)
	now := time.Now()

	playbackProbeMu.Lock()
	defer playbackProbeMu.Unlock()
	cached, ok := playbackProbeCache[key]
	if !ok || cached.size != info.Size() || cached.modifiedAt != info.ModTime().UnixNano() || !now.Before(cached.expiresAt) {
		if ok {
			delete(playbackProbeCache, key)
		}
		return empty, false
	}
	return cached.support, true
}

// probePlaybackSupport shares the expensive hardware probe between the UI's
// preflight and StartStream's enforcement check. File identity includes size
// and nanosecond mtime so replacing a video at the same path invalidates it.
func probePlaybackSupport(filePath string, info os.FileInfo) (C.CPlaybackSupport, C.int) {
	var empty C.CPlaybackSupport
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return empty, C.int(-1)
	}
	key := filepath.Clean(absPath)
	modifiedAt := info.ModTime().UnixNano()
	now := time.Now()

	// Serialize cache misses as well as protecting map access. Hardware probing
	// is device intensive; simultaneous callers for one or more files should not
	// launch competing decoder/encoder trials on the same GPU.
	playbackProbeMu.Lock()
	defer playbackProbeMu.Unlock()
	if cached, ok := playbackProbeCache[key]; ok {
		if cached.size == info.Size() && cached.modifiedAt == modifiedAt && now.Before(cached.expiresAt) {
			return cached.support, 0
		}
		delete(playbackProbeCache, key)
	}

	cPath := C.CString(filePath)
	defer C.free(unsafe.Pointer(cPath))
	var support C.CPlaybackSupport
	ret := C.probe_playback_support(cPath, &support)
	if ret >= 0 {
		if len(playbackProbeCache) >= 128 {
			for cacheKey, cached := range playbackProbeCache {
				if !now.Before(cached.expiresAt) {
					delete(playbackProbeCache, cacheKey)
				}
			}
			if len(playbackProbeCache) >= 128 {
				for cacheKey := range playbackProbeCache {
					delete(playbackProbeCache, cacheKey)
					break
				}
			}
		}
		playbackProbeCache[key] = cachedPlaybackProbe{
			size: info.Size(), modifiedAt: modifiedAt,
			expiresAt: now.Add(playbackProbeCacheTTL), support: support,
		}
	}
	return support, ret
}

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
	if _, exists := getPlaybackSession(targetID); exists {
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
	if sess.SessionCtx != nil {
		sess.SessionCtx.stop()
	}
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

	log.Printf("[SIDECAR -> unregisterPlayback] Unregistered playback for target: %s", targetID)
}

// getPlaybackSession returns the active session for a target (thread-safe)
func getPlaybackSession(targetID string) (*PlaybackSession, bool) {
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

//export goStreamWriteCallback
func goStreamWriteCallback(buf *C.uchar, bufSize C.int, userToken C.uintptr_t) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered from invalid cgo.Handle in goStreamWriteCallback: %v", r)
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

	// Preserve every fMP4 byte. A bounded channel intentionally blocks the C
	// producer when gRPC/WebSocket/MSE is behind; dropping an arbitrary AVIO
	// chunk corrupts a fragment and creates an unrecoverable playback gap.
	select {
	case session.StreamChan <- ChunkPayload{Data: data, PTS: pts}:
	case <-session.Done:
		return
	}
}

//export goHDRToneMapFrameProcessed
func goHDRToneMapFrameProcessed(userToken C.uintptr_t) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered from invalid cgo.Handle in goHDRToneMapFrameProcessed: %v", r)
		}
	}()

	session := cgo.Handle(userToken).Value().(*SessionContext)
	atomic.StoreInt32(&session.HDRToneMapUsed, 1)
}

//export goVideoConversionProgress
func goVideoConversionProgress(progress C.double, encoderName *C.char, hardwareAccelerated C.int, userToken C.uintptr_t) C.int {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered from invalid cgo.Handle in goVideoConversionProgress: %v", r)
		}
	}()
	state := cgo.Handle(userToken).Value().(*conversionProgressContext)
	state.encoderName = C.GoString(encoderName)
	state.hardwareAccelerated = hardwareAccelerated != 0
	if state.ctx.Err() != nil {
		return -1
	}
	if err := state.stream.Send(&proto.VideoConversionProgress{
		Progress:            float64(progress),
		Stage:               "converting",
		EncoderName:         state.encoderName,
		HardwareAccelerated: state.hardwareAccelerated,
	}); err != nil {
		state.err = err
		return -1
	}
	return 0
}

//export set_session_eof
func set_session_eof(token C.uintptr_t, isEof C.int) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SIDECAR] Recovered in set_session_eof: %v", r)
		}
	}()

	handle := cgo.Handle(token)
	if sessCtx, ok := handle.Value().(*SessionContext); ok && sessCtx != nil {
		atomic.StoreInt32(&sessCtx.IsEOF, int32(isEof))
	}
}

func main() {
	// Config `log` to send normal operational logs to os.Stdout rather than os.Stderr
	log.SetOutput(os.Stdout)

	addr := flag.String("addr", "", "Address for IPC (Unix Domain Socket path or TCP Address)")
	probePath := flag.String("probe", "", "Path to media file to probe metadata and exit")
	// Parse the incoming command-line arguments
	flag.Parse()

	if *probePath != "" {
		server := &ffmpegServer{}
		resp, err := server.GetMediaProperties(context.Background(), &proto.MetadataRequest{
			FilePath: *probePath,
		})
		if err != nil {
			log.Fatalf("CLI Probe Failed: %v", err)
		}

		marshaller := protojson.MarshalOptions{
			Indent:          "  ",
			Multiline:       true,
			EmitUnpopulated: false,
		}

		jsonBytes, err := marshaller.Marshal(resp)
		if err != nil {
			log.Fatalf("Failed to serialize response: %v", err)
		}

		fmt.Println(string(jsonBytes))
		os.Exit(0)
	}

	if *addr == "" {
		log.Fatal("Error: Sidecar must be started with a valid -addr path or address.")
	}

	var listener net.Listener
	var err error

	useTCP := runtime.GOOS == "windows" || os.Getenv("GOTEDO_FFMPEG_SIDECAR_USE_TCP") == "1"
	if useTCP {
		// Windows and isolated integration tests: Listen on TCP.
		listener, err = net.Listen("tcp", *addr)
		if err != nil {
			log.Fatalf("Failed to bind TCP Address: %v", err)
		}
		log.Printf("FFmpeg Go-gRPC Sidecar listening on TCP: %s", *addr)
	} else {
		// Unix-like: Clean up stale socket files if they exist from previous unexpected crashes
		_ = os.Remove(*addr)

		listener, err = net.Listen("unix", *addr)
		if err != nil {
			log.Fatalf("Failed to bind Unix Domain Socket: %v", err)
		}
		log.Printf("FFmpeg Go-gRPC Sidecar listening on UDS: %s", *addr)
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

		if !useTCP {
			_ = os.Remove(*addr)
		}
		os.Exit(0)
	}()

	log.Printf("FFmpeg Go-gRPC Sidecar serving at %s", *addr)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("gRPC server run failure: %v", err)
	}
}

// StartStream implements the gRPC streaming endpoint with per-target session tracking
func (s *ffmpegServer) StartStream(req *proto.StreamRequest, stream proto.FFmpegService_StartStreamServer) error {
	targetID := req.GetTargetId()
	streamMode := req.GetStreamMode()
	filePath := req.GetFilePath()
	streamContainer := req.GetContainer()

	log.Printf("[SIDECAR] Received StartStream request for target=%s; file: %s; stream mode: %s; stream container: %s", targetID, filePath, streamMode, streamContainer)

	fileInfo, statErr := os.Stat(filePath)
	if os.IsNotExist(statErr) {
		return status.Errorf(codes.NotFound, "media file does not exist: %s", filePath)
	}
	if statErr != nil {
		return status.Errorf(codes.Internal, "failed to inspect media file: %v", statErr)
	}

	var preflightPlan C.CPlaybackSupport
	hasPreflightPlan := false

	// Transcoding must never silently fall back to CPU. The application performs
	// this same preflight before opening the WebSocket, while this server-side
	// check protects callers that bypass the application UI.
	if streamMode == proto.StreamMode_transcode && req.GetProcessingPolicy() == proto.ProcessingPolicy_PROCESSING_GPU_REQUIRED {
		support, ret := probePlaybackSupport(filePath, fileInfo)
		if ret < 0 {
			return status.Errorf(codes.FailedPrecondition, "native GPU playback preflight failed with code: %d", int(ret))
		}
		gpuPathVerified := support.native_gpu_supported != 0 ||
			(support.hardware_decode_supported != 0 &&
				support.hardware_encode_supported != 0 &&
				support.encoder_frame_tested != 0)
		if !gpuPathVerified {
			failureCode := C.GoString(&support.failure_code[0])
			reason := C.GoString(&support.reason[0])
			return status.Errorf(codes.FailedPrecondition, "verified GPU playback unavailable (%s): %s; retry with processing_policy=PROCESSING_ALLOW_CPU after user confirmation", failureCode, reason)
		}
		preflightPlan = support
		hasPreflightPlan = true
		log.Printf("[SIDECAR] Verified GPU playback preflight passed for target=%s (native_zero_copy=%t)", targetID, support.native_gpu_supported != 0)
	} else if streamMode == proto.StreamMode_transcode {
		log.Printf("[SIDECAR] CPU fallback explicitly allowed for target=%s", targetID)
		preflightPlan, hasPreflightPlan = cachedPlaybackSupport(filePath, fileInfo)
		if hasPreflightPlan {
			log.Printf("[SIDECAR] Reusing cached preflight plan for target=%s", targetID)
		}
	}

	// PRE: Handle case where target already has an active playback
	if oldSess, exists := getPlaybackSession(targetID); exists && oldSess != nil {
		log.Printf("[SIDECAR] Target %s already has active playback — replacing it", targetID)

		// Cancel the old streaming context so its handler exits cleanly
		if oldSess.Cancel != nil {
			oldSess.Cancel()
		}

		// Fully unregister (stops audio, frees C memory, deletes handle)
		unregisterPlayback(targetID)

		// Give C pipeline time to exit cleanly
		time.Sleep(400 * time.Millisecond)
	}

	// 1. Create isolated Go session context + cgo handle
	// A small bounded queue is deliberate: it propagates pressure from the
	// browser through gRPC to FFmpeg instead of accumulating hundreds of MiB
	// or dropping partial fMP4 data.
	sessionCtx := &SessionContext{
		StreamChan: make(chan ChunkPayload, 64),
		Done:       make(chan struct{}),
	}

	// Wrap this private instance in a localized runtime Cgo handle
	handle := cgo.NewHandle(sessionCtx)
	// We will delete the handle inside unregisterPlayback

	// Convert streamMode string ("passthrough" vs "transcode") to C flag
	isPassthrough := C.int(0)
	if streamMode == proto.StreamMode_passthrough {
		isPassthrough = C.int(1)
	}

	// Map Protobuf ContainerFormat to C PassthroughContainerType
	var containerType C.PassthroughContainerType = C.CONTAINER_FMP4
	if streamContainer == proto.PassthroughContainerFormat_CONTAINER_WEBM {
		containerType = C.CONTAINER_WEBM
	}

	// 2. Allocate C contexts (we keep explicit frees for early error paths)
	decCtx := (*C.DemuxDecContext)(C.calloc(1, C.size_t(unsafe.Sizeof(C.DemuxDecContext{}))))
	decCtx.is_passthrough = isPassthrough
	decCtx.container_type = containerType
	if hasPreflightPlan {
		C.apply_playback_probe_plan(decCtx, &preflightPlan)
	}

	cPath := C.CString(filePath)
	defer C.free(unsafe.Pointer(cPath))

	// 3. Open decoders (allocated only if !is_passthrough)
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
		FilePath:   filePath,
	}
	// Update the session context
	sessionCtx.Mu.Lock()
	sessionCtx.playbackSession = sess
	sessionCtx.Mu.Unlock()
	registerPlayback(targetID, sess)

	// 5. Start the C pipeline goroutine
	pipelineErrChan := make(chan int, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := make([]byte, 4096)
				n := runtime.Stack(stack, false)
				log.Println(fmt.Sprintf("[%v] PANIC recovered in goroutine: %s", r, stack[:n]))

				// Stop playback
				unregisterPlayback(targetID)
			}
		}()
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

			// CRITICAL: Reaching the end of the file is a successful processing conclusion, not a failure.
			// We skip the error handling block if the code indicates EOF.
			if errCode < 0 && errCode != int(C.GO_AVERROR_EOF) {
				// Real error path -> execute full cleanup
				unregisterPlayback(targetID)
				return status.Errorf(codes.Internal, "C-pipeline processing failed with error code: %d", errCode)
			}

			// Success path (errCode == 0): Do NOT free yet.
			// We intentionally do NOT call unregisterPlayback here.
			// The session stays registered until client disconnect or new request
			// for the same target overwrites it.
			log.Printf("[SIDECAR] Pipeline ended successfully for target %s — waiting for client to finish (audio drain)", targetID)

		case chunk := <-sessionCtx.StreamChan:
			err := stream.Send(&proto.StreamResponse{
				Fmp4Chunk: chunk.Data,
				Pts:       chunk.PTS,
				IsEof:     atomic.LoadInt32(&sessionCtx.IsEOF) != 0, // Thread-safe atomic load
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
	includePacketCounts := C.int(0)
	if req.GetIncludePacketCounts() {
		includePacketCounts = 1
	}
	ret := C.probe_media_properties(cPath, &cProps, includePacketCounts)
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
		AudioPacketCount:   int64(cProps.audio_packet_count),

		CandidateVideoMime: C.GoString(&cProps.candidate_video_mime[0]),
		CandidateAudioMime: C.GoString(&cProps.candidate_audio_mime[0]),
		CandidateFullMime:  C.GoString(&cProps.candidate_full_mime[0]),
	}, nil
}

// GetPlaybackSupport performs a real decoder/encoder preflight without
// starting a playback session. The application uses this before opening the
// WebSocket so a CPU fallback is always an explicit user decision.
func (s *ffmpegServer) GetPlaybackSupport(ctx context.Context, req *proto.PlaybackSupportRequest) (*proto.PlaybackSupportResponse, error) {
	filePath := req.GetFilePath()
	log.Printf("[SIDECAR] Probing playback hardware support for file: %s", filePath)

	fileInfo, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "media file does not exist: %s", filePath)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to inspect media file: %v", err)
	}

	cSupport, ret := probePlaybackSupport(filePath, fileInfo)
	if ret < 0 {
		return nil, status.Errorf(codes.Internal, "FFmpeg playback support probe failed with code: %d", int(ret))
	}

	nativeGPU := cSupport.native_gpu_supported != 0
	hardwareDecode := cSupport.hardware_decode_supported != 0
	hardwareEncode := cSupport.hardware_encode_supported != 0
	zeroCopy := cSupport.zero_copy_supported != 0
	cpuFallback := cSupport.cpu_fallback_available != 0
	failureCode := C.GoString(&cSupport.failure_code[0])
	reason := C.GoString(&cSupport.reason[0])
	decoderBackend := C.GoString(&cSupport.decoder_backend[0])
	encoderBackend := C.GoString(&cSupport.encoder_backend[0])
	recommendation, cpuWarning := playbackSupportGuidance(failureCode, decoderBackend, encoderBackend, nativeGPU)

	statusValue := proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_UNAVAILABLE
	switch {
	case nativeGPU:
		statusValue = proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_NATIVE_GPU
	case hardwareDecode && hardwareEncode:
		statusValue = proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_GPU_ASSISTED
	case cpuFallback:
		statusValue = proto.PlaybackSupportStatus_PLAYBACK_SUPPORT_CPU_FALLBACK
	}

	log.Printf("[SIDECAR] Playback support result: status=%s native_gpu=%t decoder=%s encoder=%s failure=%s", statusValue.String(), nativeGPU, decoderBackend, encoderBackend, failureCode)

	return &proto.PlaybackSupportResponse{
		Status:                  statusValue,
		NativeGpuSupported:      nativeGPU,
		HardwareDecodeSupported: hardwareDecode,
		HardwareEncodeSupported: hardwareEncode,
		ZeroCopySupported:       zeroCopy,
		CpuFallbackAvailable:    cpuFallback,
		EncoderFrameTested:      cSupport.encoder_frame_tested != 0,
		EncoderWidth:            int32(cSupport.encoder_width),
		EncoderHeight:           int32(cSupport.encoder_height),
		OperatingSystem:         runtime.GOOS,
		Architecture:            runtime.GOARCH,
		DecoderBackend:          decoderBackend,
		EncoderBackend:          encoderBackend,
		FailureCode:             failureCode,
		Reason:                  reason,
		Recommendation:          recommendation,
		CpuWarning:              cpuWarning,
	}, nil
}

func playbackSupportGuidance(failureCode, decoderBackend, encoderBackend string, nativeGPU bool) (string, string) {
	if nativeGPU {
		return "Native GPU playback is available.", ""
	}

	var recommendation string
	switch runtime.GOOS {
	case "windows":
		switch failureCode {
		case "gpu_encode_unavailable":
			recommendation = "Install the newest driver supported by the GPU and reboot. This sidecar is built against NVENC API 12.2 for compatibility with older supported drivers; if the probe still fails, the GPU or its driver does not expose a usable H.264 hardware encoder. AMD systems require a current Adrenalin/AMF runtime and Intel systems require a current graphics driver."
		case "gpu_decode_capability":
			recommendation = "Install the newest driver supported by the GPU and verify hardware decoding for this codec/profile, especially HEVC Main 10. A driver update cannot add missing decoder hardware; if this GPU lacks HEVC Main 10 support, use a newer GPU or CPU processing."
		case "zero_copy_unavailable":
			recommendation = "Install the newest supported driver and use a matching native decoder/encoder pair from the same GPU vendor. If the GPU or driver cannot provide that pairing, native zero-copy playback is unavailable on this system."
		default:
			recommendation = "Update the graphics driver, restart the application, and retry the native GPU preflight."
		}
	case "linux":
		recommendation = "Install or update the vendor graphics driver and verify the required VAAPI, NVENC, or AMF runtime is available to the application."
	case "darwin":
		recommendation = "Update macOS and verify that VideoToolbox supports this codec/profile on the current Mac."
	default:
		recommendation = "Verify that the operating-system graphics driver and FFmpeg hardware runtime are installed and current."
	}

	warning := "CPU processing is available, but 4K playback may stutter, buffer slowly, or pause because decoding/encoding will run through system memory."
	if decoderBackend != "" || encoderBackend != "" {
		warning = fmt.Sprintf("CPU processing is available, but the current path (%s decode, %s encode) may not sustain 4K realtime playback.", decoderBackend, encoderBackend)
	}
	return recommendation, warning
}

// GetVideoScreenshot extracts a single frame image out of a media asset layout.
func (s *ffmpegServer) GetVideoScreenshot(ctx context.Context, req *proto.ScreenshotRequest) (*proto.ScreenshotResponse, error) {
	if _, err := os.Stat(req.GetFilePath()); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "media file does not exist: %s", req.GetFilePath())
	}

	timeTargetMs := req.GetTimeMs()
	if timeTargetMs <= 0 {
		timeTargetMs = 1000 // Sane default to bypass initial black frames
	}
	log.Printf("[SIDECAR] Extracting screenshot for file: %s at effective timestamp %d ms (requested %d ms)", req.GetFilePath(), timeTargetMs, req.GetTimeMs())

	cPath := C.CString(req.GetFilePath())
	defer C.free(unsafe.Pointer(cPath))

	var outBuf *C.uint8_t
	var outSize C.int

	// Call the native video screenshot function
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

// ConvertVideo runs the offline conversion pipeline in this sidecar instance.
// The caller uses a dedicated sidecar process so CPU-heavy conversion cannot
// contend with playback decoding or encoding.
func (s *ffmpegServer) ConvertVideo(req *proto.VideoConversionRequest, stream proto.FFmpegService_ConvertVideoServer) error {
	if !s.conversionMu.TryLock() {
		return status.Error(codes.ResourceExhausted, "this sidecar is already processing a video conversion")
	}
	defer s.conversionMu.Unlock()
	if req.GetJobId() == "" || req.GetSourcePath() == "" || req.GetOutputPath() == "" {
		return status.Error(codes.InvalidArgument, "job_id, source_path, and output_path are required")
	}
	if req.GetVideoBitrateKbps() < 100 || req.GetVideoBitrateKbps() > 100000 ||
		req.GetAudioBitrateKbps() < 32 || req.GetAudioBitrateKbps() > 2048 {
		return status.Error(codes.InvalidArgument, "conversion bitrates are outside the supported range")
	}
	switch req.GetEncoderPreset() {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow":
	default:
		return status.Error(codes.InvalidArgument, "unsupported H.264 encoder preset")
	}
	if req.GetHdrHandling() != "tone_map" && req.GetHdrHandling() != "preserve" {
		return status.Error(codes.InvalidArgument, "hdr_handling must be tone_map or preserve")
	}
	sourcePath, err := filepath.Abs(filepath.Clean(req.GetSourcePath()))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid source path: %v", err)
	}
	outputPath, err := filepath.Abs(filepath.Clean(req.GetOutputPath()))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid output path: %v", err)
	}
	if sourcePath == outputPath {
		return status.Error(codes.InvalidArgument, "output path must differ from source path")
	}
	info, err := os.Stat(sourcePath)
	if err != nil || !info.Mode().IsRegular() {
		return status.Errorf(codes.NotFound, "video source is unavailable: %s", sourcePath)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return status.Errorf(codes.Internal, "could not create conversion output directory: %v", err)
	}
	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		return status.Errorf(codes.Internal, "could not replace conversion output: %v", err)
	}
	conversionSucceeded := false
	defer func() {
		if !conversionSucceeded {
			_ = os.Remove(outputPath)
		}
	}()

	cancelFlag := (*C.int)(C.calloc(1, C.size_t(unsafe.Sizeof(C.int(0)))))
	if cancelFlag == nil {
		return status.Error(codes.ResourceExhausted, "could not allocate conversion control state")
	}
	defer C.free(unsafe.Pointer(cancelFlag))
	watcherDone := make(chan struct{})
	watcherExited := make(chan struct{})
	go func() {
		defer close(watcherExited)
		select {
		case <-stream.Context().Done():
			C.request_video_conversion_cancel(cancelFlag)
		case <-watcherDone:
		}
	}()
	defer func() {
		close(watcherDone)
		<-watcherExited
	}()

	progressState := &conversionProgressContext{ctx: stream.Context(), stream: stream}
	progressHandle := cgo.NewHandle(progressState)
	defer progressHandle.Delete()
	cSource := C.CString(sourcePath)
	cOutput := C.CString(outputPath)
	cPreset := C.CString(req.GetEncoderPreset())
	defer C.free(unsafe.Pointer(cSource))
	defer C.free(unsafe.Pointer(cOutput))
	defer C.free(unsafe.Pointer(cPreset))
	errBuffer := make([]C.char, 1024)
	encoderNameBuffer := make([]C.char, 64)
	hardwareAccelerated := C.int(0)
	preserveHDR := C.int(0)
	if req.GetHdrHandling() == "preserve" {
		preserveHDR = 1
	}
	allowHardwareEncoder := C.int(1)
	if os.Getenv("GOTEDO_TEST_MODE") == "1" && os.Getenv("GOTEDO_FFMPEG_FORCE_CPU_ENCODER") == "1" {
		allowHardwareEncoder = 0
	}
	ret := C.convert_video_file(cSource, cOutput,
		C.int(req.GetVideoBitrateKbps()), C.int(req.GetAudioBitrateKbps()),
		cPreset, preserveHDR, allowHardwareEncoder, cancelFlag,
		(C.VideoConversionProgressCallback)(C.sidecar_go_conversion_progress),
		C.uintptr_t(progressHandle), &encoderNameBuffer[0], C.size_t(len(encoderNameBuffer)),
		&hardwareAccelerated, &errBuffer[0], C.size_t(len(errBuffer)))
	if ret < 0 {
		if stream.Context().Err() != nil {
			return status.FromContextError(stream.Context().Err()).Err()
		}
		if progressState.err != nil {
			return progressState.err
		}
		return status.Errorf(codes.Internal, "%s", C.GoString(&errBuffer[0]))
	}
	if err := stream.Send(&proto.VideoConversionProgress{
		Progress: 100, Stage: "completed", Completed: true, OutputPath: outputPath,
		EncoderName: C.GoString(&encoderNameBuffer[0]), HardwareAccelerated: hardwareAccelerated != 0,
	}); err != nil {
		return err
	}
	conversionSucceeded = true
	return nil
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
	sess, ok := getPlaybackSession(targetID)

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
		if sess.DecCtx != nil && sess.SessionCtx != nil {
			// If playback is current at EOF
			if atomic.LoadInt32(&sess.SessionCtx.IsEOF) != 0 {
				// Trigger an atomic seek to 0 ms.
				// This sets dec_ctx->seek_requested = 1 in C, breaking the EOF idle loop.
				C.request_seek_on_dec_ctx(sess.DecCtx, C.int64_t(0))
				atomic.StoreInt32(&sess.SessionCtx.IsEOF, int32(0))
			}
			// Unpause the session
			C.set_dec_ctx_paused(sess.DecCtx, C.int(0))
			// A user resume must not stay stuck behind an old throttle.
			C.set_dec_ctx_backpressure_paused(sess.DecCtx, C.int(0))
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
		// Multiply before casting to avoid losing precision.
		seekMs := int64(req.GetSeekSeconds() * 1000)
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

	case proto.ControlRequest_BACKPRESSURE_PAUSE:
		if sess.DecCtx != nil {
			C.set_dec_ctx_backpressure_paused(sess.DecCtx, C.int(1))
		}
		log.Printf("[SIDECAR] BACKPRESSURE paused target: %s", targetID)
		return &proto.ControlResponse{
			Success: true,
			Message: "backpressure pause",
		}, nil

	case proto.ControlRequest_BACKPRESSURE_RESUME:
		if sess.DecCtx != nil {
			C.set_dec_ctx_backpressure_paused(sess.DecCtx, C.int(0))
		}
		log.Printf("[SIDECAR] BACKPRESSURE resumed target: %s", targetID)
		return &proto.ControlResponse{
			Success: true,
			Message: "backpressure resume",
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
	log.Printf("[VIDEO STOP TRACE] sidecar-stop-rpc-received target=%s", targetID)
	unregisterPlayback(targetID)
	log.Printf("[VIDEO STOP TRACE] sidecar-stop-rpc-complete target=%s", targetID)
	return &proto.StreamControlResponse{Success: true, Message: "stream stopped"}, nil
}
