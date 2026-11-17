package main

import (
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestScrubEvent(t *testing.T) {
	e := &sentry.Event{
		Message: "bad token Bearer abc.def for a@b.com",
		Request: &sentry.Request{
			URL:         "https://x.test/ws?token=eyJsecret&x=1",
			QueryString: "token=eyJsecret",
			Headers:     map[string]string{"Authorization": "Bearer eyJ", "Cookie": "s=1", "Accept": "a"},
		},
	}
	scrubEvent(e)
	if e.Request.Headers["Authorization"] != redacted || e.Request.Headers["Cookie"] != redacted || e.Request.Headers["Accept"] != "a" {
		t.Fatalf("headers: %v", e.Request.Headers)
	}
	if strings.Contains(e.Request.URL, "eyJsecret") || strings.Contains(e.Request.QueryString, "eyJsecret") {
		t.Fatalf("token leaked: %q %q", e.Request.URL, e.Request.QueryString)
	}
	if strings.Contains(e.Message, "abc.def") || strings.Contains(e.Message, "a@b.com") {
		t.Fatalf("message leaked: %q", e.Message)
	}
}

func TestInitInertWithoutDSN(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	initSentry("event-service")()
	if sentryEnabled {
		t.Fatal("enabled without DSN")
	}
}
