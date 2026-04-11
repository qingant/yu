package netproxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KeyReplacement maps a dummy key to its real value.
type KeyReplacement struct {
	Dummy string // e.g. "yu-anthropic_api_key-a1b2c3"
	Real  string // e.g. "sk-ant-api03-..."
}

// APIRoute maps a local path prefix to a real upstream endpoint,
// replacing dummy keys with real keys in transit.
type APIRoute struct {
	// PathPrefix is the local path that triggers this route, e.g. "/anthropic"
	PathPrefix string

	// Upstream is the real base URL, e.g. "https://api.anthropic.com"
	Upstream string

	// KeyReplacements: dummy → real key substitutions applied to all headers.
	KeyReplacements []KeyReplacement

	// ForceHeaders: always set these headers on upstream requests,
	// overriding whatever the agent sent (e.g. its own OAuth JWT).
	ForceHeaders map[string]string

	// ForceHeaderFunc: called per-request to get dynamic headers (e.g. auto-refreshing JWT).
	// Merged with ForceHeaders; ForceHeaderFunc takes precedence.
	ForceHeaderFunc func() map[string]string
}

// APIProxy is a local reverse proxy that routes agent API calls,
// replacing dummy credentials with real ones. No MITM, no certs.
type APIProxy struct {
	Addr   string // listen address
	Routes []APIRoute

	listener net.Listener
	auditFn  func(method, url string, status int, note string)
}

// NewAPIProxy creates a local API proxy.
func NewAPIProxy() *APIProxy {
	return &APIProxy{
		Addr: "127.0.0.1:0",
	}
}

// Start begins listening. Returns the actual address.
func (ap *APIProxy) Start() (string, error) {
	var err error
	ap.listener, err = net.Listen("tcp", ap.Addr)
	if err != nil {
		return "", fmt.Errorf("api proxy listen: %w", err)
	}

	go http.Serve(ap.listener, ap)
	return ap.listener.Addr().String(), nil
}

// Stop shuts down the proxy.
func (ap *APIProxy) Stop() {
	if ap.listener != nil {
		ap.listener.Close()
	}
}

// SetAuditFunc sets the audit logging function.
func (ap *APIProxy) SetAuditFunc(fn func(method, url string, status int, note string)) {
	ap.auditFn = fn
}

func (ap *APIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Proxy only listens on localhost — the sandbox boundary is the security,
	// not a shared secret. External agents (Claude Code) don't know the secret.

	// Find matching route
	var route *APIRoute
	for i := range ap.Routes {
		if strings.HasPrefix(r.URL.Path, ap.Routes[i].PathPrefix) {
			route = &ap.Routes[i]
			break
		}
	}
	if route == nil {
		http.Error(w, "no route matched", 404)
		return
	}

	// Build upstream URL
	upstreamPath := strings.TrimPrefix(r.URL.Path, route.PathPrefix)
	upstream, err := url.Parse(route.Upstream)
	if err != nil {
		http.Error(w, "bad upstream URL", 500)
		return
	}
	upstream.Path = strings.TrimSuffix(upstream.Path, "/") + upstreamPath
	upstream.RawQuery = r.URL.RawQuery

	// WebSocket upgrade — hijack and do raw TCP proxy
	if isWebSocketUpgrade(r) {
		ap.handleWebSocket(w, r, route, upstream)
		return
	}

	// Regular HTTP — create upstream request
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Copy headers, replacing dummy keys
	ap.copyHeaders(r.Header, upReq.Header, route)
	upReq.Header.Del("Connection")
	upReq.Header.Del("Proxy-Connection")

	// Debug: log outgoing headers
	if ap.auditFn != nil {
		var hdrs []string
		for k, v := range upReq.Header {
			val := strings.Join(v, ", ")
			if k == "Authorization" || k == "X-Api-Key" || k == "X-Goog-Api-Key" {
				val = "[REDACTED]"
			}
			hdrs = append(hdrs, k+": "+val)
		}
		ap.auditFn(r.Method, upstream.String(), 0, "headers: "+strings.Join(hdrs, " | "))
	}

	// Forward
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(upReq)
	if err != nil {
		if ap.auditFn != nil {
			ap.auditFn(r.Method, upstream.String(), 502, err.Error())
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if ap.auditFn != nil {
		ap.auditFn(r.Method, upstream.String(), resp.StatusCode, "api-proxy")
	}

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleWebSocket hijacks the client connection, dials the upstream with
// replaced credentials, sends the HTTP upgrade request, and bridges
// the two TCP connections for bidirectional streaming.
func (ap *APIProxy) handleWebSocket(w http.ResponseWriter, r *http.Request, route *APIRoute, upstream *url.URL) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket hijack not supported", 500)
		return
	}

	// Determine upstream host:port and TLS
	upHost := upstream.Hostname()
	upPort := upstream.Port()
	useTLS := upstream.Scheme == "https" || upstream.Scheme == "wss"
	if upPort == "" {
		if useTLS {
			upPort = "443"
		} else {
			upPort = "80"
		}
	}

	// Dial upstream
	var upConn net.Conn
	var err error
	addr := net.JoinHostPort(upHost, upPort)
	if useTLS {
		upConn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{
			ServerName: upHost,
		})
	} else {
		upConn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		if ap.auditFn != nil {
			ap.auditFn("WS", upstream.String(), 502, err.Error())
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Build the upgrade request to send to upstream
	wsPath := upstream.RequestURI()
	upReq, _ := http.NewRequest(r.Method, wsPath, nil)
	upReq.Host = upHost
	ap.copyHeaders(r.Header, upReq.Header, route)
	upReq.Header.Set("Host", upHost)

	// Write the upgrade request to upstream
	if err := upReq.Write(upConn); err != nil {
		upConn.Close()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Read upstream response
	upBufReader := bufio.NewReader(upConn)
	upResp, err := http.ReadResponse(upBufReader, upReq)
	if err != nil {
		upConn.Close()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if ap.auditFn != nil {
		ap.auditFn("WS", upstream.String(), upResp.StatusCode, "websocket")
	}

	// Hijack client connection
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		upConn.Close()
		return
	}

	// Write upstream response back to client
	upResp.Write(clientBuf)
	clientBuf.Flush()

	if upResp.StatusCode != http.StatusSwitchingProtocols {
		clientConn.Close()
		upConn.Close()
		return
	}

	// Bridge: upstream → client
	go func() {
		if upBufReader.Buffered() > 0 {
			buffered := make([]byte, upBufReader.Buffered())
			upBufReader.Read(buffered)
			clientConn.Write(buffered)
		}
		io.Copy(clientConn, upConn)
		clientConn.Close()
	}()
	// Bridge: client → upstream
	go func() {
		io.Copy(upConn, clientConn)
		upConn.Close()
	}()
}

// authHeaders are headers that carry credentials — never copy from agent to upstream.
// Agent sends dummy keys; real auth comes exclusively from ForceHeaders.
var authHeaders = map[string]bool{
	"Authorization":     true,
	"X-Api-Key":         true,
	"X-Goog-Api-Key":    true,

}

func (ap *APIProxy) copyHeaders(src, dst http.Header, route *APIRoute) {
	// Copy non-auth headers from agent, with dummy key replacement
	for key, values := range src {
		if authHeaders[http.CanonicalHeaderKey(key)] {
			continue // skip — auth is handled by ForceHeaders
		}
		for _, v := range values {
			replaced := v
			for _, rep := range route.KeyReplacements {
				if strings.Contains(replaced, rep.Dummy) {
					replaced = strings.Replace(replaced, rep.Dummy, rep.Real, 1)
				}
			}
			dst.Add(key, replaced)
		}
	}
	// Set real auth from ForceHeaders — this is the ONLY source of credentials
	for k, v := range route.ForceHeaders {
		dst.Set(k, v)
	}
	// Dynamic headers (e.g. auto-refreshing Copilot JWT) override static ones
	if route.ForceHeaderFunc != nil {
		for k, v := range route.ForceHeaderFunc() {
			dst.Set(k, v)
		}
	}
}
