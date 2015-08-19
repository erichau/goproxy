package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/phuslu/http2"
	// "github.com/cloudflare/golibs/lrucache"
	"github.com/golang/glog"
)

const (
	Version = "@VERSION@"
)

var (
	transport *http.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ClientSessionCache: tls.NewLRUClientSessionCache(1000),
		},
		TLSHandshakeTimeout: 30 * time.Second,
		MaxIdleConnsPerHost: 4,
		DisableCompression:  false,
	}
)

type listener struct {
	net.Listener
}

func (l *listener) Accept() (c net.Conn, err error) {
	c, err = l.Listener.Accept()
	if err != nil {
		return
	}

	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(3 * time.Minute)
	}

	return
}

func genHostname() (hostname string, err error) {
	var length uint16
	if err = binary.Read(rand.Reader, binary.BigEndian, &length); err != nil {
		return
	}

	buf := make([]byte, 5+length%7)
	for i := 0; i < len(buf); i++ {
		var c uint8
		if err = binary.Read(rand.Reader, binary.BigEndian, &c); err != nil {
			return
		}
		buf[i] = 'a' + c%('z'-'a')
	}

	return fmt.Sprintf("www.%s.com", buf), nil
}

func getCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// name := clientHello.ServerName
	name := "www.gov.cn"
	if name1, err := genHostname(); err == nil {
		name = name1
	}

	glog.Infof("Generating RootCA for %v", name)
	template := x509.Certificate{
		IsCA:         true,
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{name},
		},
		NotBefore: time.Now().Add(-time.Duration(5 * time.Minute)),
		NotAfter:  time.Now().Add(180 * 24 * time.Hour),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	certPEMBlock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEMBlock := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	return &cert, err
}

func handler(rw http.ResponseWriter, req *http.Request) {
	var err error

	var paramsPreifx string = http.CanonicalHeaderKey("X-UrlFetch-")
	params := map[string]string{}
	for key, values := range req.Header {
		if strings.HasPrefix(key, paramsPreifx) {
			params[strings.ToLower(key[len(paramsPreifx):])] = values[0]
		}
	}

	for _, key := range params {
		req.Header.Del(paramsPreifx + key)
	}

	if auth := req.Header.Get("Proxy-Authorization"); auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 {
			switch parts[0] {
			case "Basic":
				if userpass, err := base64.StdEncoding.DecodeString(parts[1]); err == nil {
					parts := strings.Split(string(userpass), ":")
					user := parts[0]
					pass := parts[1]
					glog.Infof("username=%v password=%v", user, pass)
				}
			default:
				glog.Errorf("Unrecognized auth type: %#v", parts[0])
				break
			}
		}
		req.Header.Del("Proxy-Authorization")
	}

	if req.Method == "CONNECT" {
		host, port, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = req.Host
			port = "443"
		}

		glog.Infof("%s \"%s %s:%s %s\" - -", req.RemoteAddr, req.Method, host, port, req.Proto)

		conn, err := net.Dial("tcp", net.JoinHostPort(host, port))
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}

		hijacker, ok := rw.(http.Hijacker)
		if !ok {
			http.Error(rw, fmt.Sprintf("%#v is not http.Hijacker", rw), http.StatusBadGateway)
			return
		}

		flusher, ok := rw.(http.Flusher)
		if !ok {
			http.Error(rw, fmt.Sprintf("%#v is not http.Flusher", rw), http.StatusBadGateway)
			return
		}

		rw.WriteHeader(http.StatusOK)
		flusher.Flush()

		lconn, _, err := hijacker.Hijack()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}
		defer lconn.Close()

		go io.Copy(conn, lconn)
		io.Copy(lconn, conn)

		return
	}

	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}

	if req.URL.Host == "" {
		if req.Host == "" {
			req.Host = req.Header.Get("Host")
		}
		req.URL.Host = req.Host
	}

	glog.Infof("%s \"%s %s %s\" - -", req.RemoteAddr, req.Method, req.URL.String(), req.Proto)

	resp, err := transport.RoundTrip(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadGateway)
		return
	}

	for key, values := range resp.Header {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
}

func main() {
	var err error

	logToStderr := true
	for i := 1; i < len(os.Args); i++ {
		if strings.HasPrefix(os.Args[i], "-logtostderr=") {
			logToStderr = false
			break
		}
	}
	if logToStderr {
		flag.Set("logtostderr", "true")
	}

	addr := *flag.String("addr", ":443", "goproxy vps listen addr")
	verbose := *flag.Bool("verbose", false, "goproxy vps http2 verbose mode")
	flag.Parse()

	var ln net.Listener
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		glog.Fatalf("Listen(%s) error: %s", addr, err)
	}

	cert, err := getCertificate(nil)
	if err != nil {
		glog.Fatalf("getCertificate error: %s", err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(handler),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{*cert},
			// GetCertificate: getCertificate,
		},
	}

	if verbose {
		http2.VerboseLogs = true
	}
	http2.ConfigureServer(srv, &http2.Server{})
	glog.Infof("goproxy %s ListenAndServe on %s\n", Version, ln.Addr().String())
	srv.Serve(tls.NewListener(tcpKeepAliveListener{ln.(*net.TCPListener)}, srv.TLSConfig))
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}
