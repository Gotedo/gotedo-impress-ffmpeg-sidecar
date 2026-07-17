#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>
#include <miniaudio.h>
#include <stdlib.h>
#include <stdbool.h>

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
} DemuxDecContext;

typedef struct TranscodeContext
{
  uintptr_t go_user_token;
  void (*go_callback)(uint8_t *buf, int buf_size, uintptr_t user_token);
} TranscodeContext;

// Native Playback Context
typedef struct AudioPlaybackContext
{
  ma_pcm_rb ring_buffer;
  ma_device device;
  int sample_rate;
  int channels;
  volatile int is_active;

  // Thread-Safe Latency Management Variables
  volatile int target_delay_ms;       // Desired delay targeted by Go feedback loop
  volatile int current_delay_samples; // Cumulative live sample offset balance
} AudioPlaybackContext;

// Struct mapping exactly to our proto/Go expectations
typedef struct
{
  char id[128];
  char name[256];
  bool is_default;
} NativeAudioDevice;

// Function prototypes
int init_audio_resampler(DemuxDecContext *ctx);
int open_input_and_decoders(DemuxDecContext *ctx, const char *input_path);
void free_demux_dec_context(DemuxDecContext *ctx);

int init_fmp4_muxer(AVFormatContext **out_fmt_ctx, TranscodeContext *tx_ctx);
int write_fmp4_header(AVFormatContext *out_fmt_ctx);
void free_fmp4_muxer(AVFormatContext *out_fmt_ctx);

int init_audio_playback(AudioPlaybackContext *play_ctx, int sample_rate, int channels, const char *device_id);
int write_pcm_to_ring_buffer(AudioPlaybackContext *play_ctx, const float *pcm_data, int frame_count);
void stop_audio_playback(AudioPlaybackContext *play_ctx);

void set_audio_delay_offset(AudioPlaybackContext *play_ctx, int delay_ms);

// Test Pipeline Declaration
int run_test_mux_and_play(DemuxDecContext *dec_ctx, AudioPlaybackContext *play_ctx, uintptr_t go_token);

// Queries miniaudio context and populates devices buffer up to max_devices.
// Returns total number of devices found, or a negative error code.
int get_miniaudio_devices(NativeAudioDevice *devices, int max_devices);

#endif // DECODER_H