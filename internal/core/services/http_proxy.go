package services

import (
	"bytes"
	"db-proxy/internal/core/domain"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxHTTPLogBodyBytes = 64 * 1024

// NewHTTPHandler creates a fixed-upstream HTTP reverse proxy for an API service.
// backendAddr is configuration-controlled, never taken from the client request.
func (ps *ProxyService) NewHTTPHandler(serviceName, backendAddr string) (http.Handler, error) {
	backendAddr = strings.TrimSpace(backendAddr)
	if backendAddr != "" && !strings.Contains(backendAddr, "://") {
		backendAddr = "http://" + backendAddr
	}
	target, err := url.Parse(backendAddr)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("API backend_addr must be an absolute http(s) URL, got %q", backendAddr)
	}
	if target.User != nil {
		return nil, fmt.Errorf("API backend_addr must not include credentials")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestLine := fmt.Sprintf("%s %s", r.Method, sanitizedRequestURI(r.URL))
		chaosPrefix := ""
		responseDelay := time.Duration(0)
		var apiRule *domain.ChaosRule
		if ps.chaosSvc != nil {
			if rule, matched := ps.chaosSvc.Evaluate(serviceName, requestLine); matched {
				apiRule = &rule
				if rule.Action == "delay" {
					requestDelay := time.Duration(rule.DelayRequestMs) * time.Millisecond
					responseDelay = time.Duration(rule.DelayResponseMs) * time.Millisecond
					if requestDelay > 0 {
						chaosPrefix = fmt.Sprintf("[CHAOS_DELAY_REQUEST: %dms] ", requestDelay.Milliseconds())
						if !waitForHTTPDelay(r, requestDelay) {
							return
						}
					}
					if responseDelay > 0 && chaosPrefix == "" {
						chaosPrefix = fmt.Sprintf("[CHAOS_DELAY_RESPONSE: %dms] ", responseDelay.Milliseconds())
					}
				}
			}
		}
		if apiRule != nil && apiRule.Action == "api_response" {
			status := apiRule.APIStatusCode
			if status < 100 || status > 599 {
				status = http.StatusServiceUnavailable
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, apiRule.APIResponseBody)
			ps.writeHTTPLog(serviceName, r, nil, fmt.Sprintf("HTTP %d Chaos Custom Response\n%s", status, cleanHTTPLog(apiRule.APIResponseBody)), started, "[CHAOS_API_RESPONSE] ")
			return
		}
		if apiRule != nil && apiRule.Action == "api_drop" {
			ps.writeHTTPLog(serviceName, r, nil, "[Request Dropped by Chaos Rule]", started, "[CHAOS_API_DROP] ")
			w.Header().Set("Connection", "close")
			http.Error(w, "DaPoeR API request dropped", http.StatusServiceUnavailable)
			return
		}
		requestCapture := &limitedCaptureReadCloser{ReadCloser: r.Body, limit: maxHTTPLogBodyBytes}
		outURL := *target
		outURL.Path = joinURLPath(target.Path, r.URL.Path)
		outURL.RawPath, outURL.RawQuery = "", r.URL.RawQuery
		outReq := r.Clone(r.Context())
		outReq.URL, outReq.RequestURI, outReq.Host = &outURL, "", target.Host
		outReq.Header, outReq.Body = r.Header.Clone(), requestCapture
		removeHopByHopHeaders(outReq.Header)

		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			ps.writeHTTPLog(serviceName, r, requestCapture.Bytes(), "upstream error: "+err.Error(), started, chaosPrefix)
			http.Error(w, "DaPoeR API upstream unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if responseDelay > 0 {
			ps.sysLog.Warn("[%s] Injecting response delay of %dms for API request from %s", serviceName, responseDelay.Milliseconds(), r.RemoteAddr)
			if !waitForHTTPDelay(r, responseDelay) {
				return
			}
		}
		copyHeaders(w.Header(), resp.Header)
		if apiRule != nil && apiRule.Action == "api_header" && http.CanonicalHeaderKey(apiRule.APIHeaderName) != "" {
			w.Header().Set(apiRule.APIHeaderName, apiRule.APIHeaderValue)
			chaosPrefix = "[CHAOS_API_HEADER] "
		}
		body := io.Reader(resp.Body)
		if apiRule != nil && apiRule.Action == "api_corrupt" {
			body = &corruptHTTPReader{Reader: resp.Body}
			chaosPrefix = "[CHAOS_API_CORRUPT] "
		}
		w.WriteHeader(resp.StatusCode)
		responseCapture := &limitedCaptureWriter{limit: maxHTTPLogBodyBytes}
		_, copyErr := io.Copy(w, io.TeeReader(body, responseCapture))
		response := fmt.Sprintf("HTTP %d %s\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), cleanHTTPLog(string(responseCapture.Bytes())))
		if copyErr != nil {
			response += "\n[response copy error: " + copyErr.Error() + "]"
		}
		ps.writeHTTPLog(serviceName, r, requestCapture.Bytes(), response, started, chaosPrefix)
	}), nil
}

type corruptHTTPReader struct {
	io.Reader
	corrupted bool
}

func (r *corruptHTTPReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if !r.corrupted {
		for i := 0; i < n; i++ {
			if p[i] != ' ' && p[i] != '\n' && p[i] != '\r' && p[i] != '\t' {
				p[i] = '!'
				r.corrupted = true
				break
			}
		}
	}
	return n, err
}

func waitForHTTPDelay(r *http.Request, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func (ps *ProxyService) writeHTTPLog(serviceName string, r *http.Request, requestBody []byte, response string, started time.Time, chaosPrefix string) {
	query := chaosPrefix + fmt.Sprintf("%s %s", r.Method, sanitizedRequestURI(r.URL))
	if headers := sanitizeHTTPHeaders(r.Header); headers != "" {
		query += "\nHeader : " + headers
	}
	if body := strings.TrimSpace(cleanHTTPLog(string(requestBody))); body != "" {
		query += "\nBody : " + body
	}
	entry := domain.LogEntry{Timestamp: started, ThirdParty: serviceName, ClientIP: r.RemoteAddr, Query: query, Response: response}
	if err := ps.logger.LogQuery(entry); err != nil {
		ps.sysLog.Error("[%s] Failed to log API request: %v", serviceName, err)
	}
	if ps.broadcaster != nil {
		ps.broadcaster.Broadcast(entry)
	}
}

type limitedCaptureReadCloser struct {
	io.ReadCloser
	buf   bytes.Buffer
	limit int
}

func (c *limitedCaptureReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 && c.buf.Len() < c.limit {
		remaining := c.limit - c.buf.Len()
		if n < remaining {
			remaining = n
		}
		_, _ = c.buf.Write(p[:remaining])
	}
	return n, err
}
func (c *limitedCaptureReadCloser) Bytes() []byte { return c.buf.Bytes() }

type limitedCaptureWriter struct {
	buf   bytes.Buffer
	limit int
}

func (c *limitedCaptureWriter) Write(p []byte) (int, error) {
	if c.buf.Len() < c.limit {
		remaining := c.limit - c.buf.Len()
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = c.buf.Write(p[:remaining])
	}
	return len(p), nil
}
func (c *limitedCaptureWriter) Bytes() []byte { return c.buf.Bytes() }
func joinURLPath(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		return requestPath
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(requestPath, "/")
}
func removeHopByHopHeaders(headers http.Header) {
	for _, h := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(h)
	}
}
func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if !isHopByHopHeader(key) {
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}
func isHopByHopHeader(key string) bool {
	for _, h := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		if strings.EqualFold(key, h) {
			return true
		}
	}
	return false
}

// formatHTTPHeaders produces a stable single-line representation suitable for
// file and dashboard logs. Header values are intentionally preserved verbatim
// (except CR/NUL control characters) for API debugging.
func sanitizeHTTPHeaders(headers http.Header) string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		values := headers.Values(key)
		formatted := make([]string, 0, len(values))
		for _, value := range values {
			formatted = append(formatted, cleanHTTPLog(value))
		}
		parts = append(parts, http.CanonicalHeaderKey(key)+": "+strings.Join(formatted, ", "))
	}
	return strings.Join(parts, "; ")
}

func sanitizedRequestURI(u *url.URL) string {
	return u.RequestURI()
}
func cleanHTTPLog(value string) string {
	return strings.NewReplacer("\r", "", "\x00", "").Replace(value)
}
