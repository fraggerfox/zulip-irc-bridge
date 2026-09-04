// Package zulip is a minimal Zulip REST API client covering exactly what
// the bridge needs: sending stream messages and long-polling the events
// API. Each Client owns its own http.Client; never share one Client
// between concurrently running loops.
package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is a non-success response from the Zulip API.
type APIError struct {
	Code   string
	Msg    string
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("zulip API error (HTTP %d, code %s): %s", e.Status, e.Code, e.Msg)
}

type Client struct {
	site  string
	email string
	key   string
	http  *http.Client
}

// New creates a client for the given Zulip server. Long-poll requests
// need generous timeouts, so the internal http.Client sets none; every
// call takes a context instead.
func New(site, email, key string) *Client {
	return &Client{
		site:  strings.TrimRight(site, "/"),
		email: email,
		key:   key,
		http:  &http.Client{},
	}
}

func (c *Client) call(ctx context.Context, method, path string, params url.Values, out any) error {
	var body io.Reader
	u := c.site + "/api/v1/" + path
	if method == http.MethodGet {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.email, c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	var envelope struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("non-JSON response (HTTP %d): %.200s", resp.StatusCode, raw)
	}
	if envelope.Result != "success" {
		return &APIError{Code: envelope.Code, Msg: envelope.Msg, Status: resp.StatusCode}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// SendMessage posts a message to a stream topic.
func (c *Client) SendMessage(ctx context.Context, stream, topic, content string) error {
	p := url.Values{}
	p.Set("type", "stream")
	p.Set("to", stream)
	p.Set("topic", topic)
	p.Set("content", content)
	return c.call(ctx, http.MethodPost, "messages", p, nil)
}

// Message is the subset of Zulip message fields the bridge uses.
//
// display_recipient is a string for stream messages but an array of
// user objects for direct messages, so it is decoded lazily: Stream()
// returns the stream name for stream messages and "" otherwise.
type Message struct {
	ID             int64           `json:"id"`
	SenderEmail    string          `json:"sender_email"`
	SenderFullName string          `json:"sender_full_name"`
	Type           string          `json:"type"`
	RawRecipient   json.RawMessage `json:"display_recipient"`
	Topic          string          `json:"subject"`
	Content        string          `json:"content"`
}

// Stream returns the stream name for stream messages, "" for anything
// else (direct messages, undecodable recipients).
func (m Message) Stream() string {
	if m.Type != "stream" {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.RawRecipient, &s); err != nil {
		return ""
	}
	return s
}

type event struct {
	ID      int64    `json:"id"`
	Type    string   `json:"type"`
	Message *Message `json:"message"`
}

// Queue is a registered event queue position.
type Queue struct {
	ID          string
	LastEventID int64
}

// RegisterQueue registers an event queue narrowed to message events.
func (c *Client) RegisterQueue(ctx context.Context) (Queue, error) {
	p := url.Values{}
	p.Set("event_types", `["message"]`)
	p.Set("apply_markdown", "false")
	var out struct {
		QueueID     string `json:"queue_id"`
		LastEventID int64  `json:"last_event_id"`
	}
	if err := c.call(ctx, http.MethodPost, "register", p, &out); err != nil {
		return Queue{}, err
	}
	return Queue{ID: out.QueueID, LastEventID: out.LastEventID}, nil
}

// ErrBadQueue is returned by GetMessages when the server no longer knows
// the queue (expired or server restarted); the caller must re-register.
var ErrBadQueue = fmt.Errorf("event queue expired")

// GetMessages long-polls the event queue and returns any new messages.
// Heartbeat and unknown events are consumed silently. The queue's
// LastEventID is advanced past every event seen.
func (c *Client) GetMessages(ctx context.Context, q *Queue) ([]Message, error) {
	p := url.Values{}
	p.Set("queue_id", q.ID)
	p.Set("last_event_id", fmt.Sprint(q.LastEventID))
	var out struct {
		Events []event `json:"events"`
	}
	if err := c.call(ctx, http.MethodGet, "events", p, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "BAD_EVENT_QUEUE_ID" {
			return nil, ErrBadQueue
		}
		return nil, err
	}
	var msgs []Message
	for _, ev := range out.Events {
		if ev.ID > q.LastEventID {
			q.LastEventID = ev.ID
		}
		if ev.Type == "message" && ev.Message != nil {
			msgs = append(msgs, *ev.Message)
		}
	}
	return msgs, nil
}

// SendWithRetry sends a message with bounded retries and exponential
// backoff. Client errors (4xx) are not retried — the message would fail
// again identically. Returns the last error after attempts are exhausted.
func (c *Client) SendWithRetry(ctx context.Context, stream, topic, content string, attempts int, baseDelay time.Duration) error {
	var err error
	delay := baseDelay
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		err = c.SendMessage(ctx, stream, topic, content)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
			return err
		}
	}
	return err
}
