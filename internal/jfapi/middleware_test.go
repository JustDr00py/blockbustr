package jfapi

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestLogRequestsQueryOfFailedRequest(t *testing.T) {
	var logs bytes.Buffer
	rt := newTestRouter(t, &logs)
	rt.Get("/fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	do(rt, "GET", "/fail?searchTerm=dune&ApiKey=s3cret&api_key=s3cret&sig=s3cret", "", nil)
	out := logs.String()
	if !strings.Contains(out, "searchTerm=dune") || !strings.Contains(out, "level=ERROR") {
		t.Errorf("failed request should log its query:\n%s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("credentials leaked into the log:\n%s", out)
	}

	logs.Reset()
	do(rt, "GET", "/System/Info/Public?ApiKey=s3cret", "", nil)
	if strings.Contains(logs.String(), "ApiKey") {
		t.Errorf("a fast successful request should not log its query:\n%s", logs.String())
	}
}

func TestLogRequestsClientClosedIsDebug(t *testing.T) {
	var logs bytes.Buffer
	rt := newTestRouter(t, &logs)
	rt.Get("/gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(StatusClientClosed) })
	do(rt, "GET", "/gone", "", nil)
	if out := logs.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "status=499") {
		t.Errorf("a canceled request should log at debug:\n%s", out)
	}
}
