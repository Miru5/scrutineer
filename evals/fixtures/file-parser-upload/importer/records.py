import struct

MAX_RECORDS = 1000


def parse_records(data: bytes) -> list:
    records = []
    offset = 0
    while offset < len(data):
        if len(records) >= MAX_RECORDS:
            raise ValueError("too many records")
        if offset + 2 > len(data):
            raise ValueError("truncated record header")
        (length,) = struct.unpack_from(">H", data, offset)
        offset += 2
        if offset + length > len(data):
            raise ValueError("truncated record")
        records.append(data[offset:offset + length])
        offset += length
    return records
