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
server <id>
```

`server` is the name the server gave itself. It is sent once, immediately after
a registration is accepted and before anything else — before the ref is
prepared, so a client refused because its ref could not be prepared still
learns which server refused it. Nothing follows it.

A registration that does not parse is refused before this, and gets an `error`
with no name in front of it: the name follows an accepted `register`, never a
rejected one.

It is sent **only by a server that has been given a name**. One that has not
sends no such line, and its half of the exchange is byte for byte what it
always was. Naming a server is therefore also choosing to require clients that
understand this line, since a client that does not know it refuses it as it
refuses any unknown message.

The client takes the name verbatim: it derives nothing from it, never routes on
it, and is served exactly the same whether or not there is one. Like a client's
own name it is one field of one line, so it may not contain spaces or control
characters, and it is at most 128 bytes. It is a label, not a credential — a
server can claim any name, exactly as a client can.

```
update <hash> <object-count> [<key>=<value> ...]
<pack data as pkt-lines>
0000
```

`update` announces the new state of the client's ref. `<object-count>` is how
many objects follow in decimal; it may be `0`, in which case the client is
already current and the flush-pkt follows immediately.

Whatever follows the count is a **trailer**: a named field, `<key>=<value>`.
One is defined, and it is optional.

| key | value |
|---|---|
| `seq` | the ordinal of the head this update carries, in decimal |

It may appear once. A line carrying an unknown key, a repeated key, a key with
no value or an empty one, or `seq=0`, is refused — a client that acted on half
a line it did not understand would be worse than one that turned it away. A
server with no trailer to send writes three fields and stops.

`seq` is how a client can tell which of two heads is the newer. The protocol
carries trees, not commits, so there is no parent pointer anywhere in the
object graph and nothing about two heads says which came first: content
addressing gives identity, not order. The ordinal supplies what the objects
cannot. It belongs to whatever owns the ref — a log index, a revision a state
machine keeps — and one property is required of it: **per ref, it must
increase whenever the head moves.** Nothing else about its value means
anything. Absent says this server has nothing to order against, and zero is
not another way of saying that.

Ordering it gives, agreement it does not, quite: equal ordinals mean equal
heads, while unequal ones need not mean unequal heads, since a server may be
some entries behind on a log whose entries did not touch this ref.

The trailer is not a credential, and a client is served exactly the same
whether or not a server sends it.

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

Had the server been named `node-3` and its refs ordered by a consensus layer,
the same exchange would read:

```
client → 0052register refs/heads/printer-7/config 0000000000000000000000000000000000000000
server → 0012server node-3
server → 003eupdate 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805 17 seq=98
server → 037a<886 bytes of pack>
server → 0000
client → 0031ack 35ae729ecbb6c621dc5bc6ac2d8efec6f83c2805

         … the server's data changes …

server → 003dupdate 30e5ce8082820717b3fb5fec3e962c1d62103e14 4 seq=99
server → 015f<347 bytes of pack>
server → 0000
client → 0031ack 30e5ce8082820717b3fb5fec3e962c1d62103e14
```

The server's name costs 18 bytes once, charged to the connection rather than to
any push. The sequence is charged per push and costs seven bytes at these
values, so the first update is 956 bytes here and the second 416.

Seventeen objects the first time and four the second, because the four are all
that moved: the second pack is 347 bytes against 886. Counting the lines in
full — the update message, the headers, the flush-pkt — an update with no
trailer is 949 bytes on the wire for the first push and 409 for the second,
which is what both ends report having exchanged.

## Connection lifetime

Neither side sets a deadline. The server pushes when it has something to say,
which may be hours after the last byte, and a deadline would tear down a
perfectly good connection in the meantime. Both sides enable TCP keepalive at 30
seconds so that NATs and middleboxes do not forget an idle connection; the
probes do not close a healthy one.

Either side may instead be configured to give up on a peer that has stopped
responding, by shortening that keepalive schedule and, where the platform has
it, setting `TCP_USER_TIMEOUT` to match. Nothing about that is visible on the wire —
these are socket options, not messages — so an implementation of this protocol
need not know they exist. The only effect one can observe is the one any
hang-up has: the connection ends.

One connection carries at most one `server` line, since a server's name does
not change while it is running. A `seq` belongs to the head rather than to the
connection, so one carries as many as the ref has states — but never a lower
one after a higher one, since a ref that is ordered at all moves only forward.

One connection carries one subscription, but a ref may have any number of
subscribers: a second connection naming a ref somebody else is already
following is served alongside it. Each is sent what it alone is missing,
worked out from the `<synced>` it registered with and the `ack`s it has sent
since, and all of them are pushed to when the ref moves.

Closing the connection is how a subscription ends. The server notices, drops the
subscriber and releases whatever it had prepared for that ref.
