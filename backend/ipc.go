package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Minimal client for the OpenFlux core's IPC protocol (transport/ipc in
// upstream): framed JSON over a Unix domain socket.
//
//	[4 bytes big-endian length N][1 byte type][N-1 bytes payload]
//
// Length includes the type byte but not the length prefix itself; payload
// is JSON. The backend only reads MsgStatus (0x03) frames; the core pushes
// one every second while --ipc-socket is set. Verified against upstream at
// the pinned version (manifest.json → core.version).
const (
	ipcMsgStatus = 0x03
	ipcMaxFrame  = 1 << 20
)

// ipcStatus mirrors upstream transport/ipc.StatusPayload.
type ipcStatus struct {
	Running   bool     `json:"running"`
	Connected bool     `json:"connected"`
	BytesIn   uint64   `json:"bytes_in"`
	BytesOut  uint64   `json:"bytes_out"`
	UptimeMs  int64    `json:"uptime_ms"`
	Active    string   `json:"active,omitempty"`
	ActiveAll []string `json:"active_all,omitempty"`
}

func ipcReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > ipcMaxFrame {
		return 0, nil, fmt.Errorf("ipc: invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

// ipcStatusClient maintains one connection to the core's IPC socket and
// keeps the latest status; it reconnects every 2 seconds when the core is
// down or restarting. This is read-only: the backend never sends commands
// through the IPC channel.
type ipcStatusClient struct {
	socketPath string

	mu      sync.Mutex
	latest  *ipcStatus
	lastErr string
	stop    chan struct{}
}

func newIPCStatusClient(socketPath string) *ipcStatusClient {
	return &ipcStatusClient{socketPath: socketPath, stop: make(chan struct{})}
}

func (c *ipcStatusClient) snapshot() (st *ipcStatus, err string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest, c.lastErr
}

func (c *ipcStatusClient) run() {
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if err := c.poll(); err != nil {
			c.mu.Lock()
			c.lastErr = err.Error()
			c.latest = nil
			c.mu.Unlock()
		}
		select {
		case <-c.stop:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *ipcStatusClient) poll() error {
	conn, err := net.DialTimeout("unix", c.socketPath, 3*time.Second)
	if err != nil {
		return errors.New("core IPC socket: not reachable (core down or starting)")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// The core sends a status frame every second; read a couple of frames
	// with a deadline and report the newest. A long-lived read loop would
	// need write-side keepalive handling we do not need.
	for {
		typ, payload, err := ipcReadFrame(conn)
		if err != nil {
			if c.lastStatusReceived() {
				return nil // connection dropped after good frames; keep last status
			}
			return errors.New("core IPC: " + err.Error())
		}
		if typ == ipcMsgStatus {
			st := &ipcStatus{}
			if err := json.Unmarshal(payload, st); err != nil {
				return fmt.Errorf("core IPC: bad status payload: %w", err)
			}
			c.mu.Lock()
			c.latest = st
			c.lastErr = ""
			c.mu.Unlock()
		}
	}
}

func (c *ipcStatusClient) lastStatusReceived() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest != nil
}

func (c *ipcStatusClient) close() { close(c.stop) }
