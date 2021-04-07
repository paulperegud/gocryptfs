package fusefrontend

import (
	"context"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/syscallcompat"
	"github.com/rfjakob/gocryptfs/internal/tlog"
)

// Node is a file or directory in the filesystem tree
// in a gocryptfs mount.
type Node struct {
	fs.Inode
}

// Lookup - FUSE call for discovering a file.
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (ch *fs.Inode, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Get device number and inode number into `st`
	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return nil, fs.ToErrno(err)
	}

	// Create new inode and fill `out`
	ch = n.newChild(ctx, st, out)

	// Translate ciphertext size in `out.Attr.Size` to plaintext size
	n.translateSize(dirfd, cName, &out.Attr)

	return ch, 0
}

// GetAttr - FUSE call for stat()ing a file.
//
// GetAttr is symlink-safe through use of openBackingDir() and Fstatat().
func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) (errno syscall.Errno) {
	// If the kernel gives us a file handle, use it.
	if f != nil {
		return f.(fs.FileGetattrer).Getattr(ctx, out)
	}

	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return fs.ToErrno(err)
	}

	// Fix inode number
	rn := n.rootNode()
	rn.inoMap.TranslateStat(st)
	out.Attr.FromStat(st)

	// Translate ciphertext size in `out.Attr.Size` to plaintext size
	n.translateSize(dirfd, cName, &out.Attr)

	if rn.args.ForceOwner != nil {
		out.Owner = *rn.args.ForceOwner
	}
	return 0
}

// Create - FUSE call. Creates a new file.
//
// Symlink-safe through the use of Openat().
func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	var err error
	fd := -1
	// Make sure context is nil if we don't want to preserve the owner
	rn := n.rootNode()
	if !rn.args.PreserveOwner {
		ctx = nil
	}
	newFlags := rn.mangleOpenFlags(flags)
	// Handle long file name
	ctx2 := toFuseCtx(ctx)
	if !rn.args.PlaintextNames && nametransform.IsLongContent(cName) {
		// Create ".name"
		err = rn.nameTransform.WriteLongNameAt(dirfd, cName, name)
		if err != nil {
			return nil, nil, 0, fs.ToErrno(err)
		}
		// Create content
		fd, err = syscallcompat.OpenatUser(dirfd, cName, newFlags|syscall.O_CREAT|syscall.O_EXCL, mode, ctx2)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
		}
	} else {
		// Create content, normal (short) file name
		fd, err = syscallcompat.OpenatUser(dirfd, cName, newFlags|syscall.O_CREAT|syscall.O_EXCL, mode, ctx2)
	}
	if err != nil {
		// xfstests generic/488 triggers this
		if err == syscall.EMFILE {
			var lim syscall.Rlimit
			syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
			tlog.Warn.Printf("Create %q: too many open files. Current \"ulimit -n\": %d", cName, lim.Cur)
		}
		return nil, nil, 0, fs.ToErrno(err)
	}

	// Get device number and inode number into `st`
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	ch := n.newChild(ctx, &st, out)

	f := os.NewFile(uintptr(fd), cName)
	return ch, NewFile(f, rn, &st), fuse.FOPEN_DIRECT_IO, 0
}

// Unlink - FUSE call. Delete a file.
//
// Symlink-safe through use of Unlinkat().
func (n *Node) Unlink(ctx context.Context, name string) (errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Delete content
	err := syscallcompat.Unlinkat(dirfd, cName, 0)
	if err != nil {
		return fs.ToErrno(err)
	}
	// Delete ".name" file
	if !n.rootNode().args.PlaintextNames && nametransform.IsLongContent(cName) {
		err = nametransform.DeleteLongNameAt(dirfd, cName)
		if err != nil {
			tlog.Warn.Printf("Unlink: could not delete .name file: %v", err)
		}
	}
	return fs.ToErrno(err)
}

// Readlink - FUSE call.
//
// Symlink-safe through openBackingDir() + Readlinkat().
func (n *Node) Readlink(ctx context.Context) (out []byte, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	return n.readlink(dirfd, cName)
}

// Open - FUSE call. Open already-existing file.
//
// Symlink-safe through Openat().
func (n *Node) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	rn := n.rootNode()
	newFlags := rn.mangleOpenFlags(flags)
	// Taking this lock makes sure we don't race openWriteOnlyFile()
	rn.openWriteOnlyLock.RLock()
	defer rn.openWriteOnlyLock.RUnlock()

	if rn.args.KernelCache {
		fuseFlags = fuse.FOPEN_KEEP_CACHE
	}

	// Open backing file
	fd, err := syscallcompat.Openat(dirfd, cName, newFlags, 0)
	// Handle a few specific errors
	if err != nil {
		if err == syscall.EMFILE {
			var lim syscall.Rlimit
			syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
			tlog.Warn.Printf("Open %q: too many open files. Current \"ulimit -n\": %d", cName, lim.Cur)
		}
		if err == syscall.EACCES && (int(flags)&syscall.O_ACCMODE) == syscall.O_WRONLY {
			fd, err = rn.openWriteOnlyFile(dirfd, cName, newFlags)
		}
	}
	// Could not handle the error? Bail out
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}

	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	f := os.NewFile(uintptr(fd), cName)
	fh = NewFile(f, rn, &st)
	fuseFlags = fuse.FOPEN_DIRECT_IO
	return
}

// Setattr - FUSE call. Called for chmod, truncate, utimens, ...
func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) (errno syscall.Errno) {
	// Use the fd if the kernel gave us one
	if f != nil {
		f2 := f.(*File)
		return f2.Setattr(ctx, in, out)
	}

	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// chmod(2)
	//
	// gocryptfs.diriv & gocryptfs.longname.[sha256].name files do NOT get chmod'ed
	// or chown'ed with their parent file/dir for simplicity.
	// See nametransform/perms.go for details.
	if mode, ok := in.GetMode(); ok {
		errno = fs.ToErrno(syscallcompat.FchmodatNofollow(dirfd, cName, mode))
		if errno != 0 {
			return errno
		}
	}

	// chown(2)
	uid32, uOk := in.GetUID()
	gid32, gOk := in.GetGID()
	if uOk || gOk {
		uid := -1
		gid := -1

		if uOk {
			uid = int(uid32)
		}
		if gOk {
			gid = int(gid32)
		}
		errno = fs.ToErrno(syscallcompat.Fchownat(dirfd, cName, uid, gid, unix.AT_SYMLINK_NOFOLLOW))
		if errno != 0 {
			return errno
		}
	}

	// utimens(2)
	mtime, mok := in.GetMTime()
	atime, aok := in.GetATime()
	if mok || aok {
		ap := &atime
		mp := &mtime
		if !aok {
			ap = nil
		}
		if !mok {
			mp = nil
		}
		errno = fs.ToErrno(syscallcompat.UtimesNanoAtNofollow(dirfd, cName, ap, mp))
		if errno != 0 {
			return errno
		}
	}

	// For truncate, the user has to have write permissions. That means we can
	// depend on opening a RDWR fd and letting the File handle truncate.
	if sz, ok := in.GetSize(); ok {
		f, _, errno := n.Open(ctx, syscall.O_RDWR)
		if errno != 0 {
			return errno
		}
		f2 := f.(*File)
		defer f2.Release(ctx)
		errno = syscall.Errno(f2.truncate(sz))
		if errno != 0 {
			return errno
		}
		return f2.Getattr(ctx, out)
	}

	return n.Getattr(ctx, nil, out)
}

// StatFs - FUSE call. Returns information about the filesystem.
//
// Symlink-safe because the path is ignored.
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	p := n.rootNode().args.Cipherdir
	var st syscall.Statfs_t
	err := syscall.Statfs(p, &st)
	if err != nil {
		return fs.ToErrno(err)
	}
	out.FromStatfsT(&st)
	return 0
}

// Mknod - FUSE call. Create a device file.
//
// Symlink-safe through use of Mknodat().
func (n *Node) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (inode *fs.Inode, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Make sure context is nil if we don't want to preserve the owner
	rn := n.rootNode()
	if !rn.args.PreserveOwner {
		ctx = nil
	}

	// Create ".name" file to store long file name (except in PlaintextNames mode)
	var err error
	ctx2 := toFuseCtx(ctx)
	if !rn.args.PlaintextNames && nametransform.IsLongContent(cName) {
		err := rn.nameTransform.WriteLongNameAt(dirfd, cName, name)
		if err != nil {
			errno = fs.ToErrno(err)
			return
		}
		// Create "gocryptfs.longfile." device node
		err = syscallcompat.MknodatUser(dirfd, cName, mode, int(rdev), ctx2)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
		}
	} else {
		// Create regular device node
		err = syscallcompat.MknodatUser(dirfd, cName, mode, int(rdev), ctx2)
	}
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}

	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	inode = n.newChild(ctx, st, out)
	return inode, 0
}

// Link - FUSE call. Creates a hard link at "newPath" pointing to file
// "oldPath".
//
// Symlink-safe through use of Linkat().
func (n *Node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (inode *fs.Inode, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	n2 := toNode(target)
	dirfd2, cName2, errno := n2.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd2)

	// Handle long file name (except in PlaintextNames mode)
	rn := n.rootNode()
	var err error
	if !rn.args.PlaintextNames && nametransform.IsLongContent(cName) {
		err = rn.nameTransform.WriteLongNameAt(dirfd, cName, name)
		if err != nil {
			errno = fs.ToErrno(err)
			return
		}
		// Create "gocryptfs.longfile." link
		err = syscallcompat.Linkat(dirfd2, cName2, dirfd, cName, 0)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
		}
	} else {
		// Create regular link
		err = syscallcompat.Linkat(dirfd2, cName2, dirfd, cName, 0)
	}
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}

	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	inode = n.newChild(ctx, st, out)
	return inode, 0
}

// Symlink - FUSE call. Create a symlink.
//
// Symlink-safe through use of Symlinkat.
func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (inode *fs.Inode, errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Make sure context is nil if we don't want to preserve the owner
	rn := n.rootNode()
	if !rn.args.PreserveOwner {
		ctx = nil
	}

	cTarget := target
	if !rn.args.PlaintextNames {
		// Symlinks are encrypted like file contents (GCM) and base64-encoded
		cTarget = rn.encryptSymlinkTarget(target)
	}
	// Create ".name" file to store long file name (except in PlaintextNames mode)
	var err error
	ctx2 := toFuseCtx(ctx)
	if !rn.args.PlaintextNames && nametransform.IsLongContent(cName) {
		err = rn.nameTransform.WriteLongNameAt(dirfd, cName, name)
		if err != nil {
			errno = fs.ToErrno(err)
			return
		}
		// Create "gocryptfs.longfile." symlink
		err = syscallcompat.SymlinkatUser(cTarget, dirfd, cName, ctx2)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
		}
	} else {
		// Create symlink
		err = syscallcompat.SymlinkatUser(cTarget, dirfd, cName, ctx2)
	}

	st, err := syscallcompat.Fstatat2(dirfd, cName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		errno = fs.ToErrno(err)
		return
	}
	inode = n.newChild(ctx, st, out)
	return inode, 0
}

// Rename - FUSE call.
// This function is called on the PARENT DIRECTORY of `name`.
//
// Symlink-safe through Renameat().
func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) (errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall(name)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	n2 := toNode(newParent)
	dirfd2, cName2, errno := n2.prepareAtSyscall(newName)
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd2)

	// Easy case.
	rn := n.rootNode()
	if rn.args.PlaintextNames {
		return fs.ToErrno(syscallcompat.Renameat2(dirfd, cName, dirfd2, cName2, uint(flags)))
	}
	// Long destination file name: create .name file
	nameFileAlreadyThere := false
	var err error
	if nametransform.IsLongContent(cName2) {
		err = rn.nameTransform.WriteLongNameAt(dirfd2, cName2, newName)
		// Failure to write the .name file is expected when the target path already
		// exists. Since hashes are pretty unique, there is no need to modify the
		// .name file in this case, and we ignore the error.
		if err == syscall.EEXIST {
			nameFileAlreadyThere = true
		} else if err != nil {
			return fs.ToErrno(err)
		}
	}
	// Actual rename
	tlog.Debug.Printf("Renameat %d/%s -> %d/%s\n", dirfd, cName, dirfd2, cName2)
	err = syscallcompat.Renameat2(dirfd, cName, dirfd2, cName2, uint(flags))
	if (flags&syscallcompat.RENAME_NOREPLACE == 0) && (err == syscall.ENOTEMPTY || err == syscall.EEXIST) {
		// If an empty directory is overwritten we will always get an error as
		// the "empty" directory will still contain gocryptfs.diriv.
		// Interestingly, ext4 returns ENOTEMPTY while xfs returns EEXIST.
		// We handle that by trying to fs.Rmdir() the target directory and trying
		// again.
		tlog.Debug.Printf("Rename: Handling ENOTEMPTY")
		if n2.Rmdir(ctx, newName) == 0 {
			err = syscallcompat.Renameat2(dirfd, cName, dirfd2, cName2, uint(flags))
		}
	}
	if err != nil {
		if nametransform.IsLongContent(cName2) && nameFileAlreadyThere == false {
			// Roll back .name creation unless the .name file was already there
			nametransform.DeleteLongNameAt(dirfd2, cName2)
		}
		return fs.ToErrno(err)
	}
	if nametransform.IsLongContent(cName) {
		nametransform.DeleteLongNameAt(dirfd, cName)
	}
	return 0
}
