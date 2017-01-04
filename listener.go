package typhon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/facebookgo/httpdown"
	log "github.com/monzo/slog"
	"golang.org/x/net/context"
)

const DefaultListenAddr = "127.0.0.1:0"

type Server interface {
	httpdown.Server
	Listener() net.Listener
	WaitC() <-chan struct{}
}

type server struct {
	httpdown.Server
	l net.Listener
}

func (s server) Listener() net.Listener {
	return s.l
}

func (s server) WaitC() <-chan struct{} {
	c := make(chan struct{}, 0)
	go func() {
		s.Server.Wait()
		close(c)
	}()
	return c
}

func Serve(svc Service, l net.Listener) (Server, error) {
	downer := &httpdown.HTTP{
		StopTimeout: 20 * time.Second,
		KillTimeout: 25 * time.Second}
	downerServer := downer.Serve(&http.Server{
		Handler:        httpHandler(svc),
		MaxHeaderBytes: http.DefaultMaxHeaderBytes}, l)
	log.Info(nil, "Listening on %v", l.Addr())
	return server{
		Server: downerServer,
		l:      l}, nil
}

func Listen(svc Service, addr string) (Server, error) {
	// Determine on which address to listen, choosing in order one of:
	// 1. The passed addr
	// 2. PORT variable (listening on all interfaces)
	// 3. Random, available port, on the loopback interface only
	if addr == "" {
		if _addr := os.Getenv("LISTEN_ADDR"); _addr != "" {
			addr = _addr
		} else if port, err := strconv.Atoi(os.Getenv("PORT")); err == nil && port >= 0 {
			addr = fmt.Sprintf(":%d", port)
		} else {
			addr = ":0"
		}
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}

	l, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return nil, err
	}
	return Serve(svc, l)
}

func httpHandler(svc Service) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, httpReq *http.Request) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // if already cancelled on escape, this is a no-op
		done := make(chan struct{})

		// If the ResponseWriter is a CloseNotifier, propagate the cancellation downward via the context
		if cn, ok := rw.(http.CloseNotifier); ok {
			closed := cn.CloseNotify()
			go func() {
				select {
				case <-done:
				case <-ctx.Done():
				case <-closed:
					cancel()
				}
			}()
		}

		if httpReq.Body != nil {
			defer httpReq.Body.Close()
		}

		req := Request{
			Context: ctx,
			Request: *httpReq}
		rsp := svc(req)
		close(done)

		// Write the response out to the wire
		for k, v := range rsp.Header {
			if k == "Content-Length" {
				continue
			}
			rw.Header()[k] = v
		}
		rw.WriteHeader(rsp.StatusCode)
		if rsp.Body != nil {
			defer rsp.Body.Close()
			if _, err := io.Copy(rw, rsp.Body); err != nil {
				log.Error(req, "[Typhon:http:networkTransport] Error copying response body: %v", err)
			}
		}
	})
}
