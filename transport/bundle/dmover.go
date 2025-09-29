// Package bundle provides multi-streaming transport with the functionality
// to dynamically (un)register receive endpoints, establish long-lived flows, and more.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package bundle

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/transport"
)

// Data Mover (DM): a _bundle_ of streams (this target => other target) with atomic reg, open, and broadcast help

type (
	bp struct {
		client  transport.Client
		recv    transport.RecvObj
		streams *Streams
		trname  string
		net     string // one of cmn.KnownNetworks, empty defaults to cmn.NetIntraData
	}
	DM struct {
		data        bp // data
		ack         bp // ACKs and control
		xctn        core.Xact
		config      *cmn.Config
		compression string // enum { apc.CompressNever, ... }
		multiplier  int
		owt         cmn.OWT
		stage       struct {
			regmtx sync.Mutex
			regged atomic.Bool
			opened atomic.Bool
			laterx atomic.Bool
		}
		sizePDU    int32
		maxHdrSize int32
	}
	// additional (and optional) params for new data mover instance
	Extra struct {
		RecvAck     transport.RecvObj
		Config      *cmn.Config
		Compression string
		Multiplier  int
		SizePDU     int32
		MaxHdrSize  int32
	}
)

// In re `owt` (below): data mover passes it to the target's `PutObject`
// to properly finalize received payload.
// For DMs that do not create new objects (e.g, rebalance) `owt` should
// be set to `OwtMigrateRepl`; all others are expected to have `OwtPut` (see e.g, CopyBucket).

func NewDM(trname string, recvCB transport.RecvObj, owt cmn.OWT, extra Extra) *DM {
	debug.Assert(extra.Config != nil)
	dm := &DM{config: extra.Config}
	dm.init(trname, recvCB, owt, extra)
	return dm
}

func (dm *DM) init(trname string, recvCB transport.RecvObj, owt cmn.OWT, extra Extra) {
	dm.owt = owt
	dm.multiplier = extra.Multiplier
	dm.sizePDU, dm.maxHdrSize = extra.SizePDU, extra.MaxHdrSize

	if extra.Compression == "" {
		extra.Compression = apc.CompressNever
	}
	dm.compression = extra.Compression

	dm.data.trname, dm.data.recv = trname, recvCB
	if dm.data.net == "" {
		dm.data.net = cmn.NetIntraData
	}
	dm.data.client = transport.NewIntraDataClient()
	// ack
	if dm.ack.net == "" {
		dm.ack.net = cmn.NetIntraControl
	}
	dm.ack.recv = extra.RecvAck
	if !dm.useACKs() {
		return
	}
	dm.ack.trname = "ack." + trname
	dm.ack.client = transport.NewIntraDataClient()
}

func (dm *DM) useACKs() bool { return dm.ack.recv != nil }
func (dm *DM) NetD() string  { return dm.data.net }
func (dm *DM) NetC() string  { return dm.ack.net }
func (dm *DM) OWT() cmn.OWT  { return dm.owt }

// xaction that drives and utilizes this data mover
func (dm *DM) SetXact(xctn core.Xact) { dm.xctn = xctn }
func (dm *DM) GetXact() core.Xact     { return dm.xctn }

// when config changes
func (dm *DM) Renew(trname string, recvCB transport.RecvObj, owt cmn.OWT, extra Extra) *DM {
	dm.config = extra.Config // always refresh
	if extra.Compression == "" {
		extra.Compression = apc.CompressNever
	}
	debug.Assert(owt == dm.owt)
	if dm.multiplier == extra.Multiplier && dm.compression == extra.Compression && dm.sizePDU == extra.SizePDU && dm.maxHdrSize == extra.MaxHdrSize {
		return nil
	}
	nlog.Infoln("renew DM", dm.String(), "=> [", extra.Compression, extra.Multiplier, "]")
	return NewDM(trname, recvCB, owt, extra)
}

// register user's receive-data (and, optionally, receive-ack) wrappers
func (dm *DM) RegRecv() error {
	dm.stage.regmtx.Lock()
	defer dm.stage.regmtx.Unlock()

	if dm.stage.regged.Load() {
		return errors.New("duplicated reg: " + dm.String())
	}
	if err := transport.Handle(dm.data.trname, dm.wrapRecvData); err != nil {
		debug.Assert(err == nil, dm.String(), ": ", err)
		return err
	}
	if dm.useACKs() {
		if err := transport.Handle(dm.ack.trname, dm.wrapRecvACK); err != nil {
			if nerr := transport.Unhandle(dm.data.trname); nerr != nil {
				nlog.Errorln("FATAL:", err, "[ nested:", nerr, dm.String(), "]")
				debug.AssertNoErr(nerr)
			}
			return err
		}
	}

	dm.stage.regged.Store(true)
	return nil
}

func (dm *DM) UnregRecv() {
	if dm == nil {
		return
	}
	dm.stage.regmtx.Lock()
	defer dm.stage.regmtx.Unlock()

	if !dm.stage.regged.Load() {
		nlog.WarningDepth(1, "duplicated unreg:", dm.String())
		return
	}
	defer dm.stage.regged.Store(false)

	if dm.xctn != nil {
		timeout := dm.config.Transport.QuiesceTime.D()
		if dm.xctn.IsAborted() {
			timeout = time.Second
		}
		dm.Quiesce(timeout)
	}
	if err := transport.Unhandle(dm.data.trname); err != nil {
		nlog.ErrorDepth(1, "FATAL:", err, "[", dm.data.trname, dm.String(), "]")
	}
	if dm.useACKs() {
		if err := transport.Unhandle(dm.ack.trname); err != nil {
			nlog.ErrorDepth(1, "FATAL:", err, "[", dm.ack.trname, dm.String(), "]")
		}
	}
}

func (dm *DM) IsFree() bool {
	return !dm.stage.regged.Load()
}

func (dm *DM) Open() {
	dataArgs := Args{
		Net:    dm.data.net,
		Trname: dm.data.trname,
		Extra: &transport.Extra{
			Compression: dm.compression,
			Config:      dm.config,
			SizePDU:     dm.sizePDU,
			MaxHdrSize:  dm.maxHdrSize,
		},
		Ntype:        core.Targets,
		Multiplier:   dm.multiplier,
		ManualResync: true,
	}
	dataArgs.Extra.Xact = dm.xctn
	dm.data.streams = New(dm.data.client, dataArgs)
	if dm.useACKs() {
		ackArgs := Args{
			Net:          dm.ack.net,
			Trname:       dm.ack.trname,
			Extra:        &transport.Extra{Config: dm.config},
			Ntype:        core.Targets,
			ManualResync: true,
		}
		ackArgs.Extra.Xact = dm.xctn
		dm.ack.streams = New(dm.ack.client, ackArgs)
	}

	dm.stage.opened.Store(true)
	nlog.Infoln(dm.String(), "is open")
}

func (dm *DM) String() string {
	s := "pre-or-post-"
	switch {
	case dm.stage.opened.Load():
		s = "open-"
	case dm.stage.regged.Load():
		s = "reg-" // reg-ed handlers, not open yet tho
	}
	if dm.data.streams == nil {
		return "dm-" + s + "no-streams"
	}
	if dm.data.streams.UsePDU() {
		return "dm-pdu-" + s + dm.data.streams.Trname()
	}
	return "dm-" + s + dm.data.streams.Trname()
}

// quiesce *local* Rx
func (dm *DM) Quiesce(d time.Duration) core.QuiRes {
	return dm.xctn.Quiesce(d, dm.quicb)
}

func (dm *DM) Close(err error) {
	if dm == nil {
		if cmn.Rom.V(5, cos.ModTransport) {
			nlog.Warningln("Warning: DM is <nil>") // e.g., single-node cluster
		}
		return
	}
	if !dm.stage.opened.CAS(true, false) {
		nlog.Errorln("Warning:", dm.String(), "not open")
		return
	}
	if err == nil && dm.xctn != nil && dm.xctn.IsAborted() {
		err = dm.xctn.AbortErr()
	}
	// nil: close gracefully via `fin`, otherwise abort
	dm.data.streams.Close(err == nil)
	if dm.useACKs() {
		dm.ack.streams.Close(err == nil)
	}
	nlog.Infoln(dm.String(), err)
}

func (dm *DM) Abort() {
	dm.data.streams.Abort()
	if dm.useACKs() {
		dm.ack.streams.Abort()
	}
	dm.stage.opened.Store(false)
	nlog.Warningln("dm.abort", dm.String())
}

func (dm *DM) Send(obj *transport.Obj, roc cos.ReadOpenCloser, tsi *meta.Snode, xctns ...core.Xact) (err error) { // TODO -- FIXME: separate
	err = dm.data.streams.Send(obj, roc, tsi)
	if err == nil && !transport.ReservedOpcode(obj.Hdr.Opcode) {
		xctn := dm.xctn
		if len(xctns) > 0 {
			xctn = xctns[0]
		}
		xctn.OutObjsAdd(1, obj.Size())
	}
	return
}

func (dm *DM) ACK(hdr *transport.ObjHdr, cb transport.ObjSentCB, tsi *meta.Snode) error {
	return dm.ack.streams.Send(&transport.Obj{Hdr: *hdr, Callback: cb}, nil, tsi)
}

func (dm *DM) Notif(hdr *transport.ObjHdr) error {
	return dm.ack.streams.Send(&transport.Obj{Hdr: *hdr}, nil)
}

func (dm *DM) Bcast(obj *transport.Obj, roc cos.ReadOpenCloser) error {
	return dm.data.streams.Send(obj, roc)
}

//
// private
//

func (dm *DM) quicb(time.Duration /*total*/) core.QuiRes {
	switch {
	case dm.xctn != nil && dm.xctn.IsAborted():
		return core.QuiInactiveCB
	case dm.stage.laterx.CAS(true, false):
		return core.QuiActive
	default:
		return core.QuiInactiveCB
	}
}

func (dm *DM) wrapRecvData(hdr *transport.ObjHdr, reader io.Reader, err error) error {
	// DEBUG -- TODO -- FIXME
	if dm.data.trname == SDM.trname() {
		return dm.data.recv(hdr, reader, err)
	}

	if hdr.Bck.Name != "" && hdr.ObjName != "" && hdr.ObjAttrs.Size >= 0 {
		dm.xctn.InObjsAdd(1, hdr.ObjAttrs.Size)
	}
	// NOTE: in re (hdr.ObjAttrs.Size < 0) see transport.UsePDU()

	dm.stage.laterx.Store(true)
	return dm.data.recv(hdr, reader, err)
}

func (dm *DM) wrapRecvACK(hdr *transport.ObjHdr, reader io.Reader, err error) error {
	dm.stage.laterx.Store(true)
	return dm.ack.recv(hdr, reader, err)
}
