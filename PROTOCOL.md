# The treevial protocol

A plain TCP connection carrying pkt-line framed messages. The client dials and
names the head it wants; from then on the server decides when to send.

## Framing

Every message is one **pkt-line**, the framing git itself uses: four hexadecimal
digits giving the length of the whole line including those four digits, then the
payload.

```
0031ack 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805
^^^^ 0x31 = 49 bytes in all: these four, 44 of text, and a newline
```

A line of `0000` is a **flush-pkt** and carries no payload. It appears once per
update, closing the pack.

Payloads are at most 65516 bytes. Text lines end with `\n`, which is part of the
payload and carries no meaning.

## Messages

Hashes travel as 40 lowercase hexadecimal digits. Forty zeros mean "no hash".

### Client to server

```
register <ref> <synced> [<client-id>]
ack <hash>
```

`register` must be the first message; anything else is refused. `<ref>` is the
head the client wants, and **the server takes it verbatim** — it derives
nothing from it and attaches no meaning to its shape. What a ref stands for is
between the client and whatever serves it.

The server checks only that `<ref>` is a usable git ref name, since it will be
keyed on: under `refs/`, at most 512 bytes, no empty or dot-leading component,
no `..`, no `@{`, no control characters or any of ``space ~ ^ : ? * [ \``, and
no component ending in `.lock`.

No part of treevial derives a ref from anything. The example client happens to
build one as `refs/heads/<id>/config` from an identifier it is given, but that
convention lives in that program alone; another client may ask for
`refs/devices/hall-a/row-3/seat-9` and be served just the same.

`<synced>` is the tree the client already holds in full, or forty zeros if it
holds nothing. Holding a tree means holding everything beneath it, so this one
hash is the whole of the client's state.

`<client-id>` is optional and last: a name the client gives itself so that
whoever runs the server can tell its connections apart. The server takes it
verbatim, derives nothing from it, requires nothing of it and never routes on
it — two clients may send the same name, and a client that sends none is
served exactly the same. Because it is one field of one line it may not contain
spaces or control characters, and it is at most 128 bytes. A client that does
not name itself leaves the field off, so its registration is byte for byte the
one it always sent.

`ack` confirms that everything up to `<hash>` has been interpreted. Until it
arrives the server considers the client behind, and will not push again.

### Server to client

```
update <hash> <object-count>
<pack data as pkt-lines>
0000
```

`update` announces the new state of the client's ref. `<object-count>` is how
many objects follow in decimal; it may be `0`, in which case the client is
already current and the flush-pkt follows immediately.

The pack is a standard git packfile, split across as many pkt-lines as it takes
and closed by a flush-pkt. It may be split at any point, so a reader has to
treat the lines as a byte stream rather than expecting object boundaries to fall
on them. It is framed as it is encoded, so the receiver can
inflate objects while the rest is still arriving. It contains no deltas, so
every object stands alone.

The pack must be read to the flush-pkt before the next message can be read.

```
error <code> <message>
```

`error` reports a refusal, after which the server hangs up. `<code>` is one of
`invalid`, `exists` or `internal`; `<message>` is the rest of the line and is
for people, not for matching on. treevial's own server sends `invalid` and
`internal`; `exists` is part of the vocabulary for a server that turns a ref
away because something else already has it.

## An exchange

Taken off the wire, byte for byte:

```
client → 0052register refs/heads/printer-7/config 0000000000000000000000000000000000000000
server → 0037update 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805 17
server → 037a<886 bytes of pack, starting 50 41 43 4b — "PACK">
server → 0000
client → 0031ack 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805

         … the server's data changes …

server → 0036update 30e5ce8082820717b3fb5fec3e962c1d62103e14 4
server → 015f<347 bytes of pack>
server → 0000
client → 0031ack 30e5ce8082820717b3fb5fec3e962c1d62103e14
```

Had that client named itself `printer-7`, its first line would read
`005cregister refs/heads/printer-7/config 0000…0000 printer-7` — the same line
with one more field — and nothing else about the exchange would differ.

Seventeen objects the first time and four the second, because the four are all
that moved: the second pack is 347 bytes against 886. Counting the lines in
full — the update message, the headers, the flush-pkt — the first update is 949
bytes on the wire and the second 409, which is what both ends report having
exchanged.

## Connection lifetime

Neither side sets a deadline. The server pushes when it has something to say,
which may be hours after the last byte, and a deadline would tear down a
perfectly good connection in the meantime. Both sides enable TCP keepalive at 30
seconds so that NATs and middleboxes do not forget an idle connection; the
probes do not close a healthy one.

One connection carries one subscription, but a ref may have any number of
subscribers: a second connection naming a ref somebody else is already
following is served alongside it. Each is sent what it alone is missing,
worked out from the `<synced>` it registered with and the `ack`s it has sent
since, and all of them are pushed to when the ref moves.

Closing the connection is how a subscription ends. The server notices, drops the
subscriber and releases whatever it had prepared for that ref.
