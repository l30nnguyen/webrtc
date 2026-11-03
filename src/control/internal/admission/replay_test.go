package admission

import (
	"testing"
	"time"
)

func TestReplayCacheIsSingleUseAndForgetsExpired(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	c := &ReplayCache{Now: func() time.Time { return now }}
	if !c.Use("a", now.Unix()+60) {
		t.Fatal("first use refused")
	}
	if c.Use("a", now.Unix()+60) {
		t.Fatal("replay accepted")
	}
	if !c.Use("b", now.Unix()+60) {
		t.Fatal("independent ticket refused")
	}
	now = now.Add(2 * time.Minute) // both expired; GC window passed
	if !c.Use("a", now.Unix()+60) {
		t.Fatal("expired entry was not forgotten")
	}
}

func TestReplayCacheRefusesInsteadOfForgettingWhenFull(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	c := &ReplayCache{Now: func() time.Time { return now }, MaxSize: 2}
	if !c.Use("a", now.Unix()+60) || !c.Use("b", now.Unix()+60) {
		t.Fatal("setup uses refused")
	}
	if c.Use("c", now.Unix()+60) {
		t.Fatal("full cache accepted a new ticket; it must fail closed")
	}
	if c.Use("a", now.Unix()+60) {
		t.Fatal("full cache forgot an unexpired ticket")
	}
}
