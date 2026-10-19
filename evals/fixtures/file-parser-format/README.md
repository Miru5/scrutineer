# tlvdoc

A small library for reading type-length-value documents. Each
element is a one byte type, a four byte big-endian length and a payload.
Type 1 is a UTF-8 string, type 2 is a container of nested elements and type 3
is a zlib-compressed element holding one nested element.

`tlvdoc.reader.parse(data)` returns the element tree as nested Python values.
`tlvdoc.bundle.extract_bundle(entries, dest)` unpacks a bundle of named
entries into a directory. `python -m tlvdoc.cli FILE` prints a document.

`examples/` holds a small report app that uses the library on its own sample
document.
