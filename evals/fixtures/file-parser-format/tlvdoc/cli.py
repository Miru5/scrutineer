import sys
from pathlib import Path

from tlvdoc.reader import parse


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("usage: python -m tlvdoc.cli FILE", file=sys.stderr)
        return 2
    print(parse(Path(args[0]).read_bytes()))
    return 0


if __name__ == "__main__":
    sys.exit(main())
