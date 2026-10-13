package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"adbot/internal/logx"
)

// Key schema (all under "adbot:"):
//
//	adbot:suspect                  SET of suspect uids (watchlist)
//	adbot:suspect:<uid>            HASH metadata (uid, username, first_name,
//	                                joined_at, chat_id, bio) + TTL
//	adbot:biosig                   SET of bio signatures (sha1)
//	adbot:biosig:<sha1>            STRING original normalized bio text
//	adbot:msgsig                   SET of message signatures (sha1)
//	adbot:msgsig:<sha1>            STRING original normalized message text
//	adbot:track:<chatid>:<uid>     LIST of message ids (capped)
//	adbot:puni:<uid>               HASH punishment record
//	adbot:stats                    HASH counters (punished)
const (
	keySuspectSet = "adbot:suspect"
	keyBioSigSet  = "adbot:biosig"
	keyMsgSigSet  = "adbot:msgsig"
	keyStats      = "adbot:stats"
)

// RedisStore persists state into Redis over the hand-written RESP client.
type RedisStore struct {
	conn   *respConn
	logger *logx.Logger
	ttl    time.Duration // suspect watchlist TTL (0 = disabled)
}

// OpenRedis dials, authenticates, and pings; any failure is fatal (when a
// URL is configured, Redis is the source of truth).
func OpenRedis(rawURL string, ttl time.Duration, insecureTLS bool, logger *logx.Logger) (*RedisStore, error) {
	_, _, _, useTLS, err := parseRedisURL(rawURL)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{ServerName: serverName(rawURL)}
	if insecureTLS {
		tlsCfg.InsecureSkipVerify = true
	}
	conn, err := newRespConn(rawURL, tlsCfg)
	if err != nil {
		return nil, err
	}
	s := &RedisStore{conn: conn, logger: logger, ttl: ttl}
	if err := s.conn.doPing(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("redis store: %w", err)
	}
	if logger != nil {
		logger.Info("redis_connected", "host", serverName(rawURL), "tls", useTLS)
	}
	return s, nil
}

func serverName(rawURL string) string {
	u, err := parseURLHost(rawURL)
	if err != nil {
		return rawURL
	}
	return u
}

func parseURLHost(rawURL string) (string, error) {
	addr, _, _, _, err := parseRedisURL(rawURL)
	if err != nil {
		return "", err
	}
	host, _, err := splitHostPort(addr)
	return host, err
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}

// ---------------------------------------------------------------------------
// Suspects
// ---------------------------------------------------------------------------

func (s *RedisStore) AddSuspect(ctx context.Context, sp Suspect) error {
	uid := strconv.FormatInt(sp.UID, 10)
	if _, err := s.conn.do("SADD", keySuspectSet, uid); err != nil {
		return err
	}
	meta := map[string]string{
		"uid":        uid,
		"username":   sp.Username,
		"first_name": sp.FirstName,
		"joined_at":  sp.JoinedAt.UTC().Format(time.RFC3339),
		"chat_id":    strconv.FormatInt(sp.ChatID, 10),
		"bio":        sp.Bio,
	}
	if _, err := s.conn.do("HSET", keySuspectSet+":"+uid, "uid", meta["uid"],
		"username", meta["username"], "first_name", meta["first_name"],
		"joined_at", meta["joined_at"], "chat_id", meta["chat_id"], "bio", meta["bio"]); err != nil {
		return err
	}
	if s.ttl > 0 {
		if _, err := s.conn.do("EXPIRE", keySuspectSet+":"+uid, strconv.FormatInt(int64(s.ttl.Seconds()), 10)); err != nil {
			s.warn("expire suspect", err)
		}
	}
	return nil
}

func (s *RedisStore) RemoveSuspect(_ context.Context, uid int64) error {
	u := strconv.FormatInt(uid, 10)
	if _, err := s.conn.do("SREM", keySuspectSet, u); err != nil {
		return err
	}
	_, err := s.conn.do("DEL", keySuspectSet+":"+u)
	return err
}

func (s *RedisStore) IsSuspect(_ context.Context, uid int64) (bool, error) {
	reply, err := s.conn.do("SISMEMBER", keySuspectSet, strconv.FormatInt(uid, 10))
	if err != nil {
		return false, err
	}
	return replyInt(reply) == 1, nil
}

func (s *RedisStore) GetSuspect(_ context.Context, uid int64) (*Suspect, error) {
	reply, err := s.conn.do("HGETALL", keySuspectSet+":"+strconv.FormatInt(uid, 10))
	if err != nil {
		return nil, err
	}
	m, err := replyStringMap(reply)
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return mapSuspect(m), nil
}

func (s *RedisStore) SuspectCount(_ context.Context) (int64, error) {
	reply, err := s.conn.do("SCARD", keySuspectSet)
	if err != nil {
		return 0, err
	}
	return replyInt(reply), nil
}

// ---------------------------------------------------------------------------
// Signatures
// ---------------------------------------------------------------------------

func (s *RedisStore) AddBioSignature(_ context.Context, sig, original string) error {
	if _, err := s.conn.do("SADD", keyBioSigSet, sig); err != nil {
		return err
	}
	_, err := s.conn.do("SET", keyBioSigSet+":"+sig, original)
	return err
}

func (s *RedisStore) HasBioSignature(_ context.Context, sig string) (bool, error) {
	reply, err := s.conn.do("SISMEMBER", keyBioSigSet, sig)
	if err != nil {
		return false, err
	}
	return replyInt(reply) == 1, nil
}

func (s *RedisStore) AddMessageSignature(_ context.Context, sig, original string) error {
	if _, err := s.conn.do("SADD", keyMsgSigSet, sig); err != nil {
		return err
	}
	_, err := s.conn.do("SET", keyMsgSigSet+":"+sig, original)
	return err
}

func (s *RedisStore) HasMessageSignature(_ context.Context, sig string) (bool, error) {
	reply, err := s.conn.do("SISMEMBER", keyMsgSigSet, sig)
	if err != nil {
		return false, err
	}
	return replyInt(reply) == 1, nil
}

func (s *RedisStore) BioSigCount(_ context.Context) (int64, error) {
	reply, err := s.conn.do("SCARD", keyBioSigSet)
	if err != nil {
		return 0, err
	}
	return replyInt(reply), nil
}

func (s *RedisStore) MsgSigCount(_ context.Context) (int64, error) {
	reply, err := s.conn.do("SCARD", keyMsgSigSet)
	if err != nil {
		return 0, err
	}
	return replyInt(reply), nil
}

func (s *RedisStore) BioSignatures(_ context.Context) (map[string]string, error) {
	return s.signatureMap(keyBioSigSet)
}

func (s *RedisStore) MessageSignatures(_ context.Context) (map[string]string, error) {
	return s.signatureMap(keyMsgSigSet)
}

func (s *RedisStore) signatureMap(setKey string) (map[string]string, error) {
	reply, err := s.conn.do("SMEMBERS", setKey)
	if err != nil {
		return nil, err
	}
	members, err := replyStrings(reply)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(members))
	for _, sig := range members {
		orig, err := s.conn.do("GET", setKey+":"+sig)
		if err != nil {
			return nil, err
		}
		if s, ok := orig.(string); ok {
			out[sig] = s
		} else {
			out[sig] = ""
		}
	}
	return out, nil
}

func (s *RedisStore) RemoveSignature(_ context.Context, kind, sig string) (bool, error) {
	setKey := keyBioSigSet
	if kind == "msg" {
		setKey = keyMsgSigSet
	}
	reply, err := s.conn.do("SREM", setKey, sig)
	if err != nil {
		return false, err
	}
	removed := replyInt(reply) == 1
	_, err = s.conn.do("DEL", setKey+":"+sig)
	if removed {
		return true, err
	}
	return false, err
}

// ---------------------------------------------------------------------------
// Message tracking
// ---------------------------------------------------------------------------

func (s *RedisStore) trackKey(chatID, uid int64) string {
	return fmt.Sprintf("adbot:track:%d:%d", chatID, uid)
}

func (s *RedisStore) TrackMessage(_ context.Context, chatID, uid, messageID int64, max int) error {
	key := s.trackKey(chatID, uid)
	if _, err := s.conn.do("LPUSH", key, strconv.FormatInt(messageID, 10)); err != nil {
		return err
	}
	if max > 0 {
		if _, err := s.conn.do("LTRIM", key, "0", strconv.Itoa(max-1)); err != nil {
			return err
		}
	}
	return nil
}

func (s *RedisStore) TrackedMessages(_ context.Context, chatID, uid int64) ([]int64, error) {
	reply, err := s.conn.do("LRANGE", s.trackKey(chatID, uid), "0", "-1")
	if err != nil {
		return nil, err
	}
	// LRANGE returns newest-first (we LPUSH). Reverse to oldest-first for deletion order.
	strs, err := replyStrings(reply)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(strs))
	for i, v := range strs {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, err
		}
		ids[len(strs)-1-i] = n
	}
	return ids, nil
}

func (s *RedisStore) ClearTrackedMessages(_ context.Context, chatID, uid int64) error {
	_, err := s.conn.do("DEL", s.trackKey(chatID, uid))
	return err
}

// ---------------------------------------------------------------------------
// Punishments & stats
// ---------------------------------------------------------------------------

func (s *RedisStore) RecordPunishment(_ context.Context, p Punishment) error {
	// adbot:stats is a HASH: field "punished" carries the global counter.
	if _, err := s.conn.do("HINCRBY", keyStats, "punished", "1"); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.conn.do("SET", fmt.Sprintf("adbot:puni:%d", p.UID), string(b))
	return err
}

func (s *RedisStore) Punished(_ context.Context) (int64, error) {
	reply, err := s.conn.do("HGET", keyStats, "punished")
	if err != nil {
		return 0, err
	}
	if reply == nil {
		return 0, nil
	}
	n, err := strconv.ParseInt(replyString(reply), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func (s *RedisStore) Ping(_ context.Context) error { return s.conn.doPing() }

// Do runs one raw RESP command (exposed for the selftest probe).
func (s *RedisStore) Do(args ...string) (any, error) { return s.conn.do(args...) }

func (s *RedisStore) Close() error { return s.conn.Close() }

func (s *RedisStore) warn(event string, err error) {
	if s.logger != nil {
		s.logger.Warn(event, "err", err.Error())
	}
}

// ---------------------------------------------------------------------------
// reply helpers
// ---------------------------------------------------------------------------

func replyInt(reply any) int64 {
	switch v := reply.(type) {
	case int64:
		return v
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func replyString(reply any) string {
	if reply == nil {
		return ""
	}
	if s, ok := reply.(string); ok {
		return s
	}
	return fmt.Sprint(reply)
}

func replyStrings(reply any) ([]string, error) {
	arr, ok := reply.([]any)
	if !ok {
		return nil, fmt.Errorf("redis: expected array reply, got %T", reply)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if item == nil {
			continue
		}
		out = append(out, fmt.Sprint(item))
	}
	return out, nil
}

func replyStringMap(reply any) (map[string]string, error) {
	arr, ok := reply.([]any)
	if !ok {
		return nil, fmt.Errorf("redis: expected flat array for HGETALL, got %T", reply)
	}
	m := make(map[string]string, len(arr)/2)
	for i := 0; i+1 < len(arr); i += 2 {
		m[fmt.Sprint(arr[i])] = fmt.Sprint(arr[i+1])
	}
	return m, nil
}

func mapSuspect(m map[string]string) *Suspect {
	uid, _ := strconv.ParseInt(m["uid"], 10, 64)
	chatID, _ := strconv.ParseInt(m["chat_id"], 10, 64)
	joined, _ := time.Parse(time.RFC3339, m["joined_at"])
	return &Suspect{
		UID:       uid,
		Username:  m["username"],
		FirstName: m["first_name"],
		JoinedAt:  joined,
		ChatID:    chatID,
		Bio:       m["bio"],
	}
}
