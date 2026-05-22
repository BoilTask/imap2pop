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

var verbose bool

func main() {
	defaultPort := 1100
	if v := os.Getenv("POP3_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			defaultPort = p
		}
	}
	port := flag.Int("port", defaultPort, "POP3 listen port")
	flag.BoolVar(&verbose, "verbose", os.Getenv("VERBOSE") == "1", "Log IMAP protocol exchanges")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("imap2pop starting on port %d, IMAP: %s:%d, verbose: %v", *port, imapHost, imapPort, verbose)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Accept error: %v", err)
				continue
			}
			go handlePop3Session(conn)
		}
	}()

	<-sigCh
	log.Println("Shutting down...")
	listener.Close()
}