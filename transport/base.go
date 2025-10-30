// Package transport provides long-lived http/tcp connections for
// intra-cluster communications (see README for details and usage example).
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
)

// stream TCP/HTTP session: inactive <=> active transitions
const (
	inactive = iota
	active
)

// in-send states
const (
	inHdr = iota + 1
	inPDU
	inData
	inEOB
)

const maxInReadRetries = 64 // Tx: lz4 stream read; Rx: partial object header

// termination: reasons
const (
	reasonError   = "error"
	endOfStream   = "end-of-stream"
	reasonStopped = "stopped"

	termErrWait = time.Second
)

type (
	streamer interface {
		compressed() bool
		dryrun()
		terminate(error, string) (string, error)
		doRequest() error
		inSend() bool
		abortPending(error, bool)
		errCmpl(error)
		resetCompression()
		// gc
		closeAndFree()
		drain(err error)
		idleTick()
	}
	streamBase struct {
		streamer streamer
		client   Client // stream's http client
		parent   *Parent
		stopCh   cos.StopCh    // stop/abort stream
		lastCh   cos.StopCh    // end-of-stream
		pdu      *spdu         // PDU buffer
		postCh   chan struct{} // to indicate that workCh has work
		trname   string        // http endpoint: (trname, dstURL, dstID)
		dstURL   string
		dstID    string
		loghdr   string // log prefix
		maxhdr   []byte // transport header buf must be large enough to accommodate max-size for this stream
		header   []byte // object header (slice of the maxhdr with bucket/objName, etc. fields packed/serialized)
		term     struct {
			err    error
			reason string
			mu     sync.Mutex
			done   atomic.Bool
		}
		stats Stats // stream stats (send side - compare with rxStats)
		time  struct {
			idleTeardown time.Duration // idle timeout
			inSend       atomic.Bool   // true upon Send() or Read() - info for Collector to delay cleanup
			ticks        int           // num 1s ticks until idle timeout
			index        int           // heap stuff
		}
		wg       sync.WaitGroup
		sessST   atomic.Int64 // state of the TCP/HTTP session: active (connected) | inactive (disconnected)
		sessID   int64        // stream session ID
		numCur   int64        // gets reset to zero upon each timeout
		sizeCur  int64        // ditto
		chanFull atomic.Int64
	}
)

////////////////
// streamBase //
////////////////

func newBase(client Client, dstURL, dstID string, extra *Extra) (s *streamBase) {
	var (
		sid    string
		u, err = url.Parse(dstURL)
	)
	debug.AssertNoErr(err)

	s = &streamBase{client: client, parent: extra.Parent, dstURL: dstURL, dstID: dstID}

	s.sessID = nextSessionID.Inc()
	s.trname = path.Base(u.Path)

	s.lastCh.Init()
	s.stopCh.Init()
	s.postCh = make(chan struct{}, 1)

	// default overrides
	if extra.Parent != nil && extra.Parent.Xact != nil {
		sid = "-" + extra.Parent.Xact.ID()
	}
	// NOTE: PDU-based traffic - a MUST-have for "unsized" transmissions
	if extra.UsePDU() {
		if extra.SizePDU > maxSizePDU {
			debug.Assert(false)
			extra.SizePDU = maxSizePDU
		}
		buf, _ := g.mm.AllocSize(int64(extra.SizePDU))
		s.pdu = newSendPDU(buf)
	}
	if extra.IdleTeardown > 0 {
		s.time.idleTeardown = extra.IdleTeardown
	} else {
		s.time.idleTeardown = cos.NonZero(extra.Config.Transport.IdleTeardown.D(), dfltIdleTeardown)
	}
	debug.Assert(s.time.idleTeardown >= dfltTick, s.time.idleTeardown, " vs ", dfltTick)
	s.time.ticks = int(s.time.idleTeardown / dfltTick)

	s._loghdr(sid, dstID, extra)

	s.maxhdr, _ = g.mm.AllocSize(_sizeHdr(extra.Config, int64(extra.MaxHdrSize)))

	s.sessST.Store(inactive) // initiate HTTP session upon the first arrival
	return s
}

func (s *streamBase) _loghdr(sid, dstID string, extra *Extra) {
	var (
		sb strings.Builder
		l  = 2 + len(s.trname) + len(sid) + 32 + len(dstID)
	)
	sb.Grow(l)

	sb.WriteString("s-")
	sb.WriteString(s.trname)
	sb.WriteString(sid)
	sb.WriteByte('[')
	sb.WriteString(core.T.SID()) // (consider adding back session ID, as in: [node-ID/110])

	extra.Lid(&sb) // + compressed

	sb.WriteString("]=>")
	sb.WriteString(dstID)

	s.loghdr = sb.String() // looks like: s-<trname><sid>[<sender-id>]=><dest-id>
}

// (used on the receive side as well)
func _sizeHdr(config *cmn.Config, size int64) int64 {
	if size != 0 {
		debug.Assert(size <= cmn.MaxTransportHeader, size)
		size = min(size, cmn.MaxTransportHeader)
	} else {
		size = cos.NonZero(int64(config.Transport.MaxHeaderSize), int64(cmn.DfltTransportHeader))
	}
	return size
}

func (s *streamBase) startSend(streamable fmt.Stringer) (err error) {
	s.time.inSend.Store(true) // StreamCollector to postpone cleanups

	if s.IsTerminated() {
		// slow path
		reason, errT := s.TermInfo()
		err = cmn.NewErrStreamTerminated(s.String(), errT, reason, "dropping "+streamable.String())
		nlog.Errorln(err)
		return
	}

	if s.sessST.CAS(inactive, active) {
		s.postCh <- struct{}{}
		if cmn.Rom.V(5, cos.ModTransport) {
			nlog.Infoln(s.String(), "inactive => active")
		}
	}
	return
}

func (s *streamBase) Stop()               { s.stopCh.Close() }
func (s *streamBase) URL() string         { return s.dstURL }
func (s *streamBase) ID() (string, int64) { return s.trname, s.sessID } // usage: test only
func (s *streamBase) String() string      { return s.loghdr }

func (s *streamBase) Abort() { s.Stop() } // (DM =>) SB => s.Abort() sequence (e.g. usage see otherXreb.Abort())

func (s *streamBase) IsTerminated() bool { return s.term.done.Load() }

func (s *streamBase) TermInfo() (reason string, err error) {
	// to account for an unlikely delay between done.CAS() and mu.Lock - see terminate()
	sleep := cos.ProbingFrequency(termErrWait)
	for elapsed := time.Duration(0); elapsed < termErrWait; elapsed += sleep {
		s.term.mu.Lock()
		reason, err = s.term.reason, s.term.err
		s.term.mu.Unlock()
		if reason != "" {
			break
		}
		time.Sleep(sleep)
	}
	return
}

func (s *streamBase) GetStats() (stats Stats) {
	// byte-num transfer stats
	stats.Num.Store(s.stats.Num.Load())
	stats.Offset.Store(s.stats.Offset.Load())
	stats.Size.Store(s.stats.Size.Load())
	stats.CompressedSize.Store(s.stats.CompressedSize.Load())
	return
}

func (s *streamBase) isNextReq() (reason string) {
	for {
		select {
		case <-s.lastCh.Listen():
			if cmn.Rom.V(5, cos.ModTransport) {
				nlog.Infoln(s.String(), "end-of-stream")
			}
			reason = endOfStream
			return
		case <-s.stopCh.Listen():
			if cmn.Rom.V(5, cos.ModTransport) {
				nlog.Infoln(s.String(), "stopped")
			}
			reason = reasonStopped
			return
		case <-s.postCh:
			s.sessST.Store(active)
			if cmn.Rom.V(5, cos.ModTransport) {
				nlog.Infoln(s.String(), "active <- posted")
			}
			return
		}
	}
}

func (s *streamBase) deactivate() (n int, err error) {
	err = io.EOF
	if cmn.Rom.V(5, cos.ModTransport) {
		nlog.Infoln(s.String(), "connection teardown: [", s.numCur, s.stats.Num.Load(), "]")
	}
	return
}

func (s *streamBase) sendLoop(config *cmn.Config, dryrun bool) {
	var (
		err    error
		reason string
		retry  *rtry
	)

	// main loop
	for {
		if s.sessST.Load() == active {
			if dryrun {
				s.streamer.dryrun()
			} else {
				err = s.streamer.doRequest()
			}
			if err == nil {
				if retry != nil {
					retry.oklog()
					retry = nil
				}
			} else {
				// the current send failed - complete right away
				s.streamer.errCmpl(err)

				if !_shouldRetry(err) {
					if cmn.Rom.V(4, cos.ModTransport) {
						nlog.Errorln(s.String(), "not retriable:", err)
					}
					reason = reasonError
					break
				}
				if retry == nil {
					retry = newRtry(config, s.String())
				}
				if retry.timeout(err) {
					reason = reasonError
					break
				}

				retry.sleep(err)
				err = nil
			}
		}
		if reason = s.isNextReq(); reason != "" {
			break
		}
	}

	reason, err = s.streamer.terminate(err, reason)
	s.wg.Done()

	if reason == endOfStream { // ok (via hdr.Opcode = opcFin => lastCh.Close)
		return
	}

	// termination is caused by anything other than Fin()
	// (reasonStopped is, effectively, abort via Stop() - totally legit)
	var errExt error
	if reason != reasonStopped {
		errExt = fmt.Errorf("%s[term-reason: %s, err: %w]", s, reason, err)
		nlog.Errorln(errExt)

		// NOTE: aborting grandparent xaction
		if s.parent != nil && s.parent.Xact != nil {
			s.parent.Xact.Abort(errExt)
		}
	}

	// wait for the SCQ/cmplCh to empty
	s.wg.Wait()

	// cleanup
	s.streamer.abortPending(err, false /*completions*/)

	// notify parent if defined
	if reason != reasonStopped {
		if s.parent != nil && s.parent.TermedCB != nil {
			s.parent.TermedCB(s.dstID, errExt)
		}
	}

	// count and log chanFull
	if cnt := s.chanFull.Load(); cnt > 0 {
		if (cnt >= 10 && cnt <= 20) || cmn.Rom.V(4, cos.ModTransport) {
			nlog.Errorln(s.String(), cos.ErrWorkChanFull, "cnt:", cnt)
		}
	}
}

// only for timeouts on *in-flight writes*
func _shouldRetry(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ETIMEDOUT)
}

func (s *streamBase) yelp(err error) {
	nlog.WarningDepth(1, "Error:", s.String(), "[", err, "]")
}

///////////
// Extra //
///////////

func (extra *Extra) UsePDU() bool { return extra.SizePDU > 0 }

func (extra *Extra) Compressed() bool {
	return extra.Compression != "" && extra.Compression != apc.CompressNever
}

func (extra *Extra) Lid(sb *strings.Builder) {
	if extra.Compressed() {
		sb.WriteByte('[')
		sb.WriteString(extra.Config.Transport.LZ4BlockMaxSize.String())
		sb.WriteByte(']')
	}
}

//
// misc
//

func dryrun() (dryrun bool) {
	var err error
	if a := os.Getenv("AIS_STREAM_DRY_RUN"); a != "" {
		if dryrun, err = strconv.ParseBool(a); err != nil {
			nlog.Errorln(err)
		}
	}
	return
}

//////////
// rtry //
//////////

type rtry struct {
	config   *cmn.Config
	sname    string
	total    time.Duration
	nxtSleep time.Duration
	maxSleep time.Duration
	cnt      int
}

func newRtry(config *cmn.Config, sname string) *rtry {
	ini := cos.ClampDuration(config.Timeout.CplaneOperation.D()/2, 100*time.Millisecond, 2*time.Second)
	return &rtry{
		config:   config,
		sname:    sname,
		nxtSleep: ini,
		maxSleep: cos.ClampDuration(config.Timeout.MaxKeepalive.D(), 2*time.Second, 5*time.Second),
	}
}

func (r *rtry) sleep(err error) {
	r.cnt++
	nlog.WarningDepth(1, "retry", r.sname, "[", err, r.cnt, r.total, "]")
	time.Sleep(r.nxtSleep)
	r.total += r.nxtSleep
	r.nxtSleep = min(r.nxtSleep+r.nxtSleep>>1, r.maxSleep)
	if r.cnt > 1 {
		// poor-man's jitter
		runtime.Gosched()
	}
}

func (r *rtry) timeout(err error) bool {
	if r.total < min(r.maxSleep*3, 10*time.Second) {
		return false
	}
	nlog.ErrorDepth(1, "retry timeout", r.sname, "[", err, r.cnt, r.total, "]")
	return true
}

func (r *rtry) oklog() {
	nlog.InfoDepth(1, "retry success", r.sname, "[", r.cnt, r.total, "]")
}
