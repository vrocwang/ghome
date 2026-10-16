package store

import (
	"testing"
	"time"
)

const goodDoc = `
groups:
  - name: 开发
    links:
      - name: Gitea
        url: https://git.example.com
      - name: Wiki
        url: https://wiki.example.com
`

func TestBadDocumentKeepsThePreviousOne(t *testing.T) {
	st := New()
	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if err := st.Load([]byte("groups: ["), "rev-2", "test"); err == nil {
		t.Fatal("Load() accepted invalid YAML")
	}

	payload, ok := st.Get()
	if !ok {
		t.Fatal("Get() reports nothing loaded")
	}
	if payload.Meta.Revision != "rev-1" {
		t.Errorf("Revision = %q, want rev-1: a broken push must not go live", payload.Meta.Revision)
	}
	if st.LastError() == nil {
		t.Error("LastError() = nil, want the parse failure")
	}
}

func TestLoadClearsThePreviousError(t *testing.T) {
	st := New()
	if err := st.Load([]byte("groups: ["), "rev-1", "test"); err == nil {
		t.Fatal("Load() accepted invalid YAML")
	}
	if st.LastError() == nil {
		t.Fatal("LastError() = nil after a failure")
	}
	if err := st.Load([]byte(goodDoc), "rev-2", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if st.LastError() != nil {
		t.Errorf("LastError() = %v, want nil after a successful load", st.LastError())
	}
}

func TestMetaCountsGroupsAndLinks(t *testing.T) {
	st := New()
	if err := st.Load([]byte(goodDoc), "rev-1", "gitea:o/r/nav.yaml@main"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	payload, _ := st.Get()
	if payload.Meta.GroupCount != 1 || payload.Meta.LinkCount != 2 {
		t.Errorf("Meta = %+v, want 1 group and 2 links", payload.Meta)
	}
	if payload.Meta.Source != "gitea:o/r/nav.yaml@main" {
		t.Errorf("Source = %q", payload.Meta.Source)
	}
	if payload.Meta.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

func TestSubscribeReceivesUpdates(t *testing.T) {
	st := New()
	updates, cancel := st.Subscribe()
	defer cancel()

	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified of a new document")
	}
}

func TestUnsubscribeClosesTheChannelAndIsSafeToRepeat(t *testing.T) {
	st := New()
	updates, cancel := st.Subscribe()
	cancel()
	cancel() // must not panic

	if _, open := <-updates; open {
		t.Error("channel is still open after cancel")
	}
	// A load after cancelling must not send on the closed channel.
	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if n := st.SubscriberCount(); n != 0 {
		t.Errorf("SubscriberCount() = %d, want 0", n)
	}
}

func TestSlowSubscriberNeverBlocks(t *testing.T) {
	st := New()
	// Deliberately never drained: notifications must be dropped, not block.
	_, cancel := st.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if err := st.Load([]byte(goodDoc), "rev", "test"); err != nil {
				t.Errorf("Load() error = %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Load() blocked on a subscriber that never reads")
	}
}

// A reconnecting stream asks for this counter to find out whether it missed
// anything. It has to move for every kind of notification, not just a new
// document: a probe round publishes nothing at all, and a stylesheet-only push
// leaves the document revision alone.
func TestChangesCountsEveryNotification(t *testing.T) {
	st := New()
	if got := st.Changes(); got != 0 {
		t.Errorf("Changes() = %d on a fresh store, want 0", got)
	}

	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	afterLoad := st.Changes()
	if afterLoad == 0 {
		t.Fatal("a publish did not move the counter, so a reconnect could never catch up")
	}

	st.Notify() // what the prober does when a result changes
	if got := st.Changes(); got != afterLoad+1 {
		t.Errorf("Changes() = %d after Notify, want %d", got, afterLoad+1)
	}

	// Two publishes of the same revision still count: the counter says how many
	// times subscribers were woken, not whether the bytes differ.
	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := st.Changes(); got != afterLoad+2 {
		t.Errorf("Changes() = %d after a second publish, want %d", got, afterLoad+2)
	}
}
