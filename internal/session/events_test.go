package session

import "testing"

func TestBroker(t *testing.T) {
	b := NewBroker()
	ch, unsub := b.Subscribe()
	b.Publish(Event{Type: "x", SessionID: "1"})
	if e := <-ch; e.Type != "x" {
		t.Fatal("did not receive event")
	}
	unsub()
	b.Publish(Event{Type: "y"})
	select {
	case e := <-ch:
		t.Fatalf("received after unsubscribe: %+v", e)
	default:
	}
	for i := 0; i < 100; i++ {
		b.Publish(Event{Type: "flood"})
	}
}
