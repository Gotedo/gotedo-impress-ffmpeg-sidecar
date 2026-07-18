#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>
#include <libavutil/dict.h>
#include <libavutil/pixdesc.h>
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
  // Holds the current frame's PTS context dynamically
  double current_pts;
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

int init_audio_playback(AudioPlaybackContext *play_ctx, int sample_rate, int channels, const char *device_id);
int write_pcm_to_ring_buffer(AudioPlaybackContext *play_ctx, const float *pcm_data, int frame_count);
void stop_audio_playback(AudioPlaybackContext *play_ctx);

void set_audio_delay_offset(AudioPlaybackContext *play_ctx, int delay_ms);

int run_test_mux_and_play(DemuxDecContext *dec_ctx, AudioPlaybackContext *play_ctx, uintptr_t go_token);

// Queries miniaudio context and populates devices buffer up to max_devices.
// Returns total number of devices found, or a negative error code.
int get_miniaudio_devices(NativeAudioDevice *devices, int max_devices);

// Probes a file and fills out the CMediaProperties memory layout
int probe_media_properties(const char *file_path, CMediaProperties *props);

// Extract a compressed image frame into a dynamically allocated buffer
int extract_video_screenshot(const char *file_path, int64_t time_ms, uint8_t **out_buf, int *out_size);

int run_production_mux_and_play(DemuxDecContext *dec_ctx, AudioPlaybackContext *play_ctx, uintptr_t go_token);

// Control functions for runtime playback control
void pause_playback(AudioPlaybackContext *play_ctx);
void resume_playback(AudioPlaybackContext *play_ctx);
int seek_playback(DemuxDecContext *dec_ctx, int64_t seek_time_ms);

#endif // DECODER_H