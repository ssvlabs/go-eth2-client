// Copyright © 2026 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package multi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	"github.com/attestantio/go-eth2-client/mock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newScoredService returns a Service over mock clients with the given addresses.
func newScoredService(t *testing.T, addresses ...string) *Service {
	t.Helper()

	ctx := context.Background()

	clients := make([]consensusclient.Service, 0, len(addresses))
	for _, address := range addresses {
		client, err := mock.New(ctx, mock.WithName(address))
		require.NoError(t, err)
		clients = append(clients, client)
	}

	s, err := New(ctx, WithLogLevel(zerolog.Disabled), WithClients(clients))
	require.NoError(t, err)

	return s.(*Service)
}

// newLoggedService is newScoredService logging to logs.  It is built without New,
// whose monitor would write to logs while tests read them.
func newLoggedService(t *testing.T, logs *bytes.Buffer, addresses ...string) *Service {
	t.Helper()

	s := &Service{
		log:             zerolog.New(zerolog.SyncWriter(logs)),
		clientScores:    make(map[string]int, len(addresses)),
		clientPenalties: make(map[string]uint64, len(addresses)),
	}
	for _, address := range addresses {
		client, err := mock.New(context.Background(), mock.WithName(address))
		require.NoError(t, err)
		s.activeClients = append(s.activeClients, client)
		s.clientScores[address] = maxClientScore
	}
	s.refreshBestClient()

	return s
}

// bestClientChanges counts the changes of best client in logs.
func bestClientChanges(logs *bytes.Buffer) int {
	return strings.Count(logs.String(), "Best client changed")
}

// scoreOf returns a client's score.
func scoreOf(t *testing.T, s *Service, address string) int {
	t.Helper()

	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	return s.clientScore(address)
}

// penalize penalizes a client as a call failing now would.
func penalize(t *testing.T, s *Service, address string) {
	t.Helper()

	s.clientScoresMu.Lock()
	defer s.clientScoresMu.Unlock()

	s.penalizeClient(address, s.clientPenalties[address])
}

// clientByAddress returns the active client with the given address.
func clientByAddress(t *testing.T, s *Service, address string) consensusclient.Service {
	t.Helper()

	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	for _, client := range s.activeClients {
		if client.Address() == address {
			return client
		}
	}

	t.Fatalf("no active client with address %q", address)

	return nil
}

// TestScoringSortsByScore checks that a penalized client moves to the back of
// the order in which doCall tries clients.
func TestScoringSortsByScore(t *testing.T) {
	s := newScoredService(t, "a", "b", "c")

	// Equal scores keep the configured order.
	require.Equal(t, "a", s.activeClientsSortedByScore()[0].Address())

	penalize(t, s, "a")

	sorted := s.activeClientsSortedByScore()
	require.Equal(t, "b", sorted[0].Address())
	require.Equal(t, "a", sorted[len(sorted)-1].Address())
}

// TestAddressStickyBest checks that the best client changes when another client
// scores strictly higher or it is deactivated, but not when a penalized client
// recovers to a tie, and that doCall tries the best client first.
func TestAddressStickyBest(t *testing.T) {
	s := newScoredService(t, "a", "b", "c")
	require.Equal(t, "a", s.Address())

	// b and c now score higher than a; b is first in the configured order.
	penalize(t, s, "a")
	s.refreshBestClient()
	require.Equal(t, "b", s.Address(), "a lower-scored client must not stay best")

	// a recovers to a tie with b and, although first in the configured order,
	// does not take over again.
	s.decayScores()
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
	require.Equal(t, "b", s.Address(), "recovering to a tie must not take over")
	require.Equal(t, "b", s.activeClientsSortedByScore()[0].Address(), "doCall must try the best client first")

	s.deactivateClient(context.Background(), clientByAddress(t, s, "b"))
	require.NotEqual(t, "b", s.Address(), "a deactivated client must not stay best")
}

// TestBestClientChangeIsLogged checks that a change of best client is logged,
// with "none" for no client, and that a refresh that keeps it is not.
func TestBestClientChangeIsLogged(t *testing.T) {
	var logs bytes.Buffer
	s := newLoggedService(t, &logs, "a", "b")
	require.Equal(t, "a", s.Address())
	require.Contains(t, logs.String(), `"previous":"none","best":"a"`)

	logs.Reset()
	s.refreshBestClient()
	require.Empty(t, logs.String(), "an unchanged best client must not be logged")

	penalize(t, s, "a")
	s.refreshBestClient()
	require.Contains(t, logs.String(), `"previous":"a","best":"b"`)
	require.Contains(t, logs.String(), `"message":"Best client changed"`)

	s.activeClients = nil
	s.refreshBestClient()
	require.Equal(t, "none", s.Address())
	require.Contains(t, logs.String(), `"previous":"b","best":"none"`)
}

// TestPenalizedBestLosesTie checks that a best client that fails a call gives way
// on a tie, also at minClientScore, where its score can't fall, and that it keeps
// its place when the other client recovers to a tie.
func TestPenalizedBestLosesTie(t *testing.T) {
	s := newScoredService(t, "a", "b")

	// a is best at the top score; b is a point behind after a failure.
	penalize(t, s, "b")
	s.refreshBestClient()
	require.Equal(t, "a", s.Address())

	failA := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "a" {
			return nil, errors.New("boom")
		}

		return "ok", nil
	}

	// a fails once, down to a tie with b, and gives way.
	res, err := s.doCall(context.Background(), failA, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, scoreOf(t, s, "b"), scoreOf(t, s, "a"))
	require.Equal(t, "b", s.Address(), "a best client penalized to a tie must give way")

	// Both recover to the top score; b keeps its place.
	s.decayScores()
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
	require.Equal(t, "b", s.Address(), "recovering to a tie must not take over")

	// With both at minClientScore, b failing lowers no score, but b still gives way.
	for range maxClientScore {
		penalize(t, s, "a")
		penalize(t, s, "b")
	}
	s.refreshBestClient()
	require.Equal(t, "b", s.Address())

	failB := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "b" {
			return nil, errors.New("boom")
		}

		return "ok", nil
	}
	_, err = s.doCall(context.Background(), failB, nil)
	require.NoError(t, err)
	require.Equal(t, minClientScore, scoreOf(t, s, "b"))
	require.Equal(t, "a", s.Address(), "a best client failing at minClientScore must give way")
}

// TestScoringFloorsAndRecovers checks that scores are floored at minClientScore,
// and that decay restores them to maxClientScore.
func TestScoringFloorsAndRecovers(t *testing.T) {
	s := newScoredService(t, "a")

	for range maxClientScore + 5 {
		penalize(t, s, "a")
	}
	require.Equal(t, minClientScore, scoreOf(t, s, "a"))

	for range maxClientScore {
		s.decayScores()
	}
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
}

// TestDoCallPenalizesFailingClient checks that a failover penalizes only the
// failing client, and returns the next client's result.
func TestDoCallPenalizesFailingClient(t *testing.T) {
	s := newScoredService(t, "a", "b")

	call := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "a" {
			return nil, errors.New("boom")
		}

		return "ok", nil
	}

	res, err := s.doCall(context.Background(), call, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, maxClientScore-1, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallPenalizesOncePerIncident checks that calls in flight when a client
// fails cost it one point between them, not one each.
func TestDoCallPenalizesOncePerIncident(t *testing.T) {
	s := newScoredService(t, "a", "b")

	const calls = 12

	// a stalls until every call has reached it, then fails them all.
	var stalled sync.WaitGroup
	stalled.Add(calls)
	call := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "a" {
			stalled.Done()
			stalled.Wait()

			return nil, errors.New("stalled")
		}

		return "ok", nil
	}

	var done sync.WaitGroup
	results := make(chan any, calls)
	for range calls {
		done.Add(1)
		go func() {
			defer done.Done()

			res, err := s.doCall(context.Background(), call, nil)
			if err != nil {
				res = err
			}
			results <- res
		}()
	}
	done.Wait()
	close(results)

	for res := range results {
		require.Equal(t, "ok", res)
	}
	require.Equal(t, maxClientScore-1, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallSharedFailureKeepsBest checks that a call failing on every client
// leaves the best client in place, whether the other scored as high or lower,
// also when the other is at minClientScore, where its score can't fall.
func TestDoCallSharedFailureKeepsBest(t *testing.T) {
	failAll := func(_ context.Context, _ consensusclient.Service) (any, error) {
		return nil, errors.New("boom")
	}

	// The penalties of a, the best client, and of b before the call.
	for _, penalties := range [][2]int{{0, 0}, {0, 1}, {maxClientScore - 1, maxClientScore}} {
		var logs bytes.Buffer
		s := newLoggedService(t, &logs, "a", "b")
		for range penalties[0] {
			penalize(t, s, "a")
		}
		for range penalties[1] {
			penalize(t, s, "b")
		}
		s.refreshBestClient()
		require.Equal(t, "a", s.Address())
		logs.Reset()

		_, err := s.doCall(context.Background(), failAll, nil)
		require.Error(t, err)
		require.Zero(t, bestClientChanges(&logs), "a failure every client shares must not move the best client")
	}
}

// TestDoCallFailoverMovesBestToServingClient checks that a best client a call
// penalized to a tie gives way to the tied client that served the call, not to one
// the call penalized too.
func TestDoCallFailoverMovesBestToServingClient(t *testing.T) {
	var logs bytes.Buffer
	s := newLoggedService(t, &logs, "a", "b", "c")
	penalize(t, s, "c")
	s.refreshBestClient()
	require.Equal(t, "a", s.Address())
	logs.Reset()

	call := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "c" {
			return "ok", nil
		}

		return nil, errors.New("boom")
	}

	res, err := s.doCall(context.Background(), call, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	for _, address := range []string{"a", "b", "c"} {
		require.Equal(t, maxClientScore-1, scoreOf(t, s, address))
	}
	require.Equal(t, "c", s.Address())
	require.Equal(t, 1, bestClientChanges(&logs), "the best client must move once, straight to c")
}

// TestDoCallOverlappingFailures checks two overlapping calls that fail on a, of
// which b serves the first and fails the second: the best client moves once, to
// b, not back to a, also at minClientScore.
func TestDoCallOverlappingFailures(t *testing.T) {
	type callKey struct{}

	for _, atMinimum := range []bool{false, true} {
		var logs bytes.Buffer
		s := newLoggedService(t, &logs, "a", "b")
		if atMinimum {
			for range maxClientScore {
				penalize(t, s, "a")
				penalize(t, s, "b")
			}
		}
		require.Equal(t, "a", s.Address())
		logs.Reset()

		// Both calls reach a before either fails there, so they share one incident.
		// The second fails on a once the first has moved on to b, and on b once the
		// first has returned.
		var aReached sync.WaitGroup
		aReached.Add(2)
		firstOnB := make(chan struct{})
		firstDone := make(chan struct{})
		call := func(ctx context.Context, client consensusclient.Service) (any, error) {
			first := ctx.Value(callKey{}) == 1
			if client.Address() == "a" {
				aReached.Done()
				aReached.Wait()
				if !first {
					<-firstOnB
				}

				return nil, errors.New("a down")
			}
			if first {
				close(firstOnB)

				return "ok", nil
			}
			<-firstDone

			return nil, errors.New("b down")
		}

		var firstErr, secondErr error
		var calls sync.WaitGroup
		calls.Add(2)
		go func() {
			defer calls.Done()
			_, firstErr = s.doCall(context.WithValue(context.Background(), callKey{}, 1), call, nil)
			close(firstDone)
		}()
		go func() {
			defer calls.Done()
			_, secondErr = s.doCall(context.WithValue(context.Background(), callKey{}, 2), call, nil)
		}()
		calls.Wait()
		require.NoError(t, firstErr)
		require.Error(t, secondErr)

		require.Equal(t, "b", s.Address())
		require.Equal(t, 1, bestClientChanges(&logs), "the best client must move once, to b")
	}
}

// TestDoCallPenalizesEachIncident checks that failures one after another each
// cost a point: only calls in flight together share one.
func TestDoCallPenalizesEachIncident(t *testing.T) {
	s := newScoredService(t, "a")

	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		return nil, errors.New("boom")
	}

	for range 2 {
		_, err := s.doCall(context.Background(), call, nil)
		require.Error(t, err)
	}
	require.Equal(t, maxClientScore-2, scoreOf(t, s, "a"))
}

// TestDoCallNoPenaltyOnUserError checks that a 4xx response is returned at once,
// penalizing no client.
func TestDoCallNoPenaltyOnUserError(t *testing.T) {
	s := newScoredService(t, "a", "b")

	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		return nil, &api.Error{StatusCode: 400}
	}

	_, err := s.doCall(context.Background(), call, nil)
	require.Error(t, err)
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallNoPenaltyOnCanceled checks that a canceled-context error is returned
// at once, penalizing no client.
func TestDoCallNoPenaltyOnCanceled(t *testing.T) {
	s := newScoredService(t, "a", "b")

	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		return nil, context.Canceled
	}

	_, err := s.doCall(context.Background(), call, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallFailsOverOnClientTimeout checks that a client timing out while the
// caller's context is live is penalized, and the call fails over (upstream returns
// the error).
func TestDoCallFailsOverOnClientTimeout(t *testing.T) {
	s := newScoredService(t, "a", "b")

	call := func(_ context.Context, client consensusclient.Service) (any, error) {
		if client.Address() == "a" {
			return nil, fmt.Errorf("request timed out: %w", context.DeadlineExceeded)
		}

		return "ok", nil
	}

	res, err := s.doCall(context.Background(), call, nil)
	require.NoError(t, err)
	require.Equal(t, "ok", res)
	require.Equal(t, maxClientScore-1, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallReturnsResultOnDoneContext checks that a call made with a context
// that is already done still returns what the client serves without a request,
// as upstream does.
func TestDoCallReturnsResultOnDoneContext(t *testing.T) {
	s := newScoredService(t, "a", "b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		return "cached", nil
	}

	res, err := s.doCall(ctx, call, nil)
	require.NoError(t, err)
	require.Equal(t, "cached", res)
}

// TestDoCallStopsOnDeadContext checks that a failure with a caller context that
// was already done penalizes no client, and that no further client is called.
func TestDoCallStopsOnDeadContext(t *testing.T) {
	s := newScoredService(t, "a", "b", "c")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		calls++

		return nil, errors.New("boom")
	}

	_, err := s.doCall(ctx, call, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	for _, address := range []string{"a", "b", "c"} {
		require.Equal(t, maxClientScore, scoreOf(t, s, address))
	}
}

// TestDoCallStopsWhenContextEndsMidCall checks that a client still running when
// the caller's deadline passes is penalized, and that no further client is
// called.
func TestDoCallStopsWhenContextEndsMidCall(t *testing.T) {
	s := newScoredService(t, "a", "b")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var calls int
	call := func(ctx context.Context, _ consensusclient.Service) (any, error) {
		calls++
		// Hang until the caller's deadline passes.
		<-ctx.Done()

		return nil, ctx.Err()
	}

	_, err := s.doCall(ctx, call, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
	require.Equal(t, maxClientScore-1, scoreOf(t, s, "a"))
	require.Equal(t, maxClientScore, scoreOf(t, s, "b"))
}

// TestDoCallKeepsContextErrorWhenStopping checks that when the caller's context
// ends during a call that fails for another reason, the returned error carries
// both errors.
func TestDoCallKeepsContextErrorWhenStopping(t *testing.T) {
	s := newScoredService(t, "a", "b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	call := func(_ context.Context, _ consensusclient.Service) (any, error) {
		cancel()

		return nil, errors.New("boom")
	}

	_, err := s.doCall(ctx, call, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "boom")
	require.Equal(t, maxClientScore-1, scoreOf(t, s, "a"))
}
