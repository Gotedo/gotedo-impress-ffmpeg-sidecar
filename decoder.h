#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>
#include <libavutil/dict.h>
#include <libavutil/pixdesc.h>
#include <libavutil/time.h>
#include <libavutil/audio_fifo.h>
#include <libavutil/error.h>
#include <libavutil/opt.h>
#include <libavutil/channel_layout.h>
#include <libavutil/imgutils.h>
#include <libavutil/log.h>
#include <libswscale/swscale.h>
#include <miniaudio.h>
#include <string.h>
#include <stdlib.h>
#include <stdbool.h>

// Evaluates complex macros inside C space and assigns them to tokens CGO can read
enum
{
  // Native FFmpeg Error Codes
  GO_AVERROR_EOF = AVERROR_EOF,                   // End of file / stream
  GO_AVERROR_EAGAIN = AVERROR(EAGAIN),            // Resource temporarily unavailable (try again)
  GO_AVERROR_INVALIDDATA = AVERROR_INVALIDDATA,   // Invalid data found when processing input
  GO_AVERROR_BUG = AVERROR_BUG,                   // Internal bug detected
  GO_AVERROR_UNKNOWN = AVERROR_UNKNOWN,           // Unknown error (typically from external libraries)
  GO_AVERROR_EXIT = AVERROR_EXIT,                 // Immediate exit was requested by the application
  GO_AVERROR_PATCHWELCOME = AVERROR_PATCHWELCOME, // Feature not implemented yet, patches welcome

  // Component Discovery Errors
  GO_AVERROR_DECODER_NOT_FOUND = AVERROR_DECODER_NOT_FOUND,   // Decoder not found for the requested stream
  GO_AVERROR_ENCODER_NOT_FOUND = AVERROR_ENCODER_NOT_FOUND,   // Encoder not found for the requested codec
  GO_AVERROR_DEMUXER_NOT_FOUND = AVERROR_DEMUXER_NOT_FOUND,   // Demuxer not found for the input format
  GO_AVERROR_MUXER_NOT_FOUND = AVERROR_MUXER_NOT_FOUND,       // Muxer not found for the output format
  GO_AVERROR_PROTOCOL_NOT_FOUND = AVERROR_PROTOCOL_NOT_FOUND, // Protocol driver not found (e.g., rtsp, http)
  GO_AVERROR_FILTER_NOT_FOUND = AVERROR_FILTER_NOT_FOUND,     // Audio/Video filter not found
  GO_AVERROR_BSF_NOT_FOUND = AVERROR_BSF_NOT_FOUND,           // Bitstream filter not found

  // Buffer and Configuration Errors
  GO_AVERROR_BUFFER_TOO_SMALL = AVERROR_BUFFER_TOO_SMALL, // Provided data buffer size is too small
  GO_AVERROR_OPTION_NOT_FOUND = AVERROR_OPTION_NOT_FOUND, // Specified AVOption field was not found

  // Common POSIX-Wrapped I/O Errors
  GO_AVERROR_EIO = AVERROR(EIO),            // Generic Input/Output error
  GO_AVERROR_ENOMEM = AVERROR(ENOMEM),      // Cannot allocate memory / Out of memory
  GO_AVERROR_EINVAL = AVERROR(EINVAL),      // Invalid argument passed to function
  GO_AVERROR_ENOENT = AVERROR(ENOENT),      // No such file or directory found
  GO_AVERROR_ETIMEDOUT = AVERROR(ETIMEDOUT) // Connection or I/O operation timed out
};

typedef struct DemuxDecContext
{
  AVFormatContext *fmt_ctx;

  // Video Decoder State
  int video_stream_idx;
  AVCodecContext *video_dec_ctx;

  // Audio Decoder State
  int audio_stream_idx;
  AVCodecContext *audio_dec_ctx;

  // Resampler State
  SwrContext *swr_ctx;

  // Target parameters
  int target_sample_rate;
  AVChannelLayout target_ch_layout;
  enum AVSampleFormat target_sample_fmt;

  // Robust Streaming Control States
  // These fields allow the background demux/remux goroutine to react to
  // pause, seek, and stop commands from the Go side in a thread-safe manner
  // without races on av_read_frame / av_seek_frame.
  // Decision: Use volatile + atomic operations for simple flags.
  // For seek_target we accept a small race window because seeking is infrequent.
  volatile int paused;             // 1 = pipeline should stop pushing new data
  volatile int64_t seek_target_ms; // Target time when seek_requested is set
  volatile int seek_requested;     // Set to 1 by Go to request a seek
  volatile int stop_requested;     // Clean exit signal from Go
  volatile int eof_flushed;        // Indicates if EOF is reached and pipeline flushed; 1 = Flushed
} DemuxDecContext;

typedef struct TranscodeContext
{
  uintptr_t go_user_token;
  // Holds the current frame's PTS context dynamically
  double current_pts;
  void (*go_callback)(uint8_t *buf, int buf_size, uintptr_t user_token);
} TranscodeContext;

// Struct mapping exactly to our proto/Go expectations
typedef struct
{
  char id[128];
  char name[256];
  bool is_default;
} NativeAudioDevice;

// Media properties schema
typedef struct
{
  char format_name[64];
  char format_long_name[128];
  int64_t duration_ms;
  int64_t file_size_bytes;
  int64_t bit_rate;

  char title[128];
  char author[128];
  char album[128];
  char track[16];
  char genre[64];
  char creation_time[64];
  char last_modified[64];

  int32_t has_video;
  char video_codec[32];
  char video_codec_long_name[128];
  char video_profile[64];
  int32_t width;
  int32_t height;
  double framerate;
  char aspect_ratio[16];
  char pixel_format[32];
  char color_space[32];
  char color_transfer[32];
  char color_primaries[32];

  int32_t has_audio;
  char audio_codec[32];
  char audio_codec_long_name[128];
  char audio_profile[64];
  int32_t audio_channels;
  int32_t sample_rate;
  char channel_layout[64];
  int64_t audio_bit_rate;
} CMediaProperties;

// Function prototypes
int init_audio_resampler(DemuxDecContext *ctx);
int open_input_and_decoders(DemuxDecContext *ctx, const char *input_path);
void free_demux_dec_context(DemuxDecContext *ctx);

int init_fmp4_muxer(AVFormatContext **out_fmt_ctx, TranscodeContext *tx_ctx);
int write_fmp4_header(AVFormatContext *out_fmt_ctx);
void free_fmp4_muxer(AVFormatContext *out_fmt_ctx);

// Queries miniaudio context and populates devices buffer up to max_devices.
// Returns total number of devices found, or a negative error code.
int get_miniaudio_devices(NativeAudioDevice *devices, int max_devices);

// Probes a file and fills out the CMediaProperties memory layout
int probe_media_properties(const char *file_path, CMediaProperties *props);

// Extract a compressed image frame into a dynamically allocated buffer
int extract_video_screenshot(const char *file_path, int64_t time_ms, uint8_t **out_buf, int *out_size);

int run_production_mux_and_play(DemuxDecContext *dec_ctx, uintptr_t go_token);

// Control functions for runtime playback control
int seek_playback(DemuxDecContext *dec_ctx, int64_t seek_time_ms);

/**
 * ROBUST INCREMENTAL STREAMING PIPELINE
 *
 * This function implements a proper streaming model combining the best aspects
 * of paced remuxing, just-in-time audio, and controllable background processing.
 *
 * Key Design Decisions & Rationale:
 * - Uses PTS read-ahead window (Option 1) so we only remux video ~8-12 seconds
 *   ahead of what the browser is currently rendering. This prevents wasting
 *   CPU/disk on unwatched parts of long videos.
 * - Audio is decoded just-in-time (Option 2): before decoding an audio packet
 *   we check ring buffer free space. If low, we sleep. This naturally throttles
 *   the entire pipeline to roughly real-time speed.
 * - Control flags in DemuxDecContext (Option 3 style) allow clean pause/seek/stop
 *   while the loop is running. The loop checks these flags frequently.
 * - The goroutine stays alive until explicit stop or client disconnect.
 *   It does NOT exit on EOF while the client is still connected.
 * - Audio processing is stopped exactly when paused == 1. This guarantees
 *   A/V sync on resume (no audio "running ahead" while video is frozen).
 *
 * Merits:
 *   + Excellent CPU efficiency for long videos (only processes what is needed).
 *   + True incremental streaming behavior.
 *   + Pause exactly freezes both video delivery and audio decoding.
 *   + Seek works cleanly even while pipeline is active.
 *   + Backpressure via channel size + PTS window.
 *
 * Demerits / Trade-offs:
 *   - Slightly more complex than the original burst version.
 *   - Requires the frontend to regularly report current PTS via set_session_pts
 *     (already happening).
 *   - On very high bitrate video with tiny GOP, the read-ahead may still send
 *     a few MB quickly, but far less than the entire file.
 */
int run_streaming_mux_and_play(DemuxDecContext *dec_ctx, uintptr_t go_token);

// Control flag setters (called from Go to signal the streaming loop)
void set_dec_ctx_paused(DemuxDecContext *ctx, int paused);
void request_seek_on_dec_ctx(DemuxDecContext *ctx, int64_t seek_ms);
void request_stop_on_dec_ctx(DemuxDecContext *ctx);

#endif // DECODER_H