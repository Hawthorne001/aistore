// Package cos provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
)

type (
	ErrSignal struct {
		signal syscall.Signal
	}
	ErrValue struct {
		atomic.Value
		cnt atomic.Int64
	}
)

///////////////
// ErrValue //
///////////////

func (ea *ErrValue) Store(err error) {
	if ea.cnt.Inc() == 1 {
		ea.Value.Store(err)
	}
}

func (ea *ErrValue) _load() (err error) {
	if x := ea.Value.Load(); x != nil {
		err = x.(error)
	}
	return
}

func (ea *ErrValue) Err() (err error) {
	err = ea._load()
	if err != nil {
		if cnt := ea.cnt.Load(); cnt > 1 {
			err = fmt.Errorf("%w (cnt=%d)", err, cnt)
		}
	}
	return
}

////////////////////////
// IS-syscall helpers //
////////////////////////

// likely out of socket descriptors
func IsErrConnectionNotAvail(err error) (yes bool) {
	return errors.Is(err, syscall.EADDRNOTAVAIL)
}

func IsErrConnectionRefused(err error) (yes bool) {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// TCP RST
func IsErrConnectionReset(err error) (yes bool) {
	return errors.Is(err, syscall.ECONNRESET) || IsErrBrokenPipe(err)
}

// Check if a given error is a broken-pipe one.
func IsErrBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

func IsErrOOS(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

func IsUnreachable(err error, status int) bool {
	return IsErrConnectionRefused(err) ||
		errors.Is(err, context.DeadlineExceeded) ||
		status == http.StatusRequestTimeout ||
		status == http.StatusServiceUnavailable ||
		IsEOF(err) ||
		status == http.StatusBadGateway
}

///////////////
// ErrSignal //
///////////////

// https://tldp.org/LDP/abs/html/exitcodes.html
func (e *ErrSignal) ExitCode() int               { return 128 + int(e.signal) }
func NewSignalError(s syscall.Signal) *ErrSignal { return &ErrSignal{signal: s} }
func (e *ErrSignal) Error() string               { return fmt.Sprintf("Signal %d", e.signal) }

//////////////////////////
// Abnormal Termination //
//////////////////////////

// Exitf writes formatted message to STDOUT and exits with non-zero status code.
func Exitf(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, f, a...)
	fmt.Fprintln(os.Stderr)
	os.Exit(1)
}

// ExitLogf is wrapper around `Exitf` with `glog` logging. It should be used
// instead `Exitf` if the `glog` was initialized.
func ExitLogf(f string, a ...interface{}) {
	glog.Errorf("FATAL ERROR: "+f, a...)
	glog.Flush()
	Exitf(f, a...)
}
