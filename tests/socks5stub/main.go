// socks5stub is a minimal no-auth SOCKS5 server used by the real-tunnel
// e2e to prove that the exit node's egress traffic goes through the
// configured proxy. It logs every CONNECT target to stdout, connects to
// it and splices the bytes, so a request that traverses the stub both
// succeeds and leaves an auditable trace.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: socks5stub <listen-addr>")
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	// greeting: VER NMETHODS METHODS — accept the no-auth choice.
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	// request: VER CMD RSV ATYP ADDR PORT — CONNECT only.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[0] != 5 {
		return
	}
	if req[1] != 1 {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	var host string
	switch req[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3: // domain
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return
		}
		d := make([]byte, n[0])
		if _, err := io.ReadFull(conn, d); err != nil {
			return
		}
		host = string(d)
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))

	fmt.Printf("CONNECT %s\n", target)

	up, err := net.Dial("tcp", target)
	if err != nil {
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}
