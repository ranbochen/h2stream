// adapt type from package http2 to use original code with minimal modification

package h2stream

import (
	"golang.org/x/net/http2"
)

// frame
type Frame = http2.Frame
type Framer = http2.Framer
type DataFrame = http2.DataFrame
type MetaHeadersFrame = http2.MetaHeadersFrame
type PriorityFrame = http2.PriorityFrame
type RSTStreamFrame = http2.RSTStreamFrame
type SettingsFrame = http2.SettingsFrame
type PushPromiseFrame = http2.PushPromiseFrame
type PingFrame = http2.PingFrame
type GoAwayFrame = http2.GoAwayFrame
type WindowUpdateFrame = http2.WindowUpdateFrame
type ContinuationFrame = http2.ContinuationFrame

// errors
type ErrCode = http2.ErrCode
type ConnectionError = http2.ConnectionError

func streamError(id uint32, code ErrCode) http2.StreamError {
	// return StreamError{http2.StreamError{StreamID: id, Code: code}}
	return http2.StreamError{StreamID: id, Code: code}
}

// 6.9.1 The Flow Control Window
// "If a sender receives a WINDOW_UPDATE that causes a flow control
// window to exceed this maximum it MUST terminate either the stream
// or the connection, as appropriate. For streams, [...]; for the
// connection, a GOAWAY frame with a FLOW_CONTROL_ERROR code."
type goAwayFlowError struct{}

func (goAwayFlowError) Error() string { return "connection exceeded flow control window size" }

type Setting = http2.Setting
type PriorityParam = http2.PriorityParam
type PushPromiseParam = http2.PushPromiseParam
type HeadersFrameParam = http2.HeadersFrameParam
