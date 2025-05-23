// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package allocatorimpl

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator/load"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/allocator/storepool"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/constraint"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvflowcontrol/rac2"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/raftutil"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/raft/tracker"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

const (
	// minReplicaWeight sets a floor for how low a replica weight can be. This is
	// needed because a weight of zero doesn't work in the current lease scoring
	// algorithm.
	minReplicaWeight = 0.001
)

// LeaseRebalanceThreshold is the minimum ratio of a store's lease surplus
// to the mean range/lease count that permits lease-transfers away from that
// store.
// Made configurable for the sake of testing.
var LeaseRebalanceThreshold = settings.RegisterFloatSetting(
	settings.SystemOnly,
	"kv.allocator.lease_rebalance_threshold",
	"minimum fraction away from the mean a store's lease count can be before "+
		"it is considered for lease-transfers",
	0.05,
	settings.WithPublic)

// LeaseRebalanceThresholdMin is the absolute number of leases above/below the
// mean lease count that a store can have before considered overfull/underfull.
// Made configurable for the sake of testing.
var LeaseRebalanceThresholdMin = 5.0

// getBaseLoadBasedLeaseRebalanceThreshold returns the equivalent of
// LeaseRebalanceThreshold for load-based lease rebalance decisions (i.e.
// "follow-the-workload"). It's the base threshold for decisions that get
// adjusted based on the load and latency of the involved ranges/nodes.
func getBaseLoadBasedLeaseRebalanceThreshold(leaseRebalanceThreshold float64) float64 {
	return 2 * leaseRebalanceThreshold
}

// MinLeaseTransferStatsDuration configures the minimum amount of time a
// replica must wait for stats about request counts to accumulate before
// making decisions based on them. The higher this is, the less likely
// thrashing is (up to a point).
// Made configurable for the sake of testing.
var MinLeaseTransferStatsDuration = 30 * time.Second

// EnableLoadBasedLeaseRebalancing controls whether lease rebalancing is done
// via the new heuristic based on request load and latency or via the simpler
// approach that purely seeks to balance the number of leases per node evenly.
var EnableLoadBasedLeaseRebalancing = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"kv.allocator.load_based_lease_rebalancing.enabled",
	"set to enable rebalancing of range leases based on load and latency",
	true,
	settings.WithPublic)

// leaseRebalancingAggressiveness enables users to tweak how aggressive their
// cluster is at moving leases towards the localities where the most requests
// are coming from. Settings lower than 1.0 will make the system less
// aggressive about moving leases toward requests than the default, while
// settings greater than 1.0 will cause more aggressive placement.
//
// Setting this to 0 effectively disables load-based lease rebalancing, and
// settings less than 0 are disallowed.
var leaseRebalancingAggressiveness = settings.RegisterFloatSetting(
	settings.SystemOnly,
	"kv.allocator.lease_rebalancing_aggressiveness",
	"set greater than 1.0 to rebalance leases toward load more aggressively, "+
		"or between 0 and 1.0 to be more conservative about rebalancing leases",
	1.0,
	settings.NonNegativeFloat,
)

// recoveryStoreSelector controls the strategy for choosing a store to recover
// replicas to: either to any valid store ("good") or to a store that has low
// range count ("best"). With this set to "good", recovering from a dead node or
// from a decommissioning node can be faster, because nodes can send replicas to
// more target stores (instead of multiple nodes sending replicas to a few
// stores with a low range count).
var recoveryStoreSelector = settings.RegisterStringSetting(
	settings.SystemOnly,
	"kv.allocator.recovery_store_selector",
	"if set to 'good', the allocator may recover replicas to any valid store, if set "+
		"to 'best' it will pick one of the most ideal stores",
	"good",
)

// AllocatorAction enumerates the various replication adjustments that may be
// recommended by the allocator.
type AllocatorAction int

// These are the possible allocator actions.
const (
	_ AllocatorAction = iota
	AllocatorNoop
	AllocatorRemoveVoter
	AllocatorRemoveNonVoter
	AllocatorAddVoter
	AllocatorAddNonVoter
	AllocatorReplaceDeadVoter
	AllocatorReplaceDeadNonVoter
	AllocatorRemoveDeadVoter
	AllocatorRemoveDeadNonVoter
	AllocatorReplaceDecommissioningVoter
	AllocatorReplaceDecommissioningNonVoter
	AllocatorRemoveDecommissioningVoter
	AllocatorRemoveDecommissioningNonVoter
	AllocatorRemoveLearner
	AllocatorConsiderRebalance
	AllocatorRangeUnavailable
	AllocatorFinalizeAtomicReplicationChange
)

// Add indicates an action adding a replica.
func (a AllocatorAction) Add() bool {
	return a == AllocatorAddVoter || a == AllocatorAddNonVoter
}

// Replace indicates an action replacing a dead or decommissioning replica.
func (a AllocatorAction) Replace() bool {
	return a == AllocatorReplaceDeadVoter ||
		a == AllocatorReplaceDeadNonVoter ||
		a == AllocatorReplaceDecommissioningVoter ||
		a == AllocatorReplaceDecommissioningNonVoter
}

// Remove indicates an action removing a replica, i.e. in overreplication cases.
func (a AllocatorAction) Remove() bool {
	return a == AllocatorRemoveVoter ||
		a == AllocatorRemoveNonVoter ||
		a == AllocatorRemoveDeadVoter ||
		a == AllocatorRemoveDeadNonVoter ||
		a == AllocatorRemoveDecommissioningVoter ||
		a == AllocatorRemoveDecommissioningNonVoter
}

// TargetReplicaType returns that the action is for a voter or non-voter replica.
func (a AllocatorAction) TargetReplicaType() TargetReplicaType {
	var t TargetReplicaType
	if a == AllocatorRemoveVoter ||
		a == AllocatorAddVoter ||
		a == AllocatorReplaceDeadVoter ||
		a == AllocatorRemoveDeadVoter ||
		a == AllocatorReplaceDecommissioningVoter ||
		a == AllocatorRemoveDecommissioningVoter {
		t = VoterTarget
	} else if a == AllocatorRemoveNonVoter ||
		a == AllocatorAddNonVoter ||
		a == AllocatorReplaceDeadNonVoter ||
		a == AllocatorRemoveDeadNonVoter ||
		a == AllocatorReplaceDecommissioningNonVoter ||
		a == AllocatorRemoveDecommissioningNonVoter {
		t = NonVoterTarget
	}
	return t
}

// ReplicaStatus returns that the action is due to a live, dead, or
// decommissioning replica.
func (a AllocatorAction) ReplicaStatus() ReplicaStatus {
	var s ReplicaStatus
	if a == AllocatorRemoveVoter ||
		a == AllocatorRemoveNonVoter ||
		a == AllocatorAddVoter ||
		a == AllocatorAddNonVoter {
		s = Alive
	} else if a == AllocatorReplaceDeadVoter ||
		a == AllocatorReplaceDeadNonVoter ||
		a == AllocatorRemoveDeadVoter ||
		a == AllocatorRemoveDeadNonVoter {
		s = Dead
	} else if a == AllocatorReplaceDecommissioningVoter ||
		a == AllocatorReplaceDecommissioningNonVoter ||
		a == AllocatorRemoveDecommissioningVoter ||
		a == AllocatorRemoveDecommissioningNonVoter {
		s = Decommissioning
	}
	return s
}

var allocatorActionNames = map[AllocatorAction]string{
	AllocatorNoop:                            "noop",
	AllocatorRemoveVoter:                     "remove voter",
	AllocatorRemoveNonVoter:                  "remove non-voter",
	AllocatorAddVoter:                        "add voter",
	AllocatorAddNonVoter:                     "add non-voter",
	AllocatorReplaceDeadVoter:                "replace dead voter",
	AllocatorReplaceDeadNonVoter:             "replace dead non-voter",
	AllocatorRemoveDeadVoter:                 "remove dead voter",
	AllocatorRemoveDeadNonVoter:              "remove dead non-voter",
	AllocatorReplaceDecommissioningVoter:     "replace decommissioning voter",
	AllocatorReplaceDecommissioningNonVoter:  "replace decommissioning non-voter",
	AllocatorRemoveDecommissioningVoter:      "remove decommissioning voter",
	AllocatorRemoveDecommissioningNonVoter:   "remove decommissioning non-voter",
	AllocatorRemoveLearner:                   "remove learner",
	AllocatorConsiderRebalance:               "consider rebalance",
	AllocatorRangeUnavailable:                "range unavailable",
	AllocatorFinalizeAtomicReplicationChange: "finalize conf change",
}

func (a AllocatorAction) String() string {
	return allocatorActionNames[a]
}

// SafeValue implements the redact.SafeValue interface.
func (a AllocatorAction) SafeValue() {}

// Priority defines the priorities for various repair operations.
//
// NB: These priorities only influence the replicateQueue's understanding of
// which ranges are to be dealt with before others. In other words, these
// priorities don't influence the relative order of actions taken on a given
// range. Within a given range, the ordering of the various checks inside
// `Allocator.computeAction` determines which repair/rebalancing actions are
// taken before the others.
func (a AllocatorAction) Priority() float64 {
	switch a {
	case AllocatorFinalizeAtomicReplicationChange:
		return 12002
	case AllocatorRemoveLearner:
		return 12001
	case AllocatorReplaceDeadVoter:
		return 12000
	case AllocatorAddVoter:
		return 10000
	case AllocatorReplaceDecommissioningVoter:
		return 5000
	case AllocatorRemoveDeadVoter:
		return 1000
	case AllocatorRemoveDecommissioningVoter:
		return 900
	case AllocatorRemoveVoter:
		return 800
	case AllocatorReplaceDeadNonVoter:
		return 700
	case AllocatorAddNonVoter:
		return 600
	case AllocatorReplaceDecommissioningNonVoter:
		return 500
	case AllocatorRemoveDeadNonVoter:
		return 400
	case AllocatorRemoveDecommissioningNonVoter:
		return 300
	case AllocatorRemoveNonVoter:
		return 200
	case AllocatorConsiderRebalance, AllocatorRangeUnavailable, AllocatorNoop:
		return 0
	default:
		panic(fmt.Sprintf("unknown AllocatorAction: %s", a))
	}
}

// TargetReplicaType indicates whether the target replica is a voter or
// non-voter.
type TargetReplicaType int

const (
	_ TargetReplicaType = iota
	// VoterTarget represents a voting target replica.
	VoterTarget
	// NonVoterTarget represents a non-voting target replica.
	NonVoterTarget
)

// ReplicaStatus represents whether a replica is currently alive,
// dead or decommissioning.
type ReplicaStatus int

const (
	_ ReplicaStatus = iota
	// Alive represents a replica on a live node.
	Alive
	// Dead represents a replica on a dead node.
	Dead
	// Decommissioning represents a replica on a dead node.
	Decommissioning
)

// AddChangeType returns the roachpb.ReplicaChangeType corresponding to the
// given targetReplicaType.
//
// TODO(aayush): Clean up usages of ADD_{NON_}VOTER. Use
// targetReplicaType.{Add,Remove}ChangeType methods wherever possible.
func (t TargetReplicaType) AddChangeType() roachpb.ReplicaChangeType {
	switch t {
	case VoterTarget:
		return roachpb.ADD_VOTER
	case NonVoterTarget:
		return roachpb.ADD_NON_VOTER
	default:
		panic(fmt.Sprintf("unknown targetReplicaType %d", t))
	}
}

// RemoveChangeType returns the roachpb.ReplicaChangeType corresponding to the
// given targetReplicaType.
func (t TargetReplicaType) RemoveChangeType() roachpb.ReplicaChangeType {
	switch t {
	case VoterTarget:
		return roachpb.REMOVE_VOTER
	case NonVoterTarget:
		return roachpb.REMOVE_NON_VOTER
	default:
		panic(fmt.Sprintf("unknown targetReplicaType %d", t))
	}
}

func (t TargetReplicaType) String() string {
	switch t {
	case VoterTarget:
		return "voter"
	case NonVoterTarget:
		return "non-voter"
	default:
		panic(fmt.Sprintf("unknown targetReplicaType %d", t))
	}
}

// SafeValue implements the redact.SafeValue interface.
func (t TargetReplicaType) SafeValue() {}

func (s ReplicaStatus) String() string {
	switch s {
	case Alive:
		return "live"
	case Dead:
		return "dead"
	case Decommissioning:
		return "decommissioning"
	default:
		panic(fmt.Sprintf("unknown replicaStatus %d", s))
	}
}

// SafeValue implements the redact.SafeValue interface.
func (t ReplicaStatus) SafeValue() {}

type accessLocalityTransferDecision int

const (
	_ accessLocalityTransferDecision = iota
	shouldTransfer
	shouldNotTransfer
	decideWithoutStats
)

// allocatorError indicates a retryable error condition which sends replicas
// being processed through the replicate_queue into purgatory so that they
// can be retried quickly as soon as new stores come online, or additional
// space frees up.
type allocatorError struct {
	constraints           []roachpb.ConstraintsConjunction
	voterConstraints      []roachpb.ConstraintsConjunction
	existingVoterCount    int
	existingNonVoterCount int
	aliveStores           int
	throttledStores       int
	fullStores            int
}

var _ errors.SafeFormatter = &allocatorError{}

func (ae *allocatorError) Error() string {
	return redact.Sprint(ae).StripMarkers()
}

func (ae *allocatorError) SafeFormatError(p errors.Printer) (next error) {
	var existingVoterStr redact.RedactableString
	if ae.existingVoterCount == 1 {
		existingVoterStr = "1 already has a voter"
	} else {
		existingVoterStr = redact.Sprintf("%d already have a voter",
			ae.existingVoterCount)
	}

	var existingNonVoterStr redact.RedactableString
	if ae.existingNonVoterCount == 1 {
		existingNonVoterStr = "1 already has a non-voter"
	} else {
		existingNonVoterStr = redact.Sprintf("%d already have a non-voter",
			ae.existingNonVoterCount)
	}

	var throttledMessage redact.RedactableString
	if ae.throttledStores != 0 {
		throttledMessage = redact.Sprintf("%d throttled, ", ae.throttledStores)
	}

	var fullStoreMessage redact.RedactableString
	if ae.fullStores != 0 {
		fullStoreMessage = redact.Sprintf("%d full disk, ", ae.fullStores)
	}

	baseMsg := redact.Sprintf(
		"0 of %d live stores are able to take a new replica for the range (%s%s%s, %s)",
		ae.aliveStores, throttledMessage, fullStoreMessage,
		existingVoterStr, existingNonVoterStr)

	if len(ae.constraints) == 0 && len(ae.voterConstraints) == 0 {
		p.Print(baseMsg)
		if ae.throttledStores == 0 {
			p.Printf("; likely not enough nodes in cluster")
		}
		return
	}

	var b redact.StringBuilder
	b.Print(baseMsg)
	b.Printf("; replicas must match constraints [")
	for i := range ae.constraints {
		if i > 0 {
			b.SafeRune(' ')
		}
		b.SafeRune('{')
		b.Print(ae.constraints[i])
		b.SafeRune('}')
	}
	b.SafeRune(']')

	b.Printf("; voting replicas must match voter_constraints [")
	for i := range ae.voterConstraints {
		if i > 0 {
			b.SafeRune(' ')
		}
		b.SafeRune('{')
		b.Print(ae.voterConstraints[i].String())
		b.SafeRune('}')
	}
	b.SafeRune(']')

	p.Print(b)
	return nil
}

func (*allocatorError) AllocationErrorMarker() {}
func (*allocatorError) PurgatoryErrorMarker()  {}

// allocatorRand pairs a rand.Rand with a mutex.
// NOTE: Allocator is typically only accessed from a single thread (the
// replication queue), but this assumption is broken in tests which force
// replication scans. If those tests can be modified to suspend the normal
// replication queue during the forced scan, then this rand could be used
// without a mutex.
type allocatorRand struct {
	*syncutil.Mutex
	*rand.Rand
}

func makeAllocatorRand(source rand.Source) allocatorRand {
	return allocatorRand{
		Mutex: &syncutil.Mutex{},
		Rand:  rand.New(source),
	}
}

var (
	// Load-based lease transfers.
	metaLBLeaseTransferCannotFindBetterCandidate = metric.Metadata{
		Name: "kv.allocator.load_based_lease_transfers.cannot_find_better_candidate",
		Help: "The number times the allocator determined that the lease was on the best" +
			" possible replica",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBLeaseTransferExistingNotOverfull = metric.Metadata{
		Name: "kv.allocator.load_based_lease_transfers.existing_not_overfull",
		Help: "The number times the allocator determined that the lease was not on an" +
			" overfull store",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBLeaseTransferDeltaNotSignificant = metric.Metadata{
		Name: "kv.allocator.load_based_lease_transfers.delta_not_significant",
		Help: "The number times the allocator determined that the delta between the existing" +
			" store and the best candidate was not significant",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBLeaseTransferMissingStatsForExistingStore = metric.Metadata{
		Name:        "kv.allocator.load_based_lease_transfers.missing_stats_for_existing_stores",
		Help:        "The number times the allocator was missing qps stats for the leaseholder",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBLeaseTransferShouldTransfer = metric.Metadata{
		Name: "kv.allocator.load_based_lease_transfers.should_transfer",
		Help: "The number times the allocator determined that the lease should be" +
			" transferred to another replica for better load distribution",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBLeaseTransferFollowTheWorkload = metric.Metadata{
		Name: "kv.allocator.load_based_lease_transfers.follow_the_workload",
		Help: "The number times the allocator determined that the lease should be" +
			" transferred to another replica for locality.",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}

	// Load-based replica rebalances.
	metaLBReplicaRebalancingCannotFindBetterCandidate = metric.Metadata{
		Name: "kv.allocator.load_based_replica_rebalancing.cannot_find_better_candidate",
		Help: "The number times the allocator determined that the range was on the best" +
			" possible stores",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBReplicaRebalancingExistingNotOverfull = metric.Metadata{
		Name: "kv.allocator.load_based_replica_rebalancing.existing_not_overfull",
		Help: "The number times the allocator determined that none of the range's replicas" +
			" were on overfull stores",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBReplicaRebalancingDeltaNotSignificant = metric.Metadata{
		Name: "kv.allocator.load_based_replica_rebalancing.delta_not_significant",
		Help: "The number times the allocator determined that the delta between an" +
			" existing store and the best replacement candidate was not high enough",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBReplicaRebalancingMissingStatsForExistingStore = metric.Metadata{
		Name:        "kv.allocator.load_based_replica_rebalancing.missing_stats_for_existing_store",
		Help:        "The number times the allocator was missing the qps stats for the existing store",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
	metaLBReplicaRebalancingShouldTransfer = metric.Metadata{
		Name: "kv.allocator.load_based_replica_rebalancing.should_transfer",
		Help: "The number times the allocator determined that the replica should be" +
			" rebalanced to another store for better load distribution",
		Measurement: "Attempts",
		Unit:        metric.Unit_COUNT,
	}
)

type loadBasedLeaseTransferMetrics struct {
	CannotFindBetterCandidate    *metric.Counter
	ExistingNotOverfull          *metric.Counter
	DeltaNotSignificant          *metric.Counter
	MissingStatsForExistingStore *metric.Counter
	ShouldTransfer               *metric.Counter
	FollowTheWorkload            *metric.Counter
}

type loadBasedReplicaRebalanceMetrics struct {
	CannotFindBetterCandidate    *metric.Counter
	ExistingNotOverfull          *metric.Counter
	DeltaNotSignificant          *metric.Counter
	MissingStatsForExistingStore *metric.Counter
	ShouldRebalance              *metric.Counter
}

// AllocatorMetrics capture metrics about the allocator's decisions.
type AllocatorMetrics struct {
	LoadBasedLeaseTransferMetrics    loadBasedLeaseTransferMetrics
	LoadBasedReplicaRebalanceMetrics loadBasedReplicaRebalanceMetrics
}

// Allocator tries to spread replicas as evenly as possible across the stores
// in the cluster.
type Allocator struct {
	st            *cluster.Settings
	deterministic bool
	nodeLatencyFn func(nodeID roachpb.NodeID) (time.Duration, bool)
	// TODO(aayush): Let's replace this with a *rand.Rand that has a rand.Source
	// wrapped inside a mutex, to avoid misuse.
	randGen allocatorRand
	Metrics AllocatorMetrics

	knobs *allocator.TestingKnobs
}

func makeAllocatorMetrics() AllocatorMetrics {
	return AllocatorMetrics{
		LoadBasedLeaseTransferMetrics: loadBasedLeaseTransferMetrics{
			CannotFindBetterCandidate:    metric.NewCounter(metaLBLeaseTransferCannotFindBetterCandidate),
			ExistingNotOverfull:          metric.NewCounter(metaLBLeaseTransferExistingNotOverfull),
			DeltaNotSignificant:          metric.NewCounter(metaLBLeaseTransferDeltaNotSignificant),
			MissingStatsForExistingStore: metric.NewCounter(metaLBLeaseTransferMissingStatsForExistingStore),
			ShouldTransfer:               metric.NewCounter(metaLBLeaseTransferShouldTransfer),
			FollowTheWorkload:            metric.NewCounter(metaLBLeaseTransferFollowTheWorkload),
		},
		LoadBasedReplicaRebalanceMetrics: loadBasedReplicaRebalanceMetrics{
			CannotFindBetterCandidate:    metric.NewCounter(metaLBReplicaRebalancingCannotFindBetterCandidate),
			ExistingNotOverfull:          metric.NewCounter(metaLBReplicaRebalancingExistingNotOverfull),
			DeltaNotSignificant:          metric.NewCounter(metaLBReplicaRebalancingDeltaNotSignificant),
			MissingStatsForExistingStore: metric.NewCounter(metaLBReplicaRebalancingMissingStatsForExistingStore),
			ShouldRebalance:              metric.NewCounter(metaLBReplicaRebalancingShouldTransfer),
		},
	}
}

// MakeAllocator creates a new allocator.
// The deterministic flag indicates that this allocator is intended to be used
// with a deterministic store pool.
//
// In test cases where the store pool is nil, deterministic should be false.
// TODO(sarkesian): Eliminate the need for this flag, which is a remnant of
// close coupling with the StorePool.
func MakeAllocator(
	st *cluster.Settings,
	deterministic bool,
	nodeLatencyFn func(nodeID roachpb.NodeID) (time.Duration, bool),
	knobs *allocator.TestingKnobs,
) Allocator {
	var randSource rand.Source
	if deterministic {
		randSource = rand.NewSource(777)
	} else {
		randSource = rand.NewSource(rand.Int63())
	}
	allocator := Allocator{
		st:            st,
		deterministic: deterministic,
		nodeLatencyFn: nodeLatencyFn,
		randGen:       makeAllocatorRand(randSource),
		Metrics:       makeAllocatorMetrics(),
		knobs:         knobs,
	}
	return allocator
}

// GetNeededVoters calculates the number of voters a range should have given its
// zone config and the number of nodes available for up-replication (i.e. not
// decommissioning).
func GetNeededVoters(zoneConfigVoterCount int32, clusterNodes int) int {
	numZoneReplicas := int(zoneConfigVoterCount)
	need := numZoneReplicas

	// Adjust the replication factor for all ranges if there are fewer
	// nodes than replicas specified in the zone config, so the cluster
	// can still function.
	if clusterNodes < need {
		need = clusterNodes
	}

	// Ensure that we don't up- or down-replicate to an even number of replicas
	// unless an even number of replicas was specifically requested by the user
	// in the zone config.
	//
	// Note that in the case of 5 desired replicas and a decommissioning store,
	// this prefers down-replicating from 5 to 3 rather than sticking with 4
	// desired stores or blocking the decommissioning from completing.
	if need == numZoneReplicas {
		return need
	}
	if need%2 == 0 {
		need = need - 1
	}
	if need < 3 {
		need = 3
	}
	if need > numZoneReplicas {
		need = numZoneReplicas
	}

	return need
}

// GetNeededNonVoters calculates the number of non-voters a range should have
// given the number of voting replicas the range has and the number of nodes
// available for up-replication.
//
// NB: This method assumes that we have exactly as many voters as we need, since
// this method should only be consulted after voting replicas have been
// upreplicated / rebalanced off of dead/decommissioning nodes.
func GetNeededNonVoters(numVoters, zoneConfigNonVoterCount, clusterNodes int) int {
	need := zoneConfigNonVoterCount
	if clusterNodes-numVoters < need {
		// We only need non-voting replicas for the nodes that do not have a voting
		// replica.
		need = clusterNodes - numVoters
	}
	if need < 0 {
		need = 0 // Must be non-negative.
	}
	return need
}

// WillHaveFragileQuorum determines, based on the number of existing voters,
// incoming voters, and needed voters, if we will be upreplicating to a state
// in which we don't have enough needed voters and yet will have a fragile quorum
// due to an even number of voter replicas.
func WillHaveFragileQuorum(
	numExistingVoters, numNewVoters, zoneConfigVoterCount, clusterNodes int,
) bool {
	neededVoters := GetNeededVoters(int32(zoneConfigVoterCount), clusterNodes)
	willHave := numExistingVoters + numNewVoters
	// NB: If willHave >= neededVoters, then always allow up-replicating as that
	// will be the case when up-replicating a range with a decommissioning
	// replica.
	return numNewVoters > 0 && willHave < neededVoters && willHave%2 == 0
}

// LiveAndDeadVoterAndNonVoterReplicas splits up the replica in the given range
// descriptor by voters vs non-voters and live replicas vs dead replicas.
func LiveAndDeadVoterAndNonVoterReplicas(
	storePool storepool.AllocatorStorePool, desc *roachpb.RangeDescriptor,
) (
	voterReplicas, nonVoterReplicas, liveVoterReplicas, deadVoterReplicas, liveNonVoterReplicas, deadNonVoterReplicas []roachpb.ReplicaDescriptor,
) {
	voterReplicas = desc.Replicas().VoterDescriptors()
	nonVoterReplicas = desc.Replicas().NonVoterDescriptors()
	liveVoterReplicas, deadVoterReplicas = storePool.LiveAndDeadReplicas(
		voterReplicas, true, /* includeSuspectAndDrainingStores */
	)
	liveNonVoterReplicas, deadNonVoterReplicas = storePool.LiveAndDeadReplicas(
		nonVoterReplicas, true, /* includeSuspectAndDrainingStores */
	)
	return
}

// DetermineReplicaToReplaceAndFilter is used on add or replace allocator actions
// to filter the set of live voter and non-voter replicas to use in determining
// a new allocation target. It identifies a dead or decommissioning replica to
// replace from the list of voters or non-voters, depending on the replica
// status and target type, and returns the filtered live voters and non-voters
// along with the list of existing replicas and the index of the removal candidate.
// In case of an add action, no replicas are removed and a removeIdx of -1 is
// returned, and if no candidates for replacement can be found during a replace
// action, the returned nothingToDo flag will be set to true.
func DetermineReplicaToReplaceAndFilter(
	storePool storepool.AllocatorStorePool,
	action AllocatorAction,
	voters, nonVoters []roachpb.ReplicaDescriptor,
	liveVoterReplicas, deadVoterReplicas []roachpb.ReplicaDescriptor,
	liveNonVoterReplicas, deadNonVoterReplicas []roachpb.ReplicaDescriptor,
) (
	existing, remainingLiveVoters, remainingLiveNonVoters []roachpb.ReplicaDescriptor,
	removeIdx int,
	nothingToDo bool,
	err error,
) {
	removeIdx = -1
	remainingLiveVoters = liveVoterReplicas
	remainingLiveNonVoters = liveNonVoterReplicas
	var deadReplicas, removalCandidates []roachpb.ReplicaDescriptor

	if !(action.Add() || action.Replace()) {
		err = errors.AssertionFailedf(
			"unexpected attempt to filter replicas on non-add/non-replacement action %s",
			action,
		)
		return
	}

	replicaType := action.TargetReplicaType()
	replicaStatus := action.ReplicaStatus()

	switch replicaType {
	case VoterTarget:
		existing = voters
		deadReplicas = deadVoterReplicas
	case NonVoterTarget:
		existing = nonVoters
		deadReplicas = deadNonVoterReplicas
	default:
		panic(fmt.Sprintf("unknown targetReplicaType: %s", replicaType))
	}
	switch replicaStatus {
	case Alive:
		// NB: Live replicas are not candidates for replacement.
		return
	case Dead:
		removalCandidates = deadReplicas
	case Decommissioning:
		removalCandidates = storePool.DecommissioningReplicas(existing)
	default:
		panic(fmt.Sprintf("unknown replicaStatus: %s", replicaStatus))
	}
	if len(removalCandidates) == 0 {
		nothingToDo = true
		return
	}

	removeIdx = getRemoveIdx(existing, removalCandidates[0])
	if removeIdx < 0 {
		err = errors.AssertionFailedf(
			"%s %s %v unexpectedly not found in %v",
			replicaStatus, replicaType, removalCandidates[0], existing,
		)
		return
	}

	// TODO(sarkesian): Add comment on why this filtering only happens for voters.
	if replicaType == VoterTarget {
		if len(existing) == 1 {
			// If only one replica remains, that replica is the leaseholder and
			// we won't be able to swap it out. Ignore the removal and simply add
			// a replica.
			removeIdx = -1
		}

		if removeIdx >= 0 {
			replToRemove := existing[removeIdx]
			for i, r := range liveVoterReplicas {
				if r.ReplicaID == replToRemove.ReplicaID {
					remainingLiveVoters = append(liveVoterReplicas[:i:i], liveVoterReplicas[i+1:]...)
					break
				}
			}
		}
	}
	return
}

func getRemoveIdx(
	repls []roachpb.ReplicaDescriptor, deadOrDecommissioningRepl roachpb.ReplicaDescriptor,
) (removeIdx int) {
	removeIdx = -1
	for i, rDesc := range repls {
		if rDesc.StoreID == deadOrDecommissioningRepl.StoreID {
			removeIdx = i
			break
		}
	}
	return removeIdx
}

// FilterReplicasForAction converts a range descriptor to the filtered
// voter and non-voter replicas needed to allocate a target for the given action.
// NB: This is a convenience method for callers of allocator.AllocateTarget(..).
func FilterReplicasForAction(
	storePool storepool.AllocatorStorePool, desc *roachpb.RangeDescriptor, action AllocatorAction,
) (
	filteredVoters, filteredNonVoters []roachpb.ReplicaDescriptor,
	replacing *roachpb.ReplicaDescriptor,
	nothingToDo bool,
	err error,
) {
	voterReplicas, nonVoterReplicas,
		liveVoterReplicas, deadVoterReplicas,
		liveNonVoterReplicas, deadNonVoterReplicas := LiveAndDeadVoterAndNonVoterReplicas(storePool, desc)

	removeIdx := -1
	var existing []roachpb.ReplicaDescriptor
	existing, filteredVoters, filteredNonVoters, removeIdx, nothingToDo, err = DetermineReplicaToReplaceAndFilter(
		storePool,
		action,
		voterReplicas, nonVoterReplicas,
		liveVoterReplicas, deadVoterReplicas,
		liveNonVoterReplicas, deadNonVoterReplicas,
	)

	if removeIdx >= 0 {
		replacing = &existing[removeIdx]
	}

	return filteredVoters, filteredNonVoters, replacing, nothingToDo, err
}

// ComputeAction determines the exact operation needed to repair the
// supplied range, as governed by the supplied zone configuration. It
// returns the required action that should be taken and a priority.
func (a *Allocator) ComputeAction(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	desc *roachpb.RangeDescriptor,
) (action AllocatorAction, priority float64) {
	if storePool == nil {
		// Do nothing if storePool is nil for some unittests.
		action = AllocatorNoop
		return action, action.Priority()
	}

	if desc.Replicas().InAtomicReplicationChange() {
		// With a similar reasoning to the learner branch below, if we're in a
		// joint configuration the top priority is to leave it before we can
		// even think about doing anything else.
		action = AllocatorFinalizeAtomicReplicationChange
		return action, action.Priority()
	}

	if learners := desc.Replicas().LearnerDescriptors(); len(learners) > 0 {
		// Seeing a learner replica at this point is unexpected because learners are
		// a short-lived (ish) transient state in a learner+snapshot+voter cycle,
		// which is always done atomically. Only two places could have added a
		// learner: the replicate queue or AdminChangeReplicas request.
		//
		// The replicate queue only operates on leaseholders, which means that only
		// one node at a time is operating on a given range except in rare cases
		// (old leaseholder could start the operation, and a new leaseholder steps
		// up and also starts an overlapping operation). Combined with the above
		// atomicity, this means that if the replicate queue sees a learner, either
		// the node that was adding it crashed somewhere in the
		// learner+snapshot+voter cycle and we're the new leaseholder or we caught a
		// race.
		//
		// In the first case, we could assume the node that was adding it knew what
		// it was doing and finish the addition. Or we could leave it and do higher
		// priority operations first if there are any. However, this comes with code
		// complexity and concept complexity (computing old vs new quorum sizes
		// becomes ambiguous, the learner isn't in the quorum but it likely will be
		// soon, so do you count it?). Instead, we do the simplest thing and remove
		// it before doing any other operations to the range. We'll revisit this
		// decision if and when the complexity becomes necessary.
		//
		// If we get the race where AdminChangeReplicas is adding a replica and the
		// queue happens to run during the snapshot, this will remove the learner
		// and AdminChangeReplicas will notice either during the snapshot transfer
		// or when it tries to promote the learner to a voter. AdminChangeReplicas
		// should retry.
		//
		// On the other hand if we get the race where a leaseholder starts adding a
		// replica in the replicate queue and during this loses its lease, it should
		// probably not retry.
		//
		// TODO(dan): Since this goes before anything else, the priority here should
		// be influenced by whatever operations would happen right after the learner
		// is removed. In the meantime, we don't want to block something important
		// from happening (like addDeadReplacementVoterPriority) by queueing this at
		// a low priority so until this TODO is done, keep
		// removeLearnerReplicaPriority as the highest priority.
		action = AllocatorRemoveLearner
		return action, action.Priority()
	}

	return a.computeAction(ctx, storePool, conf, desc.Replicas().VoterDescriptors(),
		desc.Replicas().NonVoterDescriptors())
}

func (a *Allocator) computeAction(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	voterReplicas []roachpb.ReplicaDescriptor,
	nonVoterReplicas []roachpb.ReplicaDescriptor,
) (action AllocatorAction, adjustedPriority float64) {
	// NB: The ordering of the checks in this method is intentional. The order in
	// which these actions are returned by this method determines the relative
	// priority of the actions taken on a given range. We want this to be
	// symmetric with regards to the priorities defined at the top of this file
	// (which influence the replicateQueue's decision of which range it'll pick to
	// repair/rebalance before the others).
	//
	// In broad strokes, we first handle all voting replica-based actions and then
	// the actions pertaining to non-voting replicas. Within each replica set, we
	// first handle operations that correspond to repairing/recovering the range.
	// After that we handle rebalancing related actions, followed by removal
	// actions.
	haveVoters := len(voterReplicas)
	decommissioningVoters := storePool.DecommissioningReplicas(voterReplicas)
	postDecommissionVoters := haveVoters - len(decommissioningVoters)
	// Node count including dead nodes but excluding
	// decommissioning/decommissioned nodes.
	clusterNodes := storePool.ClusterNodeCount()
	neededVoters := GetNeededVoters(conf.GetNumVoters(), clusterNodes)
	desiredQuorum := computeQuorum(neededVoters)
	quorum := computeQuorum(haveVoters)

	// TODO(aayush): When haveVoters < neededVoters but we don't have quorum to
	// actually execute the addition of a new replica, we should be returning a
	// AllocatorRangeUnavailable.
	if haveVoters < neededVoters {
		// Range is under-replicated, and should add an additional voter.
		// Priority is adjusted by the difference between the current voter
		// count and the quorum of the desired voter count.
		action = AllocatorAddVoter
		adjustedPriority = action.Priority() + float64(desiredQuorum-haveVoters)
		log.KvDistribution.VEventf(ctx, 3, "%s - missing voter need=%d, have=%d, priority=%.2f",
			action, neededVoters, haveVoters, adjustedPriority)
		return action, adjustedPriority
	}

	// NB: For the purposes of determining whether a range has quorum, we
	// consider stores marked as "suspect" to be live. This is necessary because
	// we would otherwise spuriously consider ranges with replicas on suspect
	// stores to be unavailable, just because their nodes have failed a liveness
	// heartbeat in the recent past. This means we won't move those replicas
	// elsewhere (for a regular rebalance or for decommissioning).
	const includeSuspectAndDrainingStores = true
	liveVoters, deadVoters := storePool.LiveAndDeadReplicas(voterReplicas, includeSuspectAndDrainingStores)

	if len(liveVoters) < quorum {
		// Do not take any replacement/removal action if we do not have a quorum of
		// live voters. If we're correctly assessing the unavailable state of the
		// range, we also won't be able to add replicas as we try above, but hope
		// springs eternal.
		action = AllocatorRangeUnavailable
		log.KvDistribution.VEventf(ctx, 1, "unable to take action - live voters %v don't meet quorum of %d",
			liveVoters, quorum)
		return action, action.Priority()
	}

	if postDecommissionVoters <= neededVoters && len(deadVoters) > 0 {
		// Range has dead voter(s). We should up-replicate to add another before
		// removing the dead one. This can avoid permanent data loss in cases
		// where the node is only temporarily dead, but we remove it from the range
		// and lose a second node before we can up-replicate (#25392).
		action = AllocatorReplaceDeadVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - replacement for %d dead voters priority=%.2f",
			action, len(deadVoters), action.Priority())
		return action, action.Priority()
	}

	if postDecommissionVoters < neededVoters {
		// Range has decommissioning voter(s), which should be replaced.
		action = AllocatorReplaceDecommissioningVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - replacement for %d decommissioning voters priority=%.2f",
			action, len(decommissioningVoters), action.Priority())
		return action, action.Priority()
	}

	// Voting replica removal actions follow.
	// TODO(aayush): There's an additional case related to dead voters that we
	// should handle above. If there are one or more dead replicas, have < need,
	// and there are no available stores to up-replicate to, then we should try to
	// remove the dead replica(s) to get down to an odd number of replicas.
	if len(deadVoters) > 0 {
		// The range has dead replicas, which should be removed immediately.
		action = AllocatorRemoveDeadVoter
		adjustedPriority = action.Priority() + float64(quorum-len(liveVoters))
		log.KvDistribution.VEventf(ctx, 3, "%s - dead=%d, live=%d, quorum=%d, priority=%.2f",
			action, len(deadVoters), len(liveVoters), quorum, adjustedPriority)
		return action, adjustedPriority
	}

	if len(decommissioningVoters) > 0 {
		// Range is over-replicated, and has a decommissioning voter which
		// should be removed.
		action = AllocatorRemoveDecommissioningVoter
		log.KvDistribution.VEventf(ctx, 3,
			"%s - need=%d, have=%d, num_decommissioning=%d, priority=%.2f",
			action, neededVoters, haveVoters, len(decommissioningVoters), action.Priority())
		return action, action.Priority()
	}

	if haveVoters > neededVoters {
		// Range is over-replicated, and should remove a voter.
		// Ranges with an even number of voters get extra priority because
		// they have a more fragile quorum.
		action = AllocatorRemoveVoter
		adjustedPriority = action.Priority() - float64(haveVoters%2)
		log.KvDistribution.VEventf(ctx, 3, "%s - need=%d, have=%d, priority=%.2f", action, neededVoters,
			haveVoters, adjustedPriority)
		return action, adjustedPriority
	}

	// Non-voting replica actions follow.
	//
	// Non-voting replica addition / replacement.
	haveNonVoters := len(nonVoterReplicas)
	neededNonVoters := GetNeededNonVoters(haveVoters, int(conf.GetNumNonVoters()), clusterNodes)
	if haveNonVoters < neededNonVoters {
		action = AllocatorAddNonVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - missing non-voter need=%d, have=%d, priority=%.2f",
			action, neededNonVoters, haveNonVoters, action.Priority())
		return action, action.Priority()
	}

	decommissioningNonVoters := storePool.DecommissioningReplicas(nonVoterReplicas)
	postDecommissionNonVoters := haveNonVoters - len(decommissioningNonVoters)
	liveNonVoters, deadNonVoters := storePool.LiveAndDeadReplicas(
		nonVoterReplicas, includeSuspectAndDrainingStores,
	)

	if postDecommissionNonVoters <= neededNonVoters && len(deadNonVoters) > 0 {
		// The range has non-voter(s) on a dead node that we should replace.
		action = AllocatorReplaceDeadNonVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - replacement for %d dead non-voters priority=%.2f",
			action, len(deadNonVoters), action.Priority())
		return action, action.Priority()
	}

	if postDecommissionNonVoters < neededNonVoters {
		// The range has non-voter(s) on a decommissioning node that we should
		// replace.
		action = AllocatorReplaceDecommissioningNonVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - replacement for %d decommissioning non-voters priority=%.2f",
			action, len(decommissioningNonVoters), action.Priority())
		return action, action.Priority()
	}

	// Non-voting replica removal.
	if len(deadNonVoters) > 0 {
		// The range is over-replicated _and_ has non-voter(s) on a dead node. We'll
		// just remove these.
		action = AllocatorRemoveDeadNonVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - dead=%d, live=%d, priority=%.2f",
			action, len(deadNonVoters), len(liveNonVoters), action.Priority())
		return action, action.Priority()
	}

	if len(decommissioningNonVoters) > 0 {
		// The range is over-replicated _and_ has non-voter(s) on a decommissioning
		// node. We'll just remove these.
		action = AllocatorRemoveDecommissioningNonVoter
		log.KvDistribution.VEventf(ctx, 3,
			"%s - need=%d, have=%d, num_decommissioning=%d, priority=%.2f",
			action, neededNonVoters, haveNonVoters, len(decommissioningNonVoters), action.Priority())
		return action, action.Priority()
	}

	if haveNonVoters > neededNonVoters {
		// The range is simply over-replicated and should remove a non-voter.
		action = AllocatorRemoveNonVoter
		log.KvDistribution.VEventf(ctx, 3, "%s - need=%d, have=%d, priority=%.2f", action,
			neededNonVoters, haveNonVoters, action.Priority())
		return action, action.Priority()
	}

	// Nothing needs to be done, but we may want to rebalance.
	action = AllocatorConsiderRebalance
	return action, action.Priority()
}

// getReplicasForDiversityCalc returns the set of replica descriptors that
// should be used for computing the diversity scores for a target when
// allocating/removing/rebalancing a replica of `targetType`.
func getReplicasForDiversityCalc(
	targetType TargetReplicaType, existingVoters, allExistingReplicas []roachpb.ReplicaDescriptor,
) []roachpb.ReplicaDescriptor {
	switch t := targetType; t {
	case VoterTarget:
		// When computing the "diversity score" for a given store for a voting
		// replica allocation/rebalance/removal, we consider the localities of only
		// the stores that contain a voting replica for the range.
		//
		// Note that if we were to consider all stores that have any kind of replica
		// for the range, voting replica allocation would be disincentivized to pick
		// stores that (partially or fully) share locality hierarchies with stores
		// that contain a non-voting replica. This is undesirable because this could
		// inadvertently reduce the fault-tolerance of the range in cases like the
		// following:
		//
		// Consider 3 regions (A, B, C), each with 2 AZs. Suppose that regions A and
		// B have a voting replica each, whereas region C has a non-voting replica.
		// In cases like these, we would want region C to be picked over regions A
		// and B for allocating a new third voting replica since that improves our
		// fault tolerance to the greatest degree.
		// In the counterfactual (i.e. if we were to compute diversity scores based
		// off of all `existingReplicas`), regions A, B, and C would all be equally
		// likely to get a new voting replica.
		return existingVoters
	case NonVoterTarget:
		return allExistingReplicas
	default:
		panic(fmt.Sprintf("unsupported targetReplicaType: %v", t))
	}
}

type decisionDetails struct {
	Target   string
	Existing string `json:",omitempty"`
}

// CandidateSelector is an interface to select a store from a list of
// candidates.
type CandidateSelector interface {
	selectOne(cl candidateList) *candidate
}

// BestCandidateSelector in used to choose the best store to allocate.
type BestCandidateSelector struct {
	randGen allocatorRand
}

// NewBestCandidateSelector returns a CandidateSelector for choosing the best
// candidate store.
func (a *Allocator) NewBestCandidateSelector() CandidateSelector {
	return &BestCandidateSelector{a.randGen}
}

func (s *BestCandidateSelector) selectOne(cl candidateList) *candidate {
	return cl.selectBest(s.randGen)
}

// GoodCandidateSelector is used to choose a random store out of the stores that
// are good enough.
type GoodCandidateSelector struct {
	randGen allocatorRand
}

// NewGoodCandidateSelector returns a CandidateSelector for choosing a random store
// out of the stores that are good enough.
func (a *Allocator) NewGoodCandidateSelector() CandidateSelector {
	return &GoodCandidateSelector{a.randGen}
}

func (s *GoodCandidateSelector) selectOne(cl candidateList) *candidate {
	return cl.selectGood(s.randGen)
}

// AllocateTarget returns a suitable store for a new allocation of a voting or
// non-voting replica with the required attributes. Nodes already accommodating
// voting replicas are ruled out in the voter case, and nodes accommodating
// _any_ replicas are ruled out in the non-voter case.
func (a *Allocator) AllocateTarget(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	replacing *roachpb.ReplicaDescriptor,
	replicaStatus ReplicaStatus,
	targetType TargetReplicaType,
) (roachpb.ReplicationTarget, string, error) {
	options := a.ScorerOptions(ctx)
	candidateStoreList, aliveStoreCount, throttled := storePool.GetStoreList(storepool.StoreFilterThrottled)

	// If the replica is alive we are upreplicating, and in that case we want to
	// allocate new replicas on the best possible store. Otherwise, the replica is
	// dead or decommissioned, and we want to recover the missing replica as soon
	// as possible, and therefore any store that is good enough will be
	// considered.
	var selector CandidateSelector
	if replicaStatus == Alive || recoveryStoreSelector.Get(&a.st.SV) == "best" {
		selector = a.NewBestCandidateSelector()
	} else {
		selector = a.NewGoodCandidateSelector()
	}

	// Only consider the effects of replacing a replica on constraint conformance
	// during decommission. In the case that replicas are being replaced due to
	// a store being dead but no remaining live stores meet all constraints, they
	// should be considered of otherwise equal validity, with candidate ranking
	// chosing the best of the available options.
	var decommissioningReplica *roachpb.ReplicaDescriptor
	if replicaStatus == Decommissioning {
		decommissioningReplica = replacing
	}

	target, details := a.allocateTargetFromList(
		ctx,
		storePool,
		candidateStoreList,
		conf,
		existingVoters,
		existingNonVoters,
		decommissioningReplica,
		options,
		selector,
		// When allocating a *new* replica, we explicitly disregard nodes with any
		// existing replicas. This is important for multi-store scenarios as
		// otherwise, stores on the nodes that have existing replicas are simply
		// discouraged via the diversity heuristic. We want to entirely avoid
		// allocating multiple replicas onto different stores of the same node.
		false, /* allowMultipleReplsPerNode */
		targetType,
	)

	if !roachpb.Empty(target) {
		return target, details, nil
	}

	// When there are throttled stores that do match, we shouldn't send
	// the replica to purgatory.
	if len(throttled) > 0 {
		return roachpb.ReplicationTarget{}, "", errors.Errorf(
			"%d matching stores are currently throttled: %v", len(throttled), throttled,
		)
	}

	// Count the number of live stores which have full disks, to be included in
	// the error detail.
	aliveFullStoreCount := 0
	for _, store := range candidateStoreList.Stores {
		if !options.getDiskOptions().maxCapacityCheck(store) {
			aliveFullStoreCount++
		}
	}

	return roachpb.ReplicationTarget{}, "", &allocatorError{
		voterConstraints:      conf.VoterConstraints,
		constraints:           conf.Constraints,
		existingVoterCount:    len(existingVoters),
		existingNonVoterCount: len(existingNonVoters),
		aliveStores:           aliveStoreCount,
		throttledStores:       len(throttled),
		fullStores:            aliveFullStoreCount,
	}
}

// CheckAvoidsFragileQuorum ensures that if we are allocating a new voter and
// will result in an even number of voters, that we can allocate another voter
// target in order to avoid a fragile quorum state. This check should be
// performed whenever we are planning or testing allocation of a new voter.
//
// We can skip this check if we're swapping a replica or allocating a non-voter,
// since that does not change the quorum size.
func (a *Allocator) CheckAvoidsFragileQuorum(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	existingVoters, remainingLiveNonVoters []roachpb.ReplicaDescriptor,
	replicaStatus ReplicaStatus,
	replicaType TargetReplicaType,
	newTarget roachpb.ReplicationTarget,
	isReplacement bool,
) error {
	// Validation is only applicable when allocating new voters.
	if replicaType != VoterTarget {
		return nil
	}
	newVoters := 0
	if !isReplacement {
		newVoters = 1
	}
	clusterNodes := storePool.ClusterNodeCount()
	neededVoters := GetNeededVoters(conf.GetNumVoters(), clusterNodes)

	if WillHaveFragileQuorum(len(existingVoters), newVoters, neededVoters, clusterNodes) {
		// This means we are going to up-replicate to an even replica state.
		// Check if it is possible to go to an odd replica state beyond it.
		oldPlusNewReplicas := existingVoters
		oldPlusNewReplicas = append(
			oldPlusNewReplicas,
			roachpb.ReplicaDescriptor{NodeID: newTarget.NodeID, StoreID: newTarget.StoreID},
		)

		_, _, err := a.AllocateVoter(ctx, storePool, conf, oldPlusNewReplicas, remainingLiveNonVoters, nil /* replacing */, replicaStatus)
		return err
	}

	return nil
}

// AllocateVoter returns a suitable store for a new allocation of a voting
// replica with the required attributes. Nodes already accommodating existing
// voting replicas are ruled out as targets.
func (a *Allocator) AllocateVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	replacing *roachpb.ReplicaDescriptor,
	replicaStatus ReplicaStatus,
) (roachpb.ReplicationTarget, string, error) {
	return a.AllocateTarget(ctx, storePool, conf, existingVoters, existingNonVoters, replacing, replicaStatus, VoterTarget)
}

// AllocateNonVoter returns a suitable store for a new allocation of a
// non-voting replica with the required attributes. Nodes already accommodating
// _any_ existing replicas are ruled out as targets.
func (a *Allocator) AllocateNonVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	replacing *roachpb.ReplicaDescriptor,
	replicaStatus ReplicaStatus,
) (roachpb.ReplicationTarget, string, error) {
	return a.AllocateTarget(ctx, storePool, conf, existingVoters, existingNonVoters, replacing, replicaStatus, NonVoterTarget)
}

// AllocateTargetFromList returns a suitable store for a new allocation of a
// replica of the given type from the set of candidate stores, with the given
// existing set of voters and non-voters..
func (a *Allocator) AllocateTargetFromList(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	candidateStores storepool.StoreList,
	conf *roachpb.SpanConfig,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	options ScorerOptions,
	selector CandidateSelector,
	allowMultipleReplsPerNode bool,
	targetType TargetReplicaType,
) (roachpb.ReplicationTarget, string) {
	return a.allocateTargetFromList(ctx, storePool, candidateStores, conf, existingVoters,
		existingNonVoters, nil /* replacing */, options, selector, allowMultipleReplsPerNode, targetType)
}

func (a *Allocator) allocateTargetFromList(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	candidateStores storepool.StoreList,
	conf *roachpb.SpanConfig,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	replacing *roachpb.ReplicaDescriptor,
	options ScorerOptions,
	selector CandidateSelector,
	allowMultipleReplsPerNode bool,
	targetType TargetReplicaType,
) (roachpb.ReplicationTarget, string) {
	existingReplicas := append(existingVoters, existingNonVoters...)
	if replacing != nil {
		existingReplicas = append(existingReplicas, *replacing)
	}
	analyzedOverallConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingReplicas,
		conf.NumReplicas,
		conf.Constraints,
	)
	analyzedVoterConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingVoters,
		conf.GetNumVoters(),
		conf.VoterConstraints,
	)

	var replacingStore roachpb.StoreDescriptor
	var replacingStoreOK bool
	if replacing != nil {
		replacingStore, replacingStoreOK = storePool.GetStoreDescriptor(replacing.StoreID)
	}

	var constraintsChecker constraintsCheckFn
	switch t := targetType; t {
	case VoterTarget:
		// If we are replacing an existing replica, make sure we check the
		// constraints to ensure we are not going from a state in which a
		// constraint is satisfied to one in which we are not. In this case, we
		// consider no candidates to be valid, as no sorting of replicas would lead
		// to a satisfying candidate being selected.
		if replacing != nil && replacingStoreOK {
			constraintsChecker = voterConstraintsCheckerForReplace(
				analyzedOverallConstraints,
				analyzedVoterConstraints,
				replacingStore,
			)
		} else {
			constraintsChecker = voterConstraintsCheckerForAllocation(
				analyzedOverallConstraints,
				analyzedVoterConstraints,
			)
		}
	case NonVoterTarget:
		if replacing != nil && replacingStoreOK {
			constraintsChecker = nonVoterConstraintsCheckerForReplace(
				analyzedOverallConstraints, replacingStore,
			)
		} else {
			constraintsChecker = nonVoterConstraintsCheckerForAllocation(analyzedOverallConstraints)
		}
	default:
		log.KvDistribution.Fatalf(ctx, "unsupported targetReplicaType: %v", t)
	}

	// We'll consider the targets that have a non-voter as feasible
	// relocation/up-replication targets for existing/new voting replicas, since
	// we always want voter constraint conformance to take precedence over
	// non-voters. For instance, in cases where we can only satisfy constraints
	// for either 1 voter or 1 non-voter, we want the voter to be able to displace
	// the non-voter.
	existingReplicaSet := getReplicasForDiversityCalc(targetType, existingVoters, existingReplicas)
	candidates := rankedCandidateListForAllocation(
		ctx,
		candidateStores,
		constraintsChecker,
		existingReplicaSet,
		existingNonVoters,
		storePool.GetLocalitiesByStore(existingReplicaSet),
		storePool.IsStoreReadyForRoutineReplicaTransfer,
		allowMultipleReplsPerNode,
		options,
		targetType,
	)

	log.KvDistribution.VEventf(ctx, 3, "allocate %s: %s", targetType, candidates)
	if target := selector.selectOne(candidates); target != nil {
		log.KvDistribution.VEventf(ctx, 3, "add target: %s", target)
		details := decisionDetails{Target: target.compactString()}
		detailsBytes, err := json.Marshal(details)
		if err != nil {
			log.KvDistribution.Warningf(ctx, "failed to marshal details for choosing allocate target: %+v", err)
		}
		return roachpb.ReplicationTarget{
			NodeID: target.store.Node.NodeID, StoreID: target.store.StoreID,
		}, string(detailsBytes)
	}

	return roachpb.ReplicationTarget{}, ""
}

func (a Allocator) simulateRemoveTarget(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	targetStore roachpb.StoreID,
	conf *roachpb.SpanConfig,
	candidates []roachpb.ReplicaDescriptor,
	existingVoters []roachpb.ReplicaDescriptor,
	existingNonVoters []roachpb.ReplicaDescriptor,
	sl storepool.StoreList,
	rangeUsageInfo allocator.RangeUsageInfo,
	targetType TargetReplicaType,
	options ScorerOptions,
) (roachpb.ReplicationTarget, string, error) {
	candidateStores := make([]roachpb.StoreDescriptor, 0, len(candidates))
	for _, cand := range candidates {
		for _, store := range sl.Stores {
			if cand.StoreID == store.StoreID {
				candidateStores = append(candidateStores, store)
			}
		}
	}

	// Update statistics first
	switch t := targetType; t {
	case VoterTarget:
		storePool.UpdateLocalStoreAfterRebalance(targetStore, rangeUsageInfo, roachpb.ADD_VOTER)
		defer storePool.UpdateLocalStoreAfterRebalance(
			targetStore,
			rangeUsageInfo,
			roachpb.REMOVE_VOTER,
		)
		log.KvDistribution.VEventf(ctx, 3, "simulating which voter would be removed after adding s%d",
			targetStore)

		return a.RemoveTarget(
			ctx, storePool, conf, storepool.MakeStoreList(candidateStores),
			existingVoters, existingNonVoters, VoterTarget, options,
		)
	case NonVoterTarget:
		storePool.UpdateLocalStoreAfterRebalance(targetStore, rangeUsageInfo, roachpb.ADD_NON_VOTER)
		defer storePool.UpdateLocalStoreAfterRebalance(
			targetStore,
			rangeUsageInfo,
			roachpb.REMOVE_NON_VOTER,
		)
		log.KvDistribution.VEventf(ctx, 3, "simulating which non-voter would be removed after adding s%d",
			targetStore)
		return a.RemoveTarget(
			ctx, storePool, conf, storepool.MakeStoreList(candidateStores),
			existingVoters, existingNonVoters, NonVoterTarget, options,
		)
	default:
		panic(fmt.Sprintf("unknown targetReplicaType: %s", t))
	}
}

// RemoveTarget returns a suitable replica (of the given type) to remove from
// the provided set of replicas.
func (a Allocator) RemoveTarget(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	candidateStoreList storepool.StoreList,
	existingVoters []roachpb.ReplicaDescriptor,
	existingNonVoters []roachpb.ReplicaDescriptor,
	targetType TargetReplicaType,
	options ScorerOptions,
) (roachpb.ReplicationTarget, string, error) {
	if len(candidateStoreList.Stores) == 0 {
		return roachpb.ReplicationTarget{}, "", errors.Errorf(
			"must supply at least one" +
				" candidate replica to allocator.removeTarget()",
		)
	}

	existingReplicas := append(existingVoters, existingNonVoters...)
	analyzedOverallConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingReplicas,
		conf.NumReplicas,
		conf.Constraints,
	)
	analyzedVoterConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingVoters,
		conf.GetNumVoters(),
		conf.VoterConstraints,
	)

	var constraintsChecker constraintsCheckFn
	switch t := targetType; t {
	case VoterTarget:
		// Voting replicas have to abide by both the overall `constraints` (which
		// apply to all replicas) and `voter_constraints` which apply only to voting
		// replicas.
		constraintsChecker = voterConstraintsCheckerForRemoval(
			analyzedOverallConstraints,
			analyzedVoterConstraints,
		)
	case NonVoterTarget:
		constraintsChecker = nonVoterConstraintsCheckerForRemoval(analyzedOverallConstraints)
	default:
		log.KvDistribution.Fatalf(ctx, "unsupported targetReplicaType: %v", t)
	}

	replicaSetForDiversityCalc := getReplicasForDiversityCalc(targetType, existingVoters, existingReplicas)
	rankedCandidates := candidateListForRemoval(
		ctx,
		candidateStoreList,
		constraintsChecker,
		storePool.GetLocalitiesByStore(replicaSetForDiversityCalc),
		options,
	)

	log.KvDistribution.VEventf(ctx, 3, "remove %s: %s", targetType, rankedCandidates)
	if bad := rankedCandidates.selectWorst(a.randGen); bad != nil {
		for _, exist := range existingReplicas {
			if exist.StoreID == bad.store.StoreID {
				log.KvDistribution.VEventf(ctx, 3, "remove target: %s", bad)
				details := decisionDetails{Target: bad.compactString()}
				detailsBytes, err := json.Marshal(details)
				if err != nil {
					log.KvDistribution.Warningf(ctx, "failed to marshal details for choosing remove target: %+v", err)
				}
				return roachpb.ReplicationTarget{
					StoreID: exist.StoreID, NodeID: exist.NodeID,
				}, string(detailsBytes), nil
			}
		}
	}

	return roachpb.ReplicationTarget{}, "", errors.New("could not select an appropriate replica to be removed")
}

// RemoveVoter returns a suitable replica to remove from the provided replica
// set. It first attempts to randomly select a target from the set of stores
// that have greater than the average number of replicas. Failing that, it falls
// back to selecting a random target from any of the existing voting replicas.
func (a Allocator) RemoveVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	voterCandidates []roachpb.ReplicaDescriptor,
	existingVoters []roachpb.ReplicaDescriptor,
	existingNonVoters []roachpb.ReplicaDescriptor,
	options ScorerOptions,
) (roachpb.ReplicationTarget, string, error) {
	// Retrieve store descriptors for the provided candidates from the StorePool.
	candidateStoreIDs := make(roachpb.StoreIDSlice, len(voterCandidates))
	for i, exist := range voterCandidates {
		candidateStoreIDs[i] = exist.StoreID
	}
	candidateStoreList, _, _ := storePool.GetStoreListFromIDs(candidateStoreIDs, storepool.StoreFilterNone)

	return a.RemoveTarget(
		ctx,
		storePool,
		conf,
		candidateStoreList,
		existingVoters,
		existingNonVoters,
		VoterTarget,
		options,
	)
}

// RemoveNonVoter returns a suitable non-voting replica to remove from the
// provided set. It first attempts to randomly select a target from the set of
// stores that have greater than the average number of replicas. Failing that,
// it falls back to selecting a random target from any of the existing
// non-voting replicas.
func (a Allocator) RemoveNonVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	nonVoterCandidates []roachpb.ReplicaDescriptor,
	existingVoters []roachpb.ReplicaDescriptor,
	existingNonVoters []roachpb.ReplicaDescriptor,
	options ScorerOptions,
) (roachpb.ReplicationTarget, string, error) {
	// Retrieve store descriptors for the provided candidates from the StorePool.
	candidateStoreIDs := make(roachpb.StoreIDSlice, len(nonVoterCandidates))
	for i, exist := range nonVoterCandidates {
		candidateStoreIDs[i] = exist.StoreID
	}
	candidateStoreList, _, _ := storePool.GetStoreListFromIDs(candidateStoreIDs, storepool.StoreFilterNone)

	return a.RemoveTarget(
		ctx,
		storePool,
		conf,
		candidateStoreList,
		existingVoters,
		existingNonVoters,
		NonVoterTarget,
		options,
	)
}

// RebalanceTarget returns a suitable store for a rebalance target (of the given
// type) with required attributes.
func (a Allocator) RebalanceTarget(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	raftStatus *raft.Status,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	rangeUsageInfo allocator.RangeUsageInfo,
	filter storepool.StoreFilter,
	targetType TargetReplicaType,
	options ScorerOptions,
) (add, remove roachpb.ReplicationTarget, details string, ok bool) {
	sl, _, _ := storePool.GetStoreList(filter)

	// If we're considering a rebalance due to an `AdminScatterRequest`, we'd like
	// to ensure that we're returning a random rebalance target to a new store
	// that's a reasonable fit for an existing replica. So we might jitter the
	// existing stats on the stores inside `sl`.
	sl = options.maybeJitterStoreStats(sl, a.randGen)

	existingReplicas := append(existingVoters, existingNonVoters...)

	zero := roachpb.ReplicationTarget{}
	analyzedOverallConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingReplicas,
		conf.NumReplicas,
		conf.Constraints,
	)
	analyzedVoterConstraints := constraint.AnalyzeConstraints(
		storePool,
		existingVoters,
		conf.GetNumVoters(),
		conf.VoterConstraints,
	)
	var removalConstraintsChecker constraintsCheckFn
	var rebalanceConstraintsChecker rebalanceConstraintsCheckFn
	var replicaSetToRebalance, otherReplicaSet []roachpb.ReplicaDescriptor

	switch t := targetType; t {
	case VoterTarget:
		removalConstraintsChecker = voterConstraintsCheckerForRemoval(
			analyzedOverallConstraints,
			analyzedVoterConstraints,
		)
		rebalanceConstraintsChecker = voterConstraintsCheckerForRebalance(
			analyzedOverallConstraints,
			analyzedVoterConstraints,
		)
		replicaSetToRebalance = existingVoters
		otherReplicaSet = existingNonVoters
	case NonVoterTarget:
		removalConstraintsChecker = nonVoterConstraintsCheckerForRemoval(analyzedOverallConstraints)
		rebalanceConstraintsChecker = nonVoterConstraintsCheckerForRebalance(analyzedOverallConstraints)
		replicaSetToRebalance = existingNonVoters
		// When rebalancing non-voting replicas, we don't consider stores that
		// already have voting replicas as possible candidates. Voting replicas are
		// supposed to be rebalanced before non-voting replicas, and they do
		// consider the non-voters' stores as possible candidates.
		otherReplicaSet = existingVoters
	default:
		log.KvDistribution.Fatalf(ctx, "unsupported targetReplicaType: %v", t)
	}

	replicaSetForDiversityCalc := getReplicasForDiversityCalc(targetType, existingVoters, existingReplicas)
	results := rankedCandidateListForRebalancing(
		ctx,
		sl,
		removalConstraintsChecker,
		rebalanceConstraintsChecker,
		existingVoters,
		existingNonVoters,
		targetType,
		storePool.GetLocalitiesByStore(replicaSetForDiversityCalc),
		storePool.IsStoreReadyForRoutineReplicaTransfer,
		options,
		a.Metrics,
	)

	if len(results) == 0 {
		return zero, zero, "", false
	}
	// Keep looping until we either run out of options or find a target that we're
	// pretty sure we won't want to remove immediately after adding it. If we
	// would, we don't want to actually rebalance to that target.
	var target, existingCandidate *candidate
	var removeReplica roachpb.ReplicationTarget
	for {
		target, existingCandidate = bestRebalanceTarget(a.randGen, results)
		if target == nil {
			return zero, zero, "", false
		}

		// Add a fake new replica to our copy of the replica descriptor so that we can
		// simulate the removal logic. If we decide not to go with this target, note
		// that this needs to be removed from desc before we try any other target.
		newReplica := roachpb.ReplicaDescriptor{
			NodeID:    target.store.Node.NodeID,
			StoreID:   target.store.StoreID,
			ReplicaID: maxReplicaID(existingReplicas) + 1,
		}
		// Deep-copy the Replicas slice since we'll mutate it below.
		existingPlusOneNew := append([]roachpb.ReplicaDescriptor(nil), replicaSetToRebalance...)
		existingPlusOneNew = append(existingPlusOneNew, newReplica)
		replicaCandidates := existingPlusOneNew
		// If we can, filter replicas as we would if we were actually removing one.
		// If we can't (e.g. because we're the leaseholder but not the raft leader),
		// it's better to simulate the removal with the info that we do have than to
		// assume that the rebalance is ok (#20241).
		if targetType == VoterTarget && raftStatus != nil && raftStatus.Progress != nil {
			replicaCandidates = simulateFilterUnremovableReplicas(
				ctx, raftStatus, replicaCandidates, newReplica.ReplicaID)
		}
		if len(replicaCandidates) == 0 {
			// No existing replicas are suitable to remove.
			log.KvDistribution.VEventf(ctx, 2, "not rebalancing %s to s%d because there are no existing "+
				"replicas that can be removed", targetType, target.store.StoreID)
			return zero, zero, "", false
		}

		// If the target is a necessary non-voter promotion to satisfy some
		// constraint, then do not attempt to simulate a remove target. We know
		// that the target can be promoted, whilst another store is demoted (or
		// removed) in order to satisfy a voter constraint. When every replica is
		// necessary to satisfy an all-replica, or voter constraint, the simulated
		// remove replica will not always be the existingCandidate depending on
		// whether every voter is considered necessary.
		if target.voterNecessary {
			removeReplica = roachpb.ReplicationTarget{
				NodeID:  existingCandidate.store.Node.NodeID,
				StoreID: existingCandidate.store.StoreID,
			}
			break
		}

		var removeDetails string
		var err error
		removeReplica, removeDetails, err = a.simulateRemoveTarget(
			ctx,
			storePool,
			target.store.StoreID,
			conf,
			replicaCandidates,
			existingPlusOneNew,
			otherReplicaSet,
			sl,
			rangeUsageInfo,
			targetType,
			options,
		)
		if err != nil {
			log.KvDistribution.Warningf(ctx, "simulating removal of %s failed: %+v", targetType, err)
			return zero, zero, "", false
		}
		if target.store.StoreID != removeReplica.StoreID {
			// Successfully populated these variables
			_, _ = target, removeReplica
			break
		}

		log.KvDistribution.VEventf(ctx, 2, "not rebalancing to s%d because we'd immediately remove it: %s",
			target.store.StoreID, removeDetails)
	}

	// Compile the details entry that will be persisted into system.rangelog for
	// debugging/auditability purposes.
	dDetails := decisionDetails{
		Target:   target.compactString(),
		Existing: existingCandidate.compactString(),
	}
	detailsBytes, err := json.Marshal(dDetails)
	if err != nil {
		log.KvDistribution.Warningf(ctx, "failed to marshal details for choosing rebalance target: %+v", err)
	}

	addTarget := roachpb.ReplicationTarget{
		NodeID:  target.store.Node.NodeID,
		StoreID: target.store.StoreID,
	}
	removeTarget := roachpb.ReplicationTarget{
		NodeID:  removeReplica.NodeID,
		StoreID: removeReplica.StoreID,
	}
	return addTarget, removeTarget, string(detailsBytes), true
}

// RebalanceVoter returns a suitable store for a rebalance target with required
// attributes. Rebalance targets are selected via the same mechanism as
// AllocateVoter(), except the chosen target must follow some additional
// criteria. Namely, if chosen, it must further the goal of balancing the
// cluster.
//
// The supplied parameters are the required attributes for the range and
// information about the range being considered for rebalancing.
//
// The existing voting replicas modulo any store with dead replicas are
// candidates for rebalancing.
//
// Simply ignoring a rebalance opportunity in the event that the target chosen
// by rankedCandidateListForRebalancing() doesn't fit balancing criteria is
// perfectly fine, as other stores in the cluster will also be doing their
// probabilistic best to rebalance. This helps prevent a stampeding herd
// targeting an abnormally under-utilized store.
//
// The return values are, in order:
//
//  1. The target on which to add a new replica,
//  2. An existing replica to remove,
//  3. a JSON string for use in the range log, and
//  4. a boolean indicationg whether 1-3 were populated (i.e. whether a rebalance
//     opportunity was found).
func (a Allocator) RebalanceVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	raftStatus *raft.Status,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	rangeUsageInfo allocator.RangeUsageInfo,
	filter storepool.StoreFilter,
	options ScorerOptions,
) (add, remove roachpb.ReplicationTarget, details string, ok bool) {
	return a.RebalanceTarget(
		ctx,
		storePool,
		conf,
		raftStatus,
		existingVoters,
		existingNonVoters,
		rangeUsageInfo,
		filter,
		VoterTarget,
		options,
	)
}

// RebalanceNonVoter returns a suitable pair of rebalance candidates for a
// non-voting replica. This behaves very similarly to `RebalanceVoter` as
// explained above. The key differences are the following:
//
// 1. Non-voting replicas only adhere to the overall `constraints` and not the
// `voter_constraints`.
// 2. We do not consider stores that have voters as valid candidates for
// rebalancing.
// 3. Diversity score calculation for non-voters is relative to all existing
// replicas. This is in contrast to how we compute the diversity scores for
// voting replicas, which are computed relative to just the set of voting
// replicas.
func (a Allocator) RebalanceNonVoter(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	raftStatus *raft.Status,
	existingVoters, existingNonVoters []roachpb.ReplicaDescriptor,
	rangeUsageInfo allocator.RangeUsageInfo,
	filter storepool.StoreFilter,
	options ScorerOptions,
) (add, remove roachpb.ReplicationTarget, details string, ok bool) {
	return a.RebalanceTarget(
		ctx,
		storePool,
		conf,
		raftStatus,
		existingVoters,
		existingNonVoters,
		rangeUsageInfo,
		filter,
		NonVoterTarget,
		options,
	)
}

// ScorerOptions returns the default scorer option, for use in the rebalancing
// machinery to achieve range count convergence.
func (a *Allocator) ScorerOptions(ctx context.Context) *RangeCountScorerOptions {
	return &RangeCountScorerOptions{
		IOOverloadOptions:       a.IOOverloadOptions(),
		DiskCapacityOptions:     a.DiskOptions(),
		deterministic:           a.deterministic,
		rangeRebalanceThreshold: RangeRebalanceThreshold.Get(&a.st.SV),
	}
}

// ScorerOptionsForScatter returns the scorer options for scattering purposes.
func (a *Allocator) ScorerOptionsForScatter(ctx context.Context) *ScatterScorerOptions {
	return &ScatterScorerOptions{
		RangeCountScorerOptions: RangeCountScorerOptions{
			IOOverloadOptions:       a.IOOverloadOptions(),
			DiskCapacityOptions:     a.DiskOptions(),
			deterministic:           a.deterministic,
			rangeRebalanceThreshold: 0,
		},
		// We set jitter to be equal to the padding around replica-count rebalancing
		// because we'd like to make it such that rebalances made due to an
		// `AdminScatter` are roughly in line (but more random than) the rebalances
		// made by the replicateQueue during normal course of operations. In other
		// words, we don't want stores that are too far away from the mean to be
		// affected by the jitter.
		jitter: RangeRebalanceThreshold.Get(&a.st.SV),
	}
}

// ValidLeaseTargets returns a set of candidate stores that are suitable to be
// transferred a lease for the given range.
//
// - It excludes stores that are dead, or marked draining or suspect.
// - If the range has lease_preferences, and there are any non-draining,
// non-suspect nodes that match those preferences, it excludes stores that don't
// match those preferences.
// - It excludes replicas that may need snapshots. If replica calling this
// method is not the Raft leader (meaning that it doesn't know whether follower
// replicas need a snapshot or not), produces no results.
// - It excludes replicas that are on stores which are IO overloaded.
func (a *Allocator) ValidLeaseTargets(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	desc *roachpb.RangeDescriptor,
	conf *roachpb.SpanConfig,
	existing []roachpb.ReplicaDescriptor,
	leaseRepl interface {
		StoreID() roachpb.StoreID
		RaftStatus() *raft.Status
		GetCompactedIndex() kvpb.RaftIndex
		SendStreamStats(*rac2.RangeSendStreamStats)
	},
	opts allocator.TransferLeaseOptions,
) []roachpb.ReplicaDescriptor {
	candidates := make([]roachpb.ReplicaDescriptor, 0, len(existing))
	replDescs := roachpb.MakeReplicaSet(existing)
	for i := range existing {
		if err := roachpb.CheckCanReceiveLease(existing[i], replDescs, false /* wasLastLeaseholder */); err != nil {
			continue
		}
		// If we're not allowed to include the current replica, remove it from
		// consideration here.
		if existing[i].StoreID == leaseRepl.StoreID() && opts.ExcludeLeaseRepl {
			continue
		}
		candidates = append(candidates, existing[i])
	}
	candidates, _ = storePool.LiveAndDeadReplicas(
		candidates, false, /* includeSuspectAndDrainingStores */
	)

	if a.knobs == nil || !a.knobs.AllowLeaseTransfersToReplicasNeedingSnapshots {
		// Only proceed with the lease transfer if we are also the raft leader (we
		// already know we are the leaseholder at this point), and only consider
		// replicas that are in `StateReplicate` as potential candidates.
		//
		// NB: The RaftStatus() only returns a non-empty and non-nil result on the
		// Raft leader (since Raft followers do not track the progress of other
		// replicas, only the leader does).
		//
		// NB: On every Raft tick, we try to ensure that leadership is collocated with
		// leaseholdership (see
		// Replica.maybeTransferRaftLeadershipToLeaseholderLocked()). This means that
		// on a range that is not already borked (i.e. can accept writes), periods of
		// leader/leaseholder misalignment should be ephemeral and rare. We choose to
		// be pessimistic here and choose to bail on the lease transfer, as opposed to
		// potentially transferring the lease to a replica that may be waiting for a
		// snapshot (which will wedge the range until the replica applies that
		// snapshot).
		validSnapshotCandidates := []roachpb.ReplicaDescriptor{}

		if opts.AllowUninitializedCandidates {
			// When alowing uninitialized candidates, ignore the raft status of
			// candidates which are not in the current range descriptors
			// replica set, however are in the candidate list. Uninitialized
			// replicas will always need a snapshot.
			existingCandidates := []roachpb.ReplicaDescriptor{}
			for _, candidate := range candidates {
				if _, ok := desc.GetReplicaDescriptor(candidate.StoreID); ok {
					existingCandidates = append(existingCandidates, candidate)
				} else {
					validSnapshotCandidates = append(validSnapshotCandidates, candidate)
				}
			}
			candidates = existingCandidates
		}

		status := leaseRepl.RaftStatus()
		if a.knobs != nil && a.knobs.RaftStatusFn != nil {
			status = a.knobs.RaftStatusFn(desc, leaseRepl.StoreID())
		}

		candidates = append(validSnapshotCandidates, excludeReplicasInNeedOfSnapshots(
			ctx, status, leaseRepl.GetCompactedIndex(), candidates)...)
		candidates = excludeReplicasInNeedOfCatchup(
			ctx, leaseRepl.SendStreamStats, candidates)
	}

	// Determine which store(s) is preferred based on user-specified preferences.
	// If any stores match, only consider those stores as candidates.
	preferred := a.PreferredLeaseholders(storePool, conf, candidates)
	if len(preferred) > 0 {
		candidates = preferred
	}

	// Filter the candidate list to only those stores which are not IO
	// overloaded.
	nonIOOverloadedPreferred := a.nonIOOverloadedLeaseTargets(
		ctx,
		storePool,
		candidates,
		leaseRepl.StoreID(),
		a.IOOverloadOptions(),
	)

	return nonIOOverloadedPreferred
}

// nonIOOverloadedLeaseTargets returns a list of non IO overloaded lease
// replica targets and whether the leaseholder replica should be replaced,
// given the existing replicas, IO overload  options and IO overload of
// existing replica stores.
func (a *Allocator) nonIOOverloadedLeaseTargets(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	existingReplicas []roachpb.ReplicaDescriptor,
	leaseStoreID roachpb.StoreID,
	ioOverloadOptions IOOverloadOptions,
) (candidates []roachpb.ReplicaDescriptor) {
	// We return early to avoid unnecessary work when IO overload is set to be
	// ignored anyway.
	if ioOverloadOptions.LeaseEnforcementLevel == IOOverloadThresholdIgnore {
		return existingReplicas
	}

	sl, _, _ := storePool.GetStoreListFromIDs(replDescsToStoreIDs(existingReplicas), storepool.StoreFilterSuspect)

	for _, replDesc := range existingReplicas {
		store, ok := sl.FindStoreByID(replDesc.StoreID)
		// If the replica is the current leaseholder, don't include it as a
		// candidate and if it is filtered out of the store list due to being
		// suspect; or the leaseholder store doesn't pass the leaseholder IO
		// overload check.
		//
		// Note that the leaseholder store IO overload check is less strict than
		// the transfer target check below. We don't want to shed leases at the
		// same point a candidate becomes ineligible as it could lead to thrashing.
		// Instead, we create a buffer between the two to avoid leases moving back
		// and forth.
		if (replDesc.StoreID == leaseStoreID) &&
			(!ok || !ioOverloadOptions.ExistingLeaseCheck(ctx, store, sl)) {
			continue
		}

		// If the replica is not the leaseholder, don't include it as a candidate
		// if it is filtered out similar to above, or the replica store doesn't
		// pass the lease transfer IO overload check.
		if replDesc.StoreID != leaseStoreID &&
			(!ok || !ioOverloadOptions.transferLeaseToCheck(ctx, store, sl)) {
			continue
		}

		candidates = append(candidates, replDesc)
	}

	return candidates
}

// leaseholderShouldMoveDueToIOOverload returns true if the current leaseholder
// store is IO overloaded and there are other viable leaseholder stores.
func (a *Allocator) leaseholderShouldMoveDueToIOOverload(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	existingReplicas []roachpb.ReplicaDescriptor,
	leaseStoreID roachpb.StoreID,
	ioOverloadOptions IOOverloadOptions,
) bool {
	sl, _, _ := storePool.GetStoreListFromIDs(replDescsToStoreIDs(existingReplicas), storepool.StoreFilterSuspect)

	// Check the existing replicas for the leaseholder, if it doesn't meet the
	// check return that the lease should be moved due to IO overload on the
	// current leaseholder store. If the leaseholder is suspect or doesn't have a
	// store descriptor ready, then we ignore it below and don't consider it IO
	// overloaded.
	for _, replDesc := range existingReplicas {
		if store, ok := sl.FindStoreByID(replDesc.StoreID); ok && replDesc.StoreID == leaseStoreID {
			return !ioOverloadOptions.ExistingLeaseCheck(ctx, store, sl)
		}
	}

	return false
}

// LeaseholderShouldMoveDueToPreferences returns true if the current leaseholder
// is in violation of lease preferences _that can otherwise be satisfied_ by
// some existing replica.
func (a *Allocator) LeaseholderShouldMoveDueToPreferences(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	leaseRepl interface {
		StoreID() roachpb.StoreID
		RaftStatus() *raft.Status
		GetCompactedIndex() kvpb.RaftIndex
		SendStreamStats(*rac2.RangeSendStreamStats)
	},
	allExistingReplicas []roachpb.ReplicaDescriptor,
	exclReplsInNeedOfSnapshots bool,
) bool {
	// Defensive check to ensure that this is never called with a replica set that
	// does not contain the leaseholder.
	var leaseholderInExisting bool
	for _, repl := range allExistingReplicas {
		if repl.StoreID == leaseRepl.StoreID() {
			leaseholderInExisting = true
			break
		}
	}
	// If the leaseholder is not in the descriptor, then we should not move the
	// lease since we don't know who the leaseholder is. This normally doesn't
	// happen, but can occasionally since the loading of the leaseholder and of
	// the existing replicas aren't always under a consistent lock.
	if !leaseholderInExisting {
		log.KvDistribution.Info(ctx, "expected leaseholder store to be in the slice of existing replicas")
		return false
	}

	// Exclude suspect/draining/dead stores.
	candidates, _ := storePool.LiveAndDeadReplicas(
		allExistingReplicas, false, /* includeSuspectAndDrainingStores */
	)
	// If there are any replicas that do match lease preferences, then we check if
	// the existing leaseholder is one of them.
	preferred := a.PreferredLeaseholders(storePool, conf, candidates)
	if exclReplsInNeedOfSnapshots {
		preferred = excludeReplicasInNeedOfSnapshots(
			ctx, leaseRepl.RaftStatus(), leaseRepl.GetCompactedIndex(), preferred)
		preferred = excludeReplicasInNeedOfCatchup(
			ctx, leaseRepl.SendStreamStats, preferred)
	}
	if len(preferred) == 0 {
		return false
	}
	for _, repl := range preferred {
		if repl.StoreID == leaseRepl.StoreID() {
			return false
		}
	}
	return true
}

// DiskOptions returns the disk options. The disk options are used to determine
// whether a store has disk capacity for additional replicas; or whether the
// disk is over capacity and should shed replicas.
func (a *Allocator) DiskOptions() DiskCapacityOptions {
	return makeDiskCapacityOptions(&a.st.SV)
}

// IOOverloadOptions returns the store IO overload options. It is used to
// filter and score candidates based on their level of IO overload and
// enforcement level.
func (a *Allocator) IOOverloadOptions() IOOverloadOptions {
	return IOOverloadOptions{
		ReplicaEnforcementLevel:      ReplicaIOOverloadThresholdEnforcement.Get(&a.st.SV),
		LeaseEnforcementLevel:        LeaseIOOverloadThresholdEnforcement.Get(&a.st.SV),
		UseIOThresholdMax:            true,
		ReplicaIOOverloadThreshold:   ReplicaIOOverloadThreshold.Get(&a.st.SV),
		LeaseIOOverloadThreshold:     LeaseIOOverloadThreshold.Get(&a.st.SV),
		LeaseIOOverloadShedThreshold: LeaseIOOverloadShedThreshold.Get(&a.st.SV),
	}
}

// TransferLeaseTarget returns a suitable replica to transfer the range lease
// to from the provided list. It includes the current lease holder replica
// unless asked to do otherwise by the excludeLeaseRepl parameter.
//
// Returns an empty descriptor if no target is found.
//
// TODO(aayush, andrei): If a draining leaseholder doesn't see any other voters
// in its locality, but sees a learner, rather than allowing the lease to be
// transferred outside of its current locality (likely violating leaseholder
// preferences, at least temporarily), it would be nice to promote the existing
// learner to a voter. This could be further extended to cases where we have a
// dead voter in a given locality along with a live learner. In such cases, we
// would want to promote the live learner to a voter and demote the dead voter
// to a learner.
func (a *Allocator) TransferLeaseTarget(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	desc *roachpb.RangeDescriptor,
	conf *roachpb.SpanConfig,
	existing []roachpb.ReplicaDescriptor,
	leaseRepl interface {
		StoreID() roachpb.StoreID
		GetRangeID() roachpb.RangeID
		RaftStatus() *raft.Status
		GetCompactedIndex() kvpb.RaftIndex
		SendStreamStats(*rac2.RangeSendStreamStats)
	},
	usageInfo allocator.RangeUsageInfo,
	forceDecisionWithoutStats bool,
	opts allocator.TransferLeaseOptions,
) roachpb.ReplicaDescriptor {
	if a.knobs != nil {
		if blockFn := a.knobs.BlockTransferTarget; blockFn != nil && blockFn(leaseRepl.GetRangeID()) {
			return roachpb.ReplicaDescriptor{}
		}
	}
	excludeLeaseRepl := opts.ExcludeLeaseRepl
	excludeReplsInNeedOfSnap := a.knobs == nil || !a.knobs.AllowLeaseTransfersToReplicasNeedingSnapshots
	if a.LeaseholderShouldMoveDueToPreferences(ctx, storePool, conf, leaseRepl, existing, excludeReplsInNeedOfSnap) ||
		a.leaseholderShouldMoveDueToIOOverload(ctx, storePool, existing, leaseRepl.StoreID(), a.IOOverloadOptions()) {
		// Explicitly exclude the current leaseholder from the result set if it is
		// in violation of lease preferences that can be satisfied by some other
		// replica or is IO overloaded.
		excludeLeaseRepl = true
	}

	allStoresList, _, _ := storePool.GetStoreList(storepool.StoreFilterNone)
	storeDescMap := allStoresList.ToMap()
	sl, _, _ := storePool.GetStoreList(storepool.StoreFilterSuspect)
	sl = sl.ExcludeInvalid(conf.Constraints)
	sl = sl.ExcludeInvalid(conf.VoterConstraints)
	candidateLeasesMean := sl.CandidateLeases.Mean

	source, ok := storePool.GetStoreDescriptor(leaseRepl.StoreID())
	if !ok {
		return roachpb.ReplicaDescriptor{}
	}

	validTargets := a.ValidLeaseTargets(ctx, storePool, desc, conf, existing, leaseRepl, opts)

	// Short-circuit if there are no valid targets out there.
	if len(validTargets) == 0 || (len(validTargets) == 1 && validTargets[0].StoreID == leaseRepl.StoreID()) {
		log.KvDistribution.VEventf(ctx, 2, "no lease transfer target found for r%d", leaseRepl.GetRangeID())
		return roachpb.ReplicaDescriptor{}
	}

	switch g := opts.Goal; g {
	case allocator.FollowTheWorkload:
		// Try to pick a replica to transfer the lease to while also determining
		// whether we actually should be transferring the lease. The transfer
		// decision is only needed if we've been asked to not exclude the current
		// lease replica.
		//
		// TODO(aayush): Whenever `excludeLeaseRepl` is true, `followTheWorkload`
		// falls back to `leaseCountConvergence`. Rationalize this or refactor this
		// logic to be more clear.
		transferDec, repl := a.shouldTransferLeaseForAccessLocality(
			ctx, storePool, source, validTargets, usageInfo, nil, candidateLeasesMean,
		)
		if !excludeLeaseRepl {
			switch transferDec {
			case shouldNotTransfer:
				if !forceDecisionWithoutStats {
					return roachpb.ReplicaDescriptor{}
				}
				fallthrough
			case decideWithoutStats:
				if !a.shouldTransferLeaseForLeaseCountConvergence(ctx, storePool, sl, source, validTargets) {
					return roachpb.ReplicaDescriptor{}
				}
			case shouldTransfer:
			default:
				log.KvDistribution.Fatalf(ctx, "unexpected transfer decision %d with replica %+v", transferDec, repl)
			}
		}
		if repl != (roachpb.ReplicaDescriptor{}) {
			// We found a lease transfer candidate, using follow the workload.
			// Update the respective metric and return the replica.
			// NB: shouldTransferLeaseForAccessLocality will never return the
			// current leaseholder as a target.
			a.Metrics.LoadBasedLeaseTransferMetrics.FollowTheWorkload.Inc(1)
			return repl
		}
		// Fall back to logic that doesn't take request counts and latency into
		// account if the counts/latency-based logic couldn't pick a best replica.
		fallthrough

	case allocator.LeaseCountConvergence:
		// If we want to ignore the existing lease counts on replicas, just do a
		// random transfer.
		if !opts.CheckCandidateFullness {
			a.randGen.Lock()
			defer a.randGen.Unlock()
			return validTargets[a.randGen.Intn(len(validTargets))]
		}

		var bestOption roachpb.ReplicaDescriptor
		candidates := make([]roachpb.ReplicaDescriptor, 0, len(validTargets))
		bestOptionLeaseCount := int32(math.MaxInt32)
		for _, repl := range validTargets {
			if leaseRepl.StoreID() == repl.StoreID {
				continue
			}
			storeDesc, ok := storePool.GetStoreDescriptor(repl.StoreID)
			if !ok {
				continue
			}
			if float64(storeDesc.Capacity.LeaseCount) < candidateLeasesMean-0.5 {
				candidates = append(candidates, repl)
			}
			if storeDesc.Capacity.LeaseCount < bestOptionLeaseCount {
				bestOption = repl
				bestOptionLeaseCount = storeDesc.Capacity.LeaseCount
			}
		}
		if len(candidates) == 0 {
			// If there were no validTargets replicas on stores with less-than-mean
			// leases, and we _must_ move the lease away (indicated by
			// `opts.excludeLeaseRepl`), just return the best possible option.
			if excludeLeaseRepl {
				return bestOption
			}
			return roachpb.ReplicaDescriptor{}
		}
		a.randGen.Lock()
		defer a.randGen.Unlock()
		return candidates[a.randGen.Intn(len(candidates))]

	case allocator.LoadConvergence:
		leaseReplLoad := usageInfo.TransferImpact()
		candidates := make([]roachpb.StoreID, 0, len(validTargets)-1)
		for _, repl := range validTargets {
			if repl.StoreID != leaseRepl.StoreID() {
				candidates = append(candidates, repl.StoreID)
			}
		}

		// When the goal is to further QPS convergence across stores, we ensure that
		// any lease transfer decision we make *reduces the delta between the store
		// serving the highest QPS and the store serving the lowest QPS* among our
		// list of candidates.
		//
		// NB: We're assuming that the lease transfer will move all of the
		// leaseholder's load to the replica that receives the lease. This will not
		// be true in all cases (some percentage of the leaseholder's traffic could
		// be follower read traffic). See
		// https://github.com/cockroachdb/cockroach/issues/75630.
		bestStore, noRebalanceReason := bestStoreToMinimizeLoadDelta(
			leaseReplLoad,
			leaseRepl.StoreID(),
			candidates,
			storeDescMap,
			&LoadScorerOptions{
				IOOverloadOptions:            a.IOOverloadOptions(),
				DiskOptions:                  a.DiskOptions(),
				Deterministic:                a.deterministic,
				LoadDims:                     opts.LoadDimensions,
				LoadThreshold:                LoadThresholds(&a.st.SV, opts.LoadDimensions...),
				MinLoadThreshold:             LoadMinThresholds(opts.LoadDimensions...),
				MinRequiredRebalanceLoadDiff: LoadRebalanceRequiredMinDiff(&a.st.SV, opts.LoadDimensions...),
				RebalanceImpact:              leaseReplLoad,
			},
		)

		switch noRebalanceReason {
		case noBetterCandidate:
			a.Metrics.LoadBasedLeaseTransferMetrics.CannotFindBetterCandidate.Inc(1)
			log.KvDistribution.VEventf(ctx, 5, "r%d: could not find a better target for lease", leaseRepl.GetRangeID())
			return roachpb.ReplicaDescriptor{}
		case existingNotOverfull:
			a.Metrics.LoadBasedLeaseTransferMetrics.ExistingNotOverfull.Inc(1)
			log.KvDistribution.VEventf(
				ctx, 5, "r%d: existing leaseholder s%d is not overfull",
				leaseRepl.GetRangeID(), leaseRepl.StoreID(),
			)
			return roachpb.ReplicaDescriptor{}
		case deltaNotSignificant:
			a.Metrics.LoadBasedLeaseTransferMetrics.DeltaNotSignificant.Inc(1)
			log.KvDistribution.VEventf(
				ctx, 5,
				"r%d: delta between s%d and the coldest follower (ignoring r%d's lease) is not large enough",
				leaseRepl.GetRangeID(), leaseRepl.StoreID(), leaseRepl.GetRangeID(),
			)
			return roachpb.ReplicaDescriptor{}
		case missingStatsForExistingStore:
			a.Metrics.LoadBasedLeaseTransferMetrics.MissingStatsForExistingStore.Inc(1)
			log.KvDistribution.VEventf(
				ctx, 5, "r%d: missing stats for leaseholder s%d",
				leaseRepl.GetRangeID(), leaseRepl.StoreID(),
			)
			return roachpb.ReplicaDescriptor{}
		case shouldRebalance:
			a.Metrics.LoadBasedLeaseTransferMetrics.ShouldTransfer.Inc(1)
			log.KvDistribution.VEventf(
				ctx,
				5,
				"r%d: should transfer lease load=%s from s%d load=%s to s%d load=%s",
				leaseRepl.GetRangeID(),
				leaseReplLoad,
				leaseRepl.StoreID(),
				storeDescMap[leaseRepl.StoreID()].Capacity.Load(),
				bestStore,
				storeDescMap[bestStore].Capacity.Load(),
			)
		default:
			log.KvDistribution.Fatalf(ctx, "unknown declineReason: %v", noRebalanceReason)
		}

		for _, repl := range validTargets {
			if repl.StoreID == bestStore {
				return repl
			}
		}
		panic("unreachable")
	default:
		log.KvDistribution.Fatalf(ctx, "unexpected lease transfer goal %d", g)
	}
	panic("unreachable")
}

// getCandidateWithMinLoad returns the StoreID that belongs to the store
// serving the lowest load among all the `candidates` stores, given a single
// dimension of load e.g. QPS.
func getCandidateWithMinLoad(
	storeLoadMap map[roachpb.StoreID]load.Load,
	candidates []roachpb.StoreID,
	dimension load.Dimension,
) (bestCandidate roachpb.StoreID) {
	minCandidateLoad := math.MaxFloat64
	for _, store := range candidates {
		candidateLoad, ok := storeLoadMap[store]
		if !ok {
			continue
		}
		candidateLoadDim := candidateLoad.Dim(dimension)
		if minCandidateLoad > candidateLoadDim {
			minCandidateLoad = candidateLoadDim
			bestCandidate = store
		}
	}
	return bestCandidate
}

// getLoadDelta returns the difference between the store serving the highest QPS
// and the store serving the lowest QPS, among the set of stores in the
// `domain`.
func getLoadDelta(
	storeLoadMap map[roachpb.StoreID]load.Load, domain []roachpb.StoreID, dimension load.Dimension,
) float64 {
	maxCandidateLoad := float64(0)
	minCandidateLoad := math.MaxFloat64
	for _, cand := range domain {
		candidateLoad, ok := storeLoadMap[cand]
		if !ok {
			continue
		}
		candidateLoadDim := candidateLoad.Dim(dimension)
		if maxCandidateLoad < candidateLoadDim {
			maxCandidateLoad = candidateLoadDim
		}
		if minCandidateLoad > candidateLoadDim {
			minCandidateLoad = candidateLoadDim
		}
	}
	return maxCandidateLoad - minCandidateLoad
}

// TransferLeaseDecision indicates whether a range lease should be transferred
// and if so, for what reason.
type TransferLeaseDecision int

const (
	_ TransferLeaseDecision = iota
	// DontTransferLeaseCountBalanced indicates the load/lease counts of the
	// valid replica stores are balanced within the target threshold and
	// therefore the lease should not be transferred.
	DontTransferLeaseBalanced
	// DontTransferLeaseNoValidTargets indicates the lease should no be
	// transferred from the current leaseholder because there are no valid
	// leaseholder targets.
	DontTransferLeaseNoValidTargets
	// DontTransferLeaseNoStoreDescriptor indicates the lease should not be
	// transferred because the current leaseholder's store descriptor cannot be
	// found. This can occur on startup, before gossip has propogated the local
	// descriptor.
	DontTransferLeaseNoStoreDescriptor
	// TransferLeaseForCountBalance indicates the lease should be transferred to
	// better balance lease counts.
	TransferLeaseForCountBalance
	// TransferLeaseForAccessLocality indicates the lease should be transferred
	// for better access locality.
	TransferLeaseForAccessLocality
	// TransferLeaseForIOOverload indicates the lease should be transferred
	// because the current leaseholder's store is IO overloaded.
	TransferLeaseForIOOverload
	// TransferLeaseForPreferences indicates the lease should be transferred
	// because there is a more preferred leaseholder according the applied range
	// lease preferences.
	TransferLeaseForPreferences
)

// ShouldTransfer returns true when the lease should be transferred, false
// otherwise.
func (t TransferLeaseDecision) ShouldTransfer() bool {
	switch t {
	case TransferLeaseForCountBalance, TransferLeaseForAccessLocality,
		TransferLeaseForIOOverload, TransferLeaseForPreferences:
		return true
	case DontTransferLeaseBalanced, DontTransferLeaseNoValidTargets,
		DontTransferLeaseNoStoreDescriptor:
		return false
	default:
		panic(fmt.Sprintf("unknown transfer lease decision %d", t))
	}
}

// Priority returns the relative urgency of the lease transfer decision. The
// priority may be used to determine the ordering of lease transfers when
// multiple should occur.
func (t TransferLeaseDecision) Priority() float64 {
	switch t {
	case TransferLeaseForPreferences:
		return 300
	case TransferLeaseForIOOverload:
		return 200
	case TransferLeaseForAccessLocality:
		return 100
	case TransferLeaseForCountBalance:
		return 0
	case DontTransferLeaseBalanced, DontTransferLeaseNoValidTargets,
		DontTransferLeaseNoStoreDescriptor:
		return 0
	default:
		panic(fmt.Sprintf("unknown transfer lease decision %d", t))
	}
}

// SafeValue implements the redact.SafeValue interface.
func (t TransferLeaseDecision) SafeValue() {}

func (t TransferLeaseDecision) String() string {
	switch t {
	case TransferLeaseForCountBalance:
		return "transfer(lease count)"
	case TransferLeaseForAccessLocality:
		return "transfer(access locality)"
	case TransferLeaseForIOOverload:
		return "transfer(io-overload)"
	case TransferLeaseForPreferences:
		return "transfer(preferences)"
	case DontTransferLeaseBalanced:
		return "no-transfer(balanced)"
	case DontTransferLeaseNoStoreDescriptor:
		return "no-transfer(missing store descriptor)"
	case DontTransferLeaseNoValidTargets:
		return "no-transfer(no valid targets)"
	default:
		panic(fmt.Sprintf("unknown transfer lease decision %d", t))
	}
}

// ShouldTransferLease returns true if the specified store is overfull in terms
// of leases with respect to the other stores matching the specified
// attributes.
func (a *Allocator) ShouldTransferLease(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	desc *roachpb.RangeDescriptor,
	conf *roachpb.SpanConfig,
	existing []roachpb.ReplicaDescriptor,
	leaseRepl interface {
		StoreID() roachpb.StoreID
		RaftStatus() *raft.Status
		GetCompactedIndex() kvpb.RaftIndex
		SendStreamStats(*rac2.RangeSendStreamStats)
	},
	usageInfo allocator.RangeUsageInfo,
) TransferLeaseDecision {
	excludeReplsInNeedOfSnap := a.knobs == nil || !a.knobs.AllowLeaseTransfersToReplicasNeedingSnapshots
	if a.LeaseholderShouldMoveDueToPreferences(ctx, storePool, conf, leaseRepl, existing, excludeReplsInNeedOfSnap) {
		return TransferLeaseForPreferences
	}

	if a.leaseholderShouldMoveDueToIOOverload(
		ctx, storePool, existing, leaseRepl.StoreID(), a.IOOverloadOptions()) {
		return TransferLeaseForIOOverload
	}

	existing = a.ValidLeaseTargets(
		ctx,
		storePool,
		desc,
		conf,
		existing,
		leaseRepl,
		allocator.TransferLeaseOptions{},
	)

	// Short-circuit if there are no valid targets out there.
	if len(existing) == 0 || (len(existing) == 1 && existing[0].StoreID == leaseRepl.StoreID()) {
		return DontTransferLeaseNoValidTargets
	}
	source, ok := storePool.GetStoreDescriptor(leaseRepl.StoreID())
	if !ok {
		return DontTransferLeaseNoStoreDescriptor
	}

	sl, _, _ := storePool.GetStoreList(storepool.StoreFilterSuspect)
	sl = sl.ExcludeInvalid(conf.Constraints)
	sl = sl.ExcludeInvalid(conf.VoterConstraints)
	log.KvDistribution.VEventf(ctx, 3, "ShouldTransferLease (lease-holder=s%d):\n%s", leaseRepl.StoreID(), sl)

	transferDec, _ := a.shouldTransferLeaseForAccessLocality(
		ctx,
		storePool,
		source,
		existing,
		usageInfo,
		nil,
		sl.CandidateLeases.Mean,
	)
	var result TransferLeaseDecision
	switch transferDec {
	case shouldNotTransfer:
		result = DontTransferLeaseBalanced
	case shouldTransfer:
		result = TransferLeaseForAccessLocality
	case decideWithoutStats:
		if a.shouldTransferLeaseForLeaseCountConvergence(ctx, storePool, sl, source, existing) {
			result = TransferLeaseForCountBalance
		} else {
			result = DontTransferLeaseBalanced
		}
	default:
		log.KvDistribution.Fatalf(ctx, "unexpected transfer decision %d", transferDec)
	}
	log.KvDistribution.VEventf(
		ctx, 3, "ShouldTransferLease decision (lease-holder=s%d): %v", leaseRepl.StoreID(), result,
	)
	return result
}

// FollowTheWorkloadPrefersLocal returns whether following the
// follow-the-workload strategy would still prefer selecting the local store.
func (a Allocator) FollowTheWorkloadPrefersLocal(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	sl storepool.StoreList,
	source roachpb.StoreDescriptor,
	candidate roachpb.StoreID,
	existing []roachpb.ReplicaDescriptor,
	usageInfo allocator.RangeUsageInfo,
) bool {
	adjustments := make(map[roachpb.StoreID]float64)
	decision, _ := a.shouldTransferLeaseForAccessLocality(ctx, storePool, source, existing, usageInfo, adjustments, sl.CandidateLeases.Mean)
	if decision == decideWithoutStats {
		return false
	}
	adjustment := adjustments[candidate]
	baseLoadBasedLeaseRebalanceThreshold := getBaseLoadBasedLeaseRebalanceThreshold(LeaseRebalanceThreshold.Get(&a.st.SV))
	if adjustment > baseLoadBasedLeaseRebalanceThreshold {
		log.KvDistribution.VEventf(ctx, 3,
			"s%d is a better fit than s%d due to follow-the-workload (score: %.2f; threshold: %.2f)",
			source.StoreID, candidate, adjustment, baseLoadBasedLeaseRebalanceThreshold)
		return true
	}
	return false
}

func (a Allocator) shouldTransferLeaseForAccessLocality(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	source roachpb.StoreDescriptor,
	existing []roachpb.ReplicaDescriptor,
	usageInfo allocator.RangeUsageInfo,
	rebalanceAdjustments map[roachpb.StoreID]float64,
	candidateLeasesMean float64,
) (accessLocalityTransferDecision, roachpb.ReplicaDescriptor) {
	// Only use load-based rebalancing if it's enabled and we have both
	// stats and locality information to base our decision on.
	if usageInfo.RequestLocality == nil ||
		!EnableLoadBasedLeaseRebalancing.Get(&a.st.SV) {
		return decideWithoutStats, roachpb.ReplicaDescriptor{}
	}
	replicaLocalities := storePool.GetLocalitiesByNode(existing)
	for _, locality := range replicaLocalities {
		if len(locality.Tiers) == 0 {
			return decideWithoutStats, roachpb.ReplicaDescriptor{}
		}
	}

	qpsStats := usageInfo.RequestLocality.Counts
	qpsStatsDur := usageInfo.RequestLocality.Duration

	// If we haven't yet accumulated enough data, avoid transferring for now,
	// unless we've been explicitly asked otherwise. Do not fall back to the
	// algorithm that doesn't use stats, since it can easily start fighting with
	// the stats-based algorithm. This provides some amount of safety from lease
	// thrashing, since leases cannot transfer more frequently than this threshold
	// (because replica stats get reset upon lease transfer).
	if qpsStatsDur < MinLeaseTransferStatsDuration {
		return shouldNotTransfer, roachpb.ReplicaDescriptor{}
	}

	// On the other hand, if we don't have any stats with associated localities,
	// then do fall back to the algorithm that doesn't use request stats.
	delete(qpsStats, "")
	if len(qpsStats) == 0 {
		return decideWithoutStats, roachpb.ReplicaDescriptor{}
	}

	replicaWeights := make(map[roachpb.NodeID]float64)
	for requestLocalityStr, qps := range qpsStats {
		var requestLocality roachpb.Locality
		if err := requestLocality.Set(requestLocalityStr); err != nil {
			log.KvDistribution.Errorf(ctx, "unable to parse locality string %q: %+v", requestLocalityStr, err)
			continue
		}
		for nodeID, replicaLocality := range replicaLocalities {
			// Add weights to each replica based on the number of requests from
			// that replica's locality and neighboring localities.
			replicaWeights[nodeID] += (1 - replicaLocality.DiversityScore(requestLocality)) * qps
		}
	}

	log.KvDistribution.VEventf(ctx, 5,
		"shouldTransferLease qpsStats: %+v, replicaLocalities: %+v, replicaWeights: %+v",
		qpsStats, replicaLocalities, replicaWeights)
	sourceWeight := math.Max(minReplicaWeight, replicaWeights[source.Node.NodeID])

	// TODO(a-robinson): This may not have enough protection against all leases
	// ending up on a single node in extreme cases. Continue testing against
	// different situations.
	var bestRepl roachpb.ReplicaDescriptor
	bestReplScore := int32(math.MinInt32)
	for _, repl := range existing {
		if repl.NodeID == source.Node.NodeID {
			continue
		}
		storeDesc, ok := storePool.GetStoreDescriptor(repl.StoreID)
		if !ok {
			continue
		}
		remoteLatency, ok := a.nodeLatencyFn(repl.NodeID)
		if !ok {
			continue
		}

		remoteWeight := math.Max(minReplicaWeight, replicaWeights[repl.NodeID])
		replScore, rebalanceAdjustment := loadBasedLeaseRebalanceScore(
			ctx, a.st, remoteWeight, remoteLatency, storeDesc, sourceWeight, source, candidateLeasesMean)
		if replScore > bestReplScore {
			bestReplScore = replScore
			bestRepl = repl
		}
		if rebalanceAdjustments != nil {
			rebalanceAdjustments[repl.StoreID] = rebalanceAdjustment
		}
	}

	if bestReplScore > 0 {
		return shouldTransfer, bestRepl
	}

	// Return the best replica even in cases where transferring is not advised in
	// order to support forced lease transfers, such as when removing a replica or
	// draining all leases before shutdown.
	return shouldNotTransfer, bestRepl
}

// loadBasedLeaseRebalanceScore attempts to give a score to how desirable it
// would be to transfer a range lease from the local store to a remote store.
// It does so using a formula based on the latency between the stores and
// a number that we call the "weight" of each replica, which represents how
// many requests for the range have been coming from localities near the
// replica.
//
// The overarching goal is to move leases towards where requests are coming
// from when the latency between localities is high, because the leaseholder
// being near the request gateway makes for lower request latencies.
// This must be balanced against hurting throughput by putting too many leases
// one just a few nodes, though, which is why we get progressively more
// aggressive about moving the leases toward requests when latencies are high.
//
// The calculations below were determined via a bunch of manual testing (see
// #13232 or the leaseholder_locality.md RFC for more details), but the general
// logic behind each part of the formula is as follows:
//
//   - LeaseRebalancingAggressiveness: Allow the aggressiveness to be tuned via
//     a cluster setting.
//   - 0.1: Constant factor to reduce aggressiveness by default
//   - math.Log10(remoteWeight/sourceWeight): Comparison of the remote replica's
//     weight to the local replica's weight. Taking the log of the ratio instead
//     of using the ratio directly makes things symmetric -- i.e. r1 comparing
//     itself to r2 will come to the same conclusion as r2 comparing itself to r1.
//   - math.Log1p(remoteLatencyMillis): This will be 0 if there's no latency,
//     removing the weight/latency factor from consideration. Otherwise, it grows
//     the aggressiveness for stores that are farther apart. Note that Log1p grows
//     faster than Log10 as its argument gets larger, which is intentional to
//     increase the importance of latency.
//   - overfullScore and underfullScore: rebalanceThreshold helps us get an idea
//     of the ideal number of leases on each store. We then calculate these to
//     compare how close each node is to its ideal state and use the differences
//     from the ideal state on each node to compute a final score.
//
// Returns a total score for the replica that takes into account the number of
// leases already on each store. Also returns the raw "adjustment" value that's
// purely based on replica weights and latency in order for the caller to
// determine how large a role the user's workload played in the decision.  The
// adjustment value is positive if the remote store is preferred for load-based
// reasons or negative if the local store is preferred. The magnitude depends
// on the difference in load and the latency between the nodes.
//
// TODO(a-robinson): Should this be changed to avoid even thinking about lease
// counts now that we try to spread leases and replicas based on QPS? As is it
// may fight back a little bit against store-level QPS-based rebalancing.
func loadBasedLeaseRebalanceScore(
	ctx context.Context,
	st *cluster.Settings,
	remoteWeight float64,
	remoteLatency time.Duration,
	remoteStore roachpb.StoreDescriptor,
	sourceWeight float64,
	source roachpb.StoreDescriptor,
	meanLeases float64,
) (int32, float64) {
	remoteLatencyMillis := float64(remoteLatency) / float64(time.Millisecond)
	rebalanceAdjustment :=
		leaseRebalancingAggressiveness.Get(&st.SV) * 0.1 * math.Log10(remoteWeight/sourceWeight) * math.Log1p(remoteLatencyMillis)
	// Start with twice the base rebalance threshold in order to fight more
	// strongly against thrashing caused by small variances in the distribution
	// of request weights.
	rebalanceThreshold := getBaseLoadBasedLeaseRebalanceThreshold(LeaseRebalanceThreshold.Get(&st.SV)) - rebalanceAdjustment

	overfullLeaseThreshold := int32(math.Ceil(meanLeases * (1 + rebalanceThreshold)))
	overfullScore := source.Capacity.LeaseCount - overfullLeaseThreshold
	underfullLeaseThreshold := int32(math.Floor(meanLeases * (1 - rebalanceThreshold)))
	underfullScore := underfullLeaseThreshold - remoteStore.Capacity.LeaseCount
	totalScore := overfullScore + underfullScore

	log.KvDistribution.VEventf(
		ctx,
		5,
		"node: %d, sourceWeight: %.2f, remoteWeight: %.2f, remoteLatency: %v, "+
			"rebalanceThreshold: %.2f, meanLeases: %.2f, sourceLeaseCount: %d, overfullThreshold: %d, "+
			"remoteLeaseCount: %d, underfullThreshold: %d, totalScore: %d",
		remoteStore.Node.NodeID, sourceWeight, remoteWeight, remoteLatency,
		rebalanceThreshold, meanLeases, source.Capacity.LeaseCount, overfullLeaseThreshold,
		remoteStore.Capacity.LeaseCount, underfullLeaseThreshold, totalScore,
	)
	return totalScore, rebalanceAdjustment
}

func (a Allocator) shouldTransferLeaseForLeaseCountConvergence(
	ctx context.Context,
	storePool storepool.AllocatorStorePool,
	sl storepool.StoreList,
	source roachpb.StoreDescriptor,
	existing []roachpb.ReplicaDescriptor,
) bool {
	// TODO(a-robinson): Should we disable this behavior when load-based lease
	// rebalancing is enabled? In happy cases it's nice to keep this working
	// to even out the number of leases in addition to the number of replicas,
	// but it's certainly a blunt instrument that could undo what we want.

	// Allow lease transfer if we're above the overfull threshold, which is
	// mean*(1+LeaseRebalanceThreshold).
	leaseRebalanceThreshold := LeaseRebalanceThreshold.Get(&a.st.SV)
	overfullLeaseThreshold := int32(math.Ceil(sl.CandidateLeases.Mean * (1 + leaseRebalanceThreshold)))
	minOverfullThreshold := int32(math.Ceil(sl.CandidateLeases.Mean + LeaseRebalanceThresholdMin))
	if overfullLeaseThreshold < minOverfullThreshold {
		overfullLeaseThreshold = minOverfullThreshold
	}
	if source.Capacity.LeaseCount > overfullLeaseThreshold {
		return true
	}

	if float64(source.Capacity.LeaseCount) > sl.CandidateLeases.Mean {
		underfullLeaseThreshold := int32(math.Ceil(sl.CandidateLeases.Mean * (1 - leaseRebalanceThreshold)))
		minUnderfullThreshold := int32(math.Ceil(sl.CandidateLeases.Mean - LeaseRebalanceThresholdMin))
		if underfullLeaseThreshold > minUnderfullThreshold {
			underfullLeaseThreshold = minUnderfullThreshold
		}

		for _, repl := range existing {
			storeDesc, ok := storePool.GetStoreDescriptor(repl.StoreID)
			if !ok {
				continue
			}
			if storeDesc.Capacity.LeaseCount < underfullLeaseThreshold {
				return true
			}
		}
	}
	return false
}

// PreferredLeaseholders returns a slice of replica descriptors corresponding to
// replicas that meet lease preferences (among the `existing` replicas).
func (a Allocator) PreferredLeaseholders(
	storePool storepool.AllocatorStorePool,
	conf *roachpb.SpanConfig,
	existing []roachpb.ReplicaDescriptor,
) []roachpb.ReplicaDescriptor {
	// Go one preference at a time. As soon as we've found replicas that match a
	// preference, we don't need to look at the later preferences, because
	// they're meant to be ordered by priority.
	for _, preference := range conf.LeasePreferences {
		var preferred []roachpb.ReplicaDescriptor
		for _, repl := range existing {
			// TODO(a-robinson): Do all these lookups at once, up front? We could
			// easily be passing a slice of StoreDescriptors around all the Allocator
			// functions instead of ReplicaDescriptors.
			storeDesc, ok := storePool.GetStoreDescriptor(repl.StoreID)
			if !ok {
				continue
			}
			if constraint.CheckStoreConjunction(storeDesc, preference.Constraints) {
				preferred = append(preferred, repl)
			}
		}
		if len(preferred) > 0 {
			return preferred
		}
	}
	return nil
}

// computeQuorum computes the quorum value for the given number of nodes.
func computeQuorum(nodes int) int {
	return (nodes / 2) + 1
}

// FilterBehindReplicas removes any "behind" replicas from the supplied
// slice. A "behind" replica is one which is not at or past the quorum commit
// index.
func FilterBehindReplicas(
	ctx context.Context, st *raft.Status, replicas []roachpb.ReplicaDescriptor,
) []roachpb.ReplicaDescriptor {
	var candidates []roachpb.ReplicaDescriptor
	for _, r := range replicas {
		if !raftutil.ReplicaIsBehind(st, r.ReplicaID) {
			candidates = append(candidates, r)
		}
	}
	return candidates
}

// excludeReplicasInNeedOfSnapshots filters out the `replicas` that may be in
// need of a raft snapshot. VOTER_INCOMING replicas are not filtered out.
// Other replicas may be filtered out if this function is called with the
// `raftStatus` of a non-raft leader replica.
func excludeReplicasInNeedOfSnapshots(
	ctx context.Context,
	st *raft.Status,
	compacted kvpb.RaftIndex,
	replicas []roachpb.ReplicaDescriptor,
) []roachpb.ReplicaDescriptor {
	filled := 0
	for _, repl := range replicas {
		snapStatus := raftutil.ReplicaMayNeedSnapshot(st, compacted, repl.ReplicaID)
		if snapStatus != raftutil.NoSnapshotNeeded {
			log.KvDistribution.VEventf(
				ctx,
				5,
				"not considering %s as a potential candidate for a lease transfer"+
					" because the replica may be waiting for a snapshot: %s",
				repl, snapStatus,
			)
			continue
		}
		replicas[filled] = repl
		filled++
	}
	return replicas[:filled]
}

// sendStreamStatsPool is a pool of RangeSendStreamStats objects, used to avoid
// churning memory when computing lease transfer decisions.
var sendStreamStatsPool = sync.Pool{
	New: func() interface{} {
		return &rac2.RangeSendStreamStats{}
	},
}

// excludeReplicasInNeedOfCatchup filters out the `replicas` that may be in
// need of a catchup messages before able to apply the lease, based on the
// provided RangeSendStreamStats.
func excludeReplicasInNeedOfCatchup(
	ctx context.Context,
	sendStreamStats func(*rac2.RangeSendStreamStats),
	replicas []roachpb.ReplicaDescriptor,
) []roachpb.ReplicaDescriptor {
	if sendStreamStats == nil {
		// When we don't have stats, we can't make an informed decision about which
		// replicas are behind. We'll just return the replicas as is. This can
		// occur if the current leaseholder is not yet the raft leader, or only
		// recently became one (concurrent to the lease transfer decision).
		return replicas
	}
	stats := sendStreamStatsPool.Get().(*rac2.RangeSendStreamStats)
	stats.Clear()
	defer sendStreamStatsPool.Put(stats)
	sendStreamStats(stats)
	filled := 0
	for _, repl := range replicas {
		if replicaSendStreamStats, ok := stats.ReplicaSendStreamStats(repl.ReplicaID); ok &&
			(!replicaSendStreamStats.IsStateReplicate || replicaSendStreamStats.HasSendQueue) {
			log.KvDistribution.VEventf(ctx, 5,
				"not considering %v as a potential candidate for a lease transfer "+
					"because the replica requires catchup: "+
					"replica=(%v) range=%v",
				repl, replicaSendStreamStats, stats)
			continue
		} else if ok {
			log.KvDistribution.VEventf(ctx, 6,
				"replica %v is up-to-date and does not require catchup "+
					"replica=(%v) range=%v",
				repl, replicaSendStreamStats, stats)
		} else {
			log.KvDistribution.VEventf(ctx, 4,
				"replica %v is not in the send stream stats range=%v",
				repl, stats)
		}
		// We are also not excluding any replicas which weren't included in the
		// stats here. If they weren't included it indicates that they were either
		// recently added or removed and in either case we don't know enough to
		// preclude them as lease transfer targets.
		replicas[filled] = repl
		filled++
	}
	return replicas[:filled]
}

// simulateFilterUnremovableReplicas removes any unremovable replicas from the
// supplied slice. Unlike FilterUnremovableReplicas, brandNewReplicaID is
// considered up-to-date (and thus can participate in quorum), but is not
// considered a candidate for removal.
func simulateFilterUnremovableReplicas(
	ctx context.Context,
	raftStatus *raft.Status,
	replicas []roachpb.ReplicaDescriptor,
	brandNewReplicaID roachpb.ReplicaID,
) []roachpb.ReplicaDescriptor {
	status := *raftStatus
	status.Progress[raftpb.PeerID(brandNewReplicaID)] = tracker.Progress{
		State: tracker.StateReplicate,
		Match: status.Commit,
	}
	return FilterUnremovableReplicas(ctx, &status, replicas, brandNewReplicaID)
}

// FilterUnremovableReplicas removes any unremovable replicas from the supplied
// slice. An unremovable replica is one which is a necessary part of the
// quorum that will result from removing 1 replica. We forgive brandNewReplicaID
// for being behind, since a new range can take a little while to catch up.
// This is important when we've just added a replica in order to rebalance to
// it (#17879).
func FilterUnremovableReplicas(
	ctx context.Context,
	raftStatus *raft.Status,
	replicas []roachpb.ReplicaDescriptor,
	brandNewReplicaID roachpb.ReplicaID,
) []roachpb.ReplicaDescriptor {
	upToDateReplicas := FilterBehindReplicas(ctx, raftStatus, replicas)
	return FilterUnremovableReplicasWithoutRaftStatus(
		ctx, replicas, upToDateReplicas, brandNewReplicaID)
}

// FilterUnremovableReplicasWithoutRaftStatus is like FilterUnremovableReplicas,
// but takes an upToDateReplicas slice to avoid the Raft status dependency.
func FilterUnremovableReplicasWithoutRaftStatus(
	ctx context.Context,
	replicas []roachpb.ReplicaDescriptor,
	upToDateReplicas []roachpb.ReplicaDescriptor,
	brandNewReplicaID roachpb.ReplicaID,
) []roachpb.ReplicaDescriptor {
	oldQuorum := computeQuorum(len(replicas))
	if len(upToDateReplicas) < oldQuorum {
		// The number of up-to-date replicas is less than the old quorum. No
		// replicas can be removed. A below quorum range won't be able to process a
		// replica removal in any case. The logic here prevents any attempt to even
		// try the removal.
		return nil
	}

	newQuorum := computeQuorum(len(replicas) - 1)
	if len(upToDateReplicas) > newQuorum {
		// The number of up-to-date replicas is larger than the new quorum. Any
		// replica can be removed, though we want to filter out brandNewReplicaID.
		if brandNewReplicaID != 0 {
			candidates := make([]roachpb.ReplicaDescriptor, 0, len(replicas)-len(upToDateReplicas))
			for _, r := range replicas {
				if r.ReplicaID != brandNewReplicaID {
					candidates = append(candidates, r)
				}
			}
			return candidates
		}
		return replicas
	}

	// The number of up-to-date replicas is equal to the new quorum. Only allow
	// removal of behind replicas (except for brandNewReplicaID which is given a
	// free pass).
	candidates := make([]roachpb.ReplicaDescriptor, 0, len(replicas)-len(upToDateReplicas))
	necessary := func(r roachpb.ReplicaDescriptor) bool {
		if r.ReplicaID == brandNewReplicaID {
			return true
		}
		for _, t := range upToDateReplicas {
			if t == r {
				return true
			}
		}
		return false
	}
	for _, r := range replicas {
		if !necessary(r) {
			candidates = append(candidates, r)
		}
	}
	return candidates
}

func maxReplicaID(replicas []roachpb.ReplicaDescriptor) roachpb.ReplicaID {
	var max roachpb.ReplicaID
	for i := range replicas {
		if replicaID := replicas[i].ReplicaID; replicaID > max {
			max = replicaID
		}
	}
	return max
}

func replDescsToStoreIDs(descs []roachpb.ReplicaDescriptor) []roachpb.StoreID {
	ret := make([]roachpb.StoreID, len(descs))
	for i, desc := range descs {
		ret[i] = desc.StoreID
	}
	return ret
}
