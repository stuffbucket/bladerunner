package update_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/update"
)

// TestCheck_NoPublishedChannel holds the behavior a user sees when no manifest
// has been published: the site build writes no latest.json when no release
// carries the updater assets, so the manifest URL answers 404. That must read
// as "no published update channel", not as a raw HTTP status, because the two
// have opposite remedies — one is nothing to do, the other is a network fault.
func TestCheck_NoPublishedChannel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := update.Check(context.Background(), update.Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err == nil {
		t.Fatal("Check succeeded against a 404 manifest URL")
	}
	if !errors.Is(err, update.ErrNoUpdateChannel) {
		t.Fatalf("Check error = %v, want ErrNoUpdateChannel", err)
	}
	if strings.Contains(err.Error(), "404") {
		t.Errorf("error surfaces the raw status: %v", err)
	}
}

// TestCheck_ServerFaultIsNotMistakenForAnEmptyChannel keeps the honest 404
// message narrow. A 500 is a real fault and must keep saying so.
func TestCheck_ServerFaultIsNotMistakenForAnEmptyChannel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := update.Check(context.Background(), update.Options{
		CurrentVersion: "0.4.7",
		ManifestURL:    srv.URL,
		HTTPClient:     srv.Client(),
	})
	if err == nil {
		t.Fatal("Check succeeded against a failing manifest server")
	}
	if errors.Is(err, update.ErrNoUpdateChannel) {
		t.Fatalf("a 500 was reported as an empty update channel: %v", err)
	}
}
