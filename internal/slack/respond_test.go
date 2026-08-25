package slack_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// responseServer stands in for hooks.slack.com. The host check in Respond is
// what keeps a stored response URL from pointing anywhere else, so a test that
// wants to exercise the POST has to disable it — hence the export hook rather
// than a relaxed rule in production code.
func responseServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *slack.Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, slept := newTestClient(t, srv, slack.Config{})
	c.AllowResponseHostForTest(srv.Listener.Addr().String())
	return srv, c, slept
}

func TestRespondPostsTheMessage(t *testing.T) {
	t.Parallel()

	var got slack.ResponseMessage
	var auth string
	srv, c, _ := responseServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = w.Write([]byte("ok"))
	})

	msg := slack.ResponseMessage{Text: "hi", ResponseType: slack.ResponseInChannel}
	if err := c.Respond(t.Context(), srv.URL+"/commands/T1/B2/C3", msg); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got.Text != "hi" || got.ResponseType != slack.ResponseInChannel {
		t.Errorf("posted %+v, want the message as given", got)
	}
	// The URL is the credential; sending the bot token as well would hand a
	// second one to whoever holds it.
	if auth != "" {
		t.Errorf("Authorization = %q, want none: a response URL carries its own authority", auth)
	}
}

// TestRespondRejectsAForeignHost: the URL is read back out of a job argument —
// a database column — minutes after Slack sent it, and handed to an HTTP client.
// Pinning the host is what stops a corrupted or edited row from turning this
// service into a request forger.
func TestRespondRejectsAForeignHost(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, httptest.NewServer(http.NotFoundHandler()), slack.Config{})
	for _, url := range []string{
		"https://evil.example.com/commands/T1/B2/C3",
		"http://hooks.slack.com/commands/T1/B2/C3", // plain HTTP
		"https://hooks.slack.com.evil.example.com/x",
		"",
	} {
		if err := c.Respond(t.Context(), url, slack.NoticeMessage("hi")); err == nil {
			t.Errorf("Respond(%q) = nil, want a refusal", url)
		}
	}
}

// TestRespondRetriesServerErrorsButNotExpiry: a 500 is Slack having a moment; a
// 404 is the response URL past its 30 minutes or its five messages, and no
// number of retries brings either back.
func TestRespondRetriesServerErrorsButNotExpiry(t *testing.T) {
	t.Parallel()

	t.Run("retries a 500", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		srv, c, _ := responseServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte("ok"))
		})
		if err := c.Respond(t.Context(), srv.URL+"/commands/x", slack.NoticeMessage("hi")); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("calls = %d, want the 500 retried once", got)
		}
	})

	t.Run("stops on an expired URL", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		srv, c, _ := responseServer(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("expired_url"))
		})
		err := c.Respond(t.Context(), srv.URL+"/commands/x", slack.NoticeMessage("hi"))
		if err == nil {
			t.Fatal("want an error for an expired response URL")
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("calls = %d, want no retry of a dead URL", got)
		}
		if !strings.Contains(err.Error(), "expired_url") {
			t.Errorf("error does not carry Slack's reason: %v", err)
		}
	})
}

// TestRespondNeverEchoesTheURL: an error is the one place a capability URL
// predictably ends up in a log.
func TestRespondNeverEchoesTheURL(t *testing.T) {
	t.Parallel()

	const secretPath = "/commands/T1/B00000000/zzzzSECRETzzzz"
	srv, c, _ := responseServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("no_text"))
	})
	err := c.Respond(t.Context(), srv.URL+secretPath, slack.NoticeMessage("hi"))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("the response URL leaked into the error: %v", err)
	}
}

// TestMessageResponseCarriesTheBlocks: a digest part answers a command as the
// same blocks the scheduled digest posts, with only the audience differing.
func TestMessageResponseCarriesTheBlocks(t *testing.T) {
	t.Parallel()

	msgs := slack.BuildDigest(slack.DigestData{
		Team: "payments", Project: "payments",
		People: []slack.PersonDigest{{
			Person:   slack.Mention{SlackID: "U1"},
			ToReview: []slack.ReviewItem{{IID: 1, Title: "T", WebURL: "https://gl/1"}},
		}},
	})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}

	inChannel := msgs[0].Response(true)
	if inChannel.ResponseType != slack.ResponseInChannel {
		t.Errorf("response_type = %q, want %q", inChannel.ResponseType, slack.ResponseInChannel)
	}
	if len(inChannel.Blocks) != len(msgs[0].Blocks) || inChannel.Text != msgs[0].Text {
		t.Error("the command answer must be the digest message itself")
	}
	// Ephemeral is Slack's default, so the personal answer sends no type at all;
	// sending "ephemeral" explicitly is equivalent but the zero value is what the
	// struct's omitempty is for.
	if got := msgs[0].Response(false).ResponseType; got != "" {
		t.Errorf("response_type = %q, want empty (ephemeral)", got)
	}
}
