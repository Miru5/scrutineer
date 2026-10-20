import struct
from dataclasses import dataclass, field

HEADER = struct.Struct(">HHHHHH")
MAX_JUMPS = 16


@dataclass
class Question:
    name: str
    qtype: int
    qclass: int


@dataclass
class Message:
    id: int
    flags: int
    questions: list = field(default_factory=list)


def read_name(data, offset):
    """Return the name starting at offset and the offset after its first encoding."""
    labels = []
    jumps = 0
    end = None
    while True:
        if offset >= len(data):
            raise ValueError("name runs past the end of the message")
        length = data[offset]
        if length & 0xC0 == 0xC0:
            if offset + 1 >= len(data):
                raise ValueError("truncated compression pointer")
            if end is None:
                end = offset + 2
            jumps += 1
            if jumps > MAX_JUMPS:
                raise ValueError("too many compression pointers")
            offset = ((length & 0x3F) << 8) | data[offset + 1]
        elif length & 0xC0:
            raise ValueError("unsupported label type")
        elif length == 0:
            return ".".join(labels), end if end is not None else offset + 1
        else:
            start = offset + 1
            if start + length > len(data):
                raise ValueError("label runs past the end of the message")
            labels.append(data[start:start + length].decode("ascii", "replace"))
            offset = start + length


def parse_message(data: bytes) -> Message:
    if len(data) < HEADER.size:
        raise ValueError("message shorter than the header")
    ident, flags, qdcount, _, _, _ = HEADER.unpack_from(data)
    message = Message(ident, flags)
    offset = HEADER.size
    for _ in range(qdcount):
        name, offset = read_name(data, offset)
        if offset + 4 > len(data):
            raise ValueError("truncated question")
        qtype, qclass = struct.unpack_from(">HH", data, offset)
        offset += 4
        message.questions.append(Question(name, qtype, qclass))
    return message
