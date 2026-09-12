# The treevial protocol

A plain TCP connection carrying pkt-line framed messages. The client dials and
identifies itself; from then on the server decides when to send.

## Framing

Every message is one **pkt-line**, the framing git itself uses: four hexadecimal
digits giving the length of the whole line including those four digits, then the
payload.

```
0016register printer-7
^^^^ 0x16 = 22 bytes in total
```

A line of `0000` is a **flush-pkt** and carries no payload. It is used once, to
close a pack.

Payloads are at most 65516 bytes. Text lines end with `\n`, which is part of the
payload and not significant.

## Messages

Hashes travel as 40 lowercase hexadecimal digits. Forty zeros mean "no hash".

### Client to server

```
register <ref> <synced>
ack <hash>
```

`register` must be the first message; anything else is refused. `<ref>` is the
head the client wants, and **the server takes it verbatim** — it derives
nothing from it and attaches no meaning to its shape. What a ref stands for is
between the client and whatever serves it.

The server checks only that `<ref>` is a usable git ref name, since it will be
keyed on: under `refs/`, at most 512 bytes, no empty or dot-leading component,
no `..`, no `@{`, no control characters or any of ``space ~ ^ : ? * [ \``, and
not ending in `.lock`.

No part of treevial derives a ref from anything. The example client happens to
build one as `refs/heads/<id>/config` from an identifier it is given, but that
convention lives in that program alone; another client may ask for
`refs/devices/hall-a/row-3/seat-9` and be served just the same.

`<synced>` is the tree the client already holds in full, or forty zeros if it
holds nothing. Holding a tree means holding everything beneath it, so this one
hash is the whole of the client's state.

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
and closed by a flush-pkt. It is framed as it is encoded, so the receiver can
inflate objects while the rest is still arriving. It contains no deltas, so
every object stands alone.

The pack must be read to the flush-pkt before the next message can be read.

```
error <code> <message>
```

`error` reports a refusal, after which the server hangs up. `<code>` is one of
`invalid`, `exists` or `internal`; `<message>` is the rest of the line and is
for people, not for matching on.

## An exchange

```
client → 0040register refs/heads/printer-7/config 0000000000000000000000000000000000000000
server → 0039update df0e0e1cd7146ab580338deb67e66bd05d42c1e8 16
server → 8004<pack bytes>
server → 0000
client → 002fack df0e0e1cd7146ab580338deb67e66bd05d42c1e8

         … the server's data changes …

server → 0039update b501ed1768b41ecd5086ec43345aece2a0fb5d1c 4
server → 0231<pack bytes>
server → 0000
client → 002fack b501ed1768b41ecd5086ec43345aece2a0fb5d1c
```

## Connection lifetime

Neither side sets a deadline. The server pushes when it has something to say,
which may be hours after the last byte, and a deadline would tear down a
perfectly good connection in the meantime. Both sides enable TCP keepalive at 30
seconds so that NATs and middleboxes do not forget an idle connection; the
probes do not close a healthy one.

One connection carries one subscription. A second connection naming a ref that
already has a subscriber is refused with `exists`.

Closing the connection is how a subscription ends. The server notices, drops the
subscriber and releases whatever it had prepared for that ref.
