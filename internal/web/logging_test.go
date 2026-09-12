package web

import (
	"bytes"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/infodancer/logging"
)

// The web layer logged through the standard log package, which put its lines
// outside every level-based alert. A caller that hands it a logger gets them.
func TestWithLoggerRoutesWebLines(t *testing.T) {
	var buf bytes.Buffer
	h := &handlers{logger: logging.NewLoggerTo(&buf, "info")}

	// An unknown template is the web layer's own error: the request fails with
	// a 500 and the only account of why is the log.
	rec := httptest.NewRecorder()
	h.renderPublicPage(rec, "no-such-template", nil)

	out := buf.String()
	if !strings.Contains(out, "level=error") {
		t.Errorf("expected an error-level line, got: %s", out)
	}
	if !strings.Contains(out, "template=no-such-template") {
		t.Errorf("expected the template name as an attribute, got: %s", out)
	}
}

// Recovery reports a panic through the logger it is given, with the request
// that caused it -- a panic with no route attached is most of a page of stack
// and no way to reproduce it.
func TestRecoveryLogsThePanicWithItsRequest(t *testing.T) {
	var buf bytes.Buffer
	h := Recovery(logging.NewLoggerTo(&buf, "info"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/articles/7", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{"level=error", "panic=boom", "path=/articles/7", "method=GET"} {
		if !strings.Contains(out, want) {
			t.Errorf("recovery line missing %q: %s", want, out)
		}
	}
}

// A handler built without a logger still logs -- to the default, which the
// command sets. Silence would be worse than a line in the wrong place.
func TestRecoveryWithoutALoggerUsesTheDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(logging.NewLoggerTo(&buf, "info"))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := Recovery(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(buf.String(), "level=error") {
		t.Errorf("a nil logger should fall back to slog.Default(), got: %s", buf.String())
	}
}

// Nothing in this package may write to the standard log package: those lines
// carry no level, so Loki reads them as detected_level=unknown and the error
// alert never matches them.
func TestNothingUsesTheStandardLog(t *testing.T) {
	var stdlog bytes.Buffer
	log.SetOutput(&stdlog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var buf bytes.Buffer
	h := &handlers{logger: logging.NewLoggerTo(&buf, "info")}
	h.renderPublicPage(httptest.NewRecorder(), "no-such-template", nil)

	rec := Recovery(logging.NewLoggerTo(&buf, "info"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if stdlog.Len() != 0 {
		t.Errorf("something logged through the standard log package:\n%s", stdlog.String())
	}
}
