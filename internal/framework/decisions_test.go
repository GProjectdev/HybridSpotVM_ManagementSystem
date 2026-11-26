package framework

import (
	"testing"
	"time"
)

func TestCloudPreparationTiming(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ wait, lead, expected int64 }{{600, 600, 0}, {1800, 600, 1200}, {0, 600, 0}, {300, 600, 0}, {1800, 0, 1800}} {
		got, err := PreparationAt(t0, tc.wait, tc.lead)
		if err != nil || got.Sub(t0) != time.Duration(tc.expected)*time.Second {
			t.Fatalf("%+v: %v %v", tc, got, err)
		}
	}
	if _, err := PreparationAt(time.Time{}, 600, 600); err == nil {
		t.Fatal("accepted missing shortage time")
	}
}

func TestOnPremWinsDuringPreparationAndCommitIsSticky(t *testing.T) {
	t0 := time.Now().UTC()
	o := ActivationObservation{Now: t0.Add(25 * time.Minute), ShortageAt: t0, WaitSeconds: 1800, PreparationSeconds: 600, CloudAllowed: true, ResourceShortage: true, CloudReady: true, OnPremCluster: "onpre1", CloudCluster: "aws1"}
	d, err := DecideActivation(o)
	if err != nil || !d.PrepareCloud || d.AllowCloud {
		t.Fatalf("early cloud admission: %+v %v", d, err)
	}
	o.Now = t0.Add(30 * time.Minute)
	d, _ = DecideActivation(o)
	if !d.AllowCloud {
		t.Fatal("ready cloud not eligible at wait boundary")
	}
	o.OnPremAdmitted = true
	d, _ = DecideActivation(o)
	if !d.RetireUnusedCloud || d.AllowCloud {
		t.Fatal("on-prem admission must win")
	}
	o.CommittedCluster = "aws1"
	d, _ = DecideActivation(o)
	if !d.AllowCloud || d.RetireUnusedCloud {
		t.Fatal("must not migrate a committed cloud execution back implicitly")
	}
	o.CommittedCluster = "foreign"
	if _, err = DecideActivation(o); err == nil {
		t.Fatal("foreign commit accepted")
	}
}

func TestNonResourceErrorsNeverTriggerCloud(t *testing.T) {
	o := ActivationObservation{Now: time.Now(), ShortageAt: time.Now().Add(-time.Hour), WaitSeconds: 600, PreparationSeconds: 600, CloudAllowed: true, OnPremCluster: "onpre1", CloudCluster: "aws1"}
	d, err := DecideActivation(o)
	if err != nil || d.PrepareCloud {
		t.Fatal("unknown/volume/image error treated as capacity shortage")
	}
}

func TestCheckpointBoundsDoNotFlattenRiskPolicy(t *testing.T) {
	for _, tc := range []struct {
		user, dynamic int64
		spot, fresh   bool
		want          int64
		bad           bool
	}{
		{7200, 0, false, false, 3600, false}, {300, 0, false, false, 300, false},
		{3600, 120, true, true, 120, false}, {120, 600, true, true, 120, false},
		{3600, 9000, true, true, 3600, false}, {3600, 120, true, false, 0, true}, {0, 120, true, true, 0, true},
	} {
		got, err := CheckpointInterval(tc.user, tc.dynamic, tc.spot, tc.fresh)
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
	}
}

func recoveryFixture() TransitionEvidence {
	now := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	return TransitionEvidence{Mode: "PlannedReplacement", Now: now, TargetReadyAt: now.Add(-10 * time.Minute), TargetInfrastructureReady: true, TargetWorkloadReady: true, WorkloadUID: "work-uid", ExecutionID: "exec-1", WorldSize: 4, Archives: []Archive{{ID: "complete", WorkloadUID: "work-uid", ExecutionID: "exec-1", WorldSize: 4, DurableRanks: 4, CommonStep: 100, RequestedAt: now.Add(-5 * time.Minute), DurableAt: now.Add(-4 * time.Minute), IntegrityVerified: true}}}
}

func TestPlannedReplacementOrdersPreparationBeforeFreshCheckpoint(t *testing.T) {
	e := recoveryFixture()
	e.TargetWorkloadReady = false
	d, err := DecideTransition(e)
	if err != nil || !d.PrepareTarget || d.RequestCheckpoint || d.RequestRestore {
		t.Fatalf("%+v %v", d, err)
	}
	e.TargetWorkloadReady = true
	e.Archives[0].RequestedAt = e.TargetReadyAt.Add(-time.Minute)
	d, _ = DecideTransition(e)
	if !d.RequestCheckpoint || d.RequestRestore {
		t.Fatal("old archive used for planned replacement")
	}
	e.Archives[0].RequestedAt = e.TargetReadyAt
	d, _ = DecideTransition(e)
	if !d.RequireFence || d.RequestRestore {
		t.Fatal("restore bypassed fencing")
	}
	e.SourceFenced = true
	d, _ = DecideTransition(e)
	if !d.RequestRestore || d.ReleaseSource {
		t.Fatal("source retired before restore")
	}
	e.RestoreCompleted = true
	d, _ = DecideTransition(e)
	if d.ReleaseSource {
		t.Fatal("restore engine success without rank progress accepted")
	}
	e.AllRanksProgressed = true
	d, _ = DecideTransition(e)
	if !d.ReleaseSource || d.Phase == "Completed" {
		t.Fatal("retirement confirmation skipped")
	}
	e.OldResourcesTerminated = true
	d, _ = DecideTransition(e)
	if d.Phase != "Completed" {
		t.Fatal(d)
	}
}

func TestPreemptionPreparesInParallelAndFallsBackAfterEmergencyFailure(t *testing.T) {
	e := recoveryFixture()
	e.Mode = "Preemption"
	e.TargetWorkloadReady = false
	d, _ := DecideTransition(e)
	if !d.PrepareTarget || !d.RequestCheckpoint {
		t.Fatal("emergency save must overlap target preparation")
	}
	e.TargetWorkloadReady = true
	e.Archives[0].RequestedAt = e.TargetReadyAt.Add(-20 * time.Minute)
	e.Archives = append(e.Archives, Archive{ID: "partial-new", WorkloadUID: e.WorkloadUID, ExecutionID: e.ExecutionID, CommonStep: 200, WorldSize: 4, DurableRanks: 3, RequestedAt: e.Now.Add(-time.Minute), DurableAt: e.Now, IntegrityVerified: true})
	d, _ = DecideTransition(e)
	if d.ArchiveID != "" || !d.RequestCheckpoint {
		t.Fatal("emergency attempt not finished")
	}
	e.EmergencyAttemptFinished = true
	d, _ = DecideTransition(e)
	if d.ArchiveID != "complete" || !d.RequireFence {
		t.Fatalf("fallback did not preserve last complete archive: %+v", d)
	}
}

func TestInvalidArchivesAndMissingFenceRemainBlocked(t *testing.T) {
	for _, mutate := range []func(*Archive){
		func(a *Archive) { a.WorkloadUID = "other" }, func(a *Archive) { a.ExecutionID = "old" },
		func(a *Archive) { a.WorldSize = 2 }, func(a *Archive) { a.DurableRanks = 3 },
		func(a *Archive) { a.IntegrityVerified = false }, func(a *Archive) { a.DurableAt = a.DurableAt.Add(2 * time.Hour) },
		func(a *Archive) { a.RequestedAt = time.Time{} }, func(a *Archive) { a.CommonStep = -1 },
	} {
		e := recoveryFixture()
		mutate(&e.Archives[0])
		d, err := DecideTransition(e)
		if err != nil || d.ArchiveID != "" || d.RequestRestore || d.ReleaseSource {
			t.Fatalf("invalid archive admitted: %+v %v", d, err)
		}
	}
	e := recoveryFixture()
	e.Mode = "Failover"
	e.EmergencyAttemptFinished = true
	e.RestoreCompleted = true
	e.AllRanksProgressed = true
	d, _ := DecideTransition(e)
	if d.RequestRestore || d.ReleaseSource || !d.RequireFence {
		t.Fatal("unreachable source cannot substitute for fencing")
	}
}
