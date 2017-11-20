package h2stream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// StreamHandler handle new stream in a new goroutine
type StreamHandler func(st Stream)

type sessionType bool

func (st sessionType) String() string {
	if st {
		return "server"
	}
	return "client"
}

// Session represent an communications session between an HTTP/2 client and server.
type Session struct {
	ctx        context.Context
	cancelCtx  context.CancelFunc
	conn       net.Conn
	localAddr  net.Addr
	remoteAddr net.Addr
	server     sessionType // http/2 server session or client session

	bw               *bufio.Writer
	framer           *http2.Framer
	doneServing      chan struct{}          // closed when serve ends
	readFrameCh      chan readFrameResult   // written by readFrames
	wantWriteFrameCh chan FrameWriteRequest // from stream handlers -> serve
	wroteFrameCh     chan frameWriteResult  // from writeFrameAsync -> serve, tickles more frame writes
	bodyReadCh       chan bodyReadMsg       // from handlers -> serve. use to notify n bytes of DATA readed from stream.
	serveMsgCh       chan interface{}       // misc messages & code to send to / run on the serve loop
	flow             flow                   // conn-wide (not stream-specific) outbound flow control
	inflow           flow                   // conn-wide inbound flow control

	writeSched   WriteScheduler
	handler      StreamHandler // stream handler
	sessionReady chan struct{} // closed when handshake OK

	// Everything following is owned by the serve loop; use serveG.check():
	serveG                      goroutineLock // used to verify funcs are on serve()
	streams                     map[uint32]*stream
	curClientStreams            uint32 // number of open streams initiated by client
	curPushedStreams            uint32 // number of open streams initiated by server
	maxClientStreamID           uint32 // max ever seen from client (odd), or 0 if there have been no client requests
	maxPushPromiseID            uint32 // ID of the last push promise (even), or 0 if there have been no pushes
	sawFirstSettings            bool   // got the initial SETTINGS frame after the preface
	needToSendSettingsAck       bool
	unackedSettings             int // how many SETTINGS have we sent without ACKs?
	initialStreamSendWindowSize int32
	initialStreamRecvWindowSize int32
	headerTableSize             uint32 // Settings: SETTINGS_HEADER_TABLE_SIZE.
	peerMaxHeaderListSize       uint32 // Settings: SETTINGS_HEADER_TABLE_SIZE. zero means unknown (default)
	pushEnabled                 bool   // Settings: SETTINGS_ENABLE_PUSH
	advMaxStreams               uint32 // Settings: our SETTINGS_MAX_CONCURRENT_STREAMS
	peerMaxStreams              uint32 // Settings: SETTINGS_MAX_CONCURRENT_STREAMS from remote
	maxFrameSize                int32  // Settings: SETTINGS_MAX_FRAME_SIZE
	writingFrame                bool   // started writing a frame (on serve goroutine or separate)
	writingFrameAsync           bool   // started a frame on its own goroutine but haven't heard back on wroteFrameCh
	needsFrameFlush             bool   // last frame write wasn't a flush
	inGoAway                    bool   // we've started to or sent GOAWAY
	inFrameScheduleLoop         bool   // whether we're in the scheduleFrameWrite loop
	needToSendGoAway            bool   // we need to schedule a GOAWAY frame write
	goAwayCode                  ErrCode
	shutdownTimer               *time.Timer // nil until used

	// timeout
	connectionTimeout time.Duration // timeout for connection establishment(http/2 handshaking)
	idleTimeout       time.Duration
	idleTimer         *time.Timer // nil if unused

	// Owned by the writeFrameAsync goroutine:
	headerWriteBuf bytes.Buffer
	hpackEncoder   *hpack.Encoder

	// Used by StartGracefulShutdown.
	shutdownOnce sync.Once
}

// newSession make new connection from net.Conn
func newSession(nc net.Conn, server bool, config *SessionConfig) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	// TODO: config verification

	s := &Session{
		ctx:        ctx,
		cancelCtx:  cancel,
		conn:       nc,
		localAddr:  nc.LocalAddr(),
		remoteAddr: nc.RemoteAddr(),
		server:     sessionType(server),

		bw:                bufio.NewWriterSize(nc, config.WriteBufferSize),
		streams:           make(map[uint32]*stream),
		readFrameCh:       make(chan readFrameResult),
		wantWriteFrameCh:  make(chan FrameWriteRequest, 8),
		serveMsgCh:        make(chan interface{}, 8),
		wroteFrameCh:      make(chan frameWriteResult, 1), // buffered; one send in writeFrameAsync
		bodyReadCh:        make(chan bodyReadMsg),         // buffering doesn't matter either way
		doneServing:       make(chan struct{}),
		sessionReady:      make(chan struct{}),
		writeSched:        NewPriorityWriteScheduler(nil),
		connectionTimeout: config.ConnectionTimeout,

		peerMaxStreams:              math.MaxUint32, // Section 6.5.2: "Initially, there is no limit to this value"
		advMaxStreams:               math.MaxUint32,
		initialStreamSendWindowSize: int32(config.InitialStreamWindowSize),
		initialStreamRecvWindowSize: int32(config.InitialStreamWindowSize),
		maxFrameSize:                int32(config.MaxFrameSize),
		headerTableSize:             config.HeaderTableSize,

		idleTimeout: config.IdleTimeout,
	}

	// These start at the RFC-specified defaults. If there is a higher
	// configured value for inflow, that will be updated when we send a
	// WINDOW_UPDATE shortly after sending SETTINGS.
	s.flow.add(int32(config.InitialSessionWindowSize))
	s.inflow.add(int32(config.InitialSessionWindowSize))
	s.hpackEncoder = hpack.NewEncoder(&s.headerWriteBuf)

	fr := http2.NewFramer(s.bw, nc)
	fr.ReadMetaHeaders = hpack.NewDecoder(http2InitHeaderTableSize, nil)
	fr.MaxHeaderListSize = config.MaxHeaderBytes
	fr.SetMaxReadFrameSize(defaultMaxReadFrameSize)
	s.framer = fr

	return s
}

// NewClientSession returns a h2stream client session.
func NewClientSession(conn net.Conn, config *SessionConfig) (*Session, error) {
	if config == nil {
		config = &DefaultSessionConfig
	}

	return newSession(conn, false, config), nil
}

// NewServerSession returns a h2stream server session.
func NewServerSession(conn net.Conn, config *SessionConfig) (*Session, error) {
	if config == nil {
		config = &DefaultSessionConfig
	}

	return newSession(conn, true, config), nil
}

// WaitForReady wait untile handshake done.
// handshake timeout seted by config
func (sc *Session) WaitForReady() error {
	select {
	case <-sc.sessionReady:
		return nil
	case <-sc.doneServing:
		return errDisconnected
	}
}

func (sc *Session) Context() context.Context { return sc.ctx }

// interface writeContext
func (sc *Session) Framer() *Framer  { return sc.framer }
func (sc *Session) CloseConn() error { return sc.conn.Close() }
func (sc *Session) Flush() error     { return sc.bw.Flush() }
func (sc *Session) HeaderEncoder() (*hpack.Encoder, *bytes.Buffer) {
	return sc.hpackEncoder, &sc.headerWriteBuf
}

var (
	errDisconnected       = errors.New("http2: disconnected")
	errStreamClosed       = errors.New("http2: stream closed")
	errHandlerComplete    = errors.New("http2: request body closed due to handler exiting")
	ErrStreamLimitReached = errors.New("http2: would exceed peer's SETTINGS_MAX_CONCURRENT_STREAMS")
)

type serverMessage int

// Message values sent to serveMsgCh.
var (
	idleTimerMsg        = new(serverMessage)
	shutdownTimerMsg    = new(serverMessage)
	gracefulShutdownMsg = new(serverMessage)
)

func (sc *Session) onIdleTimer()     { sc.sendServeMsg(idleTimerMsg) }
func (sc *Session) onShutdownTimer() { sc.sendServeMsg(shutdownTimerMsg) }

func (sc *Session) sendServeMsg(msg interface{}) {
	sc.serveG.checkNotOn() // NOT
	select {
	case sc.serveMsgCh <- msg:
	case <-sc.doneServing:
	}
}

type createStreamRequest struct {
	parentID  uint32
	header    http.Header
	endStream bool
	streamCh  chan Stream
	errCh     chan error
}

// OpenStream create a new stream on the session.
// header can not be empty.
func (sc *Session) OpenStream(header http.Header, parent Stream, endStream bool) (Stream, error) {
	if VerboseLogs {
		sc.logf("http2: %s OpenStream %v", sc.server.String(), header)
	}
	if len(header) == 0 {
		return nil, errors.New("OpenStream with empty header")
	}

	// for k := range header {
	// 	if !validWireHeaderFieldName(lowerHeader(k)) {
	// 		return nil, errors.New("invalid header field name: " + k)
	// 	}
	// }

	parentID := uint32(0)
	if parent != nil {
		parentID = parent.ID()
	}
	msg := &createStreamRequest{
		parentID:  parentID,
		header:    header,
		endStream: endStream,
		streamCh:  make(chan Stream),
		errCh:     errChanPool.Get().(chan error),
	}
	defer close(msg.streamCh)

	select {
	case <-sc.doneServing:
		return nil, errDisconnected
	case sc.serveMsgCh <- msg:
	}

	select {
	case <-sc.doneServing:
		return nil, errDisconnected
	case err := <-msg.errCh:
		errChanPool.Put(msg.errCh)
		return nil, err
	case st := <-msg.streamCh:
		errChanPool.Put(msg.errCh)
		return st, nil
	}
}

// Serve run the session.
// If handler is nil, all income stream will be refused.
func (sc *Session) Serve(handler StreamHandler) {
	sc.handler = handler
	sc.serve()
	sc.cancelCtx()
}

// create stream
func (sc *Session) createStream(msg *createStreamRequest) {
	sc.serveG.check()

	maxStreamID := &sc.maxClientStreamID
	curStreams := sc.curClientStreams
	if sc.server {
		maxStreamID = &sc.maxPushPromiseID
		curStreams = sc.curPushedStreams
	}

	// http://tools.ietf.org/html/rfc7540#section-6.5.2.
	if curStreams+1 > sc.peerMaxStreams {
		msg.errCh <- ErrStreamLimitReached
		return
	}

	// http://tools.ietf.org/html/rfc7540#section-5.1.1.
	// Streams initiated by the server MUST use even-numbered identifiers.
	// A server that is unable to establish a new stream identifier can send a GOAWAY
	// frame so that the client is forced to open a new connection for new streams.
	if *maxStreamID+2 >= 1<<31 {
		sc.startGracefulShutdownInternal()
		msg.errCh <- ErrStreamLimitReached
		return
	}
	if !sc.server && sc.maxClientStreamID == 0 {
		// client begin with 1(odd)
		*maxStreamID = 1
	} else {
		*maxStreamID += 2
	}
	id := *maxStreamID
	initialState := stateOpen
	if msg.endStream {
		initialState = stateHalfClosedRemote
	}
	st := sc.newStream(id, msg.parentID, initialState)
	sc.writeFrame(FrameWriteRequest{
		write: &writeResHeaders{
			streamID:  st.id,
			h:         msg.header,
			endStream: msg.endStream,
		},
		stream: st,
	})
	msg.streamCh <- st
}

func (sc *Session) serve() {
	sc.serveG = newGoroutineLock()
	defer sc.conn.Close()
	defer sc.closeAllStreamsOnConnClose()
	defer sc.stopShutdownTimer()
	defer close(sc.doneServing) // unblocks handlers trying to send

	if VerboseLogs {
		sc.vlogf("h2stream: %s serve connection %v <--> %v",
			sc.server.String(), sc.remoteAddr, sc.localAddr)
	}

	sc.conn.SetDeadline(time.Now().Add(sc.connectionTimeout))
	// handshake: preface, settings, optional windowupdate
	if sc.server {
		if err := sc.readPreface(); err != nil {
			sc.condlogf(err, "http2: server: error reading preface from client %v: %v", sc.remoteAddr, err)
			return
		}
	} else if _, err := sc.bw.Write(clientPreface); err != nil {
		return
	}
	if err := sc.bw.Flush(); err != nil {
		return
	}

	// queue settings frame
	sc.writeFrame(FrameWriteRequest{
		write: writeSettings{
			{ID: http2.SettingEnablePush, Val: 0}, // do not use push
			{ID: http2.SettingMaxConcurrentStreams, Val: sc.advMaxStreams},
			{ID: http2.SettingInitialWindowSize, Val: uint32(sc.initialStreamRecvWindowSize)},
			{ID: http2.SettingMaxFrameSize, Val: uint32(sc.maxFrameSize)},
		},
	})
	sc.unackedSettings++

	// Queue a window update frame if a higher value is configured.
	if diff := sc.inflow.available() - defaultWindowSize; diff > 0 {
		sc.vlogf("h2stream: update init window size: %d %d", sc.inflow.available(), diff)
		sc.sendWindowUpdate(nil, int(diff))
	}

	// Start read loop
	go sc.readFrames() // closed by defer sc.conn.Close above

	// write loop
	loopNum := 0
	for {
		loopNum++
		select {
		case wr := <-sc.wantWriteFrameCh:
			if se, ok := wr.write.(writeStreamError); ok {
				sc.resetStream(se.StreamError)
				break
			}
			sc.writeFrame(wr)
		case res := <-sc.wroteFrameCh:
			sc.wroteFrame(res)
		case res := <-sc.readFrameCh:
			if !sc.processFrameFromReader(res) {
				return
			}
			res.readMore()
		case m := <-sc.bodyReadCh:
			sc.noteBodyRead(m.st, m.n)
		case msg := <-sc.serveMsgCh:
			switch v := msg.(type) {
			case func(int):
				v(loopNum) // for testing
			case *serverMessage:
				switch v {
				case idleTimerMsg:
					sc.vlogf("http2: %s session is idle. close.", sc.server.String())
					sc.goAway(http2.ErrCodeNo)
				case shutdownTimerMsg:
					sc.vlogf("GOAWAY close timer fired; closing conn from %v", sc.conn.RemoteAddr())
					return
				case gracefulShutdownMsg:
					sc.startGracefulShutdownInternal()
				default:
					panic("unknown timer")
				}
			case *createStreamRequest:
				sc.createStream(v)
			default:
				panic(fmt.Sprintf("unexpected type %T", v))
			}
		}

		// Start the shutdown timer after sending a GOAWAY. When sending GOAWAY
		// with no error code (graceful shutdown), don't start the timer until
		// all open streams have been completed.
		sentGoAway := sc.inGoAway && !sc.needToSendGoAway && !sc.writingFrame
		gracefulShutdownComplete := sc.goAwayCode == http2.ErrCodeNo && sc.curOpenStreams() == 0
		if sentGoAway && sc.shutdownTimer == nil && (sc.goAwayCode != http2.ErrCodeNo || gracefulShutdownComplete) {
			sc.shutDownIn(goAwayTimeout)
		}
	}
}

func (sc *Session) state(streamID uint32) (streamState, *stream) {
	sc.serveG.check()
	// http://tools.ietf.org/html/rfc7540#section-5.1
	if st, ok := sc.streams[streamID]; ok {
		return st.state, st
	}
	// "The first use of a new stream identifier implicitly closes all
	// streams in the "idle" state that might have been initiated by
	// that peer with a lower-valued stream identifier. For example, if
	// a client sends a HEADERS frame on stream 7 without ever sending a
	// frame on stream 5, then stream 5 transitions to the "closed"
	// state when the first frame for stream 7 is sent or received."
	if streamID%2 == 1 {
		if streamID <= sc.maxClientStreamID {
			return stateClosed, nil
		}
	} else {
		if streamID <= sc.maxPushPromiseID {
			return stateClosed, nil
		}
	}
	return stateIdle, nil
}

func (sc *Session) curOpenStreams() uint32 {
	sc.serveG.check()
	return sc.curClientStreams + sc.curPushedStreams
}

func (sc *Session) newStream(id, pusherID uint32, state streamState) *stream {
	sc.serveG.check()
	if id == 0 {
		panic("internal error: cannot create stream with id 0")
	}

	ctx, cancelCtx := context.WithCancel(sc.ctx)
	st := &stream{
		sc:        sc,
		id:        id,
		state:     state,
		ctx:       ctx,
		cancelCtx: cancelCtx,
		readyCh:   make(chan struct{}),
		body:      pipe{b: &dataBuffer{expected: -1}},
	}
	st.cw.Init()
	st.flow.conn = &sc.flow // link to conn-level counter
	st.flow.add(sc.initialStreamSendWindowSize)
	st.inflow.conn = &sc.inflow // link to conn-level counter
	st.inflow.add(sc.initialStreamRecvWindowSize)

	sc.streams[id] = st
	sc.writeSched.OpenStream(st.id, OpenStreamOptions{PusherID: pusherID})
	if st.isPushed() {
		sc.curPushedStreams++
	} else {
		sc.curClientStreams++
	}

	return st
}

// write reset frame
func (sc *Session) resetStream(se http2.StreamError) {
	sc.serveG.check()
	sc.writeFrame(FrameWriteRequest{
		write: writeStreamError{se}})
	if st, ok := sc.streams[se.StreamID]; ok {
		st.resetQueued = true
	}
}

// closeStream set stream status to stateClosed and remove stream from session
func (sc *Session) closeStream(st *stream, err error) {
	// sc.logf("http2: %s closeStream: %d err: %v", sc.server.String(), st.id, err)
	sc.serveG.check()
	if st.state == stateIdle || st.state == stateClosed {
		panic(fmt.Sprintf("invariant; can't close stream in state %v", st.state))
	}
	st.state = stateClosed
	if st.writeDeadline != nil {
		st.writeDeadline.Stop()
	}
	if st.isPushed() {
		sc.curPushedStreams--
	} else {
		sc.curClientStreams--
	}
	delete(sc.streams, st.id)
	if len(sc.streams) == 0 {
		if sc.idleTimeout != 0 {
			sc.idleTimer.Reset(sc.idleTimeout)
		}
	}
	// Return any buffered unread bytes worth of conn-level flow control.
	// See golang.org/issue/16481
	sc.sendWindowUpdate(nil, st.body.Len())
	st.body.CloseWithError(err)
	st.cancelCtx()
	st.cw.Close() // signals Handler's CloseNotifier, unblocks writes, etc
	sc.writeSched.CloseStream(st.id)
}

func (sc *Session) closeAllStreamsOnConnClose() {
	sc.serveG.check()
	for _, st := range sc.streams {
		sc.closeStream(st, errDisconnected)
	}
}

// Close shuts down session, and wait until session has shut down.
// If session has not yet been fully established, it just close the net connection.
// Otherwise StartGracefulShutdown is called to shut down the session.
func (sc *Session) Close() error {
	select {
	case <-sc.doneServing:
		return nil
	default:
	}

	if !sc.sawFirstSettings {
		return sc.CloseConn()
	}

	sc.StartGracefulShutdown()
	timeout := time.After(goAwayTimeout + 1*time.Second)
	select {
	case <-sc.doneServing:
		return nil
	case <-timeout:
		return errors.New("shutdown timeout")
	}
}

// StartGracefulShutdown gracefully shuts down a connection. This
// sends GOAWAY with ErrCodeNo to tell the client we're gracefully
// shutting down. The connection isn't closed until all current
// streams are done.
//
// StartGracefulShutdown returns immediately; it does not wait until
// the connection has shut down.
func (sc *Session) StartGracefulShutdown() {
	sc.serveG.checkNotOn() // NOT
	sc.shutdownOnce.Do(func() { sc.sendServeMsg(gracefulShutdownMsg) })
}

// After sending GOAWAY, the connection will close after goAwayTimeout.
// If we close the connection immediately after sending GOAWAY, there may
// be unsent data in our kernel receive buffer, which will cause the kernel
// to send a TCP RST on close() instead of a FIN. This RST will abort the
// connection immediately, whether or not the client had received the GOAWAY.
//
// Ideally we should delay for at least 1 RTT + epsilon so the client has
// a chance to read the GOAWAY and stop sending messages. Measuring RTT
// is hard, so we approximate with 1 second. See golang.org/issue/18701.
//
// This is a var so it can be shorter in tests, where all requests uses the
// loopback interface making the expected RTT very small.
//
// TODO: configurable?
var goAwayTimeout = 1 * time.Second

func (sc *Session) startGracefulShutdownInternal() {
	sc.goAway(http2.ErrCodeNo)
}

func (sc *Session) goAway(code ErrCode) {
	sc.serveG.check()
	if sc.inGoAway {
		return
	}
	sc.inGoAway = true
	sc.needToSendGoAway = true
	sc.goAwayCode = code
	sc.scheduleFrameWrite()
}

func (sc *Session) shutDownIn(d time.Duration) {
	sc.serveG.check()
	sc.shutdownTimer = time.AfterFunc(d, sc.onShutdownTimer)
}

func (sc *Session) stopShutdownTimer() {
	sc.serveG.check()
	if t := sc.shutdownTimer; t != nil {
		t.Stop()
	}
}

// A bodyReadMsg tells the server loop that the http.Handler read n
// bytes of the DATA from the client on the given stream.
type bodyReadMsg struct {
	st *stream
	n  int
}

// called from handler goroutines.
// Notes that the handler for the given stream ID read n bytes of its body
// and schedules flow control tokens to be sent.
func (sc *Session) noteBodyReadFromHandler(st *stream, n int, err error) {
	sc.serveG.checkNotOn() // NOT on
	if n > 0 {
		select {
		case sc.bodyReadCh <- bodyReadMsg{st, n}:
		case <-sc.doneServing:
		}
	}
}

func (sc *Session) noteBodyRead(st *stream, n int) {
	sc.serveG.check()
	sc.sendWindowUpdate(nil, n) // conn-level
	if st.state != stateHalfClosedRemote && st.state != stateClosed {
		// Don't send this WINDOW_UPDATE if the stream is closed
		// remotely.
		sc.sendWindowUpdate(st, n)
	}
}
