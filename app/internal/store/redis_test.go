package store

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis is an in-test RESP2 server exercising the AUTH/TLS/command-codec
// path of the hand-written client. It makes the Redis store tests fully
// offline (no real redis-server, no network beyond loopback).
type fakeRedis struct {
	t        *testing.T
	ln       net.Listener
	password string

	mu       sync.Mutex
	data     map[string]string
	sets     map[string]map[string]bool
	hashes   map[string]map[string]string
	lists    map[string][]string
	counters map[string]int64
	conns    map[net.Conn]bool
	auts     []string // captured AUTH commands
}

func newFakeRedis(t *testing.T, password string) *fakeRedis {
	return &fakeRedis{
		t:        t,
		password: password,
		data:     map[string]string{},
		sets:     map[string]map[string]bool{},
		hashes:   map[string]map[string]string{},
		lists:    map[string][]string{},
		counters: map[string]int64{},
		conns:    map[net.Conn]bool{},
	}
}

func (f *fakeRedis) listen(t *testing.T, useTLS bool) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if useTLS {
		cert := selfSigned(t)
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	f.ln = ln
	go f.serve()
	scheme := "redis"
	if useTLS {
		scheme = "rediss"
	}
	return fmt.Sprintf("%s://:%s@%s", scheme, f.password, ln.Addr().String())
}

func (f *fakeRedis) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRedis) handle(conn net.Conn) {
	f.mu.Lock()
	f.conns[conn] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, conn)
		f.mu.Unlock()
		conn.Close()
	}()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	authed := f.password == ""
	for {
		cmd, err := readCommand(r)
		if err != nil {
			return
		}
		if len(cmd) == 0 {
			continue
		}
		verb := strings.ToUpper(cmd[0])
		if !authed && verb != "AUTH" && verb != "QUIT" {
			fmt.Fprint(w, "-NOAUTH Authentication required.\r\n")
			w.Flush()
			continue
		}
		switch verb {
		case "AUTH":
			f.mu.Lock()
			f.auts = append(f.auts, strings.Join(cmd[1:], ":"))
			f.mu.Unlock()
			pass := cmd[len(cmd)-1]
			if pass == f.password {
				authed = true
				fmt.Fprint(w, "+OK\r\n")
			} else {
				fmt.Fprint(w, "-WRONGPASS invalid username-password pair\r\n")
			}
		case "PING":
			fmt.Fprint(w, "+PONG\r\n")
		case "QUIT":
			fmt.Fprint(w, "+OK\r\n")
			w.Flush()
			return
		case "SET":
			f.mu.Lock()
			f.data[cmd[1]] = cmd[2]
			f.mu.Unlock()
			fmt.Fprint(w, "+OK\r\n")
		case "GET":
			f.mu.Lock()
			v, ok := f.data[cmd[1]]
			f.mu.Unlock()
			if !ok {
				fmt.Fprint(w, "$-1\r\n")
			} else {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(v), v)
			}
		case "DEL":
			f.mu.Lock()
			n := int64(0)
			for _, k := range cmd[1:] {
				if _, ok := f.data[k]; ok {
					delete(f.data, k)
					n++
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", n)
		case "EXISTS":
			f.mu.Lock()
			_, ok := f.data[cmd[1]]
			f.mu.Unlock()
			if ok {
				fmt.Fprint(w, ":1\r\n")
			} else {
				fmt.Fprint(w, ":0\r\n")
			}
		case "EXPIRE":
			fmt.Fprint(w, ":1\r\n")
		case "SADD":
			f.mu.Lock()
			if f.sets[cmd[1]] == nil {
				f.sets[cmd[1]] = map[string]bool{}
			}
			added := int64(0)
			for _, m := range cmd[2:] {
				if !f.sets[cmd[1]][m] {
					f.sets[cmd[1]][m] = true
					added++
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", added)
		case "SREM":
			f.mu.Lock()
			removed := int64(0)
			if f.sets[cmd[1]] != nil {
				for _, m := range cmd[2:] {
					if f.sets[cmd[1]][m] {
						delete(f.sets[cmd[1]], m)
						removed++
					}
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", removed)
		case "SISMEMBER":
			f.mu.Lock()
			_, ok := f.sets[cmd[1]][cmd[2]]
			f.mu.Unlock()
			if ok {
				fmt.Fprint(w, ":1\r\n")
			} else {
				fmt.Fprint(w, ":0\r\n")
			}
		case "SMEMBERS":
			f.mu.Lock()
			members := f.sets[cmd[1]]
			fmt.Fprintf(w, "*%d\r\n", len(members))
			for m := range members {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(m), m)
			}
			f.mu.Unlock()
		case "SCARD":
			f.mu.Lock()
			n := len(f.sets[cmd[1]])
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", n)
		case "HSET":
			f.mu.Lock()
			if f.hashes[cmd[1]] == nil {
				f.hashes[cmd[1]] = map[string]string{}
			}
			for i := 2; i+1 < len(cmd); i += 2 {
				f.hashes[cmd[1]][cmd[i]] = cmd[i+1]
			}
			f.mu.Unlock()
			fmt.Fprint(w, ":1\r\n")
		case "HGET":
			f.mu.Lock()
			v, ok := f.hashes[cmd[1]][cmd[2]]
			f.mu.Unlock()
			if !ok {
				fmt.Fprint(w, "$-1\r\n")
			} else {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(v), v)
			}
		case "HGETALL":
			f.mu.Lock()
			h := f.hashes[cmd[1]]
			fmt.Fprintf(w, "*%d\r\n", len(h)*2)
			for k, v := range h {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(k), k)
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(v), v)
			}
			f.mu.Unlock()
		case "HINCRBY":
			f.mu.Lock()
			if f.hashes[cmd[1]] == nil {
				f.hashes[cmd[1]] = map[string]string{}
			}
			cur, _ := strconv.ParseInt(f.hashes[cmd[1]][cmd[2]], 10, 64)
			delta, _ := strconv.ParseInt(cmd[3], 10, 64)
			cur += delta
			f.hashes[cmd[1]][cmd[2]] = strconv.FormatInt(cur, 10)
			v := cur
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", v)
		case "LPUSH":
			f.mu.Lock()
			f.lists[cmd[1]] = append(append([]string{}, cmd[2:]...), f.lists[cmd[1]]...)
			n := len(f.lists[cmd[1]])
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", n)
		case "LRANGE":
			f.mu.Lock()
			l := f.lists[cmd[1]]
			f.mu.Unlock()
			start, _ := strconv.Atoi(cmd[2])
			stop, _ := strconv.Atoi(cmd[3])
			if start < 0 {
				start = 0
			}
			if stop < 0 || stop >= len(l) {
				stop = len(l) - 1
			}
			var out []string
			if start <= stop {
				out = l[start : stop+1]
			}
			fmt.Fprintf(w, "*%d\r\n", len(out))
			for _, v := range out {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(v), v)
			}
		case "LTRIM":
			f.mu.Lock()
			l := f.lists[cmd[1]]
			start, _ := strconv.Atoi(cmd[2])
			stop, _ := strconv.Atoi(cmd[3])
			if start < 0 {
				start = 0
			}
			if stop >= len(l) || stop < 0 {
				stop = len(l) - 1
			}
			if start > stop || start >= len(l) {
				f.lists[cmd[1]] = nil
			} else {
				f.lists[cmd[1]] = l[start : stop+1]
			}
			f.mu.Unlock()
			fmt.Fprint(w, "+OK\r\n")
		case "INCR":
			f.mu.Lock()
			cur, _ := strconv.ParseInt(f.data[cmd[1]], 10, 64)
			cur++
			f.counters[cmd[1]] = cur
			f.data[cmd[1]] = strconv.FormatInt(cur, 10)
			v := cur
			f.mu.Unlock()
			fmt.Fprintf(w, ":%d\r\n", v)
		case "SCAN", "KEYS":
			f.mu.Lock()
			var keys []string
			for k := range f.data {
				keys = append(keys, k)
			}
			f.mu.Unlock()
			// SCAN returns [cursor, [keys...]]; simplify to KEYS-style array.
			fmt.Fprintf(w, "*%d\r\n", len(keys))
			for _, k := range keys {
				fmt.Fprintf(w, "$%d\r\n%s\r\n", len(k), k)
			}
		default:
			fmt.Fprintf(w, "-ERR unknown command '%s'\r\n", verb)
		}
		w.Flush()
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("bad command line %q", line)
	}
	var n int
	if _, err := fmt.Sscanf(line, "*%d", &n); err != nil {
		return nil, err
	}
	cmd := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimSpace(hdr)
		var l int
		if _, err := fmt.Sscanf(hdr, "$%d", &l); err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := readFull(r, buf); err != nil {
			return nil, err
		}
		cmd = append(cmd, string(buf[:l]))
	}
	return cmd, nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func selfSigned(t *testing.T) tls.Certificate {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// ---------------------------------------------------------------------------
// RedisStore tests (TLS + AUTH paths, command encoding, roundtrips).
// ---------------------------------------------------------------------------

func openTestRedis(t *testing.T, useTLS bool) (*RedisStore, *fakeRedis) {
	t.Helper()
	fake := newFakeRedis(t, "sekrit")
	url := fake.listen(t, useTLS)
	store, err := OpenRedis(url, 0, !useTLS || trueAux(t), nil)
	if err != nil {
		t.Fatalf("OpenRedis: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, fake
}

// trueAux exists so openTestRedis also exercises the insecure-TLS flag when
// the self-signed cert would otherwise fail verification. It always returns
// true: a self-signed cert is inherently untrusted, so skip verification.
func trueAux(t *testing.T) bool { return true }

func TestRedisTLSRoundtrip(t *testing.T) {
	store, fake := openTestRedis(t, true)
	ctx := context.Background()

	sp := Suspect{UID: 42, Username: "user42", FirstName: "U", ChatID: 7, Bio: "办证 加V", JoinedAt: time.Now().UTC().Truncate(time.Second)}
	if err := store.AddSuspect(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.IsSuspect(ctx, 42); !ok {
		t.Fatal("suspect not found")
	}
	got, err := store.GetSuspect(ctx, 42)
	if err != nil || got == nil {
		t.Fatalf("GetSuspect = %v, %v", got, err)
	}
	if got.Bio != "办证 加V" || got.Username != "user42" {
		t.Fatalf("suspect roundtrip = %+v", got)
	}

	// Check AUTH was actually sent over the wire.
	fake.mu.Lock()
	auts := append([]string{}, fake.auts...)
	fake.mu.Unlock()
	if len(auts) == 0 || auts[0] != "sekrit" {
		t.Fatalf("AUTH commands = %v, want [sekrit]", auts)
	}
}

func TestRedisPlaintextRoundtrip(t *testing.T) {
	store, _ := openTestRedis(t, false)
	ctx := context.Background()
	if err := store.AddMessageSignature(ctx, "deadbeef", "办证 加V"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.HasMessageSignature(ctx, "deadbeef"); !ok {
		t.Fatal("msg sig not found")
	}
	sigs, _ := store.MessageSignatures(ctx)
	if sigs["deadbeef"] != "办证 加V" {
		t.Fatalf("sigs = %v", sigs)
	}
	if n, _ := store.MsgSigCount(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
}

func TestRedisSignaturesRemove(t *testing.T) {
	store, _ := openTestRedis(t, true)
	ctx := context.Background()
	store.AddBioSignature(ctx, "sig1", "orig")
	ok, err := store.RemoveSignature(ctx, "bio", "sig1")
	if err != nil || !ok {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	if ok, _ := store.HasBioSignature(ctx, "sig1"); ok {
		t.Fatal("should be removed")
	}
}

func TestRedisTracking(t *testing.T) {
	store, _ := openTestRedis(t, true)
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		store.TrackMessage(ctx, 10, 7, i, 3)
	}
	ids, err := store.TrackedMessages(ctx, 10, 7)
	if err != nil {
		t.Fatal(err)
	}
	// LRANGE returns newest-first; TrackedMessages reverses to oldest-first.
	want := []int64{3, 4, 5}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestRedisPunishment(t *testing.T) {
	store, _ := openTestRedis(t, true)
	ctx := context.Background()
	store.RecordPunishment(ctx, Punishment{UID: 1, Action: "ban"})
	store.RecordPunishment(ctx, Punishment{UID: 2, Action: "kick"})
	if n, _ := store.Punished(ctx); n != 2 {
		t.Fatalf("punished = %d", n)
	}
}

func TestRedisOpenFailsOnBadAuth(t *testing.T) {
	fake := newFakeRedis(t, "right")
	url := fake.listen(t, true)
	bad := strings.Replace(url, ":right@", ":wrong@", 1)
	if _, err := OpenRedis(bad, 0, true, nil); err == nil {
		t.Fatal("bad auth should fail OpenRedis")
	}
}

func TestRedisOpenFailsWhenDown(t *testing.T) {
	if _, err := OpenRedis("rediss://:pw@127.0.0.1:1", 0, true, nil); err == nil {
		t.Fatal("dead port should fail")
	}
}

func TestParseRedisURL(t *testing.T) {
	addr, pass, user, tlsOn, err := parseRedisURL("rediss://default:secret@host:6380")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "host:6380" || pass != "secret" || user != "default" || !tlsOn {
		t.Fatalf("addr=%q pass=%q user=%q tls=%v", addr, pass, user, tlsOn)
	}
	if _, _, _, _, err := parseRedisURL("http://h"); err == nil {
		t.Fatal("bad scheme should error")
	}
}
