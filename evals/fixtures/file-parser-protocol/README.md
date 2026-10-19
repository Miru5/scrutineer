# dnswire

A small pure-Python library for reading DNS messages from the wire. `dnswire.message.parse_message(data)` takes the bytes of one DNS message and returns a `Message` with the header fields and the list of questions. Names are decoded including compression pointers.

The library is meant for tools that inspect DNS traffic, such as resolvers and packet analyzers. It has no dependencies beyond the standard library.

```python
from dnswire import parse_message

message = parse_message(packet)
print(message.questions[0].name)
```
