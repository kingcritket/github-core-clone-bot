// Forge binary entrypoint: listen on an ephemeral port (or -port), print the port
// as `PORT <n>` on stdout for the Node launcher (main.ts), then serve.
//
// The trial runs `node /bot/main.ts`; main.ts spawns this binary, reads the PORT
// line, registers the port with the environment and long-polls /done. Running the
// binary directly is the local-dev path: ./forge -port 18080.

package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	port := flag.Int("port", 0, "port to listen on (0 = ephemeral, chosen by the kernel)")
	flag.Parse()

	s := newStore()
	srv := &http.Server{Handler: makeHandler(s)}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintf(os.Stderr, "[forge] server did not bind a TCP port\n")
		os.Exit(1)
	}
	// The launcher reads this line to learn the port. Nothing else goes to stdout.
	fmt.Printf("PORT %d\n", tcpAddr.Port)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
