#include "decode.h"

#include <stdlib.h>
#include <string.h>

uint8_t *raw1_decode(const uint8_t *data, size_t size, uint16_t *width, uint16_t *height) {
    if (size < 12 || memcmp(data, "RAW1", 4) != 0) {
        return NULL;
    }
    uint16_t w = (uint16_t)(data[4] | (data[5] << 8));
    uint16_t h = (uint16_t)(data[6] | (data[7] << 8));
    uint32_t len = (uint32_t)data[8] | ((uint32_t)data[9] << 8) | ((uint32_t)data[10] << 16) | ((uint32_t)data[11] << 24);

    uint8_t *pixels = malloc((size_t)w * h);
    if (pixels == NULL) {
        return NULL;
    }
    memcpy(pixels, data + 12, len);
    *width = w;
    *height = h;
    return pixels;
}
