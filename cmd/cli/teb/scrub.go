// Package teb contains templates and (templated) tables to format CLI output.
/*
 * Copyright (c) 2024-2025, NVIDIA CORPORATION. All rights reserved.
 */
package teb

import (
	"strconv"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// naming-wise, see also: fmtLsObjStatus (cmd/cli/teb/lso.go)

const (
	colBucket         = "BUCKET"               // + [/PREFIX]
	colObjects        = "OBJECTS"              // num listed
	colNotIn          = "NOT-CACHED"           // "not present", "not cached", not in-cluster
	colMisplacedNode  = "MISPLACED(cluster)"   // cluster-wise
	colMisplacedMpath = "MISPLACED(mountpath)" // local misplacement
	colMissingCp      = "MISSING-COPIES"
	colSmallSz        = "SMALL"
	colLargeSz        = "LARGE"
	colVchanged       = "VER-CHANGED"
	colVremoved       = "DELETED"
)

const (
	ScrObjects = iota
	ScrNotIn
	ScrMisplacedNode
	ScrMisplacedMpath
	ScrMissingCp
	ScrSmallSz
	ScrLargeSz
	ScrVchanged
	ScrVremoved

	ScrNumStats // NOTE: must be the last
)

var (
	ScrCols = [...]string{colObjects, colNotIn, colMisplacedNode, colMisplacedMpath, colMissingCp, colSmallSz, colLargeSz, colVchanged, colVremoved}
	ScrNums = [ScrNumStats]int64{}
)

type (
	CntSiz struct {
		Cnt int64
		Siz int64
	}
	ScrBp struct {
		Bck    cmn.Bck
		Prefix string
		Stats  [ScrNumStats]CntSiz
		// work
		Line  cos.Sbuilder
		Cname string
	}
	ScrubHelper struct {
		All []*ScrBp
	}
)

func (h *ScrubHelper) colFirst() string {
	var num int
	for _, scr := range h.All {
		if scr.Prefix != "" {
			num++
		}
	}
	switch {
	case num == len(h.All):
		return colBucket + "/PREFIX"
	case num > 0:
		return colBucket + "[/PREFIX]"
	default:
		return colBucket
	}
}

func (h *ScrubHelper) MakeTab(units string, haveRemote, allColumns bool) *Table {
	debug.Assert(len(ScrCols) == len(ScrNums))

	cols := make([]*header, 1, len(ScrCols)+1)
	cols[0] = &header{name: h.colFirst()}
	for _, col := range ScrCols {
		cols = append(cols, &header{name: col})
	}

	table := newTable(cols...)

	// hide assorted columns
	if !allColumns {
		h.hideMissingCp(cols, colMisplacedNode)
		h.hideMissingCp(cols, colMisplacedMpath)
		h.hideMissingCp(cols, colMissingCp)
	}
	if !haveRemote {
		h._hideCol(cols, colNotIn)
		h._hideCol(cols, colVchanged)
		h._hideCol(cols, colVremoved)
	}

	// make tab
	for _, scr := range h.All {
		row := make([]string, 1, len(ScrCols)+1)
		row[0] = scr.Bck.Cname(scr.Prefix)

		for _, v := range scr.Stats {
			row = append(row, scr.fmtVal(v, units))
		}
		table.addRow(row)
	}

	return table
}

// missing-cp: hide when all-zeros
func (h *ScrubHelper) hideMissingCp(cols []*header, col string) {
	for _, scr := range h.All {
		if scr.Stats[ScrMissingCp].Cnt != 0 {
			return
		}
	}
	h._hideCol(cols, col)
}

func (*ScrubHelper) _hideCol(cols []*header, name string) {
	for _, col := range cols {
		if col.name == name {
			col.hide = true
		}
	}
}

// format values
const zeroCnt = "-"

func (*ScrBp) fmtVal(v CntSiz, units string) string {
	if v.Cnt == 0 {
		return zeroCnt
	}
	return strconv.FormatInt(v.Cnt, 10) + " (" + FmtSize(v.Siz, units, 1) + ")"
}
