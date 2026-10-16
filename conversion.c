#include "decoder.h"
#include "_cgo_export.h"
#include <errno.h>

int sidecar_go_conversion_progress(double progress, uintptr_t user_token)
{
  return goVideoConversionProgress(progress, user_token);
}

void request_video_conversion_cancel(volatile int *cancel_flag)
{
  if (cancel_flag)
    __atomic_store_n(cancel_flag, 1, __ATOMIC_RELEASE);
}

typedef struct
{
  volatile int *cancel_flag;
} ConversionInterrupt;

static int conversion_interrupted(void *opaque)
{
  ConversionInterrupt *control = (ConversionInterrupt *)opaque;
  return control && control->cancel_flag &&
         __atomic_load_n(control->cancel_flag, __ATOMIC_ACQUIRE) != 0;
}

static int conversion_write_packets(AVCodecContext *encoder,
                                    AVFormatContext *output,
                                    AVStream *stream,
                                    AVPacket *packet)
{
  int ret;
  while ((ret = avcodec_receive_packet(encoder, packet)) >= 0)
  {
    av_packet_rescale_ts(packet, encoder->time_base, stream->time_base);
    packet->stream_index = stream->index;
    ret = av_interleaved_write_frame(output, packet);
    av_packet_unref(packet);
    if (ret < 0)
      return ret;
  }
  return ret == AVERROR(EAGAIN) || ret == AVERROR_EOF ? 0 : ret;
}

static int conversion_encode_frame(AVCodecContext *encoder,
                                   AVFormatContext *output,
                                   AVStream *stream,
                                   AVFrame *frame,
                                   AVPacket *packet)
{
  int ret = avcodec_send_frame(encoder, frame);
  if (ret < 0 && ret != AVERROR(EAGAIN))
    return ret;
  if (ret == AVERROR(EAGAIN))
  {
    ret = conversion_write_packets(encoder, output, stream, packet);
    if (ret < 0)
      return ret;
    ret = avcodec_send_frame(encoder, frame);
    if (ret < 0)
      return ret;
  }
  return conversion_write_packets(encoder, output, stream, packet);
}

static int conversion_flush_encoder(AVCodecContext *encoder,
                                    AVFormatContext *output,
                                    AVStream *stream,
                                    AVPacket *packet)
{
  int ret = avcodec_send_frame(encoder, NULL);
  if (ret < 0 && ret != AVERROR_EOF)
    return ret;
  while ((ret = avcodec_receive_packet(encoder, packet)) >= 0)
  {
    av_packet_rescale_ts(packet, encoder->time_base, stream->time_base);
    packet->stream_index = stream->index;
    ret = av_interleaved_write_frame(output, packet);
    av_packet_unref(packet);
    if (ret < 0)
      return ret;
  }
  return ret == AVERROR_EOF || ret == AVERROR(EAGAIN) ? 0 : ret;
}

static int conversion_audio_emit(AVAudioFifo *fifo,
                                AVCodecContext *encoder,
                                AVFormatContext *output,
                                AVStream *stream,
                                AVFrame *frame,
                                AVPacket *packet,
                                int64_t *next_pts,
                                int flush_partial)
{
  int frame_size = encoder->frame_size > 0 ? encoder->frame_size : 1024;
  while (av_audio_fifo_size(fifo) >= frame_size ||
         (flush_partial && av_audio_fifo_size(fifo) > 0))
  {
    int available = av_audio_fifo_size(fifo);
    int read_count = available < frame_size ? available : frame_size;
    av_frame_unref(frame);
    frame->nb_samples = frame_size;
    frame->format = encoder->sample_fmt;
    frame->sample_rate = encoder->sample_rate;
    if (av_channel_layout_copy(&frame->ch_layout, &encoder->ch_layout) < 0)
      return AVERROR(ENOMEM);
    int ret = av_frame_get_buffer(frame, 0);
    if (ret < 0)
      return ret;
    ret = av_audio_fifo_read(fifo, (void **)frame->extended_data, read_count);
    if (ret < read_count)
      return ret < 0 ? ret : AVERROR(EIO);
    if (read_count < frame_size)
    {
      ret = av_samples_set_silence(frame->extended_data, read_count,
                                   frame_size - read_count,
                                   encoder->ch_layout.nb_channels,
                                   encoder->sample_fmt);
      if (ret < 0)
        return ret;
    }
    frame->pts = *next_pts;
    *next_pts += frame_size;
    ret = conversion_encode_frame(encoder, output, stream, frame, packet);
    if (ret < 0)
      return ret;
  }
  return 0;
}

static int conversion_decode_audio_frame(AVFrame *decoded,
                                         SwrContext *resampler,
                                         AVAudioFifo *fifo,
                                         AVCodecContext *encoder,
                                         AVFormatContext *output,
                                         AVStream *stream,
                                         AVFrame *encode_frame,
                                         AVPacket *encode_packet,
                                         int64_t *next_pts)
{
  int capacity = swr_get_out_samples(resampler, decoded->nb_samples);
  if (capacity <= 0)
    return capacity < 0 ? capacity : 0;
  uint8_t **converted = NULL;
  int line_size = 0;
  int ret = av_samples_alloc_array_and_samples(&converted, &line_size,
                                              encoder->ch_layout.nb_channels,
                                              capacity, encoder->sample_fmt, 0);
  if (ret < 0)
    return ret;
  int converted_samples = swr_convert(resampler, converted, capacity,
                                      (const uint8_t **)decoded->extended_data,
                                      decoded->nb_samples);
  if (converted_samples >= 0 && converted_samples > 0)
  {
    ret = av_audio_fifo_realloc(fifo, av_audio_fifo_size(fifo) + converted_samples);
    if (ret >= 0)
    {
      ret = av_audio_fifo_write(fifo, (void **)converted, converted_samples);
      if (ret >= 0 && ret != converted_samples)
        ret = AVERROR(EIO);
    }
  }
  else if (converted_samples < 0)
  {
    ret = converted_samples;
  }
  if (converted)
  {
    av_freep(&converted[0]);
    av_freep(&converted);
  }
  if (ret < 0)
    return ret;
  return conversion_audio_emit(fifo, encoder, output, stream, encode_frame,
                               encode_packet, next_pts, 0);
}

static int conversion_process_audio(AVCodecContext *decoder,
                                    SwrContext *resampler,
                                    AVAudioFifo *fifo,
                                    AVCodecContext *encoder,
                                    AVFormatContext *output,
                                    AVStream *stream,
                                    AVFrame *decoded,
                                    AVFrame *encode_frame,
                                    AVPacket *encode_packet,
                                    int64_t *next_pts)
{
  int ret;
  while ((ret = avcodec_receive_frame(decoder, decoded)) >= 0)
  {
    ret = conversion_decode_audio_frame(decoded, resampler, fifo, encoder,
                                        output, stream, encode_frame,
                                        encode_packet, next_pts);
    av_frame_unref(decoded);
    if (ret < 0)
      return ret;
  }
  return ret == AVERROR(EAGAIN) || ret == AVERROR_EOF ? 0 : ret;
}

static void conversion_set_error(char *buffer, size_t buffer_size,
                                 const char *operation, int error_code)
{
  if (!buffer || buffer_size == 0)
    return;
  char detail[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(error_code, detail, sizeof(detail));
  snprintf(buffer, buffer_size, "%s: %s (%d)", operation, detail, error_code);
}

static int conversion_scale_video_frame(AVFrame *decoded, AVFrame *scaled,
                                        AVCodecContext *decoder,
                                        AVCodecContext *encoder,
                                        AVStream *input_stream,
                                        SwsContext **scaler,
                                        AVFilterGraph **hdr_graph,
                                        AVFilterContext **hdr_source,
                                        AVFilterContext **hdr_sink,
                                        uintptr_t user_token,
                                        int *hdr_frame_reported,
                                        int preserve_hdr)
{
  int ret = av_frame_make_writable(scaled);
  if (ret < 0)
    return ret;
  if (!preserve_hdr &&
      (decoder->color_trc == AVCOL_TRC_SMPTE2084 ||
       decoder->color_trc == AVCOL_TRC_ARIB_STD_B67 ||
       decoder->color_primaries == AVCOL_PRI_BT2020))
  {
    if (decoded->color_primaries == AVCOL_PRI_UNSPECIFIED)
      decoded->color_primaries = decoder->color_primaries;
    if (decoded->color_trc == AVCOL_TRC_UNSPECIFIED)
      decoded->color_trc = decoder->color_trc;
    if (decoded->colorspace == AVCOL_SPC_UNSPECIFIED)
      decoded->colorspace = decoder->colorspace;
    if (decoded->color_range == AVCOL_RANGE_UNSPECIFIED)
      decoded->color_range = decoder->color_range;
    AVFrame *tone_mapped = av_frame_alloc();
    if (!tone_mapped)
      return AVERROR(ENOMEM);
    ret = tone_map_hdr_frame(hdr_graph, hdr_source, hdr_sink, decoded,
                             tone_mapped, input_stream->time_base,
                             encoder->width, encoder->height, AVCOL_RANGE_MPEG,
                             encoder->pix_fmt, 0, hdr_frame_reported);
    if (ret >= 0)
      av_frame_move_ref(scaled, tone_mapped);
    av_frame_free(&tone_mapped);
    if (ret >= 0)
      scaled->pict_type = AV_PICTURE_TYPE_NONE;
    return ret < 0 ? ret : 0;
  }

  *scaler = sws_getCachedContext(*scaler, decoded->width, decoded->height,
                                 decoded->format, encoder->width, encoder->height,
                                 encoder->pix_fmt, SWS_BICUBIC, NULL, NULL, NULL);
  if (!*scaler)
    return AVERROR(ENOMEM);
  int rows = sws_scale(*scaler, (const uint8_t *const *)decoded->data,
                       decoded->linesize, 0, decoded->height,
                       scaled->data, scaled->linesize);
  if (rows <= 0)
    return rows < 0 ? rows : AVERROR(EINVAL);
  scaled->color_primaries = encoder->color_primaries;
  scaled->color_trc = encoder->color_trc;
  scaled->colorspace = encoder->colorspace;
  scaled->color_range = encoder->color_range;
  scaled->pict_type = AV_PICTURE_TYPE_NONE;
  return 0;
}

int convert_video_file(const char *source_path, const char *output_path,
                       int video_bitrate_kbps, int audio_bitrate_kbps,
                       const char *encoder_preset, int preserve_hdr,
                       volatile int *cancel_flag,
                       VideoConversionProgressCallback progress_callback,
                       uintptr_t user_token, char *err_buf, size_t err_buf_size)
{
  int ret = 0;
  AVFormatContext *input = NULL, *output = NULL;
  AVCodecContext *video_decoder = NULL, *video_encoder = NULL;
  AVCodecContext *audio_decoder = NULL, *audio_encoder = NULL;
  AVStream *video_input_stream = NULL, *audio_input_stream = NULL;
  AVStream *video_output_stream = NULL, *audio_output_stream = NULL;
  AVPacket *packet = NULL, *encoded_packet = NULL;
  AVFrame *decoded_frame = NULL, *video_frame = NULL, *audio_frame = NULL;
  AVFrame *audio_encode_frame = NULL;
  SwsContext *scaler = NULL;
  SwrContext *resampler = NULL;
  AVAudioFifo *audio_fifo = NULL;
  AVFilterGraph *hdr_graph = NULL;
  AVFilterContext *hdr_source = NULL, *hdr_sink = NULL;
  int hdr_frame_reported = 0;
  int video_index = -1, audio_index = -1;
  int64_t first_video_pts = AV_NOPTS_VALUE, video_duration = 0;
  int64_t audio_next_pts = 0;
  double duration_seconds = 0.0, last_progress = -1.0;
  ConversionInterrupt interrupt = {cancel_flag};

  if (err_buf && err_buf_size)
    err_buf[0] = '\0';
  if (!source_path || !source_path[0] || !output_path || !output_path[0])
    return AVERROR(EINVAL);
  if (video_bitrate_kbps < 100 || video_bitrate_kbps > 100000 ||
      audio_bitrate_kbps < 32 || audio_bitrate_kbps > 2048)
    return AVERROR(EINVAL);

  input = avformat_alloc_context();
  if (!input)
    return AVERROR(ENOMEM);
  input->interrupt_callback.callback = conversion_interrupted;
  input->interrupt_callback.opaque = &interrupt;
  ret = avformat_open_input(&input, source_path, NULL, NULL);
  if (ret < 0)
    goto fail;
  ret = avformat_find_stream_info(input, NULL);
  if (ret < 0)
    goto fail;
  video_index = av_find_best_stream(input, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
  if (video_index < 0)
  {
    ret = video_index;
    goto fail;
  }
  audio_index = av_find_best_stream(input, AVMEDIA_TYPE_AUDIO, -1, video_index, NULL, 0);
  video_input_stream = input->streams[video_index];
  if (audio_index >= 0)
    audio_input_stream = input->streams[audio_index];
  if (input->duration != AV_NOPTS_VALUE && input->duration > 0)
    duration_seconds = (double)input->duration / AV_TIME_BASE;
  else if (video_input_stream->duration != AV_NOPTS_VALUE && video_input_stream->duration > 0)
    duration_seconds = video_input_stream->duration * av_q2d(video_input_stream->time_base);

  const AVCodec *decoder = avcodec_find_decoder(video_input_stream->codecpar->codec_id);
  if (!decoder)
  {
    ret = AVERROR_DECODER_NOT_FOUND;
    goto fail;
  }
  video_decoder = avcodec_alloc_context3(decoder);
  if (!video_decoder)
  {
    ret = AVERROR(ENOMEM);
    goto fail;
  }
  ret = avcodec_parameters_to_context(video_decoder, video_input_stream->codecpar);
  if (ret < 0 || (ret = avcodec_open2(video_decoder, decoder, NULL)) < 0)
    goto fail;

  const AVCodec *video_codec = avcodec_find_encoder_by_name("libx264");
  if (!video_codec)
    video_codec = avcodec_find_encoder(AV_CODEC_ID_H264);
  if (!video_codec)
  {
    ret = AVERROR_ENCODER_NOT_FOUND;
    goto fail;
  }
  video_encoder = avcodec_alloc_context3(video_codec);
  if (!video_encoder)
  {
    ret = AVERROR(ENOMEM);
    goto fail;
  }
  video_encoder->width = video_decoder->width;
  video_encoder->height = video_decoder->height;
  video_encoder->width &= ~1;
  video_encoder->height &= ~1;
  if (video_encoder->width < 2 || video_encoder->height < 2)
  {
    ret = AVERROR(EINVAL);
    goto fail;
  }
  video_encoder->pix_fmt = preserve_hdr ? AV_PIX_FMT_YUV420P10LE : AV_PIX_FMT_YUV420P;
  video_encoder->time_base = video_input_stream->avg_frame_rate.num > 0 && video_input_stream->avg_frame_rate.den > 0
                                 ? av_inv_q(video_input_stream->avg_frame_rate)
                                 : (AVRational){1, 25};
  video_encoder->framerate = av_inv_q(video_encoder->time_base);
  video_encoder->bit_rate = (int64_t)video_bitrate_kbps * 1000;
  video_encoder->rc_max_rate = video_encoder->bit_rate;
  video_encoder->rc_buffer_size = video_bitrate_kbps * 2000;
  video_encoder->gop_size = 2 * video_encoder->framerate.num / video_encoder->framerate.den;
  if (video_encoder->gop_size < 1)
    video_encoder->gop_size = 50;
  video_encoder->color_range = preserve_hdr ? video_decoder->color_range : AVCOL_RANGE_MPEG;
  video_encoder->color_primaries = preserve_hdr ? video_decoder->color_primaries : AVCOL_PRI_BT709;
  video_encoder->color_trc = preserve_hdr ? video_decoder->color_trc : AVCOL_TRC_BT709;
  video_encoder->colorspace = preserve_hdr ? video_decoder->colorspace : AVCOL_SPC_BT709;
  video_encoder->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  av_opt_set(video_encoder->priv_data, "preset",
             encoder_preset && encoder_preset[0] ? encoder_preset : "slow", 0);
  av_opt_set(video_encoder->priv_data, "profile", preserve_hdr ? "high10" : "high", 0);
  ret = avformat_alloc_output_context2(&output, NULL, "mp4", output_path);
  if (ret < 0 || !output)
  {
    if (ret >= 0)
      ret = AVERROR_MUXER_NOT_FOUND;
    goto fail;
  }
  ret = avcodec_open2(video_encoder, video_codec, NULL);
  if (ret < 0)
    goto fail;
  if (output->oformat->flags & AVFMT_GLOBALHEADER)
  {
    video_encoder->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  }
  video_output_stream = avformat_new_stream(output, NULL);
  if (!video_output_stream)
  {
    ret = AVERROR(ENOMEM);
    goto fail;
  }
  video_output_stream->time_base = video_encoder->time_base;
  ret = avcodec_parameters_from_context(video_output_stream->codecpar, video_encoder);
  if (ret < 0)
    goto fail;
  video_output_stream->codecpar->codec_tag = 0;

  if (audio_input_stream)
  {
    const AVCodec *audio_decoder_codec = avcodec_find_decoder(audio_input_stream->codecpar->codec_id);
    const AVCodec *audio_encoder_codec = avcodec_find_encoder(AV_CODEC_ID_AAC);
    if (audio_decoder_codec && audio_encoder_codec)
    {
      audio_decoder = avcodec_alloc_context3(audio_decoder_codec);
      audio_encoder = avcodec_alloc_context3(audio_encoder_codec);
      if (!audio_decoder || !audio_encoder)
      {
        ret = AVERROR(ENOMEM);
        goto fail;
      }
      ret = avcodec_parameters_to_context(audio_decoder, audio_input_stream->codecpar);
      if (ret < 0 || (ret = avcodec_open2(audio_decoder, audio_decoder_codec, NULL)) < 0)
        goto fail;
      if (audio_decoder->sample_rate <= 0)
        audio_decoder->sample_rate = 48000;
      if (audio_decoder->ch_layout.nb_channels <= 0)
        av_channel_layout_default(&audio_decoder->ch_layout,
                                  audio_input_stream->codecpar->ch_layout.nb_channels > 0
                                      ? audio_input_stream->codecpar->ch_layout.nb_channels
                                      : 2);
      audio_encoder->sample_rate = 48000;
      audio_encoder->sample_fmt = AV_SAMPLE_FMT_FLTP;
      audio_encoder->bit_rate = (int64_t)audio_bitrate_kbps * 1000;
      av_channel_layout_default(&audio_encoder->ch_layout, 2);
      audio_encoder->time_base = (AVRational){1, audio_encoder->sample_rate};
      if (output->oformat->flags & AVFMT_GLOBALHEADER)
        audio_encoder->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
      ret = avcodec_open2(audio_encoder, audio_encoder_codec, NULL);
      if (ret < 0)
        goto fail;
      audio_output_stream = avformat_new_stream(output, NULL);
      if (!audio_output_stream)
      {
        ret = AVERROR(ENOMEM);
        goto fail;
      }
      audio_output_stream->time_base = audio_encoder->time_base;
      ret = avcodec_parameters_from_context(audio_output_stream->codecpar, audio_encoder);
      if (ret < 0)
        goto fail;
      audio_output_stream->codecpar->codec_tag = 0;
      ret = swr_alloc_set_opts2(&resampler, &audio_encoder->ch_layout,
                                audio_encoder->sample_fmt, audio_encoder->sample_rate,
                                &audio_decoder->ch_layout, audio_decoder->sample_fmt,
                                audio_decoder->sample_rate, 0, NULL);
      if (ret < 0 || !resampler)
      {
        if (ret >= 0)
          ret = AVERROR(ENOMEM);
        goto fail;
      }
      ret = swr_init(resampler);
      if (ret < 0)
        goto fail;
      audio_fifo = av_audio_fifo_alloc(audio_encoder->sample_fmt,
                                       audio_encoder->ch_layout.nb_channels,
                                       audio_encoder->frame_size * 2);
      if (!audio_fifo)
      {
        ret = AVERROR(ENOMEM);
        goto fail;
      }
    }
  }
  ret = avio_open(&output->pb, output_path, AVIO_FLAG_WRITE);
  if (ret < 0)
    goto fail;
  AVDictionary *mux_options = NULL;
  av_dict_set(&mux_options, "movflags", "+faststart", 0);
  ret = avformat_write_header(output, &mux_options);
  av_dict_free(&mux_options);
  if (ret < 0)
    goto fail;

  packet = av_packet_alloc();
  encoded_packet = av_packet_alloc();
  decoded_frame = av_frame_alloc();
  video_frame = av_frame_alloc();
  if (!packet || !encoded_packet || !decoded_frame || !video_frame)
  {
    ret = AVERROR(ENOMEM);
    goto fail;
  }
  video_frame->format = video_encoder->pix_fmt;
  video_frame->width = video_encoder->width;
  video_frame->height = video_encoder->height;
  ret = av_frame_get_buffer(video_frame, 32);
  if (ret < 0)
    goto fail;
  if (audio_encoder)
  {
    audio_frame = av_frame_alloc();
    audio_encode_frame = av_frame_alloc();
    if (!audio_frame || !audio_encode_frame)
    {
      ret = AVERROR(ENOMEM);
      goto fail;
    }
  }
  if (progress_callback && progress_callback(0.0, user_token) < 0)
  {
    ret = AVERROR_EXIT;
    goto fail;
  }

  while ((ret = av_read_frame(input, packet)) >= 0)
  {
    if (conversion_interrupted(&interrupt))
    {
      ret = AVERROR_EXIT;
      av_packet_unref(packet);
      goto fail;
    }
    if (packet->stream_index == video_index)
    {
      ret = avcodec_send_packet(video_decoder, packet);
      av_packet_unref(packet);
      if (ret < 0 && ret != AVERROR(EAGAIN))
        goto fail;
      while ((ret = avcodec_receive_frame(video_decoder, decoded_frame)) >= 0)
      {
        if (first_video_pts == AV_NOPTS_VALUE)
          first_video_pts = decoded_frame->best_effort_timestamp != AV_NOPTS_VALUE
                                ? decoded_frame->best_effort_timestamp
                                : decoded_frame->pts;
        ret = conversion_scale_video_frame(decoded_frame, video_frame,
                                           video_decoder, video_encoder,
                                           video_input_stream, &scaler,
                                           &hdr_graph, &hdr_source, &hdr_sink,
                                           user_token, &hdr_frame_reported,
                                           preserve_hdr);
        if (ret < 0)
          goto fail;
        int64_t frame_pts = decoded_frame->best_effort_timestamp != AV_NOPTS_VALUE
                                ? decoded_frame->best_effort_timestamp
                                : decoded_frame->pts;
        if (frame_pts == AV_NOPTS_VALUE)
          frame_pts = video_duration;
        else if (first_video_pts != AV_NOPTS_VALUE)
          frame_pts -= first_video_pts;
        video_frame->pts = av_rescale_q(frame_pts, video_input_stream->time_base,
                                        video_encoder->time_base);
        video_duration = frame_pts + 1;
        ret = conversion_encode_frame(video_encoder, output, video_output_stream,
                                      video_frame, encoded_packet);
        av_frame_unref(decoded_frame);
        if (ret < 0)
          goto fail;
        double elapsed = frame_pts * av_q2d(video_input_stream->time_base);
        if (duration_seconds > 0.0 && elapsed >= last_progress + 0.25)
        {
          last_progress = elapsed;
          double percent = elapsed * 100.0 / duration_seconds;
          if (percent > 99.0)
            percent = 99.0;
          if (progress_callback && progress_callback(percent, user_token) < 0)
          {
            ret = AVERROR_EXIT;
            goto fail;
          }
        }
      }
      if (ret != AVERROR(EAGAIN) && ret != AVERROR_EOF)
        goto fail;
    }
    else if (audio_decoder && packet->stream_index == audio_index)
    {
      ret = avcodec_send_packet(audio_decoder, packet);
      av_packet_unref(packet);
      if (ret < 0 && ret != AVERROR(EAGAIN))
        goto fail;
      ret = conversion_process_audio(audio_decoder, resampler, audio_fifo,
                                     audio_encoder, output, audio_output_stream,
                                     audio_frame, audio_encode_frame, encoded_packet,
                                     &audio_next_pts);
      if (ret < 0)
        goto fail;
    }
    else
    {
      av_packet_unref(packet);
    }
  }
  if (ret != AVERROR_EOF)
    goto fail;

  ret = avcodec_send_packet(video_decoder, NULL);
  if (ret < 0 && ret != AVERROR_EOF)
    goto fail;
  while ((ret = avcodec_receive_frame(video_decoder, decoded_frame)) >= 0)
  {
    if (first_video_pts == AV_NOPTS_VALUE)
      first_video_pts = decoded_frame->best_effort_timestamp;
    ret = conversion_scale_video_frame(decoded_frame, video_frame,
                                       video_decoder, video_encoder,
                                       video_input_stream, &scaler,
                                       &hdr_graph, &hdr_source, &hdr_sink,
                                       user_token, &hdr_frame_reported,
                                       preserve_hdr);
    if (ret < 0)
      goto fail;
    int64_t frame_pts = decoded_frame->best_effort_timestamp != AV_NOPTS_VALUE
                            ? decoded_frame->best_effort_timestamp
                            : video_duration;
    if (first_video_pts != AV_NOPTS_VALUE)
      frame_pts -= first_video_pts;
    video_frame->pts = av_rescale_q(frame_pts, video_input_stream->time_base,
                                    video_encoder->time_base);
    ret = conversion_encode_frame(video_encoder, output, video_output_stream,
                                  video_frame, encoded_packet);
    av_frame_unref(decoded_frame);
    if (ret < 0)
      goto fail;
  }
  if (ret != AVERROR_EOF && ret != AVERROR(EAGAIN))
    goto fail;

  if (audio_decoder)
  {
    ret = avcodec_send_packet(audio_decoder, NULL);
    if (ret < 0 && ret != AVERROR_EOF)
      goto fail;
    ret = conversion_process_audio(audio_decoder, resampler, audio_fifo,
                                   audio_encoder, output, audio_output_stream,
                                   audio_frame, audio_encode_frame, encoded_packet,
                                   &audio_next_pts);
    if (ret < 0)
      goto fail;
    int capacity = swr_get_out_samples(resampler, 0);
    if (capacity > 0)
    {
      uint8_t **converted = NULL;
      int line_size = 0;
      ret = av_samples_alloc_array_and_samples(&converted, &line_size,
                                              audio_encoder->ch_layout.nb_channels,
                                              capacity, audio_encoder->sample_fmt, 0);
      if (ret < 0)
        goto fail;
      int count = swr_convert(resampler, converted, capacity, NULL, 0);
      if (count > 0)
      {
        ret = av_audio_fifo_realloc(audio_fifo, av_audio_fifo_size(audio_fifo) + count);
        if (ret >= 0)
          ret = av_audio_fifo_write(audio_fifo, (void **)converted, count);
      }
      av_freep(&converted[0]);
      av_freep(&converted);
      if (ret < 0)
        goto fail;
    }
    ret = conversion_audio_emit(audio_fifo, audio_encoder, output,
                                audio_output_stream, audio_encode_frame,
                                encoded_packet, &audio_next_pts, 1);
    if (ret < 0)
      goto fail;
  }
  ret = conversion_flush_encoder(video_encoder, output, video_output_stream,
                                 encoded_packet);
  if (ret < 0)
    goto fail;
  if (audio_encoder)
  {
    ret = conversion_flush_encoder(audio_encoder, output, audio_output_stream,
                                   encoded_packet);
    if (ret < 0)
      goto fail;
  }
  ret = av_write_trailer(output);
  if (ret < 0)
    goto fail;
  if (progress_callback && progress_callback(100.0, user_token) < 0)
  {
    ret = AVERROR_EXIT;
    goto fail;
  }
  ret = 0;
  goto cleanup;

fail:
  if (err_buf && err_buf_size)
    conversion_set_error(err_buf, err_buf_size,
                         ret == AVERROR_EXIT ? "conversion cancelled" : "video conversion failed",
                         ret);
cleanup:
  if (output && output->pb)
    avio_closep(&output->pb);
  avformat_free_context(output);
  avformat_close_input(&input);
  avcodec_free_context(&video_decoder);
  avcodec_free_context(&video_encoder);
  avcodec_free_context(&audio_decoder);
  avcodec_free_context(&audio_encoder);
  av_packet_free(&packet);
  av_packet_free(&encoded_packet);
  av_frame_free(&decoded_frame);
  av_frame_free(&video_frame);
  av_frame_free(&audio_frame);
  av_frame_free(&audio_encode_frame);
  sws_freeContext(scaler);
  swr_free(&resampler);
  av_audio_fifo_free(audio_fifo);
  avfilter_graph_free(&hdr_graph);
  return ret;
}
