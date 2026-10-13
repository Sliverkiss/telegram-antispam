package store

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-process Store for development and tests. Its data dies
// with the process — the startup warn log makes that explicit.
type Memory struct {
	mu       sync.RWMutex
	suspects map[int64]Suspect

	bioSigs  map[string]string // sha1 → original
	msgSigs  map[string]string
	tracked  map[trackKey][]int64
	punished int64
	closed   bool
}

type trackKey struct {
	chatID int64
	uid    int64
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{
		suspects: map[int64]Suspect{},
		bioSigs:  map[string]string{},
		msgSigs:  map[string]string{},
		tracked:  map[trackKey][]int64{},
	}
}

func (m *Memory) AddSuspect(_ context.Context, s Suspect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.JoinedAt.IsZero() {
		s.JoinedAt = time.Now()
	}
	m.suspects[s.UID] = s
	return nil
}

func (m *Memory) RemoveSuspect(_ context.Context, uid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.suspects, uid)
	return nil
}

func (m *Memory) IsSuspect(_ context.Context, uid int64) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.suspects[uid]
	return ok, nil
}

func (m *Memory) GetSuspect(_ context.Context, uid int64) (*Suspect, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.suspects[uid]
	if !ok {
		return nil, nil
	}
	cp := s
	return &cp, nil
}

func (m *Memory) SuspectCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.suspects)), nil
}

func (m *Memory) AddBioSignature(_ context.Context, sig, original string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bioSigs[sig] = original
	return nil
}

func (m *Memory) HasBioSignature(_ context.Context, sig string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.bioSigs[sig]
	return ok, nil
}

func (m *Memory) AddMessageSignature(_ context.Context, sig, original string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgSigs[sig] = original
	return nil
}

func (m *Memory) HasMessageSignature(_ context.Context, sig string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.msgSigs[sig]
	return ok, nil
}

func (m *Memory) BioSigCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.bioSigs)), nil
}

func (m *Memory) MsgSigCount(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return int64(len(m.msgSigs)), nil
}

func (m *Memory) BioSignatures(_ context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMap(m.bioSigs), nil
}

func (m *Memory) MessageSignatures(_ context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMap(m.msgSigs), nil
}

func (m *Memory) RemoveSignature(_ context.Context, kind, sig string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var src map[string]string
	if kind == "bio" {
		src = m.bioSigs
	} else {
		src = m.msgSigs
	}
	if _, ok := src[sig]; !ok {
		return false, nil
	}
	delete(src, sig)
	return true, nil
}

func (m *Memory) TrackMessage(_ context.Context, chatID, uid, messageID int64, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := trackKey{chatID: chatID, uid: uid}
	list := m.tracked[k]
	list = append(list, messageID)
	if max > 0 && len(list) > max {
		list = append([]int64(nil), list[len(list)-max:]...)
	}
	m.tracked[k] = list
	return nil
}

func (m *Memory) TrackedMessages(_ context.Context, chatID, uid int64) ([]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := m.tracked[trackKey{chatID: chatID, uid: uid}]
	out := make([]int64, len(list))
	copy(out, list)
	return out, nil
}

func (m *Memory) ClearTrackedMessages(_ context.Context, chatID, uid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tracked, trackKey{chatID: chatID, uid: uid})
	return nil
}

func (m *Memory) RecordPunishment(_ context.Context, p Punishment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.At.IsZero() {
		p.At = time.Now()
	}
	m.punished++
	return nil
}

func (m *Memory) Punished(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.punished, nil
}

func (m *Memory) Ping(_ context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil // memory store is always "up"
	}
	return nil
}

func (m *Memory) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
