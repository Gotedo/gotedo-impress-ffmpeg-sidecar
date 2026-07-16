#ifndef DECODER_H
#define DECODER_H

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libswresample/swresample.h>

typedef struct DemuxDecContext {
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

// Function prototypes
int init_audio_resampler(DemuxDecContext *ctx);
int open_input_and_decoders(DemuxDecContext *ctx, const char *input_path);
void free_demux_dec_context(DemuxDecContext *ctx);

#endif // DECODER_H