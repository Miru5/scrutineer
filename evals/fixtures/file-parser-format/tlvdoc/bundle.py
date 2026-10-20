import os
from pathlib import Path


def extract_bundle(entries, dest):
    """Write named entries (name, data) under dest and return the paths written.

    Absolute names, parent directory segments and anything that resolves
    outside dest are rejected. Existing files are never overwritten.
    """
    root = Path(dest).resolve()
    written = []
    for name, data in entries:
        if os.path.isabs(name) or Path(name).is_absolute() or name.startswith(("/", "\\")):
            raise ValueError("absolute entry name: %r" % name)
        if ".." in Path(name.replace("\\", "/")).parts:
            raise ValueError("entry escapes destination: %r" % name)
        target = (root / name).resolve()
        if not target.is_relative_to(root):
            raise ValueError("entry escapes destination: %r" % name)
        if target.exists():
            raise FileExistsError(str(target))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        written.append(target)
    return written
