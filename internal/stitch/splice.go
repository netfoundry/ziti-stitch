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

// halfConn is a connection whose sending half can be closed on its own.
// Ziti edge connections and *net.TCPConn both qualify.
type halfConn interface {
	net.Conn
	CloseWrite() error
}

// splice copies between a and b in both directions until both are done,
// and returns the byte counts and the first error.
//
// When one side finishes sending (EOF), the other side's sending half is
// closed so its far end sees EOF too, while the opposite direction keeps
// going: request/response protocols that half-close work end to end. An
// error in either direction closes both connections. With idle > 0, so
// does no traffic in either direction for that long. Both connections are
// closed when splice returns.
func splice(a, b halfConn, idle time.Duration) (aToB, bToA int64, err error) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	defer closeBoth()

	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
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
				case now := <-t.C:
					if now.Sub(time.Unix(0, lastActive.Load())) >= idle {
						idled.Store(true)
						closeBoth()
						return
					}
				}
			}
		}()
	}

	errs := make(chan error, 2)
	go func() { errs <- copyHalf(b, a, &aToB, &lastActive) }()
	go func() { errs <- copyHalf(a, b, &bToA, &lastActive) }()
	for range 2 {
		if e := <-errs; e != nil {
			if err == nil {
				err = e
			}
			closeBoth() // unblocks the other direction
		}
	}
	close(done)

	if idled.Load() {
		err = errIdle
	}
	return aToB, bToA, err
}

// copyHalf copies src to dst until src ends, then closes dst's sending
// half. n and lastActive are updated as bytes move.
func copyHalf(dst, src halfConn, n *int64, lastActive *atomic.Int64) error {
	bp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bp)
	buf := *bp
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			lastActive.Store(time.Now().UnixNano())
			nw, werr := dst.Write(buf[:nr])
			*n += int64(nw)
			if werr != nil {
				return werr
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if errors.Is(rerr, io.EOF) {
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
