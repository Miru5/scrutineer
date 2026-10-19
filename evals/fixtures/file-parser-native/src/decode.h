#ifndef IMGDEC_DECODE_H
#define IMGDEC_DECODE_H

#include <stddef.h>
#include <stdint.h>

/* Decode a RAW1 image. Returns a malloc'd width*height byte buffer or NULL. */
uint8_t *raw1_decode(const uint8_t *data, size_t size, uint16_t *width, uint16_t *height);

#endif
