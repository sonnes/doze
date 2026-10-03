// Command helper is a dev server for tests. By default it ignores PORT and
// listens on a random port, like a server that picks its own port.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

var envName = flag.String("env", "", "add the value of this environment variable to each response")

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	portEnv := flag.Bool("port-env", false, "listen on 127.0.0.1:$PORT")
	count := flag.String("count", "", "append one line to this file on start")
	exit := flag.Int("exit", -1, "print a line and exit with this code")
	flag.Parse()

	if *count != "" {
		f, err := os.OpenFile(*count, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(f, "start")
		f.Close()
	}
	if *exit >= 0 {
		fmt.Println("boom: helper exited")
		os.Exit(*exit)
	}
	if *portEnv {
		*addr = "127.0.0.1:" + os.Getenv("PORT")
	}

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		panic(err)
	}
	fmt.Println("listening on", l.Addr())
	http.Serve(l, http.HandlerFunc(serve))
}

func serve(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "echo") {
		echo(w)
		return
	}
	fmt.Fprintf(w, "host=%s path=%s forwarded=%s", r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Host"))
	if *envName != "" {
		fmt.Fprintf(w, " env=%s", os.Getenv(*envName))
	}
}

// echo upgrades the connection and echoes each line back until the client
// closes it.
func echo(w http.ResponseWriter) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
	rw.Flush()
	s := bufio.NewScanner(rw)
	for s.Scan() {
		rw.WriteString(s.Text() + "\n")
		rw.Flush()
	}
}
