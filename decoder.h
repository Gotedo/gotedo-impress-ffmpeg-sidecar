#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>

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

// Function prototypes
int init_audio_resampler(DemuxDecContext *ctx);
int open_input_and_decoders(DemuxDecContext *ctx, const char *input_path);
void free_demux_dec_context(DemuxDecContext *ctx);

int init_fmp4_muxer(AVFormatContext **out_fmt_ctx, TranscodeContext *tx_ctx);
int write_fmp4_header(AVFormatContext *out_fmt_ctx);
void free_fmp4_muxer(AVFormatContext *out_fmt_ctx);

#endif // DECODER_H