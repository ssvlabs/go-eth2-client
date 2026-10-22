// Copyright © 2021 - 2026 Attestant Limited.
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
	"time"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/http"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
)

const (
	// maxClientScore is the score of a client without recent failures: clients
	// start at it, and decay restores them to it.
	maxClientScore = 10
	// minClientScore floors the score, so a client recovers in bounded time
	// however many failures it had.
	minClientScore = 0
)

// Service handles multiple Ethereum 2 clients.
type Service struct {
	log zerolog.Logger

	name string

	clientsMu       sync.RWMutex
	activeClients   []consensusclient.Service
	inactiveClients []consensusclient.Service

	// eventsRetryInterval is how long Events waits between attempts to subscribe a client that
	// was not synced, or failed to subscribe, when Events was called.  Read it through
	// retryInterval, which covers it being unset.
	eventsRetryInterval time.Duration

	// clientScoresMu guards clientScores, clientPenalties and bestClient.
	clientScoresMu sync.RWMutex
	// clientScores maps client addresses to scores in [minClientScore,
	// maxClientScore].  Failures cost points (see penalizeClient) and decay restores
	// them, so doCall prefers clients that are working.
	clientScores map[string]int
	// clientPenalties counts the penalties that lowered each client's score; see
	// penalizeClient.
	clientPenalties map[string]uint64
	// bestClient is the address Address() returns; see stickyBestLocked.
	bestClient string
}

// defaultEventsRetryInterval is the default for Service.eventsRetryInterval.
const defaultEventsRetryInterval = 5 * time.Second

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
		log:                 log,
		name:                parameters.name,
		activeClients:       activeClients,
		inactiveClients:     inactiveClients,
		eventsRetryInterval: parameters.eventsRetryInterval,
		clientScores:        clientScores,
		clientPenalties:     make(map[string]uint64, len(clientScores)),
	}

	// Set initial metrics.
	for _, client := range s.activeClients {
		s.setProviderStateMetric(ctx, client.Address(), "active")
	}

	for _, client := range s.inactiveClients {
		s.setProviderStateMetric(ctx, client.Address(), "inactive")
	}

	s.setConnectionsMetric(ctx, len(activeClients), len(inactiveClients))

	// Pick the best client before Address() is first called.
	s.refreshBestClient()

	// Kick off monitor.
	go s.monitor(ctx)

	return s, nil
}

// Name returns the name of the client implementation.
func (*Service) Name() string {
	return "multi"
}

// Address returns the address of the best active client, or "none".  It is
// cached and sticky (see stickyBestLocked), as Events calls it for every event
// to pick the client whose events it forwards.
func (s *Service) Address() string {
	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	return addressOrNone(s.bestClient)
}

// addressOrNone returns address, or "none" for no client.
func addressOrNone(address string) string {
	if address == "" {
		return "none"
	}

	return address
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

// activeClientsSortedByScore returns a copy of the active clients, highest score
// first.  The best client leads its score tie, so requests go to the client
// whose events are forwarded; other ties keep the active list's order.
func (s *Service) activeClientsSortedByScore() []consensusclient.Service {
	s.clientsMu.RLock()
	result := slices.Clone(s.activeClients)
	s.clientsMu.RUnlock()

	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	slices.SortStableFunc(result, func(x, y consensusclient.Service) int {
		// Highest score first, then the best client.
		if c := cmp.Compare(s.clientScore(y.Address()), s.clientScore(x.Address())); c != 0 {
			return c
		}
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

// refreshBestClient recomputes bestClient after the active set or scores
// change.  It takes clientsMu, then clientScoresMu.
func (s *Service) refreshBestClient() {
	s.refreshBestClientAfterPenalties(nil)
}

// refreshBestClientAfterPenalties is refreshBestClient after a call lowered the
// scores of the clients in penalized (see stickyBestLocked).
func (s *Service) refreshBestClientAfterPenalties(penalized []string) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()

	s.refreshBestClientLocked(penalized)
}

// refreshBestClientLocked is refreshBestClientAfterPenalties for a caller that
// holds clientsMu.  It logs a change, as it moves both requests and forwarded
// events.
func (s *Service) refreshBestClientLocked(penalized []string) {
	s.clientScoresMu.Lock()
	defer s.clientScoresMu.Unlock()

	best := s.stickyBestLocked(penalized)
	if best == s.bestClient {
		return
	}

	event := s.log.Info().Str("previous", addressOrNone(s.bestClient)).Str("best", addressOrNone(best))
	if best != "" {
		event = event.Int("score", s.clientScores[best])
	}
	event.Msg("Best client changed")
	s.bestClient = best
}

// stickyBestLocked returns the address of the highest-scored active client, but
// keeps the current best client unless it is no longer active or another client
// scores strictly higher: switching on a tie would flip the forwarded events
// back and forth as penalized clients recover.  A best client that a call just
// penalized (penalized lists the clients that failed the call and whose scores
// fell while it ran) also gives way on a tie, to a client the call did not
// penalize: so a failing best client does not hold on while another scores as
// high, but a failure every client shares moves nothing.  The caller must hold
// clientsMu and clientScoresMu.
func (s *Service) stickyBestLocked(penalized []string) string {
	var (
		challenger      string
		challengerScore int
		incumbentScore  int
		incumbentActive bool
	)

	for _, client := range s.activeClients {
		address := client.Address()
		score := s.clientScores[address]

		if address == s.bestClient {
			incumbentScore, incumbentActive = score, true

			continue
		}
		// Of equal challengers, prefer one the call did not penalize.
		if challenger == "" || score > challengerScore ||
			score == challengerScore && slices.Contains(penalized, challenger) && !slices.Contains(penalized, address) {
			challenger, challengerScore = address, score
		}
	}

	switch {
	case !incumbentActive:
		return challenger
	case challenger == "":
		return s.bestClient
	case challengerScore > incumbentScore,
		challengerScore == incumbentScore && slices.Contains(penalized, s.bestClient) && !slices.Contains(penalized, challenger):
		return challenger
	default:
		return s.bestClient
	}
}
