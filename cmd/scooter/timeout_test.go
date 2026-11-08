package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStalledRequestTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c := newClient(srv.URL, headerSource{})
	c.timeout = 200 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- c.call(context.Background(), "GET", "/whoami", nil, nil) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "200ms") {
			t.Errorf("err = %v, want a timeout naming %s", err, srv.URL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("GET /whoami still waiting after 3s")
	}
}
