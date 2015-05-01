// See the file LICENSE for copyright and licensing information.

// Adapted from Plan 9 from User Space's src/cmd/9pfuse/fuse.c,
// which carries this notice:
//
// The files in this directory are subject to the following license.
//
// The author of this software is Russ Cox.
//
//         Copyright (c) 2006 Russ Cox
//
// Permission to use, copy, modify, and distribute this software for any
// purpose without fee is hereby granted, provided that this entire notice
// is included in all copies of any software which is or includes a copy
// or modification of this software and in all copies of the supporting
// documentation for such software.
//
// THIS SOFTWARE IS BEING PROVIDED "AS IS", WITHOUT ANY EXPRESS OR IMPLIED
// WARRANTY.  IN PARTICULAR, THE AUTHOR MAKES NO REPRESENTATION OR WARRANTY
// OF ANY KIND CONCERNING THE MERCHANTABILITY OF THIS SOFTWARE OR ITS
// FITNESS FOR ANY PARTICULAR PURPOSE.

// Package fuse enables writing FUSE file systems on Linux, OS X, and FreeBSD.
//
// On OS X, it requires OSXFUSE (http://osxfuse.github.com/).
//
// There are two approaches to writing a FUSE file system.  The first is to speak
// the low-level message protocol, reading from a Conn using ReadRequest and
// writing using the various Respond methods.  This approach is closest to
// the actual interaction with the kernel and can be the simplest one in contexts
// such as protocol translators.
//
// Servers of synthesized file systems tend to share common
// bookkeeping abstracted away by the second approach, which is to
// call fs.Serve to serve the FUSE protocol using an implementation of
// the service methods in the interfaces FS* (file system), Node* (file
// or directory), and Handle* (opened file or directory).
// There are a daunting number of such methods that can be written,
// but few are required.
// The specific methods are described in the documentation for those interfaces.
//
// The hellofs subdirectory contains a simple illustration of the fs.Serve approach.
//
// Service Methods
//
// The required and optional methods for the FS, Node, and Handle interfaces
// have the general form
//
//	Op(ctx context.Context, req *OpRequest, resp *OpResponse) Error
//
// where Op is the name of a FUSE operation. Op reads request
// parameters from req and writes results to resp. An operation whose
// only result is the error result omits the resp parameter.
//
// Multiple goroutines may call service methods simultaneously; the
// methods being called are responsible for appropriate
// synchronization.
//
// The operation must not hold on to the request or response,
// including any []byte fields such as WriteRequest.Data or
// SetxattrRequest.Xattr.
//
// Errors
//
// Operations can return errors. The FUSE interface can only
// communicate POSIX errno error numbers to file system clients, the
// message is not visible to file system clients. The returned error
// can implement ErrorNumber to control the errno returned. Without
// ErrorNumber, a generic errno (EIO) is returned.
//
// Errors messages will be visible in the debug log as part of the
// response.
//
// Interrupted Operations
//
// In some file systems, some operations
// may take an undetermined amount of time.  For example, a Read waiting for
// a network message or a matching Write might wait indefinitely.  If the request
// is cancelled and no longer needed, the context will be cancelled.
// Blocking operations should select on a receive from ctx.Done() and attempt to
// abort the operation early if the receive succeeds (meaning the channel is closed).
// To indicate that the operation failed because it was aborted, return fuse.EINTR.
//
// If an operation does not block for an indefinite amount of time, supporting
// cancellation is not necessary.
//
// Authentication
//
// All requests types embed a Header, meaning that the method can
// inspect req.Pid, req.Uid, and req.Gid as necessary to implement
// permission checking. The kernel FUSE layer normally prevents other
// users from accessing the FUSE file system (to change this, see
// AllowOther, AllowRoot), but does not enforce access modes (to
// change this, see DefaultPermissions).
//
// Mount Options
//
// Behavior and metadata of the mounted file system can be changed by
// passing MountOption values to Mount.
//
package fuse // import "github.com/bpowers/fuse"

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// A Conn represents a connection to a mounted FUSE file system.
type Conn struct {
	// Ready is closed when the mount is complete or has failed.
	Ready <-chan struct{}

	// MountError stores any error from the mount process. Only valid
	// after Ready is closed.
	MountError error

	// File handle for kernel communication. Only safe to access if
	// rio or wio is held.
	dev *os.File
	buf []byte
	wio sync.Mutex
	rio sync.RWMutex
}

// Mount mounts a new FUSE connection on the named directory
// and returns a connection for reading and writing FUSE messages.
//
// After a successful return, caller must call Close to free
// resources.
//
// Even on successful return, the new mount is not guaranteed to be
// visible until after Conn.Ready is closed. See Conn.MountError for
// possible errors. Incoming requests on Conn must be served to make
// progress.
func Mount(dir string, options ...MountOption) (*Conn, error) {
	conf := MountConfig{
		options: make(map[string]string),
	}
	for _, option := range options {
		if err := option(&conf); err != nil {
			return nil, err
		}
	}

	ready := make(chan struct{}, 1)
	c := &Conn{
		Ready: ready,
	}
	f, err := mount(dir, &conf, ready, &c.MountError)
	if err != nil {
		return nil, err
	}
	c.dev = f
	return c, nil
}

// A Request represents a single FUSE request received from the kernel.
// Use a type switch to determine the specific kind.
// A request of unrecognized type will have concrete type *Header.
type Request interface {
	// Hdr returns the Header associated with this request.
	Hdr() *Header

	// RespondError responds to the request with the given error.
	RespondError(error)

	String() string
}

// A RequestID identifies an active FUSE request.
type RequestID uint64

// A NodeID is a number identifying a directory or file.
// It must be unique among IDs returned in LookupResponses
// that have not yet been forgotten by ForgetRequests.
type NodeID uint64

// A HandleID is a number identifying an open directory or file.
// It only needs to be unique while the directory or file is open.
type HandleID uint64

// The RootID identifies the root directory of a FUSE file system.
const RootID NodeID = rootID

// A Header describes the basic information sent in every request.
type Header struct {
	Conn   *Conn `json:"-"` // connection this request was received on
	Len    uint32
	Opcode uint32
	ID     RequestID // unique ID for request
	Node   NodeID    // file or directory the request is about
	Uid    uint32    // user ID of process making request
	Gid    uint32    // group ID of process making request
	Pid    uint32    // process ID of process making request
}

func (h *Header) String() string {
	return fmt.Sprintf("ID=%#x Node=%#x Uid=%d Gid=%d Pid=%d", h.ID, h.Node, h.Uid, h.Gid, h.Pid)
}

func (h *Header) Hdr() *Header {
	return h
}

func (h *Header) noResponse() {
	//putMessage(h.msg)
}

func (h *Header) respond(out *outHeader, n uintptr) {
	h.Conn.respond(out, n)
	//putMessage(h.msg)
}

func (h *Header) respondData(out *outHeader, n uintptr, data []byte) {
	h.Conn.respondData(out, n, data)
	//putMessage(h.msg)
}

func (h *Header) respondSafe(out *outHeader, data []byte) {
	h.Conn.respondSafe(out, data)
	//putMessage(h.msg)
}

// An ErrorNumber is an error with a specific error number.
//
// Operations may return an error value that implements ErrorNumber to
// control what specific error number (errno) to return.
type ErrorNumber interface {
	// Errno returns the the error number (errno) for this error.
	Errno() Errno
}

const (
	// ENOSYS indicates that the call is not supported.
	ENOSYS = Errno(syscall.ENOSYS)

	// ESTALE is used by Serve to respond to violations of the FUSE protocol.
	ESTALE = Errno(syscall.ESTALE)

	ENOENT = Errno(syscall.ENOENT)
	EIO    = Errno(syscall.EIO)
	EPERM  = Errno(syscall.EPERM)

	// EINTR indicates request was interrupted by an InterruptRequest.
	// See also fs.Intr.
	EINTR = Errno(syscall.EINTR)

	ERANGE  = Errno(syscall.ERANGE)
	ENOTSUP = Errno(syscall.ENOTSUP)
	EEXIST  = Errno(syscall.EEXIST)
)

// DefaultErrno is the errno used when error returned does not
// implement ErrorNumber.
const DefaultErrno = EIO

var errnoNames = map[Errno]string{
	ENOSYS: "ENOSYS",
	ESTALE: "ESTALE",
	ENOENT: "ENOENT",
	EIO:    "EIO",
	EPERM:  "EPERM",
	EINTR:  "EINTR",
	EEXIST: "EEXIST",
}

// Errno implements Error and ErrorNumber using a syscall.Errno.
type Errno syscall.Errno

var _ = ErrorNumber(Errno(0))
var _ = error(Errno(0))

func (e Errno) Errno() Errno {
	return e
}

func (e Errno) String() string {
	return syscall.Errno(e).Error()
}

func (e Errno) Error() string {
	return syscall.Errno(e).Error()
}

// ErrnoName returns the short non-numeric identifier for this errno.
// For example, "EIO".
func (e Errno) ErrnoName() string {
	s := errnoNames[e]
	if s == "" {
		s = fmt.Sprint(e.Errno())
	}
	return s
}

func (e Errno) MarshalText() ([]byte, error) {
	s := e.ErrnoName()
	return []byte(s), nil
}

func (h *Header) RespondError(err error) {
	errno := DefaultErrno
	if ferr, ok := err.(ErrorNumber); ok {
		errno = ferr.Errno()
	}
	// FUSE uses negative errors!
	// TODO: File bug report against OSXFUSE: positive error causes kernel panic.
	out := &outHeader{Error: -int32(errno), Unique: uint64(h.ID)}
	h.respond(out, unsafe.Sizeof(*out))
}

// Maximum file write size we are prepared to receive from the kernel.
// 31 pages should be enough for anyone.
const maxWrite = 31 * 4 * 1024

// All requests read from the kernel, without data, are shorter than
// this.
var maxRequestSize = syscall.Getpagesize()
var bufSize = maxRequestSize + maxWrite

// reqPool is a pool of messages.
//
// Lifetime of a logical message is from getMessage to putMessage.
// getMessage is called by ReadRequest. putMessage is called by
// Conn.ReadRequest, Request.Respond, or Request.RespondError.
//
// Messages in the pool are guaranteed to have conn and off zeroed,
// buf allocated and len==bufSize, and hdr set.
var bufPool = sync.Pool{
	New: allocBuf,
}

func allocBuf() interface{} {
	return make([]byte, bufSize)
}

func getBuffer() []byte {
	return bufPool.Get().([]byte)
}

func putBuffer(buf []byte) {
	buf = buf[:bufSize]
	bufPool.Put(buf)
}

func ReadHeader(h *Header, buf []byte) error {
	// FIXME: is it always little endian, or is it the endian-ness
	// of the current arch?
	h.Len = binary.LittleEndian.Uint32(buf[0:4])
	h.Opcode = binary.LittleEndian.Uint32(buf[4:8])
	h.ID = RequestID(binary.LittleEndian.Uint64(buf[8:16]))
	h.Node = NodeID(binary.LittleEndian.Uint64(buf[16:24]))
	h.Uid = binary.LittleEndian.Uint32(buf[24:28])
	h.Gid = binary.LittleEndian.Uint32(buf[28:32])
	h.Pid = binary.LittleEndian.Uint32(buf[32:36])
	return nil
}

// fileMode returns a Go os.FileMode from a Unix mode.
func fileMode(unixMode uint32) os.FileMode {
	mode := os.FileMode(unixMode & 0777)
	switch unixMode & syscall.S_IFMT {
	case syscall.S_IFREG:
		// nothing
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFCHR:
		mode |= os.ModeCharDevice | os.ModeDevice
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	default:
		// no idea
		mode |= os.ModeDevice
	}
	if unixMode&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if unixMode&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	return mode
}

type noOpcode struct {
	Opcode uint32
}

func (m noOpcode) String() string {
	return fmt.Sprintf("No opcode %v", m.Opcode)
}

type malformedMessage struct {
}

func (malformedMessage) String() string {
	return "malformed message"
}

// Close closes the FUSE connection.
func (c *Conn) Close() error {
	c.wio.Lock()
	defer c.wio.Unlock()
	c.rio.Lock()
	defer c.rio.Unlock()
	return c.dev.Close()
}

// caller must hold wio or rio
func (c *Conn) fd() int {
	return int(c.dev.Fd())
}

// ReadRequest returns the next FUSE request from the kernel.
//
// Caller must call either Request.Respond or Request.RespondError in
// a reasonable time. Caller must not retain Request after that call.
func (c *Conn) ReadRequest() (Request, error) {
	buf := getBuffer()
	defer putBuffer(buf)
loop:
	c.rio.RLock()
	n, err := syscall.Read(c.fd(), buf)
	c.rio.RUnlock()
	if err == syscall.EINTR {
		// OSXFUSE sends EINTR to userspace when a request interrupt
		// completed before it got sent to userspace?
		goto loop
	}
	if err != nil && err != syscall.ENODEV {
		return nil, err
	}
	if n <= 0 {
		return nil, io.EOF
	}
	buf = buf[:n]

	if n < inHeaderSize {
		return nil, errors.New("fuse: message too short")
	}

	hdr := Header{Conn: c}
	if err := ReadHeader(&hdr, buf[:inHeaderSize]); err != nil {
		return nil, fmt.Errorf("ReadHeader")
	}
	buf = buf[inHeaderSize:]

	// FreeBSD FUSE sends a short length in the header
	// for FUSE_INIT even though the actual read length is correct.
	if hdr.Opcode == opInit && n == inHeaderSize+initInSize && hdr.Len < uint32(n) {
		hdr.Len = uint32(n)
	}

	// OSXFUSE sometimes sends the wrong hdr.Len in a FUSE_WRITE message.
	if hdr.Opcode == opWrite && hdr.Len < uint32(n) && hdr.Len >= writeInSize {
		hdr.Len = uint32(n)
	}

	if hdr.Len != uint32(n) {
		return nil, fmt.Errorf("fuse: bad hdr len") //read %d opcode %d but expected %d", n, hdr.Opcode, hdr.Len)
	}

	// Convert to data structures.
	// Do not trust kernel to hand us well-formed data.
	var req Request
	switch hdr.Opcode {
	default:
		//Debug(noOpcode{Opcode: hdr.Opcode})
		goto unrecognized

	case opLookup:
		buf := buf
		n := len(buf)
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		req = &LookupRequest{
			Header: hdr,
			Name:   string(buf[:n-1]),
		}

	case opForget:
		var in forgetIn
		if len(buf) < forgetInSize {
			goto corrupt
		}
		in.Nlookup = binary.LittleEndian.Uint64(buf[0:8])
		req = &ForgetRequest{
			Header: hdr,
			N:      in.Nlookup,
		}

	case opGetattr:
		req = &GetattrRequest{
			Header: hdr,
		}

	case opSetattr:
		var in setattrIn
		if len(buf) < setattrInSize {
			goto corrupt
		}
		in.Valid = binary.LittleEndian.Uint32(buf[0:4])
		in.Padding = binary.LittleEndian.Uint32(buf[4:8])
		in.Fh = binary.LittleEndian.Uint64(buf[8:16])
		in.Size = binary.LittleEndian.Uint64(buf[16:24])
		in.LockOwner = binary.LittleEndian.Uint64(buf[24:32])
		in.Atime = binary.LittleEndian.Uint64(buf[32:40])
		in.Mtime = binary.LittleEndian.Uint64(buf[40:48])
		in.Unused2 = binary.LittleEndian.Uint64(buf[48:56])
		in.AtimeNsec = binary.LittleEndian.Uint32(buf[56:60])
		in.MtimeNsec = binary.LittleEndian.Uint32(buf[60:64])
		in.Unused3 = binary.LittleEndian.Uint32(buf[64:68])
		in.Mode = binary.LittleEndian.Uint32(buf[68:72])
		in.Unused4 = binary.LittleEndian.Uint32(buf[72:76])
		in.Uid = binary.LittleEndian.Uint32(buf[76:80])
		in.Gid = binary.LittleEndian.Uint32(buf[80:84])
		in.Unused5 = binary.LittleEndian.Uint32(buf[84:88])
		req = &SetattrRequest{
			Header:   hdr,
			Valid:    SetattrValid(in.Valid),
			Handle:   HandleID(in.Fh),
			Size:     in.Size,
			Atime:    time.Unix(int64(in.Atime), int64(in.AtimeNsec)),
			Mtime:    time.Unix(int64(in.Mtime), int64(in.MtimeNsec)),
			Mode:     fileMode(in.Mode),
			Uid:      in.Uid,
			Gid:      in.Gid,
			Bkuptime: in.BkupTime(),
			Chgtime:  in.Chgtime(),
			Flags:    in.Flags(),
		}
	case opReadlink:
		if len(buf) > 0 {
			goto corrupt
		}
		req = &ReadlinkRequest{
			Header: hdr,
		}

	case opSymlink:
		// buf is "newName\0target\0"
		names := buf
		if len(names) == 0 || names[len(names)-1] != 0 {
			goto corrupt
		}
		i := bytes.IndexByte(names, '\x00')
		if i < 0 {
			goto corrupt
		}
		newName, target := names[0:i], names[i+1:len(names)-1]
		req = &SymlinkRequest{
			Header:  hdr,
			NewName: string(newName),
			Target:  string(target),
		}

	case opLink:
		var in linkIn
		if len(buf) < linkInSize {
			goto corrupt
		}
		in.Oldnodeid = binary.LittleEndian.Uint64(buf[0:8])
		newName := buf[linkInSize:]
		if len(newName) < 2 || newName[len(newName)-1] != '\x00' {
			goto corrupt
		}
		req = &LinkRequest{
			Header:  hdr,
			OldNode: NodeID(in.Oldnodeid),
			NewName: string(newName[:len(newName)-1]),
		}

	case opMknod:
		var in mknodIn
		if len(buf) < mknodInSize {
			goto corrupt
		}
		in.Mode = binary.LittleEndian.Uint32(buf[0:4])
		in.Rdev = binary.LittleEndian.Uint32(buf[4:8])
		name := buf[mknodInSize:]
		if len(name) < 2 || name[len(name)-1] != '\x00' {
			goto corrupt
		}
		req = &MknodRequest{
			Header: hdr,
			Mode:   fileMode(in.Mode),
			Rdev:   in.Rdev,
			Name:   string(name),
		}

	case opMkdir:
		var in mkdirIn
		if len(buf) < mkdirInSize {
			goto corrupt
		}
		in.Mode = binary.LittleEndian.Uint32(buf[0:4])
		in.Padding = binary.LittleEndian.Uint32(buf[4:8])
		name := buf[mkdirInSize:]
		i := bytes.IndexByte(name, '\x00')
		if i < 0 {
			goto corrupt
		}
		req = &MkdirRequest{
			Header: hdr,
			Name:   string(name[:i]),
			// observed on Linux: mkdirIn.Mode & syscall.S_IFMT == 0,
			// and this causes fileMode to go into it's "no idea"
			// code branch; enforce type to directory
			Mode: fileMode((in.Mode &^ syscall.S_IFMT) | syscall.S_IFDIR),
		}
	case opUnlink, opRmdir:
		buf := buf
		n := len(buf)
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		req = &RemoveRequest{
			Header: hdr,
			Name:   string(buf[:n-1]),
			Dir:    hdr.Opcode == opRmdir,
		}

	case opRename:
		var in renameIn
		if len(buf) < renameInSize {
			goto corrupt
		}
		in.Newdir = binary.LittleEndian.Uint64(buf[0:8])
		newDirNodeID := NodeID(in.Newdir)
		// buf is "old\0new\0"
		names := buf[renameInSize:]
		if len(names) == 0 || names[len(names)-1] != 0 {
			goto corrupt
		}
		i := bytes.IndexByte(names, '\x00')
		if i < 0 {
			goto corrupt
		}
		oldName, newName := names[0:i], names[i+1:len(names)-1]
		req = &RenameRequest{
			Header:  hdr,
			NewDir:  newDirNodeID,
			OldName: string(oldName),
			NewName: string(newName),
		}

	case opOpendir, opOpen:
		var in openIn
		if len(buf) < openInSize {
			goto corrupt
		}
		in.Flags = binary.LittleEndian.Uint32(buf[0:4])
		req = &OpenRequest{
			Header: hdr,
			Dir:    hdr.Opcode == opOpendir,
			Flags:  openFlags(in.Flags),
		}

	case opRead, opReaddir:
		var in readIn
		if len(buf) < readInSize {
			goto corrupt
		}
		in.Fh = binary.LittleEndian.Uint64(buf[0:8])
		in.Offset = binary.LittleEndian.Uint64(buf[8:16])
		in.Size = binary.LittleEndian.Uint32(buf[16:20])
		in.Padding = binary.LittleEndian.Uint32(buf[20:24])
		req = &ReadRequest{
			Header: hdr,
			Dir:    hdr.Opcode == opReaddir,
			Handle: HandleID(in.Fh),
			Offset: int64(in.Offset),
			Size:   int(in.Size),
		}

	case opWrite:
		/*
			in := (*writeIn)(m.data())
			if m.len() < unsafe.Sizeof(*in) {
				goto corrupt
			}
			r := &WriteRequest{
				Header: hdr,
				Handle: HandleID(in.Fh),
				Offset: int64(in.Offset),
				Flags:  WriteFlags(in.WriteFlags),
			}
			buf := buf[unsafe.Sizeof(*in):]
			if uint32(len(buf)) < in.Size {
				goto corrupt
			}
			r.Data = buf
			req = r
		*/
	case opStatfs:
		req = &StatfsRequest{
			Header: hdr,
		}

	case opRelease, opReleasedir:
		var in releaseIn
		if len(buf) < releaseInSize {
			goto corrupt
		}
		in.Fh = binary.LittleEndian.Uint64(buf[0:8])
		in.Flags = binary.LittleEndian.Uint32(buf[8:12])
		in.ReleaseFlags = binary.LittleEndian.Uint32(buf[12:16])
		in.LockOwner = binary.LittleEndian.Uint32(buf[16:20])
		req = &ReleaseRequest{
			Header:       hdr,
			Dir:          hdr.Opcode == opReleasedir,
			Handle:       HandleID(in.Fh),
			Flags:        openFlags(in.Flags),
			ReleaseFlags: ReleaseFlags(in.ReleaseFlags),
			LockOwner:    in.LockOwner,
		}

	case opFsync, opFsyncdir:
		var in fsyncIn
		if len(buf) < fsyncInSize {
			goto corrupt
		}
		in.Fh = binary.LittleEndian.Uint64(buf[0:8])
		in.FsyncFlags = binary.LittleEndian.Uint32(buf[8:12])
		in.Padding = binary.LittleEndian.Uint32(buf[12:16])
		req = &FsyncRequest{
			Dir:    hdr.Opcode == opFsyncdir,
			Header: hdr,
			Handle: HandleID(in.Fh),
			Flags:  in.FsyncFlags,
		}

	case opSetxattr:
		var in setxattrIn
		if len(buf) < setxattrInSize {
			goto corrupt
		}
		in.Size = binary.LittleEndian.Uint32(buf[0:4])
		in.Flags = binary.LittleEndian.Uint32(buf[4:8])
		name := buf[setxattrInSize:]
		i := bytes.IndexByte(name, '\x00')
		if i < 0 {
			goto corrupt
		}
		xattr := name[i+1:]
		if uint32(len(xattr)) < in.Size {
			goto corrupt
		}
		xattr = xattr[:in.Size]
		req = &SetxattrRequest{
			Header:   hdr,
			Flags:    in.Flags,
			Position: in.position(),
			Name:     string(name[:i]),
			Xattr:    xattr,
		}

	case opGetxattr:
		var in getxattrIn
		if len(buf) < getxattrInSize {
			goto corrupt
		}
		in.Size = binary.LittleEndian.Uint32(buf[0:4])
		name := buf[getxattrInSize:]
		i := bytes.IndexByte(name, '\x00')
		if i < 0 {
			goto corrupt
		}
		req = &GetxattrRequest{
			Header:   hdr,
			Name:     string(name[:i]),
			Size:     in.Size,
			Position: in.position(),
		}

	case opListxattr:
		var in getxattrIn
		if len(buf) < getxattrInSize {
			goto corrupt
		}
		in.Size = binary.LittleEndian.Uint32(buf[0:4])
		req = &ListxattrRequest{
			Header:   hdr,
			Size:     in.Size,
			Position: in.position(),
		}

	case opRemovexattr:
		buf := buf
		n := len(buf)
		if n == 0 || buf[n-1] != '\x00' {
			goto corrupt
		}
		req = &RemovexattrRequest{
			Header: hdr,
			Name:   string(buf[:n-1]),
		}

	case opFlush:
		var in flushIn
		if len(buf) < flushInSize {
			goto corrupt
		}
		in.Fh = binary.LittleEndian.Uint64(buf[0:8])
		in.FlushFlags = binary.LittleEndian.Uint32(buf[8:12])
		in.Padding = binary.LittleEndian.Uint32(buf[12:16])
		in.LockOwner = binary.LittleEndian.Uint64(buf[16:24])
		req = &FlushRequest{
			Header:    hdr,
			Handle:    HandleID(in.Fh),
			Flags:     in.FlushFlags,
			LockOwner: in.LockOwner,
		}

	case opInit:
		var in initIn
		if len(buf) < initInSize {
			goto corrupt
		}
		in.Major = binary.LittleEndian.Uint32(buf[0:4])
		in.Minor = binary.LittleEndian.Uint32(buf[4:8])
		in.MaxReadahead = binary.LittleEndian.Uint32(buf[8:12])
		in.Flags = binary.LittleEndian.Uint32(buf[12:16])
		req = &InitRequest{
			Header:       hdr,
			Major:        in.Major,
			Minor:        in.Minor,
			MaxReadahead: in.MaxReadahead,
			Flags:        InitFlags(in.Flags),
		}

	case opGetlk:
		panic("opGetlk")
	case opSetlk:
		panic("opSetlk")
	case opSetlkw:
		panic("opSetlkw")

	case opAccess:
		var in accessIn
		if len(buf) < accessInSize {
			goto corrupt
		}
		in.Mask = binary.LittleEndian.Uint32(buf[0:4])
		req = &AccessRequest{
			Header: hdr,
			Mask:   in.Mask,
		}

	case opCreate:
		var in createIn
		if len(buf) < createInSize {
			goto corrupt
		}
		in.Flags = binary.LittleEndian.Uint32(buf[0:4])
		in.Mode = binary.LittleEndian.Uint32(buf[4:8])
		name := buf[createInSize:]
		i := bytes.IndexByte(name, '\x00')
		if i < 0 {
			goto corrupt
		}
		req = &CreateRequest{
			Header: hdr,
			Flags:  openFlags(in.Flags),
			Mode:   fileMode(in.Mode),
			Name:   string(name[:i]),
		}

	case opInterrupt:
		var in interruptIn
		if len(buf) < interruptInSize {
			goto corrupt
		}
		in.Unique = binary.LittleEndian.Uint64(buf[0:8])
		req = &InterruptRequest{
			Header: hdr,
			IntrID: RequestID(in.Unique),
		}

	case opBmap:
		panic("opBmap")

	case opDestroy:
		req = &DestroyRequest{
			Header: hdr,
		}

	// OS X
	case opSetvolname:
		panic("opSetvolname")
	case opGetxtimes:
		panic("opGetxtimes")
	case opExchange:
		panic("opExchange")
	}

	return req, nil

corrupt:
	Debug(malformedMessage{})
	return nil, fmt.Errorf("fuse: malformed message")

unrecognized:
	// Unrecognized message.
	// Assume higher-level code will send a "no idea what you mean" error.
	h := new(Header)
	*h = hdr
	return h, nil
}

type bugShortKernelWrite struct {
	Written int64
	Length  int64
	Error   string
	Stack   string
}

func (b bugShortKernelWrite) String() string {
	return fmt.Sprintf("short kernel write: written=%d/%d error=%q stack=\n%s", b.Written, b.Length, b.Error, b.Stack)
}

// safe to call even with nil error
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *Conn) respond(out *outHeader, n uintptr) {
	c.wio.Lock()
	defer c.wio.Unlock()
	out.Len = uint32(n)
	msg := (*[1 << 30]byte)(unsafe.Pointer(out))[:n]
	nn, err := syscall.Write(c.fd(), msg)
	if nn != len(msg) || err != nil {
		Debug(bugShortKernelWrite{
			Written: int64(nn),
			Length:  int64(len(msg)),
			Error:   errorString(err),
			Stack:   stack(),
		})
	}
}

func (c *Conn) respondData(out *outHeader, n uintptr, data []byte) {
	c.wio.Lock()
	defer c.wio.Unlock()
	// TODO: use writev
	out.Len = uint32(n + uintptr(len(data)))
	msg := make([]byte, out.Len)
	copy(msg, (*[1 << 30]byte)(unsafe.Pointer(out))[:n])
	copy(msg[n:], data)
	syscall.Write(c.fd(), msg)
}

func (c *Conn) respondSafe(out *outHeader, data []byte) {
	out.Len = uint32(outHeaderSize + len(data))
	c.wio.Lock()
	writev(c.fd(), [][]byte{(*[1 << 30]byte)(unsafe.Pointer(out))[:outHeaderSize], data})
	c.wio.Unlock()
}

// An InitRequest is the first request sent on a FUSE file system.
type InitRequest struct {
	Header `json:"-"`
	Major  uint32
	Minor  uint32
	// Maximum readahead in bytes that the kernel plans to use.
	MaxReadahead uint32
	Flags        InitFlags
}

var _ = Request(&InitRequest{})

func (r *InitRequest) String() string {
	return fmt.Sprintf("Init [%s] %d.%d ra=%d fl=%v", &r.Header, r.Major, r.Minor, r.MaxReadahead, r.Flags)
}

// An InitResponse is the response to an InitRequest.
type InitResponse struct {
	// Maximum readahead in bytes that the kernel can use. Ignored if
	// greater than InitRequest.MaxReadahead.
	MaxReadahead uint32
	Flags        InitFlags
	// Maximum size of a single write operation.
	// Linux enforces a minimum of 4 KiB.
	MaxWrite uint32
}

func (r *InitResponse) String() string {
	return fmt.Sprintf("Init %+v", *r)
}

// Respond replies to the request with the given response.
func (r *InitRequest) Respond(resp *InitResponse) {
	out := &initOut{
		outHeader:    outHeader{Unique: uint64(r.ID)},
		Major:        kernelVersion,
		Minor:        kernelMinorVersion,
		MaxReadahead: resp.MaxReadahead,
		Flags:        uint32(resp.Flags),
		MaxWrite:     resp.MaxWrite,
	}
	// MaxWrite larger than our receive buffer would just lead to
	// errors on large writes.
	if out.MaxWrite > maxWrite {
		out.MaxWrite = maxWrite
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A StatfsRequest requests information about the mounted file system.
type StatfsRequest struct {
	Header `json:"-"`
}

var _ = Request(&StatfsRequest{})

func (r *StatfsRequest) String() string {
	return fmt.Sprintf("Statfs [%s]", &r.Header)
}

// Respond replies to the request with the given response.
func (r *StatfsRequest) Respond(resp *StatfsResponse) {
	out := &statfsOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		St: kstatfs{
			Blocks:  resp.Blocks,
			Bfree:   resp.Bfree,
			Bavail:  resp.Bavail,
			Files:   resp.Files,
			Bsize:   resp.Bsize,
			Namelen: resp.Namelen,
			Frsize:  resp.Frsize,
		},
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A StatfsResponse is the response to a StatfsRequest.
type StatfsResponse struct {
	Blocks  uint64 // Total data blocks in file system.
	Bfree   uint64 // Free blocks in file system.
	Bavail  uint64 // Free blocks in file system if you're not root.
	Files   uint64 // Total files in file system.
	Ffree   uint64 // Free files in file system.
	Bsize   uint32 // Block size
	Namelen uint32 // Maximum file name length?
	Frsize  uint32 // Fragment size, smallest addressable data size in the file system.
}

func (r *StatfsResponse) String() string {
	return fmt.Sprintf("Statfs %+v", *r)
}

// An AccessRequest asks whether the file can be accessed
// for the purpose specified by the mask.
type AccessRequest struct {
	Header `json:"-"`
	Mask   uint32
}

var _ = Request(&AccessRequest{})

func (r *AccessRequest) String() string {
	return fmt.Sprintf("Access [%s] mask=%#x", &r.Header, r.Mask)
}

// Respond replies to the request indicating that access is allowed.
// To deny access, use RespondError.
func (r *AccessRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// An Attr is the metadata for a single file or directory.
type Attr struct {
	Inode  uint64      // inode number
	Size   uint64      // size in bytes
	Blocks uint64      // size in blocks
	Atime  time.Time   // time of last access
	Mtime  time.Time   // time of last modification
	Ctime  time.Time   // time of last inode change
	Crtime time.Time   // time of creation (OS X only)
	Mode   os.FileMode // file mode
	Nlink  uint32      // number of links
	Uid    uint32      // owner uid
	Gid    uint32      // group gid
	Rdev   uint32      // device numbers
	Flags  uint32      // chflags(2) flags (OS X only)
}

func unix(t time.Time) (sec uint64, nsec uint32) {
	nano := t.UnixNano()
	sec = uint64(nano / 1e9)
	nsec = uint32(nano % 1e9)
	return
}

func (a *Attr) attr() (out attr) {
	out.Ino = a.Inode
	out.Size = a.Size
	out.Blocks = a.Blocks
	out.Atime, out.AtimeNsec = unix(a.Atime)
	out.Mtime, out.MtimeNsec = unix(a.Mtime)
	out.Ctime, out.CtimeNsec = unix(a.Ctime)
	out.SetCrtime(unix(a.Crtime))
	out.Mode = uint32(a.Mode) & 0777
	switch {
	default:
		out.Mode |= syscall.S_IFREG
	case a.Mode&os.ModeDir != 0:
		out.Mode |= syscall.S_IFDIR
	case a.Mode&os.ModeDevice != 0:
		if a.Mode&os.ModeCharDevice != 0 {
			out.Mode |= syscall.S_IFCHR
		} else {
			out.Mode |= syscall.S_IFBLK
		}
	case a.Mode&os.ModeNamedPipe != 0:
		out.Mode |= syscall.S_IFIFO
	case a.Mode&os.ModeSymlink != 0:
		out.Mode |= syscall.S_IFLNK
	case a.Mode&os.ModeSocket != 0:
		out.Mode |= syscall.S_IFSOCK
	}
	if a.Mode&os.ModeSetuid != 0 {
		out.Mode |= syscall.S_ISUID
	}
	if a.Mode&os.ModeSetgid != 0 {
		out.Mode |= syscall.S_ISGID
	}
	out.Nlink = a.Nlink
	out.Uid = a.Uid
	out.Gid = a.Gid
	out.Rdev = a.Rdev
	out.SetFlags(a.Flags)

	return
}

// A GetattrRequest asks for the metadata for the file denoted by r.Node.
type GetattrRequest struct {
	Header `json:"-"`
}

var _ = Request(&GetattrRequest{})

func (r *GetattrRequest) String() string {
	return fmt.Sprintf("Getattr [%s]", &r.Header)
}

// Respond replies to the request with the given response.
func (r *GetattrRequest) Respond(resp *GetattrResponse) {
	out := &attrOut{
		outHeader:     outHeader{Unique: uint64(r.ID)},
		AttrValid:     uint64(resp.AttrValid / time.Second),
		AttrValidNsec: uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:          resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A GetattrResponse is the response to a GetattrRequest.
type GetattrResponse struct {
	AttrValid time.Duration // how long Attr can be cached
	Attr      Attr          // file attributes
}

func (r *GetattrResponse) String() string {
	return fmt.Sprintf("Getattr %+v", *r)
}

// A GetxattrRequest asks for the extended attributes associated with r.Node.
type GetxattrRequest struct {
	Header `json:"-"`

	// Maximum size to return.
	Size uint32

	// Name of the attribute requested.
	Name string

	// Offset within extended attributes.
	//
	// Only valid for OS X, and then only with the resource fork
	// attribute.
	Position uint32
}

var _ = Request(&GetxattrRequest{})

func (r *GetxattrRequest) String() string {
	return fmt.Sprintf("Getxattr [%s] %q %d @%d", &r.Header, r.Name, r.Size, r.Position)
}

// Respond replies to the request with the given response.
func (r *GetxattrRequest) Respond(resp *GetxattrResponse) {
	if r.Size == 0 {
		out := &getxattrOut{
			outHeader: outHeader{Unique: uint64(r.ID)},
			Size:      uint32(len(resp.Xattr)),
		}
		r.respond(&out.outHeader, unsafe.Sizeof(*out))
	} else {
		out := &outHeader{Unique: uint64(r.ID)}
		r.respondData(out, unsafe.Sizeof(*out), resp.Xattr)
	}
}

// A GetxattrResponse is the response to a GetxattrRequest.
type GetxattrResponse struct {
	Xattr []byte
}

func (r *GetxattrResponse) String() string {
	return fmt.Sprintf("Getxattr %x", r.Xattr)
}

// A ListxattrRequest asks to list the extended attributes associated with r.Node.
type ListxattrRequest struct {
	Header   `json:"-"`
	Size     uint32 // maximum size to return
	Position uint32 // offset within attribute list
}

var _ = Request(&ListxattrRequest{})

func (r *ListxattrRequest) String() string {
	return fmt.Sprintf("Listxattr [%s] %d @%d", &r.Header, r.Size, r.Position)
}

// Respond replies to the request with the given response.
func (r *ListxattrRequest) Respond(resp *ListxattrResponse) {
	if r.Size == 0 {
		out := &getxattrOut{
			outHeader: outHeader{Unique: uint64(r.ID)},
			Size:      uint32(len(resp.Xattr)),
		}
		r.respond(&out.outHeader, unsafe.Sizeof(*out))
	} else {
		out := &outHeader{Unique: uint64(r.ID)}
		r.respondData(out, unsafe.Sizeof(*out), resp.Xattr)
	}
}

// A ListxattrResponse is the response to a ListxattrRequest.
type ListxattrResponse struct {
	Xattr []byte
}

func (r *ListxattrResponse) String() string {
	return fmt.Sprintf("Listxattr %x", r.Xattr)
}

// Append adds an extended attribute name to the response.
func (r *ListxattrResponse) Append(names ...string) {
	for _, name := range names {
		r.Xattr = append(r.Xattr, name...)
		r.Xattr = append(r.Xattr, '\x00')
	}
}

// A RemovexattrRequest asks to remove an extended attribute associated with r.Node.
type RemovexattrRequest struct {
	Header `json:"-"`
	Name   string // name of extended attribute
}

var _ = Request(&RemovexattrRequest{})

func (r *RemovexattrRequest) String() string {
	return fmt.Sprintf("Removexattr [%s] %q", &r.Header, r.Name)
}

// Respond replies to the request, indicating that the attribute was removed.
func (r *RemovexattrRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A SetxattrRequest asks to set an extended attribute associated with a file.
type SetxattrRequest struct {
	Header `json:"-"`

	// Flags can make the request fail if attribute does/not already
	// exist. Unfortunately, the constants are platform-specific and
	// not exposed by Go1.2. Look for XATTR_CREATE, XATTR_REPLACE.
	//
	// TODO improve this later
	//
	// TODO XATTR_CREATE and exist -> EEXIST
	//
	// TODO XATTR_REPLACE and not exist -> ENODATA
	Flags uint32

	// Offset within extended attributes.
	//
	// Only valid for OS X, and then only with the resource fork
	// attribute.
	Position uint32

	Name  string
	Xattr []byte
}

var _ = Request(&SetxattrRequest{})

func trunc(b []byte, max int) ([]byte, string) {
	if len(b) > max {
		return b[:max], "..."
	}
	return b, ""
}

func (r *SetxattrRequest) String() string {
	xattr, tail := trunc(r.Xattr, 16)
	return fmt.Sprintf("Setxattr [%s] %q %x%s fl=%v @%#x", &r.Header, r.Name, xattr, tail, r.Flags, r.Position)
}

// Respond replies to the request, indicating that the extended attribute was set.
func (r *SetxattrRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A LookupRequest asks to look up the given name in the directory named by r.Node.
type LookupRequest struct {
	Header `json:"-"`
	Name   string
}

var _ = Request(&LookupRequest{})

func (r *LookupRequest) String() string {
	return fmt.Sprintf("Lookup [%s] %q", &r.Header, r.Name)
}

// Respond replies to the request with the given response.
func (r *LookupRequest) Respond(resp *LookupResponse) {
	out := &entryOut{
		outHeader:      outHeader{Unique: uint64(r.ID)},
		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A LookupResponse is the response to a LookupRequest.
type LookupResponse struct {
	Node       NodeID
	Generation uint64
	EntryValid time.Duration
	AttrValid  time.Duration
	Attr       Attr
}

func (r *LookupResponse) String() string {
	return fmt.Sprintf("Lookup %+v", *r)
}

// An OpenRequest asks to open a file or directory
type OpenRequest struct {
	Header `json:"-"`
	Dir    bool // is this Opendir?
	Flags  OpenFlags
}

var _ = Request(&OpenRequest{})

func (r *OpenRequest) String() string {
	return fmt.Sprintf("Open [%s] dir=%v fl=%v", &r.Header, r.Dir, r.Flags)
}

// Respond replies to the request with the given response.
func (r *OpenRequest) Respond(resp *OpenResponse) {
	out := &openOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Fh:        uint64(resp.Handle),
		OpenFlags: uint32(resp.Flags),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A OpenResponse is the response to a OpenRequest.
type OpenResponse struct {
	Handle HandleID
	Flags  OpenResponseFlags
}

func (r *OpenResponse) String() string {
	return fmt.Sprintf("Open %+v", *r)
}

// A CreateRequest asks to create and open a file (not a directory).
type CreateRequest struct {
	Header `json:"-"`
	Name   string
	Flags  OpenFlags
	Mode   os.FileMode
}

var _ = Request(&CreateRequest{})

func (r *CreateRequest) String() string {
	return fmt.Sprintf("Create [%s] %q fl=%v mode=%v", &r.Header, r.Name, r.Flags, r.Mode)
}

// Respond replies to the request with the given response.
func (r *CreateRequest) Respond(resp *CreateResponse) {
	out := &createOut{
		outHeader: outHeader{Unique: uint64(r.ID)},

		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),

		Fh:        uint64(resp.Handle),
		OpenFlags: uint32(resp.Flags),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A CreateResponse is the response to a CreateRequest.
// It describes the created node and opened handle.
type CreateResponse struct {
	LookupResponse
	OpenResponse
}

func (r *CreateResponse) String() string {
	return fmt.Sprintf("Create %+v", *r)
}

// A MkdirRequest asks to create (but not open) a directory.
type MkdirRequest struct {
	Header `json:"-"`
	Name   string
	Mode   os.FileMode
}

var _ = Request(&MkdirRequest{})

func (r *MkdirRequest) String() string {
	return fmt.Sprintf("Mkdir [%s] %q mode=%v", &r.Header, r.Name, r.Mode)
}

// Respond replies to the request with the given response.
func (r *MkdirRequest) Respond(resp *MkdirResponse) {
	out := &entryOut{
		outHeader:      outHeader{Unique: uint64(r.ID)},
		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A MkdirResponse is the response to a MkdirRequest.
type MkdirResponse struct {
	LookupResponse
}

func (r *MkdirResponse) String() string {
	return fmt.Sprintf("Mkdir %+v", *r)
}

// A ReadRequest asks to read from an open file.
type ReadRequest struct {
	Header `json:"-"`
	Dir    bool // is this Readdir?
	Handle HandleID
	Offset int64
	Size   int
}

var _ = Request(&ReadRequest{})

func (r *ReadRequest) String() string {
	return fmt.Sprintf("Read [%s] %#x %d @%#x dir=%v", &r.Header, r.Handle, r.Size, r.Offset, r.Dir)
}

// Respond replies to the request with the given response.
func (r *ReadRequest) Respond(resp *ReadResponse) {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respondSafe(out, resp.Data)
}

// A ReadResponse is the response to a ReadRequest.
type ReadResponse struct {
	Data []byte
}

func (r *ReadResponse) String() string {
	return fmt.Sprintf("Read %d", len(r.Data))
}

type jsonReadResponse struct {
	Len uint64
}

func (r *ReadResponse) MarshalJSON() ([]byte, error) {
	j := jsonReadResponse{
		Len: uint64(len(r.Data)),
	}
	return json.Marshal(j)
}

// A ReleaseRequest asks to release (close) an open file handle.
type ReleaseRequest struct {
	Header       `json:"-"`
	Dir          bool // is this Releasedir?
	Handle       HandleID
	Flags        OpenFlags // flags from OpenRequest
	ReleaseFlags ReleaseFlags
	LockOwner    uint32
}

var _ = Request(&ReleaseRequest{})

func (r *ReleaseRequest) String() string {
	return fmt.Sprintf("Release [%s] %#x fl=%v rfl=%v owner=%#x", &r.Header, r.Handle, r.Flags, r.ReleaseFlags, r.LockOwner)
}

// Respond replies to the request, indicating that the handle has been released.
func (r *ReleaseRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A DestroyRequest is sent by the kernel when unmounting the file system.
// No more requests will be received after this one, but it should still be
// responded to.
type DestroyRequest struct {
	Header `json:"-"`
}

var _ = Request(&DestroyRequest{})

func (r *DestroyRequest) String() string {
	return fmt.Sprintf("Destroy [%s]", &r.Header)
}

// Respond replies to the request.
func (r *DestroyRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A ForgetRequest is sent by the kernel when forgetting about r.Node
// as returned by r.N lookup requests.
type ForgetRequest struct {
	Header `json:"-"`
	N      uint64
}

var _ = Request(&ForgetRequest{})

func (r *ForgetRequest) String() string {
	return fmt.Sprintf("Forget [%s] %d", &r.Header, r.N)
}

// Respond replies to the request, indicating that the forgetfulness has been recorded.
func (r *ForgetRequest) Respond() {
	// Don't reply to forget messages.
	r.noResponse()
}

// A Dirent represents a single directory entry.
type Dirent struct {
	// Inode this entry names.
	Inode uint64

	// Type of the entry, for example DT_File.
	//
	// Setting this is optional. The zero value (DT_Unknown) means
	// callers will just need to do a Getattr when the type is
	// needed. Providing a type can speed up operations
	// significantly.
	Type DirentType

	// Name of the entry
	Name string
}

// Type of an entry in a directory listing.
type DirentType uint32

const (
	// These don't quite match os.FileMode; especially there's an
	// explicit unknown, instead of zero value meaning file. They
	// are also not quite syscall.DT_*; nothing says the FUSE
	// protocol follows those, and even if they were, we don't
	// want each fs to fiddle with syscall.

	// The shift by 12 is hardcoded in the FUSE userspace
	// low-level C library, so it's safe here.

	DT_Unknown DirentType = 0
	DT_Socket  DirentType = syscall.S_IFSOCK >> 12
	DT_Link    DirentType = syscall.S_IFLNK >> 12
	DT_File    DirentType = syscall.S_IFREG >> 12
	DT_Block   DirentType = syscall.S_IFBLK >> 12
	DT_Dir     DirentType = syscall.S_IFDIR >> 12
	DT_Char    DirentType = syscall.S_IFCHR >> 12
	DT_FIFO    DirentType = syscall.S_IFIFO >> 12
)

func (t DirentType) String() string {
	switch t {
	case DT_Unknown:
		return "unknown"
	case DT_Socket:
		return "socket"
	case DT_Link:
		return "link"
	case DT_File:
		return "file"
	case DT_Block:
		return "block"
	case DT_Dir:
		return "dir"
	case DT_Char:
		return "char"
	case DT_FIFO:
		return "fifo"
	}
	return "invalid"
}

// AppendDirent appends the encoded form of a directory entry to data
// and returns the resulting slice.
func AppendDirent(data []byte, dir Dirent) []byte {
	de := dirent{
		Ino:     dir.Inode,
		Namelen: uint32(len(dir.Name)),
		Type:    uint32(dir.Type),
	}
	de.Off = uint64(len(data) + direntSize + (len(dir.Name)+7)&^7)
	data = append(data, (*[direntSize]byte)(unsafe.Pointer(&de))[:]...)
	data = append(data, dir.Name...)
	n := direntSize + uintptr(len(dir.Name))
	if n%8 != 0 {
		var pad [8]byte
		data = append(data, pad[:8-n%8]...)
	}
	return data
}

// A WriteRequest asks to write to an open file.
type WriteRequest struct {
	Header
	Handle HandleID
	Offset int64
	Data   []byte
	Flags  WriteFlags
}

var _ = Request(&WriteRequest{})

func (r *WriteRequest) String() string {
	return fmt.Sprintf("Write [%s] %#x %d @%d fl=%v", &r.Header, r.Handle, len(r.Data), r.Offset, r.Flags)
}

type jsonWriteRequest struct {
	Handle HandleID
	Offset int64
	Len    uint64
	Flags  WriteFlags
}

func (r *WriteRequest) MarshalJSON() ([]byte, error) {
	j := jsonWriteRequest{
		Handle: r.Handle,
		Offset: r.Offset,
		Len:    uint64(len(r.Data)),
		Flags:  r.Flags,
	}
	return json.Marshal(j)
}

// Respond replies to the request with the given response.
func (r *WriteRequest) Respond(resp *WriteResponse) {
	out := &writeOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		Size:      uint32(resp.Size),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A WriteResponse replies to a write indicating how many bytes were written.
type WriteResponse struct {
	Size int
}

func (r *WriteResponse) String() string {
	return fmt.Sprintf("Write %+v", *r)
}

// A SetattrRequest asks to change one or more attributes associated with a file,
// as indicated by Valid.
type SetattrRequest struct {
	Header `json:"-"`
	Valid  SetattrValid
	Handle HandleID
	Size   uint64
	Atime  time.Time
	Mtime  time.Time
	Mode   os.FileMode
	Uid    uint32
	Gid    uint32

	// OS X only
	Bkuptime time.Time
	Chgtime  time.Time
	Crtime   time.Time
	Flags    uint32 // see chflags(2)
}

var _ = Request(&SetattrRequest{})

func (r *SetattrRequest) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Setattr [%s]", &r.Header)
	if r.Valid.Mode() {
		fmt.Fprintf(&buf, " mode=%v", r.Mode)
	}
	if r.Valid.Uid() {
		fmt.Fprintf(&buf, " uid=%d", r.Uid)
	}
	if r.Valid.Gid() {
		fmt.Fprintf(&buf, " gid=%d", r.Gid)
	}
	if r.Valid.Size() {
		fmt.Fprintf(&buf, " size=%d", r.Size)
	}
	if r.Valid.Atime() {
		fmt.Fprintf(&buf, " atime=%v", r.Atime)
	}
	if r.Valid.AtimeNow() {
		fmt.Fprintf(&buf, " atime=now")
	}
	if r.Valid.Mtime() {
		fmt.Fprintf(&buf, " mtime=%v", r.Mtime)
	}
	if r.Valid.MtimeNow() {
		fmt.Fprintf(&buf, " mtime=now")
	}
	if r.Valid.Handle() {
		fmt.Fprintf(&buf, " handle=%#x", r.Handle)
	} else {
		fmt.Fprintf(&buf, " handle=INVALID-%#x", r.Handle)
	}
	if r.Valid.LockOwner() {
		fmt.Fprintf(&buf, " lockowner")
	}
	if r.Valid.Crtime() {
		fmt.Fprintf(&buf, " crtime=%v", r.Crtime)
	}
	if r.Valid.Chgtime() {
		fmt.Fprintf(&buf, " chgtime=%v", r.Chgtime)
	}
	if r.Valid.Bkuptime() {
		fmt.Fprintf(&buf, " bkuptime=%v", r.Bkuptime)
	}
	if r.Valid.Flags() {
		fmt.Fprintf(&buf, " flags=%#x", r.Flags)
	}
	return buf.String()
}

// Respond replies to the request with the given response,
// giving the updated attributes.
func (r *SetattrRequest) Respond(resp *SetattrResponse) {
	out := &attrOut{
		outHeader:     outHeader{Unique: uint64(r.ID)},
		AttrValid:     uint64(resp.AttrValid / time.Second),
		AttrValidNsec: uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:          resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A SetattrResponse is the response to a SetattrRequest.
type SetattrResponse struct {
	AttrValid time.Duration // how long Attr can be cached
	Attr      Attr          // file attributes
}

func (r *SetattrResponse) String() string {
	return fmt.Sprintf("Setattr %+v", *r)
}

// A FlushRequest asks for the current state of an open file to be flushed
// to storage, as when a file descriptor is being closed.  A single opened Handle
// may receive multiple FlushRequests over its lifetime.
type FlushRequest struct {
	Header    `json:"-"`
	Handle    HandleID
	Flags     uint32
	LockOwner uint64
}

var _ = Request(&FlushRequest{})

func (r *FlushRequest) String() string {
	return fmt.Sprintf("Flush [%s] %#x fl=%#x lk=%#x", &r.Header, r.Handle, r.Flags, r.LockOwner)
}

// Respond replies to the request, indicating that the flush succeeded.
func (r *FlushRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A RemoveRequest asks to remove a file or directory from the
// directory r.Node.
type RemoveRequest struct {
	Header `json:"-"`
	Name   string // name of the entry to remove
	Dir    bool   // is this rmdir?
}

var _ = Request(&RemoveRequest{})

func (r *RemoveRequest) String() string {
	return fmt.Sprintf("Remove [%s] %q dir=%v", &r.Header, r.Name, r.Dir)
}

// Respond replies to the request, indicating that the file was removed.
func (r *RemoveRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// A SymlinkRequest is a request to create a symlink making NewName point to Target.
type SymlinkRequest struct {
	Header          `json:"-"`
	NewName, Target string
}

var _ = Request(&SymlinkRequest{})

func (r *SymlinkRequest) String() string {
	return fmt.Sprintf("Symlink [%s] from %q to target %q", &r.Header, r.NewName, r.Target)
}

// Respond replies to the request, indicating that the symlink was created.
func (r *SymlinkRequest) Respond(resp *SymlinkResponse) {
	out := &entryOut{
		outHeader:      outHeader{Unique: uint64(r.ID)},
		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A SymlinkResponse is the response to a SymlinkRequest.
type SymlinkResponse struct {
	LookupResponse
}

// A ReadlinkRequest is a request to read a symlink's target.
type ReadlinkRequest struct {
	Header `json:"-"`
}

var _ = Request(&ReadlinkRequest{})

func (r *ReadlinkRequest) String() string {
	return fmt.Sprintf("Readlink [%s]", &r.Header)
}

func (r *ReadlinkRequest) Respond(target string) {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respondSafe(out, []byte(target))
}

// A LinkRequest is a request to create a hard link.
type LinkRequest struct {
	Header  `json:"-"`
	OldNode NodeID
	NewName string
}

var _ = Request(&LinkRequest{})

func (r *LinkRequest) String() string {
	return fmt.Sprintf("Link [%s] node %d to %q", &r.Header, r.OldNode, r.NewName)
}

func (r *LinkRequest) Respond(resp *LookupResponse) {
	out := &entryOut{
		outHeader:      outHeader{Unique: uint64(r.ID)},
		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A RenameRequest is a request to rename a file.
type RenameRequest struct {
	Header           `json:"-"`
	NewDir           NodeID
	OldName, NewName string
}

var _ = Request(&RenameRequest{})

func (r *RenameRequest) String() string {
	return fmt.Sprintf("Rename [%s] from %q to dirnode %d %q", &r.Header, r.OldName, r.NewDir, r.NewName)
}

func (r *RenameRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

type MknodRequest struct {
	Header `json:"-"`
	Name   string
	Mode   os.FileMode
	Rdev   uint32
}

var _ = Request(&MknodRequest{})

func (r *MknodRequest) String() string {
	return fmt.Sprintf("Mknod [%s] Name %q mode %v rdev %d", &r.Header, r.Name, r.Mode, r.Rdev)
}

func (r *MknodRequest) Respond(resp *LookupResponse) {
	out := &entryOut{
		outHeader:      outHeader{Unique: uint64(r.ID)},
		Nodeid:         uint64(resp.Node),
		Generation:     resp.Generation,
		EntryValid:     uint64(resp.EntryValid / time.Second),
		EntryValidNsec: uint32(resp.EntryValid % time.Second / time.Nanosecond),
		AttrValid:      uint64(resp.AttrValid / time.Second),
		AttrValidNsec:  uint32(resp.AttrValid % time.Second / time.Nanosecond),
		Attr:           resp.Attr.attr(),
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

type FsyncRequest struct {
	Header `json:"-"`
	Handle HandleID
	// TODO bit 1 is datasync, not well documented upstream
	Flags uint32
	Dir   bool
}

var _ = Request(&FsyncRequest{})

func (r *FsyncRequest) String() string {
	return fmt.Sprintf("Fsync [%s] Handle %v Flags %v", &r.Header, r.Handle, r.Flags)
}

func (r *FsyncRequest) Respond() {
	out := &outHeader{Unique: uint64(r.ID)}
	r.respond(out, unsafe.Sizeof(*out))
}

// An InterruptRequest is a request to interrupt another pending request. The
// response to that request should return an error status of EINTR.
type InterruptRequest struct {
	Header `json:"-"`
	IntrID RequestID // ID of the request to be interrupt.
}

var _ = Request(&InterruptRequest{})

func (r *InterruptRequest) Respond() {
	// nothing to do here
	r.noResponse()
}

func (r *InterruptRequest) String() string {
	return fmt.Sprintf("Interrupt [%s] ID %v", &r.Header, r.IntrID)
}

/*{

// A XXXRequest xxx.
type XXXRequest struct {
	Header `json:"-"`
	xxx
}

var _ = Request(&XXXRequest{})

func (r *XXXRequest) String() string {
	return fmt.Sprintf("XXX [%s] xxx", &r.Header)
}

// Respond replies to the request with the given response.
func (r *XXXRequest) Respond(resp *XXXResponse) {
	out := &xxxOut{
		outHeader: outHeader{Unique: uint64(r.ID)},
		xxx,
	}
	r.respond(&out.outHeader, unsafe.Sizeof(*out))
}

// A XXXResponse is the response to a XXXRequest.
type XXXResponse struct {
	xxx
}

func (r *XXXResponse) String() string {
	return fmt.Sprintf("XXX %+v", *r)
}

 }
*/
