// Package archive provides common low-level utilities for testing archives
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package tarch

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/ext/dsort/shard"
	"github.com/NVIDIA/aistore/tools/tassert"
	"github.com/NVIDIA/aistore/tools/trand"
)

var pool1m, pool128k, pool32k sync.Pool

var (
	_ archive.ArchRCB = (*rcbCtx)(nil)
	_ archive.ArchRCB = (*rcbDummy)(nil)
)

type (
	FileContent struct {
		Name    string
		Ext     string
		Content []byte
	}
	dummyFile struct {
		name string
		size int64
	}
	rcbCtx struct {
		files []FileContent
		ext   string
	}
	rcbDummy struct {
		files []os.FileInfo
	}
)

func randomizeSize(size int, seed uint64) int {
	if size <= 100 {
		return size
	}
	jitter := (int(seed&0x7) - 4) * size / 10
	return size + jitter
}

func addBufferToArch(aw archive.Writer, path string, seed uint64, size int, buf []byte, exactSize bool) (uint64, error) {
	l := size
	if !exactSize {
		l = randomizeSize(size, seed)
	}
	if buf == nil {
		buf = newBuf(l)
		defer freeBuf(buf)
		buf = buf[:l]
		for i := 0; i < len(buf)-cos.SizeofI64; i += cos.SizeofI64 {
			binary.BigEndian.PutUint64(buf[i:], seed+uint64(i))
		}
	}
	reader := bytes.NewBuffer(buf)
	oah := cos.SimpleOAH{Size: int64(l)}
	return seed + uint64(l), aw.Write(path, oah, reader)
}

// TODO: refactor to reduce number of arguments
func CreateArchRandomFiles(shardName string, tarFormat tar.Format, ext string, fileCnt, fileSize int, recExts, randNames []string,
	dup, randDir, exactSize bool) error {
	wfh, err := cos.CreateFile(shardName)
	if err != nil {
		return err
	}

	aw := archive.NewWriter(ext, wfh, nil, &archive.Opts{TarFormat: tarFormat})
	defer func() {
		aw.Fini()
		wfh.Close()
	}()

	var (
		prevFileName string
		dupIndex     = rand.IntN(fileCnt-1) + 1
	)
	if len(recExts) == 0 {
		recExts = []string{".txt"}
	}
	seed := uint64(mono.NanoTime())
	for i := range fileCnt {
		var randomName int
		if randNames == nil {
			randomName = rand.Int()
		}
		for _, ext := range recExts {
			var fileName string
			if randNames == nil {
				fileName = fmt.Sprintf("%d%s", randomName, ext) // generate random names
				if dupIndex == i && dup {
					fileName = prevFileName
				}
			} else {
				fileName = randNames[i]
			}
			if randDir {
				layers := rand.IntN(5)
				for range layers {
					fileName = trand.String(5) + "/" + fileName
				}
			}
			var err error
			if seed, err = addBufferToArch(aw, fileName, seed, fileSize, nil, exactSize); err != nil {
				return err
			}
			prevFileName = fileName
		}
	}
	return nil
}

func CreateArchCustomFilesToW(w io.Writer, tarFormat tar.Format, ext string, fileCnt, fileSize int,
	customFileType, customFileExt string, missingKeys, exactSize bool) error {
	aw := archive.NewWriter(ext, w, nil, &archive.Opts{TarFormat: tarFormat})
	defer aw.Fini()

	seed := uint64(mono.NanoTime())
	for range fileCnt {
		fileName := strconv.Itoa(rand.Int()) // generate random names
		var err error
		if seed, err = addBufferToArch(aw, fileName+".txt", seed, fileSize, nil, exactSize); err != nil {
			return err
		}
		// If missingKeys enabled we should only add keys randomly
		if !missingKeys || (missingKeys && rand.IntN(2) == 0) {
			var buf []byte
			// random content
			if err := shard.ValidateContentKeyTy(customFileType); err != nil {
				return err
			}
			switch customFileType {
			case shard.ContentKeyInt:
				buf = []byte(strconv.Itoa(rand.Int()))
			case shard.ContentKeyString:
				buf = []byte(fmt.Sprintf("%d-%d", rand.Int(), rand.Int()))
			case shard.ContentKeyFloat:
				buf = []byte(fmt.Sprintf("%d.%d", rand.Int(), rand.Int()))
			default:
				debug.Assert(false, customFileType) // validated above
			}
			if seed, err = addBufferToArch(aw, fileName+customFileExt, seed, len(buf), buf, exactSize); err != nil {
				return err
			}
		}
	}
	return nil
}

func CreateArchCustomFiles(shardName string, tarFormat tar.Format, ext string, fileCnt, fileSize int,
	customFileType, customFileExt string, missingKeys, exactSize bool) error {
	wfh, err := cos.CreateFile(shardName)
	if err != nil {
		return err
	}
	defer wfh.Close()
	return CreateArchCustomFilesToW(wfh, tarFormat, ext, fileCnt, fileSize, customFileType, customFileExt, missingKeys, exactSize)
}

func newArchReader(mime string, buffer *bytes.Buffer) (ar archive.Reader, err error) {
	if mime == archive.ExtZip {
		// zip is special
		readerAt := bytes.NewReader(buffer.Bytes())
		ar, err = archive.NewReader(mime, readerAt, int64(buffer.Len()))
	} else {
		ar, err = archive.NewReader(mime, buffer)
	}
	return
}

func (rcb *rcbCtx) Call(filename string, reader cos.ReadCloseSizer, _ any) (bool, error) {
	var (
		buf bytes.Buffer
		ext = cos.Ext(filename)
	)
	defer reader.Close()
	if rcb.ext == ext {
		if _, err := io.Copy(&buf, reader); err != nil {
			return true, err
		}
	}
	rcb.files = append(rcb.files, FileContent{Name: filename, Ext: ext, Content: buf.Bytes()})
	return false, nil
}

func GetFilesFromArchBuffer(mime string, buffer bytes.Buffer, extension string) ([]FileContent, error) {
	var (
		rcb = rcbCtx{
			files: make([]FileContent, 0, 10),
			ext:   extension,
		}
		ar, err = newArchReader(mime, &buffer)
	)
	if err != nil {
		return nil, err
	}
	err = ar.ReadUntil(&rcb, cos.EmptyMatchAll, "")
	return rcb.files, err
}

func (rcb *rcbDummy) Call(filename string, reader cos.ReadCloseSizer, _ any) (bool, error) {
	rcb.files = append(rcb.files, newDummyFile(filename, reader.Size()))
	reader.Close()
	return false, nil
}

func GetFileInfosFromArchBuffer(buffer bytes.Buffer, mime string) ([]os.FileInfo, error) {
	var (
		rcb = rcbDummy{
			files: make([]os.FileInfo, 0, 10),
		}
		ar, err = newArchReader(mime, &buffer)
	)
	if err != nil {
		return nil, err
	}
	err = ar.ReadUntil(&rcb, cos.EmptyMatchAll, "")
	return rcb.files, err
}

///////////////
// dummyFile //
///////////////

func newDummyFile(name string, size int64) *dummyFile {
	return &dummyFile{
		name: name,
		size: size,
	}
}

func (f *dummyFile) Name() string     { return f.name }
func (f *dummyFile) Size() int64      { return f.size }
func (*dummyFile) Mode() os.FileMode  { return 0 }
func (*dummyFile) ModTime() time.Time { return time.Now() }
func (*dummyFile) IsDir() bool        { return false }
func (*dummyFile) Sys() any           { return nil }

//
// assorted buf pools
//

func newBuf(l int) (buf []byte) {
	switch {
	case l > cos.MiB:
		debug.Assertf(false, "buf size exceeds 1MB: %d", l)
	case l > 128*cos.KiB:
		return newBuf1m()
	case l > 32*cos.KiB:
		return newBuf128k()
	}
	return newBuf32k()
}

func freeBuf(buf []byte) {
	c := cap(buf)
	buf = buf[:c]
	switch c {
	case cos.MiB:
		freeBuf1m(buf)
	case 128 * cos.KiB:
		freeBuf128k(buf)
	case 32 * cos.KiB:
		freeBuf32k(buf)
	default:
		debug.Assertf(false, "unexpected buf size: %d", c)
	}
}

func newBuf1m() (buf []byte) {
	if v := pool1m.Get(); v != nil {
		pbuf := v.(*[]byte)
		buf = *pbuf
	} else {
		buf = make([]byte, cos.MiB)
	}
	return
}

func freeBuf1m(buf []byte) {
	pool1m.Put(&buf)
}

func newBuf128k() (buf []byte) {
	if v := pool128k.Get(); v != nil {
		pbuf := v.(*[]byte)
		buf = *pbuf
	} else {
		buf = make([]byte, 128*cos.KiB)
	}
	return
}

func freeBuf128k(buf []byte) {
	pool128k.Put(&buf)
}

func newBuf32k() (buf []byte) {
	if v := pool32k.Get(); v != nil {
		pbuf := v.(*[]byte)
		buf = *pbuf
	} else {
		buf = make([]byte, 32*cos.KiB)
	}
	return
}

func freeBuf32k(buf []byte) {
	pool32k.Put(&buf)
}

///////////
// DrainVerify (compare w/ Drain{} in cmn/archive)
///////////

type (
	DrainVerify struct {
		t         *testing.T
		wantNames []string
		wantSizes []int64
		i         int
		total     int64
	}
)

func NewDrainVerify(t *testing.T, wantNames []string, wantSizes []int64) *DrainVerify {
	return &DrainVerify{
		t:         t,
		wantNames: wantNames,
		wantSizes: wantSizes,
	}
}

func (drain *DrainVerify) Call(name string, r cos.ReadCloseSizer, hdr any) (bool, error) {
	tarhdr, ok := hdr.(*tar.Header)
	if !ok {
		tassert.Fatalf(drain.t, false, "expected *tar.Header, got %T", hdr)
	}
	expSize := drain.wantSizes[drain.i]
	tassert.Errorf(drain.t, tarhdr.Size == expSize, "entry[%d] size mismatch: hdr=%d exp=%d", drain.i, tarhdr.Size, expSize)

	expName := drain.wantNames[drain.i]
	tassert.Errorf(drain.t, name == expName, "entry[%d] name mismatch: got %q exp %q", drain.i, name, expName)

	n, err := io.Copy(io.Discard, r)
	drain.total += n
	_ = r.Close()
	tassert.Errorf(drain.t, n == tarhdr.Size, "entry[%d] drained %d bytes != hdr.Size %d", drain.i, n, tarhdr.Size)

	drain.i++
	return false, err
}
