// FUSE main Fs

// +build linux darwin freebsd

package mount

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/ncw/rclone/fs"
	"golang.org/x/net/context"
)

// FS represents the top level filing system
type FS struct {
	f       fs.Fs
	rootDir *Dir
}

// Check interface satistfied
var _ fusefs.FS = (*FS)(nil)

// Root returns the root node
func (f *FS) Root() (fusefs.Node, error) {
	fs.Debugf(f.f, "Root()")
	if f.rootDir == nil {
		fsDir := &fs.Dir{
			Name: "",
			When: time.Now(),
		}
		f.rootDir = newDir(f.f, fsDir)
	}
	return f.rootDir, nil
}

// mountOptions configures the options from the command line flags
func mountOptions(device string) (options []fuse.MountOption) {
	options = []fuse.MountOption{
		fuse.MaxReadahead(uint32(maxReadAhead)),
		fuse.Subtype("rclone"),
		fuse.FSName(device), fuse.VolumeName(device),
		fuse.NoAppleDouble(),
		fuse.NoAppleXattr(),

		// Options from benchmarking in the fuse module
		//fuse.MaxReadahead(64 * 1024 * 1024),
		//fuse.AsyncRead(), - FIXME this causes
		// ReadFileHandle.Read error: read /home/files/ISOs/xubuntu-15.10-desktop-amd64.iso: bad file descriptor
		// which is probably related to errors people are having
		//fuse.WritebackCache(),
	}
	if allowNonEmpty {
		options = append(options, fuse.AllowNonEmptyMount())
	}
	if allowOther {
		options = append(options, fuse.AllowOther())
	}
	if allowRoot {
		options = append(options, fuse.AllowRoot())
	}
	if defaultPermissions {
		options = append(options, fuse.DefaultPermissions())
	}
	if readOnly {
		options = append(options, fuse.ReadOnly())
	}
	if writebackCache {
		options = append(options, fuse.WritebackCache())
	}
	return options
}

// mount the file system
//
// The mount point will be ready when this returns.
//
// returns an error, and an error channel for the serve process to
// report an error when fusermount is called.
func mount(f fs.Fs, mountpoint string) (*FS, <-chan error, error) {
	fs.Debugf(f, "Mounting on %q", mountpoint)

	filesys := &FS{
		f: f,
	}

	c, err := fuse.Mount(mountpoint, mountOptions(f.Name()+":"+f.Root())...)
	if err != nil {
		return filesys, nil, err
	}
	server := fusefs.New(c, nil)

	// Serve the mount point in the background returning error to errChan
	errChan := make(chan error, 1)
	go func() {
		err := server.Serve(filesys)
		closeErr := c.Close()
		if err == nil {
			err = closeErr
		}
		errChan <- err
	}()

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		return filesys, nil, err
	}

	filesys.startSignalHandler()
	return filesys, errChan, nil
}

// Check interface satsified
var _ fusefs.FSStatfser = (*FS)(nil)

// Statfs is called to obtain file system metadata.
// It should write that data to resp.
func (f *FS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	const blockSize = 4096
	const fsBlocks = (1 << 50) / blockSize
	resp.Blocks = fsBlocks  // Total data blocks in file system.
	resp.Bfree = fsBlocks   // Free blocks in file system.
	resp.Bavail = fsBlocks  // Free blocks in file system if you're not root.
	resp.Files = 1E9        // Total files in file system.
	resp.Ffree = 1E9        // Free files in file system.
	resp.Bsize = blockSize  // Block size
	resp.Namelen = 255      // Maximum file name length?
	resp.Frsize = blockSize // Fragment size, smallest addressable data size in the file system.
	return nil
}

func (f *FS) startSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP)
	go func() {
		for {
			<-sigChan
			if f.rootDir != nil {
				f.rootDir.ForgetAll()
			}
		}
	}()
}
