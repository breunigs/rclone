// +build linux darwin freebsd

package mount

import (
	"sync"
	"sync/atomic"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/ncw/rclone/fs"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// File represents a file
type File struct {
	size    int64        // size of file - read and written with atomic int64 - must be 64 bit aligned
	d       *Dir         // parent directory - read only
	mu      sync.RWMutex // protects the following
	o       fs.Object    // NB o may be nil if file is being written
	writers int          // number of writers for this file
}

// newFile creates a new File
func newFile(d *Dir, o fs.Object) *File {
	return &File{
		d: d,
		o: o,
	}
}

// rename should be called to update f.o and f.d after a rename
func (f *File) rename(d *Dir, o fs.Object) {
	f.mu.Lock()
	f.o = o
	f.d = d
	f.mu.Unlock()
}

// addWriters increments or decrements the writers
func (f *File) addWriters(n int) {
	f.mu.Lock()
	f.writers += n
	f.mu.Unlock()
}

// Check interface satisfied
var _ fusefs.Node = (*File)(nil)

// Attr fills out the attributes for the file
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.Gid = gid
	a.Uid = uid
	a.Mode = filePerms
	// if o is nil it isn't valid yet, so return the size so far
	if f.o == nil {
		a.Size = uint64(atomic.LoadInt64(&f.size))
	} else {
		a.Size = uint64(f.o.Size())
		if !noModTime {
			modTime := f.o.ModTime()
			a.Atime = modTime
			a.Mtime = modTime
			a.Ctime = modTime
			a.Crtime = modTime
		}
	}
	a.Blocks = (a.Size + 511) / 512
	fs.Debugf(f.o, "File.Attr %+v", a)
	return nil
}

// Check interface satisfied
var _ fusefs.NodeSetattrer = (*File)(nil)

// Update file attributes. Currently supports ModTime only.
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	if noModTime {
		return nil
	}

	// if o is nil it isn't valid yet
	o, err := f.waitForValidObject()
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var newTime time.Time
	if req.Valid.MtimeNow() {
		newTime = time.Now()
	} else if req.Valid.Mtime() {
		newTime = req.Mtime
	}

	if !newTime.IsZero() {
		err := o.SetModTime(newTime)
		switch err {
		case nil:
			fs.Debugf(o, "File.Setattr ModTime OK")
		case fs.ErrorCantSetModTime:
			// do nothing, in order to not break "touch somefile" if it exists already
		default:
			fs.Errorf(o, "File.Setattr ModTime error: %v", err)
			return err
		}
	}

	return nil
}

// Update the size while writing
func (f *File) written(n int64) {
	atomic.AddInt64(&f.size, n)
}

// Update the object when written
func (f *File) setObject(o fs.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.o = o
	f.d.addObject(o, f)
}

// Wait for f.o to become non nil for a short time returning it or an
// error
//
// Call without the mutex held
func (f *File) waitForValidObject() (o fs.Object, err error) {
	for i := 0; i < 50; i++ {
		f.mu.Lock()
		o = f.o
		writers := f.writers
		f.mu.Unlock()
		if o != nil {
			return o, nil
		}
		if writers == 0 {
			return nil, errors.New("can't open file - writer failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fuse.ENOENT
}

// Check interface satisfied
var _ fusefs.NodeOpener = (*File)(nil)

// Open the file for read or write
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fh fusefs.Handle, err error) {
	// if o is nil it isn't valid yet
	o, err := f.waitForValidObject()
	if err != nil {
		return nil, err
	}
	fs.Debugf(o, "File.Open %v", req.Flags)

	switch {
	case req.Flags.IsReadOnly():
		if noSeek {
			resp.Flags |= fuse.OpenNonSeekable
		}
		fh, err = newReadFileHandle(o)
		err = errors.Wrap(err, "open for read")
	case req.Flags.IsWriteOnly() || (req.Flags.IsReadWrite() && (req.Flags&fuse.OpenTruncate) != 0):
		resp.Flags |= fuse.OpenNonSeekable
		src := newCreateInfo(f.d.f, o.Remote())
		fh, err = newWriteFileHandle(f.d, f, src)
		err = errors.Wrap(err, "open for write")
	case req.Flags.IsReadWrite():
		err = errors.New("can't open for read and write simultaneously")
	default:
		err = errors.Errorf("can't figure out how to open with flags %v", req.Flags)
	}

	/*
	   // File was opened in append-only mode, all writes will go to end
	   // of file. OS X does not provide this information.
	   OpenAppend    OpenFlags = syscall.O_APPEND
	   OpenCreate    OpenFlags = syscall.O_CREAT
	   OpenDirectory OpenFlags = syscall.O_DIRECTORY
	   OpenExclusive OpenFlags = syscall.O_EXCL
	   OpenNonblock  OpenFlags = syscall.O_NONBLOCK
	   OpenSync      OpenFlags = syscall.O_SYNC
	   OpenTruncate  OpenFlags = syscall.O_TRUNC
	*/

	if err != nil {
		fs.Errorf(o, "File.Open failed: %v", err)
		return nil, err
	}
	return fh, nil
}

// Check interface satisfied
var _ fusefs.NodeFsyncer = (*File)(nil)

// Fsync the file
//
// Note that we don't do anything except return OK
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return nil
}
