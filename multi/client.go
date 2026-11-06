// Copyright © 2021 - 2024 Attestant Limited.
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
	"fmt"
	nethttp "net/http"
	"strings"
	"time"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	"github.com/attestantio/go-eth2-client/http"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

// monitor monitors active and inactive clients, and moves them between
// lists accordingly.
func (s *Service) monitor(ctx context.Context) {
	log := s.log.With().Logger()
	ctx = log.WithContext(ctx)

	log.Trace().Msg("Monitor starting")

	for {
		select {
		case <-ctx.Done():
			log.Trace().Msg("Context done; monitor stopping")

			return
		case <-time.After(30 * time.Second):
			s.recheck(ctx)
			s.decayScores()
		}
	}
}

// recheck checks clients to update their state.
func (s *Service) recheck(ctx context.Context) {
	// Fetch all clients.
	clients := make([]consensusclient.Service, 0, len(s.activeClients)+len(s.inactiveClients))
	s.clientsMu.RLock()
	activeClients := len(s.activeClients)
	clients = append(clients, s.activeClients...)
	clients = append(clients, s.inactiveClients...)
	s.clientsMu.RUnlock()

	// Ping each client to update its state.
	for _, client := range clients {
		// We actively recheck the connection state if we had no active clients at the start of the check, in an attempt to obtain
		// at least 1 active client.
		if activeClients == 0 {
			if httpClient, isHTTPClient := client.(*http.Service); isHTTPClient {
				httpClient.CheckConnectionState(ctx)
			}
		}

		switch {
		case client.IsSynced():
			s.activateClient(ctx, client)
		default:
			s.deactivateClient(ctx, client)
		}
	}
}

// deactivateClient marks a client as deactivated, moving it to the inactive list if not currently on it.
func (s *Service) deactivateClient(ctx context.Context, client consensusclient.Service) {
	log := zerolog.Ctx(ctx)

	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	activeClients := make([]consensusclient.Service, 0, len(s.activeClients)+len(s.inactiveClients))

	inactiveClients := s.inactiveClients
	for _, activeClient := range s.activeClients {
		if activeClient == client {
			inactiveClients = append(inactiveClients, activeClient)

			s.setProviderStateMetric(ctx, client.Address(), "inactive")
		} else {
			activeClients = append(activeClients, activeClient)
		}
	}

	if len(inactiveClients) != len(s.inactiveClients) {
		log.Trace().Str("client", client.Address()).
			Int("active", len(activeClients)).
			Int("inactive", len(inactiveClients)).
			Msg("Client deactivated")
	}

	s.activeClients = activeClients
	s.inactiveClients = inactiveClients
	s.setConnectionsMetric(ctx, len(s.activeClients), len(s.inactiveClients))
	s.refreshBestClientLocked(nil)
}

// activateClient activates a client, moving it to the active list if not currently on it.
func (s *Service) activateClient(ctx context.Context, client consensusclient.Service) {
	log := zerolog.Ctx(ctx)

	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()

	activeClients := s.activeClients

	inactiveClients := make([]consensusclient.Service, 0, len(s.activeClients)+len(s.inactiveClients))
	for _, inactiveClient := range s.inactiveClients {
		if inactiveClient == client {
			activeClients = append(activeClients, inactiveClient)

			s.setProviderStateMetric(ctx, client.Address(), "active")
		} else {
			inactiveClients = append(inactiveClients, inactiveClient)
		}
	}

	if len(inactiveClients) != len(s.inactiveClients) {
		log.Trace().Str("client", client.Address()).
			Int("active", len(activeClients)).
			Int("inactive", len(inactiveClients)).
			Msg("Client activated")
	}

	s.activeClients = activeClients
	s.inactiveClients = inactiveClients
	s.setConnectionsMetric(ctx, len(s.activeClients), len(s.inactiveClients))
	s.refreshBestClientLocked(nil)
}

// clientScore returns the client's score.  The caller must hold clientScoresMu.
func (s *Service) clientScore(clientAddr string) int {
	return s.clientScores[clientAddr]
}

// penalizeClient lowers the client's score by one for a failed call, down to
// minClientScore, and returns the score.  Calls in flight together cost one point
// between them: if the client has been penalized since the call started
// (penaltiesAtStart), its score has already fallen.  The caller must hold
// clientScoresMu.
func (s *Service) penalizeClient(clientAddr string, penaltiesAtStart uint64) int {
	score, scored := s.clientScores[clientAddr]
	if !scored || score == minClientScore || s.clientPenalties[clientAddr] != penaltiesAtStart {
		return score
	}

	s.clientPenalties[clientAddr]++
	score--
	s.clientScores[clientAddr] = score

	return score
}

// currentScore returns the client's score.  It takes clientScoresMu.
func (s *Service) currentScore(clientAddr string) int {
	s.clientScoresMu.RLock()
	defer s.clientScoresMu.RUnlock()

	return s.clientScore(clientAddr)
}

// penalizeFailure is penalizeClient taking clientScoresMu.  It returns the score,
// and penalized with the client added.
func (s *Service) penalizeFailure(clientAddr string, penaltiesAtStart uint64, penalized []string) (int, []string) {
	s.clientScoresMu.Lock()
	defer s.clientScoresMu.Unlock()

	return s.penalizeClient(clientAddr, penaltiesAtStart), append(penalized, clientAddr)
}

// decayScores raises each score below maxClientScore by one.  The monitor calls
// it on every tick; it is how penalized clients recover.
func (s *Service) decayScores() {
	s.clientScoresMu.Lock()
	var recovered []string
	for addr, score := range s.clientScores {
		if score < maxClientScore {
			s.clientScores[addr] = score + 1
			if score+1 == maxClientScore {
				recovered = append(recovered, addr)
			}
		}
	}
	s.clientScoresMu.Unlock()

	for _, addr := range recovered {
		s.log.Debug().Str("address", addr).Msg("Client score recovered")
	}

	s.refreshBestClient()
}

// callFunc is the definition for a call function.  It provides a generic return interface
// to allow the caller to unpick the results as it sees fit.
type callFunc func(ctx context.Context, client consensusclient.Service) (any, error)

// errHandlerFunc is the definition for an error handler function.  It looks at the error
// returned from the client, potentially rewrites it, and also states if the error should
// result in a provider failover.
type errHandlerFunc func(ctx context.Context, client consensusclient.Service, err error) (bool, error)

// rejection is a client's rejection of a call with a user error (see
// isUserError), to penalize if another client serves the call.
type rejection struct {
	address          string
	err              error
	penaltiesAtStart uint64
}

// doCall carries out a call on the active clients in turn until one succeeds.
func (s *Service) doCall(ctx context.Context, call callFunc, errHandler errHandlerFunc) (any, error) {
	log := s.log.With().Logger()
	ctx = log.WithContext(ctx)

	// Take a snapshot of the active clients, best first.
	activeClients := s.activeClientsSortedByScore()

	if len(activeClients) == 0 {
		// There are no active clients; attempt to re-enable the inactive clients.
		s.recheck(ctx)
		activeClients = s.activeClientsSortedByScore()
	}

	if len(activeClients) == 0 {
		return nil, errors.New("no clients to which to make call")
	}

	var (
		err error
		res any
		// penalized lists the clients penalized for failing this call.  The best
		// client is refreshed once, at the end, so a failure every client shares
		// moves nothing.
		penalized []string
		// rejections lists the clients that rejected this call with a user error.
		rejections []rejection
	)
	defer func() {
		if len(penalized) > 0 {
			s.refreshBestClientAfterPenalties(penalized)
		}
	}()

	for _, client := range activeClients {
		log := log.With().Str("client", client.Name()).Str("address", client.Address()).Logger()

		// A failure with the caller's context already done is not the client's fault.
		contextDone := ctx.Err() != nil
		s.clientScoresMu.RLock()
		penaltiesAtStart := s.clientPenalties[client.Address()]
		s.clientScoresMu.RUnlock()

		res, err = call(ctx, client)
		if err != nil {
			log.Trace().Err(err).Msg("Potentially failing over due to error")

			if errors.Is(err, context.Canceled) {
				log.Trace().Msg("Not failing over on canceled context")

				return res, err
			}

			// Any other error fails over, including the client's own timeout, so
			// requests move off a hung client.
			failover := true
			if errHandler != nil {
				failover, err = errHandler(ctx, client, err)
			}

			if !failover {
				// No failover required, return.
				return res, err
			}

			score := s.currentScore(client.Address())
			switch {
			case isUserError(err):
				// The request may be at fault, or this client, if it lags behind or
				// lacks the endpoint: penalize it only if another client serves the call.
				rejections = append(rejections, rejection{address: client.Address(), err: err, penaltiesAtStart: penaltiesAtStart})
			case !contextDone:
				score, penalized = s.penalizeFailure(client.Address(), penaltiesAtStart, penalized)
			default:
				// The caller's context was already done.
			}

			// Stop once the caller's context is done, as any further call would fail too.
			if stopErr := contextStopError(ctx, callError(err, rejections)); stopErr != nil {
				log.Debug().Err(err).Int("score", score).Msg("Not failing over: context done")

				return nil, stopErr
			}

			log.Debug().Err(err).Int("score", score).Msg("Failing over on error")

			continue
		}

		if res == nil {
			// No response from this client; try the next.
			err = errors.New("empty response")

			continue
		}

		// This client served the call, so the clients that rejected it were at fault.
		for _, rejected := range rejections {
			var score int
			score, penalized = s.penalizeFailure(rejected.address, rejected.penaltiesAtStart, penalized)
			s.log.Debug().Str("address", rejected.address).Int("score", score).
				Msg("Client rejected a call another client served")
		}

		return res, nil
	}

	return nil, callError(err, rejections)
}

// callError returns the error for a call no client served: the first user error,
// as callers act on it (a 404 for a missing block, say), or else the last error.
func callError(err error, rejections []rejection) error {
	if len(rejections) > 0 {
		return rejections[0].err
	}

	return err
}

// isUserError returns true if err is a 4xx response blaming the request: 408 and
// 429 instead report that the client could not serve it.
func isUserError(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || statusCodeFamily(apiErr.StatusCode) != 4 {
		return false
	}

	return apiErr.StatusCode != nethttp.StatusRequestTimeout && apiErr.StatusCode != nethttp.StatusTooManyRequests
}

// contextStopError returns nil while ctx is live.  Once it is done, it returns the
// error for doCall to stop with: err, wrapped with the context's error unless it
// already carries it.
func contextStopError(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	switch {
	case ctxErr == nil:
		return nil
	case errors.Is(err, ctxErr):
		return err
	default:
		return fmt.Errorf("%w; last error: %w", ctxErr, err)
	}
}

// providerInfo returns information on the provider.
// Currently this just returns the name of the service (lighthouse/teku/etc.).
func (*Service) providerInfo(ctx context.Context, provider consensusclient.Service) string {
	providerName := "<unknown>"

	nodeVersionProvider, isNodeVersionProvider := provider.(consensusclient.NodeVersionProvider)
	if isNodeVersionProvider {
		response, err := nodeVersionProvider.NodeVersion(ctx, &api.NodeVersionOpts{})
		if err == nil {
			switch {
			case strings.Contains(strings.ToLower(response.Data), "lighthouse"):
				providerName = "lighthouse"
			case strings.Contains(strings.ToLower(response.Data), "lodestar"):
				providerName = "lodestar"
			case strings.Contains(strings.ToLower(response.Data), "prysm"):
				providerName = "prysm"
			case strings.Contains(strings.ToLower(response.Data), "teku"):
				providerName = "teku"
			case strings.Contains(strings.ToLower(response.Data), "nimbus"):
				providerName = "nimbus"
			default:
				// Unknown provider, keep as "<unknown>"
			}
		}
	}

	return providerName
}

func statusCodeFamily(status int) int {
	return status / 100
}
