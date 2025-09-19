// Package tetl provides helpers for ETL.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package tetl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/k8s"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/ext/etl"
	"github.com/NVIDIA/aistore/tools"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/tlog"
	"github.com/NVIDIA/aistore/xact"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	NonExistImage              = "non-exist-image"
	InvalidYaml                = "invalid-yaml"
	PodWithResourcesConstraint = "resources-constraint"

	Tar2TF        = "tar2tf"
	Echo          = "transformer-echo"
	EchoGolang    = "echo-go"
	MD5           = "transformer-md5"
	HashWithArgs  = "hash-with-args"
	Tar2tfFilters = "tar2tf-filters"
	ParquetParser = "parquet-parser"
	tar2tfFilter  = `
{
  "conversions": [
    { "type": "Decode", "ext_name": "png"},
    { "type": "Rotate", "ext_name": "png"}
  ],
  "selections": [
    { "ext_name": "png" },
    { "ext_name": "cls" }
  ]
}
`
)

// invalid pod specs
const (
	nonExistImageSpec = `
apiVersion: v1
kind: Pod
metadata:
  name: non-exist-image
  annotations:
    communication_type: ${COMMUNICATION_TYPE:-"\"hpull://\""}
    wait_timeout: 5m
spec:
  containers:
    - name: server
      image: aistorage/non-exist-image:latest
      imagePullPolicy: IfNotPresent
      ports:
        - name: default
          containerPort: 80
      command: ['/code/server.py', '--listen', '0.0.0.0', '--port', '80']
      readinessProbe:
        httpGet:
          path: /health
          port: default
`
	invalidYamlSpec = `
apiVersion: v1
kind: Pod
metadata
  name: invalid-syntax
spec:
  containers:
    - name: server
      image: aistorage/runtime_python:latest
      ports
        - name: default
          containerPort: 80
`
	podWithResourcesConstraintSpec = `
name: md5-transformer-etl
runtime:
  image: aistorage/transformer_md5:latest
resources:
  requests:
    memory: "%s"
    cpu: "%s"
  limits:
    memory: "%s"
    cpu: "%s"
`
)

var (
	links = map[string]string{
		MD5:           "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/md5/etl_spec.yaml",
		HashWithArgs:  "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/hash_with_args/etl_spec.yaml",
		Tar2TF:        "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/tar2tf/pod.yaml",
		Tar2tfFilters: "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/tar2tf/pod.yaml",
		Echo:          "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/echo/etl_spec.yaml",
		EchoGolang:    "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/go_echo/pod.yaml",
		ParquetParser: "https://raw.githubusercontent.com/NVIDIA/ais-etl/main/transformers/parquet-parser/etl_spec.yaml",
	}

	testSpecs = map[string]string{
		NonExistImage:              nonExistImageSpec,
		InvalidYaml:                invalidYamlSpec,
		PodWithResourcesConstraint: podWithResourcesConstraintSpec,
	}

	client = &http.Client{}
)

var (
	EchoTransform  = func(r io.Reader) io.Reader { return r }
	NumpyTransform = func(_ io.Reader) io.Reader { return bytes.NewReader([]byte("\x00\x00\x01\x00\x02\x00\x03\x00")) }
	MD5Transform   = func(r io.Reader) io.Reader {
		data, _ := io.ReadAll(r)
		return bytes.NewReader([]byte(cos.ChecksumB2S(data, cos.ChecksumMD5)))
	}
)

func validateETLName(name string) error {
	if _, ok := links[name]; !ok {
		return fmt.Errorf("%s is invalid etlName, expected predefined (%s, %s, %s, %s)", name, Echo, Tar2TF, MD5, ParquetParser)
	}
	return nil
}

func GetTransformYaml(etlName string, replaceArgs ...string) ([]byte, error) {
	if spec, ok := testSpecs[etlName]; ok {
		if len(replaceArgs) > 0 {
			args := make([]any, len(replaceArgs))
			for i, v := range replaceArgs {
				args[i] = v
			}
			spec = fmt.Sprintf(spec, args...)
		}
		return []byte(spec), nil
	}
	if err := validateETLName(etlName); err != nil {
		return nil, err
	}

	var (
		resp   *http.Response
		action = "get transform yaml for ETL[" + etlName + "]"
		args   = &cmn.RetryArgs{
			Call: func() (_ int, err error) {
				req, e := http.NewRequestWithContext(context.Background(), http.MethodGet, links[etlName], http.NoBody)
				if e != nil {
					return 0, err
				}
				resp, err = client.Do(req) //nolint:bodyclose // see defer close below
				if resp != nil {
					return resp.StatusCode, err
				}
				return 0, err
			},
			Action:   action,
			SoftErr:  3,
			HardErr:  1,
			IsClient: true,
		}
	)
	// with retry in case github in unavailable for a moment
	_, err := args.Do()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := cos.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(b))
	}

	specStr := os.Expand(string(b), func(v string) string {
		// Hack: Neither os.Expand, nor os.ExpandEnv supports bash env variable default-value
		// syntax. The whole ${VAR:-default} is matched as v.
		if strings.Contains(v, "COMMUNICATION_TYPE") {
			return etl.Hpull
		}
		if strings.Contains(v, "DOCKER_REGISTRY_URL") {
			return "aistore"
		}
		if etlName == Tar2tfFilters {
			if strings.Contains(v, "OPTION_KEY") {
				return "--spec"
			}
			if strings.Contains(v, "OPTION_VALUE") {
				return tar2tfFilter
			}
		}
		return ""
	})

	return []byte(specStr), nil
}

func StopAndDeleteETL(t *testing.T, bp api.BaseParams, etlName string) {
	if t.Failed() {
		tlog.Logln("Fetching logs from ETL containers")
		if logsByTarget, err := api.ETLLogs(bp, etlName); err == nil {
			for _, etlLogs := range logsByTarget {
				tlog.Logln(headETLLogs(etlLogs, 10*cos.KiB))
			}
		} else {
			tlog.Logfln("Error retrieving ETL[%s] logs: %v", etlName, err)
		}
	}
	tlog.Logfln("Stopping ETL[%s]", etlName)

	if err := api.ETLStop(bp, etlName); err != nil {
		tlog.Logfln("Stopping ETL[%s] failed; err %v", etlName, err)
	} else {
		tlog.Logfln("ETL[%s] stopped", etlName)
	}
	err := api.ETLDelete(bp, etlName)
	tassert.CheckFatal(t, err)
}

func headETLLogs(etlLogs etl.Logs, maxLen int) string {
	logs, l := etlLogs.Logs, len(etlLogs.Logs)
	if maxLen < l {
		logs = logs[:maxLen]
	}
	str := fmt.Sprintf("%s logs:\n%s", meta.Tname(etlLogs.TargetID), string(logs))
	if maxLen < l {
		str += fmt.Sprintf("\nand %d bytes more...", l-maxLen)
	}
	return str
}

func WaitForETLAborted(t *testing.T, bp api.BaseParams, etlNames ...string) {
	tlog.Logln("Waiting for all ETLs to abort...")
	var (
		etls         etl.InfoList
		stopDeadline = time.Now().Add(20 * time.Second)
		watchlist    = cos.NewStrSet(etlNames...)
		interval     = 2 * time.Second
		err          error
	)

	for {
		etls, err = api.ETLList(bp)
		tassert.CheckFatal(t, err)

		allAborted := true
		for _, info := range etls {
			if watchlist.Contains(info.Name) && info.Stage != etl.Aborted.String() {
				allAborted = false
				break
			}
		}

		if allAborted {
			tlog.Logln("All ETL containers aborted successfully")
			return
		}

		if time.Now().After(stopDeadline) {
			break
		}

		tlog.Logfln("ETLs %+v not fully aborted, waiting %s...", etls, interval)
		time.Sleep(interval)
	}

	err = fmt.Errorf("expected all ETLs to stop, got %+v still running", etls)
	tassert.CheckFatal(t, err)
}

func WaitForAborted(bp api.BaseParams, xid, kind string, timeout time.Duration) error {
	tlog.Logfln("Waiting for ETL x-%s[%s] to abort...", kind, xid)
	args := xact.ArgsMsg{ID: xid, Kind: kind, Timeout: timeout /* total timeout */}
	status, err := api.WaitForXactionIC(bp, &args)
	if err == nil {
		if !status.Aborted() {
			err = fmt.Errorf("expected ETL x-%s[%s] status to indicate 'abort', got: %+v", kind, xid, status)
		}
		return err
	}
	tlog.Logfln("Aborting ETL x-%s[%s]", kind, xid)
	if abortErr := api.AbortXaction(bp, &args); abortErr != nil {
		tlog.Logfln("Nested error: failed to abort upon api.wait failure: %v", abortErr)
	}
	return err
}

// NOTE: relies on x-kind to choose the waiting method
// TODO -- FIXME: remove and simplify - here and everywhere
func WaitForFinished(bp api.BaseParams, xid, kind string, timeout time.Duration) (err error) {
	tlog.Logfln("Waiting for ETL x-%s[%s] to finish...", kind, xid)
	args := xact.ArgsMsg{ID: xid, Kind: kind, Timeout: timeout /* total timeout */}
	if xact.IdlesBeforeFinishing(kind) {
		err = api.WaitForXactionIdle(bp, &args)
	} else {
		_, err = api.WaitForXactionIC(bp, &args)
	}
	if err == nil {
		return
	}
	tlog.Logfln("error waiting for xaction to finish: %v", err)
	tlog.Logfln("Aborting ETL x-%s[%s]", kind, xid)
	if abortErr := api.AbortXaction(bp, &args); abortErr != nil {
		tlog.Logfln("Nested error: failed to abort upon api.wait failure: %v", abortErr)
	}
	return nil
}

func ReportXactionStatus(bp api.BaseParams, xid string, stopCh *cos.StopCh, interval time.Duration, totalObj int) {
	go func() {
		var (
			xactStart = time.Now()
			etlTicker = time.NewTicker(interval)
		)
		defer etlTicker.Stop()
		for {
			select {
			case <-etlTicker.C:
				// Check number of objects transformed.
				xs, err := api.QueryXactionSnaps(bp, &xact.ArgsMsg{ID: xid})
				if err != nil {
					tlog.Logfln("Failed to get x-etl[%s] stats: %v", xid, err)
					continue
				}
				locObjs, outObjs, inObjs := xs.ObjCounts(xid)
				tlog.Logfln("ETL[%s] progress: (objs=%d, outObjs=%d, inObjs=%d) out of %d objects",
					xid, locObjs, outObjs, inObjs, totalObj)
				locBytes, outBytes, inBytes := xs.ByteCounts(xid)
				bps := float64(locBytes+outBytes) / time.Since(xactStart).Seconds()
				bpsStr := cos.ToSizeIEC(int64(bps), 2) + "/s"
				tlog.Logfln("ETL[%s] progress: (bytes=%d, outBytes=%d, inBytes=%d), %sBps",
					xid, locBytes, outBytes, inBytes, bpsStr)
			case <-stopCh.Listen():
				return
			}
		}
	}()
}

func InitSpec(t *testing.T, bp api.BaseParams, etlName, commType, argType string, replaceArgs ...string) (msg etl.InitMsg) {
	tlog.Logfln("InitSpec ETL[%s], communicator %s", etlName, commType)
	spec, err := GetTransformYaml(etlName, replaceArgs...)
	tassert.CheckFatal(t, err)

	var (
		etlSpec  etl.ETLSpecMsg
		initSpec etl.InitSpecMsg
	)
	etlName += strings.ReplaceAll(strings.ToLower(cos.GenUUID()), "_", "-") // add random suffix to avoid conflicts
	if err := yaml.Unmarshal(spec, &etlSpec); err == nil && etlSpec.Validate() == nil {
		etlSpec.EtlName = etlName
		etlSpec.CommTypeX = commType
		etlSpec.ArgTypeX = argType
		etlSpec.InitTimeout = cos.Duration(time.Minute * 2) // manually increase timeout in testing environment
		msg = &etlSpec
	} else {
		initSpec.EtlName = etlName
		initSpec.CommTypeX = commType
		initSpec.ArgTypeX = argType
		initSpec.InitTimeout = cos.Duration(time.Minute * 2) // manually increase timeout in testing environment
		initSpec.Spec = spec
		msg = &initSpec
	}

	tassert.Fatalf(t, msg.Name() == etlName, "%q vs %q", msg.Name(), etlName) // assert

	xid, err := api.ETLInit(bp, msg)
	if herr, ok := err.(*cmn.ErrHTTP); ok && herr.TypeCode == "ErrUnsupp" && msg.CommType() == etl.WebSocket {
		t.Skip("skipping, WebSocket only work with direct put supported transformers")
	}
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, cos.IsValidUUID(xid), "expected valid xaction ID, got %q", xid)
	// reread `InitMsg` and compare with the specified
	details, err := api.ETLGetDetail(bp, etlName, "")
	tassert.CheckFatal(t, err)

	tlog.Logfln("ETL %q: running x-etl-spec[%s]", etlName, xid)

	tassert.Errorf(t, details.InitMsg.Name() == etlName, "expected etlName %s, got %s", etlName, details.InitMsg.Name())
	tassert.Errorf(t, details.InitMsg.CommType() == commType, "expected communicator type %s, got %s", commType, details.InitMsg.CommType())

	if initSpec, ok := details.InitMsg.(*etl.InitSpecMsg); ok {
		tassert.Errorf(t, bytes.Equal(spec, initSpec.Spec), "pod specs differ, expected %s, got %s", string(spec), string(initSpec.Spec))
	}

	return msg
}

func InspectPod(t *testing.T, podName string) corev1.Pod {
	client, err := k8s.InitTestClient(tools.DefaultNamespace)
	tassert.CheckFatal(t, err)
	pod, err := client.Pod(podName)
	tassert.CheckFatal(t, err)
	return *pod
}

func ETLBucketWithCleanup(t *testing.T, bp api.BaseParams, bckFrom, bckTo cmn.Bck, msg *apc.TCBMsg) string {
	xid, err := api.ETLBucket(bp, bckFrom, bckTo, msg)
	tassert.CheckFatal(t, err)

	t.Cleanup(func() {
		tools.DestroyBucket(t, bp.URL, bckTo)
	})

	tlog.Logfln("ETL[%s]: running %s => %s xaction %q",
		msg.Transform.Name, bckFrom.Cname(""), bckTo.Cname(""), xid)
	return xid
}

func ETLBucketWithCmp(t *testing.T, bp api.BaseParams, bckFrom, bckTo cmn.Bck, msg *apc.TCBMsg, cmp func(r1, r2 io.Reader) bool) {
	xid := ETLBucketWithCleanup(t, bp, bckFrom, bckTo, msg)
	err := WaitForFinished(bp, xid, apc.ActETLBck, 3*time.Minute)
	tassert.CheckFatal(t, err)

	tlog.Logfln("ETL[%s]: comparing buckets, %s vs %s", msg.Transform.Name, bckFrom.Cname(""), bckTo.Cname(""))

	objeList, err := api.ListObjects(bp, bckFrom, &apc.LsoMsg{}, api.ListArgs{})
	tassert.CheckFatal(t, err)
	for _, en := range objeList.Entries {
		r1, _, err := api.GetObjectReader(bp, bckFrom, en.Name, &api.GetArgs{})
		tassert.CheckFatal(t, err)
		r2, _, err := api.GetObjectReader(bp, bckTo, en.Name, &api.GetArgs{})
		tassert.CheckFatal(t, err)
		tassert.Fatalf(t, cmp(r1, r2), "object content mismatch: %s vs %s", bckFrom.Cname(en.Name), bckTo.Cname(en.Name))
		tassert.CheckFatal(t, r1.Close())
		tassert.CheckFatal(t, r2.Close())
	}
}

func ETLCheckStage(t *testing.T, params api.BaseParams, etlName string, stage etl.Stage) {
	etls, err := api.ETLList(params)
	tassert.CheckFatal(t, err)
	for _, inst := range etls {
		if etlName == inst.Name && inst.Stage == stage.String() {
			return
		}
	}
	t.Fatalf("etl[%s] doesn't exist or isn't in status %s (%v)", etlName, stage.String(), etls)
}

func CheckNoRunningETLContainers(t *testing.T, params api.BaseParams) {
	etls, err := api.ETLList(params)
	tassert.CheckFatal(t, err)
	for _, info := range etls {
		tassert.Fatalf(t, info.Stage == etl.Aborted.String(), "expected no running ETL containers, got %s in stage %s", info.Name, info.Stage)
	}
}

func SpecToInitMsg(spec []byte /*yaml*/) (*etl.InitSpecMsg, error) {
	errCtx := &cmn.ETLErrCtx{}
	msg := &etl.InitSpecMsg{Spec: spec}
	pod, err := msg.ParsePodSpec()
	if err != nil {
		return msg, cmn.NewErrETLf(errCtx, "failed to parse pod spec: %v\n%q", err, string(msg.Spec))
	}
	errCtx.ETLName = pod.GetName()
	msg.EtlName = pod.GetName()

	if err := k8s.ValidateEtlName(msg.EtlName); err != nil {
		return msg, err
	}
	// Check annotations.
	msg.CommTypeX = podTransformCommType(pod)
	if msg.InitTimeout, err = podTransformTimeout(errCtx, pod); err != nil {
		return msg, err
	}

	err = msg.Validate()
	return msg, err
}

func podTransformCommType(pod *corev1.Pod) string {
	if pod.Annotations == nil || pod.Annotations[etl.CommTypeAnnotation] == "" {
		// By default assume `Hpush`.
		return etl.Hpush
	}
	return pod.Annotations[etl.CommTypeAnnotation]
}

func podTransformTimeout(errCtx *cmn.ETLErrCtx, pod *corev1.Pod) (cos.Duration, error) {
	if pod.Annotations == nil || pod.Annotations[etl.WaitTimeoutAnnotation] == "" {
		return 0, nil
	}

	v, err := time.ParseDuration(pod.Annotations[etl.WaitTimeoutAnnotation])
	if err != nil {
		return cos.Duration(v), cmn.NewErrETL(errCtx, err.Error()).WithPodName(pod.Name)
	}
	return cos.Duration(v), nil
}

func ListObjectsWithRetry(bp api.BaseParams, bckTo cmn.Bck, prefix string, expectedCount int, opts tools.WaitRetryOpts) (err error) {
	var (
		retries       = opts.MaxRetries
		retryInterval = opts.Interval
		i             int
	)
retry:
	list, err := api.ListObjects(bp, bckTo, &apc.LsoMsg{Prefix: prefix}, api.ListArgs{})
	if err == nil && len(list.Entries) == expectedCount {
		return nil
	}
	if !cmn.IsStatusServiceUnavailable(err) && !cos.IsRetriableConnErr(err) {
		return
	}
	time.Sleep(retryInterval)
	i++
	if i > retries {
		return fmt.Errorf("api.ListObjects max retries (%d) exceeded, expected %d objects, got %d", retries, expectedCount, len(list.Entries))
	}
	goto retry
}
