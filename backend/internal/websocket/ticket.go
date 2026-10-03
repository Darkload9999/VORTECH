package websocket

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
)

// maxTicketsPerUser bounds outstanding (unredeemed) tickets per user.
const maxTicketsPerUser = 10

var (
	errInvalidTicket = errors.New("invalid or expired ticket")
	errTooManyTicket = errors.New("too many outstanding tickets")
)

// TicketStore issues single-use, short-lived WebSocket tickets.
//
// Browsers cannot set an Authorization header on a WebSocket handshake and
// access tokens must not travel in URLs (they end up in logs and history).
// The client therefore exchanges its bearer token for a 256-bit random
// ticket over an authenticated REST call and presents it once in the
// handshake. The ticket snapshots the authenticated principal.
//
// The store is process-local; it moves to Valkey (GETDEL) when the API
// runs as several replicas.
type TicketStore struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	tickets map[string]ticket
	perUser map[uuid.UUID]int
}

type ticket struct {
	principal auth.Principal
	expires   time.Time
}

// NewTicketStore returns a store issuing tickets valid for ttl.
func NewTicketStore(ttl time.Duration) *TicketStore {
	return &TicketStore{ttl: ttl, now: time.Now, tickets: map[string]ticket{}, perUser: map[uuid.UUID]int{}}
}

// Issue creates a ticket for p.
func (s *TicketStore) Issue(p *auth.Principal) (string, time.Time, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if s.perUser[p.UserID] >= maxTicketsPerUser {
		return "", time.Time{}, errTooManyTicket
	}
	exp := s.now().Add(s.ttl)
	s.tickets[tok] = ticket{principal: *p, expires: exp}
	s.perUser[p.UserID]++
	return tok, exp, nil
}

// Redeem consumes a ticket. A ticket works at most once.
func (s *TicketStore) Redeem(tok string) (auth.Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[tok]
	if !ok {
		return auth.Principal{}, errInvalidTicket
	}
	s.removeLocked(tok, t)
	if s.now().After(t.expires) {
		return auth.Principal{}, errInvalidTicket
	}
	return t.principal, nil
}

func (s *TicketStore) removeLocked(tok string, t ticket) {
	delete(s.tickets, tok)
	if s.perUser[t.principal.UserID]--; s.perUser[t.principal.UserID] <= 0 {
		delete(s.perUser, t.principal.UserID)
	}
}

func (s *TicketStore) sweepLocked() {
	now := s.now()
	for tok, t := range s.tickets {
		if now.After(t.expires) {
			s.removeLocked(tok, t)
		}
	}
}
