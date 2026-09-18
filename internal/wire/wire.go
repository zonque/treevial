// Package wire implements the treevial protocol: pkt-line framed messages over
// a plain connection. PROTOCOL.md in the repository root describes the format.
//
// Every message is one pkt-line — four hex digits giving the length of the
// whole line, then the payload — which is the framing git itself uses, and
// which go-git already provides. A pack is sent as a run of pkt-lines closed by
// a flush-pkt, so it streams out as it is encoded and can be interpreted as it
// arrives.
//
// A Conn may be read by one goroutine while another writes, which is what the
// server does: acknowledgements arrive while pushes go out. It does not support
// two writers or two readers.
package wire

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"

	"github.com/zonque/treevial"
)

// ChunkSize is how much pack data is buffered into one pkt-line. It is well
// under pktline.MaxPayloadSize, which a line may not exceed.
const ChunkSize = 32 * 1024

// ClientMessageType tells the two messages a client sends apart.
type ClientMessageType int

const (
	// Register opens a subscription.
	Register ClientMessageType = iota
	// Ack confirms an update was interpreted.
	Ack
)

// String implements fmt.Stringer.
func (k ClientMessageType) String() string {
	if k == Register {
		return "register"
	}

	return "ack"
}

// ClientMessage is a message from client to server.
type ClientMessage struct {
	Type ClientMessageType
	// Ref is the head the client asked for, set on a Register. It is
	// carried verbatim: what the server makes of it is its own business.
	Ref string
	// Hash is the state the client holds on a Register, or the state it has
	// reached on an Ack.
	Hash plumbing.Hash
}

// ServerMessage is a message from server to client: an update, announcing a
// new state and preceding the pack that carries it.
//
// There is no discriminator because there is nothing to discriminate. The only
// other thing a server sends is an error, and that comes back from
// [Conn.ReadServerMessage] as an error rather than as a message. Should a
// second kind of message ever arrive, this is where it would earn one.
type ServerMessage struct {
	Hash        plumbing.Hash
	ObjectCount int
}

// Conn is one end of a treevial connection.
type Conn struct {
	rw   io.ReadWriteCloser
	enc  *pktline.Encoder
	scan *pktline.Scanner

	// What has crossed this connection so far. Counted here because this
	// is the only place that sees the bytes themselves, and atomic because
	// one goroutine may be reading while another writes.
	read    atomic.Int64
	written atomic.Int64
}

// NewConn wraps a connection. It takes ownership: closing the Conn closes rw.
func NewConn(rw io.ReadWriteCloser) *Conn {
	c := &Conn{rw: rw}

	// The encoder and the scanner are given counting views of rw, so
	// everything they put on or take off the wire is accounted for —
	// pkt-line headers and flush-pkts included.
	c.enc = pktline.NewEncoder(&countingWriter{w: rw, n: &c.written})
	c.scan = pktline.NewScanner(&countingReader{r: rw, n: &c.read})

	return c
}

// BytesRead reports how many bytes have arrived on this connection since it
// was opened, framing included.
func (c *Conn) BytesRead() int64 {
	return c.read.Load()
}

// BytesWritten reports how many bytes have gone out on this connection since
// it was opened, framing included.
func (c *Conn) BytesWritten() int64 {
	return c.written.Load()
}

// countingReader and countingWriter tally what passes through them. They sit
// between the pkt-line codecs and the connection, which is the one point every
// byte of the protocol has to cross.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))

	return n, err
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

// Write implements io.Writer.
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))

	return n, err
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	return c.rw.Close()
}

// WriteLine writes one raw line. It exists so tests can put malformed input on
// the wire; the typed writers below are what everything else uses.
func (c *Conn) WriteLine(line string) error {
	return c.enc.Encodef("%s\n", line)
}

// WriteRegister opens a subscription to ref, stating which tree the client
// already holds. The zero hash means it holds nothing.
func (c *Conn) WriteRegister(ref string, synced plumbing.Hash) error {
	return c.WriteLine(fmt.Sprintf("register %s %s", ref, synced))
}

// WriteAck confirms that everything up to hash has been interpreted.
func (c *Conn) WriteAck(hash plumbing.Hash) error {
	return c.WriteLine(fmt.Sprintf("ack %s", hash))
}

// WriteUpdate announces a new state. The pack follows, written through
// PackWriter, whose Close ends the update.
func (c *Conn) WriteUpdate(hash plumbing.Hash, objects int) error {
	return c.WriteLine(fmt.Sprintf("update %s %d", hash, objects))
}

// WriteError reports a refusal. Nothing follows it: the sender hangs up.
func (c *Conn) WriteError(e *treevial.Error) error {
	return c.WriteLine(fmt.Sprintf("error %s %s", e.Code, e.Message))
}

// readLine reads the next pkt-line as text. A flush-pkt where a message was
// expected is a protocol error; a closed connection is io.EOF, which is how a
// subscription ends rather than something to report back.
func (c *Conn) readLine() (string, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return "", err
		}

		return "", io.EOF
	}

	line := strings.TrimSuffix(string(c.scan.Bytes()), "\n")
	if line == "" {
		return "", treevial.Errorf(treevial.CodeInvalid, "wire: empty line")
	}

	return line, nil
}

// ReadClientMessage reads the next message from a client.
func (c *Conn) ReadClientMessage() (ClientMessage, error) {
	line, err := c.readLine()
	if err != nil {
		return ClientMessage{}, err
	}

	fields := strings.Split(line, " ")

	switch fields[0] {
	case "register":
		if len(fields) != 3 {
			return ClientMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: malformed register line %q", line)
		}

		hash, err := parseHash(fields[2])
		if err != nil {
			return ClientMessage{}, err
		}

		return ClientMessage{Type: Register, Ref: fields[1], Hash: hash}, nil

	case "ack":
		if len(fields) != 2 {
			return ClientMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: malformed ack line %q", line)
		}

		hash, err := parseHash(fields[1])
		if err != nil {
			return ClientMessage{}, err
		}

		return ClientMessage{Type: Ack, Hash: hash}, nil

	default:
		return ClientMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: unknown client message %q", fields[0])
	}
}

// ReadServerMessage reads the next message from the server. An error line comes
// back as a *treevial.Error, so a caller can classify it with treevial.CodeOf.
//
// After an Update the pack must be drained through PackReader before the next
// message can be read.
func (c *Conn) ReadServerMessage() (ServerMessage, error) {
	line, err := c.readLine()
	if err != nil {
		return ServerMessage{}, err
	}

	fields := strings.Split(line, " ")

	switch fields[0] {
	case "update":
		if len(fields) != 3 {
			return ServerMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: malformed update line %q", line)
		}

		hash, err := parseHash(fields[1])
		if err != nil {
			return ServerMessage{}, err
		}

		objects, err := strconv.Atoi(fields[2])
		if err != nil {
			return ServerMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: malformed object count in %q", line)
		}

		return ServerMessage{Hash: hash, ObjectCount: objects}, nil

	case "error":
		if len(fields) < 2 {
			return ServerMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: malformed error line %q", line)
		}

		return ServerMessage{}, &treevial.Error{
			Code:    treevial.ErrorCode(fields[1]),
			Message: strings.Join(fields[2:], " "),
		}

	default:
		return ServerMessage{}, treevial.Errorf(treevial.CodeInvalid, "wire: unknown server message %q", fields[0])
	}
}

// parseHash accepts the 40 hex digits a hash travels as.
func parseHash(s string) (plumbing.Hash, error) {
	if len(s) != 40 {
		return plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid, "wire: %q is not a hash", s)
	}

	hash := plumbing.NewHash(s)
	if hash.IsZero() && s != plumbing.ZeroHash.String() {
		return plumbing.ZeroHash, treevial.Errorf(treevial.CodeInvalid, "wire: %q is not a hash", s)
	}

	return hash, nil
}

// PackWriter frames pack bytes into pkt-lines as they are produced, so the pack
// goes out while it is still being encoded. Close ends the update.
type PackWriter struct {
	conn *Conn
	buf  []byte
}

// PackWriter returns a writer for the pack belonging to the update just
// announced.
func (c *Conn) PackWriter() *PackWriter {
	return &PackWriter{conn: c}
}

// Write implements io.Writer.
func (w *PackWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	for len(w.buf) >= ChunkSize {
		if err := w.conn.enc.Encode(w.buf[:ChunkSize]); err != nil {
			return 0, err
		}
		w.buf = w.buf[ChunkSize:]
	}

	return len(p), nil
}

// Close sends whatever is buffered and then the flush-pkt that ends the update.
func (w *PackWriter) Close() error {
	if len(w.buf) > 0 {
		if err := w.conn.enc.Encode(w.buf); err != nil {
			return err
		}
		w.buf = nil
	}

	return w.conn.enc.Flush()
}

// PackReader returns a reader over the pack of the update just read. It ends at
// the flush-pkt, after which the connection is ready for the next message.
func (c *Conn) PackReader() io.Reader {
	return &packReader{conn: c}
}

type packReader struct {
	conn *Conn
	buf  []byte
	off  int
	done bool
}

// Read implements io.Reader.
func (r *packReader) Read(p []byte) (int, error) {
	for r.off >= len(r.buf) {
		if r.done {
			return 0, io.EOF
		}

		if !r.conn.scan.Scan() {
			if err := r.conn.scan.Err(); err != nil {
				return 0, err
			}

			return 0, io.ErrUnexpectedEOF
		}

		payload := r.conn.scan.Bytes()
		if len(payload) == 0 {
			// The flush-pkt that ends the pack.
			r.done = true

			return 0, io.EOF
		}

		// The scanner reuses its buffer, so keep a copy.
		r.buf = append(r.buf[:0], payload...)
		r.off = 0
	}

	n := copy(p, r.buf[r.off:])
	r.off += n

	return n, nil
}
