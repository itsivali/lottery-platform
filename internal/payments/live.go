package payments

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Live announces payment changes to the players they belong to. The database announces every visible change on the
// payment_events channel when its transaction commits (migration 00018); Live keeps one connection listening and
// hands each change to that player's subscribers. An event only names a payment: subscribers read it back with Get,
// so what they show is always what the database holds.
//
// Delivery is at most once. Whenever announcements may have been lost (the listening connection dropped and was
// re-established), every subscriber gets a resync event and should read its snapshot again (Recent). A subscriber
// that falls behind is closed rather than allowed to hold the others back; it reconnects and resyncs.
type Live struct {
	connect   func(ctx context.Context) (*pgx.Conn, error)
	perUser   int
	total     int
	bufSize   int
	ready     chan struct{}
	readyOnce sync.Once

	mu     sync.Mutex
	closed bool
	subs   map[string]map[*Subscription]struct{}
	count  int
}

// Event is one change: a payment that changed, or a resync after announcements may have been lost.
type Event struct {
	PaymentID string
	Resync    bool
}

// Subscription receives one player's events until it is closed.
type Subscription struct {
	C      <-chan Event
	c      chan Event
	userID string
	live   *Live
	once   sync.Once
}

// Errors.
var (
	ErrTooManyStreams = errors.New("payments: too many live streams")
	ErrLiveClosed     = errors.New("payments: live updates are shutting down")
)

const liveChannel = "payment_events"

// LiveConfig bounds the subscribers; zero values take the defaults.
type LiveConfig struct {
	PerUser int // streams one player may hold open (default 3)
	Total   int // streams one api instance holds open (default 2000)
	Buffer  int // events a subscriber may fall behind before it is closed (default 32)
}

// NewLive returns a Live that listens through connections from connect. Run must be started for events to flow.
func NewLive(connect func(ctx context.Context) (*pgx.Conn, error), cfg LiveConfig) *Live {
	if cfg.PerUser <= 0 {
		cfg.PerUser = 3
	}
	if cfg.Total <= 0 {
		cfg.Total = 2000
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 32
	}
	return &Live{connect: connect, perUser: cfg.PerUser, total: cfg.Total, bufSize: cfg.Buffer,
		ready: make(chan struct{}), subs: map[string]map[*Subscription]struct{}{}}
}

// Ready is closed once the first LISTEN is in place (tests wait on it).
func (l *Live) Ready() <-chan struct{} { return l.ready }

// Subscribe registers a subscriber for the player's events. Close it when done.
func (l *Live) Subscribe(userID string) (*Subscription, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrLiveClosed
	}
	if l.count >= l.total || len(l.subs[userID]) >= l.perUser {
		return nil, ErrTooManyStreams
	}
	c := make(chan Event, l.bufSize)
	s := &Subscription{C: c, c: c, userID: userID, live: l}
	if l.subs[userID] == nil {
		l.subs[userID] = map[*Subscription]struct{}{}
	}
	l.subs[userID][s] = struct{}{}
	l.count++
	return s, nil
}

// Close unregisters the subscription and closes its channel. It is safe to call more than once.
func (s *Subscription) Close() {
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	s.closeLocked()
}

func (s *Subscription) closeLocked() {
	s.once.Do(func() {
		l := s.live
		delete(l.subs[s.userID], s)
		if len(l.subs[s.userID]) == 0 {
			delete(l.subs, s.userID)
		}
		l.count--
		close(s.c)
	})
}

// Close ends every subscription and refuses new ones (at server shutdown, so open streams end at once).
func (l *Live) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for _, set := range l.subs {
		for s := range set {
			s.closeLocked()
		}
	}
}

// Run listens until ctx ends, reconnecting with backoff when the connection fails.
func (l *Live) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		listening, err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if listening {
			backoff = time.Second
		}
		slog.WarnContext(ctx, "payment events listener stopped; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// listen holds one connection on LISTEN and dispatches notifications until it fails. It reports whether LISTEN
// was established, so a connection that worked for a while restarts the backoff.
func (l *Live) listen(ctx context.Context) (bool, error) {
	conn, err := l.connect(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+liveChannel); err != nil {
		return false, err
	}
	// Anything committed while no connection listened was not heard: everyone reads their snapshot again.
	l.broadcast(Event{Resync: true})
	l.readyOnce.Do(func() { close(l.ready) })
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		user, payment, ok := strings.Cut(n.Payload, ":")
		if !ok {
			continue
		}
		l.send(user, Event{PaymentID: payment})
	}
}

func (l *Live) send(userID string, e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for s := range l.subs[userID] {
		l.deliverLocked(s, e)
	}
}

func (l *Live) broadcast(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, set := range l.subs {
		for s := range set {
			l.deliverLocked(s, e)
		}
	}
}

// deliverLocked never blocks: a subscriber whose buffer is full is closed (it reconnects and resyncs).
func (l *Live) deliverLocked(s *Subscription, e Event) {
	select {
	case s.c <- e:
	default:
		s.closeLocked()
	}
}
