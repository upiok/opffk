// fakecore emulates the OpenFlux core's observable contract for the opffk
// end-to-end tests: it accepts the same flags the backend passes
// (--config, --ipc-socket), opens the IPC Unix socket, pushes a status
// frame every second (like upstream transport/ipc), and answers
// --make-link - with a JSON result. It exits immediately when
// --crash-after=<seconds> is given (used to test update rollback).
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func writeFrame(w *bufio.Writer, typ byte, payload []byte) {
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[:4], uint32(1+len(payload)))
	hdr[4] = typ
	w.Write(hdr)
	w.Write(payload)
	w.Flush()
}

func main() {
	config := flag.String("config", "", "path to .conf (parsed shallowly for Mode)")
	ipc := flag.String("ipc-socket", "", "IPC unix socket path")
	crashAfter := flag.Int("crash-after", 0, "exit non-zero after N seconds (test rollback)")
	link := flag.Bool("make-link", false, "emulate --make-link")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println("fakecore 1.0")
		return
	}
	if *link {
		out := map[string]string{"link": "openflux://fake-link-from-fakecore"}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		return
	}

	mode := "l4"
	if *config != "" {
		if data, err := os.ReadFile(*config); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Mode") {
					if i := strings.Index(line, "="); i > 0 {
						mode = strings.TrimSpace(line[i+1:])
					}
				}
			}
		}
	}
	fmt.Printf("fakecore: mode=%s config=%s ipc=%s\n", mode, *config, *ipc)
	if *ipc != "" {
		_ = os.Remove(*ipc)
		ln, err := net.Listen("unix", *ipc)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakecore: ipc listen:", err)
			os.Exit(1)
		}
		defer ln.Close()
		go acceptLoop(ln, mode)
	}
	if *crashAfter > 0 {
		time.Sleep(time.Duration(*crashAfter) * time.Second)
		fmt.Fprintln(os.Stderr, "fakecore: simulating crash")
		os.Exit(3)
	}
	select {} // run until killed
}

func acceptLoop(ln net.Listener, mode string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(conn, mode)
	}
}

func serve(conn net.Conn, mode string) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	start := time.Now()
	for tick := 0; ; tick++ {
		st := map[string]interface{}{
			"running":   true,
			"connected": true,
			"bytes_in":  uint64(tick) * 1024,
			"bytes_out": uint64(tick) * 2048,
			"uptime_ms": time.Since(start).Milliseconds(),
			"active":    "direct",
		}
		payload, _ := json.Marshal(st)
		writeFrame(w, 0x03, payload)
		time.Sleep(1 * time.Second)
	}
	_ = strconv.Itoa // keep strconv imported for parity with upstream frames
}
