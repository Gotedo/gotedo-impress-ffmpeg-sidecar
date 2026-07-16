#include "decoder.h"
#include <libavutil/opt.h>
#include <libavutil/channel_layout.h>

/**
 * Initializes the audio resampler context (SwrContext).
 *
 * Media files contain various sample rates (e.g., 44.1kHz, 48kHz), formats (e.g., s16, s32, flt),
 * and channel layouts (e.g., Mono, Stereo, 5.1). Miniaudio requires a consistent and predictable
 * hardware format to route audio cleanly.
 *
 * This function standardizes any incoming audio stream to:
 *   - Sample Rate: 48,000 Hz (48kHz Studio Quality)
 *   - Channels: 2 Channels (Stereo)
 *   - Format: 32-bit Floating Point (highly compatible with miniaudio & modern sound cards)
 *
 * @param ctx   Pointer to the active DemuxDecContext tracking the audio decoder.
 *
 * @return 0 on success, or a negative FFmpeg error code on failure.
 */
int init_audio_resampler(DemuxDecContext *ctx)
{
  int ret;

  // Set normalized audio properties
  ctx->target_sample_rate = 48000;
  av_channel_layout_default(&ctx->target_ch_layout, 2); // Force standard Stereo (L/R)
  ctx->target_sample_fmt = AV_SAMPLE_FMT_FLT;           // Force 32-bit float format

  // Allocate and configure the SwrContext using the modern channel layout API.
  // This calculates transition coefficients between the source and target formats.
  ret = swr_alloc_set_opts2(
      &ctx->swr_ctx,
      &ctx->target_ch_layout,          // Output channel layout
      ctx->target_sample_fmt,          // Output sample format (Float32)
      ctx->target_sample_rate,         // Output sample rate (48000 Hz)
      &ctx->audio_dec_ctx->ch_layout,  // Input channel layout (from source codec)
      ctx->audio_dec_ctx->sample_fmt,  // Input sample format (from source codec)
      ctx->audio_dec_ctx->sample_rate, // Input sample rate (from source codec)
      0, NULL);                        // No extra log offset parameters

  // Ensure allocation succeeded before proceeding
  if (ret < 0 || !ctx->swr_ctx)
  {
    return (ret < 0) ? ret : AVERROR(ENOMEM);
  }

  // Initialize the resampler context with the configured parameters
  ret = swr_init(ctx->swr_ctx);
  if (ret < 0)
  {
    swr_free(&ctx->swr_ctx);
    return ret;
  }

  return 0;
}

/**
 * Opens a media file, reads its metadata, and initializes the decoders
 * for both the primary video and audio streams.
 *
 * This function manages the safe, initial step-by-step pipeline discovery:
 *   1. Opens the media file container and parses headers.
 *   2. Extracts packet-level stream descriptors (resolution, codecs, sample rates).
 *   3. Locates the best video stream and opens its corresponding hardware-agnostic decoder.
 *   4. Locates the best audio stream, opens its decoder, and initializes the resampler.
 *
 * If any middle step fails, a unified "fail" label acts as an emergency sweep,
 * freeing partially allocated memory blocks to prevent structural leaks.
 *
 * @param ctx          Pointer to the uninitialized DemuxDecContext structure.
 * @param input_path   System file path to the source video (e.g., MKV, MP4).
 *
 * @return 0 on success, or a negative FFmpeg error code on failure.
 */
int open_input_and_decoders(DemuxDecContext *ctx, const char *input_path)
{
  int ret;
  ctx->fmt_ctx = NULL;
  ctx->video_stream_idx = -1;
  ctx->audio_stream_idx = -1;
  ctx->video_dec_ctx = NULL;
  ctx->audio_dec_ctx = NULL;
  ctx->swr_ctx = NULL;

  // 1. Open the media file container and extract initial stream parameters
  ret = avformat_open_input(&ctx->fmt_ctx, input_path, NULL, NULL);
  if (ret < 0)
    return ret;

  // 2. Query the stream contents of the container to discover streams
  ret = avformat_find_stream_info(ctx->fmt_ctx, NULL);
  if (ret < 0)
    goto fail;

  // 3. Find and configure the best Video Stream in the container
  const AVCodec *video_codec = NULL;
  ret = av_find_best_stream(ctx->fmt_ctx, AVMEDIA_TYPE_VIDEO, -1, -1, &video_codec, 0);
  if (ret >= 0)
  {
    ctx->video_stream_idx = ret;

    // Allocate raw codec context block
    ctx->video_dec_ctx = avcodec_alloc_context3(video_codec);
    if (!ctx->video_dec_ctx)
    {
      ret = AVERROR(ENOMEM);
      goto fail;
    }

    // Copy stream configurations (width, height, profiles) to the decoder context
    ret = avcodec_parameters_to_context(ctx->video_dec_ctx, ctx->fmt_ctx->streams[ctx->video_stream_idx]->codecpar);
    if (ret < 0)
      goto fail;

    // Initialize and open the decoder thread
    ret = avcodec_open2(ctx->video_dec_ctx, video_codec, NULL);
    if (ret < 0)
      goto fail;
  }

  // 4. Find and configure the best Audio Stream in the container
  const AVCodec *audio_codec = NULL;
  ret = av_find_best_stream(ctx->fmt_ctx, AVMEDIA_TYPE_AUDIO, -1, -1, &audio_codec, 0);
  if (ret >= 0)
  {
    ctx->audio_stream_idx = ret;

    // Allocate raw audio codec context block
    ctx->audio_dec_ctx = avcodec_alloc_context3(audio_codec);
    if (!ctx->audio_dec_ctx)
    {
      ret = AVERROR(ENOMEM);
      goto fail;
    }

    // Copy stream configurations (sample rate, bit-rate, channel layout) to the decoder context
    ret = avcodec_parameters_to_context(ctx->audio_dec_ctx, ctx->fmt_ctx->streams[ctx->audio_stream_idx]->codecpar);
    if (ret < 0)
      goto fail;

    // Initialize and open the audio decoder
    ret = avcodec_open2(ctx->audio_dec_ctx, audio_codec, NULL);
    if (ret < 0)
      goto fail;

    // Initialize swresample to normalize audio for miniaudio
    ret = init_audio_resampler(ctx);
    if (ret < 0)
      goto fail;
  }

  return 0;

fail:
  // Rollback phase: cleanly close allocations in reverse order of initialization
  if (ctx->video_dec_ctx)
    avcodec_free_context(&ctx->video_dec_ctx);
  if (ctx->audio_dec_ctx)
    avcodec_free_context(&ctx->audio_dec_ctx);
  if (ctx->fmt_ctx)
    avformat_close_input(&ctx->fmt_ctx);
  return ret;
}

/**
 * Safely releases, closes, and tears down all resources managed inside
 * the DemuxDecContext, preventing memory leaks between presentation runs.
 *
 * Deallocation operates in strict alignment with ownership rules:
 *   1. Destroys the software resampler context.
 *   2. Deallocates and closes the video and audio decoders.
 *   3. Closes the container format context and releases file descriptors.
 *   4. Frees dynamically allocated channel layout structures.
 *
 * @param ctx   Pointer to the active DemuxDecContext struct to clean up.
 */
void free_demux_dec_context(DemuxDecContext *ctx)
{
  if (!ctx)
    return;

  // Free the resampler context
  if (ctx->swr_ctx)
    swr_free(&ctx->swr_ctx);

  // Free the video decoder resources
  if (ctx->video_dec_ctx)
    avcodec_free_context(&ctx->video_dec_ctx);

  // Free the audio decoder resources
  if (ctx->audio_dec_ctx)
    avcodec_free_context(&ctx->audio_dec_ctx);

  // Close format input and set context back to NULL
  if (ctx->fmt_ctx)
    avformat_close_input(&ctx->fmt_ctx);

  // Uninitialize the custom target channel layout to prevent leaking layout maps
  av_channel_layout_uninit(&ctx->target_ch_layout);
}

/**
 * Custom I/O write callback for the AVIOContext.
 *
 * This function is triggered by the FFmpeg muxer whenever a chunk or fragment
 * of the fragmented MP4 (fMP4) stream is finalized in memory. It immediately
 * passes the pointer and size of the generated binary buffer back to the Go
 * runtime using the registered Go callback function and user context data pointer.
 *
 * @param opaque    A pointer to the user-defined TranscodeContext struct.
 * @param buf       The raw binary buffer containing fMP4 data.
 * @param buf_size  The size of the buffer in bytes.
 *
 * @return The number of written bytes, representing success or custom error.
 */
static int write_to_memory_cb(void *opaque, const uint8_t *buf, int buf_size)
{
  TranscodeContext *ctx = (TranscodeContext *)opaque;
  if (ctx && ctx->go_callback)
  {
    ctx->go_callback(buf, buf_size, ctx->go_user_data);
  }
  return buf_size;
}

/**
 * Allocates and initializes the output Format Context specifically for
 * fragmented MP4 (fMP4) streaming without using any local filesystem disk I/O.
 *
 * This function handles allocating a custom, dynamic AVIOContext containing
 * a 4KB write-buffer. All data generated by the multiplexing phase is redirected
 * through our 'write_to_memory_cb' directly to Go.
 *
 * @param out_fmt_ctx  Pointer to the uninitialized AVFormatContext pointer.
 * @param tx_ctx       Pointer to the Go callback routing context structure.
 *
 * @return 0 on success, or a negative FFmpeg error code on failure.
 */
int init_fmp4_muxer(AVFormatContext **out_fmt_ctx, TranscodeContext *tx_ctx)
{
  int ret;
  size_t avio_buf_size = 4096;
  uint8_t *avio_buf = NULL;
  AVIOContext *avio_ctx = NULL;

  ret = avformat_alloc_output_context2(out_fmt_ctx, NULL, "mp4", NULL);
  if (ret < 0)
  {
    return ret;
  }

  avio_buf = (uint8_t *)av_malloc(avio_buf_size);
  if (!avio_buf)
  {
    avformat_free_context(*out_fmt_ctx);
    *out_fmt_ctx = NULL;
    return AVERROR(ENOMEM);
  }

  avio_ctx = avio_alloc_context(
      avio_buf,
      avio_buf_size,
      1,                  // Flag 1 sets the IO context to Write-Only mode
      tx_ctx,             // Passed into callbacks as 'opaque' parameter
      NULL,               // Read callback is unneeded for our live stream output
      write_to_memory_cb, // Primary write interceptor callback
      NULL                // Seek callback is omitted since we can't seek live TCP streams
  );

  if (!avio_ctx)
  {
    av_free(avio_buf);
    avformat_free_context(*out_fmt_ctx);
    *out_fmt_ctx = NULL;
    return AVERROR(ENOMEM);
  }

  (*out_fmt_ctx)->pb = avio_ctx;
  return 0;
}

/**
 * Formats and writes the initial fMP4 container headers to the memory context.
 *
 * This function injects the specific metadata options (`movflags`) required by
 * browser Media Source Extensions (MSE) to begin playback instantly.
 *
 * Options utilized:
 * - 'empty_moov': Writes a clean, frame-index-free structural header immediately.
 * - 'default_base_moof': Resolves frame offsets relative to fragment start points.
 * - 'frag_keyframe': Flushes a play-ready segment on every video keyframe.
 * - 'omit_tfhd_offset': Streamlines file metadata sizing for browser parsing.
 *
 * @param out_fmt_ctx  The output context allocated via init_fmp4_muxer.
 *
 * @return 0 on success, or a negative FFmpeg error code on failure.
 */
int write_fmp4_header(AVFormatContext *out_fmt_ctx)
{
  int ret;
  AVDictionary *opts = NULL;

  ret = av_dict_set(&opts, "movflags", "empty_moov+default_base_moof+frag_keyframe+omit_tfhd_offset", 0);
  if (ret < 0)
  {
    return ret;
  }

  ret = avformat_write_header(out_fmt_ctx, &opts);
  av_dict_free(&opts);
  return ret;
}

/**
 * Safely releases and tears down all allocations associated with the memory-based
 * fMP4 multiplexing context, completely eliminating memory leaks.
 *
 * It extracts the dynamic buffer inside the AVIOContext and frees it in the correct
 * chronological order to prevent dangling pointer faults.
 *
 * @param out_fmt_ctx  The output context to be deallocated.
 */
void free_fmp4_muxer(AVFormatContext *out_fmt_ctx)
{
  if (!out_fmt_ctx)
  {
    return;
  }

  if (out_fmt_ctx->pb)
  {
    // Free the internal dynamic 4KB buffer inside the AVIO structure
    av_freep(&out_fmt_ctx->pb->buffer);
    // Free the custom IO context structure
    avio_context_free(&out_fmt_ctx->pb);
  }

  avformat_free_context(out_fmt_ctx);
}