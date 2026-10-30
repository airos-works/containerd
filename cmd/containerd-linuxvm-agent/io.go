//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/containerd/v2/pkg/linuxvm/mux"
	"github.com/containerd/ttrpc"
)

// ioStreams pairs the stdio streams the host opens ("io <token>") with the
// Create and Exec requests that name them ("mux:<token>").
type ioStreams struct {
	mu      sync.Mutex
	streams map[string]*mux.Stream
	waiters map[string]chan *mux.Stream
}

func newIOStreams() *ioStreams {
	return &ioStreams{
		streams: make(map[string]*mux.Stream),
		waiters: make(map[string]chan *mux.Stream),
	}
}

func (r *ioStreams) register(token string, st *mux.Stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.waiters[token]; ok {
		delete(r.waiters, token)
		w <- st
		return
	}
	r.streams[token] = st
	// a stream no request claims must not linger
	time.AfterFunc(time.Minute, func() {
		r.mu.Lock()
		if s, ok := r.streams[token]; ok && s == st {
			delete(r.streams, token)
			st.Close()
		}
		r.mu.Unlock()
	})
}

// claim returns the stream a request names, waiting briefly for it: the
// host opens the streams before it sends the request, but they race.
func (r *ioStreams) claim(ctx context.Context, name string) (*mux.Stream, error) {
	if name == "" {
		return nil, nil
	}
	token, ok := strings.CutPrefix(name, linuxvm.IOPrefix)
	if !ok {
		return nil, fmt.Errorf("stdio %q is not a host stream", name)
	}
	r.mu.Lock()
	if st, ok := r.streams[token]; ok {
		delete(r.streams, token)
		r.mu.Unlock()
		return st, nil
	}
	w := make(chan *mux.Stream, 1)
	r.waiters[token] = w
	r.mu.Unlock()
	select {
	case st := <-w:
		return st, nil
	case <-ctx.Done():
	case <-time.After(5 * time.Minute): // minutes on an emulated VM
	}
	r.mu.Lock()
	delete(r.waiters, token)
	r.mu.Unlock()
	return nil, fmt.Errorf("stdio stream %s never arrived", token)
}

// chanListener hands the ttrpc server the streams with the ttrpc header.
type chanListener struct {
	conns <-chan net.Conn
	done  <-chan struct{}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { return nil }
func (l *chanListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "mux" }
func (dummyAddr) String() string  { return linuxvm.PortName }

func serveTTRPC(svc *taskService, conns <-chan net.Conn, done <-chan struct{}) {
	server, err := ttrpc.NewServer()
	if err != nil {
		logf("ttrpc: %v", err)
		return
	}
	svc.register(server)
	go func() {
		<-done
		_ = server.Close()
	}()
	_ = server.Serve(context.Background(), &chanListener{conns: conns, done: done})
}

// pipe copies until either side ends, then closes the write side.
func pipe(dst io.WriteCloser, src io.Reader) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.Close()
}
