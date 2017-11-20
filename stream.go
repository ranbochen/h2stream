package h2stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

// Stream is a bidriection HTTP/2 stream
type Stream interface {
	io.ReadWriteCloser

	ID() uint32
	Context() context.Context
	ClosedChan() <-chan struct{}
	Header() http.Header
	ReplyHeader() (http.Header, bool)
	Reply(header http.Header, endStream bool) error
	WaitForReply(ctx context.Context) error
	CloseWrite() error
	ResetWithError(errCode http2.ErrCode) error
}

type streamState int

const (
	stateIdle streamState = iota
	stateOpen
	stateHalfClosedLocal
	stateHalfClosedRemote
	stateClosed
)

var stateName = [...]string{
	stateIdle:             "Idle",
	stateOpen:             "Open",
	stateHalfClosedLocal:  "HalfClosedLocal",
	stateHalfClosedRemote: "HalfClosedRemote",
	stateClosed:           "Closed",
}

func (st streamState) String() string {
	return stateName[st]
}

// stream represents a bdirection stream.
type stream struct {
	id        uint32
	sc        *Session        // name compact with http2 code
	ctx       context.Context // the associated context of the stream
	cancelCtx context.CancelFunc
	cw        closeWaiter // closed wait stream transitions to closed state

	// reply
	replied     bool
	replyHeader http.Header   // header replied by peer
	readyCh     chan struct{} // closed when recv reply

	// owned by session's serve loop:
	state     streamState
	flow      flow  //outbound flow control
	inflow    flow  //inbound flow control
	bodyBytes int64 // body bytes seen so far

	header  http.Header
	trailer http.Header // accumulated trailers
	body    pipe        // buffered pipe

	resetQueued      bool        // RST_STREAM queued for write; set by conn.resetStream
	gotTrailerHeader bool        // HEADER frame for trailers was seen
	wroteHeaders     bool        // whether we wrote headers (not status 100)
	writeDeadline    *time.Timer // nil if unused
}

// ID returns the id of the stream.
func (st *stream) ID() uint32 {
	return st.id
}

// ClosedChan returns a chan to wait stream transitions to closed state.
func (st *stream) ClosedChan() <-chan struct{} {
	return st.cw
}

// Context returns the context of the stream.
func (st *stream) Context() context.Context {
	return st.ctx
}

// Headers returns the header used to open the stream
func (st *stream) Header() http.Header {
	return st.header
}

// ReplyHeader returns the header from peer reply.
func (st *stream) ReplyHeader() (http.Header, bool) {
	return st.replyHeader, st.replied
}

// io.ReadWriteCloser

// Read reads all p bytes from the wire for this stream.
func (st *stream) Read(p []byte) (n int, err error) {
	return st.body.Read(p)
}

// Write writes data to a stream.
func (st *stream) Write(data []byte) (n int, err error) {
	err = st.WriteData(data, false)
	if err == nil {
		n = len(data)
	}
	return
}

// WriteData writes data to a stream. it will block until write succ or fail.
func (st *stream) WriteData(data []byte, endStream bool) (err error) {
	err = st.sc.writeDataFromHandler(st, data, endStream)
	return
}

// CloseWrite writes a nil DATA frame with end flag to close local write of the stream.
func (st *stream) CloseWrite() error {
	return st.WriteData(nil, true)
}

// Close closes the stream on both sides.
func (st *stream) Close() error {
	return st.ResetWithError(http2.ErrCodeNo)
}

// Reply send a header frame to peer.
// if header is empty, a ":status 200" header frame will be send.
func (st *stream) Reply(header http.Header, endStream bool) error {
	if st.isPushed() == bool(st.sc.server) {
		panic("Reply a local created stream")
	}
	if st.replied {
		return errors.New("stream has already replied")
	}
	statusCode := 0
	if len(header) == 0 {
		statusCode = 200
		header = nil
	}
	st.replied = true
	close(st.readyCh)
	return st.sc.writeHeaders(st, &writeResHeaders{
		streamID:    st.id,
		httpResCode: statusCode,
		h:           header,
		endStream:   endStream,
	})
}

var errTimeout = errors.New("Timeout occurred")

// WaitForReply wait peer to reply this stream.
// the stream must be opened by local.
// use ctx to deal with timeout/cancel
func (st *stream) WaitForReply(ctx context.Context) error {
	var timeoutChan <-chan time.Time
	now := time.Now()
	if d, ok := ctx.Deadline(); ok {
		if !d.IsZero() && d.After(now) {
			timeoutChan = time.After(d.Sub(now))
		}
	}

	select {
	case <-st.cw:
		return errStreamClosed
	case <-st.ctx.Done():
		return errStreamClosed
	case <-timeoutChan:
		return errTimeout
	case <-st.readyCh:
		return nil
	}
}

func (st *stream) ResetWithError(errCode http2.ErrCode) error {
	if st.state == stateClosed || st.resetQueued {
		return errors.New("reset a dead stream")
	}
	st.replied = true
	st.sc.writeFrameFromHandler(FrameWriteRequest{
		write: writeStreamError{streamError(st.id, errCode)}})
	return nil
}

// isPushed reports whether the stream is server-initiated.
func (st *stream) isPushed() bool {
	return st.id%2 == 0
}

// endStream closes remote write (or after trailers).
func (st *stream) endStream() {
	sc := st.sc
	sc.serveG.check()
	st.state = stateHalfClosedRemote
	st.body.CloseWithError(io.EOF)
}

// // onWriteTimeout is run on its own goroutine (from time.AfterFunc)
// // when the stream'st WriteTimeout has fired.
// func (st *stream) onWriteTimeout() {
// 	st.sc.writeFrameFromHandler(
// 		FrameWriteRequest{write: streamError(st.id, http2.ErrCodeInternal)})
// }

func (st *stream) processTrailerHeaders(f *MetaHeadersFrame) error {
	sc := st.sc
	sc.serveG.check()
	if st.gotTrailerHeader {
		return ConnectionError(http2.ErrCodeProtocol)
	}
	st.gotTrailerHeader = true
	if !f.StreamEnded() {
		return streamError(st.id, http2.ErrCodeProtocol)
	}

	if len(f.PseudoFields()) > 0 {
		return streamError(st.id, http2.ErrCodeProtocol)
	}
	if st.trailer != nil {
		for _, hf := range f.RegularFields() {
			// dont need to virify trailer in h2stream
			key := hf.Name
			st.trailer[key] = append(st.trailer[key], hf.Value)
		}
	}
	st.endStream()
	return nil
}
