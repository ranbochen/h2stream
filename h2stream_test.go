package h2stream

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

//go test -v github.com/ranbochen/h2stream
func TestH2Stream(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "h2stream")
}

func logf(format string, a ...interface{}) {
	fmt.Println(fmt.Sprintf(format, a...))
}

var (
	wg sync.WaitGroup
	ln net.Listener
	ch chan net.Conn
)

var _ = BeforeSuite(func() {
	var err error
	ln, ch, err = runTcpServer(&wg)
	Expect(err).NotTo(HaveOccurred())
	// logf("TCP Server listen on: %s", ln.Addr().String())
})

var _ = AfterSuite(func() {
	ln.Close()
	wg.Wait()
})

var _ = Describe("h2stream", func() {
	var _ = Context("adapter", func() {
		It("StreamError", func() {
			var se http2.StreamError = streamError(1, http2.ErrCodeInternal)
			var err error = se
			_, ok := err.(http2.StreamError)
			Expect(ok).To(BeTrue())
			Expect(terminalReadFrameError(se)).To(BeFalse())
		})
	})

	// var _ = Context("Handshake", func() {
	// 	It("server timeout", func() {
	// 		srvConn, cliConn, err := getConn()
	// 		Expect(err).NotTo(HaveOccurred())
	// 		cfg := DefaultSessionConfig
	// 		cfg.ConnectionTimeout = 2 * time.Second
	// 		ss, err := NewServerSession(srvConn, &cfg)
	// 		Expect(err).NotTo(HaveOccurred())

	// 		go func() {
	// 			ss.Serve(nil)
	// 		}()

	// 		ctx := ss.Context()
	// 		timer := time.After(3 * time.Second)
	// 		select {
	// 		case <-ctx.Done():
	// 		case <-timer:
	// 		}
	// 		Expect(ctx.Err()).To(HaveOccurred())
	// 		cliConn.Close()
	// 		ss.CloseConn()
	// 	})

	// 	It("client timeout", func() {
	// 		srvConn, cliConn, err := getConn()
	// 		Expect(err).NotTo(HaveOccurred())
	// 		cfg := DefaultSessionConfig
	// 		cfg.ConnectionTimeout = 2 * time.Second
	// 		cs, err := NewClientSession(srvConn, &cfg)
	// 		Expect(err).NotTo(HaveOccurred())

	// 		go func() {
	// 			cs.Serve(nil)
	// 		}()

	// 		ctx := cs.Context()
	// 		timer := time.After(3 * time.Second)
	// 		select {
	// 		case <-ctx.Done():
	// 		case <-timer:
	// 		}
	// 		Expect(ctx.Err()).To(HaveOccurred())
	// 		cliConn.Close()
	// 		srvConn.Close()
	// 	})

	// 	It("preface error", func() {
	// 		srvConn, cliConn, err := getConn()
	// 		Expect(err).NotTo(HaveOccurred())
	// 		cfg := DefaultSessionConfig
	// 		cfg.ConnectionTimeout = 2 * time.Second
	// 		ss, err := NewServerSession(srvConn, &cfg)
	// 		Expect(err).NotTo(HaveOccurred())

	// 		go func() {
	// 			ss.Serve(serverHandler)
	// 		}()

	// 		errorPreface := []byte("ERROR.ClientPreface.LONG.VERY.LONG")
	// 		_, err = cliConn.Write(errorPreface)
	// 		Expect(err).NotTo(HaveOccurred())
	// 		ctx := ss.Context()
	// 		timer := time.After(3 * time.Second)
	// 		select {
	// 		case <-ctx.Done():
	// 		case <-timer:
	// 		}
	// 		Expect(ctx.Err()).To(HaveOccurred())
	// 		cliConn.Close()
	// 		ss.CloseConn()
	// 	})
	// })

	It("open stream", func() {
		ss, cs := getSess()
		defer ss.CloseConn()
		defer cs.CloseConn()

		go ss.Serve(serverHandler)
		go cs.Serve(clientHandler)
		err := ss.WaitForReady()
		Expect(err).NotTo(HaveOccurred())
		err = cs.WaitForReady()
		Expect(err).NotTo(HaveOccurred())

		// empty/invalid header
		st1, err := ss.OpenStream(nil, nil, false)
		Expect(err).To(HaveOccurred())
		// invalidHeader := http.Header{}
		// invalidHeader.Set(":method", "TEST")
		// st1, err = ss.OpenStream(invalidHeader, nil, false)
		// Expect(err).To(HaveOccurred())

		// open stream reply
		header := http.Header{}
		header.Set("TEST-REQUEST", "request-data")
		header.Set(":method", "GET")
		header.Set(":scheme", "https")
		header.Set(":authority", "user:pwd@google.com:443")
		header.Set(":path", "search?key=value")
		st1, err = ss.OpenStream(header, nil, false)
		Expect(err).NotTo(HaveOccurred())
		st2, err := cs.OpenStream(header, nil, false)
		Expect(err).NotTo(HaveOccurred())
		defer st1.Close()
		defer st2.Close()

		ctx2, cancel2 := context.WithTimeout(st2.Context(), 2*time.Second)
		defer cancel2()
		err = st2.WaitForReply(ctx2)
		Expect(err).NotTo(HaveOccurred())
		rh, ok := st2.ReplyHeader()
		Expect(ok).To(BeTrue())
		Expect(rh.Get("Reply-Header")).Should(Equal("Reply-Data"))
		buf := [100]byte{0}
		n, err := st2.Read(buf[0:])
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(len(helloData)))
		Expect(bytes.Equal(buf[:n], helloData)).To(BeTrue())
		st2.Close()

		time.Sleep(5 * time.Second)
		ss.Close()
	})
})

var helloData = []byte("hello from server")

func serverHandler(s Stream) {
	defer GinkgoRecover()
	st := s.(*stream)
	logf("new strem from client: %d %p %q", st.ID(), st, st.header)
	header := http.Header{}
	header.Set("Reply-Header", "Reply-Data")
	Expect(st.Reply(header, false)).NotTo(HaveOccurred())

	n, err := st.Write(helloData)
	Expect(err).NotTo(HaveOccurred())
	Expect(n).To(Equal(len(helloData)))
}

func clientHandler(s Stream) {
	defer GinkgoRecover()
	st := s.(*stream)
	logf("new strem from server: %d %p %q", st.ID(), st, st.header)
}

func getSess() (ss, cs *Session) {
	srvConn, cliConn, err := getConn()
	Expect(err).NotTo(HaveOccurred())
	ss, err = NewServerSession(srvConn, nil)
	Expect(err).NotTo(HaveOccurred())
	cs, err = NewClientSession(cliConn, nil)
	Expect(err).NotTo(HaveOccurred())
	return
}

func getConn() (srvConn, cliConn net.Conn, err error) {
clear:
	for {
		select {
		case <-ch:
		default:
			break clear
		}
	}
	srvConn, err = net.Dial("tcp", ln.Addr().String())
	if err == nil {
		cliConn = <-ch
	}
	return
}

func runTcpServer(wg *sync.WaitGroup) (net.Listener, chan net.Conn, error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan net.Conn, 5)
	wg.Add(1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				break
			}
			ch <- conn
		}
		wg.Done()
	}()
	return ln, ch, nil
}
