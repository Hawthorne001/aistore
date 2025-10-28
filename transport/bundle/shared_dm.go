// Package bundle provides multi-streaming transport with the functionality
// to dynamically (un)register receive endpoints, establish long-lived flows, and more.
/*
 * Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
 */
package bundle

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/xact"
)

const iniSdmCap = 16

// in other words, "oldAge-rxent = cmn.SharedStreamsDflt"
const oldAgeTickCount = int32((cmn.SharedStreamsDflt + hk.Prune2mIval - 1) / hk.Prune2mIval)

// constant (until and if multiple instances)
const SDMName = "shared-dm"

type (
	rxent struct {
		rx    transport.Receiver
		ticks atomic.Int32 // idle tick count: inc every hk.Prune2mIval; reset upon recv() call
	}
	sharedDM struct {
		receivers map[string]*rxent
		dm        DM
		ocmu      sync.Mutex
		rxmu      sync.RWMutex
	}
)

// global
var SDM sharedDM

// called upon target startup
func InitSDM(config *cmn.Config, compression string) {
	debug.Assert(oldAgeTickCount > 1)
	extra := Extra{Config: config, Compression: compression}

	// NOTE:
	// - see bundle.go for Streams.Resync()
	// - and note that cmn/archive/read returns cos.ReadCloseSizer (not Opener)
	debug.Assert(extra.Multiplier == 0 || extra.Multiplier == 1, "cannot have many-to-one connections: cannot reopen archived files")

	SDM.dm.init(SDM.trname(), SDM.recv, cmn.OwtNone, extra)
}

func (*sharedDM) trname() string { return SDMName }

func (sdm *sharedDM) isOpen() bool { return sdm.dm.stage.opened.Load() }

func (sdm *sharedDM) IsActive() (active bool) {
	sdm.rxmu.RLock()
	active = sdm.getActive() != ""
	sdm.rxmu.RUnlock()
	return
}

// is called under rlock or wlock
func (sdm *sharedDM) getActive() string {
	for xid, en := range sdm.receivers {
		if en.ticks.Load() < oldAgeTickCount {
			return xid
		}
	}
	return ""
}

// called on-demand
func (sdm *sharedDM) Open() error {
	if sdm.isOpen() {
		return nil
	}

	sdm.ocmu.Lock()
	if sdm.isOpen() {
		sdm.ocmu.Unlock()
		return nil
	}

	sdm.rxmu.Lock()
	sdm.receivers = make(map[string]*rxent, iniSdmCap)
	sdm.rxmu.Unlock()

	if err := sdm.dm.RegRecv(); err != nil {
		sdm.ocmu.Unlock()
		nlog.ErrorDepth(1, core.T.String(), err)
		debug.AssertNoErr(err)
		return err
	}
	sdm.dm.Open()
	sdm.ocmu.Unlock()

	hk.Reg(sdm.trname()+hk.NameSuffix, sdm.housekeep, hk.Prune2mIval)

	nlog.InfoDepth(1, core.T.String(), "open", sdm.trname())
	return nil
}

func (sdm *sharedDM) housekeep(int64) time.Duration {
	if !sdm.isOpen() {
		return hk.UnregInterval
	}
	sdm.rxmu.RLock()
	for _, en := range sdm.receivers {
		en.ticks.Inc()
	}
	sdm.rxmu.RUnlock()
	return hk.Prune2mIval
}

// nothing running + cmn.SharedStreamsDflt (10m) inactivity
func (sdm *sharedDM) Close() error {
	sdm.ocmu.Lock()
	sdm.rxmu.Lock()

	if xid := sdm.getActive(); xid != "" {
		sdm.rxmu.Unlock()
		sdm.ocmu.Unlock()
		debug.Assert(cos.IsValidUUID(xid), xid)
		return fmt.Errorf("cannot close %s: xid %s is still active (num: %d)", sdm.trname(), xid, len(sdm.receivers))
	}

	sdm.dm.Close(nil)
	sdm.dm.UnregRecv()
	sdm.receivers = nil
	sdm.rxmu.Unlock()

	sdm.ocmu.Unlock()

	nlog.InfoDepth(1, core.T.String(), "close", sdm.trname())
	return nil
}

// demux-level RegRecv (not to confuse with transport level)
func (sdm *sharedDM) RegRecv(rx transport.Receiver) {
	sdm.ocmu.Lock()
	sdm.rxmu.Lock()
	if sdm.isOpen() {
		en := &rxent{rx: rx}
		sdm.receivers[rx.ID()] = en
	}
	sdm.rxmu.Unlock()
	sdm.ocmu.Unlock()
}

func (sdm *sharedDM) UseRecv(rx transport.Receiver) {
	// fast path
	sdm.rxmu.RLock()
	_, ok := sdm.receivers[rx.ID()]
	sdm.rxmu.RUnlock()
	if ok {
		return
	}

	// slow and unlikely
	sdm.RegRecv(rx)
}

// remove demux entry immediately
func (sdm *sharedDM) UnregRecv(xid string) {
	sdm.rxmu.Lock()
	delete(sdm.receivers, xid)
	sdm.rxmu.Unlock()
}

func (sdm *sharedDM) Send(obj *transport.Obj, roc cos.ReadOpenCloser, tsi *meta.Snode, xctn core.Xact) error {
	return sdm.dm.Send(obj, roc, tsi, xctn)
}

func (sdm *sharedDM) Bcast(obj *transport.Obj, roc cos.ReadOpenCloser) error {
	return sdm.dm.Bcast(obj, roc)
}

func (sdm *sharedDM) recv(hdr *transport.ObjHdr, r io.Reader, err error) error {
	if err != nil {
		return err
	}
	xid := hdr.Demux
	if err := xact.CheckValidUUID(xid); err != nil {
		err = fmt.Errorf("%s: %w", sdm.trname(), err)
		return err
	}

	sdm.rxmu.RLock()
	en, ok := sdm.receivers[xid]
	if !ok {
		sdm.rxmu.RUnlock()
		return fmt.Errorf("%s: xid %s not found, dropping recv [oname: %s]", sdm.trname(), xid, hdr.ObjName)
	}
	sdm.rxmu.RUnlock()

	// (unlikely)
	if en.rx.ID() != xid {
		err = fmt.Errorf("%s: xid mismatch [%q vs %q]", sdm.trname(), xid, en.rx.ID())
		debug.AssertNoErr(err)
		return err
	}

	// note: not holding rxmu locked - race vs UnregRecv possible but benign
	if err := en.rx.RecvObj(hdr, r, nil); err != nil {
		return err
	}
	ticks := en.ticks.Swap(0)
	if ticks > 0 && cmn.Rom.V(4, cos.ModXs) {
		nlog.Warningf("%s: xid %s has been idle for >= %v [oname: %s]", sdm.trname(),
			xid, time.Duration(ticks)*hk.Prune2mIval, hdr.ObjName)
	}
	return nil
}
