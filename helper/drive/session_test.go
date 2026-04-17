package drive

import (
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// minimalSession builds a Session with only the fields needed for pathLinks /
// invalidation tests (no client, no poller).
func minimalSession() *Session {
	return &Session{
		rootID:    "root",
		nodeKRs:   make(map[string]*crypto.KeyRing),
		linkPaths: make(map[string]string),
		pathLinks: make(map[string]string),
		meta:      NewMetaCache(time.Hour),
	}
}

func TestSessionInvalidatePathClearsEntry(t *testing.T) {
	s := minimalSession()
	s.pathLinks["/a/b"] = "link-b"

	s.InvalidatePath("/a/b")

	if _, ok := s.pathLinks["/a/b"]; ok {
		t.Error("pathLinks[/a/b] should be removed after InvalidatePath")
	}
}

func TestSessionInvalidatePathClearsDescendants(t *testing.T) {
	s := minimalSession()
	s.pathLinks["/a"] = "link-a"
	s.pathLinks["/a/b"] = "link-b"
	s.pathLinks["/a/b/c"] = "link-c"
	s.pathLinks["/other"] = "link-other"

	s.InvalidatePath("/a")

	if _, ok := s.pathLinks["/a"]; ok {
		t.Error("pathLinks[/a] should be removed")
	}
	if _, ok := s.pathLinks["/a/b"]; ok {
		t.Error("pathLinks[/a/b] (descendant) should be removed")
	}
	if _, ok := s.pathLinks["/a/b/c"]; ok {
		t.Error("pathLinks[/a/b/c] (descendant) should be removed")
	}
	if _, ok := s.pathLinks["/other"]; ok == false {
		t.Error("pathLinks[/other] (unrelated) should be retained")
	}
}

func TestSessionInvalidatePathMissingNoPanic(t *testing.T) {
	s := minimalSession()
	s.InvalidatePath("/nonexistent") // must not panic
}

func TestSessionInvalidateLinkIDClearsPathLink(t *testing.T) {
	s := minimalSession()
	s.linkPaths["link-x"] = "/x/y"
	s.pathLinks["/x/y"] = "link-x"

	s.invalidateLinkID("link-x")

	if _, ok := s.pathLinks["/x/y"]; ok {
		t.Error("pathLinks[/x/y] should be removed after invalidateLinkID")
	}
}

func TestSessionInvalidateLinkIDUnknownNoPanic(t *testing.T) {
	s := minimalSession()
	s.invalidateLinkID("nonexistent") // must not panic
}

func TestSessionInvalidateAllClearsPathLinks(t *testing.T) {
	s := minimalSession()
	s.pathLinks["/a"] = "la"
	s.pathLinks["/b"] = "lb"

	s.invalidateAll()

	if len(s.pathLinks) != 0 {
		t.Errorf("pathLinks should be empty after invalidateAll, got %d entries", len(s.pathLinks))
	}
}

func TestSessionInvalidatePathAlsoInvalidatesMeta(t *testing.T) {
	s := minimalSession()
	s.meta.SetList("/a", nil, nil)
	s.pathLinks["/a"] = "la"

	s.InvalidatePath("/a")

	if _, _, ok := s.meta.GetList("/a"); ok {
		t.Error("meta list /a should also be cleared by InvalidatePath")
	}
}

// Verify that InvalidatePath does not remove entries that merely share a prefix
// without being under the same directory (e.g. /ab should not be removed when
// /a is invalidated).
func TestSessionInvalidatePathNoPrefixFalsePositive(t *testing.T) {
	s := minimalSession()
	s.pathLinks["/ab"] = "link-ab"
	s.pathLinks["/a-other"] = "link-other"

	s.InvalidatePath("/a")

	for p := range s.pathLinks {
		if strings.HasPrefix(p, "/a/") {
			t.Errorf("unexpected removal of %q", p)
		}
	}
	if _, ok := s.pathLinks["/ab"]; !ok {
		t.Error("/ab should not be removed when /a is invalidated")
	}
	if _, ok := s.pathLinks["/a-other"]; !ok {
		t.Error("/a-other should not be removed when /a is invalidated")
	}
}
