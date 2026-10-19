# File parser threat model

## Reference and limits

This is original, source-review guidance informed by the MITRE Common Weakness Enumeration (CWE), not a reproduction of the catalog or a certification of any kind. Use the linked CWE definitions and identifiers as references. Applicability depends on what the project actually implements. Source review cannot establish the memory limits, timeouts, request size caps or sandboxing a deploying application applies around the parser without additional evidence. Protocol message parsers that decode received bytes are in scope for the sections below, but checking their conformance to the protocol specification is not.

## Actors and assets

Distinguish whoever supplies the file: the uploader of a document, the sender of a mailed or downloaded file, the publisher of an archive on a registry and the author of a file a user chooses to open. Do not grant the attacker control of the parser's configuration or of limits the caller sets. Protect process availability (CPU, memory and disk), the filesystem outside the destination, other tenants or callers sharing the process, secrets reachable through external references and the integrity of what the parser hands to its caller.

Inventory each relevant boundary as an entry point, attacker-controlled bytes, effective guard chain and effect. Include nested parsers invoked on embedded content, defaults pinned by the library and behavior started implicitly by opening, listing or previewing a file. Record a limit that a caller is expected to set, or a component outside the scoped checkout such as a reverse proxy size cap, as unavailable evidence rather than treating it as unguarded.

## Resource exhaustion and amplification

References: [CWE-400](https://cwe.mitre.org/data/definitions/400.html), [CWE-409](https://cwe.mitre.org/data/definitions/409.html), [CWE-674](https://cwe.mitre.org/data/definitions/674.html), [CWE-776](https://cwe.mitre.org/data/definitions/776.html), [CWE-770](https://cwe.mitre.org/data/definitions/770.html), [CWE-1333](https://cwe.mitre.org/data/definitions/1333.html).

Find work whose cost is set by the input rather than by its size: decompression without an output bound, containers that recurse without a depth limit, entity or reference expansion, repeated structures that multiply and sizes read from a header that drive an allocation before the data is present. Compare the output limit with the input size, since a small input that expands by orders of magnitude defeats an upload size cap. Look for quadratic behavior in lookups over attacker-controlled collections and for regular expressions with nested quantifiers (CWE-1333) applied to input. A depth, size or time limit is an effective guard only when it is enforced before the work happens and covers every path that reaches the same code. A documented limit that the caller is expected to set is a design property unless the project advertises safety for untrusted input.

## Length, offset and integer handling

References: [CWE-130](https://cwe.mitre.org/data/definitions/130.html), [CWE-190](https://cwe.mitre.org/data/definitions/190.html), [CWE-1284](https://cwe.mitre.org/data/definitions/1284.html).

Follow every length, count, offset and index that is read from the input to the allocation, loop bound, slice or seek it controls. Check that a declared length is compared with the bytes actually available, that arithmetic combining width, height, count and element size cannot wrap or overflow before the check, that signed values cannot go negative and that an offset cannot point backwards into already parsed data to create a loop. In memory-safe languages the consequence is usually an oversized allocation, an exception or a wrong result; in native code it can become memory corruption, which `audit-memory` examines. Record the unchecked arithmetic here when it also causes a non-memory effect such as exhaustion or misparsing.

## Archive and container extraction

References: [CWE-22](https://cwe.mitre.org/data/definitions/22.html), [CWE-59](https://cwe.mitre.org/data/definitions/59.html).

Trace each entry name from the archive to the path the extractor writes. Look for names joined to the destination without normalization, absolute names, parent directory segments, drive prefixes and mixed separators. Check links: an entry that creates a symlink or hard link followed by an entry written through it redirects a later write. A check performed on the name string does not cover a resolved path. Look for overwrites of existing files, duplicate entries and permission or ownership bits restored from the archive. A guard that resolves the final path and verifies it stays inside the destination before writing, plus refusal to overwrite, is an effective guard and a negative result. Extraction code that never writes to disk is outside this section.

## Format confusion and differential parsing

References: [CWE-436](https://cwe.mitre.org/data/definitions/436.html), [CWE-20](https://cwe.mitre.org/data/definitions/20.html).

Look for places where two components can read the same bytes differently and the caller relies on one reading: a validator and a consumer that use different parsers, duplicate keys or fields where one side takes the first and the other the last, content type decided by an extension in one place and by magic bytes in another, polyglot files valid in two formats and lenient decoding that accepts input a strict check rejected. The finding needs a security decision based on one interpretation and an action based on the other. A parser that is lenient with no second interpreter and no security decision attached is not a finding.

## External references and embedded content

References: [CWE-611](https://cwe.mitre.org/data/definitions/611.html), [CWE-918](https://cwe.mitre.org/data/definitions/918.html).

Identify where a file can name something else to load: external entities, DTDs, includes, imports, stylesheets, linked images and remote resources. This audit records the resolution point, meaning the call that follows the reference, plus whether resolution is on by default. `audit-exfil` owns the analysis of what is disclosed or reached from it, so do not duplicate that trace. A parser configured to forbid external resolution is a negative result; one that resolves only references to the same archive or a fixed allowlist is a design property.

## Malformed input and failure handling

References: [CWE-248](https://cwe.mitre.org/data/definitions/248.html), [CWE-754](https://cwe.mitre.org/data/definitions/754.html).

Determine what happens when input is truncated, inconsistent or violates the format. In a long-running service or a library used by one, an uncaught exception, abort, panic or unchecked error return that a single file can trigger takes down the process or leaves shared state half updated. Check that error paths release resources, do not return partially parsed data as if it were complete and do not treat a failed validation as success. A crash in a one-shot command line tool on a file the user chose is intended behavior unless the project claims safety for untrusted input. Whether a supervisor restarts the process is deployment evidence.

## Evidence discipline

Every vulnerability needs a reachable first-party source-to-effect path from attacker-supplied input and a concrete security consequence under stated attacker prerequisites. Record checked enforcement points as negative results, intentional semantics as design properties and anything that needs deployment limits, memory caps or sandboxing evidence outside the checkout as an unverified assumption. Static review establishes a source argument, not an executed reproduction.
