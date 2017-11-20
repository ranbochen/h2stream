package h2stream

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

var clientPreface = []byte(http2.ClientPreface)

// readPreface reads the ClientPreface greeting from the peer
// or returns an error on timeout or an invalid greeting.
// io timeout is set by caller(ConnectionTimeout)
func (sc *Session) readPreface() error {
	// Read the client preface
	buf := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(sc.conn, buf); err != nil {
		return err
	} else if !bytes.Equal(buf, clientPreface) {
		return fmt.Errorf("bogus greeting %q", buf)
	}
	if VerboseLogs {
		sc.vlogf("http2: server: client %v said hello", sc.conn.RemoteAddr())
	}

	return nil
}

type readFrameResult struct {
	f   Frame // valid until readMore is called
	err error

	// readMore should be called once the consumer no longer needs or
	// retains f. After readMore, f is invalid and more frames can be
	// read.
	readMore func()
}

// readFrames is the loop that reads incoming frames.
// It takes care to only read one frame at a time, blocking until the
// consumer is done with the frame.
// It's run on its own goroutine.
func (sc *Session) readFrames() {
	gate := make(gate)
	gateDone := gate.Done
	for {
		f, err := sc.framer.ReadFrame()
		select {
		case sc.readFrameCh <- readFrameResult{f, err, gateDone}:
		case <-sc.doneServing:
			return
		}
		select {
		case <-gate:
		case <-sc.doneServing:
			return
		}
		if terminalReadFrameError(err) {
			return
		}
	}
}

// terminalReadFrameError reports whether err is an unrecoverable
// error from ReadFrame and no other frames should be read.
func terminalReadFrameError(err error) bool {
	if _, ok := err.(http2.StreamError); ok {
		return false
	}
	return err != nil
}

// processFrameFromReader processes the serve loop's read from readFrameCh from the
// frame-reading goroutine.
// processFrameFromReader returns whether the connection should be kept open.
func (sc *Session) processFrameFromReader(res readFrameResult) bool {
	sc.serveG.check()
	err := res.err
	if err != nil {
		if err == http2.ErrFrameTooLarge {
			sc.goAway(http2.ErrCodeFrameSize)
			return true // goAway will close the loop
		}
		clientGone := err == io.EOF || err == io.ErrUnexpectedEOF || isClosedConnError(err)
		if clientGone {
			// TODO: could we also get into this state if
			// the peer does a half close
			// (e.g. CloseWrite) because they're done
			// sending frames but they're still wanting
			// our open replies?  Investigate.
			// TODO: add CloseWrite to crypto/tls.Conn first
			// so we have a way to test this? I suppose
			// just for testing we could have a non-TLS mode.
			return false
		}
	} else {
		f := res.f
		if VerboseLogs {
			sc.vlogf("http2: %s read frame %v", sc.server.String(), summarizeFrame(f))
		}
		err = sc.processFrame(f)
		if err == nil {
			return true
		}
	}

	switch ev := err.(type) {
	case http2.StreamError:
		sc.resetStream(ev)
		return true
	case goAwayFlowError:
		sc.goAway(http2.ErrCodeFlowControl)
		return true
	case ConnectionError:
		sc.logf("http2: %s connection error from %v: %v", sc.server.String(), sc.conn.RemoteAddr(), ev)
		sc.goAway(ErrCode(ev))
		return true // goAway will handle shutdown
	default:
		if res.err != nil {
			sc.vlogf("http2: %s closing connection; error reading frame from client %s: %v",
				sc.server.String(), sc.conn.RemoteAddr(), err)
		} else {
			sc.logf("http2: %s closing connection: %v", sc.server.String(), err)
		}
		return false
	}
}

func (sc *Session) processFrame(f Frame) error {
	sc.serveG.check()

	// First frame received must be SETTINGS.
	if !sc.sawFirstSettings {
		if _, ok := f.(*SettingsFrame); !ok {
			return ConnectionError(http2.ErrCodeProtocol)
		}
		sc.sawFirstSettings = true
		close(sc.sessionReady)
		// connection established, reset deadline set by ConnectionTimeout
		sc.conn.SetDeadline(time.Time{})
	}

	switch f := f.(type) {
	case *SettingsFrame:
		return sc.processSettings(f)
	case *MetaHeadersFrame:
		return sc.processHeaders(f)
	case *WindowUpdateFrame:
		return sc.processWindowUpdate(f)
	case *PingFrame:
		return sc.processPing(f)
	case *DataFrame:
		return sc.processData(f)
	case *RSTStreamFrame:
		return sc.processResetStream(f)
	case *PriorityFrame:
		return sc.processPriority(f)
	case *GoAwayFrame:
		return sc.processGoAway(f)
	case *PushPromiseFrame:
		// A client cannot push. Thus, servers MUST treat the receipt of a PUSH_PROMISE
		// frame as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(http2.ErrCodeProtocol)
	default:
		sc.vlogf("http2: %s ignoring frame: %v", sc.server.String(), f.Header())
		return nil
	}
}

func (sc *Session) processPing(f *PingFrame) error {
	sc.serveG.check()
	if f.IsAck() {
		// 6.7 PING: " An endpoint MUST NOT respond to PING frames
		// containing this flag."
		return nil
	}
	if f.StreamID != 0 {
		// "PING frames are not associated with any individual
		// stream. If a PING frame is received with a stream
		// identifier field value other than 0x0, the recipient MUST
		// respond with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR."
		return ConnectionError(http2.ErrCodeProtocol)
	}
	if sc.inGoAway && sc.goAwayCode != http2.ErrCodeNo {
		return nil
	}
	sc.writeFrame(FrameWriteRequest{write: writePingAck{f}})
	return nil
}

func (sc *Session) processWindowUpdate(f *http2.WindowUpdateFrame) error {
	sc.serveG.check()
	switch {
	case f.StreamID != 0: // stream-level flow control
		state, st := sc.state(f.StreamID)
		if state == stateIdle {
			// Section 5.1: "Receiving any frame other than HEADERS
			// or PRIORITY on a stream in this state MUST be
			// treated as a connection error (Section 5.4.1) of
			// type PROTOCOL_ERROR."
			return ConnectionError(http2.ErrCodeProtocol)
		}
		if st == nil {
			// "WINDOW_UPDATE can be sent by a peer that has sent a
			// frame bearing the END_STREAM flag. This means that a
			// receiver could receive a WINDOW_UPDATE frame on a "half
			// closed (remote)" or "closed" stream. A receiver MUST
			// NOT treat this as an error, see Section 5.1."
			return nil
		}
		if !st.flow.add(int32(f.Increment)) {
			return streamError(f.StreamID, http2.ErrCodeFlowControl)
		}
	default: // connection-level flow control
		if !sc.flow.add(int32(f.Increment)) {
			return goAwayFlowError{}
		}
	}
	sc.scheduleFrameWrite()
	return nil
}

func (sc *Session) processResetStream(f *http2.RSTStreamFrame) error {
	sc.serveG.check()
	state, st := sc.state(f.StreamID)
	if state == stateIdle {
		// 6.4 "RST_STREAM frames MUST NOT be sent for a
		// stream in the "idle" state. If a RST_STREAM frame
		// identifying an idle stream is received, the
		// recipient MUST treat this as a connection error
		// (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(http2.ErrCodeProtocol)
	}
	if st != nil {
		sc.closeStream(st, streamError(f.StreamID, f.ErrCode))
	}
	return nil
}

func (sc *Session) processSettings(f *SettingsFrame) error {
	sc.serveG.check()
	if f.IsAck() {
		sc.unackedSettings--
		if sc.unackedSettings < 0 {
			// Why is the peer ACKing settings we never sent?
			// The spec doesn't mention this case, but
			// hang up on them anyway.
			return ConnectionError(http2.ErrCodeProtocol)
		}
		return nil
	}
	if err := f.ForeachSetting(sc.processSetting); err != nil {
		return err
	}
	sc.needToSendSettingsAck = true
	sc.scheduleFrameWrite()
	return nil
}

func (sc *Session) processSetting(s Setting) error {
	sc.serveG.check()
	if err := s.Valid(); err != nil {
		return err
	}
	if VerboseLogs {
		sstype := "client"
		if sc.server {
			sstype = "server"
		}
		sc.vlogf("http2: %s processing setting %v", sstype, s)
	}
	switch s.ID {
	case http2.SettingHeaderTableSize:
		sc.headerTableSize = s.Val
		sc.hpackEncoder.SetMaxDynamicTableSize(s.Val)
	case http2.SettingEnablePush:
		sc.pushEnabled = s.Val != 0
	case http2.SettingMaxConcurrentStreams:
		sc.peerMaxStreams = s.Val
	case http2.SettingInitialWindowSize:
		return sc.processSettingInitialWindowSize(s.Val)
	case http2.SettingMaxFrameSize:
		sc.maxFrameSize = int32(s.Val) // the maximum valid s.Val is < 2^31
	case http2.SettingMaxHeaderListSize:
		sc.peerMaxHeaderListSize = s.Val
	default:
		// Unknown setting: "An endpoint that receives a SETTINGS
		// frame with any unknown or unsupported identifier MUST
		// ignore that setting."
		if VerboseLogs {
			sc.vlogf("http2: %s ignoring unknown setting %v", sc.server.String(), s)
		}
	}
	return nil
}

func (sc *Session) processSettingInitialWindowSize(val uint32) error {
	sc.serveG.check()
	// Note: val already validated to be within range by
	// processSetting's Valid call.

	// "A SETTINGS frame can alter the initial flow control window
	// size for all current streams. When the value of
	// SETTINGS_INITIAL_WINDOW_SIZE changes, a receiver MUST
	// adjust the size of all stream flow control windows that it
	// maintains by the difference between the new value and the
	// old value."
	old := sc.initialStreamSendWindowSize
	sc.initialStreamSendWindowSize = int32(val)
	growth := int32(val) - old // may be negative
	for _, st := range sc.streams {
		if !st.flow.add(growth) {
			// 6.9.2 Initial Flow Control Window Size
			// "An endpoint MUST treat a change to
			// SETTINGS_INITIAL_WINDOW_SIZE that causes any flow
			// control window to exceed the maximum size as a
			// connection error (Section 5.4.1) of type
			// FLOW_CONTROL_ERROR."
			return ConnectionError(http2.ErrCodeFlowControl)
		}
	}
	return nil
}

func (sc *Session) processData(f *DataFrame) error {
	sc.serveG.check()
	if sc.inGoAway && sc.goAwayCode != http2.ErrCodeNo {
		return nil
	}
	data := f.Data()

	// "If a DATA frame is received whose stream is not in "open"
	// or "half closed (local)" state, the recipient MUST respond
	// with a stream error (Section 5.4.2) of type STREAM_CLOSED."
	id := f.Header().StreamID
	state, st := sc.state(id)
	if id == 0 || state == stateIdle {
		// Section 5.1: "Receiving any frame other than HEADERS
		// or PRIORITY on a stream in this state MUST be
		// treated as a connection error (Section 5.4.1) of
		// type PROTOCOL_ERROR."
		return ConnectionError(http2.ErrCodeProtocol)
	}
	if st == nil || state != stateOpen || st.gotTrailerHeader || st.resetQueued {
		// This includes sending a RST_STREAM if the stream is
		// in stateHalfClosedLocal (which currently means that
		// the http.Handler returned, so it's done reading &
		// done writing). Try to stop the client from sending
		// more DATA.

		// But still enforce their connection-level flow control,
		// and return any flow control bytes since we're not going
		// to consume them.
		if sc.inflow.available() < int32(f.Length) {
			return streamError(id, http2.ErrCodeFlowControl)
		}
		// Deduct the flow control from inflow, since we're
		// going to immediately add it back in
		// sendWindowUpdate, which also schedules sending the
		// frames.
		sc.inflow.take(int32(f.Length))
		sc.sendWindowUpdate(nil, int(f.Length)) // conn-level

		if st != nil && st.resetQueued {
			// Already have a stream error in flight. Don't send another.
			return nil
		}
		return streamError(id, http2.ErrCodeStreamClosed)
	}

	if f.Length > 0 {
		// Check whether the client has flow control quota.
		if st.inflow.available() < int32(f.Length) {
			return streamError(id, http2.ErrCodeFlowControl)
		}
		st.inflow.take(int32(f.Length))

		if len(data) > 0 {
			wrote, err := st.body.Write(data)
			if err != nil {
				return streamError(id, http2.ErrCodeStreamClosed)
			}
			if wrote != len(data) {
				panic("internal error: bad Writer")
			}
			st.bodyBytes += int64(len(data))
		}

		// Return any padded flow control now, since we won't
		// refund it later on body reads.
		if pad := int32(f.Length) - int32(len(data)); pad > 0 {
			sc.sendWindowUpdate32(nil, pad)
			sc.sendWindowUpdate32(st, pad)
		}
	}
	if f.StreamEnded() {
		st.endStream()
	}
	return nil
}

func (sc *Session) processGoAway(f *GoAwayFrame) error {
	sc.serveG.check()
	if f.ErrCode != http2.ErrCodeNo || VerboseLogs {
		sc.logf("http2: %s received GOAWAY %+v, starting graceful shutdown", sc.server.String(), f)
	}
	sc.startGracefulShutdownInternal()
	// http://tools.ietf.org/html/rfc7540#section-6.8
	// We should not create any new streams, which means we should disable push.
	sc.pushEnabled = false
	return nil
}

func (sc *Session) processHeaders(f *http2.MetaHeadersFrame) error {
	sc.serveG.check()
	id := f.StreamID
	if sc.inGoAway {
		// Ignore.
		return nil
	}
	// sc.logf("http2: %s processHeaders stream: %d %d", sc.server.String(), id, len(sc.streams))
	if f.Truncated {
		// Their header list was too long.
		// refuse stream OR disconnect session? ConnectionError
		return streamError(id, http2.ErrCodeProtocol)
	}

	// reply OpenStream from peer or trailerHeader
	if st := sc.streams[f.StreamID]; st != nil {
		if st.resetQueued {
			// We're sending RST_STREAM to close the stream, so don't bother
			// processing this frame.
			return nil
		}

		// first header frame is response header
		if !st.replied {
			st.replied = true
			reply := http.Header{}
			for _, hf := range f.Fields {
				reply.Add(hf.Name, hf.Value)
			}
			st.replyHeader = reply
			close(st.readyCh)
			return nil
		}

		// second is trailer
		return st.processTrailerHeaders(f)
	}

	// peer open a new stream
	// http://tools.ietf.org/html/rfc7540#section-5.1.1
	// Streams initiated by a client MUST use odd-numbered stream
	// identifiers. [...] An endpoint that receives an unexpected
	// stream identifier MUST respond with a connection error
	// (Section 5.4.1) of type PROTOCOL_ERROR.
	if (sc.server && id%2 != 1) || (!sc.server && id%2 != 0) {
		return ConnectionError(http2.ErrCodeProtocol)
	}
	maxStreamID := &sc.maxClientStreamID
	curStreams := sc.curClientStreams
	if !sc.server {
		maxStreamID = &sc.maxPushPromiseID
		curStreams = sc.curPushedStreams
	}
	if id <= *maxStreamID {
		return ConnectionError(http2.ErrCodeProtocol)
	}
	*maxStreamID = id

	if sc.idleTimer != nil {
		sc.idleTimer.Stop()
	}

	// http://tools.ietf.org/html/rfc7540#section-5.1.2
	// [...] Endpoints MUST NOT exceed the limit set by their peer. An
	// endpoint that receives a HEADERS frame that causes their
	// advertised concurrent stream limit to be exceeded MUST treat
	// this as a stream error (Section 5.4.2) of type PROTOCOL_ERROR
	// or REFUSED_STREAM.
	if curStreams+1 > sc.advMaxStreams {
		if sc.unackedSettings == 0 {
			// They should know better.
			return streamError(id, http2.ErrCodeProtocol)
		}
		// Assume it's a network race, where they just haven't
		// received our last SETTINGS update. But actually
		// this can't happen yet, because we don't yet provide
		// a way for users to adjust server parameters at
		// runtime.
		return streamError(id, http2.ErrCodeRefusedStream)
	}

	// no handler refuse stream
	if sc.handler == nil {
		return streamError(id, http2.ErrCodeRefusedStream)
	}

	initialState := stateOpen
	if f.StreamEnded() {
		initialState = stateHalfClosedRemote
	}
	st := sc.newStream(id, 0, initialState)
	header := http.Header{}
	for _, hf := range f.Fields {
		header.Add(hf.Name, hf.Value)
	}
	st.header = header

	if f.HasPriority() {
		if err := checkPriority(f.StreamID, f.Priority); err != nil {
			return err
		}
		sc.writeSched.AdjustStream(st.id, f.Priority)
	}
	// TODO: if initialState is stateHalfClosedRemote, do we need to close body with io.EOF?
	// run handler
	go sc.handler(st)
	return nil
}

func checkPriority(streamID uint32, p PriorityParam) error {
	if streamID == p.StreamDep {
		// Section 5.3.1: "A stream cannot depend on itself. An endpoint MUST treat
		// this as a stream error (Section 5.4.2) of type PROTOCOL_ERROR."
		// Section 5.3.3 says that a stream can depend on one of its dependencies,
		// so it's only self-dependencies that are forbidden.
		return streamError(streamID, http2.ErrCodeProtocol)
	}
	return nil
}

func (sc *Session) processPriority(f *PriorityFrame) error {
	if sc.inGoAway {
		return nil
	}
	if err := checkPriority(f.StreamID, f.PriorityParam); err != nil {
		return err
	}
	sc.writeSched.AdjustStream(f.StreamID, f.PriorityParam)
	return nil
}
