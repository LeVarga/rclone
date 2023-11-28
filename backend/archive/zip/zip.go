// Package zip implements a zip archiver for the archive backend
package zip

// FIXME this will perform poorly for unpacking as the VFS Reader is bad
// at multiple streams - need cache mode setting?

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/archive/archiver"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/dirtree"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/lib/readers"
	"github.com/rclone/rclone/vfs"
)

func init() {
	archiver.Register(archiver.Archiver{
		New:       New,
		Extension: ".zip",
	})
}

// Fs represents a wrapped fs.Fs
type Fs struct {
	f           fs.Fs
	wrapper     fs.Fs
	name        string
	features    *fs.Features // optional features
	vfs         *vfs.VFS
	node        vfs.Node        // zip file object - set if reading
	remote      string          // remote of the zip file object
	prefix      string          // position for objects
	prefixSlash string          // position for objects with a slash on
	root        string          // position to read from within the archive
	dt          dirtree.DirTree // read from zipfile
}

// New constructs an Fs from the (wrappedFs, remote) with the objects
// prefix with prefix and rooted at root
func New(ctx context.Context, wrappedFs fs.Fs, remote, prefix, root string) (fs.Fs, error) {
	// FIXME vfs cache?
	// FIXME could factor out ReadFileHandle and just use that rather than the full VFS
	fs.Debugf(nil, "Zip: New: remote=%q, prefix=%q, root=%q", remote, prefix, root)
	VFS := vfs.New(wrappedFs, nil)
	node, err := VFS.Stat(remote)
	if err != nil {
		return nil, fmt.Errorf("failed to find %q archive: %w", remote, err)
	}

	f := &Fs{
		f:           wrappedFs,
		name:        path.Join(fs.ConfigString(wrappedFs), remote),
		vfs:         VFS,
		node:        node,
		remote:      remote,
		root:        root,
		prefix:      prefix,
		prefixSlash: prefix + "/",
	}

	// Read the contents of the zip file
	singleObject, err := f.readZip()
	if err != nil {
		return nil, fmt.Errorf("failed to open zip file: %w", err)
	}

	// FIXME
	// the features here are ones we could support, and they are
	// ANDed with the ones from wrappedFs
	//
	// FIXME some of these need to be forced on - CanHaveEmptyDirectories
	f.features = (&fs.Features{
		CaseInsensitive:         false,
		DuplicateFiles:          false,
		ReadMimeType:            false, // MimeTypes not supported with gzip
		WriteMimeType:           false,
		BucketBased:             false,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f).Mask(ctx, wrappedFs).WrapsFs(f, wrappedFs)

	if singleObject {
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("Zip %q", f.name)
}

// readZip the zip file into f
//
// Returns singleObject=true if f.root points to a file
func (f *Fs) readZip() (singleObject bool, err error) {
	if f.node == nil {
		return singleObject, fs.ErrorDirNotFound
	}
	size := f.node.Size()
	if size < 0 {
		return singleObject, errors.New("can't read from zip file with unknown size")
	}
	r, err := f.node.Open(os.O_RDONLY)
	if err != nil {
		return singleObject, fmt.Errorf("failed to open zip file: %w", err)
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return singleObject, fmt.Errorf("failed to read zip file: %w", err)
	}
	dt := dirtree.New()
	for _, file := range zr.File {
		remote := strings.Trim(path.Clean(file.Name), "/")
		if remote == "." {
			remote = ""
		}
		remote = path.Join(f.prefix, remote)
		if f.root != "" {
			// Ignore all files outside the root
			if !strings.HasPrefix(remote, f.root) {
				continue
			}
			if remote == f.root {
				remote = ""
			} else {
				remote = strings.TrimPrefix(remote, f.root+"/")
			}
		}
		if strings.HasSuffix(file.Name, "/") {
			dir := fs.NewDir(remote, file.Modified)
			dt.AddDir(dir)
		} else {
			if remote == "" {
				remote = path.Base(f.root)
				singleObject = true
				dt = dirtree.New()
			}
			o := &Object{
				f:      f,
				remote: remote,
				fh:     &file.FileHeader,
				file:   file,
			}
			dt.Add(o)
			if singleObject {
				break
			}
		}
	}
	dt.CheckParents("")
	dt.Sort()
	f.dt = dt
	//fs.Debugf(nil, "dt = %v", dt)
	return singleObject, nil
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	defer log.Trace(f, "dir=%q", dir)("entries=%v, err=%v", &entries, &err)
	// _, err = f.strip(dir)
	// if err != nil {
	// 	return nil, err
	// }
	entries, ok := f.dt[dir]
	if !ok {
		return nil, fs.ErrorDirNotFound
	}
	fs.Debugf(f, "dir=%q, entries=%v", dir, entries)
	return entries, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (o fs.Object, err error) {
	defer log.Trace(f, "remote=%q", remote)("obj=%v, err=%v", &o, &err)
	if f.dt == nil {
		return nil, fs.ErrorObjectNotFound
	}
	_, entry := f.dt.Find(remote)
	if entry == nil {
		return nil, fs.ErrorObjectNotFound
	}
	o, ok := entry.(*Object)
	if !ok {
		return nil, fs.ErrorNotAFile
	}
	return o, nil
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Mkdir makes the directory (container, bucket)
//
// Shouldn't return an error if it already exists
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return vfs.EROFS
}

// Rmdir removes the directory (container, bucket) if empty
//
// Return an error if it doesn't exist or isn't empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return vfs.EROFS
}

// Put in to the remote path with the modTime given of the given size
//
// May create the object even if it returns an error - if so
// will return the object and the error, otherwise will return
// nil and the error
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (o fs.Object, err error) {
	return nil, vfs.EROFS
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.CRC32)
}

// UnWrap returns the Fs that this Fs is wrapping
func (f *Fs) UnWrap() fs.Fs {
	return f.f
}

// WrapFs returns the Fs that is wrapping this Fs
func (f *Fs) WrapFs() fs.Fs {
	return f.wrapper
}

// SetWrapper sets the Fs that is wrapping this Fs
func (f *Fs) SetWrapper(wrapper fs.Fs) {
	f.wrapper = wrapper
}

// Object describes an object to be read from the raw zip file
type Object struct {
	f      *Fs
	remote string
	fh     *zip.FileHeader
	file   *zip.File
}

// Fs returns read only access to the Fs that this object is part of
func (o *Object) Fs() fs.Info {
	return o.f
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.Remote()
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Size returns the size of the file
func (o *Object) Size() int64 {
	return int64(o.fh.UncompressedSize64)
}

// ModTime returns the modification time of the object
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.fh.Modified
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return vfs.EROFS
}

// Storable raturns a boolean indicating if this object is storable
func (o *Object) Storable() bool {
	return true
}

// Hash returns the selected checksum of the file
// If no checksum is available it returns ""
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	if ht == hash.CRC32 {
		// FIXME return empty CRC if writing
		if o.f.dt == nil {
			return "", nil
		}
		return fmt.Sprintf("%08x", o.fh.CRC32), nil
	}
	return "", hash.ErrUnsupported
}

// Open opens the file for read.  Call Close() on the returned io.ReadCloser
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (rc io.ReadCloser, err error) {
	var offset, limit int64 = 0, -1
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(o.Size())
		default:
			if option.Mandatory() {
				fs.Logf(o, "Unsupported mandatory option: %v", option)
			}
		}
	}

	rc, err = o.file.Open()
	if err != nil {
		return nil, err
	}

	// discard data from start as necessary
	if offset > 0 {
		_, err = io.CopyN(io.Discard, rc, offset)
		if err != nil {
			return nil, err
		}
	}
	// If limited then don't return everything
	if limit >= 0 {
		return readers.NewLimitedReadCloser(rc, limit-offset), nil
	}

	return rc, nil
}

// Update in to the object with the modTime given of the given size
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return vfs.EROFS
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	return vfs.EROFS
}

// Check the interfaces are satisfied
var (
	_ fs.Fs        = (*Fs)(nil)
	_ fs.UnWrapper = (*Fs)(nil)
	//	_ fs.Abouter         = (*Fs)(nil) - FIXME can implemnet
	_ fs.Wrapper = (*Fs)(nil)
	_ fs.Object  = (*Object)(nil)
)
