// Package xreg provides registry and (renew, find) functions for AIS eXtended Actions (xactions).
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package xreg

import (
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/xact"
)

func RegNonBckXact(entry Renewable) {
	debug.Assert(!xact.IsSameScope(entry.Kind(), xact.ScopeB))
	dreg.nonbckXacts[entry.Kind()] = entry // no locking: all reg-s are done at init time
}

func RenewRebalance(id int64, args *RebArgs) RenewRes {
	e := dreg.nonbckXacts[apc.ActRebalance].New(Args{UUID: xact.RebID2S(id), Custom: args}, nil)
	return dreg.renew(e, nil)
}

func RenewResilver(id string, args *ResArgs) core.Xact {
	e := dreg.nonbckXacts[apc.ActResilver].New(Args{UUID: id, Custom: args}, nil)
	rns := dreg.renew(e, nil)
	debug.Assert(!rns.IsRunning()) // NOTE: resilver is always preempted
	return rns.Entry.Get()
}

func RenewElection() RenewRes {
	e := dreg.nonbckXacts[apc.ActElection].New(Args{}, nil)
	return dreg.renew(e, nil)
}

func RenewLRU(id, ctlmsg string) RenewRes {
	e := dreg.nonbckXacts[apc.ActLRU].New(Args{UUID: id, Custom: ctlmsg}, nil)
	return dreg.renew(e, nil)
}

func RenewStoreCleanup(id, ctlmsg string) RenewRes {
	e := dreg.nonbckXacts[apc.ActStoreCleanup].New(Args{UUID: id, Custom: ctlmsg}, nil)
	return dreg.renew(e, nil)
}

func RenewDownloader(xid string, bck *meta.Bck) RenewRes {
	e := dreg.nonbckXacts[apc.ActDownload].New(Args{UUID: xid, Custom: bck}, nil)
	return dreg.renew(e, nil)
}

func RenewETL(msg any, xid string) RenewRes {
	e := dreg.nonbckXacts[apc.ActETLInline].New(Args{UUID: xid, Custom: msg}, nil)
	return dreg.renew(e, nil)
}

func RenewBckSummary(bck *meta.Bck, msg *apc.BsummCtrlMsg) RenewRes {
	e := dreg.nonbckXacts[apc.ActSummaryBck].New(Args{UUID: msg.UUID, Custom: msg}, bck)
	return dreg.renew(e, bck)
}

func RenewGetBatch(bck *meta.Bck, uuid string, designated bool) RenewRes {
	e := dreg.nonbckXacts[apc.ActGetBatch].New(Args{UUID: uuid, Custom: designated}, bck)
	if designated {
		return dreg.renew(e, bck)
	}
	return dreg.renewByID(e, bck)
}
