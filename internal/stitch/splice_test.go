/*
	Copyright 2026 NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package stitch

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		c, err := ln.AcceptTCP()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	client, err = net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

type spliceResult struct {
	aToB, bToA int64
	err        error
}

func startSplice(a, b halfConn, idle time.Duration) <-chan spliceResult {
	done := make(chan spliceResult, 1)
	go func() {
		aToB, bToA, err := splice(a, b, idle)
		done <- spliceResult{aToB, bToA, err}
	}()
	return done
}

func waitSplice(t *testing.T, done <-chan spliceResult) spliceResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("splice did not finish")
		return spliceResult{}
	}
}

// A request/response exchange where each side half-closes after sending:
// each direction must deliver its bytes and then EOF, independently.
func TestSpliceHalfCloseBothWays(t *testing.T) {
	ingressClient, a := tcpPair(t)
	b, egressServer := tcpPair(t)
	done := startSplice(a, b, 0)

	if _, err := ingressClient.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := ingressClient.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(egressServer)
	if err != nil || string(got) != "request" {
		t.Fatalf("egress read %q, %v", got, err)
	}

	// The egress side can still answer after the ingress side half-closed.
	if _, err := egressServer.Write([]byte("response")); err != nil {
		t.Fatal(err)
	}
	if err := egressServer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(ingressClient)
	if err != nil || string(got) != "response" {
		t.Fatalf("ingress read %q, %v", got, err)
	}

	r := waitSplice(t, done)
	if r.err != nil || r.aToB != 7 || r.bToA != 8 {
		t.Errorf("splice = %d, %d, %v; want 7, 8, nil", r.aToB, r.bToA, r.err)
	}
}

func TestSpliceCountsLargeTransfers(t *testing.T) {
	ingressClient, a := tcpPair(t)
	b, egressServer := tcpPair(t)
	done := startSplice(a, b, 0)

	payload := make([]byte, 5*copyBufferSize+123)
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		_, _ = ingressClient.Write(payload)
		_ = ingressClient.CloseWrite()
	}()
	got, err := io.ReadAll(egressServer)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("egress read %d bytes, %v", len(got), err)
	}
	_ = egressServer.CloseWrite()

	r := waitSplice(t, done)
	if r.aToB != int64(len(payload)) || r.bToA != 0 {
		t.Errorf("counts = %d, %d", r.aToB, r.bToA)
	}
}

func TestSpliceIdleTimeoutClosesBoth(t *testing.T) {
	ingressClient, a := tcpPair(t)
	b, egressServer := tcpPair(t)
	start := time.Now()
	done := startSplice(a, b, 50*time.Millisecond)

	r := waitSplice(t, done)
	if !errors.Is(r.err, errIdle) {
		t.Fatalf("err = %v, want errIdle", r.err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to notice an idle connection", elapsed)
	}
	// Both far ends see the connection end.
	for _, c := range []net.Conn{ingressClient, egressServer} {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Error("far end still open")
		}
	}
}

func TestSpliceTrafficKeepsIdleConnectionOpen(t *testing.T) {
	ingressClient, a := tcpPair(t)
	b, egressServer := tcpPair(t)
	done := startSplice(a, b, 200*time.Millisecond)

	buf := make([]byte, 1)
	for range 6 { // 6 x 80ms = 480ms, well past the timeout in total
		time.Sleep(80 * time.Millisecond)
		if _, err := ingressClient.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(egressServer, buf); err != nil {
			t.Fatalf("connection closed despite traffic: %v", err)
		}
	}
	_ = ingressClient.CloseWrite()
	_ = egressServer.CloseWrite()
	if r := waitSplice(t, done); r.err != nil {
		t.Errorf("err = %v", r.err)
	}
}

func TestSpliceErrorClosesBoth(t *testing.T) {
	ingressClient, a := tcpPair(t)
	b, egressServer := tcpPair(t)
	done := startSplice(a, b, 0)

	// An abortive close (RST) on the ingress side is an error, not an EOF.
	_ = ingressClient.SetLinger(0)
	_ = ingressClient.Close()

	_ = egressServer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(egressServer); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("egress side was not closed")
		}
	}
	waitSplice(t, done)
}

func TestIdleCheckInterval(t *testing.T) {
	tests := map[time.Duration]time.Duration{
		20 * time.Millisecond: 10 * time.Millisecond,
		time.Second:           250 * time.Millisecond,
		time.Hour:             30 * time.Second,
	}
	for idle, want := range tests {
		if got := idleCheckInterval(idle); got != want {
			t.Errorf("idleCheckInterval(%v) = %v, want %v", idle, got, want)
		}
	}
}
