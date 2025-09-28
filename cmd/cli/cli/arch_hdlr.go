// Package cli provides easy-to-use commands to manage, monitor, and utilize AIS clusters.
// This file handles CLI commands that pertain to AIS objects.
/*
 * Copyright (c) 2021-2025, NVIDIA CORPORATION. All rights reserved.
 */
package cli

import (
	"archive/tar"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/memsys"

	"github.com/urfave/cli"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
	"golang.org/x/sync/errgroup"
)

// See also bucket_hdlr.go for "Multi-object Rule of Convenience"

const archBucketUsage = "Archive selected or matching objects from " + bucketObjectSrcArgument + " as\n" +
	indent1 + archExts + "-formatted object (a.k.a. \"shard\"):\n" +
	indent1 + "\t- 'ais archive bucket ais://src gs://dst/a.tar.lz4 --template \"trunk-{001..997}\"'\t- archive (prefix+range) matching objects from ais://src;\n" +
	indent1 + "\t- 'ais archive bucket \"ais://src/trunk-{001..997}\" gs://dst/a.tar.lz4'\t- same as above (notice double quotes);\n" +
	indent1 + "\t- 'ais archive bucket \"ais://src/trunk-{998..999}\" gs://dst/a.tar.lz4 --append-or-put'\t- add two more objects to an existing shard;\n" +
	indent1 + "\t- 'ais archive bucket s3://src/trunk-00 ais://dst/b.tar'\t- archive \"trunk-00\" prefixed objects from an s3 bucket as a given TAR destination"

const archPutUsage = "Archive a file, a directory, or multiple files and/or directories as\n" +
	indent1 + "\t" + archExts + "-formatted object - aka \"shard\".\n" +
	indent1 + "\tBoth APPEND (to an existing shard) and PUT (a new version of the shard) are supported.\n" +
	indent1 + "\tExamples:\n" +
	indent1 + "\t- 'local-file s3://q/shard-00123.tar.lz4 --append --archpath name-in-archive' - append file to a given shard,\n" +
	indent1 + "\t   optionally, rename it (inside archive) as specified;\n" +
	indent1 + "\t- 'local-file s3://q/shard-00123.tar.lz4 --append-or-put --archpath name-in-archive' - append file to a given shard if exists,\n" +
	indent1 + "\t   otherwise, create a new shard (and name it shard-00123.tar.lz4, as specified);\n" +
	indent1 + "\t- 'src-dir gs://w/shard-999.zip --append' - archive entire 'src-dir' directory; iff the destination .zip doesn't exist create a new one;\n" +
	indent1 + "\t- '\"sys, docs\" ais://dst/CCC.tar --dry-run -y -r --archpath ggg/' - dry-run to recursively archive two directories.\n" +
	indent1 + "\tTips:\n" +
	indent1 + "\t- use '--dry-run' if in doubt;\n" +
	indent1 + "\t- to archive objects from a ais:// or remote bucket, run 'ais archive bucket' (see --help for details)."

// (compare with  objGetUsage)
const archGetUsage = "Get a shard and extract its content; get an archived file;\n" +
	indent4 + "\twrite the content locally with destination options including: filename, directory, STDOUT ('-'), or '/dev/null' (discard);\n" +
	indent4 + "\tassorted options further include:\n" +
	indent4 + "\t- '--prefix' to get multiple shards in one shot (empty prefix for the entire bucket);\n" +
	indent4 + "\t- '--progress' and '--refresh' to watch progress bar;\n" +
	indent4 + "\t- '-v' to produce verbose output when getting multiple objects.\n" +
	indent1 + "'ais archive get' examples:\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar.lz4 /tmp/out - get and extract entire shard to /tmp/out/trunk/*\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar.lz4 --archpath file45.jpeg /tmp/out - extract one named file\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar.lz4/file45.jpeg /tmp/out - same as above (and note that '--archpath' is implied)\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar.lz4/file45 /tmp/out/file456.new - same as above, with destination explicitly (re)named\n" +
	indent1 + "'ais archive get' multi-selection examples:\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar 111.tar --archregx=jpeg --archmode=suffix - return 111.tar with all *.jpeg files from a given shard\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar 222.tar --archregx=file45 --archmode=wdskey - return 222.tar with all file45.* files --/--\n" +
	indent4 + "\t- ais://abc/trunk-0123.tar 333.tar --archregx=subdir/ --archmode=prefix - 333.tar with all subdir/* files --/--"

const genShardsUsage = "Generate random " + archExts + "-formatted objects (\"shards\"), e.g.:\n" +
	indent1 + "\t- gen-shards 'ais://mmm/shard-{001..999}.tar' -\twrite 999 random shards (default sizes) to ais://mmm\n" +
	indent1 + "\t- gen-shards 'ais://mmm/shard-{001..999}.tar' --fcount 10 --output-template 'audio-file-{01..10}.wav' -\t10 archived files per shard (and note templated (deterministic) naming)\n" +
	indent1 + "\t- gen-shards \"gs://bucket2/shard-{01..20..2}.tgz\" -\twrite 10 random gzipped tarfiles to Cloud bucket\n" +
	indent1 + "\t(notice quotation marks in all cases)"

var (
	// flags
	archCmdsFlags = map[string][]cli.Flag{
		commandBucket: {
			archAppendOrPutFlag,
			continueOnErrorFlag,
			dontHeadSrcDstBucketsFlag,
			dryRunFlag,
			listFlag,
			templateFlag,
			verbObjPrefixFlag,
			nonRecursFlag,
			inclSrcBucketNameFlag,
			waitFlag,
		},
		commandPut: append(
			listRangeProgressWaitFlags,
			archAppendOrPutFlag,
			archAppendOnlyFlag,
			archpathFlag,
			numPutWorkersFlag,
			dryRunFlag,
			recursFlag,
			continueOnErrorFlag,
			verboseFlag,
			yesFlag,
			unitsFlag,
			archSrcDirNameFlag,
			skipVerCksumFlag,
		),
		cmdGenShards: {
			cleanupFlag,
			numGenShardWorkersFlag,
			fsizeFlag,
			fcountFlag,
			fextsFlag,
			tformFlag,
			outputTemplateForGenShards,
		},
	}

	// archive bucket (multiple objects => shard)
	archBucketCmd = cli.Command{
		Name:         commandBucket,
		Usage:        archBucketUsage,
		ArgsUsage:    bucketObjectSrcArgument + " " + dstShardArgument,
		Flags:        sortFlags(archCmdsFlags[commandBucket]),
		Action:       archMultiObjHandler,
		BashComplete: putPromApndCompletions,
	}

	// archive put
	archPutCmd = cli.Command{
		Name:         commandPut,
		Usage:        archPutUsage,
		ArgsUsage:    putApndArchArgument,
		Flags:        sortFlags(archCmdsFlags[commandPut]),
		Action:       putApndArchHandler,
		BashComplete: putPromApndCompletions,
	}

	// archive get
	archGetCmd = cli.Command{
		Name:         objectCmdGet.Name,
		Usage:        archGetUsage,
		ArgsUsage:    getShardArgument,
		Flags:        sortFlags(rmFlags(objectCmdGet.Flags, headObjPresentFlag, lengthFlag, offsetFlag)),
		Action:       getArchHandler,
		BashComplete: objectCmdGet.BashComplete,
	}

	// archive ls (NOTE: listArchFlag is implied)
	archLsCmd = cli.Command{
		Name:         cmdList,
		Usage:        "List archived content (supported formats: " + archFormats + ")",
		ArgsUsage:    optionalShardArgument,
		Flags:        sortFlags(rmFlags(lsCmdFlags, listArchFlag)),
		Action:       listArchHandler,
		BashComplete: bucketCompletions(bcmplop{}),
	}

	// gen shards
	genShardsCmd = cli.Command{
		Name:      cmdGenShards,
		Usage:     genShardsUsage,
		ArgsUsage: `"BUCKET/TEMPLATE.EXT"`,
		Flags:     sortFlags(archCmdsFlags[cmdGenShards]),
		Action:    genShardsHandler,
	}

	// main `ais archive`
	archCmd = cli.Command{
		Name:   commandArch,
		Usage:  "Archive multiple objects from a given bucket; archive local files and directories; list archived content",
		Action: archUsageHandler,
		Subcommands: []cli.Command{
			archBucketCmd,
			archPutCmd,
			archGetCmd,
			archLsCmd,
			genShardsCmd,
		},
	}
)

func archUsageHandler(c *cli.Context) error {
	if c.NArg() == 0 {
		cli.ShowCommandHelp(c, c.Command.Name)
		return nil
	}
	{
		// parse for put/append
		a := archput{}
		if err := a.parse(c); err == nil {
			msg := "missing " + commandArch + " subcommand"
			hint := strings.Join(findCmdMultiKeyAlt(commandArch, commandPut), " ")
			if c.NArg() > 0 {
				hint += " " + strings.Join(c.Args(), " ")
			}
			msg += " (did you mean: '" + hint + "')"
			return errors.New(msg)
		}
	}
	{
		// parse for x-archive multi-object
		src, dst := c.Args().Get(0), c.Args().Get(1)
		if _, _, err := parseBckObjURI(c, src, true); err == nil {
			if _, _, err := parseBckObjURI(c, dst, true); err == nil {
				msg := "missing " + commandArch + " subcommand"
				hint := strings.Join(findCmdMultiKeyAlt(commandArch, commandBucket), " ")
				if c.NArg() > 0 {
					hint += " " + strings.Join(c.Args(), " ")
				}
				msg += " (did you mean: '" + hint + "')"
				return errors.New(msg)
			}
		}
	}
	return fmt.Errorf("unrecognized or misplaced option '%+v', see %s for details", c.Args(), qflprn(cli.HelpFlag))
}

func archMultiObjHandler(c *cli.Context) error {
	// parse
	var a archbck
	a.apndIfExist = flagIsSet(c, archAppendOrPutFlag)
	if err := a.parse(c); err != nil {
		// is it an attempt to PUT files => archive?
		{
			b := archput{}
			if errV := b.parse(c); errV == nil {
				msg := fmt.Sprintf("%v\n(hint: check 'ais archive put --help' vs 'ais archive bucket --help')", err)
				return errors.New(msg)
			}
		}
		return err
	}

	// control msg
	msg := cmn.ArchiveBckMsg{ToBck: a.dst.bck}
	{
		msg.ArchName = a.dst.oname
		msg.InclSrcBname = flagIsSet(c, inclSrcBucketNameFlag)
		msg.ContinueOnError = flagIsSet(c, continueOnErrorFlag)
		msg.AppendIfExists = a.apndIfExist
		msg.ListRange = a.rsrc.lr
		msg.NonRecurs = flagIsSet(c, nonRecursFlag)
	}

	// dry-run
	if flagIsSet(c, dryRunFlag) {
		dryRunCptn(c)
		_archDone(c, &msg, &a)
		return nil
	}
	if !flagIsSet(c, dontHeadSrcDstBucketsFlag) {
		if _, err := headBucket(a.rsrc.bck, false /* don't add */); err != nil {
			return err
		}
		if _, err := headBucket(a.dst.bck, false /* don't add */); err != nil {
			return err
		}
	}
	// do
	_, err := api.ArchiveMultiObj(apiBP, a.rsrc.bck, &msg)
	if err != nil {
		return V(err)
	}
	// check (NOTE: not waiting through idle-ness, not looking at multiple returned xids)
	var (
		total time.Duration
		sleep = time.Second / 2
		maxw  = 2 * time.Second
	)
	if flagIsSet(c, waitFlag) {
		maxw = 8 * time.Second
	}
	for total < maxw {
		hargs := api.HeadArgs{FltPresence: apc.FltPresentNoProps, Silent: true}
		if _, errV := api.HeadObject(apiBP, a.dst.bck, a.dst.oname, hargs); errV == nil {
			goto ex
		}
		time.Sleep(sleep)
		total += sleep
	}
ex:
	_archDone(c, &msg, &a)
	return nil
}

func _archDone(c *cli.Context, msg *cmn.ArchiveBckMsg, a *archbck) {
	what := msg.ListRange.Template
	if msg.ListRange.IsList() {
		what = strings.Join(msg.ListRange.ObjNames, ", ")
	}
	fmt.Fprintf(c.App.Writer, "Archived %s/%s => %s\n", a.rsrc.bck.String(), what, a.dest())
}

func putApndArchHandler(c *cli.Context) error {
	{
		src, dst := c.Args().Get(0), c.Args().Get(1)
		if _, _, err := parseBckObjURI(c, src, true); err == nil {
			if _, _, err := parseBckObjURI(c, dst, true); err == nil {
				return fmt.Errorf("expecting %s\n(hint: use 'ais archive bucket' command, see %s for details)",
					c.Command.ArgsUsage, qflprn(cli.HelpFlag))
			}
		}
	}
	a := archput{}
	if err := a.parse(c); err != nil {
		return err
	}
	if flagIsSet(c, dryRunFlag) {
		dryRunCptn(c)
	}
	if a.srcIsRegular() {
		// [convention]: naming default when '--archpath' is omitted
		if a.archpath == "" {
			a.archpath = filepath.Base(a.src.abspath)
		}
		if err := a2aRegular(c, &a); err != nil {
			return err
		}
		msg := fmt.Sprintf("%s %s to %s", a.verb(), a.src.arg, a.dst.bck.Cname(a.dst.oname))
		if a.archpath != a.src.arg {
			msg += " as \"" + a.archpath + "\""
		}
		actionDone(c, msg+"\n")
		return nil
	}

	//
	// multi-file cases
	//
	if _, err := headBucket(a.dst.bck, false /*don't add*/); err != nil {
		if _, ok := err.(*errDoesNotExist); ok {
			return fmt.Errorf("destination %v", err)
		}
		return V(err)
	}
	if !a.appendOnly && !a.appendOrPut {
		warn := fmt.Sprintf("multi-file 'archive put' operation requires either %s or %s option",
			qflprn(archAppendOnlyFlag), qflprn(archAppendOrPutFlag))
		actionWarn(c, warn)
		if flagIsSet(c, yesFlag) {
			fmt.Fprintf(c.App.ErrWriter, "Assuming %s - proceeding to execute...\n\n", qflprn(archAppendOrPutFlag))
		} else {
			if ok := confirm(c, fmt.Sprintf("Proceed to execute 'archive put %s'?", flprn(archAppendOrPutFlag))); !ok {
				return nil
			}
		}
		a.appendOrPut = true
	}

	// archpath
	if a.archpath != "" && !cos.IsLastB(a.archpath, '/') {
		if !flagIsSet(c, yesFlag) {
			warn := fmt.Sprintf("no trailing filepath separator in: '%s=%s'", qflprn(archpathFlag), a.archpath)
			actionWarn(c, warn)
			if !confirm(c, "Proceed anyway?") {
				return nil
			}
		}
	}

	incl := flagIsSet(c, archSrcDirNameFlag)
	switch {
	case len(a.src.fdnames) > 0:
		// a) csv of files and/or directories (names) from the first arg, e.g. "f1[,f2...]" dst-bucket[/prefix]
		// b) csv from '--list' flag
		return verbList(c, &a, a.src.fdnames, a.dst.bck, a.archpath /*append pref*/, incl)
	case a.pt != nil && a.pt.IsRange():
		// a) range from the first arg, e.g. "/tmp/www/test{0..2}{0..2}.txt" dst-bucket/www.zip
		// b) from '--template'
		var trimPrefix string
		if !incl {
			trimPrefix = rangeTrimPrefix(a.pt)
		}
		return verbRange(c, &a, a.pt, a.dst.bck, trimPrefix, a.archpath, incl)
	default: // one directory
		var (
			ndir    int
			srcpath = a.src.arg
		)
		if a.pt != nil {
			debug.Assert(srcpath == "", srcpath)
			srcpath = a.pt.Prefix
		}
		fobjs, err := lsFobj(c, srcpath, "" /*trim pref*/, a.archpath /*append pref*/, &ndir, a.src.recurs, incl, false /*globbed*/)
		if err != nil {
			return err
		}
		debug.Assert(ndir == 1)
		return verbFobjs(c, &a, fobjs, a.dst.bck, ndir, a.src.recurs)
	}
}

func a2aRegular(c *cli.Context, a *archput) error {
	var (
		reader   cos.ReadOpenCloser
		progress *mpb.Progress
	)
	if flagIsSet(c, dryRunFlag) {
		// resulting message printed upon return
		return nil
	}
	fh, err := cos.NewFileHandle(a.src.abspath)
	if err != nil {
		return err
	}
	reader = fh
	if flagIsSet(c, progressFlag) {
		fi, err := fh.Stat()
		if err != nil {
			return err
		}
		// setup progress bar
		var (
			bars []*mpb.Bar
			args = barArgs{barType: sizeArg, barText: a.dst.oname, total: fi.Size()}
		)
		progress, bars = simpleBar(args)

		cb := func(n int, _ error) { bars[0].IncrBy(n) }
		reader = newRocCb(fh, cb, 0)
	}
	putArgs := api.PutArgs{
		BaseParams: apiBP,
		Bck:        a.dst.bck,
		ObjName:    a.dst.oname,
		Reader:     reader,
		Cksum:      nil,
		Size:       uint64(a.src.finfo.Size()),
		SkipVC:     flagIsSet(c, skipVerCksumFlag),
	}
	putApndArchArgs := api.PutApndArchArgs{
		PutArgs:  putArgs,
		ArchPath: a.archpath,
	}
	if a.appendOnly {
		putApndArchArgs.Flags = apc.ArchAppend
	}
	if a.appendOrPut {
		debug.Assert(!a.appendOnly)
		putApndArchArgs.Flags = apc.ArchAppendIfExist
	}
	err = api.PutApndArch(&putApndArchArgs)
	if progress != nil {
		progress.Wait()
	}
	return err
}

func getArchHandler(c *cli.Context) error {
	return getHandler(c)
}

func listArchHandler(c *cli.Context) error {
	if c.NArg() == 0 {
		return missingArgumentsError(c, c.Command.ArgsUsage)
	}
	bck, objName, err := parseBckObjURI(c, c.Args().Get(0), true /*empty ok*/)
	if err != nil {
		return err
	}
	prefix := parseStrFlag(c, listObjPrefixFlag)
	if objName != "" && prefix != "" && !strings.HasPrefix(prefix, objName) {
		return fmt.Errorf("cannot handle object name ('%s') and prefix ('%s') simultaneously - "+NIY,
			objName, prefix)
	}
	if prefix == "" {
		prefix = objName
	}
	return listObjects(c, bck, prefix, true /*list arch*/, true /*print empty*/)
}

//
// generate shards
//

func genShardsHandler(c *cli.Context) error {
	if c.NArg() == 0 {
		return incorrectUsageMsg(c, "missing destination bucket and BASH brace extension template")
	}
	if c.NArg() > 1 {
		return incorrectUsageMsg(c, "too many arguments (make sure to use quotation marks to prevent BASH brace expansion)")
	}

	// Expecting: "ais://bucket/shard-{00..99}.tar"
	bck, objname, err := parseBckObjURI(c, c.Args().Get(0), false)
	if err != nil {
		return err
	}

	mime, err := archive.Strict("", objname)
	if err != nil {
		return err
	}

	fileCnt := parseIntFlag(c, fcountFlag)

	fileSize, err := parseSizeFlag(c, fsizeFlag)
	if err != nil {
		return err
	}

	// validate output naming template if provided
	outFnameTemplate := parseStrFlag(c, outputTemplateForGenShards)
	if outFnameTemplate != "" {
		innerPt, err := cos.NewParsedTemplate(outFnameTemplate)
		if err != nil {
			return err
		}
		if n := innerPt.Count(); n < int64(fileCnt) {
			return fmt.Errorf("invalid %s option: requested %d files per shard, but the template only provides for %d names",
				qflprn(outputTemplateForGenShards), fileCnt, n)
		}
	}

	fileExts := []string{dfltFext}
	if flagIsSet(c, fextsFlag) {
		s := parseStrFlag(c, fextsFlag)
		fileExts = splitCsv(s)

		// file extension must start with "."
		for i := range fileExts {
			if fileExts[i][0] != '.' {
				fileExts[i] = "." + fileExts[i]
			}
		}
	}

	format := tar.FormatUnknown
	if flagIsSet(c, tformFlag) {
		formatAsString := parseStrFlag(c, tformFlag)
		switch formatAsString {
		case "Unknown":
			// Leave fmtat at default
		case "USTAR":
			format = tar.FormatUSTAR
		case "PAX":
			format = tar.FormatPAX
		case "GNU":
			format = tar.FormatGNU
		default:
			return fmt.Errorf("%s value, if specified, must be one of \"%s\", \"USTAR\", \"PAX\", or \"GNU\"", tformFlag.Name, dfltTform)
		}
	}

	mm := memsys.NewMMSA("cli-gen-shards", true /*silent*/)
	ext := mime
	template := strings.TrimSuffix(objname, ext)
	pt, err := cos.ParseBashTemplate(template)
	if err != nil {
		return err
	}

	if err := setupBucket(c, bck); err != nil {
		return err
	}

	var (
		shardNum   int
		progress   = mpb.New(mpb.WithWidth(barWidth))
		concLimit  = parseIntFlag(c, numGenShardWorkersFlag)
		semaCh     = make(chan struct{}, concLimit)
		group, ctx = errgroup.WithContext(context.Background())
		text       = "Shards created: "
		options    = make([]mpb.BarOption, 0, 6)
	)
	// progress bar
	options = append(options, mpb.PrependDecorators(
		decor.Name(text, decor.WC{W: len(text) + 2, C: decor.DSyncWidthR}),
		decor.CountersNoUnit("%d/%d", decor.WCSyncWidth),
	))
	options = appendDefaultDecorators(options)
	bar := progress.AddBar(pt.Count(), options...)

	pt.InitIter()

loop:
	for shardName, hasNext := pt.Next(); hasNext; shardName, hasNext = pt.Next() {
		select {
		case semaCh <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		group.Go(func(i int, name string) func() error {
			return func() error {
				defer func() {
					bar.Increment()
					<-semaCh
				}()

				name := fmt.Sprintf("%s%s", name, ext)
				sgl := mm.NewSGL(fileSize * int64(fileCnt))
				defer sgl.Free()

				if err := genOne(sgl, ext, i*fileCnt, (i+1)*fileCnt, fileCnt, int(fileSize), fileExts, format, outFnameTemplate); err != nil {
					return err
				}
				putArgs := api.PutArgs{
					BaseParams: apiBP,
					Bck:        bck,
					ObjName:    name,
					Reader:     sgl,
					SkipVC:     true,
				}
				_, err := api.PutObject(&putArgs)
				return V(err)
			}
		}(shardNum, shardName))
		shardNum++
	}
	if err := group.Wait(); err != nil {
		bar.Abort(true)
		return err
	}
	progress.Wait()
	return nil
}

func genOne(w io.Writer, shardExt string, start, end, fileCnt, fileSize int, fileExts []string, format tar.Format, outFnameTemplate string) error {
	var (
		pt     *cos.ParsedTemplate
		prefix = make([]byte, 10)
		width  = len(strconv.Itoa(fileCnt))
		oah    = cos.SimpleOAH{Size: int64(fileSize), Atime: time.Now().UnixNano()}
		opts   = archive.Opts{CB: archive.SetTarHeader, TarFormat: format, Serialize: false}
		writer = archive.NewWriter(shardExt, w, nil /*cksum*/, &opts)
	)

	// output naming template if provided
	if outFnameTemplate != "" {
		tmpl, err := cos.NewParsedTemplate(outFnameTemplate)
		if err != nil {
			debug.AssertNoErr(err) // validated above
			return err
		}
		pt = &tmpl
		pt.InitIter()
	}

	for idx := start; idx < end; idx++ {
		if pt == nil {
			cryptorand.Read(prefix)
		}

		for extIdx, fext := range fileExts {
			var name string

			if pt != nil {
				// templated naming
				templateName, hasNext := pt.Next()
				if !hasNext {
					err := fmt.Errorf("template %s exhausted at file %d (in shard starting at %d)",
						qflprn(outputTemplateForGenShards), idx*len(fileExts)+extIdx, start)
					debug.AssertNoErr(err) // validated above
					return err
				}
				name = templateName
			} else {
				// random naming
				name = fmt.Sprintf("%s-%0*d"+fext, hex.EncodeToString(prefix), width, idx)
			}

			if err := writer.Write(name, oah, io.LimitReader(cryptorand.Reader, int64(fileSize))); err != nil {
				writer.Fini()
				return err
			}
		}
	}

	return writer.Fini()
}
