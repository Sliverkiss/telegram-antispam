// Package store persists the antispam bot's state: suspect watchlist, bio and
// message signature libraries, tracked per-user message ids, punishment
// records, and aggregated counters. Two implementations share one interface:
// a Memory store (dev/test default) and a Redis store (hand-written RESP
// client, stdlib only).
package store

import (
	"context"
	"errors"
	"time"
)

// Suspect is one watchlist entry: a user who joined a group and needs a bio
// check (and possibly ongoing message scrutiny).
type Suspect struct {
	UID       int64     `json:"uid"`
	Username  string    `json:"username"`
	FirstName string    `json:"first_name"`
	JoinedAt  time.Time `json:"joined_at"`
	ChatID    int64     `json:"chat_id"`
	Bio       string    `json:"bio"`
}

// Punishment is one recorded enforcement action.
type Punishment struct {
	UID      int64     `json:"uid"`
	ChatID   int64     `json:"chat_id"`
	Action   string    `json:"action"` // ban or kick
	Reason   string    `json:"reason"`
	Category string    `json:"category"`
	At       time.Time `json:"at"`
}

// Stats is a small counter aggregate for /statusz.
type Stats struct {
	Punished int64
}

// ErrNotConfigured marks a missing BOT_REDIS_URL (memory fallback expected).
var ErrNotConfigured = errors.New("store: no redis url configured")

// Store is the persistence contract. All methods are safe for concurrent
// use by multiple goroutines (the bot's single poll loop and the HTTP admin
// handlers).
type Store interface {
	// Suspects -----------------------------------------------------------------
	// AddSuspect upserts a watchlist entry and its metadata hash.
	AddSuspect(ctx context.Context, s Suspect) error
	// RemoveSuspect drops a suspect from the watchlist.
	RemoveSuspect(ctx context.Context, uid int64) error
	// IsSuspect reports whether uid is on the watchlist.
	IsSuspect(ctx context.Context, uid int64) (bool, error)
	// GetSuspect returns the stored metadata for uid (nil if absent).
	GetSuspect(ctx context.Context, uid int64) (*Suspect, error)
	// SuspectCount returns the watchlist size.
	SuspectCount(ctx context.Context) (int64, error)

	// Signatures ---------------------------------------------------------------
	// AddBioSignature records a normalized-bio sha1 signature.
	AddBioSignature(ctx context.Context, sig, original string) error
	// HasBioSignature reports whether the bio signature is known.
	HasBioSignature(ctx context.Context, sig string) (bool, error)
	// AddMessageSignature records a normalized-message sha1 signature.
	AddMessageSignature(ctx context.Context, sig, original string) error
	// HasMessageSignature reports whether the message signature is known.
	HasMessageSignature(ctx context.Context, sig string) (bool, error)
	// BioSigCount / MsgSigCount return library sizes.
	BioSigCount(ctx context.Context) (int64, error)
	MsgSigCount(ctx context.Context) (int64, error)
	// BioSignatures returns every (sig, original) bio signature.
	BioSignatures(ctx context.Context) (map[string]string, error)
	// MessageSignatures returns every (sig, original) message signature.
	MessageSignatures(ctx context.Context) (map[string]string, error)
	// RemoveSignature deletes a signature from one or both libraries, return
	// true if anything was removed. kind is "bio" or "msg".
	RemoveSignature(ctx context.Context, kind, sig string) (bool, error)

	// Message tracking ----------------------------------------------------------
	// TrackMessage appends a message id to the user's per-chat list, capping
	// at max entries (oldest dropped).
	TrackMessage(ctx context.Context, chatID, uid, messageID int64, max int) error
	// TrackedMessages returns the ordered list of message ids.
	TrackedMessages(ctx context.Context, chatID, uid int64) ([]int64, error)
	// ClearTrackedMessages removes the tracking list.
	ClearTrackedMessages(ctx context.Context, chatID, uid int64) error

	// Punishments & stats -------------------------------------------------------
	// RecordPunishment stores one punishment and bumps counters.
	RecordPunishment(ctx context.Context, p Punishment) error
	// Punished increments the global punishment counter.
	Punished(ctx context.Context) (int64, error)

	// Health ---------------------------------------------------------------------
	// Ping verifies the store is reachable.
	Ping(ctx context.Context) error

	// Close releases resources (no-op for memory).
	Close() error
}
