// Package stats provides methods and functionality to register, track, log,
// and export metrics that, for the most part, include "counter" and "latency" kinds.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package stats

import (
	"net/http"
	"strings"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/fs"
)

// enum: `statsValue` kinds
const (
	// prometheus and statsd counters
	// error counters must have "err_" prefix (see `errPrefix`)

	KindCounter = "counter"
	KindTotal   = "total"
	KindSize    = "size"

	// prometheus and statsd gauges

	KindSpecial = "special" // uptime

	KindGauge              = "gauge"  // disk I/O
	KindComputedThroughput = "compbw" // disk read/write throughput

	KindLatency    = "latency" // computed internally over 'periodic.stats_time' (milliseconds)
	KindThroughput = "bw"      // ditto (MB/s)
)

// static labels
const (
	ConstlabNode = "node_id"
)

// variable labels
const (
	VlabBucket    = "bucket"
	VlabXkind     = "xkind"
	VlabMountpath = "mountpath"
)

type (
	Tracker interface {
		cos.StatsUpdater

		StartedUp() bool

		PromHandler() http.Handler

		Inc(metric string)
		IncWith(metric string, vlabs map[string]string)
		IncBck(name string, bck *cmn.Bck)

		GetStats() *Node

		ResetStats(errorsOnly bool)
		GetMetricNames() cos.StrKVs // (name, kind) pairs

		// for aistore modules, to add their respective metrics
		RegExtMetric(node *meta.Snode, name, kind string, extra *Extra)
	}
)

type (

	// REST API
	Node struct {
		Snode   *meta.Snode `json:"snode"`
		Tracker copyTracker `json:"tracker"`
		Tcdf    fs.Tcdf     `json:"capacity"`
	}
	Cluster struct {
		Proxy  *Node            `json:"proxy"`
		Target map[string]*Node `json:"target"`
	}
	ClusterRaw struct {
		Proxy  *Node           `json:"proxy"`
		Target cos.JSONRawMsgs `json:"target"`
	}

	// (includes stats.Node and more; NOTE: direct API call w/ no proxying)
	NodeStatus struct {
		RebSnap *core.Snap `json:"rebalance_snap,omitempty"`
		// assorted props
		Status         string `json:"status"`
		DeploymentType string `json:"deployment"`
		Version        string `json:"ais_version"`  // major.minor.build
		BuildTime      string `json:"build_time"`   // YYYY-MM-DD HH:MM:SS-TZ
		K8sPodName     string `json:"k8s_pod_name"` // (via ais-k8s/operator `MY_POD` env var)
		Reserved1      string `json:"reserved1,omitempty"`
		Reserved2      string `json:"reserved2,omitempty"`
		Node
		Cluster     cos.NodeStateInfo
		MemCPUInfo  apc.MemCPUInfo `json:"sys_info"`
		SmapVersion int64          `json:"smap_version,string"`
		Reserved3   int64          `json:"reserved3,omitempty"`
		Reserved4   int64          `json:"reserved4,omitempty"`
	}
)

type (
	Extra struct {
		Labels  cos.StrKVs // static or (same) constant
		StrName string
		Help    string
		VarLabs []string // variable labels: {VlabBucket, ...}
	}
)

func IsErrMetric(name string) bool {
	return strings.HasPrefix(name, errPrefix) // e.g., "err.get.n"
}

func IsIOErrMetric(name string) bool {
	return strings.HasPrefix(name, ioErrPrefix) // e.g., "err.io.get.n" (see ioErrNames)
}

//
// name translations, to recompute latency and throughput over client-controlled intervals
// see stats/common for "Naming conventions"
//

// see also base.init() in ais/backend/common
func LatencyToCounter(latName string) string {
	// 1. common
	switch latName {
	case GetLatency, GetRedirLatency, GetLatencyTotal:
		return GetCount
	case PutLatency, PutRedirLatency, PutLatencyTotal:
		return PutCount
	case ETLOfflineLatencyTotal:
		return ETLOfflineCount
	case RatelimGetRetryLatencyTotal:
		return RatelimGetRetryCount
	case RatelimPutRetryLatencyTotal:
		return RatelimPutRetryCount
	case HeadLatencyTotal:
		return HeadCount
	case ListLatency:
		return ListCount
	case AppendLatency:
		return AppendCount
	}
	// 2. filter out
	isget := strings.Contains(latName, "get.")
	if !isget && !strings.Contains(latName, "put.") {
		return ""
	}
	// backend
	if strings.HasSuffix(latName, ".ns.total") {
		for p := range apc.Providers {
			if p == apc.AIS {
				p = apc.RemAIS
			}
			if strings.HasPrefix(latName, p) {
				if isget {
					return p + "." + GetCount
				}
				return p + "." + PutCount
			}
		}
	}
	return ""
}

func SizeToThroughputCount(name, kind string) (string, string) {
	if kind != KindSize {
		return "", ""
	}
	if !strings.HasSuffix(name, ".size") { // see stats/common for "Naming conventions"
		debug.Assert(false, name)
		return "", ""
	}
	root := strings.TrimSuffix(name, ".size")
	return root + ".bps", root + ".n"
}
