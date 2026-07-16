#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>
#include <miniaudio.h>

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
  void *go_user_data;
  void (*go_callback)(const uint8_t *buf, int buf_size, void *user_data);
} TranscodeContext;

// Native Playback Context
typedef struct AudioPlaybackContext
{
  ma_pcm_rb ring_buffer;
  ma_device device;
  int sample_rate;
  int channels;
  volatile int is_active;
} AudioPlaybackContext;

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

#endif // DECODER_H