import struct
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import tlvdoc.reader as tlvdoc  # noqa: E402

SAMPLE = Path(__file__).with_name("sample.tlv")


def element(kind, payload):
    return struct.pack(">BI", kind, len(payload)) + payload


def write_sample():
    items = [element(1, b"Quarterly report"), element(1, b"Total: 42")]
    SAMPLE.write_bytes(element(2, b"".join(items)))


def main():
    if not SAMPLE.exists():
        write_sample()
    title, *lines = tlvdoc.parse(SAMPLE.read_bytes())
    print(title)
    for line in lines:
        print("  " + line)


if __name__ == "__main__":
    main()
