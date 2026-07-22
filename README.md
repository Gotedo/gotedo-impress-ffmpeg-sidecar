# FFmpeg Sidecar for Gotedo Impress

This repository contains the **FFmpeg Sidecar for Gotedo Impress**—a high-performance media processing microservice designed to interface directly with custom-built FFmpeg dynamic libraries.

At its core is a high-performance, embedded C and FFmpeg streaming library designed for native desktop applications (e.g., Wails, Electron, CGO bridges) that need to deliver local media files to webview frontend players via Media Source Extensions (MSE) over WebSockets or HTTP streams.

To maintain compliance with the **GPL/LGPL license requirements** (due to the use of FFmpeg) while keeping the parent application closed-source, this sidecar runs as an isolated background process and communicates with the main application strictly over network boundaries (via **gRPC** or **WebSockets**). Consequently, this sidecar and its complete build system are fully open-source.

---

## GPL Compliance

* **Process Isolation:** The sidecar links directly (and dynamically) to our custom-built FFmpeg libraries.
* **Network Boundary:** Your proprietary application communicates with this sidecar using gRPC or WebSockets. Because they run in separate execution domains and share no memory, your main application remains 100% proprietary and compliant under GPL/LGPL rules.
* **Reproducible Builds:** This repository includes the exact `Dockerfile` and compiler configuration flags (`builder.sh`) used to build FFmpeg, fulfilling the "Corresponding Source" requirements of copyleft licenses.

## Architecture & Data Flow

The engine acts as an dynamic demuxing, remuxing, and transcoding pipeline. It converts arbitrary local media containers (MKV, MP4, MOV, AVI) on the fly into fragmented MP4 (`fMP4`) atoms and pushes them in real-time to the frontend.

```

+-----------------------------------------------------------------------------------------+
|                                    NATIVE C PIPELINE                                    |
|                                                                                         |
|  +------------------+     +-----------------------+     +----------------------------+  |
|  | Input Media File | --> | Demuxer (libavformat) | --> | Video Passthrough / H.264  |  |
|  +------------------+     +-----------------------+     | Encoder (libx264)          |  |
|                                                         +----------------------------+  |
|                                                                       |                 |
|                                                                       v                 |
|  +------------------+     +-----------------------+     +----------------------------+  |
|  | WebSocket Bridge | <-- | 64KB AVIO Memory Buffer|<-- | fMP4 Muxer                 |  |
|  | (Go/CGO Callback)|     +-----------------------+     | (empty_moov + frag_keyframe|  |
|  +------------------+                                   +----------------------------+  |
|           |                                                           ^                 |
|           |                                                           |                 |
|           |               +-----------------------+     +----------------------------+  |
|           +-------------> | Resampler (swresample)| --> | Audio Transcoder           |  |
|                           | (48kHz FLTP Stereo)   |     | (AAC + AVAudioFifo)        |  |
|                           +-----------------------+     +----------------------------+  |
+-----------------------------------------------------------------------------------------+
|
v (Binary WebSockets)
+-----------------------------------------------------------------------------------------+
|                                  WEBVIEW FRONTEND (MSE)                                 |
|                                                                                         |
|  +--------------------+    +--------------------+    +-------------------------------+  |
|  | WebSocket Receiver | -> | SourceBuffer Queue | -> | HTML5 Media Player ()  |  |
|  +--------------------+    +--------------------+    +-------------------------------+  |
+-----------------------------------------------------------------------------------------+

```

---

## Real-Time Pacing & Read-Ahead Logic

To minimize CPU overhead and prevent unbounded memory growth on long videos, the C pipeline incorporates a **Wall-Clock Pacing Controller**. It limits media delivery to a fixed 10-second read-ahead runway (`READ_AHEAD_US`) ahead of the client's current playback head.

### Pacing Timeline vs. Buffer Runway

```text
Time (s)
   0s            5s           10s           15s           20s           25s
---|-------------|-------------|-------------|-------------|-------------|--->
                 ^                           ^
           Player Playhead             Buffer Runway
          (currentTime = 5s)        (Max Read-Ahead = 15s)
                 |                           |
                 +===========================+
                    10-second Active Buffer
               (Pipeline sleeps if > 15s)

```

### Buffer Pacing Line Chart

```text
 Buffer Runway (Seconds)
  12s |                     /---\         /---\        (Read-Ahead Ceiling: 10s)
  10s |-------------------/-------X-----/-------X--------------------------------
   8s |                 /           \ /           \
   6s |               /              |             \
   4s |             /                |              \  (Paced Consumption Zone)
   2s |           /                  |               \
   0s +---------+--------------------+----------------+-------------------------
             0s                    10s              20s         Playback Time (s)
              
      Legend: 
        /  = Demuxer/Muxer actively processing frames
        X  = Pacing Throttle Triggered (av_usleep pause)
        \  = Frontend consuming buffer during playback

```

---

## Functionalities & Key Features

* **Zero-CPU Video Passthrough**: If the source video track is natively H.264 (`AV_CODEC_ID_H264`), the engine bypasses video re-encoding completely, maintaining 0% CPU usage and zero quality loss.
* **On-the-Fly H.264 Re-Encoding**: Automatically falls back to a low-latency H.264 encoder (`preset=veryfast`, `tune=zerolatency`, `profile=main`) for non-H.264 formats (VP9, HEVC, AV1).
* **JIT AAC Audio Standardization**: Resamples incoming multi-channel, multi-rate audio into a standard 48kHz Stereo FLTP AAC stream, dynamically managed by an `AVAudioFifo` ring buffer to guarantee perfect A/V synchronization.
* **Atom-Safe AVIO Memory Buffering**: Employs a 64KB dynamic `AVIOContext` memory buffer that prevents atom shredding across WebSocket frames, resolving browser `MEDIA_ERR_DECODE` (Code 3) errors.
* **Atomic Thread-Safe Controls**: Supports pause, resume, seek, and stop operations using non-blocking atomic operations (`__atomic_load_n`/`__atomic_store_n`), preventing race conditions with `av_read_frame`.
* **Seamless Seeking & Re-initialization**: Recreates encoder lookahead threads, flushes FIFO structures, and issues new `moov` initialization headers upon seeking to safely reset browser MSE engines without reloading the player.
* **Clean Stream Termination**: Flushes final P/B frames and writes the `fMP4` trailer at EOF to prevent end-of-video truncation.
* **Compile-Time Tagged Logging**: Built-in tiered logging (`C-MUX`, `C-READ`, `C-SEEK`, `C-PROBE`) with environment-based stripping (`-DPRODUCTION`) to separate development traces from production builds.

---

## Core Assumptions

1. **Client Environment**: The receiving webview or browser supports standard HTML5 Media Source Extensions (MSE) with the codec descriptor `video/mp4; codecs="avc1.4d401f, mp4a.40.2"`.
2. **C Runtime & Toolchain**: Modern C99-compliant compiler with POSIX atomic primitives (`__atomic_*`) or GCC/Clang built-ins.
3. **FFmpeg Libraries**: Linked against `libavformat`, `libavcodec`, `libswresample`, `libswscale`, and `libavutil` (v5.0+ recommended).
4. **Binary Data Transport**: The Sidecar host handles binary transport over WebSockets or gRPC streams using standard byte array callbacks.

---

## Technical Trade-offs & Engineering Decisions

| Feature / Architecture | Chosen Approach | Trade-off / Alternative | Engineering Rationale |
| --- | --- | --- | --- |
| **Video Processing** | Hybrid Passthrough + Re-encode Fallback | Universal Full Re-encoding | Preserves battery life and zero CPU usage on H.264 files while guaranteeing playback compatibility for foreign codecs. |
| **Muxing Memory Buffer** | 64KB `AVIOContext` Buffer | 4KB Default Buffer | Eliminates MSE bitstream corruption (`MEDIA_ERR_DECODE`) caused by splitting video keyframes across tiny WebSocket chunks. |
| **Threading Model** | Single Loop with Atomic Flags | Multi-threaded Lock/Mutex | Avoids deadlock scenarios inside FFmpeg functions like `av_read_frame()` while maintaining sub-millisecond reaction times to user inputs. |
| **Audio Resampling** | Dynamic FIFO Alignment (`AVAudioFifo`) | Fixed Sample Pushing | Prevents sample dropping under load, which causes micro-desyncs that crash browser audio pipelines over long playback sessions. |
| **Stream Termination** | Mid-Stream EOF Trailer Write | Termination on EOF | Writing the trailer at EOF pushes the trailing 3–5 seconds of P/B frames before idling, preventing early video cutoffs. |

---

## C API Reference & Usage

### Core Data Structures

```c
// Decoder and Pipeline Context
typedef struct DemuxDecContext {
    AVFormatContext *fmt_ctx;
    int video_stream_idx;
    AVCodecContext *video_dec_ctx;
    int audio_stream_idx;
    AVCodecContext *audio_dec_ctx;
    SwrContext *swr_ctx;
    
    // Thread-safe control flags
    volatile int paused;
    volatile int64_t seek_target_ms;
    volatile int seek_requested;
    volatile int stop_requested;
    volatile int eof_flushed;
} DemuxDecContext;

```

### Basic Pipeline Execution

```c
#include "decoder.h"

int main() {
    DemuxDecContext dec_ctx = {0};
    uintptr_t go_user_token = 1; // Passed to callback functions

    // 1. Open media file and probe streams
    if (open_input_and_decoders(&dec_ctx, "/path/to/media.mp4") < 0) {
        return -1;
    }

    // 2. Launch the streaming pipeline (blocking execution loop)
    // Runs until stop_requested is set or stream completes
    run_streaming_mux_and_play(&dec_ctx, go_user_token);

    // 3. Clean up context resources
    free_demux_dec_context(&dec_ctx);
    return 0;
}

```

### Controlling Playback at Runtime

```c
// Pause playback
set_dec_ctx_paused(&dec_ctx, 1);

// Resume playback
set_dec_ctx_paused(&dec_ctx, 0);

// Request seek to 45.5 seconds (45500 ms)
request_seek_on_dec_ctx(&dec_ctx, 45500);

// Stop streaming loop cleanly
request_stop_on_dec_ctx(&dec_ctx);

```

---

## CGO & Go Host Integration

### 1. Build Tag Configuration

To enable debug logs during development and strip them out for production builds, pass the compile tag via `CGO_CFLAGS`:

```bash
# Development Build (With detailed C-MUX / C-READ logs)
go build -tags development .

# Production Build (Debug logs stripped at compile-time)
CGO_CFLAGS="-DPRODUCTION" go build -tags production .

```

### 2. Exported CGO Callback Interface

```go
package main

/*
#include "decoder.h"
*/
import "C"
import (
	"unsafe"
	"[github.com/gorilla/websocket](https://github.com/gorilla/websocket)"
)

//export goStreamWriteCallback
func goStreamWriteCallback(buf *C.uint8_t, bufSize C.int, userToken C.uintptr_t) {
	gobuf := C.GoBytes(unsafe.Pointer(buf), bufSize)
	
	// Retrieve active WebSocket session using userToken
	session := getSession(uintptr(userToken))
	if session != nil && session.WS != nil {
		session.WS.WriteMessage(websocket.BinaryMessage, gobuf)
	}
}

```

---

## Webview Frontend Integration (Vue 3 / JavaScript)

```typescript
// Initialize MSE Pipeline in Webview
const mediaSource = new MediaSource();
const videoElement = document.querySelector('video');
videoElement.src = URL.createObjectURL(mediaSource);

mediaSource.addEventListener('sourceopen', () => {
  const mimeCodec = 'video/mp4; codecs="avc1.4d401f, mp4a.40.2"';
  const sourceBuffer = mediaSource.addSourceBuffer(mimeCodec);
  sourceBuffer.mode = 'segments';

  const ws = new WebSocket('ws://localhost:8080/stream');
  ws.binaryType = 'arraybuffer';

  const chunkQueue: ArrayBuffer[] = [];
  let isAppending = false;

  const processQueue = () => {
    if (!sourceBuffer.updating && chunkQueue.length > 0) {
      isAppending = true;
      const chunk = chunkQueue.shift();
      sourceBuffer.appendBuffer(chunk);
    }
  };

  sourceBuffer.addEventListener('updateend', () => {
    isAppending = false;
    processQueue();
  });

  ws.onmessage = (event: MessageEvent) => {
    chunkQueue.push(event.data as ArrayBuffer);
    processQueue();
  };
});

```

---

## Prerequisites

Before building, ensure your host system has the following installed:

1. **Docker** (Desktop or Engine) with **Buildx support** enabled.
2. **Go (v1.18 or later)** (to compile the task runner locally).
3. **Hardware virtualization** enabled in your BIOS/OS (required by Docker).

---

## Quick Start: Build FFmpeg

The build system is entirely self-contained. It compiles the task runner locally so you do not have to install global build utilities on your system.

### 1. Setup Your Computer

1. Install Go on your computer. Follow the instructions at: https://go.dev/doc/install.
2. Install Git on your computer. Follow the instructions at: https://git-scm.com/install.
3. Install Docker on your computer. Following the instructions at: https://docs.docker.com/desktop/setup/install/windows-install.
4. Clone this repository:

    ```bash
    git clone https://github.com/Gotedo/gotedo-impress-ffmpeg-sidecar.git
    ```

### 2. Initialize & Install the Build Runner

From the root of this repository, run the following command to download and compile the `task` utility locally inside your workspace:

```bash
#1. Go into the directory of the downloaded repository.
cd gotedo-impress-ffmpeg-sidecar

# 2. Initialize the Go module
go mod tidy

# 3. Compile and install 'go-task' into the local repository
GOBIN="$(pwd)/bin" go install github.com/go-task/task/v3/cmd/task@latest

```

*(This compiles the `task` executable and places it securely under `./bin/` which is ignored by Git).*

### 3. Compile FFmpeg (All Targets)

To trigger the complete cross-compilation pipeline (this will pull the toolchains, run an APT caching proxy to speed up dependencies, and build FFmpeg for Linux, macOS, and Windows):

```bash
./bin/task build:ffmpeg

```

---

## Customizing the Build Target

By default, running the build task compiles libraries for all supported platforms: `windows,linux,darwin` across `amd64,arm64` architectures.

You can target a specific operating system and architecture by passing environmental overrides to the task runner:

### Build for Windows AMD64 Only

```bash
GOOS_VAR="windows" ARCH_VAR="amd64" ./bin/task build:ffmpeg

```

### Build for Apple Silicon (macOS M-series) Only

```bash
GOOS_VAR="darwin" ARCH_VAR="arm64" ./bin/task build:ffmpeg

```

### Build Multiple Selected Platforms

```bash
GOOS_VAR="linux,windows" ARCH_VAR="amd64" ./bin/task build:ffmpeg

```

---

## Build Artifacts Directory

Once a build successfully finishes, compiled outputs, header files, and shared binaries are deposited into the local `dist/` directory structured by platform:

```text
dist/
├── linux/
│   ├── amd64/          # Shared .so libraries and headers
│   └── arm64/
├── darwin/
│   ├── amd64/          # Shared .dylib libraries and headers
│   └── arm64/
└── windows/
    └── amd64/          # Shared .dll binaries, .lib files, and headers

```

---

## Cleaning Build Caches

The compilation environment caches heavy dependencies (like toolchains and package managers) to keep subsequent builds extremely fast. If you need to wipe these caches and force a completely clean build, run:

```bash
# Wipe the build caching structures
rm -rf build_cache/ dist/

```

---

## Run Tests

To run integration tests for this sidecar, do:

```bash
/bin/task build IS_TEST=true
```

---

## Licensing

The code in this repository (the sidecar interface, wrappers, and build configurations) is open-source. The compiled binaries produced by this build system link against **FFmpeg**, which is licensed under the **GNU Lesser General Public License (LGPL) v2.1** or the **GNU General Public License (GPL) v3** depending on compilation flags used (e.g. `--enable-gpl`).

By keeping this sidecar repository open-source and providing full build instructions (via the local `Dockerfile` and `Taskfile.yml`), we fully satisfy our open-source compliance commitments.