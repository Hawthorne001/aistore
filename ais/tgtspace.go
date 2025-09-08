// Package ais provides AIStore's proxy and target nodes.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/ios"
	"github.com/NVIDIA/aistore/nl"
	"github.com/NVIDIA/aistore/space"
	"github.com/NVIDIA/aistore/xact"
	"github.com/NVIDIA/aistore/xact/xreg"
)

const (
	// - note that an API call (e.g. CLI) will go through anyway
	// - compare with cmn/cos/oom
	// - compare with fs/health/fshc
	minAutoDetectInterval = 10 * time.Minute
)

var (
	lastTrigOOS atomic.Int64
)

// triggers by an out-of-space condition or a suspicion of thereof

func (t *target) oos(config *cmn.Config) fs.CapStatus {
	debug.Assert(config != nil)
	return t.OOS(nil, config, nil)
}

func (t *target) OOS(csRefreshed *fs.CapStatus, config *cmn.Config, tcdf *fs.Tcdf) (cs fs.CapStatus) {
	var errCap error
	if csRefreshed != nil {
		cs = *csRefreshed
		errCap = cs.Err()
	} else {
		var err error
		cs, err, errCap = fs.CapRefresh(config, tcdf)
		if err != nil {
			nlog.Errorln(t.String(), "failed to update capacity stats:", err) // (unlikely)
			return cs
		}
	}

	//
	// TODO: refactor
	//

	if errCap == nil {
		return cs // unlikely; nothing to do
	}
	if prev := lastTrigOOS.Load(); mono.Since(prev) < minAutoDetectInterval {
		nlog.Warningf("%s: _not_ running store cleanup: (%v, %v), %s", t, prev, minAutoDetectInterval, cs.String())
		return cs
	}

	if cs.IsOOS() {
		t.statsT.SetFlag(cos.NodeAlerts, cos.OOS)
	} else {
		t.statsT.SetFlag(cos.NodeAlerts, cos.LowCapacity)
	}
	nlog.Warningln(t.String(), "running store cleanup:", cs.String())

	//
	// run serially - cleanup first, LRU second (but only if out-of-space persists)
	//
	go func() {
		var xargs xact.ArgsMsg // no bucket, no xid - nothing
		cs2 := t.runSpaceCleanup(&xargs, nil /*wg*/)
		lastTrigOOS.Store(mono.NanoTime())
		if cs2.Err() != nil {
			nlog.Warningln(t.String(), "still out of space, running LRU eviction now:", cs2.String())
			t.runLRU("" /*uuid*/, nil /*wg*/, false)
		}
	}()

	return cs
}

func (t *target) runLRU(id string, wg *sync.WaitGroup, force bool, bcks ...cmn.Bck) {
	var (
		ctlmsg  string
		regToIC = id == ""
	)
	if regToIC {
		id = cos.GenUUID()
	}
	if len(bcks) > 0 {
		ctlmsg = fmt.Sprintf("%v", bcks)
	}
	rns := xreg.RenewLRU(id, ctlmsg)
	if rns.Err != nil || rns.IsRunning() {
		debug.Assert(rns.Err == nil || cmn.IsErrXactUsePrev(rns.Err))
		if wg != nil {
			wg.Done()
		}
		return
	}
	xlru := rns.Entry.Get()
	if regToIC && xlru.ID() == id {
		// pre-existing UUID: notify IC members
		regMsg := xactRegMsg{UUID: id, Kind: apc.ActLRU, Srcs: []string{t.SID()}}
		msg := t.newAmsgActVal(apc.ActRegGlobalXaction, regMsg)
		t.bcastAsyncIC(msg)
	}
	ini := space.IniLRU{
		Xaction:             xlru.(*space.XactLRU),
		StatsT:              t.statsT,
		Buckets:             bcks,
		GetFSUsedPercentage: ios.GetFSUsedPercentage,
		GetFSStats:          ios.GetFSStats,
		WG:                  wg,
		Force:               force,
	}
	xlru.AddNotif(&xact.NotifXact{
		Base: nl.Base{When: core.UponTerm, Dsts: []string{equalIC}, F: t.notifyTerm},
		Xact: xlru,
	})
	space.RunLRU(&ini)
}

func (t *target) runSpaceCleanup(xargs *xact.ArgsMsg, wg *sync.WaitGroup) fs.CapStatus {
	var (
		ctlmsg  string
		regToIC = xargs.ID != ""
	)
	if !regToIC {
		xargs.ID = cos.GenUUID()
	}
	if len(xargs.Buckets) > 0 {
		ctlmsg = fmt.Sprintf("%v", xargs.Buckets)
	}
	rns := xreg.RenewStoreCleanup(xargs.ID, ctlmsg)
	if rns.Err != nil || rns.IsRunning() {
		debug.Assert(rns.Err == nil || cmn.IsErrXactUsePrev(rns.Err))
		if wg != nil {
			wg.Done()
		}
		return fs.CapStatus{}
	}
	xcln := rns.Entry.Get()
	if regToIC && xcln.ID() == xargs.ID {
		// pre-existing UUID: notify IC members
		regMsg := xactRegMsg{UUID: xargs.ID, Kind: apc.ActStoreCleanup, Srcs: []string{t.SID()}}
		msg := t.newAmsgActVal(apc.ActRegGlobalXaction, regMsg)
		t.bcastAsyncIC(msg)
	}
	ini := space.IniCln{
		StatsT:  t.statsT,
		Xaction: xcln.(*space.XactCln),
		WG:      wg,
		Args:    xargs,
	}
	xcln.AddNotif(&xact.NotifXact{
		Base: nl.Base{When: core.UponTerm, Dsts: []string{equalIC}, F: t.notifyTerm},
		Xact: xcln,
	})
	return space.RunCleanup(&ini)
}
