// Package framework implements the On-Premise-first control contract. It reuses
// the existing provisioner and checkpoint/restore engines; it is not a scheduler.
package framework

import (
	"fmt"
	"time"
)

const DefaultWaitSeconds int64 = 600
const DefaultPreparationSeconds int64 = 600
const MaximumCheckpointSeconds int64 = 3600

// PreparationAt is anchored to the first verified resource-shortage observation,
// not to controller startup or the latest reconcile. Zero wait is intentional.
func PreparationAt(shortageAt time.Time, waitSeconds, preparationSeconds int64) (time.Time, error) {
	if shortageAt.IsZero() || waitSeconds < 0 || preparationSeconds < 0 || waitSeconds > 604800 || preparationSeconds > 86400 {
		return time.Time{}, fmt.Errorf("invalid shortage timestamp or wait/preparation duration")
	}
	delay := waitSeconds - preparationSeconds
	if delay < 0 {
		delay = 0
	}
	return shortageAt.Add(time.Duration(delay) * time.Second), nil
}

// CheckpointInterval preserves a user's tighter recovery requirement. A missing
// hazard estimate must never be interpreted as measured zero hazard.
func CheckpointInterval(userSeconds, dynamicSeconds int64, spot bool, riskFresh bool) (int64, error) {
	if userSeconds <= 0 {
		return 0, fmt.Errorf("positive user checkpoint interval required")
	}
	interval := min(userSeconds, MaximumCheckpointSeconds)
	if spot {
		if !riskFresh || dynamicSeconds <= 0 {
			return 0, fmt.Errorf("Spot checkpoint decision requires a valid risk-policy result")
		}
		interval = min(interval, dynamicSeconds)
	}
	return interval, nil
}

type ActivationObservation struct {
	Now, ShortageAt                                            time.Time
	WaitSeconds, PreparationSeconds                            int64
	CloudAllowed, ResourceShortage, OnPremAdmitted, CloudReady bool
	// Once any execution is committed, disappearance is a recovery event. It
	// must not return to the initial placement path and start a second world.
	CommittedCluster            string
	OnPremCluster, CloudCluster string
}

type ActivationDecision struct {
	Phase                                       string
	PrepareCloud, AllowCloud, RetireUnusedCloud bool
	PrepareAt                                   time.Time
}

func DecideActivation(o ActivationObservation) (ActivationDecision, error) {
	if o.OnPremCluster == "" || (o.CloudAllowed && (o.CloudCluster == "" || o.OnPremCluster == o.CloudCluster)) {
		return ActivationDecision{}, fmt.Errorf("distinct configured member clusters required")
	}
	if o.CommittedCluster != "" {
		if o.CommittedCluster != o.OnPremCluster && o.CommittedCluster != o.CloudCluster {
			return ActivationDecision{}, fmt.Errorf("foreign committed cluster")
		}
		return ActivationDecision{Phase: "ExecutionCommitted", AllowCloud: o.CommittedCluster == o.CloudCluster, RetireUnusedCloud: o.CommittedCluster == o.OnPremCluster}, nil
	}
	if o.OnPremAdmitted {
		return ActivationDecision{Phase: "OnPremAdmitted", RetireUnusedCloud: true}, nil
	}
	if !o.CloudAllowed || !o.ResourceShortage {
		return ActivationDecision{Phase: "WaitingOnPrem"}, nil
	}
	at, err := PreparationAt(o.ShortageAt, o.WaitSeconds, o.PreparationSeconds)
	if err != nil {
		return ActivationDecision{}, err
	}
	d := ActivationDecision{Phase: "WaitingOnPrem", PrepareAt: at}
	if !o.Now.Before(at) {
		d.PrepareCloud = true
		d.Phase = "PreparingCloud"
	}
	// A faster-than-estimated VM must not shorten a user's requested wait.
	if d.PrepareCloud && o.CloudReady && !o.Now.Before(o.ShortageAt.Add(time.Duration(o.WaitSeconds)*time.Second)) {
		d.AllowCloud = true
		d.Phase = "CloudEligible"
	}
	return d, nil
}

type Archive struct {
	ID, WorkloadUID, ExecutionID        string
	CommonStep, WorldSize, DurableRanks int64
	RequestedAt, DurableAt              time.Time
	IntegrityVerified                   bool
}

// SelectArchive never falls forward to a partially written emergency archive.
// Planned replacement additionally requires a request after target preparation.
func SelectArchive(candidates []Archive, workloadUID, executionID string, worldSize int64, now, requestedAfter time.Time) (Archive, error) {
	if workloadUID == "" || executionID == "" || worldSize < 1 {
		return Archive{}, fmt.Errorf("archive identity required")
	}
	var best Archive
	for _, a := range candidates {
		if a.ID == "" || a.WorkloadUID != workloadUID || a.ExecutionID != executionID || a.WorldSize != worldSize || a.DurableRanks != worldSize || !a.IntegrityVerified || a.CommonStep < 0 || a.RequestedAt.IsZero() || a.DurableAt.IsZero() || a.DurableAt.Before(a.RequestedAt) || a.DurableAt.After(now) {
			continue
		}
		if !requestedAfter.IsZero() && a.RequestedAt.Before(requestedAfter) {
			continue
		}
		if best.ID == "" || a.CommonStep > best.CommonStep || (a.CommonStep == best.CommonStep && a.DurableAt.After(best.DurableAt)) {
			best = a
		}
	}
	if best.ID == "" {
		return Archive{}, fmt.Errorf("no complete durable archive for this execution")
	}
	return best, nil
}

type TransitionEvidence struct {
	Mode                                           string // PlannedReplacement, Preemption, Failover
	Now, TargetReadyAt                             time.Time
	TargetInfrastructureReady, TargetWorkloadReady bool
	EmergencyAttemptFinished                       bool
	WorkloadUID, ExecutionID                       string
	WorldSize                                      int64
	Archives                                       []Archive
	// Evidence is assembled from UID-bound engine/provider observations, never
	// from a user-writable annotation asserting that the old source is dead.
	SourceFenced, RestoreCompleted, AllRanksProgressed, OldResourcesTerminated bool
}

type TransitionDecision struct {
	Phase                                                                         string
	PrepareTarget, RequestCheckpoint, RequireFence, RequestRestore, ReleaseSource bool
	ArchiveID                                                                     string
}

func DecideTransition(e TransitionEvidence) (TransitionDecision, error) {
	if e.Mode != "PlannedReplacement" && e.Mode != "Preemption" && e.Mode != "Failover" {
		return TransitionDecision{}, fmt.Errorf("unsupported transition mode")
	}
	if e.WorkloadUID == "" || e.ExecutionID == "" || e.WorldSize < 1 {
		return TransitionDecision{}, fmt.Errorf("transition execution identity required")
	}
	planned := e.Mode == "PlannedReplacement"
	if !e.TargetInfrastructureReady || !e.TargetWorkloadReady || e.TargetReadyAt.IsZero() || e.TargetReadyAt.After(e.Now) {
		return TransitionDecision{Phase: "PreparingTarget", PrepareTarget: true, RequestCheckpoint: !planned && !e.EmergencyAttemptFinished}, nil
	}
	after := time.Time{}
	if planned {
		after = e.TargetReadyAt
	}
	a, err := SelectArchive(e.Archives, e.WorkloadUID, e.ExecutionID, e.WorldSize, e.Now, after)
	if !planned && !e.EmergencyAttemptFinished {
		return TransitionDecision{Phase: "EmergencyCheckpoint", RequestCheckpoint: true}, nil
	}
	if err != nil {
		return TransitionDecision{Phase: "CheckpointRequired", RequestCheckpoint: planned}, nil
	}
	d := TransitionDecision{Phase: "FencingSource", ArchiveID: a.ID, RequireFence: true}
	if !e.SourceFenced {
		return d, nil
	}
	d.RequireFence = false
	d.Phase = "Restoring"
	d.RequestRestore = true
	if !e.RestoreCompleted || !e.AllRanksProgressed {
		return d, nil
	}
	d.RequestRestore = false
	d.Phase = "RetiringSource"
	d.ReleaseSource = true
	if e.OldResourcesTerminated {
		d.Phase = "Completed"
		d.ReleaseSource = false
	}
	return d, nil
}
