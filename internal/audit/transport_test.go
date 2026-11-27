package audit

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTransportRecordsTheWholeTransfer(t *testing.T) {
	path := useLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/limited":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/redirect":
			http.Redirect(w, r, "/data", http.StatusFound)
		case "/broken":
			// Promises 100 bytes, sends 4, and hangs up.
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("data"))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		default:
			_, _ = w.Write([]byte("data"))
		}
	}))
	t.Cleanup(srv.Close)

	get := func(hc *http.Client, p string) {
		t.Helper()
		resp, err := hc.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	download := &http.Client{Transport: Transport(nil, "files.download", true)}
	upload := &http.Client{
		Transport:     Transport(nil, "files.upload", false),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get(download, "/limited")
	get(download, "/redirect") // two lines: the redirect, then the data
	get(upload, "/redirect")
	get(download, "/broken")

	lines := readLines(t, path)
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5: %+v", len(lines), lines)
	}
	if e := lines[0]; !e.RateLimited || e.RetryAfterS != 30 {
		t.Errorf("429 = %+v, want retry_after_s 30", e)
	}
	if e := lines[1]; e.Status != 302 || e.Failed() {
		t.Errorf("download redirect = %+v, want a 302 that is no failure", e)
	}
	if e := lines[2]; e.Status != 200 || e.Failed() {
		t.Errorf("download after the redirect = %+v", e)
	}
	if e := lines[3]; e.Method != "files.upload" || e.Status != 302 || e.Error != "redirect" || !e.Failed() {
		t.Errorf("upload redirect = %+v, want the failure \"redirect\"", e)
	}
	if e := lines[4]; e.Status != 200 || e.Error != "network" || !e.Failed() {
		t.Errorf("broken body = %+v, want a 200 that failed with \"network\"", e)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]float64{
		"": 0, "30": 30, " 1.5 ": 1.5, "NaN": 0, "-3": 0, "soon": 0,
		now.Add(time.Minute).Format(http.TimeFormat):  60,
		now.Add(-time.Minute).Format(http.TimeFormat): 0,
	} {
		if got := retryAfterSeconds(h, now); got != want {
			t.Errorf("retryAfterSeconds(%q) = %v, want %v", h, got, want)
		}
	}
}
