// Package cli provides easy-to-use commands to manage, monitor, and utilize AIS clusters.
// This file handles object operations.
/*
 * Copyright (c) 2018-2023, NVIDIA CORPORATION. All rights reserved.
 */
package cli

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmd/cli/teb"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/urfave/cli"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

type (
	uploadParams struct {
		bck       cmn.Bck
		files     []fobj
		workerCnt int
		refresh   time.Duration
		totalSize int64
	}
	uploadCtx struct {
		wg            cos.WG
		errCount      atomic.Int32 // uploads failed so far
		processedCnt  atomic.Int32 // files processed so far
		processedSize atomic.Int64 // size of already processed files
		totalBars     []*mpb.Bar
		progress      *mpb.Progress
		errSb         strings.Builder
		lastReport    time.Time
		reportEvery   time.Duration
		mx            sync.Mutex
		verbose       bool
		showProgress  bool
	}
)

func putMultipleObjects(c *cli.Context, files []fobj, bck cmn.Bck) error {
	if len(files) == 0 {
		return fmt.Errorf("no files to PUT (hint: check filename pattern and/or source directory name)")
	}

	// calculate total size, group by extension
	totalSize, extSizes := groupByExt(files)
	totalCount := int64(len(files))

	if flagIsSet(c, dryRunFlag) {
		i := 0
		for ; i < dryRunExamplesCnt; i++ {
			fmt.Fprintf(c.App.Writer, "PUT %q => \"%s/%s\"\n", files[i].path, bck.DisplayName(), files[i].name)
		}
		if i < len(files) {
			fmt.Fprintf(c.App.Writer, "(and %d more)\n", len(files)-i)
		}
		return nil
	}

	var (
		units, errU = parseUnitsFlag(c, unitsFlag)
		tmpl        = teb.MultiPutTmpl + strconv.FormatInt(totalCount, 10) + "\t" + cos.ToSizeIEC(totalSize, 2) + "\n"
		opts        = teb.Opts{AltMap: teb.FuncMapUnits(units)}
	)
	if errU != nil {
		return errU
	}
	if err := teb.Print(extSizes, tmpl, opts); err != nil {
		return err
	}

	// ask a user for confirmation
	if !flagIsSet(c, yesFlag) {
		l := len(files)
		if ok := confirm(c, fmt.Sprintf("PUT %d file%s => %s?", l, cos.Plural(l), bck)); !ok {
			fmt.Fprintln(c.App.Writer, "Operation canceled")
			return nil
		}
	}

	refresh := calcPutRefresh(c)
	numWorkers := parseIntFlag(c, concurrencyFlag)
	params := &uploadParams{
		bck:       bck,
		files:     files,
		workerCnt: numWorkers,
		refresh:   refresh,
		totalSize: totalSize,
	}
	return uploadFiles(c, params)
}

func uploadFiles(c *cli.Context, p *uploadParams) error {
	u := &uploadCtx{
		verbose:      flagIsSet(c, verboseFlag),
		showProgress: flagIsSet(c, progressFlag),
		wg:           cos.NewLimitedWaitGroup(p.workerCnt, 0),
		lastReport:   time.Now(),
		reportEvery:  p.refresh,
	}
	if u.showProgress {
		var (
			filesBarArg = barArgs{
				total:   int64(len(p.files)),
				barText: "Uploaded files progress",
				barType: unitsArg,
			}
			sizeBarArg = barArgs{
				total:   p.totalSize,
				barText: "Uploaded sizes progress",
				barType: sizeArg,
			}
		)
		u.progress, u.totalBars = simpleBar(filesBarArg, sizeBarArg)
	}

	for _, f := range p.files {
		u.wg.Add(1)
		go u.put(c, p, f)
	}
	u.wg.Wait()

	if u.showProgress {
		u.progress.Wait()
		fmt.Fprint(c.App.Writer, u.errSb.String())
	}
	if failed := u.errCount.Load(); failed != 0 {
		return fmt.Errorf("failed to PUT %d object%s", failed, cos.Plural(int(failed)))
	}
	fmt.Fprintf(c.App.Writer, "PUT %d object%s to %q\n", len(p.files), cos.Plural(len(p.files)), p.bck.DisplayName())
	return nil
}

///////////////
// uploadCtx //
///////////////

func (u *uploadCtx) put(c *cli.Context, p *uploadParams, f fobj) {
	defer u.fini(c, p, f)

	reader, err := cos.NewFileHandle(f.path)
	if err != nil {
		str := fmt.Sprintf("Failed to open file %q: %v\n", f.path, err)
		if u.showProgress {
			u.errSb.WriteString(str)
		} else {
			fmt.Fprint(c.App.Writer, str)
		}
		u.errCount.Inc()
		return
	}

	// setup progress bar(s)
	var (
		bar       *mpb.Bar
		updateBar = func(int, error) {} // no-op unless (below)
	)
	if u.showProgress {
		if u.verbose {
			bar = u.progress.AddBar(
				f.size,
				mpb.BarRemoveOnComplete(),
				mpb.PrependDecorators(
					decor.Name(f.name+" ", decor.WC{W: len(f.name) + 1, C: decor.DSyncWidthR}),
					decor.Counters(decor.UnitKiB, "%.1f/%.1f", decor.WCSyncWidth),
				),
				mpb.AppendDecorators(decor.Percentage(decor.WCSyncWidth)),
			)
			updateBar = func(n int, _ error) {
				u.totalBars[1].IncrBy(n)
				bar.IncrBy(n)
			}
		} else {
			updateBar = func(n int, _ error) {
				u.totalBars[1].IncrBy(n)
			}
		}
	}

	var (
		countReader = cos.NewCallbackReadOpenCloser(reader, updateBar /*progress callback*/)
		putArgs     = api.PutArgs{
			BaseParams: apiBP,
			Bck:        p.bck,
			ObjName:    f.name,
			Reader:     countReader,
			SkipVC:     flagIsSet(c, skipVerCksumFlag),
		}
	)
	if _, err := api.PutObject(putArgs); err != nil {
		str := fmt.Sprintf("Failed to PUT %q => %s: %v\n", f.name, p.bck.DisplayName(), err)
		if u.showProgress {
			u.errSb.WriteString(str)
		} else {
			fmt.Fprint(c.App.Writer, str)
		}
		u.errCount.Inc()
	} else if u.verbose && !u.showProgress {
		fmt.Fprintf(c.App.Writer, "%s -> %s\n", f.path, f.name)
	}
}

func (u *uploadCtx) fini(c *cli.Context, p *uploadParams, f fobj) {
	var (
		total = int(u.processedCnt.Inc())
		size  = u.processedSize.Add(f.size)
	)
	if u.showProgress {
		u.totalBars[0].Increment()
	}
	u.wg.Done()
	if u.reportEvery == 0 {
		return
	}

	// lock after releasing semaphore, so the next file can start
	// uploading even if we are stuck on mutex for a while
	u.mx.Lock()
	if !u.showProgress && time.Since(u.lastReport) > u.reportEvery {
		fmt.Fprintf(
			c.App.Writer, "Uploaded %d(%d%%) objects, %s (%d%%).\n",
			total, 100*total/len(p.files), cos.ToSizeIEC(size, 1), 100*size/p.totalSize,
		)
		u.lastReport = time.Now()
	}
	u.mx.Unlock()
}
