# Record import service

A small service that loads batches of records. `POST /import` takes a binary record file as the request body and returns a JSON summary with the number of records and their total size. `GET /health` reports that the service is running.

The record format is a sequence of records, each a 2-byte big-endian length followed by that many bytes of payload. `importer/records.py` reads it. Start the service with `python server.py` and send a file with `curl --data-binary @batch.rec http://localhost:8080/import`.
