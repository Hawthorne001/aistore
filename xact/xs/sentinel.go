// Package xact provides core functionality for the AIStore eXtended Actions (xactions).
/*
 * Copyright (c) 2021-2025, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
)

// sentinel values
const (
	opDone = iota + 27182
	opAbort
	opRequest
	opResponse
)

const apairDeleted int64 = -1

type (
	apair struct {
		last     atomic.Int64 // last progress update
		progress atomic.Int64 // num visited objects
	}
	sentinel struct {
		r    core.Xact
		pend struct {
			m map[string]*apair // map [tid => apair]
			p []string          // reusable slice [tid]
			i atomic.Int64      // periodic log & progress
			n atomic.Int64      // current num running (<= `nat`)
		}
		nat int
	}
)

func (s *sentinel) init(r core.Xact, smap *meta.Smap, nat int) {
	s.r = r
	s.nat = nat
	s.pend.n.Store(int64(nat - 1))
	s.pend.m = make(map[string]*apair, nat-1)
	for tid := range smap.Tmap {
		if tid == core.T.SID() || smap.InMaintOrDecomm(tid) {
			continue
		}
		s.pend.m[tid] = &apair{}
	}
	debug.Assert(nat > 1)
}

func (s *sentinel) cleanup() {
	clear(s.pend.m)
	s.pend.p = s.pend.p[:0]
}

func (s *sentinel) bcast(uuid string, dm *bundle.DM, abortErr error) {
	o := transport.AllocSend()
	o.Hdr.Opcode = opDone
	if uuid != "" {
		o.Hdr.Opaque = cos.UnsafeB(uuid)
	}
	if abortErr != nil {
		if isErrRecvAbort(abortErr) {
			return // do nothing
		}
		o.Hdr.Opcode = opAbort
		o.Hdr.ObjName = abortErr.Error() // (compare w/ sendTerm)
	}

	err := dm.Bcast(o, nil /*roc*/)

	switch {
	case abortErr != nil:
		nlog.WarningDepth(1, s.r.Name(), "aborted [", abortErr, err, "]")
	case err != nil:
		nlog.WarningDepth(1, s.r.Name(), err)
	default:
		if cmn.Rom.V(4, cos.ModXs) {
			nlog.Infoln(s.r.Name(), "done")
		}
	}
}

func (s *sentinel) initLast(now int64) {
	for tid := range s.pend.m {
		apair := s.pend.m[tid]
		apair.last.CAS(0, now)
	}
}

func (s *sentinel) qcb(dm *bundle.DM, tot, ival, progressTimeout time.Duration, ecnt int) core.QuiRes {
	i := int64(tot / ival)
	if i <= s.pend.i.Load() {
		return core.QuiActive
	}
	s.pend.i.Store(i)

	// 1. log
	s.pending()
	if ecnt > 0 {
		nlog.Warningln(s.r.Name(), "quiescing [", tot, "errs:", ecnt, "pending:", s.pend.p, "]")
	} else {
		nlog.Warningln(s.r.Name(), "quiescing [", tot, s.pend.p, "]")
	}
	if len(s.pend.p) == 0 {
		return core.QuiDone
	}

	// 2. check Smap; abort if membership changed
	smap := core.T.Sowner().Get()
	if err := s.checkSmap(smap, s.pend.p); err != nil {
		return s._qabort(err)
	}

	// 3. check progress timeout
	now := mono.NanoTime()
	for tid := range s.pend.m {
		apair := s.pend.m[tid]
		if last := apair.last.Load(); last != apairDeleted {
			debug.Assert(last != 0)
			if since := time.Duration(now - last); since > progressTimeout {
				err := fmt.Errorf("%s: timed out waiting for %s [ %v, %v, %v ]", s.r.Name(), meta.Tname(tid), since, tot, s.pend.p)
				return s._qabort(err)
			}
		}
	}

	// 4. request progress
	o := transport.AllocSend()
	o.Hdr.Opcode = opRequest

	if err := dm.Bcast(o, nil); err != nil {
		// (is it too harsh?)
		nlog.Warningln(s.r.Name(), err)
		return s._qabort(err)
	}
	return core.QuiActive
}

func (s *sentinel) _qabort(err error) core.QuiRes {
	nlog.ErrorDepth(1, err)
	s.r.Abort(err)
	return core.QuiAborted
}

func (s *sentinel) checkSmap(smap *meta.Smap, pending []string) error {
	if nat := smap.CountActiveTs(); nat != s.nat {
		return cmn.NewErrMembershipChanges(fmt.Sprint(s.r.Name(), smap.String(), nat, s.nat))
	}
	for _, tid := range pending {
		if smap.GetNode(tid) == nil || smap.InMaintOrDecomm(tid) {
			return cmn.NewErrMembershipChanges(fmt.Sprint(s.r.Name(), smap.String(), tid))
		}
	}
	return nil
}

func (s *sentinel) pending() {
	s.pend.p = s.pend.p[:0]
	for tid := range s.pend.m {
		apair := s.pend.m[tid]
		if apair.last.Load() != apairDeleted {
			s.pend.p = append(s.pend.p, tid)
		}
	}
}

//
// receive
//

func (s *sentinel) rxDone(hdr *transport.ObjHdr) {
	if s.r.IsAborted() || s.r.Finished() {
		return
	}
	apair := s.pend.m[hdr.SID]
	if apair == nil { // unlikely
		debug.Assert(false, "missing apair ", hdr.SID)
		return
	}
	if prev := apair.last.Swap(apairDeleted); prev != apairDeleted {
		s.pend.n.Dec()
	}

	if cmn.Rom.V(4, cos.ModXs) {
		nlog.InfoDepth(1, s.r.Name(), "recv 'done' from:", meta.Tname(hdr.SID), s.pend.n.Load())
	}
}

func (s *sentinel) rxAbort(hdr *transport.ObjHdr) {
	r := s.r
	if r.IsAborted() || r.Finished() {
		return
	}
	err := newErrRecvAbort(r, hdr)
	r.Abort(err)
	nlog.WarningDepth(1, err)
}

func (s *sentinel) rxProgress(hdr *transport.ObjHdr) {
	var (
		numvis = int64(binary.BigEndian.Uint64(hdr.Opaque))
		apair  = s.pend.m[hdr.SID]
	)
	if apair == nil { // unlikely
		debug.Assert(false, "missing apair ", hdr.SID)
		return
	}
	if prev := apair.progress.Swap(numvis); prev != numvis {
		// target hdr.SID is making progress
		debug.Assert(prev < numvis)
		apair.last.Store(mono.NanoTime())
	}

	if cmn.Rom.V(5, cos.ModXs) {
		nlog.InfoDepth(1, s.r.Name(), "recv 'progress'", numvis, "from:", meta.Tname(hdr.SID), "pending:", s.pend.n.Load())
	}
}
