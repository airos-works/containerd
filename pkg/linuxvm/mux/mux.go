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

// Package mux multiplexes byte streams over one connection: the virtio
// serial port between air/OS and its Linux VM, which is a single channel.
// The host opens streams; the VM's agent accepts them. Each stream has its
// own flow-control window, so a stream nobody reads (a container's stdout
// with no client) never stalls the others.
package mux

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	typeOpen   = 1 // payload: the stream's header line
	typeData   = 2
	typeWindow = 3 // payload: uint32 of bytes the receiver consumed
	typeClose  = 4 // the sender writes no more (half close)
	typeReset  = 5 // the stream is gone in both directions

	headerSize = 9 // type (1), stream id (4), length (4)
	maxFrame   = 32 << 10
	window     = 256 << 10
)

// ErrClosed is returned once the session has ended.
var ErrClosed = errors.New("mux: session closed")

// Session is one end of a multiplexed connection.
type Session struct {
	conn io.ReadWriteCloser

	wmu sync.Mutex // serializes frames on conn

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	accept  chan *Stream
	closed  bool
	err     error
	done    chan struct{}
}

// Client starts the opening side of a session.
func Client(conn io.ReadWriteCloser) *Session { return newSession(conn, 1) }

// Server starts the accepting side of a session.
func Server(conn io.ReadWriteCloser) *Session { return newSession(conn, 2) }

func newSession(conn io.ReadWriteCloser, first uint32) *Session {
	s := &Session{
		conn:    conn,
		streams: make(map[uint32]*Stream),
		nextID:  first,
		accept:  make(chan *Stream, 64),
		done:    make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session and every stream on it.
func (s *Session) Close() error {
	s.shutdown(ErrClosed)
	return nil
}

func (s *Session) shutdown(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.err = err
	streams := s.streams
	s.streams = map[uint32]*Stream{}
	s.mu.Unlock()
	_ = s.conn.Close()
	for _, st := range streams {
		st.reset(err)
	}
	close(s.done)
}

// Open starts a stream whose first frame carries header (the peer reads
// it with Stream.Header to decide what the stream is for).
func (s *Session) Open(header string) (*Stream, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	id := s.nextID
	s.nextID += 2
	st := newStream(s, id, header)
	s.streams[id] = st
	s.mu.Unlock()
	if err := s.writeFrame(typeOpen, id, []byte(header)); err != nil {
		s.remove(id)
		return nil, err
	}
	return st, nil
}

// Accept waits for the peer to open a stream.
func (s *Session) Accept() (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, ErrClosed
	}
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	var hdr [headerSize]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], id)
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	if _, err := s.conn.Write(hdr[:]); err != nil {
		go s.shutdown(err)
		return err
	}
	if len(payload) > 0 {
		if _, err := s.conn.Write(payload); err != nil {
			go s.shutdown(err)
			return err
		}
	}
	return nil
}

func (s *Session) readLoop() {
	var hdr [headerSize]byte
	for {
		if _, err := io.ReadFull(s.conn, hdr[:]); err != nil {
			s.shutdown(err)
			return
		}
		typ := hdr[0]
		id := binary.BigEndian.Uint32(hdr[1:5])
		n := binary.BigEndian.Uint32(hdr[5:9])
		if n > window {
			s.shutdown(errors.New("mux: oversized frame"))
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(s.conn, payload); err != nil {
			s.shutdown(err)
			return
		}
		s.mu.Lock()
		st := s.streams[id]
		if typ == typeOpen && st == nil && !s.closed {
			st = newStream(s, id, string(payload))
			s.streams[id] = st
			s.mu.Unlock()
			select {
			case s.accept <- st:
			case <-s.done:
				return
			}
			continue
		}
		s.mu.Unlock()
		if st == nil {
			if typ == typeData {
				// a stream we reset; refuse the rest of it
				_ = s.writeFrame(typeReset, id, nil)
			}
			continue
		}
		switch typ {
		case typeData:
			st.push(payload)
		case typeWindow:
			if len(payload) == 4 {
				st.grant(binary.BigEndian.Uint32(payload))
			}
		case typeClose:
			st.remoteClose()
		case typeReset:
			st.reset(io.ErrClosedPipe)
			s.remove(id)
		}
	}
}

// Stream is one multiplexed byte stream. It implements net.Conn so that
// ttrpc and the stdio and port forwarders can use it directly.
type Stream struct {
	s      *Session
	id     uint32
	header string

	mu        sync.Mutex
	cond      *sync.Cond
	buf       []byte
	unacked   uint32 // consumed bytes not yet granted back to the peer
	sendWin   uint32
	rclosed   bool // peer sent close: reads hit EOF once buf drains
	wclosed   bool // we sent close
	err       error
	rdeadline time.Time
	wdeadline time.Time
}

func newStream(s *Session, id uint32, header string) *Stream {
	st := &Stream{s: s, id: id, header: header, sendWin: window}
	st.cond = sync.NewCond(&st.mu)
	return st
}

// Header is the line the opener sent with the stream.
func (st *Stream) Header() string { return st.header }

func (st *Stream) push(p []byte) {
	st.mu.Lock()
	st.buf = append(st.buf, p...)
	st.mu.Unlock()
	st.cond.Broadcast()
}

func (st *Stream) grant(n uint32) {
	st.mu.Lock()
	st.sendWin += n
	st.mu.Unlock()
	st.cond.Broadcast()
}

func (st *Stream) remoteClose() {
	st.mu.Lock()
	st.rclosed = true
	st.mu.Unlock()
	st.cond.Broadcast()
}

func (st *Stream) reset(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.rclosed = true
	st.mu.Unlock()
	st.cond.Broadcast()
}

// wait blocks on the condition until cond() holds or the deadline passes.
// st.mu is held.
func (st *Stream) wait(deadline time.Time, cond func() bool) error {
	var timer *time.Timer
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer = time.AfterFunc(d, st.cond.Broadcast)
		defer timer.Stop()
	}
	for !cond() {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		st.cond.Wait()
	}
	return nil
}

func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	err := st.wait(st.rdeadline, func() bool { return len(st.buf) > 0 || st.rclosed || st.err != nil })
	if err != nil {
		st.mu.Unlock()
		return 0, err
	}
	if len(st.buf) == 0 {
		err := st.err
		st.mu.Unlock()
		if err != nil && err != io.ErrClosedPipe {
			return 0, err
		}
		return 0, io.EOF
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	if len(st.buf) == 0 {
		st.buf = nil
	}
	st.unacked += uint32(n)
	var ack uint32
	if st.unacked >= window/4 || len(st.buf) == 0 {
		ack = st.unacked
		st.unacked = 0
	}
	st.mu.Unlock()
	if ack > 0 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], ack)
		_ = st.s.writeFrame(typeWindow, st.id, b[:])
	}
	return n, nil
}

func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		err := st.wait(st.wdeadline, func() bool { return st.sendWin > 0 || st.err != nil || st.wclosed })
		if err == nil && st.err != nil {
			err = st.err
		}
		if err == nil && st.wclosed {
			err = io.ErrClosedPipe
		}
		if err != nil {
			st.mu.Unlock()
			return written, err
		}
		n := len(p)
		if n > maxFrame {
			n = maxFrame
		}
		if uint32(n) > st.sendWin {
			n = int(st.sendWin)
		}
		st.sendWin -= uint32(n)
		st.mu.Unlock()
		if err := st.s.writeFrame(typeData, st.id, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// CloseWrite tells the peer no more data follows (its reads see EOF).
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	if st.wclosed || st.err != nil {
		st.mu.Unlock()
		return nil
	}
	st.wclosed = true
	st.mu.Unlock()
	return st.s.writeFrame(typeClose, st.id, nil)
}

// Close ends the stream in both directions.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.err != nil {
		st.mu.Unlock()
		st.s.remove(st.id)
		return nil
	}
	st.err = io.ErrClosedPipe
	st.rclosed = true
	st.mu.Unlock()
	st.cond.Broadcast()
	st.s.remove(st.id)
	return st.s.writeFrame(typeReset, st.id, nil)
}

type addr string

func (a addr) Network() string { return "mux" }
func (a addr) String() string  { return string(a) }

func (st *Stream) LocalAddr() net.Addr  { return addr("local") }
func (st *Stream) RemoteAddr() net.Addr { return addr(st.header) }

func (st *Stream) SetDeadline(t time.Time) error {
	st.mu.Lock()
	st.rdeadline, st.wdeadline = t, t
	st.mu.Unlock()
	st.cond.Broadcast()
	return nil
}

func (st *Stream) SetReadDeadline(t time.Time) error {
	st.mu.Lock()
	st.rdeadline = t
	st.mu.Unlock()
	st.cond.Broadcast()
	return nil
}

func (st *Stream) SetWriteDeadline(t time.Time) error {
	st.mu.Lock()
	st.wdeadline = t
	st.mu.Unlock()
	st.cond.Broadcast()
	return nil
}

var _ net.Conn = (*Stream)(nil)
