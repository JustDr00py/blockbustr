package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestInternalErrorOfCanceledRequest(t *testing.T) {
	a := &api{Deps: Deps{Log: testutil.Discard()}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	a.internalError(rec, httptest.NewRequestWithContext(ctx, "GET", "/Items", nil), context.Canceled)
	if rec.Code != jfapi.StatusClientClosed {
		t.Errorf("canceled request: status %d, want %d", rec.Code, jfapi.StatusClientClosed)
	}

	rec = httptest.NewRecorder()
	a.internalError(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/Items", nil), errors.New("db down"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("live request: status %d, want 500", rec.Code)
	}
}
