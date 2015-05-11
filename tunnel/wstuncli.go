// Copyright (c) 2014 RightScale, Inc. - see LICENSE

// Websockets tunnel client, which runs at the HTTP server end (yes, I know, it's confusing)
// This client connects to a websockets tunnel server and waits to receive HTTP requests
// tunneled through the websocket, then issues these HTTP requests locally to an HTTP server
// grabs the response and ships that back through the tunnel.
//
// This client is highly concurrent: it spawns a goroutine for each received request and issues
// that concurrently to the HTTP server and then sends the response back whenever the HTTP
// request returns. The response can thus go back out of order and multiple HTTP requests can
// be in flight at a time.
//
// This client also sends periodic ping messages through the websocket and expects prompt
// responses. If no response is received, it closes the websocket and opens a new one.
//
// The main limitation of this client is that responses have to go throught the same socket
// that the requests arrived on. Thus, if the websocket dies while an HTTP request is in progress
// it impossible for the response to travel on the next websocket, instead it will be dropped
// on the floor. This should not be difficult to fix, though.
//
// Another limitation is that it keeps a single websocket open and can thus get stuck for
// many seconds until the timeout on the websocket hits and a new one is opened.

package tunnel

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"os"
	"regexp"
	"runtime"
	//"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	_ "net/http/pprof"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/inconshreveable/log15.v2"
)

var _ fmt.Formatter

type WSTunnelClient struct {
	Token          string          // Rendez-vous token
	Tunnel         string          // websocket server to connect to (ws[s]://hostname:port)
	Server         string          // local HTTP(S) server to send received requests to (default server)
	InternalServer http.Handler    // internal Server to dispatch HTTP requests to
	Regexp         *regexp.Regexp  // regexp for allowed local HTTP(S) servers
	Insecure       bool            // accept self-signed SSL certs from local HTTPS servers
	Timeout        time.Duration   // timeout on websocket
	Log            log15.Logger    // logger with "pkg=WStuncli"
	exitChan       chan struct{}   // channel to tell the tunnel goroutines to end
	ws             *websocket.Conn // websocket connection
}

//===== Main =====

func NewWSTunnelClient(args []string) *WSTunnelClient {
	wstunCli := WSTunnelClient{}

	var cliFlag = flag.NewFlagSet("client", flag.ExitOnError)
	cliFlag.StringVar(&wstunCli.Token, "token", "",
		"rendez-vous token identifying this server")
	cliFlag.StringVar(&wstunCli.Tunnel, "tunnel", "",
		"websocket server ws[s]://hostname:port to connect to")
	cliFlag.StringVar(&wstunCli.Server, "server", "",
		"http server http[s]://hostname:port to send received requests to")
	cliFlag.BoolVar(&wstunCli.Insecure, "insecure", false,
		"accept self-signed SSL certs from local HTTPS servers")
	var sre *string = cliFlag.String("regexp", "",
		"regexp for local HTTP(S) server to allow sending received requests to")
	var tout *int = cliFlag.Int("timeout", 30, "timeout on websocket in seconds")
	var pidf *string = cliFlag.String("pidfile", "", "path for pidfile")
	var logf *string = cliFlag.String("logfile", "", "path for log file")

	cliFlag.Parse(args)

	wstunCli.Log = makeLogger("WStuncli", *logf, "")
	writePid(*pidf)
	wstunCli.Timeout = calcWsTimeout(*tout)

	// process -regexp
	if *sre != "" {
		var err error
		wstunCli.Regexp, err = regexp.Compile(*sre)
		if err != nil {
			log15.Crit("Can't parse -regexp", "err", err.Error())
			os.Exit(1)
		}
	}
	return &wstunCli
}

func (t *WSTunnelClient) Start() error {
	t.Log.Info(VV)

	// validate -tunnel
	if t.Tunnel == "" {
		return fmt.Errorf("Must specify tunnel server ws://hostname:port using -tunnel option")
	}
	if !strings.HasPrefix(t.Tunnel, "ws://") && !strings.HasPrefix(t.Tunnel, "wss://") {
		return fmt.Errorf("Remote tunnel (-tunnel option) must begin with ws:// or wss://")
	}
	t.Tunnel = strings.TrimSuffix(t.Tunnel, "/")

	// validate -server
	if t.InternalServer != nil {
		t.Server = ""
	} else if t.Server != "" {
		if !strings.HasPrefix(t.Server, "http://") && !strings.HasPrefix(t.Server, "https://") {
			return fmt.Errorf("Local server (-server option) must begin with http:// or https://")
		}
		t.Server = strings.TrimSuffix(t.Server, "/")
	}

	// validate token and timeout
	if t.Token == "" {
		return fmt.Errorf("Must specify rendez-vous token using -token option")
	}

	if t.Insecure {
		t.Log.Info("Accepting unverified SSL certs from local HTTPS servers")
	}

	if t.InternalServer != nil {
		t.Log.Info("Dispatching to internal server")
	} else if t.Server != "" || t.Regexp != nil {
		t.Log.Info("Dispatching to external server(s)", "server", t.Server, "regexp", t.Regexp)
	} else {
		return fmt.Errorf("Must specify internal server or server or regexp")
	}

	// for test purposes we have a signal that tells wstuncli to exit instead of reopening
	// a fresh connection
	t.exitChan = make(chan struct{}, 1)

	//===== Goroutine =====

	// Keep opening websocket connections to tunnel requests
	go func() {
		for {
			d := &websocket.Dialer{
				ReadBufferSize:  100 * 1024,
				WriteBufferSize: 100 * 1024,
			}
			h := make(http.Header)
			h.Add("Origin", t.Token)
			url := fmt.Sprintf("%s/_tunnel", t.Tunnel)
			timer := time.NewTimer(10 * time.Second)
			t.Log.Info("WS   Opening", "url", url)
			var err error
			var resp *http.Response
			t.ws, resp, err = d.Dial(url, h)
			if err != nil {
				extra := ""
				if resp != nil {
					extra = resp.Status
					buf := make([]byte, 80)
					resp.Body.Read(buf)
					if len(buf) > 0 {
						extra = extra + " -- " + string(buf)
					}
					resp.Body.Close()
				}
				t.Log.Error("Error opening connection",
					"err", err.Error(), "info", extra)
			} else {
				// Safety setting
				t.ws.SetReadLimit(100 * 1024 * 1024)
				// Request Loop
				t.Log.Info("WS   ready", "server", t.Server)
				t.handleWsRequests()
			}
			// check whether we need to exit
			select {
			case <-t.exitChan:
				break
			}

			<-timer.C // ensure we don't open connections too rapidly
		}
	}()

	return nil
}

func (t *WSTunnelClient) Stop() {
	t.exitChan <- struct{}{}
}

// Main function to handle WS requests: it reads a request from the socket, then forks
// a goroutine to perform the actual http request and return the result
func (t *WSTunnelClient) handleWsRequests() {
	go t.pinger()
	for {
		t.ws.SetReadDeadline(time.Time{}) // separate ping-pong routine does timeout
		typ, r, err := t.ws.NextReader()
		if err != nil {
			t.Log.Info("WS   ReadMessage", "err", err.Error())
			break
		}
		if typ != websocket.BinaryMessage {
			t.Log.Info("WS   invalid message type", "type", typ)
			break
		}
		// give the sender a minute to produce the request
		t.ws.SetReadDeadline(time.Now().Add(time.Minute))
		// read request id
		var id int16
		_, err = fmt.Fscanf(io.LimitReader(r, 4), "%04x", &id)
		if err != nil {
			t.Log.Info("WS   cannot read request ID", "err", err.Error())
			break
		}
		// read request itself
		req, err := http.ReadRequest(bufio.NewReader(r))
		if err != nil {
			t.Log.Info("WS   cannot read request body", "id", id, "err", err.Error())
			break
		}
		// Hand off to goroutine to finish off while we read the next request
		if t.InternalServer != nil {
			go t.finishInternalRequest(id, req)
		} else {
			go t.finishRequest(id, req)
		}
	}
	// delay a few seconds to allow for writes to drain and then force-close the socket
	go func() {
		time.Sleep(5 * time.Second)
		t.ws.Close()
	}()
}

//===== Keep-alive ping-pong =====

// Pinger that keeps connections alive and terminates them if they seem stuck
func (t *WSTunnelClient) pinger() {
	t.Log.Info("pinger starting")
	// timeout handler sends a close message, waits a few seconds, then kills the socket
	timeout := func() {
		t.ws.WriteControl(websocket.CloseMessage, nil, time.Now().Add(1*time.Second))
		t.Log.Info("ping timeout, closing WS")
		time.Sleep(5 * time.Second)
		t.ws.Close()
	}
	// timeout timer
	timer := time.AfterFunc(t.Timeout, timeout)
	// pong handler resets last pong time
	ph := func(message string) error {
		timer.Reset(t.Timeout)
		return nil
	}
	t.ws.SetPongHandler(ph)
	// ping loop, ends when socket is closed...
	for {
		err := t.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(t.Timeout/3))
		if err != nil {
			break
		}
		time.Sleep(t.Timeout / 3)
	}
	t.Log.Info("pinger ending (WS errored or closed)")
	t.ws.Close()
}

//===== HTTP Header Stuff =====

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
	"Host",
}

//===== HTTP response writer, used for internal request handlers

type responseWriter struct {
	resp *http.Response
	buf  *bytes.Buffer
}

func newResponseWriter(req *http.Request) *responseWriter {
	buf := bytes.Buffer{}
	resp := http.Response{
		Header:        make(http.Header),
		Body:          ioutil.NopCloser(&buf),
		StatusCode:    -1,
		ContentLength: -1,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
	}
	return &responseWriter{
		resp: &resp,
		buf:  &buf,
	}

}

func (rw *responseWriter) Write(buf []byte) (int, error) {
	if rw.resp.StatusCode == -1 {
		rw.WriteHeader(200)
	}
	return rw.buf.Write(buf)
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.resp.StatusCode = code
	rw.resp.Status = http.StatusText(code)
}

func (rw *responseWriter) Header() http.Header { return rw.resp.Header }

func (rw *responseWriter) finishResponse() error {
	if rw.resp.StatusCode == -1 {
		return fmt.Errorf("HTTP internal handler did not call Write or WriteHeader")
	}
	rw.resp.ContentLength = int64(rw.buf.Len())

	return nil
}

//===== HTTP driver and response sender =====

var wsWriterMutex sync.Mutex // mutex to allow a single goroutine to send a response at a time

// Issue a request to an internal handler. This duplicates some logic found in
// net.http.serve http://golang.org/src/net/http/server.go?#L1124 and
// net.http.readRequest http://golang.org/src/net/http/server.go?#L
func (t *WSTunnelClient) finishInternalRequest(id int16, req *http.Request) {
	log := t.Log.New("id", id, "verb", req.Method, "uri", req.RequestURI)
	log.Info("HTTP issuing internal request")

	// Remove hop-by-hop headers
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}

	// Add fake protocol version
	req.Proto = "HTTP/1.0"
	req.ProtoMajor = 1
	req.ProtoMinor = 0

	// Dump the request into a buffer in case we want to log it
	dump, _ := httputil.DumpRequest(req, false)
	log.Debug("dump", "req", strings.Replace(string(dump), "\r\n", " || ", -1))

	// Make sure we don't die if a panic occurs in the handler
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Error("HTTP panic in handler", "err", err, "stack", string(buf))
		}
	}()

	// Concoct Response
	rw := newResponseWriter(req)

	// Issue the request to the HTTP server
	t.InternalServer.ServeHTTP(rw, req)

	err := rw.finishResponse()
	if err != nil {
		//dump2, _ := httputil.DumpResponse(resp, true)
		//log15.Info("handleWsRequests: request error", "err", err.Error(),
		//	"req", string(dump), "resp", string(dump2))
		log.Info("HTTP request error", "err", err.Error())
		writeResponseMessage(t, id, concoctResponse(req, err.Error(), 502))
		return
	}

	log.Info("HTTP responded", "status", rw.resp.Status)
	writeResponseMessage(t, id, rw.resp)
}

func (t *WSTunnelClient) finishRequest(id int16, req *http.Request) {

	log := t.Log.New("id", id, "verb", req.Method, "uri", req.RequestURI)

	// Honor X-Host header
	host := t.Server
	xHost := req.Header.Get("X-Host")
	if xHost != "" {
		if t.Regexp == nil {
			log.Info("WS   got x-host header but no regexp provided")
			writeResponseMessage(t, id, concoctResponse(req,
				"X-Host header disallowed by wstunnel cli (no -regexp option)", 403))
			return
		} else if t.Regexp.FindString(xHost) == xHost {
			host = xHost
		} else {
			log.Info("WS   x-host disallowed by regexp", "x-host", xHost)
			writeResponseMessage(t, id, concoctResponse(req,
				"X-Host header does not match regexp in wstunnel cli", 403))
			return
		}
	} else if host == "" {
		log.Info("WS   no x-host header and -server not specified")
		writeResponseMessage(t, id, concoctResponse(req,
			"X-Host header required by wstunnel cli (no -server option)", 403))
		return
	}
	req.Header.Del("X-Host")

	// Construct the URL for the outgoing request
	var err error
	req.URL, err = url.Parse(fmt.Sprintf("%s%s", host, req.RequestURI))
	if err != nil {
		log.Warn("WS   cannot parse requestURI", "err", err.Error())
		writeResponseMessage(t, id, concoctResponse(req,
			"Cannot parse request URI", 400))
		return
	}
	req.Host = req.URL.Host // we delete req.Header["Host"] further down
	req.RequestURI = ""
	log.Info("HTTP issuing request", "url", req.URL.String())

	// Accept self-signed certs
	client := http.Client{} // default client, rejects self-signed certs
	if t.Insecure {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
		client = http.Client{Transport: tr}
	}

	// Remove hop-by-hop headers
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	// Issue the request to the HTTP server
	dump, _ := httputil.DumpRequest(req, false)
	log.Debug("dump", "req", strings.Replace(string(dump), "\r\n", " || ", -1))
	resp, err := client.Do(req)
	if err != nil {
		//dump2, _ := httputil.DumpResponse(resp, true)
		//log15.Info("handleWsRequests: request error", "err", err.Error(),
		//	"req", string(dump), "resp", string(dump2))
		log.Info("HTTP request error", "err", err.Error())
		writeResponseMessage(t, id, concoctResponse(req, err.Error(), 502))
		return
	}
	log.Info("HTTP responded", "status", resp.Status)
	defer resp.Body.Close()

	writeResponseMessage(t, id, resp)
}

// Write the response message to the websocket
func writeResponseMessage(t *WSTunnelClient, id int16, resp *http.Response) {
	// Get writer's lock
	wsWriterMutex.Lock()
	defer wsWriterMutex.Unlock()
	// Write response into the tunnel
	t.ws.SetWriteDeadline(time.Now().Add(time.Minute))
	w, err := t.ws.NextWriter(websocket.BinaryMessage)
	// got an error, reply with a "hey, retry" to the request handler
	if err != nil {
		t.Log.Warn("WS   NextWriter", "err", err.Error())
		t.ws.Close()
		return
	}

	// write the request Id
	_, err = fmt.Fprintf(w, "%04x", id)
	if err != nil {
		t.Log.Warn("WS   cannot write request Id", "err", err.Error())
		t.ws.Close()
		return
	}

	// write the response itself
	err = resp.Write(w)
	if err != nil {
		t.Log.Warn("WS   cannot write response", "err", err.Error())
		t.ws.Close()
		return
	}

	// done
	err = w.Close()
	if err != nil {
		t.Log.Warn("WS   write-close failed", "err", err.Error())
		t.ws.Close()
		return
	}
}

// Create an http Response from scratch, there must be a better way that this but I
// don't know what it is
func concoctResponse(req *http.Request, message string, code int) *http.Response {
	r := http.Response{
		Status:     "Bad Gateway", //strconv.Itoa(code),
		StatusCode: code,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Header:     make(map[string][]string),
		Request:    req,
	}
	body := bytes.NewReader([]byte(message))
	r.Body = ioutil.NopCloser(body)
	r.ContentLength = int64(body.Len())
	r.Header.Add("content-type", "text/plain")
	r.Header.Add("date", time.Now().Format(time.RFC1123))
	r.Header.Add("server", "wstunnel")
	return &r
}