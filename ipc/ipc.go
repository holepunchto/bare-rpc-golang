// Package ipc connects bare-rpc across a process boundary over an inherited file descriptor.
//
// From Go, Socketpair gives the two ends; pass the child end through exec.Cmd.ExtraFiles
// (the first entry becomes fd 3) and wrap the parent end with net.FileConn. From Bare,
// bare-subprocess spawns with stdio: ['inherit', 'inherit', 'inherit', 'pipe'] and talks on
// subprocess.stdio[3]; the Go child picks that up with Inherited(3).
package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Inherited wraps a descriptor handed down by the parent process as a net.Conn.
func Inherited(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), fmt.Sprintf("ipc-fd-%d", fd))
	if f == nil {
		return nil, fmt.Errorf("ipc: fd %d is not open", fd)
	}
	defer f.Close()
	return net.FileConn(f)
}

// Socketpair returns both ends of a connected AF_UNIX stream pair, close-on-exec, so only
// the end passed through ExtraFiles reaches a child.
func Socketpair() (parent, child *os.File, err error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return os.NewFile(uintptr(fds[0]), "ipc-parent"), os.NewFile(uintptr(fds[1]), "ipc-child"), nil
}

// Dial connects like net.Dial but keeps retrying until ctx is done, for a server that is still
// starting up, such as a Bare sidecar this process just spawned.
func Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	for {
		conn, err := dialer.DialContext(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ipc: dial %s %s: %w (last error: %v)", network, address, ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
