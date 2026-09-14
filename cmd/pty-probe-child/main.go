// Command pty-probe-child is a spike fixture: a PTY child that emits the
// same terminal startup probes Codex sends, then reports every reply byte it
// reads back. It exists to prove the Go headless terminal can act as the
// child's terminal peer with zero physical terminal attached.
//
// Protocol on stdout:
//
//	PROBE_SENT            - after the query batch is written
//	REPLY_HEX <hex>       - all reply bytes read from stdin, hex-encoded
//	REPLY_HEX2 <hex>      - reply to the second CSI ?u (after CSI >1u push)
//	PROBE_DONE            - finished
package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	tcgets = 0x5401
	tcsets = 0x5402
)

func makeRaw(fd uintptr) (*syscall.Termios, error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tcgets, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tcsets, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return &old, nil
}

func restore(fd uintptr, old *syscall.Termios) {
	syscall.Syscall(syscall.SYS_IOCTL, fd, tcsets, uintptr(unsafe.Pointer(old)))
}

// readFor collects stdin bytes until quiet for quietMs or the deadline hits.
func readFor(fd uintptr, quietMs, maxMs int) []byte {
	var out []byte
	buf := make([]byte, 4096)
	deadline := time.Now().Add(time.Duration(maxMs) * time.Millisecond)
	quiet := time.Now().Add(time.Duration(quietMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		var fds syscall.FdSet
		fdSet(&fds, int(fd))
		tv := syscall.Timeval{Sec: 0, Usec: 50_000}
		n, err := syscall.Select(int(fd)+1, &fds, nil, nil, &tv)
		if err == nil && n > 0 {
			m, rerr := syscall.Read(int(fd), buf)
			if m > 0 {
				out = append(out, buf[:m]...)
				quiet = time.Now().Add(time.Duration(quietMs) * time.Millisecond)
			}
			if rerr != nil {
				break
			}
		}
		if len(out) > 0 && time.Now().After(quiet) {
			break
		}
	}
	return out
}

func fdSet(s *syscall.FdSet, fd int) {
	s.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

func main() {
	in := os.Stdin.Fd()
	old, err := makeRaw(in)
	if err != nil {
		fmt.Printf("PROBE_ERROR raw: %v\n", err)
		os.Exit(2)
	}
	defer restore(in, old)

	// Codex TUI startup probe batch: CSI 6n, OSC 10;?, OSC 11;?, CSI ?u, CSI c.
	os.Stdout.WriteString("\x1b[6n\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b[?u\x1b[c")
	os.Stdout.WriteString("PROBE_SENT\r\n")

	replies := readFor(in, 250, 3000)
	fmt.Printf("REPLY_HEX %x\r\n", replies)

	// Second phase: push kitty flags like crossterm's
	// PushKeyboardEnhancementFlags, then re-query. Expected reply: CSI ?1u.
	os.Stdout.WriteString("\x1b[>1u\x1b[?u")
	replies2 := readFor(in, 250, 2000)
	fmt.Printf("REPLY_HEX2 %x\r\n", replies2)

	// Wait for a single go byte so the harness can resize the PTY mid-run,
	// then report the live winsize.
	readFor(in, 1, 10_000)
	var ws struct{ Row, Col, Xp, Yp uint16 }
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, in, syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws))); errno == 0 {
		fmt.Printf("WINSIZE %dx%d\r\n", ws.Row, ws.Col)
	} else {
		fmt.Printf("WINSIZE error %v\r\n", errno)
	}

	os.Stdout.WriteString("PROBE_DONE\r\n")
}
