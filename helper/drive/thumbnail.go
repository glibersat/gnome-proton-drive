package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// thumbnailRevisionResp is a minimal envelope for the revision endpoint that
// captures the Thumbnails array omitted by go-proton-api's Revision struct.
type thumbnailRevisionResp struct {
	Revision struct {
		Thumbnails []struct {
			ThumbnailID string `json:"ThumbnailID"`
			Type        int    `json:"Type"`
		} `json:"Thumbnails"`
	} `json:"Revision"`
}

type thumbnailBlockInfo struct {
	ThumbnailID string `json:"ThumbnailID"`
	BareURL     string `json:"BareURL"`
	Token       string `json:"Token"`
}

// FetchThumbnail downloads and caches the server-side thumbnail for a file
// revision. Returns the local filesystem path to the cached image on success,
// or "" if the revision has no thumbnail or the server has no data.
//
// The thumbnail is cached under ~/.cache/proton-drive/<account>/thumbnails/
// and LRU-eviction is NOT applied (thumbnails are small, typically < 100 KB).
// The cache key is (linkID, revisionID) so stale thumbnails are automatically
// superseded when a new revision is uploaded (a new revisionID appears).
func (s *Session) FetchThumbnail(ctx context.Context, linkID, revisionID string) (string, error) {
	if linkID == "" || revisionID == "" {
		return "", nil
	}

	thumbPath := s.thumbnailCachePath(linkID, revisionID)

	// Fast path: already cached on disk.
	// A zero-byte file is a negative-cache sentinel: the revision was checked
	// and has no thumbnail, so skip the network round-trip.
	if info, err := os.Stat(thumbPath); err == nil {
		if info.Size() == 0 {
			return "", nil
		}
		now := time.Now()
		_ = os.Chtimes(thumbPath, now, now)
		log.Printf("thumbnail cache hit  %s", linkID)
		return thumbPath, nil
	}

	uid, bearer := s.currentAuth()
	if uid == "" {
		return "", fmt.Errorf("thumbnail fetch: no active auth token")
	}

	ids, err := s.revisionThumbnailIDs(ctx, linkID, revisionID, uid, bearer)
	if err != nil {
		return "", fmt.Errorf("thumbnail fetch: revision IDs: %w", err)
	}
	if len(ids) == 0 {
		// Cache the negative result so subsequent calls are a fast disk hit.
		_ = writeThumbnailCache(thumbPath, []byte{})
		return "", nil
	}

	blocks, err := s.thumbnailBlocks(ctx, ids, uid, bearer)
	if err != nil {
		return "", fmt.Errorf("thumbnail fetch: blocks: %w", err)
	}
	if len(blocks) == 0 {
		return "", nil
	}

	// Download the first block (Type 0 = small standard thumbnail).
	data, err := downloadThumbnailBlock(ctx, blocks[0])
	if err != nil {
		return "", fmt.Errorf("thumbnail fetch: download: %w", err)
	}

	if err := writeThumbnailCache(thumbPath, data); err != nil {
		log.Printf("thumbnail cache write %s: %v", linkID, err)
		return "", nil
	}

	log.Printf("thumbnail cached    %s (%d bytes)", linkID, len(data))
	return thumbPath, nil
}

func (s *Session) thumbnailCachePath(linkID, revisionID string) string {
	base, _ := s.cacheBaseFor()
	return filepath.Join(base, "thumbnails", linkID, revisionID)
}

func writeThumbnailCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// revisionThumbnailIDs fetches the file revision and returns the thumbnail IDs.
// go-proton-api's Revision struct omits the Thumbnails field, so we make a raw
// authenticated HTTP call and decode only what we need.
func (s *Session) revisionThumbnailIDs(ctx context.Context, linkID, revisionID, uid, bearer string) ([]string, error) {
	url := fmt.Sprintf("%s/drive/shares/%s/files/%s/revisions/%s?FromBlockIndex=1&PageSize=1",
		s.apiBase, s.shareID, linkID, revisionID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setProtonHeaders(req, uid, bearer)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("revision endpoint: HTTP %d", resp.StatusCode)
	}

	var body thumbnailRevisionResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(body.Revision.Thumbnails))
	for _, t := range body.Revision.Thumbnails {
		ids = append(ids, t.ThumbnailID)
	}
	return ids, nil
}

// thumbnailBlocks calls POST /drive/volumes/{volumeID}/thumbnails to obtain
// BareURL and Token for each requested thumbnail ID.
func (s *Session) thumbnailBlocks(ctx context.Context, thumbnailIDs []string, uid, bearer string) ([]thumbnailBlockInfo, error) {
	body, _ := json.Marshal(struct {
		ThumbnailIDs []string `json:"ThumbnailIDs"`
	}{ThumbnailIDs: thumbnailIDs})

	url := fmt.Sprintf("%s/drive/volumes/%s/thumbnails", s.apiBase, s.volumeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setProtonHeaders(req, uid, bearer)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("thumbnails endpoint: HTTP %d", resp.StatusCode)
	}

	var result struct {
		Thumbnails []thumbnailBlockInfo `json:"Thumbnails"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Thumbnails, nil
}

// downloadThumbnailBlock downloads a thumbnail block from Proton block storage.
// Unlike file content blocks (which use a pm-storage-token header), thumbnail
// blocks use the token as a URL path suffix — matching the Windows Drive client.
func downloadThumbnailBlock(ctx context.Context, block thumbnailBlockInfo) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, block.BareURL+"/"+block.Token, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("block storage: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// setProtonHeaders applies the standard Proton API auth headers.
func setProtonHeaders(r *http.Request, uid, bearer string) {
	r.Header.Set("x-pm-uid", uid)
	r.Header.Set("Authorization", "Bearer "+bearer)
	r.Header.Set("x-pm-appversion", "windows-drive@1.13.1")
}
