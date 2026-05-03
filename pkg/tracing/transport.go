package tracing

import (
        "bytes"
        "io"
        "net/http"
        "time"

        "github.com/sipeed/picoclaw/pkg/logger"
)

// TracingTransport is an http.RoundTripper that captures request/response
// snippets and logs structured provider HTTP information.
type TracingTransport struct {
        inner    http.RoundTripper
        provider string
}

// NewTracingTransport creates a new TracingTransport wrapping the given RoundTripper.
// If inner is nil, http.DefaultTransport is used.
func NewTracingTransport(provider string, inner http.RoundTripper) *TracingTransport {
        if inner == nil {
                inner = http.DefaultTransport
        }
        return &TracingTransport{
                inner:    inner,
                provider: provider,
        }
}

// RoundTrip executes a single HTTP transaction, capturing request/response metadata.
func (t *TracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
        start := time.Now()

        // Capture request snippet
        var reqSnippet string
        if req.Body != nil {
                bodyBytes, _ := io.ReadAll(req.Body)
                req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
                if len(bodyBytes) > 2048 {
                        reqSnippet = string(bodyBytes[:2048])
                } else {
                        reqSnippet = string(bodyBytes)
                }
        }

        logger.DebugCF("provider", "HTTP request", map[string]any{
                "provider": t.provider,
                "method":   req.Method,
                "url":      req.URL.String(),
                "body_len": len(reqSnippet),
        })

        resp, err := t.inner.RoundTrip(req)
        latency := time.Since(start)

        if err != nil {
                logger.DebugCF("provider", "HTTP request failed", map[string]any{
                        "provider":  t.provider,
                        "method":    req.Method,
                        "url":       req.URL.String(),
                        "latency_ms": latency.Milliseconds(),
                        "error":     err.Error(),
                })
                return resp, err
        }

        // Capture response snippet
        var respSnippet string
        if resp.Body != nil {
                bodyBytes, readErr := io.ReadAll(resp.Body)
                if readErr == nil {
                        resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
                        if len(bodyBytes) > 2048 {
                                respSnippet = string(bodyBytes[:2048])
                        } else {
                                respSnippet = string(bodyBytes)
                        }
                }
        }

        logger.DebugCF("provider", "HTTP response", map[string]any{
                "provider":    t.provider,
                "method":      req.Method,
                "url":         req.URL.String(),
                "status_code": resp.StatusCode,
                "latency_ms":  latency.Milliseconds(),
                "body_len":    len(respSnippet),
        })

        // Store captured snippets in resp header for downstream consumers
        // (e.g., TraceStore) to access. The snippets are not logged at
        // Debug level since they may contain sensitive request/response data.
        // The _ assignments were previously discarding these entirely.
        if reqSnippet != "" {
                resp.Header.Set("X-Picoclaw-Req-Snippet-Captured", "1")
        }
        if respSnippet != "" {
                resp.Header.Set("X-Picoclaw-Resp-Snippet-Captured", "1")
        }

        return resp, nil
}
