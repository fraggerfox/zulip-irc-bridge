package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	return New(srv.URL, "bot@example.com", "secret-key"), srv
}

func TestSendMessageAuthAndForm(t *testing.T) {
	var gotAuth, gotType, gotTo, gotTopic, gotContent string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		gotAuth = user + ":" + pass
		r.ParseForm()
		gotType = r.PostForm.Get("type")
		gotTo = r.PostForm.Get("to")
		gotTopic = r.PostForm.Get("topic")
		gotContent = r.PostForm.Get("content")
		fmt.Fprint(w, `{"result":"success","msg":"","id":42}`)
	})
	defer srv.Close()

	if err := c.SendMessage(context.Background(), "irc-chan", "general", "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if gotAuth != "bot@example.com:secret-key" {
		t.Errorf("basic auth = %q", gotAuth)
	}
	if gotType != "stream" || gotTo != "irc-chan" || gotTopic != "general" || gotContent != "hello" {
		t.Errorf("form = %q %q %q %q", gotType, gotTo, gotTopic, gotContent)
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"result":"error","msg":"Stream does not exist","code":"STREAM_DOES_NOT_EXIST"}`)
	})
	defer srv.Close()

	err := c.SendMessage(context.Background(), "nope", "t", "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if apiErr.Code != "STREAM_DOES_NOT_EXIST" || apiErr.Status != 400 {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestSendWithRetryRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(502)
			fmt.Fprint(w, `{"result":"error","msg":"bad gateway","code":"INTERNAL"}`)
			return
		}
		fmt.Fprint(w, `{"result":"success","msg":""}`)
	})
	defer srv.Close()

	err := c.SendWithRetry(context.Background(), "s", "t", "x", 5, time.Millisecond)
	if err != nil {
		t.Fatalf("SendWithRetry: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestSendWithRetryDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"result":"error","msg":"nope","code":"BAD_REQUEST"}`)
	})
	defer srv.Close()

	err := c.SendWithRetry(context.Background(), "s", "t", "x", 5, time.Millisecond)
	if err == nil {
		t.Fatal("want error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestSendWithRetryHonorsContext(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"result":"error","msg":"boom","code":"INTERNAL"}`)
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.SendWithRetry(ctx, "s", "t", "x", 5, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRegisterAndGetMessages(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/register":
			fmt.Fprint(w, `{"result":"success","msg":"","queue_id":"q1","last_event_id":-1}`)
		case "/api/v1/events":
			if r.URL.Query().Get("queue_id") != "q1" {
				t.Errorf("queue_id = %q", r.URL.Query().Get("queue_id"))
			}
			fmt.Fprint(w, `{"result":"success","msg":"","events":[
				{"id":0,"type":"heartbeat"},
				{"id":1,"type":"message","message":{
					"id":100,"sender_email":"user@example.com",
					"sender_full_name":"User","type":"stream",
					"display_recipient":"irc-chan","subject":"general",
					"content":"hi from zulip"}}
			]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	defer srv.Close()

	q, err := c.RegisterQueue(context.Background())
	if err != nil {
		t.Fatalf("RegisterQueue: %v", err)
	}
	if q.ID != "q1" || q.LastEventID != -1 {
		t.Errorf("queue = %+v", q)
	}

	msgs, err := c.GetMessages(context.Background(), &q)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1 (heartbeat skipped)", len(msgs))
	}
	if msgs[0].Stream() != "irc-chan" || msgs[0].Content != "hi from zulip" {
		t.Errorf("msg = %+v", msgs[0])
	}
	if q.LastEventID != 1 {
		t.Errorf("LastEventID = %d, want 1", q.LastEventID)
	}
}

func TestGetMessagesBadQueue(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"result":"error","msg":"Bad event queue id","code":"BAD_EVENT_QUEUE_ID"}`)
	})
	defer srv.Close()

	q := Queue{ID: "stale", LastEventID: 5}
	_, err := c.GetMessages(context.Background(), &q)
	if !errors.Is(err, ErrBadQueue) {
		t.Fatalf("want ErrBadQueue, got %v", err)
	}
}

func TestDirectMessageRecipientDoesNotBreakDecoding(t *testing.T) {
	raw := `{"id":100,"sender_email":"u@e.com","sender_full_name":"U","type":"private",
		"display_recipient":[{"email":"a@b.c"}],"subject":"","content":"psst"}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal DM: %v", err)
	}
	if m.Stream() != "" {
		t.Errorf("Stream() for DM = %q, want empty", m.Stream())
	}
}

func TestNonJSONResponse(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		fmt.Fprint(w, "<html>bad gateway</html>")
	})
	defer srv.Close()

	err := c.SendMessage(context.Background(), "s", "t", "x")
	if err == nil {
		t.Fatal("want error for non-JSON response")
	}
}
