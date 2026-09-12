// Package herdr talks to a running Herdr server over its local socket API and
// normalizes the result into attention items.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// SourceName is the attention source this adapter owns.
const SourceName = "herdr"

// ErrorBody is the Herdr socket API error shape.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ErrorBody) Error() string {
	return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message)
}

// NotFound reports whether err is a Herdr not_found error.
func NotFound(err error) bool {
	var body *ErrorBody
	return errors.As(err, &body) && body.Code == "not_found"
}

// Client issues one request per connection over the Herdr socket. Herdr keeps
// no per-connection state for plain request/response methods, and a local Unix
// socket dial is cheap next to the connection bookkeeping a pooled client would
// need. Event subscriptions would need a long-lived connection; this client
// does not do them.
type Client struct {
	socket  string
	timeout time.Duration
	counter atomic.Uint64
}

// NewClient returns a client. An empty socket path is resolved on each call, so
// a Herdr server started after attentiond is picked up without a restart.
func NewClient(socket string, timeout time.Duration) *Client {
	return &Client{socket: socket, timeout: timeout}
}

// Call sends one request and decodes result into out, which may be nil.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	socket, err := ResolveSocket(c.socket)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("dial herdr socket %s: %w", socket, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	if params == nil {
		params = struct{}{}
	}
	request := struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{
		ID:     "attentiond-" + strconv.FormatUint(c.counter.Add(1), 10),
		Method: method,
		Params: params,
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}

	// Snapshots run to hundreds of kilobytes, so read the line without the
	// token size ceiling bufio.Scanner would impose.
	line, err := bufio.NewReaderSize(conn, 64*1024).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}

	var response struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *ErrorBody      `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if response.Error != nil {
		return response.Error
	}
	if response.ID != request.ID {
		return fmt.Errorf("herdr replied to %q, expected %q", response.ID, request.ID)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}
