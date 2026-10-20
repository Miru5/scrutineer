import struct
import zlib

STRING, CONTAINER, COMPRESSED = 1, 2, 3


def parse(data: bytes):
    """Parse a complete document and return its root value."""
    value, end = read_element(data, 0)
    if end != len(data):
        raise ValueError("trailing bytes after document")
    return value


def read_element(buf, pos):
    """Read one element at pos and return (value, next position)."""
    if pos + 5 > len(buf):
        raise ValueError("truncated header")
    kind, length = struct.unpack_from(">BI", buf, pos)
    start = pos + 5
    end = start + length
    if end > len(buf):
        raise ValueError("truncated payload")
    payload = buf[start:end]
    if kind == STRING:
        return payload.decode("utf-8"), end
    if kind == CONTAINER:
        items = []
        cursor = 0
        while cursor < len(payload):
            item, cursor = read_element(payload, cursor)
            items.append(item)
        return items, end
    if kind == COMPRESSED:
        return read_compressed(payload), end
    raise ValueError("unknown element type %d" % kind)


def read_compressed(payload):
    """Inflate a compressed element and parse the element inside it."""
    inner = zlib.decompress(payload)
    value, _ = read_element(inner, 0)
    return value
