// Package cli provides easy-to-use commands to manage, monitor, and utilize AIS clusters.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/xact"

	jsoniter "github.com/json-iterator/go"
	"github.com/urfave/cli"
)

//
// copy -----------------------------------------------------------------------------
//

// via (I) x-copy-bucket ("full bucket") _or_ (II) x-copy-listrange ("multi-object")
// Notice a certain usable redundancy:
//
//	(I) `ais cp from to --prefix abc" is the same as (II) `ais cp from to --template abc"
//
// Also, note [CONVENTIONS] below.
//
//	(III) `ais cp from-bucket/from-object to-bucket[/to-object]`
func copyBucketHandler(c *cli.Context) (err error) {
	var (
		bckFrom, bckTo cmn.Bck
		objFrom, objTo string
	)
	switch {
	case c.NArg() == 0:
		err = missingArgumentsError(c, c.Command.ArgsUsage)
	case c.NArg() == 1:
		bckFrom, objFrom, err = parseBckObjURI(c, c.Args().Get(0), true /*emptyObjnameOK*/)
	default:
		bckFrom, bckTo, objFrom, objTo, err = parseFromToURIs(c, bucketSrcArgument, bucketDstArgument, 0 /*shift*/, true, true /*optional src, dst oname*/)
	}
	if err != nil {
		return err
	}

	// [CONVENTIONS]
	// 1. '--sync' and '--latest' both require aistore to reach out for remote metadata and, therefore,
	//   if destination is omitted both options imply namesake in-cluster destination (ie., bckTo = bckFrom)
	// 2. in addition, '--sync' also implies '--all' (will traverse non-present)

	if bckTo.IsEmpty() {
		if !flagIsSet(c, syncFlag) && !flagIsSet(c, latestVerFlag) {
			var hint string
			if bckFrom.IsRemote() {
				hint = fmt.Sprintf(" (or, did you mean 'ais cp %s %s' or 'ais cp %s %s'?)",
					c.Args().Get(0), flprn(syncFlag), c.Args().Get(0), flprn(latestVerFlag))
			}
			return incorrectUsageMsg(c, "missing destination bucket%s", hint)
		}
		bckTo = bckFrom
	}

	if objTo != "" {
		if objFrom == "" {
			return fmt.Errorf("missing source object name: cannot copy from bucket (%s) to object (%s)", bckFrom.Cname(""), bckTo.Cname(objTo))
		}
		err := copyObject(c, bckFrom, objFrom, bckTo, objTo)
		if err != nil {
			if cos.IsErrNotFound(err) && strings.Contains(err.Error(), bckFrom.Cname(objFrom)) {
				err = fmt.Errorf("source object %q not found (did you mean to copy multiple objects with prefix %q?)", bckFrom.Cname(objFrom), objFrom)
			}
		}
		return err
	}

	// NOTE: copyAllObjsFlag forces 'x-list' to list the remote one, and vice versa
	return copyTransform(c, "" /*etlName*/, objFrom, bckFrom, bckTo)
}

//
// main function: (cp | etl) & (bucket | multi-object)
//

func copyTransform(c *cli.Context, etlName, objNameOrTmpl string, bckFrom, bckTo cmn.Bck) (err error) {
	text1, text2 := "copy", "Copying"
	if etlName != "" {
		text1, text2 = "transform", "Transforming"
	}

	// HEAD(from)
	if bckFrom.Props, err = headBucket(bckFrom, true /* don't add */); err != nil {
		return err
	}

	allIncludingRemote := flagIsSet(c, copyAllObjsFlag)
	empty, err := isBucketEmpty(bckFrom, !bckFrom.IsRemote() || !allIncludingRemote /*cached*/)
	debug.AssertNoErr(err)
	if empty {
		if bckFrom.IsRemote() && !allIncludingRemote {
			hint := "(tip: use option %s to " + text1 + " remote objects from the backend store)\n"
			note := fmt.Sprintf("source %s appears to be empty "+hint, bckFrom.String(), qflprn(copyAllObjsFlag))
			actionNote(c, note)
			return nil
		}
		note := fmt.Sprintf("source %s is empty, nothing to do\n", bckFrom.String())
		actionNote(c, note)
		return nil
	}

	oltp, err := dopOLTP(c, bckFrom, objNameOrTmpl)
	if err != nil {
		return err
	}

	//
	// (0) single object (see related: lsObjVsPref)
	//
	if oltp.objName != "" {
		debug.Assertf(oltp.list == "" && oltp.tmpl == "", "%+v", oltp)
		return copyObject(c, bckFrom, oltp.objName, bckTo, "")
	}

	// bck-to exists?
	if _, err = api.HeadBucket(apiBP, bckTo, true /* don't add */); err != nil {
		if herr, ok := err.(*cmn.ErrHTTP); !ok || herr.Status != http.StatusNotFound {
			return err
		}
		warn := fmt.Sprintf("destination %s doesn't exist and will be created with configuration copied from the source (%s))",
			bckTo.Cname(""), bckFrom.Cname(""))
		actionWarn(c, warn)
	}

	dryRun := flagIsSet(c, copyDryRunFlag)

	//
	// (1) copy/transform bucket (x-tcb)
	//
	if oltp.objName == "" && oltp.list == "" && oltp.tmpl == "" {
		// NOTE: e.g. 'ais cp gs://abc gs:/abc' to sync remote bucket => aistore
		if bckFrom.Equal(&bckTo) && !bckFrom.IsRemote() {
			return incorrectUsageMsg(c, errFmtSameBucket, commandCopy, bckTo.Cname(""))
		}
		if dryRun {
			// TODO: show object names with destinations, make the output consistent with etl dry-run
			dryRunCptn(c)
			actionDone(c, text2+" the entire bucket")
		}
		if etlName != "" {
			return etlBucket(c, etlName, bckFrom, bckTo)
		}
		return copyBucket(c, bckFrom, bckTo)
	}

	//
	// (2) multi-object x-tco
	//
	if oltp.list == "" && oltp.tmpl == "" {
		oltp.list = oltp.objName // (compare with `_prefetchOne`)
	}
	if dryRun {
		var prompt string
		if oltp.list != "" {
			prompt = fmt.Sprintf("%s %q ...\n", text2, oltp.list)
		} else {
			prompt = fmt.Sprintf("%s objects that match the pattern %q ...\n", text2, oltp.tmpl)
		}
		dryRunCptn(c) // TODO: ditto
		actionDone(c, prompt)
	}
	return runTCO(c, bckFrom, bckTo, oltp.list, oltp.tmpl, etlName)
}

func _iniTCBMsg(c *cli.Context, msg *apc.TCBMsg) error {
	// CopyBckMsg part
	{
		msg.Prepend = parseStrFlag(c, copyPrependFlag)
		msg.Prefix = parseStrFlag(c, verbObjPrefixFlag)
		msg.DryRun = flagIsSet(c, copyDryRunFlag)
		msg.Force = flagIsSet(c, forceFlag)
		msg.LatestVer = flagIsSet(c, latestVerFlag)
		msg.Sync = flagIsSet(c, syncFlag)
		msg.NonRecurs = flagIsSet(c, nonRecursFlag)
	}
	if msg.Sync && msg.Prepend != "" {
		return fmt.Errorf("prepend option (%q) is incompatible with %s (the latter requires identical source/destination naming)",
			msg.Prepend, qflprn(progressFlag))
	}

	// TCBMsg
	msg.ContinueOnError = flagIsSet(c, continueOnErrorFlag)
	if flagIsSet(c, numWorkersFlag) {
		msg.NumWorkers = parseIntFlag(c, numWorkersFlag)
	}
	return nil
}

func copyBucket(c *cli.Context, bckFrom, bckTo cmn.Bck) error {
	var (
		msg          apc.TCBMsg
		showProgress = flagIsSet(c, progressFlag)
		from, to     = bckFrom.Cname(""), bckTo.Cname("")
	)
	if showProgress && flagIsSet(c, copyDryRunFlag) {
		warn := fmt.Sprintf("dry-run option is incompatible with %s - "+NIY, qflprn(progressFlag))
		actionWarn(c, warn)
		showProgress = false
	}
	// copy: with/wo progress/wait
	if err := _iniTCBMsg(c, &msg); err != nil {
		return err
	}

	// by default, copying in-cluster objects, with an option to copy remote as well (TODO: FltExistsOutside)
	fltPresence := apc.FltPresent
	if flagIsSet(c, copyAllObjsFlag) || flagIsSet(c, etlAllObjsFlag) {
		fltPresence = apc.FltExists
	}

	if showProgress {
		var cpr cprCtx
		_, cpr.xname = xact.GetKindName(apc.ActCopyBck)
		cpr.from, cpr.to = bckFrom.Cname(""), bckTo.Cname("")
		return cpr.copyBucket(c, bckFrom, bckTo, &msg, fltPresence)
	}

	if flagIsSet(c, copyAllObjsFlag) && (bckFrom.Provider != apc.AIS || !bckFrom.Ns.IsGlobal()) {
		const s = "copying remote (ie, not in-cluster) objects may take considerable time"
		warn := fmt.Sprintf("%s (tip: use %s to show progress, '--help' for details)", s, qflprn(progressFlag))
		actionWarn(c, warn)
	}

	xid, err := api.CopyBucket(apiBP, bckFrom, bckTo, &msg, fltPresence)
	if err != nil {
		return V(err)
	}
	// NOTE: may've transitioned TCB => TCO
	kind := apc.ActCopyBck
	if !apc.IsFltPresent(fltPresence) {
		kind, _, err = getKindNameForID(xid, kind)
		if err != nil {
			return err
		}
	}

	if !flagIsSet(c, waitFlag) && !flagIsSet(c, waitJobXactFinishedFlag) {
		/// TODO: unify vs e2e: ("%s[%s] %s => %s", kind, xid, from, to)
		if flagIsSet(c, nonverboseFlag) {
			fmt.Fprintln(c.App.Writer, xid)
		} else {
			actionDone(c, tcbtcoCptn("Copying", bckFrom, bckTo)+". "+toMonitorMsg(c, xid, ""))
		}
		return nil
	}

	// wait
	var timeout time.Duration
	if flagIsSet(c, waitJobXactFinishedFlag) {
		timeout = parseDurationFlag(c, waitJobXactFinishedFlag)
	}
	fmt.Fprint(c.App.Writer, tcbtcoCptn("Copying", bckFrom, bckTo)+" ...")
	xargs := xact.ArgsMsg{ID: xid, Kind: kind, Timeout: timeout}
	if err := waitXact(&xargs); err != nil {
		fmt.Fprintf(c.App.ErrWriter, fmtXactFailed, "copy", from, to)
		return err
	}
	actionDone(c, fmtXactSucceeded)
	return nil
}

func tcbtcoCptn(action string, bckFrom, bckTo cmn.Bck) string {
	from, to := bckFrom.Cname(""), bckTo.Cname("")
	if bckFrom.Equal(&bckTo) {
		return fmt.Sprintf("%s %s", action, from)
	}
	return fmt.Sprintf("%s %s => %s", action, from, to)
}

//
// etl -------------------------------------------------------------------------------
//

func etlBucketHandler(c *cli.Context) error {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	}
	etlNameOrPipeline := c.Args().Get(0)
	bckFrom, bckTo, objFrom, _, err := parseFromToURIs(c, bucketSrcArgument, bucketDstArgument, 1 /*shift*/, true, false /*optional src, dst oname*/)

	if err != nil {
		return err
	}
	return copyTransform(c, etlNameOrPipeline, objFrom, bckFrom, bckTo)
}

func etlBucket(c *cli.Context, etlNameOrPipeline string, bckFrom, bckTo cmn.Bck) error {
	// Parse pipeline or single ETL name
	var transform apc.Transform
	etlNames, err := parseETLNames(etlNameOrPipeline)
	if err != nil {
		return err
	}
	transform = apc.Transform{
		Name: etlNames[0], // First ETL in the pipeline
	}
	if len(etlNames) > 1 {
		transform.Pipeline = etlNames[1:] // Only populate pipeline if more than one ETL
	}

	var msg = apc.TCBMsg{
		Transform: transform,
	}
	if err := _iniTCBMsg(c, &msg); err != nil {
		return err
	}
	if flagIsSet(c, etlExtFlag) {
		mapStr := parseStrFlag(c, etlExtFlag)
		extMap := make(cos.StrKVs, 1)
		err := jsoniter.UnmarshalFromString(mapStr, &extMap)
		if err != nil {
			// add quotation marks and reparse
			tmp := strings.ReplaceAll(mapStr, " ", "")
			tmp = strings.ReplaceAll(tmp, "{", "{\"")
			tmp = strings.ReplaceAll(tmp, "}", "\"}")
			tmp = strings.ReplaceAll(tmp, ":", "\":\"")
			tmp = strings.ReplaceAll(tmp, ",", "\",\"")
			if jsoniter.UnmarshalFromString(tmp, &extMap) == nil {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("invalid format --%s=%q. Usage examples: {jpg:txt}, \"{in1:out1,in2:out2}\"",
				etlExtFlag.GetName(), mapStr)
		}
		msg.Ext = extMap
	}

	// by default, copying objects in the cluster, with an option to override
	// TODO: FltExistsOutside maybe later
	fltPresence := apc.FltPresent
	if flagIsSet(c, copyAllObjsFlag) || flagIsSet(c, etlAllObjsFlag) {
		fltPresence = apc.FltExists
	}

	xid, err := api.ETLBucket(apiBP, bckFrom, bckTo, &msg, fltPresence)
	if errV := handleETLHTTPError(err, transform.Name); errV != nil {
		return errV
	}

	_, xname := xact.GetKindName(apc.ActETLBck)
	text := fmt.Sprintf("%s %s => %s", xact.Cname(xname, xid), bckFrom.String(), bckTo.String())
	if !flagIsSet(c, waitFlag) && !flagIsSet(c, waitJobXactFinishedFlag) {
		fmt.Fprintln(c.App.Writer, text)
		return nil
	}

	// wait
	var timeout time.Duration
	if flagIsSet(c, waitJobXactFinishedFlag) {
		timeout = parseDurationFlag(c, waitJobXactFinishedFlag)
	}
	fmt.Fprintln(c.App.Writer, text+" ...")
	xargs := xact.ArgsMsg{ID: xid, Kind: apc.ActETLBck, Timeout: timeout}
	if err := waitXact(&xargs); err != nil {
		return err
	}
	if !flagIsSet(c, copyDryRunFlag) {
		return nil
	}

	// [DRY-RUN]
	snaps, err := api.QueryXactionSnaps(apiBP, &xargs)
	if err != nil {
		return V(err)
	}
	dryRunCptn(c)
	locObjs, outObjs, inObjs := snaps.ObjCounts(xid)
	fmt.Fprintf(c.App.Writer, "ETL object counts:\t transformed=%d, sent=%d, received=%d", locObjs, outObjs, inObjs)
	locBytes, outBytes, inBytes := snaps.ByteCounts(xid)
	fmt.Fprintf(c.App.Writer, "ETL byte stats:\t transformed=%d, sent=%d, received=%d", locBytes, outBytes, inBytes)
	return nil
}
