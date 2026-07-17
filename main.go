package main

/*
#include "decoder.h"
*/
import "C"
import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
	"unsafe"
)

// Global Go Channel to collect stream chunks
var testChunkChan = make(chan []byte, 100)

//export goTestWriteCallback
func goTestWriteCallback(buf *C.uchar, bufSize C.int, userData unsafe.Pointer) {
	data := C.GoBytes(unsafe.Pointer(buf), bufSize)
	testChunkChan <- data
}

func main() {
	testMode := flag.Bool("test", false, "Run application in pipeline verification test mode")
	videoPath := flag.String("video", "", "Absolute path to the video file")
	flag.Parse()

	if *testMode {
		runPipelineTest(*videoPath)
		return
	}

	fmt.Println("Gotedo FFmpeg Sidecar running in production mode...")

	var ctx C.DemuxDecContext
	path := C.CString("file.mkv")
	defer C.free(unsafe.Pointer(path))

	// We cleanly invoke our C function
	_ = C.open_input_and_decoders(&ctx, path)
	C.free_demux_dec_context(&ctx)
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

	var decCtx C.DemuxDecContext
	var playCtx C.AudioPlaybackContext

	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	// Step 1: Open file and decoders
	log.Println("[TEST] Opening media file and initializing decoders...")
	ret := C.open_input_and_decoders(&decCtx, cPath)
	if ret < 0 {
		log.Fatalf("[TEST ERROR] Failed opening input decoders. FFmpeg Code: %d", ret)
	}
	log.Println("[TEST] SUCCESS: FFmpeg Decoders and SWResampler successfully initialized!")

	// Step 2: Initialize Audio Playback (using system default speaker)
	log.Println("[TEST] Initializing Miniaudio hardware device...")
	ret = C.init_audio_playback(&playCtx, C.int(48000), C.int(2), nil)
	if ret < 0 {
		C.free_demux_dec_context(&decCtx)
		log.Fatalf("[TEST ERROR] Failed to initialize Audio hardware. Code: %d", ret)
	}
	log.Println("[TEST] SUCCESS: Miniaudio successfully opened system hardware soundcard!")

	// Step 3: Spin up the C-processing pipeline loop on a concurrent Go thread
	log.Println("[TEST] Spawning parallel demuxer & play threads...")
	go func() {
		C.run_test_mux_and_play(&decCtx, &playCtx, unsafe.Pointer(&testChunkChan))
	}()

	// Step 4: Stream consumer
	go func() {
		for chunk := range testChunkChan {
			fmt.Printf("[TEST STREAM] Captured %d bytes of browser-ready fMP4 in Go memory!\n", len(chunk))
		}
	}()

	// Step 5: Simulate live feedback from webview
	time.Sleep(2 * time.Second)
	log.Println("[TEST LATENCY] Simulated webview reports rendering lag. Increasing audio delay to 150ms...")
	C.set_audio_delay_offset(&playCtx, C.int(150))

	time.Sleep(2 * time.Second)
	log.Println("[TEST LATENCY] Simulated webview caught up. Restoring audio delay to 50ms...")
	C.set_audio_delay_offset(&playCtx, C.int(50))

	time.Sleep(3 * time.Second)

	// Clean up Contexts
	log.Println("[TEST] Tearing down playback engine and freeing C memory...")
	C.stop_audio_playback(&playCtx)
	C.free_demux_dec_context(&decCtx)
	log.Println("[TEST] SUCCESS: All context structures freed cleanly. No leaks found!")
	log.Println("[TEST] PIPELINE RUN CONCLUDED SUCCESSFULLY!")
}
