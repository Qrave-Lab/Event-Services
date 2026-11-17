package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// Optional Sentry error reporting. Completely inert unless SENTRY_DSN is set.
// No request bodies are attached; headers and sensitive query params are
// redacted in BeforeSend (the websocket token travels in ?token=).

const redacted = "[redacted]"

var (
	sentryEnabled    bool
	sensitiveHeaders = map[string]bool{"authorization": true, "cookie": true, "set-cookie": true, "x-csrf-token": true, "x-api-key": true, "proxy-authorization": true, "sec-websocket-protocol": true}
	sensitiveQuery   = []string{"token", "otp", "pin", "signature", "secret", "password", "key", "code", "sig"}
	emailRe          = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	digitsRe         = regexp.MustCompile(`\+?\d[\d\s\-]{7,}\d`)
	bearerRe         = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
)

func initSentry(service string) (flush func()) {
	dsn := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if dsn == "" {
		return func() {}
	}
	first := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(os.Getenv(n)); v != "" {
				return v
			}
		}
		return ""
	}
	rate, _ := strconv.ParseFloat(os.Getenv("SENTRY_TRACES_SAMPLE_RATE"), 64)
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      first("SENTRY_ENVIRONMENT", "APP_ENV"),
		Release:          first("SENTRY_RELEASE", "RENDER_GIT_COMMIT", "GIT_COMMIT"),
		EnableTracing:    rate > 0,
		TracesSampleRate: rate,
		SendDefaultPII:   false,
		AttachStacktrace: true,
		BeforeSend:       func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event { return scrubEvent(e) },
	})
	if err != nil {
		slog.Warn("sentry init failed; error reporting disabled", "error", err)
		return func() {}
	}
	sentryEnabled = true
	sentry.ConfigureScope(func(s *sentry.Scope) { s.SetTag("service", service) })
	return func() { sentry.Flush(5 * time.Second) }
}

func scrubEvent(e *sentry.Event) *sentry.Event {
	if e == nil {
		return nil
	}
	if r := e.Request; r != nil {
		for k := range r.Headers {
			if sensitiveHeaders[strings.ToLower(k)] {
				r.Headers[k] = redacted
			}
		}
		r.Cookies, r.Data, r.Env = "", "", nil
		r.QueryString = scrubQuery(r.QueryString)
		if u, err := url.Parse(r.URL); err == nil {
			u.User = nil
			u.RawQuery = scrubQuery(u.RawQuery)
			r.URL = u.String()
		}
	}
	e.User = sentry.User{ID: e.User.ID}
	e.Message = redactText(e.Message)
	for i := range e.Exception {
		e.Exception[i].Value = redactText(e.Exception[i].Value)
	}
	for _, b := range e.Breadcrumbs {
		b.Message, b.Data = redactText(b.Message), nil
	}
	return e
}

func scrubQuery(q string) string {
	if q == "" {
		return ""
	}
	vals, err := url.ParseQuery(q)
	if err != nil {
		return redacted
	}
	for k := range vals {
		lk := strings.ToLower(k)
		for _, s := range sensitiveQuery {
			if strings.Contains(lk, s) {
				vals[k] = []string{redacted}
				break
			}
		}
	}
	return vals.Encode()
}

func redactText(s string) string {
	s = bearerRe.ReplaceAllString(s, "Bearer "+redacted)
	s = emailRe.ReplaceAllString(s, redacted)
	return digitsRe.ReplaceAllString(s, redacted)
}

// reportPanic sends a recovered panic value to Sentry and flushes (the caller
// is about to re-panic, which may terminate the process).
func reportPanic(rec any, r *http.Request, where string) {
	if !sentryEnabled {
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(s *sentry.Scope) {
		s.SetTag("where", where)
		if r != nil {
			s.SetTag("route", r.URL.Path)
			s.SetRequest(r)
		}
		hub.Recover(rec)
	})
	hub.Flush(3 * time.Second)
}

// withSentry reports panics from HTTP/websocket handlers and re-panics, so
// net/http's own behaviour (log + close the connection) is unchanged.
func withSentry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec != http.ErrAbortHandler {
					reportPanic(rec, r, "http")
				}
				panic(rec)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// repanicReported is deferred at the top of a goroutine: it reports a panic to
// Sentry and then re-panics, keeping the existing crash behaviour.
func repanicReported(where string) {
	if rec := recover(); rec != nil {
		reportPanic(rec, nil, where)
		panic(rec)
	}
}
