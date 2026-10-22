package remotemcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// sendNotification sends a JSON-RPC notification. The endpoint normally
// answers 202/204 with an empty body.
func (c *Client) sendNotification(ctx context.Context, method string, params any) error {
	_, _, err := c.do(ctx, method, params, false)
	return err
}

// do performs one bounded Streamable HTTP request. When wantResponse is false
// the request is a JSON-RPC notification and any 2xx response is accepted.
func (c *Client) do(ctx context.Context, method string, params any, wantResponse bool) (json.RawMessage, http.Header, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, ErrClosed
	}
	sessionID := c.sessionID
	protocolVersion := c.protocolVersion
	timeout := c.timeout
	var id int64
	if wantResponse {
		id = c.nextID
		c.nextID++
	}
	c.mu.Unlock()

	rawParams, err := marshalBounded(params)
	if err != nil {
		return nil, nil, err
	}

	var body []byte
	if wantResponse {
		body, err = json.Marshal(rpcRequest{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: rawParams})
	} else {
		body, err = json.Marshal(rpcNotification{JSONRPC: JSONRPCVersion, Method: method, Params: rawParams})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: marshal request: %v", ErrInvalidRequest, err)
	}
	if len(body) > maxResponseBytes {
		return nil, nil, fmt.Errorf("%w: request %d > %d bytes", ErrTooLarge, len(body), maxResponseBytes)
	}

	reqCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: build request: %v", ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	if sessionID != "" {
		httpReq.Header.Set(headerSessionID, sessionID)
	}
	if method != methodInitialize && protocolVersion != "" {
		httpReq.Header.Set(headerProtocolVersion, protocolVersion)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, nil, transportError(ctx, reqCtx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if readErr != nil {
			return nil, nil, transportError(ctx, reqCtx, readErr)
		}
		return nil, nil, httpStatusError(resp, errBody)
	}
	if !wantResponse {
		return nil, resp.Header, nil
	}

	switch mediaType := mediaTypeOf(resp.Header); mediaType {
	case "text/event-stream":
		// Parse incrementally and return on the first complete matching event;
		// a Streamable HTTP SSE response may stay open after the response.
		result, sseErr := parseSSEStream(reqCtx, resp.Body, id)
		if sseErr != nil {
			var readErr *sseReadError
			if errors.As(sseErr, &readErr) {
				return nil, nil, transportError(ctx, reqCtx, readErr.err)
			}
			if reqCtx.Err() != nil ||
				errors.Is(sseErr, context.Canceled) || errors.Is(sseErr, context.DeadlineExceeded) {
				return nil, nil, transportError(ctx, reqCtx, sseErr)
			}
			return nil, nil, sseErr
		}
		return result, resp.Header, nil
	case "application/json":
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if readErr != nil {
			return nil, nil, transportError(ctx, reqCtx, readErr)
		}
		if len(respBody) > maxResponseBytes {
			return nil, nil, fmt.Errorf("%w: response %d > %d bytes", ErrTooLarge, len(respBody), maxResponseBytes)
		}
		result, parseErr := parseJSONResponse(respBody, id)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		return result, resp.Header, nil
	case "":
		return nil, nil, fmt.Errorf("%w: missing or invalid Content-Type %q", ErrProtocol, resp.Header.Get("Content-Type"))
	default:
		return nil, nil, fmt.Errorf("%w: unsupported Content-Type %q", ErrProtocol, mediaType)
	}
}

func marshalBounded(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal params: %v", ErrInvalidRequest, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: params %d > %d bytes", ErrTooLarge, len(raw), maxResponseBytes)
	}
	return raw, nil
}

func transportError(ctx, reqCtx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("%w: %v", ErrCanceled, err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(reqCtx.Err(), context.DeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	if errors.Is(reqCtx.Err(), context.Canceled) {
		return fmt.Errorf("%w: %v", ErrCanceled, err)
	}
	if isTLSError(err) {
		return fmt.Errorf("%w: %w", ErrTLS, err)
	}
	return fmt.Errorf("%w: %v", ErrNetwork, err)
}

func isTLSError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return true
	}
	var recordErr tls.RecordHeaderError
	return errors.As(err, &recordErr)
}

func httpStatusError(resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		msg := "HTTP 401 Unauthorized"
		if challenge := strings.TrimSpace(resp.Header.Get("WWW-Authenticate")); challenge != "" {
			msg += " (" + snippet([]byte(challenge), 128) + ")"
		}
		return fmt.Errorf("%w: %s", ErrUnauthorized, msg)
	case http.StatusForbidden:
		return fmt.Errorf("%w: HTTP 403 Forbidden", ErrForbidden)
	default:
		return fmt.Errorf("%w: HTTP %d %s: %s", ErrProtocol, resp.StatusCode,
			http.StatusText(resp.StatusCode), snippet(body, maxErrorSnippetBytes))
	}
}

// snippet returns a bounded single-line rendering of response bytes for error
// messages. It never includes request credentials.
func snippet(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func parseJSONResponse(body []byte, id int64) (json.RawMessage, error) {
	result, err, found := extractResponse(body, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no JSON-RPC response for id %d", ErrProtocol, id)
	}
	return result, nil
}

// sseReadError marks a transport read failure from a streaming SSE body so the
// caller can classify it as a network/timeout error instead of a protocol error.
type sseReadError struct{ err error }

func (e *sseReadError) Error() string { return e.err.Error() }
func (e *sseReadError) Unwrap() error { return e.err }

// parseSSEStream incrementally parses server-sent data events until the
// JSON-RPC response matching id is complete. It returns as soon as a complete
// matching event (terminated by a blank line) has been consumed; it does not
// wait for the server to close a streaming response. Total bytes and event
// count stay bounded, and ctx cancellation/deadline unblocks a held stream.
func parseSSEStream(ctx context.Context, body io.Reader, id int64) (json.RawMessage, error) {
	reader := bufio.NewReaderSize(body, 4096)
	var data []byte
	events := 0
	total := 0

	dispatch := func() (json.RawMessage, error, bool) {
		if len(data) == 0 {
			return nil, nil, false
		}
		events++
		if events > maxSSEEvents {
			return nil, fmt.Errorf("%w: more than %d SSE events", ErrProtocol, maxSSEEvents), true
		}
		result, err, found := extractResponse(data, id)
		if err != nil {
			return nil, err, true
		}
		if !found {
			return nil, nil, false
		}
		return result, nil, true
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadString('\n')
		total += len(line)
		if total > maxResponseBytes {
			return nil, fmt.Errorf("%w: SSE response exceeds %d bytes", ErrTooLarge, maxResponseBytes)
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if result, err, found := dispatch(); found {
				return result, err
			}
			data = data[:0]
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return nil, fmt.Errorf("%w: no JSON-RPC response for id %d in event stream", ErrProtocol, id)
				}
				return nil, &sseReadError{err: readErr}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if result, err, found := dispatch(); found {
					return result, err
				}
				return nil, fmt.Errorf("%w: no JSON-RPC response for id %d in event stream", ErrProtocol, id)
			}
			return nil, &sseReadError{err: readErr}
		}
	}
}

// extractResponse looks for the JSON-RPC response for id in one JSON value
// (object or batch array). Notifications, other ids and non-responses are
// ignored. Matching error responses return an *RPCError.
func extractResponse(data []byte, id int64) (json.RawMessage, error, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, false
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("%w: malformed JSON-RPC batch: %v", ErrProtocol, err), false
		}
		for _, item := range items {
			result, err, found := extractResponse(item, id)
			if err != nil {
				return nil, err, found
			}
			if found {
				return result, nil, true
			}
		}
		return nil, nil, false
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, fmt.Errorf("%w: malformed JSON-RPC message: %v", ErrProtocol, err), false
	}
	msgID, hasID := parseID(msg.ID)
	if !hasID {
		return nil, nil, false // notification or server request
	}
	if msgID != id {
		return nil, nil, false
	}
	if msg.JSONRPC != JSONRPCVersion {
		return nil, fmt.Errorf("%w: response jsonrpc %q", ErrProtocol, msg.JSONRPC), true
	}
	if len(msg.Error) > 0 {
		var parsed struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg.Error, &parsed); err != nil || !isJSONObject(msg.Error) {
			return nil, fmt.Errorf("%w: malformed JSON-RPC error object", ErrProtocol), true
		}
		return nil, &RPCError{Code: parsed.Code, Message: parsed.Message, Data: parsed.Data}, true
	}
	if len(msg.Result) == 0 {
		return nil, fmt.Errorf("%w: response has neither result nor error", ErrProtocol), true
	}
	return append(json.RawMessage(nil), msg.Result...), nil, true
}

func parseID(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	if s[0] == '"' || s[0] == '{' || s[0] == '[' {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
