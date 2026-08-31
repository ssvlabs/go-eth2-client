// Copyright © 2021, 2024 Attestant Limited.
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
	"cmp"
	"context"
	"slices"
	"sync"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/http"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
)

const (
	// maxClientScore is the reputation score of a client with no recent
	// failures. Clients start here and periodic decay restores them to it.
	maxClientScore = 10
	// minClientScore bounds the penalty so a client that recovers returns to
	// parity within a bounded time instead of staying deprioritised
	// indefinitely after a burst of failures.
	minClientScore = 0
	// scoreStickinessMargin is how far a challenger's score must exceed the
	// current best client's before Address() switches to it. At 1 the incumbent
	// is kept on ties and only yields to a strictly higher-scored client: that
	// is what stops the event-forwarding provider from flapping when a penalised
	// client decays back to parity, while still moving off a client that is
	// genuinely behind.
	scoreStickinessMargin = 1
)

// Service handles multiple Ethereum 2 clients.
type Service struct {
	log zerolog.Logger

	name string

	clientsMu       sync.RWMutex
	activeClients   []consensusclient.Service
	inactiveClients []consensusclient.Service

	// clientScoresMu guards clientScores.
	clientScoresMu sync.RWMutex
	// clientScores maps a client address to a reputation score in
	// [minClientScore, maxClientScore]. A client starts at the maximum, is
	// penalised on each failed request and recovers via periodic decay, so
	// doCall prefers stable clients while a recovered one returns to parity.
	clientScores map[string]int
	// bestClient is the cached address returned by Address(): the sticky best
	// active client, changed only when it leaves the active set or a challenger
	// beats it by scoreStickinessMargin. Guarded by clientScoresMu.
	bestClient string
}

// New creates a new Ethereum 2 client with multiple endpoints.
// The endpoints are periodically checked to see if they are active,
// and requests will retry a different client if the currently active
// client fails to respond.
func New(ctx context.Context, params ...Parameter) (consensusclient.Service, error) {
	parameters, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Wrap(err, "problem with parameters")
	}

	// Set logging.
	log := zerologger.With().Str("service", "client").Str("impl", "multi").Logger()
	if parameters.logLevel != log.GetLevel() {
		log = log.Level(parameters.logLevel)
	}

	ctx = log.WithContext(ctx)

	if parameters.monitor != nil {
		if err := registerMetrics(ctx, parameters.monitor); err != nil {
			return nil, errors.Wrap(err, "failed to register metrics")
		}
	}

	// Check the state of each client and put it in the active or inactive list, accordingly.
	activeClients := make([]consensusclient.Service, 0, len(parameters.clients))

	inactiveClients := make([]consensusclient.Service, 0, len(parameters.clients))
	for _, client := range parameters.clients {
		switch {
		case client.IsSynced():
			activeClients = append(activeClients, client)
		default:
			inactiveClients = append(inactiveClients, client)
		}
	}

	for _, address := range parameters.addresses {
		client, err := http.New(ctx,
			http.WithLogLevel(parameters.logLevel),
			http.WithTimeout(parameters.timeout),
			http.WithAddress(address),
			http.WithEnforceJSON(parameters.enforceJSON),
			http.WithExtraHeaders(parameters.extraHeaders),
			http.WithAllowDelayedStart(true),
		)
		if err != nil {
			log.Error().Str("provider", address).Msg("Provider not present; dropping from rotation")

			continue
		}

		switch {
		case client.IsSynced():
			activeClients = append(activeClients, client)
		default:
			inactiveClients = append(inactiveClients, client)
		}
	}

	if len(activeClients) == 0 && !parameters.allowDelayedStart {
		return nil, consensusclient.ErrNotActive
	}

	log.Trace().Int("active", len(activeClients)).Int("inactive", len(inactiveClients)).Msg("Initial providers")

	clientScores := make(map[string]int, len(activeClients)+len(inactiveClients))
	for _, client := range activeClients {
		clientScores[client.Address()] = maxClientScore
	}
	for _, client := range inactiveClients {
		clientScores[client.Address()] = maxClientScore
	}

	s := &Service{
		log:             log,
		name:            parameters.name,
		activeClients:   activeClients,
		inactiveClients: inactiveClients,
		clientScores:    clientScores,
	}

	// Set initial metrics.
	for _, client := range s.activeClients {
		s.setProviderStateMetric(ctx, client.Address(), "active")
	}

	for _, client := range s.inactiveClients {
		s.setProviderStateMetric(ctx, client.Address(), "inactive")
	}

	s.setConnectionsMetric(ctx, len(activeClients), len(inactiveClients))

	// Seed the cached best client before Address()/events are served.
	s.refreshBestClient()

	// Kick off monitor.
	go s.monitor(ctx)

	return s, nil
}

// Name returns the name of the client implementation.
func (*Service) Name() string {
	return "multi"
}

// Address returns the address of the best client available. The value is cached
// and refreshed only when the active set or scores change (see
// refreshBestClient), so it is O(1) and sticky to avoid flapping the
// event-forwarding provider as scores decay and recover.
func (s *Service) Address() string {
	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	if s.bestClient == "" {
		return "none"
	}

	return s.bestClient
}

// IsActive returns true if the client is active.
// The service is considered active if at least one client is synced.
func (s *Service) IsActive() bool {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	return len(s.activeClients) > 0
}

// IsSynced returns true if the client is synced.
// The service is considered synced if at least one client is synced.
func (s *Service) IsSynced() bool {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	return len(s.activeClients) > 0
}

// activeClientsSortedByScore returns a copy of the active clients sorted by
// reputation score (highest first), with the sticky best client (see Address())
// leading any score tie so the request path is served by the same client whose
// events are forwarded, then configured order. It sorts a copy so the shared
// slice stays untouched for concurrent readers.
func (s *Service) activeClientsSortedByScore() []consensusclient.Service {
	result := s.activeClientsCopy()

	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	slices.SortStableFunc(result, func(x, y consensusclient.Service) int {
		// Highest score first.
		if c := cmp.Compare(s.scoreClient(y.Address()), s.scoreClient(x.Address())); c != 0 {
			return c
		}
		// On a tie the sticky incumbent leads, then the stable sort keeps
		// configured order for the rest.
		switch s.bestClient {
		case x.Address():
			return -1
		case y.Address():
			return 1
		default:
			return 0
		}
	})

	return result
}

// activeClientsCopy returns a copy of the activeClients slice.
func (s *Service) activeClientsCopy() []consensusclient.Service {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	return slices.Clone(s.activeClients)
}

// refreshBestClient recomputes the cached best client. Call it after the active
// set or scores change; it takes clientsMu then clientScoresMu.
func (s *Service) refreshBestClient() {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	s.refreshBestClientLocked()
}

// refreshBestClientLocked recomputes the cached best client. The caller must
// hold clientsMu (read or write); it takes clientScoresMu.
func (s *Service) refreshBestClientLocked() {
	s.clientScoresMu.Lock()
	defer s.clientScoresMu.Unlock()

	s.bestClient = s.stickyBestLocked()
}

// stickyBestLocked returns the highest-scored active client's address, keeping
// the incumbent unless it has left the active set or a challenger beats it by
// scoreStickinessMargin. The caller must hold clientsMu and clientScoresMu.
func (s *Service) stickyBestLocked() string {
	var (
		best            string
		bestScore       int
		incumbentScore  int
		incumbentActive bool
	)

	for _, client := range s.activeClients {
		address := client.Address()
		score := s.clientScores[address]

		if best == "" || score > bestScore {
			best, bestScore = address, score
		}
		if address == s.bestClient {
			incumbentScore, incumbentActive = score, true
		}
	}

	if incumbentActive && bestScore < incumbentScore+scoreStickinessMargin {
		return s.bestClient
	}

	return best
}
