package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMemorySuspectLifecycle(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	sp := Suspect{UID: 1, Username: "u", FirstName: "f", ChatID: 9, Bio: "bio", JoinedAt: time.Now()}
	if err := s.AddSuspect(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsSuspect(ctx, 1); !ok {
		t.Fatal("expected suspect")
	}
	got, _ := s.GetSuspect(ctx, 1)
	if got == nil || got.Bio != "bio" || got.UID != 1 {
		t.Fatalf("GetSuspect = %+v", got)
	}
	if n, _ := s.SuspectCount(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
	if err := s.RemoveSuspect(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsSuspect(ctx, 1); ok {
		t.Fatal("suspect should be removed")
	}
}

func TestMemorySignatures(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	if err := s.AddBioSignature(ctx, "a", "orig-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessageSignature(ctx, "b", "orig-b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasBioSignature(ctx, "a"); !ok {
		t.Fatal("bio sig missing")
	}
	if ok, _ := s.HasMessageSignature(ctx, "b"); !ok {
		t.Fatal("msg sig missing")
	}
	if n, _ := s.BioSigCount(ctx); n != 1 {
		t.Fatalf("bio count = %d", n)
	}
	if n, _ := s.MsgSigCount(ctx); n != 1 {
		t.Fatalf("msg count = %d", n)
	}
	bios, _ := s.BioSignatures(ctx)
	if bios["a"] != "orig-a" {
		t.Fatalf("bios = %v", bios)
	}
	ok, err := s.RemoveSignature(ctx, "bio", "a")
	if err != nil || !ok {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	if ok, _ := s.HasBioSignature(ctx, "a"); ok {
		t.Fatal("sig should be removed")
	}
	ok, _ = s.RemoveSignature(ctx, "bio", "nope")
	if ok {
		t.Fatal("removing missing sig should report false")
	}
}

func TestMemoryTracking(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		if err := s.TrackMessage(ctx, 10, 7, i, 3); err != nil {
			t.Fatal(err)
		}
	}
	ids, _ := s.TrackedMessages(ctx, 10, 7)
	// Capped at 3, oldest dropped: expect [3 4 5].
	want := []int64{3, 4, 5}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	if err := s.ClearTrackedMessages(ctx, 10, 7); err != nil {
		t.Fatal(err)
	}
	ids, _ = s.TrackedMessages(ctx, 10, 7)
	if len(ids) != 0 {
		t.Fatalf("ids after clear = %v", ids)
	}
}

func TestMemoryPunishment(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	s.RecordPunishment(ctx, Punishment{UID: 1, Action: "ban"})
	s.RecordPunishment(ctx, Punishment{UID: 2, Action: "kick"})
	if n, _ := s.Punished(ctx); n != 2 {
		t.Fatalf("punished = %d", n)
	}
}

func TestMemoryPingClose(t *testing.T) {
	s := NewMemory()
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSweepTTLNotExpiredUntouched(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s := NewMemoryWithClock(func() time.Time { return now })
	ctx := context.Background()
	s.AddSuspect(ctx, Suspect{UID: 1})
	s.AddBioSignature(ctx, "a", "orig")
	now = base.Add(1 * time.Hour) // 1h of 72h elapsed — still fresh
	res, err := s.Sweep(ctx, SweepOptions{SuspectTTL: 72 * time.Hour, SignatureTTL: 72 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedSuspects != 0 || res.DroppedSignatures != 0 {
		t.Fatalf("nothing should be dropped: %+v", res)
	}
	if ok, _ := s.IsSuspect(ctx, 1); !ok {
		t.Fatal("suspect should survive")
	}
	if ok, _ := s.HasBioSignature(ctx, "a"); !ok {
		t.Fatal("signature should survive")
	}
}

func TestSweepTTLJustExpiredDeleted(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s := NewMemoryWithClock(func() time.Time { return now })
	ctx := context.Background()
	s.AddSuspect(ctx, Suspect{UID: 1})
	s.AddMessageSignature(ctx, "b", "orig")
	now = base.Add(72 * time.Hour) // exactly at the TTL boundary — expired
	res, _ := s.Sweep(ctx, SweepOptions{SuspectTTL: 72 * time.Hour, SignatureTTL: 72 * time.Hour})
	if res.DroppedSuspects != 1 || res.DroppedSignatures != 1 {
		t.Fatalf("both should expire at the boundary: %+v", res)
	}
	if ok, _ := s.IsSuspect(ctx, 1); ok {
		t.Fatal("suspect should be gone")
	}
	if ok, _ := s.HasMessageSignature(ctx, "b"); ok {
		t.Fatal("signature should be gone")
	}
}

func TestSweepSlidingExpiryRenews(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s := NewMemoryWithClock(func() time.Time { return now })
	s.SetSliding(true)
	ctx := context.Background()
	s.AddSuspect(ctx, Suspect{UID: 1})
	s.AddBioSignature(ctx, "a", "orig")

	now = base.Add(71 * time.Hour) // nearly expired
	s.IsSuspect(ctx, 1)            // touch suspect
	s.HasBioSignature(ctx, "a")    // touch signature

	now = base.Add(72 * time.Hour) // original TTL boundary; sliding should have pushed it out
	res, _ := s.Sweep(ctx, SweepOptions{SuspectTTL: 72 * time.Hour, SignatureTTL: 72 * time.Hour})
	if res.DroppedSuspects != 0 || res.DroppedSignatures != 0 {
		t.Fatalf("sliding should keep fresh hits alive: %+v", res)
	}
}

func TestMemoryCapacityEviction(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s := NewMemoryWithClock(func() time.Time { return now })
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		s.AddSuspect(ctx, Suspect{UID: i})
		now = now.Add(time.Minute) // distinct ts so oldest-first is deterministic
	}
	res, _ := s.Sweep(ctx, SweepOptions{MaxSuspects: 2})
	if res.EvictedSuspects != 3 {
		t.Fatalf("evicted = %d, want 3", res.EvictedSuspects)
	}
	// Oldest (uid 1..3) dropped; newest (4..5) kept.
	for _, uid := range []int64{1, 2, 3} {
		if ok, _ := s.IsSuspect(ctx, uid); ok {
			t.Fatalf("uid %d should be evicted", uid)
		}
	}
	for _, uid := range []int64{4, 5} {
		if ok, _ := s.IsSuspect(ctx, uid); !ok {
			t.Fatalf("uid %d should survive", uid)
		}
	}
}

func TestMemorySignatureCapacityEviction(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	s := NewMemoryWithClock(func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.AddBioSignature(ctx, fmt.Sprintf("bio%d", i), "o")
		now = now.Add(time.Minute)
	}
	for i := 0; i < 3; i++ {
		s.AddMessageSignature(ctx, fmt.Sprintf("msg%d", i), "o")
		now = now.Add(time.Minute)
	}
	res, _ := s.Sweep(ctx, SweepOptions{MaxSignatures: 3})
	if res.EvictedSignatures != 3 {
		t.Fatalf("evicted = %d, want 3", res.EvictedSignatures)
	}
	if n, _ := s.BioSigCount(ctx); n < 0 {
		t.Fatal("unreachable")
	}
	total, _ := s.BioSigCount(ctx)
	msgTotal, _ := s.MsgSigCount(ctx)
	if total+msgTotal != 3 {
		t.Fatalf("remaining = %d (bio) + %d (msg), want 3", total, msgTotal)
	}
}

func TestSweepConcurrentSafe(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			s.AddSuspect(ctx, Suspect{UID: int64(i % 50)})
			s.AddBioSignature(ctx, fmt.Sprintf("s%d", i%100), "o")
			s.Sweep(ctx, SweepOptions{
				SuspectTTL:    72 * time.Hour,
				SignatureTTL:  72 * time.Hour,
				MaxSuspects:   40,
				MaxSignatures: 80,
			})
		}
	}()
	for i := 0; i < 1000; i++ {
		s.IsSuspect(ctx, int64(i%50))
		s.HasBioSignature(ctx, fmt.Sprintf("s%d", i%100))
	}
	<-done
}
