import ctypes
from pathlib import Path

_lib = ctypes.CDLL(str(Path(__file__).with_name("libdecode.so")))
_lib.raw1_decode.restype = ctypes.POINTER(ctypes.c_uint8)
_lib.raw1_decode.argtypes = [
    ctypes.c_char_p,
    ctypes.c_size_t,
    ctypes.POINTER(ctypes.c_uint16),
    ctypes.POINTER(ctypes.c_uint16),
]


def decode(data: bytes):
    """Return (width, height, pixels) for a RAW1 image."""
    width, height = ctypes.c_uint16(), ctypes.c_uint16()
    out = _lib.raw1_decode(data, len(data), ctypes.byref(width), ctypes.byref(height))
    if not out:
        raise ValueError("not a RAW1 image")
    return width.value, height.value, bytes(out[: width.value * height.value])
