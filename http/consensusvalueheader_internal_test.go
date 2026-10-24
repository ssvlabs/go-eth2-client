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

package http

import (
	"math/big"
	"testing"

	"github.com/attestantio/go-eth2-client/api"
	"github.com/stretchr/testify/require"
)

// TestProposalConsensusValueHeader checks that an empty Eth-Consensus-Block-Value
// header keeps the zero default, while other values are parsed or rejected as
// before.
func TestProposalConsensusValueHeader(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected *big.Int
		err      string
	}{
		{
			name:     "Empty",
			value:    "",
			expected: big.NewInt(0),
		},
		{
			name:     "Valid",
			value:    "3476149000000000",
			expected: big.NewInt(3476149000000000),
		},
		{
			name:  "Invalid",
			value: "lots",
			err:   "proposal header Eth-Consensus-Block-Value lots not a valid integer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &api.Response[*api.VersionedProposal]{
				Data: &api.VersionedProposal{
					ConsensusValue: big.NewInt(0),
					ExecutionValue: big.NewInt(0),
				},
			}

			err := (&Service{}).populateProposalDataFromHeaders(response, map[string]string{
				"Eth-Consensus-Block-Value": test.value,
			})
			if test.err != "" {
				require.EqualError(t, err, test.err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.expected, response.Data.ConsensusValue)
		})
	}
}

// TestEPBSProposalConsensusValueHeader checks that an empty
// Eth-Consensus-Block-Value header leaves a Gloas proposal's consensus value
// unknown, while other values are parsed or rejected as before.
func TestEPBSProposalConsensusValueHeader(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected *big.Int
		err      string
	}{
		{
			name:  "Empty",
			value: "",
		},
		{
			name:     "Valid",
			value:    "3476149000000000",
			expected: big.NewInt(3476149000000000),
		},
		{
			name:  "Invalid",
			value: "lots",
			err:   "proposal header Eth-Consensus-Block-Value lots not a valid integer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal := &api.VersionedEPBSProposal{}

			err := populateEPBSProposalValuesFromHeaders(proposal, map[string]string{
				"Eth-Consensus-Block-Value":   test.value,
				"Eth-Execution-Payload-Value": "5",
			})
			if test.err != "" {
				require.EqualError(t, err, test.err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, test.expected, proposal.ConsensusValue)
			require.Equal(t, big.NewInt(5), proposal.ExecutionValue)
		})
	}
}
