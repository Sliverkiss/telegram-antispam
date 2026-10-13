package store

import (
	"context"
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
