package drive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchThumbnail_EmptyLinkID(t *testing.T) {
	s := &Session{cacheBase: t.TempDir(), apiBase: "http://unused"}
	path, err := s.FetchThumbnail(context.Background(), "", "rev1")
	if err != nil || path != "" {
		t.Errorf("expected empty path, got %q, %v", path, err)
	}
}

func TestFetchThumbnail_EmptyRevisionID(t *testing.T) {
	s := &Session{cacheBase: t.TempDir(), apiBase: "http://unused"}
	path, err := s.FetchThumbnail(context.Background(), "link1", "")
	if err != nil || path != "" {
		t.Errorf("expected empty path, got %q, %v", path, err)
	}
}

func TestFetchThumbnail_CacheHit(t *testing.T) {
	s := &Session{cacheBase: t.TempDir(), apiBase: "http://unused"}
	thumbPath := s.thumbnailCachePath("link1", "rev1")
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("fake-jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)

	got, err := s.FetchThumbnail(context.Background(), "link1", "rev1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != thumbPath {
		t.Errorf("got %q, want %q", got, thumbPath)
	}
	// mtime must be touched on a cache hit.
	info, _ := os.Stat(thumbPath)
	if info.ModTime().Before(before) {
		t.Error("expected mtime to be updated on cache hit")
	}
}

func TestFetchThumbnail_NoThumbnailsInRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Code":1000,"Revision":{"Thumbnails":[]}}`))
	}))
	defer srv.Close()

	s := &Session{shareID: "share1", volumeID: "vol1", cacheBase: t.TempDir(), apiBase: srv.URL}
	s.setAuth("uid", "tok")

	got, err := s.FetchThumbnail(context.Background(), "link1", "rev1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path for no-thumbnail revision, got %q", got)
	}
}

func TestFetchThumbnail_FullFlow(t *testing.T) {
	const thumbContent = "fake-thumbnail-bytes"

	blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(thumbContent))
	}))
	defer blockSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"Code":1000,"Revision":{"Thumbnails":[{"ThumbnailID":"tid1","Type":0}]}}`))
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"Code":1000,"Thumbnails":[{"ThumbnailID":"tid1","BareURL":"` + blockSrv.URL + `","Token":"tok1"}]}`))
		}
	}))
	defer apiSrv.Close()

	s := &Session{shareID: "share1", volumeID: "vol1", cacheBase: t.TempDir(), apiBase: apiSrv.URL}
	s.setAuth("uid", "tok")

	got, err := s.FetchThumbnail(context.Background(), "link1", "rev1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty thumbnail path")
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("cannot read cached thumbnail: %v", err)
	}
	if string(data) != thumbContent {
		t.Errorf("cached content %q, want %q", data, thumbContent)
	}

	// Second call must be a cache hit — same path returned, no additional HTTP.
	got2, err := s.FetchThumbnail(context.Background(), "link1", "rev1")
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if got2 != got {
		t.Errorf("second call: got %q, want %q", got2, got)
	}
}
