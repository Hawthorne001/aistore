// Package integration_test.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/readers"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/tlog"
	"github.com/NVIDIA/aistore/tools/trand"
	"github.com/NVIDIA/aistore/xact"

	jsoniter "github.com/json-iterator/go"
)

// more tools

const rebalanceObjectDistributionTestCoef = 0.3

const (
	prefixDir     = "filter"
	largeFileSize = 4 * cos.MiB

	workerCnt = 10
)

const testMpath = "/tmp/ais/mountpath"

var (
	cliBck             cmn.Bck
	cliIOCtxChunksConf *ioCtxChunksConf
	errObjectFound     = errors.New("found") // to interrupt fs.Walk when object found
	_onceInit          sync.Once
)

type (
	ioContext struct {
		t         *testing.T
		smap      *meta.Smap
		controlCh chan struct{}
		stopCh    chan struct{}
		objNames  []string
		bck       cmn.Bck

		// Chunks configuration
		chunksConf *ioCtxChunksConf

		// File size configuration
		fileSize      uint64
		fixedSize     bool
		fileSizeRange [2]uint64 // [min, max] size range; take precedence over `fileSize` and `fixedSize`

		proxyURL            string
		prefix              string
		otherTasksToTrigger int
		originalTargetCount int
		originalProxyCount  int
		num                 int
		numGetsEachFile     int
		nameLen             int
		objIdx              int // Used in `m.nextObjName`
		numPutErrs          int

		numGetErrs atomic.Uint64

		getErrIsFatal       bool
		silent              bool
		deleteRemoteBckObjs bool
		ordered             bool // true - object names make sequence, false - names are random
		skipVC              bool // skip loading existing object's metadata (see also: apc.QparamSkipVC and api.PutArgs.SkipVC)
	}
	ioCtxChunksConf struct {
		numChunks int // desired number of chunks
		multipart bool
	}
)

func (m *ioContext) initAndSaveState(cleanup bool) {
	m.init(cleanup)
	m.saveCluState(m.proxyURL)
}

func (m *ioContext) saveCluState(proxyURL string) {
	m.smap = tools.GetClusterMap(m.t, proxyURL)
	m.originalTargetCount = m.smap.CountActiveTs()
	m.originalProxyCount = m.smap.CountActivePs()
	tlog.Logfln("targets: %d, proxies: %d", m.originalTargetCount, m.originalProxyCount)
}

func (m *ioContext) waitAndCheckCluState() {
	smap, err := tools.WaitForClusterState(
		m.proxyURL,
		"cluster state",
		m.smap.Version,
		m.originalProxyCount,
		m.originalTargetCount,
	)
	tassert.CheckFatal(m.t, err)
	m.checkCluState(smap)
}

func (m *ioContext) checkCluState(smap *meta.Smap) {
	m.t.Helper()
	proxyCount := smap.CountActivePs()
	targetCount := smap.CountActiveTs()
	if targetCount != m.originalTargetCount ||
		proxyCount != m.originalProxyCount {
		m.t.Errorf(
			"cluster state is not preserved. targets (before: %d, now: %d); proxies: (before: %d, now: %d)",
			targetCount, m.originalTargetCount,
			proxyCount, m.originalProxyCount,
		)
	}
}

func (m *ioContext) init(cleanup bool) {
	m.proxyURL = tools.RandomProxyURL()
	if m.proxyURL == "" {
		// if random selection failed, use RO url
		m.proxyURL = tools.GetPrimaryURL()
	}
	if m.fileSize == 0 && m.fileSizeRange[0] == 0 && m.fileSizeRange[1] == 0 {
		m.fileSizeRange[0], m.fileSizeRange[1] = cos.KiB, 4*cos.KiB
	}
	if m.num > 0 {
		m.objNames = make([]string, 0, m.num)
	}
	if m.otherTasksToTrigger > 0 {
		m.controlCh = make(chan struct{}, m.otherTasksToTrigger)
	}
	if m.bck.Name == "" {
		m.bck.Name = trand.String(15)
	}
	if m.bck.Provider == "" {
		m.bck.Provider = apc.AIS
	}
	if m.numGetsEachFile == 0 {
		m.numGetsEachFile = 1
	}
	m.stopCh = make(chan struct{})

	// NOTE: randomize skipVC (may need to assign explicitly in the future)
	m.skipVC = mono.NanoTime()&1 == 0

	if m.bck.IsRemote() {
		if m.deleteRemoteBckObjs {
			m.del(-1 /*delete all*/, 0 /* lsmsg.Flags */)
		} else {
			tools.EvictRemoteBucket(m.t, m.proxyURL, m.bck, true /*keepMD*/)
		}
	}
	if cleanup {
		// cleanup upon exit from this (m.t) test
		m.t.Cleanup(func() {
			m.del()
			if m.bck.IsRemote() {
				tools.EvictRemoteBucket(m.t, m.proxyURL, m.bck, true /*keepMD*/)
			}
		})
	}
	// If no chunks configuration is provided, use the default from the environment variable
	if m.chunksConf == nil {
		m.chunksConf = cliIOCtxChunksConf
	}
}

func (m *ioContext) expectTargets(n int) {
	if m.originalTargetCount < n {
		m.t.Skipf("Must have %d or more targets in the cluster, have only %d", n, m.originalTargetCount)
	}
}

func (m *ioContext) expectProxies(n int) {
	if m.originalProxyCount < n {
		m.t.Skipf("Must have %d or more proxies in the cluster, have only %d", n, m.originalProxyCount)
	}
}

func (m *ioContext) checkObjectDistribution(t *testing.T) {
	m.t.Helper()
	var (
		requiredCount     = int64(rebalanceObjectDistributionTestCoef * (float64(m.num) / float64(m.originalTargetCount)))
		targetObjectCount = make(map[string]int64)
	)
	tlog.Logfln("Checking if each target has a required number of object in bucket %s...", m.bck.String())
	baseParams := tools.BaseAPIParams(m.proxyURL)
	lst, err := api.ListObjects(baseParams, m.bck, &apc.LsoMsg{Props: apc.GetPropsLocation}, api.ListArgs{})
	tassert.CheckFatal(t, err)
	for _, obj := range lst.Entries {
		tname, _ := core.ParseObjLoc(obj.Location)
		tid := meta.N2ID(tname)
		targetObjectCount[tid]++
	}
	if len(targetObjectCount) != m.originalTargetCount {
		t.Fatalf("Rebalance error, %d/%d targets received no objects from bucket %s\n",
			m.originalTargetCount-len(targetObjectCount), m.originalTargetCount, m.bck.String())
	}
	for targetURL, objCount := range targetObjectCount {
		if objCount < requiredCount {
			t.Fatalf("Rebalance error, target %s didn't receive required number of objects\n", targetURL)
		}
	}
}

func (m *ioContext) sizesToString() (s string) {
	siz0, siz1 := int64(m.fileSizeRange[0]), int64(m.fileSizeRange[1])
	switch {
	case siz0 >= 0 && siz1 > 0:
		s = fmt.Sprintf(" (size %s - %s)", cos.ToSizeIEC(siz0, 0), cos.ToSizeIEC(siz1, 0))
		debug.Assert(siz1 >= siz0, s)
	case m.fixedSize:
		s = fmt.Sprintf(" (size %d)", m.fileSize)
	case m.fileSize > 0:
		s = fmt.Sprintf(" (approx. size %d)", m.fileSize)
	}
	if m.chunksConf != nil && m.chunksConf.multipart {
		s += fmt.Sprintf(" (chunked into %d chunks)", m.chunksConf.numChunks)
	}
	return s
}

func (m *ioContext) puts(ignoreErrs ...bool) {
	m.t.Helper()
	if !m.bck.IsAIS() {
		m.remotePuts(false /*evict*/)
		return
	}
	baseParams := tools.BaseAPIParams(m.proxyURL)
	p, err := api.HeadBucket(baseParams, m.bck, false /* don't add */)
	tassert.CheckFatal(m.t, err)

	if !m.silent {
		s := m.sizesToString()
		tlog.Logfln("PUT %d objects%s => %s", m.num, s, m.bck.Cname(m.prefix))
	}
	putArgs := tools.PutObjectsArgs{
		ProxyURL:     m.proxyURL,
		Bck:          m.bck,
		ObjPath:      m.prefix,
		ObjCnt:       m.num,
		ObjNameLn:    m.nameLen,
		ObjSizeRange: m.fileSizeRange, // take precedence over `ObjSize` and `FixedSize`
		ObjSize:      m.fileSize,
		FixedSize:    m.fixedSize,
		CksumType:    p.Cksum.Type,
		WorkerCnt:    0, // TODO: Should we set something custom?
		IgnoreErr:    len(ignoreErrs) > 0 && ignoreErrs[0],
		Ordered:      m.ordered,
		SkipVC:       m.skipVC,
	}
	if m.chunksConf != nil && m.chunksConf.multipart {
		putArgs.MultipartNumChunks = m.chunksConf.numChunks
		m.objNames, m.numPutErrs, err = tools.PutRandObjs(putArgs)
		tassert.CheckFatal(m.t, err)

		// verify objects are chunked
		ls, err := api.ListObjects(baseParams, m.bck, &apc.LsoMsg{Prefix: m.prefix, Props: apc.GetPropsChunked}, api.ListArgs{})
		tassert.CheckFatal(m.t, err)
		if len(ls.Entries) != m.num {
			tlog.Logfln("warning: expected %d objects, got %d", m.num, len(ls.Entries))
		}
	} else {
		m.objNames, m.numPutErrs, err = tools.PutRandObjs(putArgs)
	}
	tassert.CheckFatal(m.t, err)
}

// update updates the object with a new random reader and returns the reader and the size; reader is used to validate the object after the update
func (m *ioContext) update(baseParams api.BaseParams, objName, cksumType string) (readers.Reader, uint64) {
	var (
		putArgs = &tools.PutObjectsArgs{
			ProxyURL:           m.proxyURL,
			Bck:                m.bck,
			ObjPath:            m.prefix,
			ObjCnt:             m.num,
			ObjNameLn:          m.nameLen,
			MultipartNumChunks: m.chunksConf.numChunks,
		}
		size        = putArgs.GetSize()
		reader, err = readers.NewRand(int64(size), cksumType)
	)
	tassert.CheckFatal(m.t, err)

	if m.chunksConf != nil && m.chunksConf.multipart && m.chunksConf.numChunks != 0 {
		putArgs.Reader = reader
		err = tools.PutMultipartObject(baseParams, m.bck, objName, size, putArgs)
	} else {
		_, err = api.PutObject(&api.PutArgs{
			BaseParams: baseParams,
			Bck:        m.bck,
			ObjName:    objName,
			Size:       size,
			Reader:     reader,
			Cksum:      reader.Cksum(),
		})
	}
	tassert.CheckFatal(m.t, err)
	return reader, size
}

func (m *ioContext) updateAndValidate(baseParams api.BaseParams, idx int, cksumType string) {
	if idx < 0 || idx >= len(m.objNames) {
		m.t.Fatalf("index out of range: %d", idx)
	}

	r, size := m.update(baseParams, m.objNames[idx], cksumType)

	// GET and validate the object with the pattern
	w := bytes.NewBuffer(nil)
	result, s, err := api.GetObjectReader(baseParams, m.bck, m.objNames[idx], &api.GetArgs{Writer: w})
	tassert.CheckFatal(m.t, err)

	// Compare retrieved content with original data
	br, err := r.Open()
	tassert.CheckFatal(m.t, err)
	tassert.Fatalf(m.t, s == int64(size), "object %s size mismatch: expected %d, got %d", m.objNames[idx], size, s)
	tassert.Fatalf(m.t, tools.ReaderEqual(br, result), "object %s content mismatch", m.objNames[idx])
}

// remotePuts by default empties remote bucket and puts new `m.num` objects
// into the bucket. If `override` parameter is set then the existing objects
// are updated with new ones (new version and checksum).
func (m *ioContext) remotePuts(evict bool, overrides ...bool) {
	var override bool
	if len(overrides) > 0 {
		override = overrides[0]
	}

	if !override {
		// Cleanup the remote bucket.
		m.del()
		m.objNames = m.objNames[:0]
	}

	m._remoteFill(m.num, evict, override)
}

// remoteRefill calculates number of missing objects and refills the bucket.
// It is expected that the number of missing objects is positive meaning that
// some of the objects were removed before calling remoteRefill.
func (m *ioContext) remoteRefill() {
	var (
		baseParams = tools.BaseAPIParams()
		msg        = &apc.LsoMsg{Prefix: m.prefix, Props: apc.GetPropsName}
	)

	lst, err := api.ListObjects(baseParams, m.bck, msg, api.ListArgs{})
	tassert.CheckFatal(m.t, err)

	m.objNames = m.objNames[:0]
	for _, obj := range lst.Entries {
		m.objNames = append(m.objNames, obj.Name)
	}

	leftToFill := m.num - len(lst.Entries)
	tassert.Fatalf(m.t, leftToFill > 0, "leftToFill %d", leftToFill)

	m._remoteFill(leftToFill, false /*evict*/, false /*override*/)
}

func (m *ioContext) _remoteFill(objCnt int, evict, override bool) {
	var (
		baseParams = tools.BaseAPIParams()
		errCh      = make(chan error, objCnt)
		wg         = cos.NewLimitedWaitGroup(20, 0)
	)
	if !m.silent {
		s := m.sizesToString()
		tlog.Logfln("remote PUT %d objects%s => %s", objCnt, s, m.bck.Cname(m.prefix))
	}
	p, err := api.HeadBucket(baseParams, m.bck, false /* don't add */)
	tassert.CheckFatal(m.t, err)

	for i := range objCnt {
		r, err := readers.NewRand(int64(m.fileSize), p.Cksum.Type)
		tassert.CheckFatal(m.t, err)

		var objName string
		switch {
		case override:
			objName = m.objNames[i]
		case m.ordered:
			objName = fmt.Sprintf("%s%d", m.prefix, i)
		default:
			objName = fmt.Sprintf("%s%s-%d", m.prefix, trand.String(8), i)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			tools.Put(m.proxyURL, m.bck, objName, r, errCh)
		}()
		if !override {
			m.objNames = append(m.objNames, objName)
		}
	}
	wg.Wait()
	tassert.SelectErr(m.t, errCh, "put", true)
	tlog.Logfln("remote bucket %s: %d cached objects", m.bck.String(), m.num)

	if evict {
		m.evict()
	}
}

func (m *ioContext) evict() {
	var (
		baseParams = tools.BaseAPIParams()
		msg        = &apc.LsoMsg{Prefix: m.prefix, Props: apc.GetPropsName}
	)

	lst, err := api.ListObjects(baseParams, m.bck, msg, api.ListArgs{})
	tassert.CheckFatal(m.t, err)
	if len(lst.Entries) != m.num {
		m.t.Fatalf("list_objects err: %d != %d", len(lst.Entries), m.num)
	}

	tlog.Logfln("evicting remote bucket %s...", m.bck.String())
	err = api.EvictRemoteBucket(baseParams, m.bck, false)
	tassert.CheckFatal(m.t, err)
}

// TODO: optionally, filter by content type as well
// NOTE: assuming 'RequiredDeployment == tools.ClusterTypeLocal'
func (m *ioContext) backdateLocalObjs(age time.Duration) {
	var (
		sep     = string(filepath.Separator)
		old     = time.Now().Add(-age)
		touched int
	)
	err := filepath.WalkDir(rootDir, func(path string, de iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if de.IsDir() {
			return nil
		}
		if strings.HasPrefix(de.Name(), m.prefix) && strings.Contains(path, sep+m.bck.Name+sep) {
			touched++
			return os.Chtimes(path, old, old)
		}
		return nil
	})
	tassert.CheckFatal(m.t, err)

	if touched != len(m.objNames) {
		tlog.Logfln("Warning: touched %d != %d objnames", touched, len(m.objNames))
	}
}

func (m *ioContext) remotePrefetch(prefetchCnt int) {
	var (
		baseParams = tools.BaseAPIParams()
		msg        = &apc.LsoMsg{Prefix: m.prefix, Props: apc.GetPropsName}
	)

	lst, err := api.ListObjects(baseParams, m.bck, msg, api.ListArgs{})
	tassert.CheckFatal(m.t, err)

	tlog.Logfln("remote PREFETCH %d objects...", prefetchCnt)

	wg := &sync.WaitGroup{}
	for idx, obj := range lst.Entries {
		if idx >= prefetchCnt {
			break
		}

		wg.Add(1)
		go func(obj *cmn.LsoEnt) {
			_, err := api.GetObject(baseParams, m.bck, obj.Name, nil)
			tassert.CheckError(m.t, err)
			wg.Done()
		}(obj)
	}
	wg.Wait()
}

func isContextDeadline(err error) bool {
	if err == nil {
		return false
	}
	return err == context.DeadlineExceeded || strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}

// bucket cleanup
// is called in a variety of ways including (post-test) t.Cleanup => _cleanup()
// and (pre-test) via deleteRemoteBckObjs

const maxDelObjErrCount = 100

func (m *ioContext) del(opts ...int) {
	var (
		herr        *cmn.ErrHTTP
		toRemoveCnt = -1 // remove all or opts[0]
		baseParams  = tools.BaseAPIParams()
	)
	// checks, params
	exists, err := api.QueryBuckets(baseParams, cmn.QueryBcks(m.bck), apc.FltExists)
	if isContextDeadline(err) {
		if m.bck.IsRemote() {
			time.Sleep(time.Second)
			tlog.Logfln("Warning: 2nd attempt to query buckets %q", cmn.QueryBcks(m.bck))
			exists, err = api.QueryBuckets(baseParams, cmn.QueryBcks(m.bck), apc.FltExists)
			if isContextDeadline(err) {
				tlog.Logfln("Error: failing to query buckets %q: %v - proceeding anyway...", cmn.QueryBcks(m.bck), err)
				exists, err = false, nil
			}
		}
	}
	tassert.CheckFatal(m.t, err)
	if !exists {
		return
	}

	// list
	lsmsg := &apc.LsoMsg{
		Prefix: m.prefix,
		Props:  apc.GetPropsName,
		Flags:  apc.LsBckPresent, // don't lookup unless overridden by the variadic (below)
	}
	if len(opts) > 0 {
		toRemoveCnt = opts[0]
		if len(opts) > 1 {
			lsmsg.Flags = uint64(opts[1]) // do HEAD(remote-bucket)
		}
	}
	if toRemoveCnt < 0 && m.prefix != "" {
		lsmsg.Prefix = "" // all means all
	}
	lst, err := api.ListObjects(baseParams, m.bck, lsmsg, api.ListArgs{})
	if err != nil {
		if errors.As(err, &herr) && herr.Status == http.StatusNotFound {
			return
		}
		emsg := err.Error()
		// ignore client timeout awaiting headers
		if strings.Contains(emsg, "awaiting") && strings.Contains(emsg, "headers") {
			return
		}
	}
	tassert.CheckFatal(m.t, err)

	// delete
	toRemove := lst.Entries
	if toRemoveCnt >= 0 {
		n := min(toRemoveCnt, len(toRemove))
		toRemove = toRemove[:n]
	}
	l := len(toRemove)
	if l == 0 {
		return
	}
	tlog.Logfln("deleting %d object%s from %s", l, cos.Plural(l), m.bck.Cname(lsmsg.Prefix))
	var (
		errCnt atomic.Int64
		wg     = cos.NewLimitedWaitGroup(16, l)
	)
	for _, obj := range toRemove {
		if errCnt.Load() > maxDelObjErrCount {
			tassert.CheckFatal(m.t, errors.New("too many errors"))
			break
		}
		wg.Add(1)
		go func(obj *cmn.LsoEnt) {
			m._delOne(baseParams, obj, &errCnt)
			wg.Done()
		}(obj)
	}
	wg.Wait()
}

func (m *ioContext) _delOne(baseParams api.BaseParams, obj *cmn.LsoEnt, errCnt *atomic.Int64) {
	err := api.DeleteObject(baseParams, m.bck, obj.Name)
	if err == nil {
		return
	}
	//
	// excepting benign (TODO: rid of strings.Contains)
	//
	const sleepRetry = 2 * time.Second
	e := strings.ToLower(err.Error())
	switch {
	case cmn.IsErrObjNought(err):
		return
	case strings.Contains(e, "server closed idle connection"):
		return // see (unexported) http.exportErrServerClosedIdle in the Go source
	case cos.IsErrConnectionNotAvail(err):
		errCnt.Add(maxDelObjErrCount/10 - 1)
	// retry
	case m.bck.IsCloud() && (cos.IsErrConnectionReset(err) || strings.Contains(e, "reset by peer")):
		time.Sleep(sleepRetry)
		err = api.DeleteObject(baseParams, m.bck, obj.Name)
	case m.bck.IsCloud() && strings.Contains(e, "try again"):
		// aws-error[InternalError: We encountered an internal error. Please try again.]
		time.Sleep(sleepRetry)
		err = api.DeleteObject(baseParams, m.bck, obj.Name)
	case m.bck.IsCloud() && apc.ToScheme(m.bck.Provider) == apc.GSScheme &&
		strings.Contains(e, "gateway") && strings.Contains(e, "timeout"):
		// e.g:. "googleapi: Error 504: , gatewayTimeout" (where the gateway is in fact LB)
		time.Sleep(sleepRetry)
		err = api.DeleteObject(baseParams, m.bck, obj.Name)
	}

	if err == nil || cmn.IsErrObjNought(err) {
		return
	}
	errCnt.Inc()
	if m.bck.IsCloud() && errCnt.Load() < 5 {
		tlog.Logfln("Warning: failed to cleanup %s: %v", m.bck.Cname(""), err)
	}
	tassert.CheckError(m.t, err)
}

func (m *ioContext) get(baseParams api.BaseParams, idx, totalGets int, getArgs *api.GetArgs, validate bool) {
	var (
		err     error
		objName = m.objNames[idx%len(m.objNames)]
	)
	if validate {
		_, err = api.GetObjectWithValidation(baseParams, m.bck, objName, getArgs)
	} else {
		_, err = api.GetObject(baseParams, m.bck, objName, getArgs)
	}
	if err != nil {
		if m.getErrIsFatal {
			m.t.Error(err)
		}
		m.numGetErrs.Inc()
	}
	if m.getErrIsFatal && m.numGetErrs.Load() > 0 {
		return
	}
	if idx > 0 && idx%5000 == 0 && !m.silent {
		if totalGets > 0 {
			tlog.Logfln(" %d/%d GET requests completed...", idx, totalGets)
		} else {
			tlog.Logfln(" %d GET requests completed...", idx)
		}
	}

	// Tell other tasks they can begin to do work in parallel
	if totalGets > 0 && idx == totalGets/2 { // only for `m.gets(nil, false)`
		for range m.otherTasksToTrigger {
			m.controlCh <- struct{}{}
		}
	}
}

func (m *ioContext) gets(getArgs *api.GetArgs, withValidation bool) {
	var (
		baseParams = tools.BaseAPIParams()
		totalGets  = m.num * m.numGetsEachFile
	)
	if !m.silent {
		if m.numGetsEachFile == 1 {
			tlog.Logfln("GET %d objects from %s", m.num, m.bck.String())
		} else {
			tlog.Logfln("GET %d objects %d times from %s", m.num, m.numGetsEachFile, m.bck.String())
		}
	}
	wg := cos.NewLimitedWaitGroup(20, 0)
	for i := range totalGets {
		wg.Add(1)
		go func(idx int) {
			m.get(baseParams, idx, totalGets, getArgs, withValidation)
			wg.Done()
		}(i)
	}
	wg.Wait()
}

func (m *ioContext) getsUntilStop() {
	var (
		idx        = 0
		baseParams = tools.BaseAPIParams()
		wg         = cos.NewLimitedWaitGroup(20, 0)
	)
	for {
		select {
		case <-m.stopCh:
			wg.Wait()
			return
		default:
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				m.get(baseParams, idx, 0, nil /*api.GetArgs*/, false /*validate*/)
			}(idx)
			idx++
			if idx%5000 == 0 {
				time.Sleep(500 * time.Millisecond) // prevents generating too many GET requests
			}
		}
	}
}

func (m *ioContext) stopGets() {
	m.stopCh <- struct{}{}
}

func (m *ioContext) ensureNumCopies(baseParams api.BaseParams, expectedCopies int, greaterOk bool) {
	m.t.Helper()
	time.Sleep(time.Second)
	xargs := xact.ArgsMsg{Kind: apc.ActMakeNCopies, Bck: m.bck, Timeout: tools.RebalanceTimeout}
	_, err := api.WaitForXactionIC(baseParams, &xargs)
	tassert.CheckFatal(m.t, err)

	// List Bucket - primarily for the copies
	msg := &apc.LsoMsg{Flags: apc.LsCached, Prefix: m.prefix}
	msg.AddProps(apc.GetPropsCopies, apc.GetPropsAtime, apc.GetPropsStatus)
	objectList, err := api.ListObjects(baseParams, m.bck, msg, api.ListArgs{})
	tassert.CheckFatal(m.t, err)

	total := 0
	copiesToNumObjects := make(map[int]int)
	for _, entry := range objectList.Entries {
		if entry.Atime == "" {
			m.t.Errorf("%s: access time is empty", m.bck.Cname(entry.Name))
		}
		total++
		if greaterOk && int(entry.Copies) > expectedCopies {
			copiesToNumObjects[expectedCopies]++
		} else {
			copiesToNumObjects[int(entry.Copies)]++
		}
	}
	tlog.Logfln("objects (total, copies) = (%d, %v)", total, copiesToNumObjects)
	if total != m.num {
		m.t.Errorf("list_objects: expecting %d objects, got %d", m.num, total)
	}

	if len(copiesToNumObjects) != 1 {
		s, _ := jsoniter.MarshalIndent(copiesToNumObjects, "", " ")
		m.t.Errorf("some objects do not have expected number of copies: %s", s)
	}

	for copies := range copiesToNumObjects {
		if copies != expectedCopies {
			m.t.Errorf("Expecting %d objects all to have %d replicas, got: %d", total, expectedCopies, copies)
		}
	}
}

func (m *ioContext) nextObjName() string {
	if m.objIdx >= len(m.objNames) {
		m.t.Fatal("not enough objects to get next object name")
		return ""
	}
	objName := m.objNames[m.objIdx]
	m.objIdx++
	return objName
}

func (m *ioContext) ensureNoGetErrors() {
	m.t.Helper()
	if m.numGetErrs.Load() > 0 {
		m.t.Fatalf("Number of get errors is non-zero: %d\n", m.numGetErrs.Load())
	}
}

func (m *ioContext) ensureNumMountpaths(target *meta.Snode, mpList *apc.MountpathList) {
	ensureNumMountpaths(m.t, target, mpList)
}

func ensureNumMountpaths(t *testing.T, target *meta.Snode, mpList *apc.MountpathList) {
	t.Helper()
	tname := target.StringEx()
	baseParams := tools.BaseAPIParams()
	mpl, err := api.GetMountpaths(baseParams, target)
	tassert.CheckFatal(t, err)
	for range 6 {
		if len(mpl.Available) == len(mpList.Available) &&
			len(mpl.Disabled) == len(mpList.Disabled) &&
			len(mpl.WaitingDD) == len(mpList.WaitingDD) {
			break
		}
		time.Sleep(time.Second)
	}
	if len(mpl.Available) != len(mpList.Available) {
		t.Errorf("%s ended up with %d mountpaths (dd=%v, disabled=%v), expecting: %d",
			tname, len(mpl.Available), mpl.WaitingDD, mpl.Disabled, len(mpList.Available))
	} else if len(mpl.Disabled) != len(mpList.Disabled) || len(mpl.WaitingDD) != len(mpList.WaitingDD) {
		t.Errorf("%s ended up with (dd=%v, disabled=%v) mountpaths, expecting (%v and %v), respectively",
			tname, mpl.WaitingDD, mpl.Disabled, mpList.WaitingDD, mpList.Disabled)
	}
}

func ensureNoDisabledMountpaths(t *testing.T, target *meta.Snode, mpList *apc.MountpathList) {
	t.Helper()
	for range 6 {
		if len(mpList.WaitingDD) == 0 && len(mpList.Disabled) == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(mpList.WaitingDD) != 0 || len(mpList.Disabled) != 0 {
		t.Fatalf("%s: disabled mountpaths at the start of the %q (avail=%d, dd=%v, disabled=%v)\n",
			target.StringEx(), t.Name(), len(mpList.Available), mpList.WaitingDD, mpList.Disabled)
	}
}

// background: shuffle=on increases the chance to have still-running rebalance
// at the beginning of a new rename, rebalance, copy-bucket and similar
func ensurePrevRebalanceIsFinished(baseParams api.BaseParams, err error) bool {
	herr, ok := err.(*cmn.ErrHTTP)
	if !ok {
		return false
	}
	// TODO: improve checking for cmn.ErrLimitedCoexistence
	if !strings.Contains(herr.Message, "is currently running,") {
		return false
	}
	tlog.Logln("Warning: wait for unfinished rebalance(?)")
	time.Sleep(5 * time.Second)
	args := xact.ArgsMsg{Kind: apc.ActRebalance, Timeout: tools.RebalanceTimeout}
	_, _ = api.WaitForXactionIC(baseParams, &args)
	time.Sleep(5 * time.Second)
	return true
}

func (m *ioContext) startMaintenanceNoRebalance() *meta.Snode {
	target, _ := m.smap.GetRandTarget()
	tlog.Logfln("Put %s in maintenance", target.StringEx())
	args := &apc.ActValRmNode{DaemonID: target.ID(), SkipRebalance: true}
	_, err := api.StartMaintenance(tools.BaseAPIParams(m.proxyURL), args)
	tassert.CheckFatal(m.t, err)
	m.smap, err = tools.WaitForClusterState(
		m.proxyURL,
		"put target in maintenance",
		m.smap.Version,
		m.smap.CountActivePs(),
		m.smap.CountActiveTs()-1,
	)
	tassert.CheckFatal(m.t, err)
	return target
}

func (m *ioContext) stopMaintenance(target *meta.Snode) string {
	tlog.Logfln("Take %s out of maintenance mode...", target.StringEx())
	bp := tools.BaseAPIParams(m.proxyURL)
	rebID, err := api.StopMaintenance(bp, &apc.ActValRmNode{DaemonID: target.ID()})
	tassert.CheckFatal(m.t, err)
	if rebID == "" {
		return ""
	}
	tassert.Fatalf(m.t, xact.IsValidRebID(rebID), "invalid reb ID %q", rebID)

	xargs := xact.ArgsMsg{ID: rebID, Kind: apc.ActRebalance, Timeout: tools.RebalanceStartTimeout}
	api.WaitForXactionNode(bp, &xargs, xactSnapRunning)

	return rebID
}

func (m *ioContext) setNonDefaultBucketProps() {
	baseParams := tools.BaseAPIParams()
	copies := int64(2)
	props := &cmn.BpropsToSet{
		Mirror: &cmn.MirrorConfToSet{
			Enabled: apc.Ptr(copies > 0),
			Copies:  apc.Ptr[int64](copies),
		},
		Cksum: &cmn.CksumConfToSet{
			Type:            apc.Ptr(cos.ChecksumSHA512),
			EnableReadRange: apc.Ptr(true),
			ValidateWarmGet: apc.Ptr(true),
			ValidateColdGet: apc.Ptr(false),
		},
		Extra: &cmn.ExtraToSet{
			AWS: &cmn.ExtraPropsAWSToSet{CloudRegion: apc.Ptr("us-notheast")},
		},
	}
	_, err := api.SetBucketProps(baseParams, m.bck, props)
	tassert.CheckFatal(m.t, err)
}

func runProviderTests(t *testing.T, f func(*testing.T, *meta.Bck)) {
	tests := []struct {
		name       string
		bck        cmn.Bck
		backendBck cmn.Bck
		skipArgs   tools.SkipTestArgs
		props      *cmn.BpropsToSet
	}{
		{
			name: "local",
			bck:  cmn.Bck{Name: trand.String(10), Provider: apc.AIS},
		},
		{
			name: "remote",
			bck:  cliBck,
			skipArgs: tools.SkipTestArgs{
				Long:      true,
				RemoteBck: true,
			},
		},
		{
			name: "remote_ais",
			bck: cmn.Bck{
				Name:     trand.String(10),
				Provider: apc.AIS, Ns: cmn.Ns{UUID: tools.RemoteCluster.UUID},
			},
			skipArgs: tools.SkipTestArgs{
				RequiresRemoteCluster: true,
				Long:                  true,
			},
		},
		{
			name:       "backend",
			bck:        cmn.Bck{Name: trand.String(10), Provider: apc.AIS},
			backendBck: cliBck,
			skipArgs: tools.SkipTestArgs{
				Long:      true,
				RemoteBck: true,
			},
		},
		{
			name: "local_3_copies",
			bck:  cmn.Bck{Name: trand.String(10), Provider: apc.AIS},
			props: &cmn.BpropsToSet{
				Mirror: &cmn.MirrorConfToSet{
					Enabled: apc.Ptr(true),
					Copies:  apc.Ptr[int64](3),
				},
			},
			skipArgs: tools.SkipTestArgs{Long: true},
		},
		{
			name: "local_ec_2_2",
			bck:  cmn.Bck{Name: trand.String(10), Provider: apc.AIS},
			props: &cmn.BpropsToSet{
				EC: &cmn.ECConfToSet{
					DataSlices:   apc.Ptr(2),
					ParitySlices: apc.Ptr(2),
					ObjSizeLimit: apc.Ptr[int64](0),
				},
			},
			skipArgs: tools.SkipTestArgs{Long: true},
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			if test.backendBck.IsEmpty() {
				test.skipArgs.Bck = test.bck
			} else {
				test.skipArgs.Bck = test.backendBck
				if !test.backendBck.IsCloud() {
					t.Skipf("backend bucket must be a Cloud bucket (have %q)", test.backendBck.String())
				}
			}
			tools.CheckSkip(t, &test.skipArgs)

			baseParams := tools.BaseAPIParams()

			if test.props != nil && test.props.Mirror != nil {
				skip := tools.SkipTestArgs{
					MinMountpaths: int(*test.props.Mirror.Copies),
				}
				tools.CheckSkip(t, &skip)
			}
			if test.props != nil && test.props.EC != nil {
				skip := tools.SkipTestArgs{
					MinTargets: *test.props.EC.DataSlices + *test.props.EC.ParitySlices + 1,
				}
				tools.CheckSkip(t, &skip)
			}

			if test.bck.IsAIS() || test.bck.IsRemoteAIS() {
				err := api.CreateBucket(baseParams, test.bck, test.props)
				tassert.CheckFatal(t, err)

				if !test.backendBck.IsEmpty() {
					tools.SetBackendBck(t, baseParams, test.bck, test.backendBck)
				}
				t.Cleanup(func() {
					api.DestroyBucket(baseParams, test.bck)
				})
			}

			p, err := api.HeadBucket(baseParams, test.bck, false /* don't add */)
			tassert.CheckFatal(t, err)

			bck := meta.CloneBck(&test.bck)
			bck.Props = p

			f(t, bck)
		})
	}
}

func initOnce() {
	proxyURL := tools.GetPrimaryURL()
	primary, err := tools.GetPrimaryProxy(proxyURL)
	if err != nil {
		tlog.Logf("Error: %v", err)
	}
	baseParams := tools.BaseAPIParams(proxyURL)
	cfg, err := api.GetDaemonConfig(baseParams, primary)
	if err != nil {
		tlog.Logf("Error: %v", err)
	}

	config := cmn.GCO.BeginUpdate()
	config.TestFSP.Count = 1
	config.Backend = cfg.Backend
	cmn.GCO.CommitUpdate(config)
}

func initMountpaths(t *testing.T, proxyURL string) {
	t.Helper()
	tools.CheckSkip(t, &tools.SkipTestArgs{RequiredDeployment: tools.ClusterTypeLocal})

	fs.TestNew(nil)
	_onceInit.Do(initOnce)

	baseParams := tools.BaseAPIParams(proxyURL)
	smap := tools.GetClusterMap(t, proxyURL)
	for _, target := range smap.Tmap {
		mpathList, err := api.GetMountpaths(baseParams, target)
		tassert.CheckFatal(t, err)
		ensureNoDisabledMountpaths(t, target, mpathList)

		for _, mpath := range mpathList.Available {
			fs.Add(mpath, target.ID())
		}
	}
}

// NOTE:
// - do not fs.Walk if the bucket's content is chunked; instead GET to temp and return the latter
// - this workaround won't work those tests that corrupt bits
func (m *ioContext) findObjOnDisk(bck cmn.Bck, objName string) (fqn string) {
	//
	// TODO -- FIXME: this is _not_ the only condition indicating a chunked content
	//
	if m.chunksConf != nil && m.chunksConf.multipart {
		return getObjToTemp(m.t, m.proxyURL, bck, objName)
	}

	fsWalkFunc := func(path string, de fs.DirEntry) error {
		if fqn != "" {
			return filepath.SkipDir
		}
		if de.IsDir() {
			return nil
		}

		ct, err := core.NewCTFromFQN(path, nil)
		if err != nil {
			return nil
		}
		if ct.ObjectName() == objName {
			fqn = path
			return errObjectFound
		}
		return nil
	}
	fs.WalkBck(&fs.WalkBckOpts{
		WalkOpts: fs.WalkOpts{
			Bck:      bck,
			CTs:      []string{fs.ObjCT},
			Callback: fsWalkFunc,
			Sorted:   true, // false is unsupported and asserts
		},
	})
	return fqn
}

func (m *ioContext) findObjChunksOnDisk(bck cmn.Bck, objName string) (fqn []string) {
	if m.chunksConf != nil {
		fqn = make([]string, 0, m.chunksConf.numChunks)
	} else {
		fqn = make([]string, 0, 4)
	}
	fsWalkFunc := func(path string, de fs.DirEntry) error {
		if de.IsDir() {
			return nil
		}

		ct, err := core.NewCTFromFQN(path, nil)
		if err != nil {
			return nil
		}
		if strings.HasPrefix(ct.ObjectName(), objName) { // chunk files have the same prefix as the object
			fqn = append(fqn, path)
		}
		return nil
	}
	fs.WalkBck(&fs.WalkBckOpts{
		WalkOpts: fs.WalkOpts{
			Bck:      bck,
			CTs:      []string{fs.ChunkCT},
			Callback: fsWalkFunc,
			Sorted:   true, // false is unsupported and asserts
		},
	})
	return fqn
}

func getObjToTemp(t *testing.T, proxyURL string, bck cmn.Bck, objName string) string {
	t.Helper()
	dir := t.TempDir() // is auto-removed by stdlib
	tmp := filepath.Join(dir, objName)

	err := cos.CreateDir(filepath.Dir(tmp))
	tassert.CheckFatal(t, err)

	f, err := os.Create(tmp)
	tassert.CheckFatal(t, err)
	defer f.Close()

	bp := tools.BaseAPIParams(proxyURL)
	_, err = api.GetObject(bp, bck, objName, &api.GetArgs{Writer: f})
	tassert.CheckFatal(t, err)
	return tmp
}

func corruptSingleBitInFile(m *ioContext, objName string, eced bool) {
	m.t.Helper()

	var (
		fqn string
		b   = []byte{0}
	)

	switch {
	case eced:
		fqn = m.findObjOnDisk(m.bck, objName)
	case m.chunksConf != nil && m.chunksConf.multipart:
		fqns := m.findObjChunksOnDisk(m.bck, objName)
		tassert.Fatalf(m.t, len(fqns) > 0, "no chunks found for %s", objName)
		fqn = fqns[rand.IntN(len(fqns))]
	default:
		fqn = m.findObjOnDisk(m.bck, objName)
	}

	fi, err := os.Stat(fqn)

	tassert.CheckFatal(m.t, err)
	off := rand.Int64N(fi.Size())
	file, err := os.OpenFile(fqn, os.O_RDWR, cos.PermRWR)
	tassert.CheckFatal(m.t, err)

	_, err = file.Seek(off, 0)
	tassert.CheckFatal(m.t, err)

	_, err = file.Read(b)
	tassert.CheckFatal(m.t, err)

	bit := rand.IntN(8)
	b[0] ^= 1 << bit
	_, err = file.Seek(off, 0)
	tassert.CheckFatal(m.t, err)

	_, err = file.Write(b)
	tassert.CheckFatal(m.t, err)

	file.Close()
}

func detectNewBucket(oldList, newList cmn.Bcks) (cmn.Bck, error) {
	for _, nbck := range newList {
		found := false
		for _, obck := range oldList {
			if obck.Name == nbck.Name {
				found = true
				break
			}
		}
		if !found {
			return nbck, nil
		}
	}
	return cmn.Bck{}, fmt.Errorf("new bucket is not found (old: %v, new: %v)", oldList, newList)
}

// xaction is running
func xactSnapRunning(snaps xact.MultiSnap) (running, resetProbeFreq bool) {
	tid, _, err := snaps.RunningTarget("")
	debug.AssertNoErr(err)
	running = tid != ""
	resetProbeFreq = !running // e.g. idle
	return
}

// finished = did start in the past (use check above to confirm) and currently not running
func xactSnapNotRunning(snaps xact.MultiSnap) (bool, bool) {
	running, resetProbeFreq := xactSnapRunning(snaps)
	return !running, resetProbeFreq
}
