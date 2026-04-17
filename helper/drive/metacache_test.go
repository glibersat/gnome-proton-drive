package drive

import (
	"testing"
	"time"

	proton "github.com/ProtonMail/go-proton-api"
)

func TestMetaCacheStatFreshHit(t *testing.T) {
	c := NewMetaCache(time.Hour)
	link := proton.Link{LinkID: "abc"}
	c.SetStat("/foo", link, nil)

	got, _, ok := c.GetStat("/foo")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.LinkID != "abc" {
		t.Fatalf("got %q, want %q", got.LinkID, "abc")
	}
}

func TestMetaCacheStatMiss(t *testing.T) {
	c := NewMetaCache(time.Hour)
	_, _, ok := c.GetStat("/missing")
	if ok {
		t.Fatal("expected cache miss")
	}
}

func TestMetaCacheStatExpiry(t *testing.T) {
	c := NewMetaCache(time.Millisecond)
	c.SetStat("/foo", proton.Link{LinkID: "abc"}, nil)
	time.Sleep(5 * time.Millisecond)

	_, _, ok := c.GetStat("/foo")
	if ok {
		t.Fatal("expected expired entry to miss")
	}
}

func TestMetaCacheStatStaleAfterExpiry(t *testing.T) {
	c := NewMetaCache(time.Millisecond)
	c.SetStat("/foo", proton.Link{LinkID: "abc"}, nil)
	time.Sleep(5 * time.Millisecond)

	got, _, ok := c.GetStatStale("/foo")
	if !ok {
		t.Fatal("expected stale hit")
	}
	if got.LinkID != "abc" {
		t.Fatalf("got %q, want %q", got.LinkID, "abc")
	}
}

func TestMetaCacheListFreshHit(t *testing.T) {
	c := NewMetaCache(time.Hour)
	links := []proton.Link{{LinkID: "x"}, {LinkID: "y"}}
	c.SetList("/dir", links, nil)

	got, _, ok := c.GetList("/dir")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

func TestMetaCacheListExpiry(t *testing.T) {
	c := NewMetaCache(time.Millisecond)
	c.SetList("/dir", []proton.Link{{LinkID: "x"}}, nil)
	time.Sleep(5 * time.Millisecond)

	_, _, ok := c.GetList("/dir")
	if ok {
		t.Fatal("expected expired entry to miss")
	}
}

func TestMetaCacheListStale(t *testing.T) {
	c := NewMetaCache(time.Millisecond)
	c.SetList("/dir", []proton.Link{{LinkID: "x"}}, nil)
	time.Sleep(5 * time.Millisecond)

	got, _, ok := c.GetListStale("/dir")
	if !ok {
		t.Fatal("expected stale hit")
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
}

func TestMetaCacheInvalidatePath(t *testing.T) {
	c := NewMetaCache(time.Hour)
	c.SetStat("/a/b", proton.Link{LinkID: "b"}, nil)
	c.SetList("/a/b", []proton.Link{{LinkID: "c"}}, nil)
	c.SetList("/a", []proton.Link{{LinkID: "b"}}, nil)

	c.InvalidatePath("/a/b")

	if _, _, ok := c.GetStat("/a/b"); ok {
		t.Error("stat /a/b should be gone")
	}
	if _, _, ok := c.GetList("/a/b"); ok {
		t.Error("list /a/b should be gone")
	}
	if _, _, ok := c.GetList("/a"); ok {
		t.Error("list /a (parent) should be gone")
	}
}

func TestMetaCacheInvalidatePathMissing(t *testing.T) {
	c := NewMetaCache(time.Hour)
	c.InvalidatePath("/nonexistent") // must not panic
}

func TestMetaCacheConcurrentAccess(t *testing.T) {
	c := NewMetaCache(time.Hour)
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			c.SetStat("/p", proton.Link{}, nil)
			c.GetStat("/p")
			c.InvalidatePath("/p")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
