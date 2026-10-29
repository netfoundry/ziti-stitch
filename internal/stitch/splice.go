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
	"sync"
	"sync/atomic"
	"time"
)

const copyBufferSize = 32 * 1024

var copyBuffers = sync.Pool{New: func() any { b := make([]byte, copyBufferSize); return &b }}

// errIdle reports a connection closed because nothing moved either way
// for the stitch's idle timeout.
var errIdle = errors.New("idle timeout")

// errClosed ends a direction whose source connection was closed outright,
// which ends the other direction too; it is not a failure.
var errClosed = errors.New("connection closed")

// halfConn is a connection whose sending half can be closed on its own.
// Ziti edge connections and *net.TCPConn both qualify.
type halfConn interface {
	net.Conn
	CloseWrite() error
}

// splice copies between a and b in both directions until both are done,
// counting bytes into toB and toA as they move, and returns the first
// error.
//
// When one side finishes sending (EOF), the other side's sending half is
// closed so its far end sees EOF too, while the opposite direction keeps
// going: request/response protocols that half-close work end to end. A
// Ziti connection closed outright rather than half-closed, or lost with
// its router, closes both, as does an error in either direction. With
// idle > 0, so does no traffic in either direction for that long. Both
// connections are closed when splice returns.
func splice(a, b halfConn, idle time.Duration, toB, toA *atomic.Int64) (err error) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	defer closeBoth()

	act := &activity{start: time.Now()}
	var idled atomic.Bool

	done := make(chan struct{})
	if idle > 0 {
		go func() {
			t := time.NewTicker(idleCheckInterval(idle))
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if act.idleFor() >= idle {
						idled.Store(true)
						closeBoth()
						return
					}
				}
			}
		}()
	}

	errs := make(chan error, 2)
	go func() { errs <- copyHalf(b, a, toB, act) }()
	go func() { errs <- copyHalf(a, b, toA, act) }()
	// Only the first error says why: closing both makes the other
	// direction fail too.
	var first error
	for range 2 {
		if e := <-errs; e != nil {
			if first == nil {
				first = e
			}
			closeBoth() // unblocks the other direction
		}
	}
	close(done)
	if !errors.Is(first, errClosed) {
		err = first
	}

	if idled.Load() {
		err = errIdle
	}
	return err
}

// activity is when bytes last moved, kept as the time since start so that
// it is measured on the monotonic clock: a jump in the wall clock (NTP
// correcting a resumed VM, say) cannot make every connection look idle.
type activity struct {
	start time.Time
	last  atomic.Int64 // time.Since(start) when bytes last moved
}

func (a *activity) touch() { a.last.Store(int64(time.Since(a.start))) }

// idleFor is how long nothing has moved.
func (a *activity) idleFor() time.Duration {
	return time.Since(a.start) - time.Duration(a.last.Load())
}

// copyHalf copies src to dst until src ends, then closes dst's sending
// half, or returns errClosed if src was closed outright. n and act are
// updated as bytes move.
func copyHalf(dst, src halfConn, n *atomic.Int64, act *activity) error {
	bp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bp)
	buf := *bp
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			act.touch()
			nw, werr := dst.Write(buf[:nr])
			n.Add(int64(nw))
			if werr != nil {
				return werr
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if errors.Is(rerr, io.EOF) {
			// A Ziti connection reads EOF for a FIN, and also once it is
			// closed: its far end closed it rather than half-closing, or
			// its router went away. Nothing can reach it then, so a
			// half-close would only leave dst waiting on a silent peer.
			if c, ok := src.(interface{ IsClosed() bool }); ok && c.IsClosed() {
				return errClosed
			}
			return dst.CloseWrite()
		}
		if rerr != nil {
			return rerr
		}
	}
}

// idleCheckInterval is a quarter of the timeout, kept between 10ms and 30s:
// an idle connection closes at most that late, and a long timeout does not
// cost every connection a frequent wakeup.
func idleCheckInterval(idle time.Duration) time.Duration {
	return min(max(idle/4, 10*time.Millisecond), 30*time.Second)
}
