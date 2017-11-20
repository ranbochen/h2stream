package h2stream

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/net/http2"
)

// st may be nil for conn-level
func (sc *Session) sendWindowUpdate(st *stream, n int) {
	sc.serveG.check()
	// "The legal range for the increment to the flow control
	// window is 1 to 2^31-1 (2,147,483,647) octets."
	// A Go Read call on 64-bit machines could in theory read
	// a larger Read than this. Very unlikely, but we handle it here
	// rather than elsewhere for now.
	const maxUint31 = 1<<31 - 1
	for n >= maxUint31 {
		sc.sendWindowUpdate32(st, maxUint31)
		n -= maxUint31
	}
	sc.sendWindowUpdate32(st, int32(n))
}

// st may be nil for conn-level
func (sc *Session) sendWindowUpdate32(st *stream, n int32) {
	sc.serveG.check()
	if n == 0 {
		return
	}
	if n < 0 {
		panic("negative update")
	}
	var streamID uint32
	if st != nil {
		streamID = st.id
	}
	sc.writeFrame(FrameWriteRequest{
		write:  writeWindowUpdate{streamID: streamID, n: uint32(n)},
		stream: st,
	})
	var ok bool
	if st == nil {
		ok = sc.inflow.add(n)
	} else {
		ok = st.inflow.add(n)
	}
	if !ok {
		panic("internal error; sent too many window updates without decrements?")
	}
}

// frameWriteResult is the message passed from writeFrameAsync to the serve goroutine.
type frameWriteResult struct {
	wr  FrameWriteRequest // what was written (or attempted)
	err error             // result of the writeFrame call
}

// writeFrameAsync runs in its own goroutine and writes a single frame
// and then reports when it's done.
// At most one goroutine can be running writeFrameAsync at a time per
// Session.
func (sc *Session) writeFrameAsync(wr FrameWriteRequest) {
	err := wr.write.writeFrame(sc)
	sc.wroteFrameCh <- frameWriteResult{wr, err}
}

var errChanPool = sync.Pool{
	New: func() interface{} { return make(chan error, 1) },
}

var writeDataPool = sync.Pool{
	New: func() interface{} { return new(writeData) },
}

// writeDataFromHandler writes DATA response frames from a handler on
// the given stream.
func (sc *Session) writeDataFromHandler(stream *stream, data []byte, endStream bool) error {
	ch := errChanPool.Get().(chan error)
	writeArg := writeDataPool.Get().(*writeData)
	*writeArg = writeData{stream.id, data, endStream}
	err := sc.writeFrameFromHandler(FrameWriteRequest{
		write:  writeArg,
		stream: stream,
		done:   ch,
	})
	if err != nil {
		return err
	}
	var frameWriteDone bool // the frame write is done (successfully or not)
	select {
	case err = <-ch:
		frameWriteDone = true
	case <-sc.doneServing:
		return errDisconnected
	case <-stream.cw:
		// If both ch and stream.cw were ready (as might
		// happen on the final Write after an http.Handler
		// ends), prefer the write result. Otherwise this
		// might just be us successfully closing the stream.
		// The writeFrameAsync and serve goroutines guarantee
		// that the ch send will happen before the stream.cw
		// close.
		select {
		case err = <-ch:
			frameWriteDone = true
		default:
			return errStreamClosed
		}
	}
	errChanPool.Put(ch)
	if frameWriteDone {
		writeDataPool.Put(writeArg)
	}
	return err
}

// writeFrameFromHandler sends wr to sc.wantWriteFrameCh, but aborts
// if the connection has gone away.
//
// This must not be run from the serve goroutine itself, else it might
// deadlock writing to sc.wantWriteFrameCh (which is only mildly
// buffered and is read by serve itself). If you're on the serve
// goroutine, call writeFrame instead.
func (sc *Session) writeFrameFromHandler(wr FrameWriteRequest) error {
	sc.serveG.checkNotOn() // NOT
	select {
	case sc.wantWriteFrameCh <- wr:
		return nil
	case <-sc.doneServing:
		// Serve loop is gone.
		// Client has closed their connection to the server.
		return errDisconnected
	}
}

// writeFrame schedules a frame to write and sends it if there's nothing
// already being written.
//
// There is no pushback here (the serve goroutine never blocks). It's
// the http.Handlers that block, waiting for their previous frames to
// make it onto the wire
//
// If you're not on the serve goroutine, use writeFrameFromHandler instead.
func (sc *Session) writeFrame(wr FrameWriteRequest) {
	sc.serveG.check()

	// If true, wr will not be written and wr.done will not be signaled.
	var ignoreWrite bool

	// We are not allowed to write frames on closed streams. RFC 7540 Section
	// 5.1.1 says: "An endpoint MUST NOT send frames other than PRIORITY on
	// a closed stream." Our server never sends PRIORITY, so that exception
	// does not apply.
	//
	// The Session might close an open stream while the stream's handler
	// is still running. For example, the server might close a stream when it
	// receives bad data from the client. If this happens, the handler might
	// attempt to write a frame after the stream has been closed (since the
	// handler hasn't yet been notified of the close). In this case, we simply
	// ignore the frame. The handler will notice that the stream is closed when
	// it waits for the frame to be written.
	//
	// As an exception to this rule, we allow sending RST_STREAM after close.
	// This allows us to immediately reject new streams without tracking any
	// state for those streams (except for the queued RST_STREAM frame). This
	// may result in duplicate RST_STREAMs in some cases, but the client should
	// ignore those.
	if wr.StreamID() != 0 {
		_, isReset := wr.write.(writeStreamError)
		if state, _ := sc.state(wr.StreamID()); state == stateClosed && !isReset {
			ignoreWrite = true
		}
	}

	// Don't send a 100-continue response if we've already sent headers.
	// See golang.org/issue/14030.
	switch wr.write.(type) {
	case *writeResHeaders:
		wr.stream.wroteHeaders = true
	case write100ContinueHeadersFrame:
		if wr.stream.wroteHeaders {
			// We do not need to notify wr.done because this frame is
			// never written with wr.done != nil.
			if wr.done != nil {
				panic("wr.done != nil for write100ContinueHeadersFrame")
			}
			ignoreWrite = true
		}
	}

	if !ignoreWrite {
		sc.writeSched.Push(wr)
	}
	sc.scheduleFrameWrite()
}

// startFrameWrite starts a goroutine to write wr (in a separate
// goroutine since that might block on the network), and updates the
// serve goroutine's state about the world, updated from info in wr.
func (sc *Session) startFrameWrite(wr FrameWriteRequest) {
	sc.serveG.check()
	if sc.writingFrame {
		panic("internal error: can only be writing one frame at a time")
	}

	st := wr.stream
	if st != nil {
		switch st.state {
		case stateHalfClosedLocal:
			switch wr.write.(type) {
			case writeStreamError, handlerPanicRST, writeWindowUpdate:
				// RFC 7540 Section 5.1 allows sending RST_STREAM, PRIORITY, and WINDOW_UPDATE
				// in this state. (We never send PRIORITY from the server, so that is not checked.)
			default:
				panic(fmt.Sprintf("internal error: attempt to send frame on a half-closed-local stream: %v", wr))
			}
		case stateClosed:
			panic(fmt.Sprintf("internal error: attempt to send frame on a closed stream: %v", wr))
		}
	}
	if wpp, ok := wr.write.(*writePushPromise); ok {
		var err error
		wpp.promisedID, err = wpp.allocatePromisedID()
		if err != nil {
			sc.writingFrameAsync = false
			wr.replyToWriter(err)
			return
		}
	}

	sc.writingFrame = true
	sc.needsFrameFlush = true
	if wr.write.staysWithinBuffer(sc.bw.Available()) {
		sc.writingFrameAsync = false
		err := wr.write.writeFrame(sc)
		sc.wroteFrame(frameWriteResult{wr, err})
	} else {
		sc.writingFrameAsync = true
		go sc.writeFrameAsync(wr)
	}
}

// errHandlerPanicked is the error given to any callers blocked in a read from
// Request.Body when the main goroutine panics. Since most handlers read in the
// the main ServeHTTP goroutine, this will show up rarely.
var errHandlerPanicked = errors.New("http2: handler panicked")

// wroteFrame is called on the serve goroutine with the result of
// whatever happened on writeFrameAsync.
func (sc *Session) wroteFrame(res frameWriteResult) {
	sc.serveG.check()
	if !sc.writingFrame {
		panic("internal error: expected to be already writing a frame")
	}
	sc.writingFrame = false
	sc.writingFrameAsync = false

	wr := res.wr

	if writeEndsStream(wr.write) {
		st := wr.stream
		if st == nil {
			panic("internal error: expecting non-nil stream")
		}
		switch st.state {
		case stateOpen:
			// Here we would go to stateHalfClosedLocal in
			// theory, but since our handler is done and
			// the net/http package provides no mechanism
			// for closing a ResponseWriter while still
			// reading data (see possible TODO at top of
			// this file), we go into closed state here
			// anyway, after telling the peer we're
			// hanging up on them. We'll transition to
			// stateClosed after the RST_STREAM frame is
			// written.
			st.state = stateHalfClosedLocal
			// Section 8.1: a server MAY request that the client abort
			// transmission of a request without error by sending a
			// RST_STREAM with an error code of NO_ERROR after sending
			// a complete response.
			sc.resetStream(streamError(st.id, http2.ErrCodeNo))
		case stateHalfClosedRemote:
			sc.closeStream(st, errHandlerComplete)
		}
	} else {
		switch v := wr.write.(type) {
		case writeStreamError:
			// st may be unknown if the RST_STREAM was generated to reject bad input.
			if st, ok := sc.streams[v.StreamID]; ok {
				sc.closeStream(st, v)
			}
		case handlerPanicRST:
			sc.closeStream(wr.stream, errHandlerPanicked)
		}
	}

	// Reply (if requested) to unblock the ServeHTTP goroutine.
	wr.replyToWriter(res.err)

	sc.scheduleFrameWrite()
}

// scheduleFrameWrite tickles the frame writing scheduler.
//
// If a frame is already being written, nothing happens. This will be called again
// when the frame is done being written.
//
// If a frame isn't being written we need to send one, the best frame
// to send is selected, preferring first things that aren't
// stream-specific (e.g. ACKing settings), and then finding the
// highest priority stream.
//
// If a frame isn't being written and there's nothing else to send, we
// flush the write buffer.
func (sc *Session) scheduleFrameWrite() {
	sc.serveG.check()
	if sc.writingFrame || sc.inFrameScheduleLoop {
		return
	}
	sc.inFrameScheduleLoop = true
	for !sc.writingFrameAsync {
		if sc.needToSendGoAway {
			sc.needToSendGoAway = false
			sc.startFrameWrite(FrameWriteRequest{
				write: &writeGoAway{
					maxStreamID: sc.maxClientStreamID,
					code:        sc.goAwayCode,
				},
			})
			continue
		}
		if sc.needToSendSettingsAck {
			sc.needToSendSettingsAck = false
			sc.startFrameWrite(FrameWriteRequest{write: writeSettingsAck{}})
			continue
		}
		if !sc.inGoAway || sc.goAwayCode == http2.ErrCodeNo {
			if wr, ok := sc.writeSched.Pop(); ok {
				sc.startFrameWrite(wr)
				continue
			}
		}
		if sc.needsFrameFlush {
			sc.startFrameWrite(FrameWriteRequest{write: flushFrameWriter{}})
			sc.needsFrameFlush = false // after startFrameWrite, since it sets this true
			continue
		}
		break
	}
	sc.inFrameScheduleLoop = false
}

// called from handler goroutines.
// h may be nil.
func (sc *Session) writeHeaders(st *stream, headerData *writeResHeaders) error {
	sc.serveG.checkNotOn() // NOT on
	var errc chan error
	if headerData.h != nil {
		// If there's a header map (which we don't own), so we have to block on
		// waiting for this frame to be written, so an http.Flush mid-handler
		// writes out the correct value of keys, before a handler later potentially
		// mutates it.
		errc = errChanPool.Get().(chan error)
	}
	if err := sc.writeFrameFromHandler(FrameWriteRequest{
		write:  headerData,
		stream: st,
		done:   errc,
	}); err != nil {
		return err
	}
	if errc != nil {
		select {
		case err := <-errc:
			errChanPool.Put(errc)
			return err
		case <-sc.doneServing:
			return errDisconnected
		case <-st.cw:
			return errStreamClosed
		}
	}
	return nil
}
