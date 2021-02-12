package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"golang.org/x/net/http2"
	fasthttp2 "github.com/dgrr/http2"
)

func main() {
	certData, priv, err := GenerateTestCertificate("localhost:8080")
	if err != nil {
		log.Fatalln(err)
	}

	cert, err := tls.X509KeyPair(certData, priv)
	if err != nil {
		log.Fatalln(err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos: []string{"h2"},
	}

	proxy := &Proxy{
		Backend: "localhost:8081",
	}

	go startBackend()

	ln, err := tls.Listen("tcp", ":8443", tlsConfig)
	if err != nil {
		log.Fatalln(err)
	}

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatalln(err)
		}

		go proxy.handleConn(c)
	}
}

type Proxy struct {
	Backend string
}

func (px *Proxy) handleConn(c net.Conn) {
	defer c.Close()

	bc, err := tls.Dial("tcp", px.Backend, &tls.Config{
		NextProtos: []string{"h2"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Fatalln(err)
	}

	if !fasthttp2.ReadPreface(c) {
		log.Fatalln("error reading preface")
	}

	err = fasthttp2.WritePreface(bc)
	if err != nil {
		log.Fatalln(err)
	}

	go readFramesFrom(bc, c)
	readFramesFrom(c, bc)
}

func readFramesFrom(c, c2 net.Conn) {
	fr := fasthttp2.AcquireFrame()
	defer fasthttp2.ReleaseFrame(fr)

	var err error
	for err == nil {
		_, err = fr.ReadFrom(c) // TODO: Use ReadFromLimitPayload?
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		debugFrame(c, fr)

		_, err = fr.WriteTo(c2)
	}
}

func debugFrame(c net.Conn, fr *fasthttp2.Frame) {
	switch fr.Type() {
	case fasthttp2.FrameHeaders:
		println("headers")
	case fasthttp2.FrameContinuation:
		println("continuation")
	case fasthttp2.FrameData:
		println("data")
	case fasthttp2.FramePriority:
		println("priority")
		// TODO: If a PRIORITY frame is received with a stream identifier of 0x0, the recipient MUST respond with a connection error
	case fasthttp2.FrameResetStream:
		println("reset")
	case fasthttp2.FrameSettings:
		println("settings")
		// TODO: Check if the client's settings fit the server ones
	case fasthttp2.FramePushPromise:
		println("pp")
	case fasthttp2.FramePing:
		println("ping")
	case fasthttp2.FrameGoAway:
		println("away")
	case fasthttp2.FrameWindowUpdate:
		println("update")
	}
}

var (
	hostArg = flag.String("host", "localhost:8081", "host")
)

func init() {
	flag.Parse()
}

func startBackend() {
	certData, priv, err := GenerateTestCertificate(*hostArg)
	if err != nil {
		log.Fatalln(err)
	}

	cert, err := tls.X509KeyPair(certData, priv)
	if err != nil {
		log.Fatalln(err)
	}

	tlsConfig := &tls.Config{
		ServerName: *hostArg,
		Certificates: []tls.Certificate{cert},
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
	}

	_, port, _ := net.SplitHostPort(*hostArg)

	s := &http.Server{
		Addr: ":"+port,
		TLSConfig: tlsConfig,
		Handler: &ReqHandler{},
	}
	s2 := &http2.Server{}

	err = http2.ConfigureServer(s, s2)
	if err != nil {
		log.Fatalln(err)
	}

	ln, err := tls.Listen("tcp", ":"+port, tlsConfig)
	if err != nil {
		log.Fatalln(err)
	}
	defer ln.Close()

	err = s.Serve(ln)
	if err != nil {
		log.Fatalln(err)
	}
}

type ReqHandler struct {}

func (rh *ReqHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hello HTTP/2\n")
}