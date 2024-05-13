// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/NVIDIA/aistore/ais/s3"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// RESTful API: datapath query parameters
type dpq struct {
	bck struct {
		provider, namespace string // bucket
	}
	apnd struct {
		ty, hdl string // QparamAppendType, QparamAppendHandle
	}
	arch struct {
		path, mime, regx, mmode string // QparamArchpath et al. (plus archmode below)
	}

	ptime       string // req timestamp at calling/redirecting proxy (QparamUnixTime)
	uuid        string // xaction
	origURL     string // ht://url->
	owt         string // object write transaction { OwtPut, ... }
	fltPresence string // QparamFltPresence
	etlName     string // QparamETLName
	binfo       string // bucket info, with or without requirement to summarize remote obj-s

	skipVC        bool // QparamSkipVC (skip loading existing object's metadata)
	isGFN         bool // QparamIsGFNRequest
	dontAddRemote bool // QparamDontAddRemote
	silent        bool // QparamSilent
	latestVer     bool // QparamLatestVer
	isS3          bool // special use: frontend S3 API
}

var (
	dpqPool sync.Pool
	dpq0    dpq
)

func dpqAlloc() *dpq {
	if v := dpqPool.Get(); v != nil {
		return v.(*dpq)
	}
	return &dpq{}
}

func dpqFree(dpq *dpq) {
	*dpq = dpq0
	dpqPool.Put(dpq)
}

// Data Path Query structure (dpq):
// Parse URL query for a selected few parameters used in the datapath.
// (This is a faster alternative to the conventional and RFC-compliant URL.Query()
// to be used narrowly to handle those few (keys) and nothing else.)
func (dpq *dpq) parse(rawQuery string) (err error) {
	query := rawQuery
	for query != "" {
		key, value := query, ""
		if i := strings.IndexByte(key, '&'); i >= 0 {
			key, query = key[:i], key[i+1:]
		} else {
			query = ""
		}
		if k, v, ok := keyEQval(key); ok {
			key, value = k, v
		}
		// supported URL query parameters explicitly named below; attempt to parse anything
		// outside this list will fail
		switch key {
		case apc.QparamProvider:
			dpq.bck.provider = value
		case apc.QparamNamespace:
			if dpq.bck.namespace, err = url.QueryUnescape(value); err != nil {
				return
			}
		case apc.QparamSkipVC:
			dpq.skipVC = cos.IsParseBool(value)
		case apc.QparamUnixTime:
			dpq.ptime = value
		case apc.QparamUUID:
			dpq.uuid = value
		case apc.QparamArchpath, apc.QparamArchmime, apc.QparamArchregx, apc.QparamArchmode:
			if err = dpq._arch(key, value); err != nil {
				return
			}
		case apc.QparamIsGFNRequest:
			dpq.isGFN = cos.IsParseBool(value)
		case apc.QparamOrigURL:
			if dpq.origURL, err = url.QueryUnescape(value); err != nil {
				return
			}
		case apc.QparamAppendType:
			dpq.apnd.ty = value
		case apc.QparamAppendHandle:
			if dpq.apnd.hdl, err = url.QueryUnescape(value); err != nil {
				return
			}
		case apc.QparamOWT:
			dpq.owt = value

		case apc.QparamFltPresence:
			dpq.fltPresence = value
		case apc.QparamDontAddRemote:
			dpq.dontAddRemote = cos.IsParseBool(value)
		case apc.QparamBinfoWithOrWithoutRemote:
			dpq.binfo = value

		case apc.QparamETLName:
			dpq.etlName = value
		case apc.QparamSilent:
			dpq.silent = cos.IsParseBool(value)
		case apc.QparamLatestVer:
			dpq.latestVer = cos.IsParseBool(value)

		default:
			debug.Func(func() {
				switch key {
				// not used yet
				case apc.QparamProxyID, apc.QparamDontHeadRemote:

				// flows that utilize these particular keys perform conventional
				// `r.URL.Query()` parsing
				case s3.QparamMptUploadID, s3.QparamMptUploads, s3.QparamMptPartNo,
					s3.QparamAccessKeyID, s3.QparamExpires, s3.QparamSignature,
					s3.HeaderAlgorithm, s3.HeaderCredentials, s3.HeaderDate,
					s3.HeaderExpires, s3.HeaderSignedHeaders, s3.HeaderSignature, s3.QparamXID:

				default:
					err = fmt.Errorf("failed to fast-parse [%s], unknown key: %q", rawQuery, key)
					debug.AssertNoErr(err)
				}
			})
		}
	}
	return
}

func (dpq *dpq) _arch(key, val string) (err error) {
	switch key {
	case apc.QparamArchpath:
		dpq.arch.path, err = url.QueryUnescape(val)
	case apc.QparamArchmime:
		dpq.arch.mime, err = url.QueryUnescape(val)
	case apc.QparamArchregx:
		dpq.arch.regx, err = url.QueryUnescape(val)
	case apc.QparamArchmode:
		dpq.arch.mmode, err = archive.ValidateMatchMode(val)
	}
	if err != nil {
		return err
	}
	// either/or
	if dpq.arch.path != "" && dpq.arch.mmode != "" { // (empty regx is fine, is EmptyMatchAny)
		err = fmt.Errorf("query parameters archpath=%q (match one) and archregx=%q (match many) are mutually exclusive",
			apc.QparamArchpath, apc.QparamArchregx)
	}
	return err
}

func (dpq *dpq) _archstr() string {
	if dpq.arch.path != "" {
		return "\"" + dpq.arch.path + "\""
	}
	return fmt.Sprintf("(archregx=%q, archmode=%q)", dpq.arch.regx, dpq.arch.mmode)
}

func keyEQval(s string) (string, string, bool) {
	if i := strings.IndexByte(s, '='); i > 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}
