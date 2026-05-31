// Package proxy implements the GhostProxy reverse proxy engine. It drives
// a configurable net/http/httputil.ReverseProxy through three distinct
// pipeline lifecycle states: RECORD (capture & persist), REPLAY (serve
// from cache), and CHAOS (inject faults). The engine core is designed as
// a single http.Handler that can be mounted directly on any net/http server.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/developer/GhostProxy/pkg/config"
	"github.com/developer/GhostProxy/pkg/storage"
)

// ghostProxy is the internal engine struct that encapsulates the reverse
// proxy instance, configuration state, and storage backend. It implements
// http.Handler to serve as the primary request dispatcher.
type ghostProxy struct {
	cfg    *config.AppConfig
	store  storage.Store
	rproxy *httputil.ReverseProxy
	target *url.URL
	logger *log.Logger
}

// chaosErrorResponse is the structured JSON payload returned to clients
// when a chaos injection rule triggers a simulated error response.
type chaosErrorResponse struct {
	Error     string `json:"error"`
	Code      int    `json:"code"`
	Injected  bool   `json:"injected_by_ghostproxy"`
	Route     string `json:"route"`
	Timestamp string `json:"timestamp"`
	Mode      string `json:"mode"`
}

// proxyStatusResponse is the structured JSON payload returned by the
// GhostProxy administrative status endpoint.
type proxyStatusResponse struct {
	Status    string `json:"status"`
	Mode      string `json:"mode"`
	Upstream  string `json:"upstream"`
	Snapshots int    `json:"cached_snapshots"`
	Timestamp string `json:"timestamp"`
}

// NewGhostProxy constructs and returns a fully initialized ghostProxy
// engine. It parses the upstream target URL, configures the reverse
// proxy's Director and ModifyResponse hooks, and wires up the storage
// backend. The returned handler is ready to serve HTTP traffic.
//
// This is the primary factory function exported for use by the bootstrap
// entry point in cmd/Ghost/main.go.
func NewGhostProxy(cfg *config.AppConfig, store storage.Store) (http.Handler, error) {

	targetURL, err := url.Parse(cfg.Upstream.Target)
	if err != nil {
		return nil, fmt.Errorf("proxy: failed to parse upstream target URL %q: %w",
			cfg.Upstream.Target, err)
	}

	logger := log.New(log.Writer(), "[GhostProxy] ", log.LstdFlags|log.Lmsgprefix)

	gp := &ghostProxy{
		cfg:    cfg,
		target: targetURL,
		logger: logger,
		store:  store,
	}

	reverseProxy := &httputil.ReverseProxy{
		Director:       gp.director,
		ModifyResponse: gp.modifyResponse,
		ErrorHandler:   gp.errorHandler,
	}

	gp.rproxy = reverseProxy

	return gp, nil
}

// ServeHTTP is the primary request dispatcher. It evaluates the current
// operational mode and routes the request through the appropriate
// pipeline: record (passthrough + capture), replay (cache-first), or
// chaos (fault injection). It also handles the built-in /__ghostproxy/status
// administrative endpoint.
func (gp *ghostProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Built-in administrative status endpoint
	if r.URL.Path == "/__ghostproxy/status" {
		gp.handleStatus(w, r)
		return
	}

	gp.logger.Printf("%-7s %s [mode=%s]", r.Method, r.URL.Path, gp.cfg.Mode)

	switch gp.cfg.Mode {
	case config.ModeReplay:
		gp.handleReplay(w, r)
	case config.ModeChaos:
		gp.handleChaos(w, r)
	case config.ModeRecord:
		gp.rproxy.ServeHTTP(w, r)
	default:
		// Fallback: transparent passthrough
		gp.rproxy.ServeHTTP(w, r)
	}
}

// director configures each outbound proxied request by rewriting the
// scheme, host, and path components to target the upstream service.
// This function is invoked by httputil.ReverseProxy before forwarding.
func (gp *ghostProxy) director(req *http.Request) {
	req.URL.Scheme = gp.target.Scheme
	req.URL.Host = gp.target.Host
	req.Host = gp.target.Host

	// Preserve the original request path and query string
	if gp.target.Path != "" && gp.target.Path != "/" {
		req.URL.Path = singleJoiningSlash(gp.target.Path, req.URL.Path)
	}

	// Strip hop-by-hop headers that should not be forwarded
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header.Set("User-Agent", "GhostProxy/1.0")
	}

	// Set X-Forwarded headers for upstream visibility
	req.Header.Set("X-Forwarded-Host", req.Host)
	req.Header.Set("X-GhostProxy-Mode", string(gp.cfg.Mode))
}

// modifyResponse is invoked by httputil.ReverseProxy after receiving the
// upstream response but before sending it to the client. In RECORD mode,
// it captures the full response body and headers, persists a snapshot to
// storage, and then restores the body for transparent client delivery.
func (gp *ghostProxy) modifyResponse(resp *http.Response) error {
	if gp.cfg.Mode != config.ModeRecord {
		return nil
	}

	// Ensure we don't leak the body if ReadAll panics
	defer resp.Body.Close()

	// Read the entire response body into memory for snapshot capture
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		gp.logger.Printf("RECORD WARNING: failed to read response body for %s: %v",
			resp.Request.URL.Path, err)
		return nil // Do not break the response pipeline
	}

	// Restore the body so the client receives the complete response
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	resp.ContentLength = int64(len(bodyBytes))

	// Capture structured headers (exclude hop-by-hop)
	capturedHeaders := make(map[string][]string)
	for key, values := range resp.Header {
		normalizedKey := strings.ToLower(key)
		if isHopByHopHeader(normalizedKey) {
			continue
		}
		capturedHeaders[key] = values
	}

	snapshot := &storage.Snapshot{
		Method:     resp.Request.Method,
		Path:       resp.Request.URL.Path,
		StatusCode: resp.StatusCode,
		Headers:    capturedHeaders,
		Body:       string(bodyBytes),
	}

	if err := gp.store.Save(snapshot); err != nil {
		gp.logger.Printf("RECORD ERROR: failed to save snapshot for %s %s: %v",
			resp.Request.Method, resp.Request.URL.Path, err)
		return nil // Do not break the response pipeline
	}

	gp.logger.Printf("RECORD: captured %s %s → %d (%d bytes)",
		resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, len(bodyBytes))

	return nil
}

// handleReplay serves a previously recorded snapshot from local storage,
// completely short-circuiting any network interaction with the upstream
// service. If no snapshot exists for the requested route, the request
// is transparently forwarded to the upstream via the reverse proxy.
func (gp *ghostProxy) handleReplay(w http.ResponseWriter, r *http.Request) {
	snap, found, err := gp.store.Load(r.Method, r.URL.Path)
	if err != nil {
		gp.logger.Printf("REPLAY ERROR: storage read failed for %s %s: %v",
			r.Method, r.URL.Path, err)
		// Fall through to live proxy on storage errors
		gp.rproxy.ServeHTTP(w, r)
		return
	}

	if !found {
		gp.logger.Printf("REPLAY MISS: no snapshot for %s %s — forwarding to upstream",
			r.Method, r.URL.Path)
		gp.rproxy.ServeHTTP(w, r)
		return
	}

	gp.logger.Printf("REPLAY HIT: serving cached %s %s → %d",
		r.Method, r.URL.Path, snap.StatusCode)

	// Restore all captured headers
	for key, values := range snap.Headers {
		for _, val := range values {
			w.Header().Add(key, val)
		}
	}

	// Mark the response as served from GhostProxy replay cache
	w.Header().Set("X-GhostProxy-Source", "replay-cache")
	w.Header().Set("X-GhostProxy-CapturedAt", snap.CapturedAt)

	w.WriteHeader(snap.StatusCode)

	if _, err := w.Write([]byte(snap.Body)); err != nil {
		gp.logger.Printf("REPLAY ERROR: failed to write response body: %v", err)
	}
}

// handleChaos evaluates per-route chaos injection rules and applies the
// configured fault simulation. If the matched route has chaos enabled,
// the engine injects artificial latency and/or returns a simulated error
// response. Routes without chaos rules or with chaos disabled are
// transparently forwarded to the upstream service.
func (gp *ghostProxy) handleChaos(w http.ResponseWriter, r *http.Request) {
	route := gp.cfg.FindRoute(r.Method, r.URL.Path)

	// No matching route or chaos disabled: transparent passthrough
	if route == nil || !route.Chaos.Enabled {
		gp.logger.Printf("CHAOS PASS: no active chaos rule for %s %s — forwarding",
			r.Method, r.URL.Path)
		gp.rproxy.ServeHTTP(w, r)
		return
	}

	// Phase 1: Inject artificial latency if configured
	if route.Chaos.LatencyMs > 0 {
		delay := time.Duration(route.Chaos.LatencyMs) * time.Millisecond
		gp.logger.Printf("CHAOS INJECT: adding %dms latency to %s %s",
			route.Chaos.LatencyMs, r.Method, r.URL.Path)
		time.Sleep(delay)
	}

	// Phase 2: Return simulated error if error code is configured
	if route.Chaos.ErrorCode > 0 {
		gp.logger.Printf("CHAOS FAULT: returning %d for %s %s",
			route.Chaos.ErrorCode, r.Method, r.URL.Path)

		errorMsg := route.Chaos.ErrorMessage
		if errorMsg == "" {
			errorMsg = fmt.Sprintf("GhostProxy chaos injection: simulated %d error", route.Chaos.ErrorCode)
		}

		errResp := chaosErrorResponse{
			Error:     errorMsg,
			Code:      route.Chaos.ErrorCode,
			Injected:  true,
			Route:     route.Path,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Mode:      "chaos",
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-GhostProxy-Source", "chaos-injection")
		w.Header().Set("X-GhostProxy-Chaos-Rule", route.Path)
		w.WriteHeader(route.Chaos.ErrorCode)

		if err := json.NewEncoder(w).Encode(errResp); err != nil {
			gp.logger.Printf("CHAOS ERROR: failed to encode error response: %v", err)
		}
		return
	}

	// Latency-only chaos: inject delay then forward to upstream
	gp.logger.Printf("CHAOS FORWARD: latency injected, forwarding %s %s to upstream",
		r.Method, r.URL.Path)
	gp.rproxy.ServeHTTP(w, r)
}

// handleStatus returns a JSON summary of the current GhostProxy state
// including operational mode, upstream target, and cached snapshot count.
func (gp *ghostProxy) handleStatus(w http.ResponseWriter, _ *http.Request) {
	snapshotCount := 0
	snapshots, err := gp.store.ListSnapshots()
	if err == nil {
		snapshotCount = len(snapshots)
	}

	status := proxyStatusResponse{
		Status:    "operational",
		Mode:      string(gp.cfg.Mode),
		Upstream:  gp.cfg.Upstream.Target,
		Snapshots: snapshotCount,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(status); err != nil {
		gp.logger.Printf("STATUS ERROR: failed to encode status response: %v", err)
	}
}

// errorHandler is invoked by httputil.ReverseProxy when the upstream
// connection fails entirely. It returns a structured JSON error response
// to the client with diagnostic information.
func (gp *ghostProxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	gp.logger.Printf("UPSTREAM ERROR: %s %s → %v", r.Method, r.URL.Path, err)

	errResp := chaosErrorResponse{
		Error:     fmt.Sprintf("GhostProxy upstream error: %v", err),
		Code:      http.StatusBadGateway,
		Injected:  false,
		Route:     r.URL.Path,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Mode:      string(gp.cfg.Mode),
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GhostProxy-Source", "error-handler")
	w.WriteHeader(http.StatusBadGateway)

	if encodeErr := json.NewEncoder(w).Encode(errResp); encodeErr != nil {
		gp.logger.Printf("ERROR HANDLER: failed to encode error response: %v", encodeErr)
	}
}

// singleJoiningSlash joins two URL path segments with exactly one
// separating slash, handling trailing/leading slash edge cases.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// isHopByHopHeader returns true if the given lowercase header name is a
// hop-by-hop header that should not be persisted in response snapshots.
func isHopByHopHeader(header string) bool {
	return hopByHopHeaders[header]
}
