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

package mux

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

func pair() (*Session, *Session) {
	a, b := net.Pipe()
	return Client(a), Server(b)
}

func TestStreams(t *testing.T) {
	c, s := pair()
	defer c.Close()
	defer s.Close()

	big := make([]byte, 3<<20)
	_, _ = rand.Read(big)

	go func() {
		for {
			st, err := s.Accept()
			if err != nil {
				return
			}
			go func() {
				// echo, then half close
				_, _ = io.Copy(st, st)
				_ = st.CloseWrite()
			}()
		}
	}()

	// a stream nobody reads must not stall another
	stalled, err := c.Open("stalled")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = stalled.Write(big) }()

	st, err := c.Open("echo")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = st.Write(big)
		_ = st.CloseWrite()
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("echo mismatch: %d bytes", len(got))
	}
}

func TestDeadline(t *testing.T) {
	c, s := pair()
	defer c.Close()
	defer s.Close()
	st, _ := c.Open("x")
	_ = st.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected deadline error")
	}
}

func TestSessionEnd(t *testing.T) {
	c, s := pair()
	st, _ := c.Open("x")
	s.Close()
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected error after session end")
	}
}
