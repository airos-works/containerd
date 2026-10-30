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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// dnsServer is a network's embedded DNS, like Docker's 127.0.0.11: it
// answers the names of the network's containers (service names in Compose)
// and forwards everything else, unparsed, to the upstream servers.
type dnsServer struct {
	nets *networks
	net  *bridgeNet
}

func startDNS(gw net.IP, nets *networks, bn *bridgeNet) *dnsServer {
	d := &dnsServer{nets: nets, net: bn}
	addr := net.JoinHostPort(gw.String(), "53")
	if pc, err := net.ListenPacket("udp4", addr); err == nil {
		go d.serveUDP(pc)
	} else {
		logf("dns on %s: %v", addr, err)
	}
	if l, err := net.Listen("tcp4", addr); err == nil {
		go d.serveTCP(l)
	}
	return d
}

func (d *dnsServer) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := d.answer(q, "udp"); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}()
	}
}

func (d *dnsServer) serveTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				var lb [2]byte
				if _, err := io.ReadFull(c, lb[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(lb[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := d.answer(q, "tcp")
				if resp == nil {
					return
				}
				binary.BigEndian.PutUint16(lb[:], uint16(len(resp)))
				if _, err := c.Write(append(lb[:], resp...)); err != nil {
					return
				}
			}
		}()
	}
}

const (
	typeA    = 1
	typeAAAA = 28
	classIN  = 1
)

// answer handles one query message.
func (d *dnsServer) answer(q []byte, proto string) []byte {
	name, qtype, qend, err := parseQuestion(q)
	if err == nil {
		if ip := d.nets.lookup(d.net, name); ip != nil {
			switch qtype {
			case typeA:
				return reply(q, qend, ip.To4())
			case typeAAAA:
				return reply(q, qend, nil) // the name exists, without IPv6
			}
		}
	}
	return d.forward(q, proto)
}

func (d *dnsServer) forward(q []byte, proto string) []byte {
	for _, server := range d.nets.upstreamServers() {
		c, err := net.DialTimeout(proto, net.JoinHostPort(server, "53"), 2*time.Second)
		if err != nil {
			continue
		}
		_ = c.SetDeadline(time.Now().Add(4 * time.Second))
		var resp []byte
		if proto == "tcp" {
			var lb [2]byte
			binary.BigEndian.PutUint16(lb[:], uint16(len(q)))
			if _, err = c.Write(append(lb[:], q...)); err == nil {
				if _, err = io.ReadFull(c, lb[:]); err == nil {
					resp = make([]byte, binary.BigEndian.Uint16(lb[:]))
					_, err = io.ReadFull(c, resp)
				}
			}
		} else {
			if _, err = c.Write(q); err == nil {
				buf := make([]byte, 4096)
				var n int
				n, err = c.Read(buf)
				resp = buf[:n]
			}
		}
		c.Close()
		if err == nil && len(resp) >= 12 {
			return resp
		}
	}
	return servfail(q)
}

// parseQuestion reads the first question of a query: its name, type, and
// where it ends.
func parseQuestion(q []byte) (string, uint16, int, error) {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) < 1 || q[2]&0x80 != 0 {
		return "", 0, 0, errors.New("not a query")
	}
	var labels []string
	i := 12
	for {
		if i >= len(q) {
			return "", 0, 0, errors.New("short name")
		}
		l := int(q[i])
		i++
		if l == 0 {
			break
		}
		if l&0xc0 != 0 || i+l > len(q) {
			return "", 0, 0, errors.New("bad label")
		}
		labels = append(labels, string(q[i:i+l]))
		i += l
	}
	if i+4 > len(q) {
		return "", 0, 0, errors.New("short question")
	}
	qtype := binary.BigEndian.Uint16(q[i : i+2])
	if binary.BigEndian.Uint16(q[i+2:i+4]) != classIN {
		return "", 0, 0, errors.New("not IN")
	}
	return strings.Join(labels, "."), qtype, i + 4, nil
}

// reply answers the question with one A record (or none, for ip == nil).
func reply(q []byte, qend int, ip net.IP) []byte {
	resp := append([]byte(nil), q[:qend]...)
	resp[2] = 0x84 | (q[2] & 0x01) // response, authoritative, keep RD
	resp[3] = 0x80                 // recursion available, NOERROR
	binary.BigEndian.PutUint16(resp[4:6], 1)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	if ip == nil {
		binary.BigEndian.PutUint16(resp[6:8], 0)
		return resp
	}
	binary.BigEndian.PutUint16(resp[6:8], 1)
	resp = append(resp, 0xc0, 12) // the question's name
	resp = binary.BigEndian.AppendUint16(resp, typeA)
	resp = binary.BigEndian.AppendUint16(resp, classIN)
	resp = binary.BigEndian.AppendUint32(resp, 0) // TTL: names come and go
	resp = binary.BigEndian.AppendUint16(resp, 4)
	return append(resp, ip...)
}

func servfail(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	resp := append([]byte(nil), q...)
	resp[2] = 0x80 | (q[2] & 0x01)
	resp[3] = 0x82 // SERVFAIL
	return resp
}
