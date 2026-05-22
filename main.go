package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	defaultPort := 1100
	if v := os.Getenv("POP3_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			defaultPort = p
		}
	}
	port := flag.Int("port", defaultPort, "POP3 listen port")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("imap2pop starting, listening on port %d, IMAP target: %s:%d", *port, imapHost, imapPort)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}
	log.Printf("POP3 server listening on port %d", *port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				continue
			}
			remote := conn.RemoteAddr().String()
			log.Printf("[%s] New POP3 connection", remote)
			go handlePop3Session(conn)
		}
	}()

	<-sigCh
	log.Println("Shutting down...")
	listener.Close()
}