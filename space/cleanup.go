// Package space provides storage cleanup and eviction functionality (the latter based on the
// least recently used cache replacement). It also serves as a built-in garbage-collection
// mechanism for orphaned workfiles.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package space

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xact"
	"github.com/NVIDIA/aistore/xact/xreg"
)

// stats counters "cleanup.store.n" & "cleanup.store.size" (not to confuse with generic ""loc-objs", "in-objs", etc.)

const (
	flagRmOldWork = 1 << iota
	flagRmMisplacedLOMs
	flagRmMisplacedEC
	flagRmAll = flagRmOldWork | flagRmMisplacedLOMs | flagRmMisplacedEC
)

type (
	XactCln struct {
		xact.Base
	}
	IniCln struct {
		StatsT  stats.Tracker
		Xaction *XactCln
		WG      *sync.WaitGroup
		Args    *xact.ArgsMsg
	}
)

// private
type (
	// parent (contains mpath joggers)
	clnP struct {
		wg      sync.WaitGroup
		joggers map[string]*clnJ
		ini     IniCln
		cs      struct {
			a fs.CapStatus // initial
			b fs.CapStatus // capacity after removing 'deleted'
			c fs.CapStatus // upon finishing
		}
		jcnt atomic.Int32
	}
	// clnJ represents a single cleanup context and a single /jogger/
	// that traverses and evicts a single given mountpath.
	clnJ struct {
		// runtime
		oldWork   []string
		misplaced struct {
			loms []*core.LOM
			ec   []*core.CT // EC slices and replicas without corresponding metafiles (CT FQN -> Meta FQN)
		}
		bck     cmn.Bck
		now     time.Time
		nvisits int64
		norphan int64
		// init-time
		p       *clnP
		ini     *IniCln
		stopCh  chan struct{}
		joggers map[string]*clnJ
		mi      *fs.Mountpath
		config  *cmn.Config
	}
	clnFactory struct {
		xreg.RenewBase
		xctn *XactCln
	}
)

// interface guard
var (
	_ xreg.Renewable = (*clnFactory)(nil)
	_ core.Xact      = (*XactCln)(nil)
)

func (*XactCln) Run(*sync.WaitGroup) { debug.Assert(false) }

func (r *XactCln) Snap() (snap *core.Snap) {
	snap = &core.Snap{}
	r.ToSnap(snap)

	snap.IdleX = r.IsIdle()
	return
}

////////////////
// clnFactory //
////////////////

func (*clnFactory) New(args xreg.Args, _ *meta.Bck) xreg.Renewable {
	return &clnFactory{RenewBase: xreg.RenewBase{Args: args}}
}

func (p *clnFactory) Start() error {
	p.xctn = &XactCln{}
	ctlmsg := p.Args.Custom.(string)
	p.xctn.InitBase(p.UUID(), apc.ActStoreCleanup, ctlmsg, nil)
	return nil
}

func (*clnFactory) Kind() string     { return apc.ActStoreCleanup }
func (p *clnFactory) Get() core.Xact { return p.xctn }

func (*clnFactory) WhenPrevIsRunning(prevEntry xreg.Renewable) (wpr xreg.WPR, err error) {
	return xreg.WprUse, cmn.NewErrXactUsePrev(prevEntry.Get().String())
}

func RunCleanup(ini *IniCln) fs.CapStatus {
	var (
		xcln    = ini.Xaction
		config  = cmn.GCO.Get()
		avail   = fs.GetAvail()
		num     = len(avail)
		joggers = make(map[string]*clnJ, num)
		parent  = &clnP{joggers: joggers, ini: *ini}
	)
	defer func() {
		if ini.WG != nil {
			ini.WG.Done()
		}
	}()
	if num == 0 {
		xcln.AddErr(cmn.ErrNoMountpaths, 0)
		xcln.Finish()
		return fs.CapStatus{}
	}
	now := time.Now()
	for mpath, mi := range avail {
		joggers[mpath] = &clnJ{
			oldWork: make([]string, 0, 64),
			stopCh:  make(chan struct{}, 1),
			mi:      mi,
			config:  config,
			ini:     &parent.ini,
			p:       parent,
			now:     now,
		}
		joggers[mpath].misplaced.loms = make([]*core.LOM, 0, 64)
		joggers[mpath].misplaced.ec = make([]*core.CT, 0, 64)
	}
	parent.jcnt.Store(int32(len(joggers)))
	providers := apc.Providers.ToSlice()
	for _, j := range joggers {
		parent.wg.Add(1)
		j.joggers = joggers
		go j.jog(providers)
	}

	parent.cs.a = fs.Cap()
	nlog.Infoln(xcln.Name(), "started: ", xcln, parent.cs.a.String())
	if ini.WG != nil {
		ini.WG.Done()
		ini.WG = nil
	}
	parent.wg.Wait()

	for _, j := range joggers {
		j.stop()
	}

	var err, errCap error
	parent.cs.c, err, errCap = fs.CapRefresh(config, nil /*tcdf*/)
	if err != nil {
		xcln.AddErr(err)
	}
	if errCap != nil {
		xcln.AddErr(errCap)
		xcln.Finish()
		nlog.Warningln(xcln.Name(), "finished with cap error:", errCap)
	} else {
		xcln.Finish()
	}
	return parent.cs.c
}

// check other conditions (other than too-early) prior to going ahead to remove misplaced
func (p *clnP) rmMisplaced() bool {
	var (
		g = xreg.GetRebMarked()
		l = xreg.GetResilverMarked()
	)
	if g.Xact == nil && l.Xact == nil && !g.Interrupted && !g.Restarted && !l.Interrupted {
		return true
	}

	// force(?) and log
	var (
		why   string
		flog  = nlog.Warningln
		cserr = p.cs.a.Err()
		ok    bool
	)
	switch {
	case g.Xact != nil:
		why = g.Xact.String() + " is running"
	case g.Interrupted:
		why = "rebalance interrupted"
		ok = p.ini.Args.Force
	case g.Restarted:
		why = "node restarted"
		ok = p.ini.Args.Force
	case l.Xact != nil:
		why = l.Xact.String() + " is running"
	case l.Interrupted:
		why = "resilver interrupted"
	}
	if cserr != nil {
		flog = nlog.Errorln
	}
	xcln := p.ini.Xaction
	if ok {
		flog(core.T.String(), xcln.String(), "proceeding to remove misplaced obj-s with force, ignoring: [", cserr, why, "]")
	} else {
		flog(core.T.String(), xcln.String(), "not removing misplaced obj-s: [", cserr, why, "]")
	}
	return ok
}

//////////
// clnJ //
//////////

func (j *clnJ) String() string {
	var sb strings.Builder
	sb.Grow(128)
	sb.WriteString(j.ini.Xaction.String())
	sb.WriteString(": jog-")
	sb.WriteString(j.mi.String())
	if j.ini.Args.Force {
		sb.WriteString("-with-force")
	}
	return sb.String()
}

func (j *clnJ) stop() { j.stopCh <- struct{}{} }

func (j *clnJ) dont() time.Duration { return j.config.Space.DontCleanupTime.D() }

func (j *clnJ) jog(providers []string) {
	// globally
	j.rmDeleted()

	// traverse
	if len(j.ini.Args.Buckets) != 0 {
		j.jogBcks(j.ini.Args.Buckets)
	} else {
		j.jogProviders(providers)
	}

	j.oldWork = slices.Clip(j.oldWork)
	j.misplaced.loms = slices.Clip(j.misplaced.loms)
	j.misplaced.ec = slices.Clip(j.misplaced.ec)

	j.p.wg.Done()
}

func (j *clnJ) jogProviders(providers []string) {
	xcln := j.ini.Xaction
	for _, provider := range providers { // for each provider (NOTE: ordering is random)
		var (
			bcks []cmn.Bck
			err  error
			opts = fs.WalkOpts{Mi: j.mi, Bck: cmn.Bck{Provider: provider, Ns: cmn.NsGlobal}}
		)
		if bcks, err = fs.AllMpathBcks(&opts); err != nil {
			xcln.AddErr(err, 0)
			continue
		}
		if len(bcks) == 0 {
			continue
		}
		j.jogBcks(bcks)
		if xcln.IsAborted() || j.done() {
			return
		}
	}
}

func (j *clnJ) jogBcks(bcks []cmn.Bck) {
	var (
		xcln   = j.ini.Xaction
		bowner = core.T.Bowner()
	)
	for i := range bcks { // for each bucket under a given provider
		var (
			err error
			bck = bcks[i]
			b   = meta.CloneBck(&bck)
		)
		j.bck = bck
		err = b.Init(bowner)
		if err != nil {
			if cmn.IsErrBckNotFound(err) || cmn.IsErrRemoteBckNotFound(err) {
				const act = "delete non-existing"
				if err = fs.DestroyBucket(act, &bck, 0 /*unknown BID*/); err == nil {
					nlog.Infof("%s: %s %s", j, act, bck.String())
				} else {
					xcln.AddErr(err)
					nlog.Errorf("%s %s: %v - skipping", j, act, err)
				}
			} else {
				// TODO: config option to scrub `fs.AllMpathBcks` buckets
				xcln.AddErr(err)
				nlog.Errorf("%s: %v - skipping %s", j, err, bck.String())
			}
			continue
		}
		j.jogBck()
		if xcln.IsAborted() || j.done() {
			return
		}
	}
}

// main method: walk and visit assorted content types (specified below)
func (j *clnJ) jogBck() {
	xcln := j.ini.Xaction
	opts := &fs.WalkOpts{
		Mi:       j.mi,
		Bck:      j.bck,
		CTs:      []string{fs.WorkCT, fs.ObjCT, fs.ECSliceCT, fs.ECMetaCT, fs.ChunkCT, fs.ChunkMetaCT},
		Callback: j.visit,
		Sorted:   false,
	}
	err := fs.Walk(opts)
	if j.norphan > 0 {
		nlog.Warningln(j.String(), "removed", j.norphan, "orphan chunks")
	}
	if err != nil {
		xcln.AddErr(err)
		return
	}
	j.rmLeftovers(flagRmAll)
}

func (j *clnJ) visit(fqn string, de fs.DirEntry) error {
	xcln := j.ini.Xaction
	if de.IsDir() {
		j.rmEmptyDir(fqn)
		return nil
	}
	if j.done() {
		return nil
	}

	if finfo, err := os.Lstat(fqn); err == nil {
		mtime := finfo.ModTime()
		if mtime.Add(j.dont()).After(j.now) {
			return nil // skipping - too early
		}
	}

	var parsed fs.ParsedFQN
	if _, err := core.ResolveFQN(fqn, &parsed); err != nil {
		xcln.AddErr(err, 0)
		return nil
	}

	debug.Assertf(parsed.Bck.Equal(&j.bck), "unexpected bucket mismatch: %s vs %s", parsed.Bck.String(), j.bck.String())

	if parsed.ContentType != fs.ObjCT {
		j.visitCT(&parsed, fqn)
	} else {
		lom := core.AllocLOM("")
		j.visitObj(fqn, lom)
		core.FreeLOM(lom)
	}

	j.nvisits++
	if fs.IsThrottleWalk(j.nvisits) {
		if pct, _, _ := fs.ThrottlePct(); pct >= fs.MaxThrottlePct {
			time.Sleep(fs.Throttle1ms)
		}
	}

	return nil
}

func (j *clnJ) visitCT(parsedFQN *fs.ParsedFQN, fqn string) {
	switch parsedFQN.ContentType {
	case fs.WorkCT:
		_, ubase := filepath.Split(fqn)
		contentResolver := fs.CSM.Resolver(fs.WorkCT)
		contentInfo := contentResolver.ParseUbase(ubase)
		// workfiles: remove old or do nothing
		if contentInfo.Ok && contentInfo.Old {
			j.oldWork = append(j.oldWork, fqn)
			j.rmAnyBatch(flagRmOldWork)
		}

	case fs.ECSliceCT:
		// EC slices:
		// - EC enabled: remove only slices with missing metafiles
		// - EC disabled: remove all slices
		ct, err := core.NewCTFromFQN(fqn, core.T.Bowner())
		if err != nil || !ct.Bck().Props.EC.Enabled {
			j.oldWork = append(j.oldWork, fqn)
			j.rmAnyBatch(flagRmOldWork)
			return
		}
		if err := ct.LoadSliceFromFS(); err != nil {
			return
		}
		// Saving a CT is not atomic: first it saves CT, then its metafile
		// follows. Ignore just updated CTs to avoid processing incomplete data.
		mtime := time.Unix(0, ct.MtimeUnix())
		if mtime.Add(j.dont()).After(j.now) {
			return
		}
		metaFQN := fs.CSM.Gen(ct, fs.ECMetaCT, "")
		if cos.Stat(metaFQN) != nil {
			j.misplaced.ec = append(j.misplaced.ec, ct)
			j.rmAnyBatch(flagRmMisplacedEC)
		}
	case fs.ECMetaCT:
		// EC metafiles:
		// - EC enabled: remove only without corresponding slice or replica
		// - EC disabled: remove all metafiles
		ct, err := core.NewCTFromFQN(fqn, core.T.Bowner())
		if err != nil || !ct.Bck().Props.EC.Enabled {
			j.oldWork = append(j.oldWork, fqn)
			j.rmAnyBatch(flagRmOldWork)
			return
		}
		// Metafile is saved the last. If there is no corresponding replica or
		// slice, it is safe to remove the stray metafile.
		sliceCT := ct.Clone(fs.ECSliceCT)
		if cos.Stat(sliceCT.FQN()) == nil {
			return
		}

		// TODO --  FIXME: does that work at all?

		objCT := ct.Clone(fs.ObjCT)
		if cos.Stat(objCT.FQN()) == nil {
			return
		}
		j.oldWork = append(j.oldWork, fqn)
		j.rmAnyBatch(flagRmOldWork)

	case fs.ChunkCT:
		contentInfo := fs.CSM.Resolver(fs.ChunkCT).ParseUbase(parsedFQN.ObjName)
		if !contentInfo.Ok {
			j.oldWork = append(j.oldWork, fqn)
			j.rmAnyBatch(flagRmOldWork)
			return
		}
		uploadID := contentInfo.Extras[0]
		lom := core.AllocLOM(contentInfo.Base)
		if j.initCTLOM(lom, fqn) == nil {
			j.visitChunk(fqn, lom, uploadID)
		}
		core.FreeLOM(lom)
	case fs.ChunkMetaCT:
		contentInfo := fs.CSM.Resolver(fs.ChunkMetaCT).ParseUbase(parsedFQN.ObjName)
		if !contentInfo.Ok {
			j.oldWork = append(j.oldWork, fqn)
			j.rmAnyBatch(flagRmOldWork)
			return
		}
		lom := core.AllocLOM(contentInfo.Base)

		// TODO -- FIXME: completed manifests must be handled by visitObj()

		if j.initCTLOM(lom, fqn) == nil {
			if len(contentInfo.Extras) > 0 {
				j.visitPartial(fqn, contentInfo.Extras[0] /*uploadID*/, lom)
			}
		}
		core.FreeLOM(lom)

	default:
		debug.Assert(false, "Unsupported content type: ", parsedFQN.ContentType)
	}
}

func (j *clnJ) initCTLOM(lom *core.LOM, fqn string) error {
	err := lom.InitBck(&j.bck)
	if err == nil {
		return nil
	}
	xcln := j.ini.Xaction
	if cmn.IsErrBckNotFound(err) || cmn.IsErrRemoteBckNotFound(err) {
		nlog.Warningln(j.String(), "bucket gone - aborting:", err)
	} else {
		err = fmt.Errorf("%s: unexpected lom-init fail [ %q => %q, %w ]", j, fqn, lom.ObjName, err)
		nlog.Errorln(err)
	}
	xcln.Abort(err)
	return err
}

func (j *clnJ) visitPartial(fqn, uploadID string, lom *core.LOM) {
	nlog.Warningln(j.String(), "removing old partial manifest:", uploadID, lom.Cname(), fqn)
	j.oldWork = append(j.oldWork, fqn)
	j.rmAnyBatch(flagRmOldWork)
}

const (
	sparseOrphanLogCnt = 100
)

func (j *clnJ) visitChunk(chunkFQN string, lom *core.LOM, uploadID string) {
	lom.Lock(false)
	id := j._getCompletedID(lom)
	lom.Unlock(false)

	if id != "" {
		if id != uploadID {
			if cmn.Rom.FastV(5, cos.SmoduleSpace) {
				nlog.Warningln(j.String(), "chunk ID vs completed manifest ID:", id, uploadID, lom.Cname())
			}
			// have completed manifest, can remove this stray chunk
			j.oldWork = append(j.oldWork, chunkFQN)
			j.rmAnyBatch(flagRmOldWork)
		}
		return
	}

	// partial manifest:
	// - resolve and check if exists;
	// - if it does: check its age and possibly remove the chunk
	fqn := fs.CSM.Gen(lom, fs.ChunkMetaCT, uploadID) // (compare with Ufest._fqns())
	if finfo, err := os.Lstat(fqn); err == nil {
		if finfo.ModTime().Add(j.dont()).After(j.now) {
			return
		}
		nlog.Warningln(j.String(), "removing old partial manifest:", uploadID, lom.Cname(), fqn)
		j.oldWork = append(j.oldWork, chunkFQN)
		j.rmAnyBatch(flagRmOldWork)
	}

	// the chunk appears to be a) orphan and b) old enough (checked above)
	// (sparse log; note total count log above)
	j.norphan++
	if j.norphan%sparseOrphanLogCnt == 1 {
		nlog.Warningln(j.String(), "removing orphan chunk:", uploadID, lom.Cname(), chunkFQN, j.norphan)
	}
	j.oldWork = append(j.oldWork, chunkFQN)
	j.rmAnyBatch(flagRmOldWork)
}

func (j *clnJ) _getCompletedID(lom *core.LOM) (id string) {
	xcln := j.ini.Xaction
	if err := lom.Load(false, true); err != nil {
		return
	}
	if !lom.IsChunked() {
		return
	}

	manifest, err := core.NewUfest("", lom, true /*must-exist*/)
	if err != nil {
		debug.AssertNoErr(err)
		xcln.AddErr(err, 0)
		return
	}
	if err := manifest.LoadCompleted(lom); err != nil {
		e := fmt.Errorf("%s: failed to load completed manifest that must exist: %v", j, err)
		xcln.AddErr(e, 0)
		return
	}
	return manifest.ID()
}

func (j *clnJ) visitObj(fqn string, lom *core.LOM) {
	xcln := j.ini.Xaction
	if err := lom.InitFQN(fqn, &j.bck); err != nil {
		if cmn.IsErrBckNotFound(err) || cmn.IsErrRemoteBckNotFound(err) {
			nlog.Warningln(j.String(), "bucket gone - aborting:", err)
			xcln.Abort(err)
		} else {
			nlog.Errorln(j.String(), "unexpected object fqn", fqn, err)
		}
		return
	}
	// handle load err
	if errLoad := lom.Load(false /*cache it*/, false /*locked*/); errLoad != nil {
		if cmn.IsErrLmetaCorrupted(errLoad) {
			if err := lom.RemoveMain(); err != nil {
				e := fmt.Errorf("%s rm MD-corrupted %s: %v (nested: %v)", j, lom, errLoad, err)
				xcln.AddErr(e, 0)
			} else {
				nlog.Errorf("%s: removed MD-corrupted %s: %v", j, lom, errLoad)
			}
		} else if cmn.IsErrLmetaNotFound(errLoad) {
			if err := lom.RemoveMain(); err != nil {
				e := fmt.Errorf("%s rm no-MD %s: %v (nested: %v)", j, lom, errLoad, err)
				xcln.AddErr(e, 0)
			} else {
				nlog.Errorf("%s: removed no-MD %s: %v", j, lom, errLoad)
			}
		}
		return
	}

	// TODO: switch
	// too early; NOTE: default dont-cleanup = 2h
	atime := lom.Atime()
	if atime.Add(j.dont()).After(j.now) {
		if cmn.Rom.FastV(5, cos.SmoduleSpace) {
			nlog.Infoln("too early for", lom.String(), "atime", lom.Atime().String(), "dont-cleanup", j.dont())
		}
		return
	}
	if lom.IsHRW() {
		if lom.HasCopies() {
			j.rmExtraCopies(lom)
		}
		if lom.Lsize() == 0 {
			if j.ini.Args.Flags&xact.XrmZeroSize == xact.XrmZeroSize {
				// remove in place
				if ecode, err := core.T.DeleteObject(lom, false /*evict*/); err != nil {
					nlog.Errorln("failed to remove zero size", lom.Cname(), "err: [", err, ecode, "]")
				} else {
					if lom.Bck().IsRemote() {
						nlog.Warningln("removed zero size", lom.Cname(), "(both cluster and remote)")
					} else {
						nlog.Warningln("removed zero size", lom.Cname())
					}
					j.ini.StatsT.Inc(stats.CleanupStoreCount)
				}
			}
		}
		return
	}
	if lom.IsCopy() {
		// will be _visited_ separately (if not already)
		return
	}
	if lom.ECEnabled() {
		// misplaced EC
		metaFQN := fs.CSM.Gen(lom, fs.ECMetaCT, "")
		if cos.Stat(metaFQN) != nil {
			j.misplaced.ec = append(j.misplaced.ec, core.LOM2CT(lom, fs.ObjCT))
			j.rmAnyBatch(flagRmMisplacedEC)
		}
	} else {
		// misplaced object
		lom = lom.Clone()
		j.misplaced.loms = append(j.misplaced.loms, lom)
		j.rmAnyBatch(flagRmMisplacedLOMs)
	}
}

//
// removals --------------------------------------------
//

func (j *clnJ) rmAnyBatch(specifier int) {
	batch := j.config.Space.BatchSize
	debug.Assert(batch >= cmn.BatchSizeMin)

	switch specifier {
	case flagRmOldWork:
		if int64(len(j.oldWork)) < batch {
			return
		}
	case flagRmMisplacedLOMs:
		if int64(len(j.misplaced.loms)) < batch {
			return
		}
	case flagRmMisplacedEC:
		if int64(len(j.misplaced.ec)) < batch {
			return
		}
	default:
		debug.Assert(false, "invalid rm-batch specifier: ", specifier)
		return
	}
	j.rmLeftovers(specifier)
}

func (j *clnJ) rmDeleted() {
	xcln := j.ini.Xaction
	err := j.mi.RemoveDeleted(j.String())
	if err != nil {
		xcln.AddErr(err)
	}
	if cnt := j.p.jcnt.Dec(); cnt > 0 {
		return
	}

	// last rm-deleted done: refresh cap now
	var errCap error
	j.p.cs.b, err, errCap = fs.CapRefresh(j.config, nil /*tcdf*/)
	if err != nil {
		xcln.Abort(err)
		return
	}
	if errCap != nil {
		nlog.Warningln(xcln.Name(), "post-rm('deleted'):", errCap)
	}
}

func (j *clnJ) rmExtraCopies(lom *core.LOM) {
	xcln := j.ini.Xaction
	if !lom.TryLock(true) {
		return // must be busy
	}
	defer lom.Unlock(true)

	// reload under lock and check atime - again
	if err := lom.Load(false /*cache it*/, true /*locked*/); err != nil {
		if !cos.IsNotExist(err) {
			xcln.AddErr(err)
		}
		return
	}
	atime := lom.Atime()
	if atime.Add(j.dont()).After(j.now) {
		return
	}
	if lom.IsCopy() {
		return // extremely unlikely but ok
	}
	if _, err := lom.DelExtraCopies(); err != nil {
		e := fmt.Errorf("%s: failed delete redundant copies of %s: %v", j, lom, err)
		xcln.AddErr(e, 5, cos.SmoduleSpace)
	}
}

func (j *clnJ) rmEmptyDir(fqn string) {
	var (
		xcln = j.ini.Xaction
		base = filepath.Base(fqn)
	)
	if fs.LikelyCT(base) {
		return
	}
	if len(fqn) < len(base)+len(j.bck.Name)+8 {
		return
	}
	if !fs.ContainsCT(fqn[0:len(fqn)-len(base)], j.bck.Name) {
		return
	}

	fh, err := os.Open(fqn)
	if err != nil {
		xcln.AddErr(fmt.Errorf("check-empty-dir: open %q: %v", fqn, err))
		core.T.FSHC(err, j.mi, "")
		return
	}
	names, ern := fh.Readdirnames(1)
	cos.Close(fh)

	switch ern {
	case nil:
		// do nothing
	case io.EOF:
		// note: removing a child may render its parent empty as well, but we do not recurs
		debug.Assert(len(names) == 0, names)
		err := syscall.Rmdir(fqn)
		debug.AssertNoErr(err)
		if cmn.Rom.FastV(4, cos.SmoduleSpace) {
			nlog.Infoln(j.String(), "rm empty dir:", fqn)
		}
	default:
		nlog.Warningf("%s read dir %q: %v", j, fqn, ern)
	}
}

func (j *clnJ) rmLeftovers(specifier int) {
	var (
		nfiles, nbytes int64
		n              int64
		xcln           = j.ini.Xaction
	)
	if cmn.Rom.FastV(4, cos.SmoduleSpace) {
		nlog.Infof("%s: num-old %d, misplaced (%d, ec=%d)", j, len(j.oldWork), len(j.misplaced.loms), len(j.misplaced.ec))
	}

	// 1. rm older work
	if specifier&flagRmOldWork != 0 {
		for _, workfqn := range j.oldWork {
			finfo, erw := os.Lstat(workfqn)
			if erw == nil {
				if err := cos.RemoveFile(workfqn); err != nil {
					e := fmt.Errorf("%s: rm old work %q: %v", j, workfqn, err)
					xcln.AddErr(e)
				} else {
					nfiles++
					nbytes += finfo.Size()
					if cmn.Rom.FastV(5, cos.SmoduleSpace) {
						nlog.Infof("%s: rm old work %q, size=%d", j, workfqn, finfo.Size())
					}
				}
			}
		}
		j.oldWork = j.oldWork[:0]
		j.now = time.Now()
	}

	// 2. rm misplaced
	if specifier&flagRmMisplacedLOMs != 0 {
		if len(j.misplaced.loms) > 0 && j.p.rmMisplaced() {
			for _, mlom := range j.misplaced.loms {
				var (
					err     error
					fqn     = mlom.FQN
					removed bool
				)
				lom := core.AllocLOM(mlom.ObjName)
				switch {
				case lom.InitBck(&j.bck) != nil:
					err = os.Remove(fqn)
					removed = err == nil
				case lom.FromFS() != nil:
					err = os.Remove(fqn)
					removed = err == nil
				default:
					removed, err = lom.DelExtraCopies(fqn)
				}
				if err != nil {
					e := fmt.Errorf("%s rm misplaced %q: %v", j, lom.String(), err)
					xcln.AddErr(e)
				}
				core.FreeLOM(lom)

				if removed {
					nfiles++
					nbytes += mlom.Lsize(true /*not loaded*/)
					if cmn.Rom.FastV(4, cos.SmoduleSpace) {
						nlog.Infof("%s: rm misplaced %q, size=%d", j, mlom, mlom.Lsize(true /*not loaded*/))
					}

					// throttle
					n++
					if fs.IsThrottleDflt(n) {
						if pct, _, _ := fs.ThrottlePct(); pct >= fs.MaxThrottlePct {
							time.Sleep(fs.Throttle10ms)
						}
					}

					if j.done() {
						return
					}
				}
			}
		}
		j.misplaced.loms = j.misplaced.loms[:0]
		j.now = time.Now()
	}

	// 3. rm EC slices and replicas that are still without corresponding metafile
	if specifier&flagRmMisplacedEC != 0 {
		for _, ct := range j.misplaced.ec {
			metaFQN := fs.CSM.Gen(ct, fs.ECMetaCT, "")
			if cos.Stat(metaFQN) == nil {
				continue
			}
			if os.Remove(ct.FQN()) == nil {
				nfiles++
				nbytes += ct.Lsize()

				// throttle
				n++
				if fs.IsThrottleDflt(n) {
					if pct, _, _ := fs.ThrottlePct(); pct >= fs.MaxThrottlePct {
						time.Sleep(fs.Throttle10ms)
					}
				}

				if j.done() {
					return
				}
			}
		}
		j.misplaced.ec = j.misplaced.ec[:0]
		j.now = time.Now()
	}

	j.ini.StatsT.Add(stats.CleanupStoreSize, nbytes)
	j.ini.StatsT.Add(stats.CleanupStoreCount, nfiles)
	xcln.ObjsAdd(int(nfiles), nbytes)
}

func (j *clnJ) done() bool {
	xcln := j.ini.Xaction
	select {
	case <-xcln.ChanAbort():
		return true
	case <-j.stopCh:
		return true
	default:
		break
	}
	return xcln.Finished()
}
