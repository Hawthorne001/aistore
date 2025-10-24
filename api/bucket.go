// Package api provides native Go-based API/SDK over HTTP(S).
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/xact/xreg"

	jsoniter "github.com/json-iterator/go"
)

// SetBucketProps sets the properties of a bucket.
// Validation of the properties passed in is performed by AIStore Proxy.
func SetBucketProps(bp BaseParams, bck cmn.Bck, props *cmn.BpropsToSet) (string, error) {
	b := cos.MustMarshal(apc.ActMsg{Action: apc.ActSetBprops, Value: props})
	return patchBprops(bp, bck, b)
}

// ResetBucketProps resets the properties of a bucket to the global configuration.
func ResetBucketProps(bp BaseParams, bck cmn.Bck) (string, error) {
	b := cos.MustMarshal(apc.ActMsg{Action: apc.ActResetBprops})
	return patchBprops(bp, bck, b)
}

func patchBprops(bp BaseParams, bck cmn.Bck, body []byte) (xid string, err error) {
	var (
		path = apc.URLPathBuckets.Join(bck.Name)
		q    = qalloc()
	)
	bp.Method = http.MethodPatch
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = path
		reqParams.Body = body
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}

		bck.SetQuery(q)
		reqParams.Query = q
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}

// HEAD(bucket): apc.HdrBucketProps => cmn.Bprops{} and apc.HdrBucketInfo => BucketInfo{}
//
// Converts the string type fields returned from the HEAD request to their
// corresponding counterparts in the cmn.Bprops struct.
//
// By default, AIStore adds remote buckets to the cluster metadata on the fly.
// Remote bucket that was never accessed before just "shows up" when user performs
// HEAD, PUT, GET, SET-PROPS, and a variety of other operations.
// This is done only once (and after confirming the bucket's existence and accessibility)
// and doesn't require any action from the user.
// Use `dontAddRemote` to override the default behavior: as the name implies, setting
// `dontAddRemote = true` prevents AIS from adding remote bucket to the cluster's metadata.
func HeadBucket(bp BaseParams, bck cmn.Bck, dontAddRemote bool) (p *cmn.Bprops, err error) {
	var (
		hdr    http.Header
		path   = apc.URLPathBuckets.Join(bck.Name)
		q      = qalloc()
		status int
	)
	if dontAddRemote {
		q.Set(apc.QparamDontAddRemote, "true")
	}
	q = bck.AddToQuery(q)

	bp.Method = http.MethodHead
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = path
		reqParams.Query = q
	}
	if hdr, status, err = reqParams.doReqHdr(); err == nil {
		p = &cmn.Bprops{}
		err = jsoniter.Unmarshal([]byte(hdr.Get(apc.HdrBucketProps)), p)
	} else {
		err = hdr2msg(bck, status, err)
	}

	FreeRp(reqParams)
	qfree(q)
	return p, err
}

// fill-in herr message (HEAD response will never contain one)
func hdr2msg(bck cmn.Bck, status int, err error) error {
	herr, ok := err.(*cmn.ErrHTTP)
	if !ok {
		return err
	}
	if status == 0 && herr.Status != 0 { // (connection refused)
		return err
	}

	if !bck.IsQuery() && status == http.StatusNotFound {
		// when message already resulted from (unwrap)
		if herr2 := cmn.Str2HTTPErr(err.Error()); herr2 != nil {
			herr.Message = herr2.Message
		} else {
			quoted := "\"" + bck.Cname("") + "\""
			herr.Message = "bucket " + quoted + " does not exist"
		}
		return herr
	}
	// common
	herr.Message = "http error code '" + http.StatusText(status) + "'"
	if status == http.StatusGone {
		herr.Message += " (removed from the backend)"
	}
	herr.Message += ", bucket "
	if bck.IsQuery() {
		herr.Message += "query "
	}
	quoted := "\"" + bck.Cname("") + "\""
	herr.Message += quoted
	return herr
}

// CreateBucket sends request to create an AIS bucket with the given name and,
// optionally, specific non-default properties (via cmn.BpropsToSet).
//
// See also:
//   - github.com/NVIDIA/aistore/blob/main/docs/bucket.md#default-bucket-properties
//   - cmn.BpropsToSet (cmn/api.go)
//
// Bucket properties can be also changed at any time via SetBucketProps (above).
func CreateBucket(bp BaseParams, bck cmn.Bck, props *cmn.BpropsToSet, dontHeadRemote ...bool) error {
	if err := bck.Validate(); err != nil {
		return err
	}
	q := qalloc()
	if len(dontHeadRemote) > 0 && dontHeadRemote[0] {
		q.Set(apc.QparamDontHeadRemote, "true")
	}
	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActCreateBck, Value: props})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = bck.AddToQuery(q)
	}
	err := reqParams.DoRequest()

	FreeRp(reqParams)
	qfree(q)
	return err
}

// DestroyBucket sends request to remove an AIS bucket with the given name.
func DestroyBucket(bp BaseParams, bck cmn.Bck) error {
	q := qalloc()

	bp.Method = http.MethodDelete
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActDestroyBck})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		bck.SetQuery(q)
		reqParams.Query = q
	}
	err := reqParams.DoRequest()

	FreeRp(reqParams)
	qfree(q)
	return err
}

// CopyBucket copies all or selected content of `bckFrom` into the destination `bckTo`.
//
//   - AIS will create `bckTo` on the fly, but only if it’s an AIS bucket; for 3rd-party
//     backends, the destination bucket must already exist.
//   - Buckets can be copied across different backends, e.g., AIS to/from AWS, GCP, Azure, etc.
//   - Copying into the same destination from multiple sources is allowed.
//
// ETLBucket is similar, but applies a transformation to each object before writing it
// to the destination bucket. Specifically:
//   - Visits all (matching) source objects
//   - Reads and transforms each using the specified ETL (by ID)
//   - Writes the result to `bckTo`
//
// `fltPresence`, if provided, applies only when `bckFrom` is remote (not ais://):
//   * apc.FltExists        - copy all objects, including those not cached locally
//   * apc.FltPresent       - copy only locally available objects (default)
//   * apc.FltExistsOutside - copy only remote objects missing locally
//
// `msg.Prefix`, if specified, filters source objects by prefix (applies to both operations).
//
// `msg.NumWorkers` controls parallelism:
//   *  0 (default) - one worker per mountpath
//   * -1           - serial (single-threaded) execution
//   * >0           - total number of concurrent workers per target node
//
// Returns xaction ID if successful, error otherwise.

func CopyBucket(bp BaseParams, bckFrom, bckTo cmn.Bck, msg *apc.TCBMsg, fltPresence ...int) (string, error) {
	jbody := cos.MustMarshal(apc.ActMsg{Action: apc.ActCopyBck, Value: msg})
	return tcb(bp, bckFrom, bckTo, jbody, fltPresence...)
}

func ETLBucket(bp BaseParams, bckFrom, bckTo cmn.Bck, msg *apc.TCBMsg, fltPresence ...int) (string, error) {
	jbody := cos.MustMarshal(apc.ActMsg{Action: apc.ActETLBck, Value: msg})
	return tcb(bp, bckFrom, bckTo, jbody, fltPresence...)
}

func tcb(bp BaseParams, bckFrom, bckTo cmn.Bck, jbody []byte, fltPresence ...int) (xid string, err error) {
	if err = bckTo.Validate(); err != nil {
		return
	}
	q := qalloc()
	bckFrom.SetQuery(q)
	_ = bckTo.AddUnameToQuery(q, apc.QparamBckTo, "" /*objName*/)
	if len(fltPresence) > 0 {
		q.Set(apc.QparamFltPresence, strconv.Itoa(fltPresence[0]))
	}

	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bckFrom.Name)
		reqParams.Body = jbody
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = q
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}

// RenameBucket renames bckFrom as bckTo.
// Returns xaction ID if successful, an error otherwise.
func RenameBucket(bp BaseParams, bckFrom, bckTo cmn.Bck) (xid string, err error) {
	if err = bckTo.Validate(); err != nil {
		return
	}
	q := qalloc()
	bckFrom.SetQuery(q)
	_ = bckTo.AddUnameToQuery(q, apc.QparamBckTo, "" /*objName*/)

	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bckFrom.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActMoveBck})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = q
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}

// EvictRemoteBucket sends request to evict an entire remote bucket from the AIStore
// - keepMD: evict objects but keep bucket metadata
func EvictRemoteBucket(bp BaseParams, bck cmn.Bck, keepMD bool) error {
	var q url.Values
	if keepMD {
		q = url.Values{apc.QparamKeepRemote: []string{"true"}}
	}

	bp.Method = http.MethodDelete
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActEvictRemoteBck})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = bck.AddToQuery(q)
	}
	err := reqParams.DoRequest()
	FreeRp(reqParams)
	return err
}

func RechunkBucket(bp BaseParams, bck cmn.Bck, objSizeLimit, chunkSize int64, prefix string) (xid string, err error) {
	q := qalloc()
	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActRechunk, Value: &xreg.RechunkArgs{
			Prefix:       prefix,
			ObjSizeLimit: objSizeLimit,
			ChunkSize:    chunkSize,
		}})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		reqParams.Query = bck.AddToQuery(q)
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}

// MakeNCopies starts an extended action (xaction) to bring a given bucket to a
// certain redundancy level (num copies).
// Returns xaction ID if successful, an error otherwise.
func MakeNCopies(bp BaseParams, bck cmn.Bck, copies int) (xid string, err error) {
	q := qalloc()

	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		reqParams.Body = cos.MustMarshal(apc.ActMsg{Action: apc.ActMakeNCopies, Value: copies})
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		bck.SetQuery(q)
		reqParams.Query = q
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}

// Erasure-code entire `bck` bucket at a given `data`:`parity` redundancy.
// The operation requires at least (`data + `parity` + 1) storage targets in the cluster.
// Returns xaction ID if successful, an error otherwise.
func ECEncodeBucket(bp BaseParams, bck cmn.Bck, data, parity int, checkAndRecover bool) (xid string, err error) {
	// Without `string` conversion it makes base64 from []byte in `Body`.
	ecConf := string(cos.MustMarshal(&cmn.ECConfToSet{
		DataSlices:   &data,
		ParitySlices: &parity,
		Enabled:      apc.Ptr(true),
	}))
	q := qalloc()

	bp.Method = http.MethodPost
	reqParams := AllocRp()
	{
		reqParams.BaseParams = bp
		reqParams.Path = apc.URLPathBuckets.Join(bck.Name)
		msg := apc.ActMsg{Action: apc.ActECEncode, Value: ecConf}
		if checkAndRecover {
			msg.Name = apc.ActEcRecover
		}
		reqParams.Body = cos.MustMarshal(msg)
		reqParams.Header = http.Header{cos.HdrContentType: []string{cos.ContentJSON}}
		bck.SetQuery(q)
		reqParams.Query = q
	}
	_, err = reqParams.doReqStr(&xid)

	FreeRp(reqParams)
	qfree(q)
	return xid, err
}
