package h2stream

import (
	"net/http"
	"time"
)

// SessionConfig consists the configurations of a h2stream Session.
type SessionConfig struct {
	// http/2 settings
	InitialSessionWindowSize uint32 // initial window size for session
	InitialStreamWindowSize  uint32 // initial window size for stream(send/recv)
	MaxFrameSize             uint32
	HeaderTableSize          uint32
	MaxHeaderBytes           uint32 // the maximum permitted size of the headers
	WriteBufferSize          int
	ReadBufferSize           int

	// timeout
	ConnectionTimeout time.Duration // timeout for connection establishment(http/2 handshaking).default is 10 seconds
	IdleTimeout       time.Duration // default is 0, nerver timeout
}

// DefaultSessionConfig is a default session configuration.
// !do not modify at run time
var DefaultSessionConfig = SessionConfig{
	InitialSessionWindowSize: defaultWindowSize,
	InitialStreamWindowSize:  defaultWindowSize,
	MaxFrameSize:             http2MaxFrameLen,
	HeaderTableSize:          http2InitHeaderTableSize,
	MaxHeaderBytes:           http.DefaultMaxHeaderBytes,
	WriteBufferSize:          defaultWriteBufSize,
	ReadBufferSize:           defaultWriteBufSize,
	ConnectionTimeout:        10 * time.Second,
	IdleTimeout:              0,
}
