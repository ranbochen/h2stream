package h2stream

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"
)

// errno returns v's underlying uintptr, else 0.
//
// TODO: remove this helper function once http2 can use build
// tags. See comment in isClosedConnError.
func errno(v error) uintptr {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Uintptr {
		return uintptr(rv.Uint())
	}
	return 0
}

// isClosedConnError reports whether err is an error from use of a closed
// network connection.
func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}

	// TODO: remove this string search and be more like the Windows
	// case below. That might involve modifying the standard library
	// to return better error types.
	str := err.Error()
	if strings.Contains(str, "use of closed network connection") {
		return true
	}

	// TODO(bradfitz): x/tools/cmd/bundle doesn't really support
	// build tags, so I can't make an http2_windows.go file with
	// Windows-specific stuff. Fix that and move this, once we
	// have a way to bundle this into std's net/http somehow.
	if runtime.GOOS == "windows" {
		if oe, ok := err.(*net.OpError); ok && oe.Op == "read" {
			if se, ok := oe.Err.(*os.SyscallError); ok && se.Syscall == "wsarecv" {
				const WSAECONNABORTED = 10053
				const WSAECONNRESET = 10054
				if n := errno(se.Err); n == WSAECONNRESET || n == WSAECONNABORTED {
					return true
				}
			}
		}
	}
	return false
}

// copy from http2.go
var httpCodeStringCommon = map[int]string{} // n -> strconv.Itoa(n)

func init() {
	for i := 100; i <= 999; i++ {
		if v := http.StatusText(i); v != "" {
			httpCodeStringCommon[i] = strconv.Itoa(i)
		}
	}
}

func httpCodeString(code int) string {
	if s, ok := httpCodeStringCommon[code]; ok {
		return s
	}
	return strconv.Itoa(code)
}

var sorterPool = sync.Pool{New: func() interface{} { return new(sorter) }}

type sorter struct {
	v []string // owned by sorter
}

func (s *sorter) Len() int           { return len(s.v) }
func (s *sorter) Swap(i, j int)      { s.v[i], s.v[j] = s.v[j], s.v[i] }
func (s *sorter) Less(i, j int) bool { return s.v[i] < s.v[j] }

// Keys returns the sorted keys of h.
//
// The returned slice is only valid until s used again or returned to
// its pool.
func (s *sorter) Keys(h http.Header) []string {
	keys := s.v[:0]
	for k := range h {
		keys = append(keys, k)
	}
	s.v = keys
	sort.Sort(s)
	return keys
}

func (s *sorter) SortStrings(ss []string) {
	// Our sorter works on s.v, which sorter owns, so
	// stash it away while we sort the user's buffer.
	save := s.v
	s.v = ss
	sort.Sort(s)
	s.v = save
}

// from pkg io
type stringWriter interface {
	WriteString(s string) (n int, err error)
}

// A gate lets two goroutines coordinate their activities.
type gate chan struct{}

func (g gate) Done() { g <- struct{}{} }
func (g gate) Wait() { <-g }

// A closeWaiter is like a sync.WaitGroup but only goes 1 to 0 (open to closed).
type closeWaiter chan struct{}

// Init makes a closeWaiter usable.
// It exists because so a closeWaiter value can be placed inside a
// larger struct and have the Mutex and Cond's memory in the same
// allocation.
func (cw *closeWaiter) Init() {
	*cw = make(chan struct{})
}

// Close marks the closeWaiter as closed and unblocks any waiters.
func (cw closeWaiter) Close() {
	close(cw)
}

// Wait waits for the closeWaiter to become closed.
func (cw closeWaiter) Wait() {
	<-cw
}

// copy from frame.go
func summarizeFrame(f Frame) string {
	var buf bytes.Buffer
	buf.WriteString(f.Header().String())
	// f.Header().writeDebug(&buf)
	switch f := f.(type) {
	case *http2.SettingsFrame:
		n := 0
		f.ForeachSetting(func(s Setting) error {
			n++
			if n == 1 {
				buf.WriteString(", settings:")
			}
			fmt.Fprintf(&buf, " %v=%v,", s.ID, s.Val)
			return nil
		})
		if n > 0 {
			buf.Truncate(buf.Len() - 1) // remove trailing comma
		}
	case *http2.DataFrame:
		data := f.Data()
		const max = 256
		if len(data) > max {
			data = data[:max]
		}
		fmt.Fprintf(&buf, " data=%q", data)
		if len(f.Data()) > max {
			fmt.Fprintf(&buf, " (%d bytes omitted)", len(f.Data())-max)
		}
	case *http2.WindowUpdateFrame:
		if f.StreamID == 0 {
			buf.WriteString(" (conn)")
		}
		fmt.Fprintf(&buf, " incr=%v", f.Increment)
	case *http2.PingFrame:
		fmt.Fprintf(&buf, " ping=%q", f.Data[:])
	case *http2.GoAwayFrame:
		fmt.Fprintf(&buf, " LastStreamID=%v ErrCode=%v Debug=%q",
			f.LastStreamID, f.ErrCode, f.DebugData())
	case *http2.RSTStreamFrame:
		fmt.Fprintf(&buf, " ErrCode=%v", f.ErrCode)
	}
	return buf.String()
}
