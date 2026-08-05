//go:build !js

package nostr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// TestPoolSharesDialAttemptsAcrossSubscriptions asserts that while a relay is
// unreachable the pool dials it at a rate driven by elapsed time, not by the number of
// open subscriptions: failures must be memoized and the reconnect backoff must be
// per-relay. Regression test for getAlby/go-nostr#5.
func TestPoolSharesDialAttemptsAcrossSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~8s of wall clock")
	}

	var dials atomic.Int64
	// never completes a websocket handshake, so every Connect() fails fast
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewSimplePool(ctx)

	const nsubs = 10
	for range nsubs {
		ch := pool.SubscribeMany(ctx, []string{srv.URL}, Filter{Kinds: []int{KindTextNote}})
		go func() {
			for range ch {
			}
		}()
	}

	// 8s covers the first three backoff rounds of a single leg (t=0, t=3, t≈8.1)
	time.Sleep(8 * time.Second)
	cancel()

	got := dials.Load()
	t.Logf("%d subscriptions produced %d dials in 8s (%.1f per subscription)",
		nsubs, got, float64(got)/nsubs)

	// with per-relay memoization this is ~3 (one per backoff round for the whole pool);
	// today it is ~3*nsubs
	require.LessOrEqual(t, got, int64(5),
		"connection attempts must be per-relay, not per-subscription")
}

// TestPoolSuppressesRedialsAfterFailure asserts that a relay that failed to connect is
// held out of rotation without any opt-in: further EnsureRelay calls must fail fast from
// the recorded failure instead of opening another socket. Regression test for
// getAlby/go-nostr#5, where nothing suppressed redials.
//
// WithPenaltyBox is passed to pin down that it is a no-op: the hold-off must be the same
// with it as without, so nobody reintroduces a second, harsher curve to maintain.
func TestPoolSuppressesRedialsAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []PoolOption
	}{
		{name: "default"},
		{name: "with deprecated penalty box", opts: []PoolOption{WithPenaltyBox()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dials.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			pool := NewSimplePool(ctx, tc.opts...)

			_, err := pool.EnsureRelay(srv.URL)
			require.Error(t, err, "first dial should fail")
			require.EqualValues(t, 1, dials.Load())

			for range 3 {
				_, err = pool.EnsureRelay(srv.URL)
				require.Error(t, err)
			}

			t.Logf("4 EnsureRelay calls after a failure produced %d dials, last error: %v", dials.Load(), err)
			require.EqualValues(t, 1, dials.Load(), "a recorded failure must suppress redials")

			f, ok := pool.dialFailures.Load(NormalizeURL(srv.URL))
			require.True(t, ok)
			require.Equal(t, dialRetryInitial, f.interval, "hold-off must not depend on the option")
		})
	}
}

// TestPoolDialBackoffGrowsThenGoesStale asserts how a relay's dial hold-off evolves:
// consecutive failures make it grow, and once a relay has been left alone for longer
// than the hold-off it was serving, the record is dropped so the next failure starts
// over from the initial interval instead of inheriting an old outage's backoff.
func TestPoolDialBackoffGrowsThenGoesStale(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out two dial hold-offs")
	}

	var dials atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewSimplePool(ctx)
	nm := NormalizeURL(srv.URL)

	failOnce := func() dialFailure {
		t.Helper()

		_, err := pool.EnsureRelay(srv.URL)
		require.Error(t, err)

		f, ok := pool.dialFailures.Load(nm)
		require.True(t, ok, "failed dial was not recorded")
		return f
	}

	first := failOnce()
	require.Equal(t, dialRetryInitial, first.interval, "first failure should wait the initial interval")

	// retry as soon as the hold-off expires: still consecutive, so it grows
	time.Sleep(time.Until(first.retryAt) + 50*time.Millisecond)
	second := failOnce()
	require.Equal(t, dialRetryInitial*17/10, second.interval, "consecutive failure should wait longer")

	// now leave the relay alone past retryAt + interval, so the record goes stale
	time.Sleep(time.Until(second.retryAt.Add(second.interval)) + 50*time.Millisecond)
	third := failOnce()
	require.Equal(t, dialRetryInitial, third.interval,
		"a stale failure record should be forgotten, not carried into the next outage")

	require.EqualValues(t, 3, dials.Load(), "each of the three attempts should dial exactly once")
}

// TestPoolCloseClosesRelayConnections asserts that closing the pool tears down the
// websockets it opened, i.e. that relays created by EnsureRelay are tied to the pool
// context. Regression test for the connection leak in getAlby/go-nostr#5.
func TestPoolCloseClosesRelayConnections(t *testing.T) {
	var open atomic.Int64
	ws := newWebsocketServer(func(conn *websocket.Conn) {
		open.Add(1)
		defer open.Add(-1)
		io.Copy(io.Discard, conn) // block until the client goes away
	})
	defer ws.Close()

	pool := NewSimplePool(context.Background())
	_, err := pool.EnsureRelay(ws.URL)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return open.Load() == 1 }, 3*time.Second, 20*time.Millisecond,
		"relay never connected")

	pool.Close("test over")

	require.Eventually(t, func() bool { return open.Load() == 0 }, 3*time.Second, 20*time.Millisecond,
		"websocket still open after pool.Close()")
}

// closingRelay is a relay that answers every REQ with CLOSED and keeps the connection
// open, recording when each REQ arrived and how many CLOSE frames came back.
//
// closedDelay holds each subscription open before closing it. Answering instantly makes
// subscriptions live for microseconds, which is too short a window to observe how many
// the client is tracking; a delay widens it without changing the retry behaviour.
type closingRelay struct {
	server      *httptest.Server
	closedDelay time.Duration

	writeMu sync.Mutex // websocket writes are not safe for concurrent use

	mu       sync.Mutex
	reqTimes []time.Time
	closes   int
}

func newClosingRelay(closedDelay time.Duration) *closingRelay {
	cr := &closingRelay{closedDelay: closedDelay}
	cr.server = newWebsocketServer(func(conn *websocket.Conn) {
		for {
			var raw []any
			if err := websocket.JSON.Receive(conn, &raw); err != nil {
				return
			}
			if len(raw) == 0 {
				continue
			}
			switch raw[0] {
			case "REQ":
				cr.mu.Lock()
				cr.reqTimes = append(cr.reqTimes, time.Now())
				cr.mu.Unlock()
				// off the read loop, so one leg's delay does not stall the others
				go func(subID any) {
					time.Sleep(cr.closedDelay)
					cr.writeMu.Lock()
					defer cr.writeMu.Unlock()
					websocket.JSON.Send(conn, []any{"CLOSED", subID, "error: go away"})
				}(raw[1])
			case "CLOSE":
				cr.mu.Lock()
				cr.closes++
				cr.mu.Unlock()
			}
		}
	})
	return cr
}

// gaps returns the intervals between consecutive REQs.
func (cr *closingRelay) gaps() []time.Duration {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	gaps := make([]time.Duration, 0, len(cr.reqTimes))
	for i := 1; i < len(cr.reqTimes); i++ {
		gaps = append(gaps, cr.reqTimes[i].Sub(cr.reqTimes[i-1]))
	}
	return gaps
}

// TestPoolBacksOffWhenRelayClosesSubscription asserts that when a relay answers every REQ
// with CLOSED, subMany's retries slow down instead of repeating at a fixed cadence. A
// single subscription is used so every REQ the relay sees belongs to the same leg, which
// makes the gaps between them a direct reading of the backoff.
//
// subMany resets interval to 3s whenever Subscribe() returns without error (pool.go), but
// a relay that CLOSEs immediately still lets Subscribe() succeed, so today the backoff is
// pinned at 3s forever. Regression test for getAlby/go-nostr#5.
func TestPoolBacksOffWhenRelayClosesSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~30s of wall clock")
	}

	// growthFactor is the minimum ratio between one retry interval and the next. subMany
	// grows the interval by 1.7x; requiring 1.4x leaves room for scheduling jitter while
	// still failing a flat cadence.
	const growthFactor = 1.4

	cr := newClosingRelay(0)
	defer cr.server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewSimplePool(ctx)
	ch := pool.SubscribeMany(ctx, []string{cr.server.URL}, Filter{Kinds: []int{KindTextNote}})
	go func() {
		for range ch {
		}
	}()

	// 30s covers t = 0, 3, 8.1, 16.77 with a growing backoff: three gaps to compare
	time.Sleep(30 * time.Second)
	cancel()

	gaps := cr.gaps()
	t.Logf("retry intervals: %v (%d CLOSE frames sent back)", gaps, cr.closes)

	require.GreaterOrEqual(t, len(gaps), 3, "not enough retries observed to judge the backoff")
	for i := 1; i < len(gaps); i++ {
		require.GreaterOrEqual(t, gaps[i].Seconds(), gaps[i-1].Seconds()*growthFactor,
			"retry interval %d (%v) did not grow over interval %d (%v): backoff is not "+
				"advancing when a relay CLOSEs every subscription", i, gaps[i], i-1, gaps[i-1])
	}
}

// TestPoolSubscriptionsDoNotAccumulateOnClosed asserts that a relay answering every REQ
// with CLOSED does not leave stale subscriptions behind on the relay object as subMany
// retries: at any instant the relay should track at most one subscription per caller.
// Regression test for getAlby/go-nostr#5.
func TestPoolSubscriptionsDoNotAccumulateOnClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~15s of wall clock")
	}

	// hold each subscription open long enough that the 10ms sampling below can see it
	const closedDelay = 300 * time.Millisecond

	cr := newClosingRelay(closedDelay)
	defer cr.server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := NewSimplePool(ctx)

	const nsubs = 20
	for range nsubs {
		ch := pool.SubscribeMany(ctx, []string{cr.server.URL}, Filter{Kinds: []int{KindTextNote}})
		go func() {
			for range ch {
			}
		}()
	}

	// sample continuously: a single reading can land inside a backoff sleep, when every
	// leg is between subscriptions and the map is legitimately empty
	var peak int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if relay, ok := pool.Relays.Load(NormalizeURL(cr.server.URL)); ok {
			if n := relay.Subscriptions.Size(); n > peak {
				peak = n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	t.Logf("%d subscriptions, %d REQs in 15s, %d CLOSE frames sent; "+
		"peak subscriptions tracked on the relay: %d", nsubs, len(cr.gaps())+1, cr.closes, peak)

	// the sampling must actually catch subscriptions in flight, otherwise a peak of 0
	// would pass this test without proving anything
	require.Positive(t, peak, "no live subscriptions observed; the test is not measuring anything")
	require.LessOrEqual(t, peak, nsubs, "stale subscriptions accumulated across CLOSED reconnects")
}
