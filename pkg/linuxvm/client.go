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

package linuxvm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Dial opens a stream to the agent through the supervisor's socket. The
// stream's purpose is its header (HeaderTTRPC, "io <token>", ...).
func Dial(socket, header string) (net.Conn, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, header+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	// The VM may have to boot first: seconds with a hypervisor, minutes
	// under emulation.
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Minute))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("the Linux VM did not answer: %w", err)
	}
	line = strings.TrimSpace(line)
	if line != "ok" {
		c.Close()
		msg, _ := strings.CutPrefix(line, "error ")
		return nil, errors.New("the Linux VM: " + msg)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}, nil
	}
	return c, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite half-closes the stream (the agent reads EOF).
func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
