// Copyright © 2021 Attestant Limited.
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

package multi_test

import (
	"context"
	"github.com/attestantio/go-eth2-client/api"
	"github.com/attestantio/go-eth2-client/spec"
	"testing"

	consensusclient "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/mock"
	"github.com/attestantio/go-eth2-client/multi"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/attestantio/go-eth2-client/testclients"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestSubmitAttestations(t *testing.T) {
	ctx := context.Background()

	client1, err := mock.New(ctx, mock.WithName("mock 1"))
	require.NoError(t, err)
	erroringClient1, err := testclients.NewErroring(ctx, 0.1, client1)
	require.NoError(t, err)
	client2, err := mock.New(ctx, mock.WithName("mock 2"))
	require.NoError(t, err)
	erroringClient2, err := testclients.NewErroring(ctx, 0.1, client2)
	require.NoError(t, err)
	client3, err := mock.New(ctx, mock.WithName("mock 3"))
	require.NoError(t, err)

	multiClient, err := multi.New(ctx,
		multi.WithLogLevel(zerolog.Disabled),
		multi.WithClients([]consensusclient.Service{
			erroringClient1,
			erroringClient2,
			client3,
		}),
	)
	require.NoError(t, err)

	versionedAttestations := []*spec.VersionedAttestation{
		{Version: spec.DataVersionPhase0, Phase0: &phase0.Attestation{}},
	}
	opts := &api.SubmitAttestationsOpts{
		Attestations: versionedAttestations,
	}
	for i := 0; i < 128; i++ {
		err := multiClient.(consensusclient.AttestationsSubmitter).SubmitAttestations(ctx, opts)
		require.NoError(t, err)
	}
	// At this point we expect mock 3 to be in active (unless probability hates us).
	require.Equal(t, "mock 3", multiClient.Address())
}

// lighthouseClient is a Lighthouse client whose attestation submissions fail with
// err, or succeed if it is nil.
type lighthouseClient struct {
	*mock.Service

	err         error
	submissions int
}

func newLighthouseClient(t *testing.T, name string, err error) *lighthouseClient {
	t.Helper()

	client, mockErr := mock.New(context.Background(), mock.WithName(name))
	require.NoError(t, mockErr)
	client.NodeVersionFunc = func(context.Context, *api.NodeVersionOpts) (*api.Response[string], error) {
		return &api.Response[string]{Data: "Lighthouse/v8.0.0"}, nil
	}

	return &lighthouseClient{Service: client, err: err}
}

func (c *lighthouseClient) SubmitAttestations(context.Context, *api.SubmitAttestationsOpts) error {
	c.submissions++

	return c.err
}

// submitToLighthouseClients submits attestations through a multi client over the
// given clients, which it tries in the order given.
func submitToLighthouseClients(t *testing.T, clients ...*lighthouseClient) error {
	t.Helper()

	ctx := context.Background()

	services := make([]consensusclient.Service, 0, len(clients))
	for _, client := range clients {
		services = append(services, client)
	}
	multiClient, err := multi.New(ctx, multi.WithLogLevel(zerolog.Disabled), multi.WithClients(services))
	require.NoError(t, err)

	return multiClient.(consensusclient.AttestationsSubmitter).SubmitAttestations(ctx, &api.SubmitAttestationsOpts{})
}

// TestSubmitAttestationsFailsOverOnUnknownHeadBlock checks that Lighthouse
// rejecting an attestation to a head block it does not know, with a 400, fails
// over: the node is behind, rather than the attestation at fault.
func TestSubmitAttestationsFailsOverOnUnknownHeadBlock(t *testing.T) {
	behind := newLighthouseClient(t, "behind", &api.Error{
		Method:     "POST",
		StatusCode: 400,
		Data:       []byte(`{"code":400,"message":"UnknownHeadBlock"}`),
	})
	synced := newLighthouseClient(t, "synced", nil)

	require.NoError(t, submitToLighthouseClients(t, behind, synced))
	require.Equal(t, 1, behind.submissions)
	require.Equal(t, 1, synced.submissions)
}

// TestSubmitAttestationsDoesNotFailOverOnPriorAttestationKnown checks that
// Lighthouse rejecting an attestation it already knows, with a 400, is returned
// without trying another client.
func TestSubmitAttestationsDoesNotFailOverOnPriorAttestationKnown(t *testing.T) {
	knowing := newLighthouseClient(t, "knowing", &api.Error{
		Method:     "POST",
		StatusCode: 400,
		Data:       []byte(`{"code":400,"message":"PriorAttestationKnown"}`),
	})
	other := newLighthouseClient(t, "other", nil)

	require.ErrorContains(t, submitToLighthouseClients(t, knowing, other), "PriorAttestationKnown")
	require.Equal(t, 1, knowing.submissions)
	require.Zero(t, other.submissions)
}
