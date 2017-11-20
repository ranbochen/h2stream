package h2stream

import (
	"io"
	"log"
)

// VerboseLogs enable debug log
var VerboseLogs = false

func (se *Session) vlogf(format string, args ...interface{}) {
	if VerboseLogs {
		se.logf(format, args...)
	}
}

func (se *Session) logf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

func (se *Session) condlogf(err error, format string, args ...interface{}) {
	if err == nil {
		return
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF || isClosedConnError(err) {
		// Boring, expected errors.
		se.vlogf(format, args...)
	} else {
		se.logf(format, args...)
	}
}
