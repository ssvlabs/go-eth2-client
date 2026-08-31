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
	"context"
	"errors"
	"testing"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	"github.com/attestantio/go-eth2-client/mock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// newScoredService builds a multi-client Service backed by mock clients whose
// addresses are the supplied names, so the scoring logic can be exercised.
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

// scoreOf reads a client's score under the appropriate lock.
func scoreOf(t *testing.T, s *Service, address string) int {
	t.Helper()

	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	return s.scoreClient(address)
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

// TestScoringSortsByScore checks that penalising a client moves it to the back
// of the score-sorted order that doCall selects from.
func TestScoringSortsByScore(t *testing.T) {
	s := newScoredService(t, "a", "b", "c")

	// Equal scores keep configured order.
	require.Equal(t, "a", s.activeClientsSortedByScore()[0].Address())

	s.clientScoresMu.Lock()
	s.penalizeClient("a")
	s.clientScoresMu.Unlock()

	sorted := s.activeClientsSortedByScore()
	require.Equal(t, "b", sorted[0].Address())
	require.Equal(t, "a", sorted[len(sorted)-1].Address())
}

// TestAddressStickyBest checks the sticky, cached best client: penalising the
// incumbent past the margin hands the primary to the next best client, decaying
// it back to parity does not reclaim the primary (the anti-flap property), the
// request-path sort leads with the same incumbent, and deactivating it switches
// away.
func TestAddressStickyBest(t *testing.T) {
	s := newScoredService(t, "a", "b", "c")
	require.Equal(t, "a", s.Address())

	// Penalising the incumbent past the margin hands the primary to the next
	// best client (b, by config order).
	s.clientScoresMu.Lock()
	for range scoreStickinessMargin {
		s.penalizeClient("a")
	}
	s.clientScoresMu.Unlock()
	s.refreshBestClient()
	require.Equal(t, "b", s.Address(), "a client behind by the margin should lose the primary")

	// Decaying a back to parity must not reclaim the primary: a returns to the
	// ceiling (tied with b, and config-first) yet Address() stays b. This is the
	// anti-flap property the stickiness exists for.
	for range scoreStickinessMargin {
		s.decayScores()
	}
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
	require.Equal(t, "b", s.Address(), "decay-to-parity must not reclaim the primary")

	// The request path leads with the same incumbent, so requests are served by
	// the client whose events are forwarded.
	require.Equal(t, "b", s.activeClientsSortedByScore()[0].Address(),
		"sort must lead with the sticky incumbent")

	// Deactivating the incumbent switches the primary away from it.
	s.deactivateClient(context.Background(), clientByAddress(t, s, "b"))
	require.NotEqual(t, "b", s.Address(), "deactivated incumbent must not stay best")
}

// TestScoringFloorsAndRecovers checks that the penalty is bounded at
// minClientScore and that periodic decay restores a client to maxClientScore.
func TestScoringFloorsAndRecovers(t *testing.T) {
	s := newScoredService(t, "a")

	s.clientScoresMu.Lock()
	for range maxClientScore + 5 {
		s.penalizeClient("a")
	}
	s.clientScoresMu.Unlock()
	require.Equal(t, minClientScore, scoreOf(t, s, "a"))

	for range maxClientScore {
		s.decayScores()
	}
	require.Equal(t, maxClientScore, scoreOf(t, s, "a"))
}

// TestDoCallPenalisesFailingClient checks that a failover penalises only the
// failing client and returns the next client's successful result.
func TestDoCallPenalisesFailingClient(t *testing.T) {
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

// TestDoCallNoPenaltyOnUserError checks that a 4xx response returns immediately
// without penalising any client.
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

// TestDoCallNoPenaltyOnCanceled checks that a canceled-context error returns
// immediately without penalising any client.
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

// TestDoCallStopsOnDeadContext checks that once the caller's context is done the
// loop stops after the current client rather than trying every remaining one.
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
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
