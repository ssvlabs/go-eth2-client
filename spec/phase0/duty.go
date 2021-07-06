package phase0

// AttesterDuty is the data regarding the duty type with the duty data
type Duty struct {
	// Type is the duty type (attest, propose)
	Type DutyType
	// PubKey is the public key of the validator that should attest.
	PubKey BLSPubKey
	// Slot is the slot in which the validator should attest.
	Slot Slot
	// ValidatorIndex is the index of the validator that should attest.
	ValidatorIndex ValidatorIndex
	// CommitteeIndex is the index of the committee in which the attesting validator has been placed.
	CommitteeIndex CommitteeIndex
	// CommitteeLength is the length of the committee in which the attesting validator has been placed.
	CommitteeLength uint64
	// CommitteesAtSlot is the number of committees in the slot.
	CommitteesAtSlot uint64
	// ValidatorCommitteeIndex is the index of the validator in the list of validators in the committee.
	ValidatorCommitteeIndex uint64
}

// dutyJSON is the standard API representation of the struct.
type dutyJSON struct {
	PubKey                  string `json:"pubkey"`
	Slot                    string `json:"slot"`
	ValidatorIndex          string `json:"validator_index"`
	CommitteeIndex          string `json:"committee_index"`
	CommitteeLength         string `json:"committee_length"`
	CommitteesAtSlot        string `json:"committees_at_slot"`
	ValidatorCommitteeIndex string `json:"validator_committee_index"`
}