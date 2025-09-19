// Package integration_test.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package integration_test

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/feat"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/docker"
	"github.com/NVIDIA/aistore/tools/readers"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/tlog"
	"github.com/NVIDIA/aistore/xact"
)

type Test struct {
	name   string
	method func(*testing.T)
}

type regressionTestData struct {
	bck        cmn.Bck
	renamedBck cmn.Bck
	numBuckets int
	rename     bool
	wait       bool
}

const (
	rootDir        = "/tmp/ais"
	testBucketName = "TESTAISBUCKET"
)

func TestListObjectsLocalGetLocation(t *testing.T) {
	var (
		m = ioContext{
			t:         t,
			num:       1000,
			fileSize:  cos.KiB,
			fixedSize: true,
		}

		targets    = make(map[string]struct{})
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
		smap       = tools.GetClusterMap(t, proxyURL)
	)

	m.initAndSaveState(true /*cleanup*/)
	m.expectTargets(1)

	tools.CreateBucket(t, proxyURL, m.bck, nil, true /*cleanup*/)

	m.puts()

	msg := &apc.LsoMsg{Props: apc.GetPropsLocation}
	lst, err := api.ListObjects(baseParams, m.bck, msg, api.ListArgs{Limit: int64(m.num)})
	tassert.CheckFatal(t, err)

	if len(lst.Entries) != m.num {
		t.Errorf("Expected %d bucket list entries, found %d\n", m.num, len(lst.Entries))
	}

	j := 10
	if len(lst.Entries) >= 200 {
		j = 100
	}
	for i, e := range lst.Entries {
		if e.Location == "" {
			t.Fatalf("[%#v]: location is empty", e)
		}
		tname, _ := core.ParseObjLoc(e.Location)
		tid := meta.N2ID(tname)
		targets[tid] = struct{}{}
		tsi := smap.GetTarget(tid)
		url := tsi.URL(cmn.NetPublic)
		baseParams := tools.BaseAPIParams(url)

		oah, err := api.GetObject(baseParams, m.bck, e.Name, nil)
		tassert.CheckFatal(t, err)
		if uint64(oah.Size()) != m.fileSize {
			t.Errorf("Expected filesize: %d, actual filesize: %d\n", m.fileSize, oah.Size())
		}

		if i%j == 0 {
			if i == 0 {
				tlog.Logln("Modifying config to enforce intra-cluster access, expecting errors...\n")
			}
			tools.SetClusterConfig(t, cos.StrKVs{"features": feat.EnforceIntraClusterAccess.String()})
			t.Cleanup(func() {
				tools.SetClusterConfig(t, cos.StrKVs{"features": "0"})
			})

			_, err = api.GetObject(baseParams, m.bck, e.Name, nil)
			if err == nil {
				tlog.Logln("Warning: expected error, got nil")
			}
			tools.SetClusterConfig(t, cos.StrKVs{"features": "0"})
		}
	}

	if smap.CountActiveTs() != len(targets) { // The objects should have been distributed to all targets
		t.Errorf("Expected %d different target URLs, actual: %d different target URLs",
			smap.CountActiveTs(), len(targets))
	}

	// Ensure no target URLs are returned when the property is not requested
	msg.Props = ""
	lst, err = api.ListObjects(baseParams, m.bck, msg, api.ListArgs{Limit: int64(m.num)})
	tassert.CheckFatal(t, err)

	if len(lst.Entries) != m.num {
		t.Errorf("Expected %d bucket list entries, found %d\n", m.num, len(lst.Entries))
	}

	for _, e := range lst.Entries {
		if e.Location != "" {
			t.Fatalf("[%#v]: location expected to be empty\n", e)
		}
	}
}

func TestListObjectsCloudGetLocation(t *testing.T) {
	var (
		m = ioContext{
			t:        t,
			bck:      cliBck,
			num:      100,
			fileSize: cos.KiB,
		}
		targets    = make(map[string]struct{})
		bck        = cliBck
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
		smap       = tools.GetClusterMap(t, proxyURL)
	)

	tools.CheckSkip(t, &tools.SkipTestArgs{RemoteBck: true, Bck: bck})

	m.initAndSaveState(true /*cleanup*/)
	m.expectTargets(2)

	m.puts()

	listObjectsMsg := &apc.LsoMsg{Props: apc.GetPropsLocation, Flags: apc.LsCached}
	lst, err := api.ListObjects(baseParams, bck, listObjectsMsg, api.ListArgs{})
	tassert.CheckFatal(t, err)

	if len(lst.Entries) < m.num {
		t.Errorf("Bucket %s has %d objects, expected %d", m.bck.String(), len(lst.Entries), m.num)
	}
	j := 10
	if len(lst.Entries) >= 200 {
		j = 100
	}
	for i, e := range lst.Entries {
		if e.Location == "" {
			t.Fatalf("[%#v]: location is empty", e)
		}
		tmp := strings.Split(e.Location, apc.LocationPropSepa)
		tid := meta.N2ID(tmp[0])
		targets[tid] = struct{}{}
		tsi := smap.GetTarget(tid)
		url := tsi.URL(cmn.NetPublic)
		baseParams := tools.BaseAPIParams(url)

		oah, err := api.GetObject(baseParams, bck, e.Name, nil)
		tassert.CheckFatal(t, err)
		if uint64(oah.Size()) != m.fileSize {
			t.Errorf("Expected fileSize: %d, actual fileSize: %d\n", m.fileSize, oah.Size())
		}

		if i%j == 0 {
			if i == 0 {
				tlog.Logln("Modifying config to enforce intra-cluster access, expecting errors...\n")
			}
			tools.SetClusterConfig(t, cos.StrKVs{"features": feat.EnforceIntraClusterAccess.String()})
			_, err = api.GetObject(baseParams, m.bck, e.Name, nil)

			if err == nil {
				tlog.Logln("Warning: expected error, got nil")
			}

			tools.SetClusterConfig(t, cos.StrKVs{"features": "0"})
		}
	}

	// The objects should have been distributed to all targets
	if m.originalTargetCount != len(targets) {
		t.Errorf("Expected %d different target URLs, actual: %d different target URLs", m.originalTargetCount, len(targets))
	}

	// Ensure no target URLs are returned when the property is not requested
	listObjectsMsg.Props = ""
	lst, err = api.ListObjects(baseParams, bck, listObjectsMsg, api.ListArgs{})
	tassert.CheckFatal(t, err)

	if len(lst.Entries) != m.num {
		t.Errorf("Expected %d bucket list entries, found %d\n", m.num, len(lst.Entries))
	}

	for _, e := range lst.Entries {
		if e.Location != "" {
			t.Fatalf("[%#v]: location expected to be empty\n", e)
		}
	}
}

// 1. PUT file
// 2. Corrupt the file
// 3. GET file
func TestGetCorruptFileAfterPut(t *testing.T) {
	var (
		m = ioContext{
			t:        t,
			num:      1,
			fileSize: cos.KiB,
		}

		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
	)

	if docker.IsRunning() {
		t.Skipf("%q requires setting xattrs, doesn't work with docker", t.Name())
	}

	m.init(true /*cleanup*/)
	initMountpaths(t, proxyURL)

	tools.CreateBucket(t, proxyURL, m.bck, nil, true /*cleanup*/)

	m.puts()

	// Test corrupting the file contents.
	objName := m.objNames[0]
	var fqn string
	if m.chunksConf != nil && m.chunksConf.multipart {
		fqns := m.findObjChunksOnDisk(m.bck, objName)
		tassert.Fatalf(t, len(fqns) > 0, "object should have chunks: %s", objName)
		fqn = fqns[0]
	} else {
		fqn = m.findObjOnDisk(m.bck, objName)
	}
	tlog.Logfln("Corrupting object data %q: %s", objName, fqn)
	err := os.WriteFile(fqn, []byte("this file has been corrupted"), cos.PermRWR)
	tassert.CheckFatal(t, err)

	_, err = api.GetObjectWithValidation(baseParams, m.bck, objName, nil)
	tassert.Errorf(t, err != nil, "error is nil, expected error getting corrupted object")
}

func TestRegressionBuckets(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     testBucketName,
			Provider: apc.AIS,
		}
		proxyURL = tools.RandomProxyURL(t)
	)
	tools.CreateBucket(t, proxyURL, bck, nil, true /*cleanup*/)
	doBucketRegressionTest(t, proxyURL, regressionTestData{bck: bck})
}

func TestRenameBucket(t *testing.T) {
	var (
		bck = cmn.Bck{
			Name:     testBucketName,
			Provider: apc.AIS,
		}
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
		renamedBck = cmn.Bck{
			Name:     bck.Name + "_" + cos.GenTie(),
			Provider: apc.AIS,
		}
	)
	for _, wait := range []bool{true, false} {
		t.Run(fmt.Sprintf("wait=%v", wait), func(t *testing.T) {
			tools.CreateBucket(t, proxyURL, bck, nil, true /*cleanup*/)
			t.Cleanup(func() {
				tools.DestroyBucket(t, proxyURL, renamedBck)
			})

			bcks, err := api.ListBuckets(baseParams, cmn.QueryBcks{Provider: bck.Provider}, apc.FltPresent)
			tassert.CheckFatal(t, err)

			regData := regressionTestData{
				bck: bck, renamedBck: renamedBck,
				numBuckets: len(bcks), rename: true, wait: wait,
			}
			doBucketRegressionTest(t, proxyURL, regData)
		})
	}
}

//
// doBucketRe*
//

//nolint:gocritic // ignoring (regressionTestData) hugeParam
func doBucketRegressionTest(t *testing.T, proxyURL string, rtd regressionTestData) {
	var (
		m = ioContext{
			t:        t,
			bck:      rtd.bck,
			num:      2036,
			fileSize: cos.KiB,
		}
		baseParams = tools.BaseAPIParams(proxyURL)

		xid string
		err error
	)

	m.init(true /*cleanup*/)
	m.puts()

	if rtd.rename {
		xid, err = api.RenameBucket(baseParams, rtd.bck, rtd.renamedBck)
		if err != nil && ensurePrevRebalanceIsFinished(baseParams, err) {
			// can retry
			xid, err = api.RenameBucket(baseParams, rtd.bck, rtd.renamedBck)
		}
		tassert.CheckFatal(t, err)

		tlog.Logfln("Renamed %s => %s", rtd.bck.String(), rtd.renamedBck.String())
		if rtd.wait {
			postRenameWaitAndCheck(t, baseParams, rtd, m.num, m.objNames, xid)
		}
		m.bck = rtd.renamedBck
	}

	var getArgs *api.GetArgs
	if !rtd.wait {
		tlog.Logln("Warning: proceeding to GET while rebalance is running ('silence = true')")
		getArgs = &api.GetArgs{Query: url.Values{apc.QparamSilent: []string{"true"}}}
	}
	m.gets(getArgs, false /*with validation*/)

	if !rtd.rename || rtd.wait {
		m.del()
	} else {
		postRenameWaitAndCheck(t, baseParams, rtd, m.num, m.objNames, xid)
		m.del()
	}
}

//nolint:gocritic // ignoring (regressionTestData) hugeParam
func postRenameWaitAndCheck(t *testing.T, baseParams api.BaseParams, rtd regressionTestData, numPuts int, objNames []string, xid string) {
	xargs := xact.ArgsMsg{ID: xid, Kind: apc.ActMoveBck, Bck: rtd.renamedBck, Timeout: tools.RebalanceTimeout}
	_, err := api.WaitForXactionIC(baseParams, &xargs)
	if err != nil {
		if herr, ok := err.(*cmn.ErrHTTP); ok && herr.Status == http.StatusNotFound {
			smap := tools.GetClusterMap(t, proxyURL)
			if smap.CountActiveTs() == 1 {
				err = nil
			}
		}
		tassert.CheckFatal(t, err)
	} else {
		tlog.Logfln("rename-bucket[%s] %s => %s done", xid, rtd.bck.String(), rtd.renamedBck.String())
	}
	bcks, err := api.ListBuckets(baseParams, cmn.QueryBcks{Provider: rtd.bck.Provider}, apc.FltPresent)
	tassert.CheckFatal(t, err)

	if len(bcks) != rtd.numBuckets {
		t.Fatalf("wrong number of ais buckets (names) before and after rename (before: %d. after: %+v)",
			rtd.numBuckets, bcks)
	}

	renamedBucketExists := false
	for _, bck := range bcks {
		switch bck.Name {
		case rtd.renamedBck.Name:
			renamedBucketExists = true
		case rtd.bck.Name:
			t.Fatalf("original ais bucket %s still exists after rename", rtd.bck.String())
		}
	}

	if !renamedBucketExists {
		t.Fatalf("renamed ais bucket %s does not exist after rename", rtd.renamedBck.String())
	}

	lst, err := api.ListObjects(baseParams, rtd.renamedBck, nil, api.ListArgs{})
	tassert.CheckFatal(t, err)
	unique := make(map[string]bool)
	for _, e := range lst.Entries {
		base := filepath.Base(e.Name)
		unique[base] = true
	}
	if len(unique) != numPuts {
		const maxcnt = 4
		var cnt int
		for _, name := range objNames {
			if _, ok := unique[name]; !ok {
				cnt++
				if cnt > maxcnt && numPuts-len(unique)-cnt > 1 {
					tlog.Logfln("not found: %s (and %d more objects)", name, numPuts-len(unique)-cnt)
					break
				}
				tlog.Logfln("not found: %s", name)
			}
		}
		err := fmt.Errorf("wrong number of objects in the bucket %s renamed as %s (before: %d. after: %d)",
			rtd.bck.String(), rtd.renamedBck.String(), numPuts, len(unique))
		if rtd.wait {
			t.Fatal(err)
		} else {
			tlog.Logln("Warning: " + err.Error())
		}
	}
}

func TestRenameObjects(t *testing.T) {
	var (
		renameStr  = "rename"
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
		bck        = cmn.Bck{
			Name:     t.Name(),
			Provider: apc.AIS,
		}
	)

	tools.CreateBucket(t, proxyURL, bck, nil, true /*cleanup*/)

	objNames, _, err := tools.PutRandObjs(tools.PutObjectsArgs{
		ProxyURL:  proxyURL,
		Bck:       bck,
		ObjCnt:    100,
		CksumType: bck.DefaultProps(initialClusterConfig).Cksum.Type,
	})
	tassert.CheckFatal(t, err)

	newObjNames := make([]string, 0, len(objNames))
	for i, objName := range objNames {
		newObjName := path.Join(renameStr, objName) + ".renamed" // objName fqn
		newObjNames = append(newObjNames, newObjName)

		err := api.RenameObject(baseParams, bck, objName, newObjName)
		tassert.CheckFatal(t, err)

		i++
		if i%50 == 0 {
			tlog.Logfln("Renamed %s => %s", objName, newObjName)
		}
	}

	// Check that renamed objects exist.
	for _, newObjName := range newObjNames {
		_, err := api.GetObject(baseParams, bck, newObjName, nil)
		tassert.CheckError(t, err)
	}
}

func TestObjectPrefix(t *testing.T) {
	runProviderTests(t, func(t *testing.T, bck *meta.Bck) {
		var (
			proxyURL  = tools.RandomProxyURL(t)
			b         = bck.Clone()
			fileNames = prefixCreateFiles(t, proxyURL, b, bck.Props.Cksum.Type)
		)
		prefixLookup(t, proxyURL, b, fileNames)
		prefixCleanup(t, proxyURL, b, fileNames)
	})
}

func TestReregisterMultipleTargets(t *testing.T) {
	tools.CheckSkip(t, &tools.SkipTestArgs{Long: true})

	var (
		filesSentOrig = make(map[string]int64)
		filesRecvOrig = make(map[string]int64)
		bytesSentOrig = make(map[string]int64)
		bytesRecvOrig = make(map[string]int64)
		filesSent     int64
		filesRecv     int64
		bytesSent     int64
		bytesRecv     int64

		m = ioContext{
			t:   t,
			num: 10000,
		}
	)

	m.initAndSaveState(true /*cleanup*/)
	m.expectTargets(2)
	targetsToUnregister := m.originalTargetCount - 1

	// Step 0: Collect rebalance stats
	clusterStats := tools.GetClusterStats(t, m.proxyURL)
	for targetID, targetStats := range clusterStats.Target {
		filesSentOrig[targetID] = tools.GetNamedStatsVal(targetStats, cos.StreamsOutObjCount)
		filesRecvOrig[targetID] = tools.GetNamedStatsVal(targetStats, cos.StreamsInObjCount)
		bytesSentOrig[targetID] = tools.GetNamedStatsVal(targetStats, cos.StreamsOutObjSize)
		bytesRecvOrig[targetID] = tools.GetNamedStatsVal(targetStats, cos.StreamsInObjSize)
	}

	// Step 1: Unregister multiple targets
	removed := make(map[string]*meta.Snode, m.smap.CountActiveTs()-1)
	defer func() {
		var rebID string
		for _, tgt := range removed {
			rebID = m.stopMaintenance(tgt)
		}
		tools.WaitForRebalanceByID(t, baseParams, rebID)
	}()

	targets := m.smap.Tmap.ActiveNodes()
	for i := range targetsToUnregister {
		tlog.Logfln("Put %s in maintenance (no rebalance)", targets[i].StringEx())
		args := &apc.ActValRmNode{DaemonID: targets[i].ID(), SkipRebalance: true}
		_, err := api.StartMaintenance(baseParams, args)
		tassert.CheckFatal(t, err)
		removed[targets[i].ID()] = targets[i]
	}

	smap, err := tools.WaitForClusterState(proxyURL, "remove targets",
		m.smap.Version, m.originalProxyCount, m.originalTargetCount-targetsToUnregister)
	tassert.CheckFatal(t, err)
	tlog.Logfln("The cluster now has %d target(s)", smap.CountActiveTs())

	// Step 2: PUT objects into a newly created bucket
	tools.CreateBucket(t, m.proxyURL, m.bck, nil, true /*cleanup*/)
	m.puts()

	// Step 3: Start performing GET requests
	go m.getsUntilStop()

	// Step 4: Simultaneously reregister each
	wg := &sync.WaitGroup{}
	for i := range targetsToUnregister {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			m.stopMaintenance(targets[r])
			delete(removed, targets[r].ID())
		}(i)
		time.Sleep(5 * time.Second) // wait some time before reregistering next target
	}
	wg.Wait()
	tlog.Logfln("Stopping GETs...")
	m.stopGets()

	baseParams := tools.BaseAPIParams(m.proxyURL)
	tools.WaitForRebalAndResil(t, baseParams)

	clusterStats = tools.GetClusterStats(t, m.proxyURL)
	for targetID, targetStats := range clusterStats.Target {
		filesSent += tools.GetNamedStatsVal(targetStats, cos.StreamsOutObjCount) - filesSentOrig[targetID]
		filesRecv += tools.GetNamedStatsVal(targetStats, cos.StreamsInObjCount) - filesRecvOrig[targetID]
		bytesSent += tools.GetNamedStatsVal(targetStats, cos.StreamsOutObjSize) - bytesSentOrig[targetID]
		bytesRecv += tools.GetNamedStatsVal(targetStats, cos.StreamsInObjSize) - bytesRecvOrig[targetID]
	}

	// Step 5: Log rebalance stats
	tlog.Logfln("Rebalance sent     %s in %d files", cos.ToSizeIEC(bytesSent, 2), filesSent)
	tlog.Logfln("Rebalance received %s in %d files", cos.ToSizeIEC(bytesRecv, 2), filesRecv)

	m.ensureNoGetErrors()
	m.waitAndCheckCluState()
}

func TestGetNodeStats(t *testing.T) {
	proxyURL := tools.RandomProxyURL(t)
	baseParams := tools.BaseAPIParams(proxyURL)
	smap := tools.GetClusterMap(t, proxyURL)

	proxy, err := smap.GetRandProxy(false)
	tassert.CheckFatal(t, err)
	tlog.Logfln("%s:", proxy.StringEx())
	stats, err := api.GetDaemonStats(baseParams, proxy)
	tassert.CheckFatal(t, err)
	tlog.Logfln("%+v", stats)

	target, err := smap.GetRandTarget()
	tassert.CheckFatal(t, err)
	tlog.Logfln("%s:", target.StringEx())
	stats, err = api.GetDaemonStats(baseParams, target)
	tassert.CheckFatal(t, err)
	tlog.Logfln("%+v", stats)
}

func TestGetClusterStats(t *testing.T) {
	proxyURL := tools.RandomProxyURL(t)
	smap := tools.GetClusterMap(t, proxyURL)
	cluStats := tools.GetClusterStats(t, proxyURL)

	for tid, vStats := range cluStats.Target {
		tsi := smap.GetNode(tid)
		tname := tsi.StringEx()
		tassert.Fatalf(t, tsi != nil, "%s is nil", tid)
		tStats, err := api.GetDaemonStats(baseParams, tsi)
		tassert.CheckFatal(t, err)

		vCDF := vStats.Tcdf
		tCDF := tStats.Tcdf
		if vCDF.PctMax != tCDF.PctMax || vCDF.PctAvg != tCDF.PctAvg {
			t.Errorf("%s: stats are different: [%+v] vs [%+v]\n", tname, vCDF, tCDF)
		}
		if len(vCDF.Mountpaths) != len(tCDF.Mountpaths) {
			t.Errorf("%s: num mountpaths is different: [%+v] vs [%+v]\n", tname, vCDF, tCDF)
		}
		for mpath := range vCDF.Mountpaths {
			tcdf := tCDF.Mountpaths[mpath]
			s := tname + mpath
			if tcdf.Capacity.Used != 0 {
				tlog.Logfln("%-30s %+v %+v", s, tcdf.Disks, tcdf.Capacity)
			}
		}
	}
}

func TestLRU(t *testing.T) {
	var (
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)

		m = &ioContext{
			t:      t,
			bck:    cliBck,
			num:    100,
			prefix: t.Name() + "_" + cos.GenTie(),
		}
	)

	tools.CheckSkip(t, &tools.SkipTestArgs{RemoteBck: true, Bck: m.bck, RequiredDeployment: tools.ClusterTypeLocal})

	m.init(true /*cleanup*/)
	m.remotePuts(false /*evict*/)

	// NOTE: cannot "fight" atimes here, need to unload all cached LOMs
	err := api.ClearLcache(baseParams, "" /*all targets*/)
	tassert.CheckFatal(t, err)

	m.backdateLocalObjs(time.Hour * 3)

	// Remember targets' watermarks
	var (
		usedPct      = int32(100)
		cluStats     = tools.GetClusterStats(t, proxyURL)
		filesEvicted = make(map[string]int64)
		bytesEvicted = make(map[string]int64)
	)

	// Find out min usage % across all targets
	for tid, v := range cluStats.Target {
		filesEvicted[tid] = tools.GetNamedStatsVal(v, "lru.evict.n")
		bytesEvicted[tid] = tools.GetNamedStatsVal(v, "lru.evict.size")
		for _, c := range v.Tcdf.Mountpaths {
			usedPct = min(usedPct, c.PctUsed)
		}
	}

	var (
		lowWM     = usedPct - 5
		cleanupWM = lowWM - 1
		highWM    = usedPct - 2
	)
	if int(lowWM) < 2 {
		t.Skipf("The current space usage is too low (%d) for the LRU to be tested", lowWM)
		return
	}

	tlog.Logfln("LRU: current min space usage in the cluster: %d%%", usedPct)
	tlog.Logfln("setting 'space.lowwm=%d' and 'space.highwm=%d'", lowWM, highWM)

	// All targets: set new watermarks; restore upon exit
	oconfig := tools.GetClusterConfig(t)
	t.Cleanup(func() {
		var (
			cleanupWMStr, _ = cos.ConvertToString(oconfig.Space.CleanupWM)
			lowWMStr, _     = cos.ConvertToString(oconfig.Space.LowWM)
			highWMStr, _    = cos.ConvertToString(oconfig.Space.HighWM)
		)
		tools.SetClusterConfig(t, cos.StrKVs{
			"space.cleanupwm":       cleanupWMStr,
			"space.lowwm":           lowWMStr,
			"space.highwm":          highWMStr,
			"lru.dont_evict_time":   oconfig.LRU.DontEvictTime.String(),
			"lru.capacity_upd_time": oconfig.LRU.CapacityUpdTime.String(),
		})
	})

	// Cluster-wide reduce dont-evict-time
	cleanupWMStr, _ := cos.ConvertToString(cleanupWM)
	lowWMStr, _ := cos.ConvertToString(lowWM)
	highWMStr, _ := cos.ConvertToString(highWM)
	tools.SetClusterConfig(t, cos.StrKVs{
		"space.cleanupwm":       cleanupWMStr,
		"space.lowwm":           lowWMStr,
		"space.highwm":          highWMStr,
		"lru.dont_evict_time":   time.Hour.String(),
		"lru.capacity_upd_time": "10s",
	})

	tlog.Logln("starting LRU...")
	xid, err := api.StartXaction(baseParams, &xact.ArgsMsg{Kind: apc.ActLRU}, "")
	tassert.CheckFatal(t, err)

	args := xact.ArgsMsg{ID: xid, Kind: apc.ActLRU, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	// Check results
	tlog.Logln("checking the results...")
	cluStats = tools.GetClusterStats(t, proxyURL)
	for k, v := range cluStats.Target {
		diffFilesEvicted := tools.GetNamedStatsVal(v, "lru.evict.n") - filesEvicted[k]
		diffBytesEvicted := tools.GetNamedStatsVal(v, "lru.evict.size") - bytesEvicted[k]
		tlog.Logf(
			"Target %s: evicted %d objects - %s (%dB) total\n",
			k, diffFilesEvicted, cos.ToSizeIEC(diffBytesEvicted, 2), diffBytesEvicted,
		)

		if diffFilesEvicted == 0 {
			t.Errorf("Target %s: LRU failed to evict any objects", k)
		}
	}
}

func TestPrefetchList(t *testing.T) {
	var (
		m = ioContext{
			t:        t,
			bck:      cliBck,
			num:      100,
			fileSize: cos.KiB,
			chunksConf: &ioCtxChunksConf{
				multipart: true,
				numChunks: 4, // will create 4 chunks
			},
		}
		bck        = cliBck
		proxyURL   = tools.RandomProxyURL(t)
		baseParams = tools.BaseAPIParams(proxyURL)
	)

	tools.CheckSkip(t, &tools.SkipTestArgs{Long: true, RemoteBck: true, Bck: bck})

	m.initAndSaveState(true /*cleanup*/)
	m.expectTargets(2)
	m.puts()

	// 2. Evict those objects from the cache and prefetch them
	tlog.Logfln("Evicting and prefetching %d objects", len(m.objNames))
	evdMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: m.objNames}}
	xid, err := api.EvictMultiObj(baseParams, bck, evdMsg)
	if err != nil {
		t.Error(err)
	}

	args := xact.ArgsMsg{ID: xid, Kind: apc.ActEvictObjects, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	// 3. Prefetch evicted objects
	{
		var msg apc.PrefetchMsg
		msg.ObjNames = m.objNames
		xid, err = api.Prefetch(baseParams, bck, &msg)
		if err != nil {
			t.Error(err)
		}
	}

	args = xact.ArgsMsg{ID: xid, Kind: apc.ActPrefetchObjects, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	// 4. Ensure that all the prefetches occurred.
	xargs := xact.ArgsMsg{ID: xid, Timeout: tools.RebalanceTimeout}
	snaps, err := api.QueryXactionSnaps(baseParams, &xargs)
	tassert.CheckFatal(t, err)
	locObjs, _, _ := snaps.ObjCounts(xid)
	if locObjs != int64(m.num) {
		t.Errorf("did not prefetch all files: missing %d of %d", int64(m.num)-locObjs, m.num)
	}

	msg := &apc.LsoMsg{}
	msg.SetFlag(apc.LsCached)
	lst, err := api.ListObjects(baseParams, bck, msg, api.ListArgs{})
	tassert.CheckFatal(t, err)
	if len(lst.Entries) != m.num {
		t.Errorf("list-objects %s: expected %d, got %d", bck.String(), m.num, len(lst.Entries))
	} else {
		tlog.Logfln("list-objects %s: %d is correct", bck.String(), len(m.objNames))
	}
}

func TestDeleteList(t *testing.T) {
	runProviderTests(t, func(t *testing.T, bck *meta.Bck) {
		var (
			err        error
			prefix     = "__listrange/tstf-"
			wg         = &sync.WaitGroup{}
			objCnt     = 100
			errCh      = make(chan error, objCnt)
			files      = make([]string, 0, objCnt)
			proxyURL   = tools.RandomProxyURL(t)
			baseParams = tools.BaseAPIParams(proxyURL)
			b          = bck.Clone()
		)

		// 1. Put files to delete
		for i := range objCnt {
			r, err := readers.NewRand(fileSize, bck.Props.Cksum.Type)
			tassert.CheckFatal(t, err)

			keyname := fmt.Sprintf("%s%d", prefix, i)

			wg.Add(1)
			go func() {
				defer wg.Done()
				tools.Put(proxyURL, b, keyname, r, errCh)
			}()
			files = append(files, keyname)
		}
		wg.Wait()
		tassert.SelectErr(t, errCh, "put", true)
		tlog.Logfln("PUT done.")

		// 2. Delete the objects
		evdMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: files}}
		xid, err := api.DeleteMultiObj(baseParams, b, evdMsg)
		tassert.CheckError(t, err)

		args := xact.ArgsMsg{ID: xid, Kind: apc.ActDeleteObjects, Timeout: tools.RebalanceTimeout}
		_, err = api.WaitForXactionIC(baseParams, &args)
		tassert.CheckFatal(t, err)

		// 3. Check to see that all the files have been deleted
		msg := &apc.LsoMsg{Prefix: prefix}
		bktlst, err := api.ListObjects(baseParams, b, msg, api.ListArgs{})
		tassert.CheckFatal(t, err)
		if len(bktlst.Entries) != 0 {
			t.Errorf("Incorrect number of remaining files: %d, should be 0", len(bktlst.Entries))
		}
	})
}

func TestPrefetchRange(t *testing.T) {
	var (
		m = ioContext{
			t:        t,
			bck:      cliBck,
			num:      200,
			fileSize: cos.KiB,
			prefix:   "regressionList/obj-",
			ordered:  true,
		}
		proxyURL      = tools.RandomProxyURL(t)
		baseParams    = tools.BaseAPIParams(proxyURL)
		prefetchRange = "{1..150}"
		bck           = cliBck
	)

	tools.CheckSkip(t, &tools.SkipTestArgs{Long: true, RemoteBck: true, Bck: bck})

	m.initAndSaveState(true /*cleanup*/)

	m.expectTargets(2)
	m.puts()
	// 1. Parse arguments
	pt, err := cos.ParseBashTemplate(prefetchRange)
	tassert.CheckFatal(t, err)
	rangeMin, rangeMax := pt.Ranges[0].Start, pt.Ranges[0].End

	// 2. Discover the number of items we expect to be prefetched
	files := make([]string, 0)
	for _, objName := range m.objNames {
		oName := strings.TrimPrefix(objName, m.prefix)
		if i, err := strconv.ParseInt(oName, 10, 64); err != nil {
			continue
		} else if (rangeMin == 0 && rangeMax == 0) || (i >= rangeMin && i <= rangeMax) {
			files = append(files, objName)
		}
	}

	// 3. Evict those objects from the cache, and then prefetch them
	rng := fmt.Sprintf("%s%s", m.prefix, prefetchRange)
	tlog.Logfln("Evicting and prefetching %d objects (range: %s)", len(files), rng)
	evdMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: nil, Template: rng}}
	xid, err := api.EvictMultiObj(baseParams, bck, evdMsg)
	tassert.CheckError(t, err)
	args := xact.ArgsMsg{ID: xid, Kind: apc.ActEvictObjects, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	{
		var msg apc.PrefetchMsg
		msg.Template = rng
		xid, err = api.Prefetch(baseParams, bck, &msg)
		tassert.CheckError(t, err)
		args = xact.ArgsMsg{ID: xid, Kind: apc.ActPrefetchObjects, Timeout: tools.RebalanceTimeout}
		_, err = api.WaitForXactionIC(baseParams, &args)
		tassert.CheckFatal(t, err)
	}

	// 4. Ensure all done
	xargs := xact.ArgsMsg{ID: xid, Timeout: tools.RebalanceTimeout}
	snaps, err := api.QueryXactionSnaps(baseParams, &xargs)
	tassert.CheckFatal(t, err)
	locObjs, _, _ := snaps.ObjCounts(xid)
	if locObjs != int64(len(files)) {
		t.Errorf("did not prefetch all files: missing %d of %d", int64(len(files))-locObjs, len(files))
	}

	msg := &apc.LsoMsg{Prefix: m.prefix}
	msg.SetFlag(apc.LsCached)
	lst, err := api.ListObjects(baseParams, bck, msg, api.ListArgs{})
	tassert.CheckFatal(t, err)
	if len(lst.Entries) < len(files) {
		t.Errorf("list-objects %s/%s: expected %d, got %d", bck.String(), m.prefix, len(files), len(lst.Entries))
	} else {
		var count int
		for _, e := range lst.Entries {
			s := e.Name[len(m.prefix):] // "obj-"
			idx, err := strconv.Atoi(s)
			if err == nil && idx >= 1 && idx <= 150 {
				count++
			}
		}
		if count != len(files) {
			t.Errorf("list-objects %s/%s: expected %d, got %d", bck.String(), m.prefix, len(files), count)
		} else {
			tlog.Logfln("list-objects %s/%s: %d is correct", bck.String(), m.prefix, len(files))
		}
	}
}

func TestDeleteRange(t *testing.T) {
	runProviderTests(t, func(t *testing.T, bck *meta.Bck) {
		var (
			err            error
			objCnt         = 100
			quarter        = objCnt / 4
			third          = objCnt / 3
			smallrangesize = third - quarter + 1
			prefix         = "__listrange/tstf-"
			smallrange     = fmt.Sprintf("%s{%04d..%04d}", prefix, quarter, third)
			bigrange       = fmt.Sprintf("%s{0000..%04d}", prefix, objCnt)
			wg             = &sync.WaitGroup{}
			errCh          = make(chan error, objCnt)
			proxyURL       = tools.RandomProxyURL(t)
			baseParams     = tools.BaseAPIParams(proxyURL)
			b              = bck.Clone()
		)

		// 1. Put files to delete
		for i := range objCnt {
			r, err := readers.NewRand(fileSize, bck.Props.Cksum.Type)
			tassert.CheckFatal(t, err)

			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				tools.Put(proxyURL, b, fmt.Sprintf("%s%04d", prefix, i), r, errCh)
			}(i)
		}
		wg.Wait()
		tassert.SelectErr(t, errCh, "put", true)
		tlog.Logfln("PUT done.")

		// 2. Delete the small range of objects
		tlog.Logfln("Delete in range %s", smallrange)
		evdSmallMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: nil, Template: smallrange}}
		xid, err := api.DeleteMultiObj(baseParams, b, evdSmallMsg)
		tassert.CheckError(t, err)
		args := xact.ArgsMsg{ID: xid, Kind: apc.ActDeleteObjects, Timeout: tools.RebalanceTimeout}
		_, err = api.WaitForXactionIC(baseParams, &args)
		tassert.CheckFatal(t, err)

		// 3. Check to see that the correct files have been deleted
		msg := &apc.LsoMsg{Prefix: prefix}
		bktlst, err := api.ListObjects(baseParams, b, msg, api.ListArgs{})
		tassert.CheckFatal(t, err)
		if len(bktlst.Entries) != objCnt-smallrangesize {
			t.Errorf("Incorrect number of remaining files: %d, should be %d", len(bktlst.Entries), objCnt-smallrangesize)
		}
		filemap := make(map[string]*cmn.LsoEnt)
		for _, en := range bktlst.Entries {
			filemap[en.Name] = en
		}
		for i := range objCnt {
			keyname := fmt.Sprintf("%s%04d", prefix, i)
			_, ok := filemap[keyname]
			if ok && i >= quarter && i <= third {
				t.Errorf("File exists that should have been deleted: %s", keyname)
			} else if !ok && (i < quarter || i > third) {
				t.Errorf("File does not exist that should not have been deleted: %s", keyname)
			}
		}

		tlog.Logfln("Delete in range %s", bigrange)
		// 4. Delete the big range of objects
		evdBigMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: nil, Template: bigrange}}
		xid, err = api.DeleteMultiObj(baseParams, b, evdBigMsg)
		tassert.CheckError(t, err)
		args = xact.ArgsMsg{ID: xid, Kind: apc.ActDeleteObjects, Timeout: tools.RebalanceTimeout}
		_, err = api.WaitForXactionIC(baseParams, &args)
		tassert.CheckFatal(t, err)

		// 5. Check to see that all the files have been deleted
		bktlst, err = api.ListObjects(baseParams, b, msg, api.ListArgs{})
		tassert.CheckFatal(t, err)
		if len(bktlst.Entries) != 0 {
			t.Errorf("Incorrect number of remaining files: %d, should be 0", len(bktlst.Entries))
		}
	})
}

// Testing only ais bucket objects since generally not concerned with cloud bucket object deletion
func TestStressDeleteRange(t *testing.T) {
	tools.CheckSkip(t, &tools.SkipTestArgs{Long: true})

	const (
		numFiles   = 20000 // FIXME: must divide by 10 and by the numReaders
		numReaders = 200
	)

	var (
		err           error
		wg            = &sync.WaitGroup{}
		errCh         = make(chan error, numFiles)
		proxyURL      = tools.RandomProxyURL(t)
		tenth         = numFiles / 10
		objNamePrefix = "__listrange/tstf-"
		partialRange  = fmt.Sprintf("%s{%d..%d}", objNamePrefix, 0, numFiles-tenth-1) // TODO: partial range with non-zero left boundary
		fullRange     = fmt.Sprintf("%s{0..%d}", objNamePrefix, numFiles)
		baseParams    = tools.BaseAPIParams(proxyURL)
		bck           = cmn.Bck{
			Name:     testBucketName,
			Provider: apc.AIS,
		}
		cksumType = bck.DefaultProps(initialClusterConfig).Cksum.Type
	)

	tools.CreateBucket(t, proxyURL, bck, nil, true /*cleanup*/)

	// 1. PUT
	tlog.Logln("putting objects...")
	for i := range numReaders {
		size := rand.Int64N(cos.KiB*128) + cos.KiB/3
		tassert.CheckFatal(t, err)
		reader, err := readers.NewRand(size, cksumType)
		tassert.CheckFatal(t, err)

		wg.Add(1)
		go func(i int, reader readers.Reader) {
			defer wg.Done()

			for j := range numFiles / numReaders {
				objName := fmt.Sprintf("%s%d", objNamePrefix, i*numFiles/numReaders+j)
				putArgs := api.PutArgs{
					BaseParams: baseParams,
					Bck:        bck,
					ObjName:    objName,
					Cksum:      reader.Cksum(),
					Reader:     reader,
				}
				_, err = api.PutObject(&putArgs)
				if err != nil {
					errCh <- err
				}
				reader.Reset()
			}
		}(i, reader)
	}
	wg.Wait()
	tassert.SelectErr(t, errCh, "put", true)

	// 2. Delete a range of objects
	tlog.Logfln("Deleting objects in range: %s", partialRange)
	evdPartialMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: nil, Template: partialRange}}
	xid, err := api.DeleteMultiObj(baseParams, bck, evdPartialMsg)
	tassert.CheckError(t, err)
	args := xact.ArgsMsg{ID: xid, Kind: apc.ActDeleteObjects, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	// 3. Check to see that correct objects have been deleted
	expectedRemaining := tenth
	msg := &apc.LsoMsg{Prefix: objNamePrefix}
	lst, err := api.ListObjects(baseParams, bck, msg, api.ListArgs{})
	tassert.CheckFatal(t, err)
	if len(lst.Entries) != expectedRemaining {
		t.Errorf("Incorrect number of remaining objects: %d, expected: %d",
			len(lst.Entries), expectedRemaining)
	}

	objNames := make(map[string]*cmn.LsoEnt)
	for _, en := range lst.Entries {
		objNames[en.Name] = en
	}
	for i := range numFiles {
		objName := fmt.Sprintf("%s%d", objNamePrefix, i)
		_, ok := objNames[objName]
		if ok && i < numFiles-tenth {
			t.Errorf("%s exists (expected to be deleted)", objName)
		} else if !ok && i >= numFiles-tenth {
			t.Errorf("%s does not exist", objName)
		}
	}

	// 4. Delete the entire range of objects
	tlog.Logfln("Deleting objects in range: %s", fullRange)
	evdFullMsg := &apc.EvdMsg{ListRange: apc.ListRange{ObjNames: nil, Template: fullRange}}
	xid, err = api.DeleteMultiObj(baseParams, bck, evdFullMsg)
	tassert.CheckError(t, err)
	args = xact.ArgsMsg{ID: xid, Kind: apc.ActDeleteObjects, Timeout: tools.RebalanceTimeout}
	_, err = api.WaitForXactionIC(baseParams, &args)
	tassert.CheckFatal(t, err)

	// 5. Check to see that all files have been deleted
	msg = &apc.LsoMsg{Prefix: objNamePrefix}
	lst, err = api.ListObjects(baseParams, bck, msg, api.ListArgs{})
	tassert.CheckFatal(t, err)
	if len(lst.Entries) != 0 {
		t.Errorf("Incorrect number of remaining files: %d, should be 0", len(lst.Entries))
	}
}
