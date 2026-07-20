package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		running  string
		want     bool
		wantErr  bool
	}{
		{"strictly newer", "0.4.8", "0.4.7", true, false},
		{"equal", "0.4.7", "0.4.7", false, false},
		{"older", "0.4.6", "0.4.7", false, false},
		{"leading v tolerated", "v0.5.0", "0.4.7", true, false},
		{"missing patch tolerated", "0.5", "0.4.7", true, false},
		{"dev running always outdated", "0.4.8", "dev", true, false},
		{"unparseable running always outdated", "0.4.8", "garbage", true, false},
		{"unparseable manifest errors", "not-a-version", "0.4.7", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := isNewer(tc.manifest, tc.running)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for manifest=%q", tc.manifest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("isNewer(%q,%q)=%v want %v", tc.manifest, tc.running, got, tc.want)
			}
		})
	}
}

func TestRequireHTTPS(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https ok", "https://example.com/x", false},
		{"http rejected", "http://example.com/x", true},
		{"file rejected", "file:///etc/passwd", true},
		{"no host rejected", "https://", true},
		{"empty rejected", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := requireHTTPS(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireHTTPS(%q) err=%v wantErr=%v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       Manifest
		wantErr bool
	}{
		{"valid", Manifest{Version: "0.4.8", URL: "https://x/y.tar.gz", Signature: "sig"}, false},
		{"missing version", Manifest{URL: "https://x/y.tar.gz", Signature: "sig"}, true},
		{"missing signature", Manifest{Version: "0.4.8", URL: "https://x/y.tar.gz"}, true},
		{"http url rejected", Manifest{Version: "0.4.8", URL: "http://x/y.tar.gz", Signature: "sig"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestFetchManifest(t *testing.T) {
	body := `{"version":"0.4.9","url":"https://example.com/Bladerunner.app.tar.gz","signature":"c2ln","notes":"fixes"}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("missing Accept header")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	m, err := fetchManifest(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetchManifest: %v", err)
	}
	if m.Version != "0.4.9" {
		t.Fatalf("version = %q", m.Version)
	}
	if m.Notes != "fixes" {
		t.Fatalf("notes = %q", m.Notes)
	}
}

func TestFetchManifest_HTTPStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchManifest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestFetchManifest_BadJSON(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	if _, err := fetchManifest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestFetchManifest_RejectsPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1","url":"https://x/y","signature":"s"}`))
	}))
	defer srv.Close()

	// srv.URL is http:// — fetchManifest must refuse before making the request.
	if _, err := fetchManifest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("expected fetchManifest to refuse a plain-http manifest URL")
	}
}
