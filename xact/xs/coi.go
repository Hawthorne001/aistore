// Package xs is a collection of eXtended actions (xactions), including multi-object
// operations, list-objects, (cluster) rebalance and (target) resilver, ETL, and more.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"sync"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/ext/etl"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport/bundle"
)

type (
	CoiParams struct {
		Xact   core.Xact
		OAH    cos.OAH // object attributes after applying core.GetROC
		Config *cmn.Config
		BckTo  *meta.Bck
		core.GetROC
		core.PutWOC
		ETLArgs         *core.ETLArgs
		ObjnameTo       string
		Buf             []byte
		OWT             cmn.OWT
		Finalize        bool // copies and EC (as in poi.finalize())
		DryRun          bool // no changes
		LatestVer       bool // can be used without changing bucket's 'versioning.validate_warm_get'; see also: QparamLatestVer
		Sync            bool // see core.GetROC at core/ldp.go
		ContinueOnError bool // when false, a failure to copy triggers abort
	}
	CoiRes struct {
		Err   error
		Lsize int64
		Ecode int
		RGET  bool // when reading source via backend.GetObjReader
	}

	COI interface {
		CopyObject(lom *core.LOM, dm *bundle.DM, coi *CoiParams) CoiRes
	}
)

// target i/f (ais/tgtimpl)
var (
	gcoi COI
)

// mem pool
var (
	coiPool sync.Pool
	coi0    CoiParams
)

//
// CoiParams pool
//

func AllocCOI() (a *CoiParams) {
	if v := coiPool.Get(); v != nil {
		a = v.(*CoiParams)
		return
	}
	return &CoiParams{}
}

func FreeCOI(a *CoiParams) {
	*a = coi0
	coiPool.Put(a)
}

//
// tcb/tcobjs common part (internal)
//

type (
	copier struct {
		r      core.Xact    // root xaction (TCB/TCO)
		xetl   *etl.XactETL // corresponding ETL xaction (if any)
		bp     core.Backend // backend(source bucket)
		getROC core.GetROC
		putWOC core.PutWOC
		rate   tcrate
		vlabs  map[string]string
	}
)

func (tc *copier) prepare(lom *core.LOM, bckTo *meta.Bck, msg *apc.TCBMsg, config *cmn.Config, buf []byte, owt cmn.OWT) (a *CoiParams, err error) {
	toName := msg.ToName(lom.ObjName)
	if cmn.Rom.V(5, cos.ModXs) {
		nlog.Infoln(tc.r.Name(), lom.Cname(), "=>", bckTo.Cname(toName))
	}

	// apply frontend rate-limit, if any
	tc.rate.acquire()

	a = AllocCOI()
	{
		a.GetROC = tc.getROC
		a.PutWOC = tc.putWOC
		a.Xact = tc.r
		a.Config = config
		a.BckTo = bckTo
		a.ObjnameTo = toName
		a.Buf = buf
		a.OWT = owt
		a.DryRun = msg.DryRun
		a.LatestVer = msg.LatestVer
		a.Sync = msg.Sync
		a.Finalize = false
		a.ContinueOnError = msg.ContinueOnError
	}

	if msg.Transform.Pipeline != nil {
		a.ETLArgs = &core.ETLArgs{}
		a.ETLArgs.Pipeline, err = etl.GetPipeline(msg.Transform.Pipeline)
		if err != nil { // unlikely, since the pipeline is already validated in the begin phase of tcb/tcobjs
			FreeCOI(a)
			return a, err
		}
	}

	return a, nil
}

func (tc *copier) do(a *CoiParams, lom *core.LOM, dm *bundle.DM) (err error) {
	started := mono.NanoTime()
	res := gcoi.CopyObject(lom, dm, a)
	contOnErr := a.ContinueOnError
	FreeCOI(a)

	switch {
	case res.Err == nil:
		debug.Assert(res.Lsize != cos.ContentLengthUnknown)
		tc.r.ObjsAdd(1, res.Lsize)

		tstats := core.T.StatsUpdater()
		tstats.IncWith(stats.ETLOfflineCount, tc.vlabs)
		tstats.AddWith(
			cos.NamedVal64{Name: stats.ETLOfflineLatencyTotal, Value: mono.SinceNano(started), VarLabs: tc.vlabs},
			cos.NamedVal64{Name: stats.ETLOfflineSize, Value: res.Lsize, VarLabs: tc.vlabs},
		)

		if res.RGET {
			// RGET stats (compare with ais/tgtimpl namesake)
			debug.Assert(tc.bp != nil)
			rgetstats(tc.bp /*from*/, tc.vlabs, res.Lsize, started)
		}
	case cos.IsNotExist(res.Err, res.Ecode):
		if tc.xetl != nil {
			tc.xetl.AddObjErr(tc.r.ID(), &etl.ObjErr{
				ObjName: lom.Cname(),
				Message: "object not found",
				Ecode:   res.Ecode,
			})
		}
	case res.Err == cmn.ErrSkip:
		// ErrSkip is returned when the object is transmitted through direct put
		tc.r.OutObjsAdd(1, res.Lsize) // TODO -- FIXME: update stats with actual size
	case cos.IsErrOOS(res.Err):
		err = res.Err
		tc.r.Abort(err)
	default:
		if cmn.Rom.V(5, cos.ModXs) {
			nlog.Warningln(tc.r.Name(), lom.Cname(), res.Err)
		}
		if tc.xetl != nil {
			tc.xetl.AddObjErr(tc.r.ID(), &etl.ObjErr{
				ObjName: lom.Cname(),
				Message: res.Err.Error(),
				Ecode:   res.Ecode,
			})
		}
		if contOnErr {
			tc.r.AddErr(res.Err, 5, cos.ModXs)
		} else {
			err = res.Err
			tc.r.Abort(err)
		}
	}

	return err
}
