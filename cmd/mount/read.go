// +build linux darwin freebsd

package mount

import (
	"io"
	"sync"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/ncw/rclone/fs"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// ReadFileHandle is an open for read file handle on a File
type ReadFileHandle struct {
	mu         sync.Mutex
	closed     bool // set if handle has been closed
	r          *fs.Account
	o          fs.Object
	readCalled bool // set if read has been called
	offset     int64
	hash       *fs.MultiHasher
}

func newReadFileHandle(o fs.Object) (*ReadFileHandle, error) {
	r, err := o.Open()
	if err != nil {
		return nil, err
	}

	var hash *fs.MultiHasher
	if !noChecksum {
		hash, err = fs.NewMultiHasherTypes(o.Fs().Hashes())
		if err != nil {
			fs.Errorf(o.Fs(), "newReadFileHandle hash error: %v", err)
		}
	}

	fh := &ReadFileHandle{
		o:    o,
		r:    fs.NewAccount(r, o).WithBuffer(), // account the transfer
		hash: hash,
	}
	fs.Stats.Transferring(fh.o.Remote())
	return fh, nil
}

// Check interface satisfied
var _ fusefs.Handle = (*ReadFileHandle)(nil)

// Check interface satisfied
var _ fusefs.HandleReader = (*ReadFileHandle)(nil)

// seek to a new offset
//
// if reopen is true, then we won't attempt to use an io.Seeker interface
//
// Must be called with fh.mu held
func (fh *ReadFileHandle) seek(offset int64, reopen bool) (err error) {
	fh.r.StopBuffering() // stop the background reading first
	fh.hash = nil
	oldReader := fh.r.GetReader()
	r := oldReader
	// Can we seek it directly?
	if do, ok := oldReader.(io.Seeker); !reopen && ok {
		fs.Debugf(fh.o, "ReadFileHandle.seek from %d to %d (io.Seeker)", fh.offset, offset)
		_, err = do.Seek(offset, 0)
		if err != nil {
			fs.Debugf(fh.o, "ReadFileHandle.Read io.Seeker failed: %v", err)
			return err
		}
	} else {
		fs.Debugf(fh.o, "ReadFileHandle.seek from %d to %d", fh.offset, offset)
		// close old one
		err = oldReader.Close()
		if err != nil {
			fs.Debugf(fh.o, "ReadFileHandle.Read seek close old failed: %v", err)
		}
		// re-open with a seek
		r, err = fh.o.Open(&fs.SeekOption{Offset: offset})
		if err != nil {
			fs.Debugf(fh.o, "ReadFileHandle.Read seek failed: %v", err)
			return err
		}
	}
	fh.r.UpdateReader(r)
	fh.offset = offset
	return nil
}

// Read from the file handle
func (fh *ReadFileHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	fs.Debugf(fh.o, "ReadFileHandle.Read size %d offset %d", req.Size, req.Offset)
	if fh.closed {
		fs.Errorf(fh.o, "ReadFileHandle.Read error: %v", errClosedFileHandle)
		return errClosedFileHandle
	}
	doSeek := req.Offset != fh.offset
	var n int
	var newOffset int64
	retries := 0
	buf := make([]byte, req.Size)
	doReopen := false
	for {
		if doSeek {
			// Are we attempting to seek beyond the end of the
			// file - if so just return EOF leaving the underlying
			// file in an unchanged state.
			if req.Offset >= fh.o.Size() {
				fs.Debugf(fh.o, "ReadFileHandle.Read attempt to read beyond end of file: %d > %d", req.Offset, fh.o.Size())
				resp.Data = nil
				return nil
			}
			// Otherwise do the seek
			err = fh.seek(req.Offset, doReopen)
		} else {
			err = nil
		}
		if err == nil {
			if req.Size > 0 {
				fh.readCalled = true
			}
			// One exception to the above is if we fail to fully populate a
			// page cache page; a read into page cache is always page aligned.
			// Make sure we never serve a partial read, to avoid that.
			n, err = io.ReadFull(fh.r, buf)
			newOffset = fh.offset + int64(n)
			// if err == nil && rand.Intn(10) == 0 {
			// 	err = errors.New("random error")
			// }
			if err == nil {
				break
			} else if (err == io.ErrUnexpectedEOF || err == io.EOF) && newOffset == fh.o.Size() {
				// Have read to end of file - reset error
				err = nil
				break
			}
		}
		if retries >= fs.Config.LowLevelRetries {
			break
		}
		retries++
		fs.Errorf(fh.o, "ReadFileHandle.Read error: low level retry %d/%d: %v", retries, fs.Config.LowLevelRetries, err)
		doSeek = true
		doReopen = true
	}
	if err != nil {
		fs.Errorf(fh.o, "ReadFileHandle.Read error: %v", err)
	} else {
		resp.Data = buf[:n]
		fh.offset = newOffset
		fs.Debugf(fh.o, "ReadFileHandle.Read OK")

		if fh.hash != nil {
			_, err = fh.hash.Write(resp.Data)
			if err != nil {
				fs.Errorf(fh.o, "ReadFileHandle.Read HashError: %v", err)
				return err
			}
		}
	}
	return err
}

// close the file handle returning errClosedFileHandle if it has been
// closed already.
//
// Must be called with fh.mu held
func (fh *ReadFileHandle) close() error {
	if fh.closed {
		return errClosedFileHandle
	}
	fh.closed = true
	fs.Stats.DoneTransferring(fh.o.Remote(), true)

	if err := fh.checkHash(); err != nil {
		return err
	}

	return fh.r.Close()
}

// Check interface satisfied
var _ fusefs.HandleFlusher = (*ReadFileHandle)(nil)

func (fh *ReadFileHandle) checkHash() error {
	if fh.hash == nil || !fh.readCalled || fh.offset < fh.o.Size() {
		return nil
	}

	for hashType, dstSum := range fh.hash.Sums() {
		srcSum, err := fh.o.Hash(hashType)
		if err != nil {
			return err
		}
		if !fs.HashEquals(dstSum, srcSum) {
			return errors.Errorf("corrupted on transfer: %v hash differ %q vs %q", hashType, dstSum, srcSum)
		}
	}

	return nil
}

// Flush is called each time the file or directory is closed.
// Because there can be multiple file descriptors referring to a
// single opened file, Flush can be called multiple times.
func (fh *ReadFileHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	fs.Debugf(fh.o, "ReadFileHandle.Flush")

	if err := fh.checkHash(); err != nil {
		fs.Errorf(fh.o, "ReadFileHandle.Flush error: %v", err)
		return err
	}

	fs.Debugf(fh.o, "ReadFileHandle.Flush OK")
	return nil
}

var _ fusefs.HandleReleaser = (*ReadFileHandle)(nil)

// Release is called when we are finished with the file handle
//
// It isn't called directly from userspace so the error is ignored by
// the kernel
func (fh *ReadFileHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if fh.closed {
		fs.Debugf(fh.o, "ReadFileHandle.Release nothing to do")
		return nil
	}
	fs.Debugf(fh.o, "ReadFileHandle.Release closing")
	err := fh.close()
	if err != nil {
		fs.Errorf(fh.o, "ReadFileHandle.Release error: %v", err)
	} else {
		fs.Debugf(fh.o, "ReadFileHandle.Release OK")
	}
	return err
}
