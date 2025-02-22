package services

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerwatch/erigon/cl/beacon/synced_data"
	"github.com/ledgerwatch/erigon/cl/clparams"
	"github.com/ledgerwatch/erigon/cl/cltypes/solid"
	"github.com/ledgerwatch/erigon/cl/phase1/core/state/lru"
	"github.com/ledgerwatch/erigon/cl/phase1/forkchoice"
	"github.com/ledgerwatch/erigon/cl/phase1/network/subnets"
	"github.com/ledgerwatch/erigon/cl/utils"
	"github.com/ledgerwatch/erigon/cl/utils/eth_clock"
	"github.com/ledgerwatch/erigon/cl/validator/committee_subscription"
)

var (
	computeSubnetForAttestation  = subnets.ComputeSubnetForAttestation
	computeCommitteeCountPerSlot = subnets.ComputeCommitteeCountPerSlot
)

type attestationService struct {
	forkchoiceStore    forkchoice.ForkChoiceStorage
	committeeSubscribe committee_subscription.CommitteeSubscribe
	ethClock           eth_clock.EthereumClock
	syncedDataManager  synced_data.SyncedData
	beaconCfg          *clparams.BeaconChainConfig
	netCfg             *clparams.NetworkConfig
	// validatorAttestationSeen maps from epoch to validator index. This is used to ignore duplicate validator attestations in the same epoch.
	validatorAttestationSeen *lru.CacheWithTTL[uint64, uint64] // validator index -> epoch
}

func NewAttestationService(
	forkchoiceStore forkchoice.ForkChoiceStorage,
	committeeSubscribe committee_subscription.CommitteeSubscribe,
	ethClock eth_clock.EthereumClock,
	syncedDataManager synced_data.SyncedData,
	beaconCfg *clparams.BeaconChainConfig,
	netCfg *clparams.NetworkConfig,
) AttestationService {
	epochDuration := beaconCfg.SlotsPerEpoch * beaconCfg.SecondsPerSlot
	return &attestationService{
		forkchoiceStore:          forkchoiceStore,
		committeeSubscribe:       committeeSubscribe,
		ethClock:                 ethClock,
		syncedDataManager:        syncedDataManager,
		beaconCfg:                beaconCfg,
		netCfg:                   netCfg,
		validatorAttestationSeen: lru.NewWithTTL[uint64, uint64]("validator_attestation_seen", validatorAttestationCacheSize, time.Duration(epochDuration)),
	}
}

func (s *attestationService) ProcessMessage(ctx context.Context, subnet *uint64, att *solid.Attestation) error {
	var (
		root           = att.AttestantionData().BeaconBlockRoot()
		slot           = att.AttestantionData().Slot()
		committeeIndex = att.AttestantionData().CommitteeIndex()
		targetEpoch    = att.AttestantionData().Target().Epoch()
	)
	headState := s.syncedDataManager.HeadState()
	if headState == nil {
		return ErrIgnore
	}

	// [REJECT] The committee index is within the expected range
	committeeCount := computeCommitteeCountPerSlot(headState, slot, s.beaconCfg.SlotsPerEpoch)
	if committeeIndex >= committeeCount {
		return fmt.Errorf("committee index out of range")
	}
	// [REJECT] The attestation is for the correct subnet -- i.e. compute_subnet_for_attestation(committees_per_slot, attestation.data.slot, index) == subnet_id
	subnetId := computeSubnetForAttestation(committeeCount, slot, committeeIndex, s.beaconCfg.SlotsPerEpoch, s.netCfg.AttestationSubnetCount)
	if subnet == nil || subnetId != *subnet {
		return fmt.Errorf("wrong subnet")
	}
	// [IGNORE] attestation.data.slot is within the last ATTESTATION_PROPAGATION_SLOT_RANGE slots (within a MAXIMUM_GOSSIP_CLOCK_DISPARITY allowance) --
	// i.e. attestation.data.slot + ATTESTATION_PROPAGATION_SLOT_RANGE >= current_slot >= attestation.data.slot (a client MAY queue future attestations for processing at the appropriate slot).
	currentSlot := s.ethClock.GetCurrentSlot()
	if currentSlot < slot || currentSlot > slot+s.netCfg.AttestationPropagationSlotRange {
		return fmt.Errorf("not in propagation range %w", ErrIgnore)
	}
	// [REJECT] The attestation's epoch matches its target -- i.e. attestation.data.target.epoch == compute_epoch_at_slot(attestation.data.slot)
	if targetEpoch != slot/s.beaconCfg.SlotsPerEpoch {
		return fmt.Errorf("epoch mismatch")
	}
	// [REJECT] The number of aggregation bits matches the committee size -- i.e. len(aggregation_bits) == len(get_beacon_committee(state, attestation.data.slot, index)).
	beaconCommittee, err := s.forkchoiceStore.GetBeaconCommitee(slot, committeeIndex)
	if err != nil {
		return err
	}
	bits := att.AggregationBits()
	expectedAggregationBitsLength := len(beaconCommittee)
	actualAggregationBitsLength := utils.GetBitlistLength(bits)
	if actualAggregationBitsLength != expectedAggregationBitsLength {
		return fmt.Errorf("aggregation bits count mismatch: %d != %d", actualAggregationBitsLength, expectedAggregationBitsLength)
	}

	//[REJECT] The attestation is unaggregated -- that is, it has exactly one participating validator (len([bit for bit in aggregation_bits if bit]) == 1, i.e. exactly 1 bit is set).
	setBits := 0
	onBitIndex := 0 // Aggregationbits is []byte, so we need to iterate over all bits.
	for i := 0; i < len(bits); i++ {
		for j := 0; j < 8; j++ {
			if bits[i]&(1<<uint(j)) != 0 {
				if i*8+j >= len(beaconCommittee) {
					continue
				}
				setBits++
				onBitIndex = i*8 + j
			}
		}
	}

	if setBits == 0 {
		return ErrIgnore // Ignore if it is just an empty bitlist
	}
	if setBits != 1 {
		return fmt.Errorf("attestation does not have exactly one participating validator")
	}

	// [IGNORE] There has been no other valid attestation seen on an attestation subnet that has an identical attestation.data.target.epoch and participating validator index.
	if err != nil {
		return err
	}
	if onBitIndex >= len(beaconCommittee) {
		return fmt.Errorf("on bit index out of committee range")
	}
	// mark the validator as seen
	vIndex := beaconCommittee[onBitIndex]
	epochLastTime, ok := s.validatorAttestationSeen.Get(vIndex)
	if ok && epochLastTime == targetEpoch {
		return fmt.Errorf("validator already seen in target epoch %w", ErrIgnore)
	}
	s.validatorAttestationSeen.Add(vIndex, targetEpoch)

	// [IGNORE] The block being voted for (attestation.data.beacon_block_root) has been seen (via both gossip and non-gossip sources)
	// (a client MAY queue attestations for processing once block is retrieved).
	if _, ok := s.forkchoiceStore.GetHeader(root); !ok {
		return ErrIgnore
	}

	// [REJECT] The attestation's target block is an ancestor of the block named in the LMD vote -- i.e.
	// get_checkpoint_block(store, attestation.data.beacon_block_root, attestation.data.target.epoch) == attestation.data.target.root
	startSlotAtEpoch := targetEpoch * s.beaconCfg.SlotsPerEpoch
	if s.forkchoiceStore.Ancestor(root, startSlotAtEpoch) != att.AttestantionData().Target().BlockRoot() {
		return fmt.Errorf("invalid target block")
	}
	// [IGNORE] The current finalized_checkpoint is an ancestor of the block defined by attestation.data.beacon_block_root --
	// i.e. get_checkpoint_block(store, attestation.data.beacon_block_root, store.finalized_checkpoint.epoch) == store.finalized_checkpoint.root
	startSlotAtEpoch = s.forkchoiceStore.FinalizedCheckpoint().Epoch() * s.beaconCfg.SlotsPerEpoch
	if s.forkchoiceStore.Ancestor(root, startSlotAtEpoch) != s.forkchoiceStore.FinalizedCheckpoint().BlockRoot() {
		return fmt.Errorf("invalid finalized checkpoint %w", ErrIgnore)
	}

	return s.committeeSubscribe.CheckAggregateAttestation(att)
}
