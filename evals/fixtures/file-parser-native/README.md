# imgdec

A tiny decoder for the RAW1 image container. A file is a four byte magic, a
two byte width, a two byte height, a four byte payload length (all
little-endian) and then the pixel payload. `src/decode.c` implements the
decoder and `imgdec/__init__.py` wraps it with ctypes. Build it with
`cc -shared -fPIC -o imgdec/libdecode.so src/decode.c`.
