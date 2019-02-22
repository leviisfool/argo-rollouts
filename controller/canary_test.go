package controller

import (
	"fmt"
	"strconv"
	"testing"
	"time"
	"encoding/json"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	testclient "k8s.io/client-go/testing"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo-rollouts/utils/annotations"
	"github.com/argoproj/argo-rollouts/utils/conditions"
)

func newCanaryRolloutWithStatus(name string, replicas int, revisionHistoryLimit *int32, steps []v1alpha1.CanaryStep, stepIndex *int32, maxSurge, maxUnavailable intstr.IntOrString, stableRS string) *v1alpha1.Rollout {
	ro := newCanaryRollout(name, replicas, revisionHistoryLimit, steps, stepIndex, maxSurge, maxUnavailable)
	ro.Status.CanaryStatus.StableRS = stableRS
	return ro
}

	func newCanaryRollout(name string, replicas int, revisionHistoryLimit *int32, steps []v1alpha1.CanaryStep, stepIndex *int32, maxSurge, maxUnavailable intstr.IntOrString) *v1alpha1.Rollout {
	selector := map[string]string{"foo": "bar"}
	rollout := newRollout(name, replicas, revisionHistoryLimit, selector)
	rollout.Spec.Strategy.Type = v1alpha1.CanaryRolloutStrategyType
	rollout.Spec.Strategy.CanaryStrategy = &v1alpha1.CanaryStrategy{
		MaxUnavailable: &maxUnavailable,
		MaxSurge:       &maxSurge,
		Steps:          steps,
	}
	rollout.Status.CurrentStepIndex = stepIndex
	return rollout
}

func bumpVersion(rollout *v1alpha1.Rollout) *v1alpha1.Rollout {
	newRollout := rollout.DeepCopy()
	revision := rollout.Annotations[annotations.RevisionAnnotation]
	newRevision, _ := strconv.Atoi(revision)
	newRevision++
	newRevisionStr := strconv.FormatInt(int64(newRevision), 10)
	annotations.SetRolloutRevision(newRollout, newRevisionStr)
	newRollout.Spec.Template.Spec.Containers[0].Image = "foo/bar" + newRevisionStr
	return newRollout
}

func calculatePatch(ro *v1alpha1.Rollout, patch string) string {
	origBytes, err := json.Marshal(ro)
	if err != nil {
		panic(err)
	}
	newBytes, err := strategicpatch.StrategicMergePatch(origBytes, []byte(patch), v1alpha1.Rollout{})
	if err != nil {
		panic(err)
	}
	newRO := &v1alpha1.Rollout{}
	json.Unmarshal(newBytes, newRO)
	newObservedGen := conditions.ComputeGenerationHash(newRO.Spec)

	newPatch := make(map[string]interface{})
	json.Unmarshal([]byte(patch), &newPatch)
	newStatus := newPatch["status"].(map[string]interface{})
	newStatus["observedGeneration"] = newObservedGen
	newPatch["status"] = newStatus
	newPatchBytes, _ := json.Marshal(newPatch)
	return string(newPatchBytes)
}

func TestReconcileCanaryStepsHandleBaseCases(t *testing.T) {
	fake := fake.Clientset{}
	k8sfake := k8sfake.Clientset{}
	controller := &Controller{
		rolloutsclientset: &fake,
		kubeclientset:     &k8sfake,
		recorder:          &record.FakeRecorder{},
	}

	// Handle case with no steps
	r := newCanaryRollout("test", 1, nil, nil, nil, intstr.FromInt(0), intstr.FromInt(1))
	stepResult, err := controller.reconcilePause(nil, nil, nil, r)
	assert.Nil(t, err)
	assert.False(t, stepResult)
	assert.Len(t, fake.Actions(), 0)

	r2 := newCanaryRollout("test", 1, nil, []v1alpha1.CanaryStep{{SetWeight: int32Ptr(10)}}, nil, intstr.FromInt(0), intstr.FromInt(1))
	r2.Status.CurrentStepIndex = int32Ptr(1)
	stepResult, err = controller.reconcilePause(nil, nil, nil, r2)
	assert.Nil(t, err)
	assert.False(t, stepResult)
	assert.Len(t, fake.Actions(), 0)

}

func TestReconcileCanaryStepsHandlePause(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	tests := []struct {
		name             string
		setPauseValue    *bool
		steps            []v1alpha1.CanaryStep
		currentStepIndex int32

		expectPatch           bool
		expectedSetPauseValue *bool
	}{
		{
			name:          "Put Canary into pause",
			setPauseValue: nil,
			steps: []v1alpha1.CanaryStep{
				{
					Pause: &v1alpha1.RolloutPause{},
				},
			},

			expectPatch:           true,
			expectedSetPauseValue: boolPtr(true),
		},
		{
			name:          "Do nothing if the canary is paused",
			setPauseValue: boolPtr(true),
			steps: []v1alpha1.CanaryStep{
				{
					Pause: &v1alpha1.RolloutPause{},
				},
			},

			expectPatch: false,
		},
		{
			name:          "Progress Canary after unpausing",
			setPauseValue: boolPtr(false),
			steps: []v1alpha1.CanaryStep{
				{
					Pause: &v1alpha1.RolloutPause{},
				},
			},

			expectPatch:           true,
			expectedSetPauseValue: nil,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			r := newCanaryRollout("test", 1, nil, test.steps, nil, intstr.FromInt(0), intstr.FromInt(1))
			r.Status.CurrentStepIndex = &test.currentStepIndex
			r.Status.SetPause = test.setPauseValue

			fake := fake.Clientset{}
			k8sfake := k8sfake.Clientset{}
			controller := &Controller{
				rolloutsclientset: &fake,
				kubeclientset:     &k8sfake,
				recorder:          &record.FakeRecorder{},
			}
			stepResult, err := controller.reconcilePause(nil, nil, nil, r)
			assert.Nil(t, err)
			assert.True(t, stepResult)
			if test.expectPatch {
				patchRollout := fake.Actions()[0].(core.PatchAction).GetPatch()
				if test.expectedSetPauseValue == nil {
					assert.Equal(t, fmt.Sprintf(setPausePatch, "null"), string(patchRollout))
				} else {
					assert.Equal(t, fmt.Sprintf(setPausePatch, "true"), string(patchRollout))
				}
			} else {
				assert.Len(t, fake.Actions(), 0)
			}

		})
	}
}

func TestResetCurrentStepIndexOnSpecChange(t *testing.T) {
	f := newFixture(t)
	steps := []v1alpha1.CanaryStep{
		{
			Pause: &v1alpha1.RolloutPause{},
		},
	}

	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(1), intstr.FromInt(0), intstr.FromInt(1))
	r1.Status.CurrentPodHash = "895c6c4f9"
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"
	r1.Status.AvailableReplicas = 10
	r2 := bumpVersion(r1)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 10, 10)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	patchBytes := filterInformerActions(f.client.Actions())[0].(core.PatchAction).GetPatch()
	expectedPatch := calculatePatch(r2,`{
	"status": {
		"currentPodHash":"5f79b78d7f",
		"currentStepIndex":0
	}
}`)
	assert.Equal(t, expectedPatch, string(patchBytes))

}

func TestCanaryRolloutCreateFirstReplicaset(t *testing.T) {
	f := newFixture(t)

	r := newCanaryRollout("foo", 10, nil, nil, nil, intstr.FromInt(1), intstr.FromInt(0))
	f.rolloutLister = append(f.rolloutLister, r)
	f.objects = append(f.objects, r)

	rs := newReplicaSet(r, "foo-895c6c4f9", 1)

	f.expectCreateReplicaSetAction(rs)
	f.expectPatchRolloutAction(r)
	f.run(getKey(r, t))

	patchBytes := filterInformerActions(f.client.Actions())[0].(core.PatchAction).GetPatch()
	expectedPatch := calculatePatch(r,`{
	"status":{
		"canaryStatus":{
			"stableRS":"895c6c4f9"
		},
		"currentPodHash":"895c6c4f9",
		"currentStepIndex":0
	}
}`)
	assert.Equal(t, expectedPatch, string(patchBytes))

}

func TestCanaryRolloutCreateNewReplicaWithCorrectWeight(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{{
		SetWeight: int32Ptr(10),
	}}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(0), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"
	r2 := bumpVersion(r1)

	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 10, 10)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 0)
	f.expectCreateReplicaSetAction(rs2)
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	newReplicaset := filterInformerActions(f.kubeclient.Actions())[0].(core.CreateAction).GetObject().(*appsv1.ReplicaSet)
	assert.Equal(t, int32(1), *newReplicaset.Spec.Replicas)
}

func TestCanaryRolloutScaleUpNewReplicaWithCorrectWeight(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{{
		SetWeight: int32Ptr(40),
	}}
	r1 := newCanaryRollout("foo", 5, nil, steps, int32Ptr(0), intstr.FromInt(0), intstr.FromInt(1))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"
	r2 := bumpVersion(r1)

	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 3, 3)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)
	f.expectUpdateReplicaSetAction(rs2)
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	newReplicaset := filterInformerActions(f.kubeclient.Actions())[0].(core.UpdateAction).GetObject().(*appsv1.ReplicaSet)
	assert.Equal(t, int32(2), *newReplicaset.Spec.Replicas)
}

func TestCanaryRolloutScaleDownStableToMatchWeight(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{{
		SetWeight: int32Ptr(10),
	}}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(0), intstr.FromInt(0), intstr.FromInt(1))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 10, 10)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 0, 0)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)
	f.expectUpdateReplicaSetAction(rs1)
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	expectedRS1 := rs1.DeepCopy()
	expectedRS1.Spec.Replicas = int32Ptr(9)
	assert.Len(t, filterInformerActions(f.kubeclient.Actions()), 1)
	assert.Equal(t, expectedRS1, filterInformerActions(f.kubeclient.Actions())[0].(core.UpdateAction).GetObject().(*appsv1.ReplicaSet))
}

func TestCanaryRolloutScaleDownOldRs(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{{
		SetWeight: int32Ptr(10),
	}}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(0), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)

	r3 := bumpVersion(r2)
	f.rolloutLister = append(f.rolloutLister, r3)
	f.objects = append(f.objects, r3)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	rs3 := newReplicaSetWithStatus(r3, "foo-8cdf7bbb4", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs3)
	f.replicaSetLister = append(f.replicaSetLister, rs3)

	f.expectUpdateReplicaSetAction(rs2)
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	expectedRS2 := rs2.DeepCopy()
	expectedRS2.Spec.Replicas = int32Ptr(0)
	expectedRS2.Annotations[annotations.DesiredReplicasAnnotation] = "10"

	assert.Equal(t, expectedRS2, filterInformerActions(f.kubeclient.Actions())[0].(core.UpdateAction).GetObject().(*appsv1.ReplicaSet))
}

func TestCanaryRolloutIncrementStepIfSetWeightsAreCorrect(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{{
		SetWeight: int32Ptr(10),
	}}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(0), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)

	r3 := bumpVersion(r2)
	r3.Status.CurrentPodHash = "8cdf7bbb4"
	f.rolloutLister = append(f.rolloutLister, r3)
	f.objects = append(f.objects, r3)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 0, 0)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	rs3 := newReplicaSetWithStatus(r3, "foo-8cdf7bbb4", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs3)
	f.replicaSetLister = append(f.replicaSetLister, rs3)

	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	patchBytes := filterInformerActions(f.client.Actions())[0].(core.PatchAction).GetPatch()
	expectedPatch := calculatePatch(r3, `{
	"status":{
		"availableReplicas":10,
		"canaryStatus":{
			"stableRS":"8cdf7bbb4"
		},
		"currentStepIndex":1
    }
}`)
	assert.Equal(t, expectedPatch, string(patchBytes))
}

func TestSyncRolloutsSetWaitStartTime(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{
		{
			SetWeight: int32Ptr(10),
		}, {
			Wait: int32Ptr(10),
		},
	}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(1), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)
	r2.Status.CurrentPodHash = "5f79b78d7f"
	r2.Status.AvailableReplicas = 10
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	patchBytes := filterInformerActions(f.client.Actions())[0].(core.PatchAction).GetPatch()
	expectedPatch := calculatePatch(r2, `{
	"status":{
		"canaryStatus":{
			"waitStartTime": "%s"
		}
	}
}`)
	expectedPatch = fmt.Sprintf(expectedPatch, metav1.Now().UTC().Format(time.RFC3339))
	assert.Equal(t, expectedPatch, string(patchBytes))
}

func TestSyncRolloutWaitAddToQueue(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{
		{
			SetWeight: int32Ptr(10),
		}, {
			Wait: int32Ptr(10),
		},
	}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(1), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)
	r2.Status.CurrentPodHash = "5f79b78d7f"
	r2.Status.AvailableReplicas = 10
	r2.Status.ObservedGeneration = conditions.ComputeGenerationHash(r2.Spec)

	now := metav1.Now()
	r2.Status.CanaryStatus.WaitStartTime = &now
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	key := fmt.Sprintf("%s/%s", r2.Namespace, r2.Name)
	c, i, k8sI := f.newController(func() time.Duration { return 30 * time.Minute })
	f.runController(key, true, false, c, i, k8sI)

	//When the controller starts, it will enqueue the rollout while syncing the informer and during the reconciliation step
	assert.Equal(t, 2, f.enqueuedObjects[key])

}

func TestSyncRolloutIgnoreWaitOutsideOfReconciliationPeriod(t *testing.T) {
	f := newFixture(t)

	steps := []v1alpha1.CanaryStep{
		{
			SetWeight: int32Ptr(10),
		}, {
			Wait: int32Ptr(int32(3600)), //1 hour
		},
	}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(1), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)
	r2.Status.CurrentPodHash = "5f79b78d7f"
	now := metav1.Now()
	r2.Status.CanaryStatus.WaitStartTime = &now
	r2.Status.ObservedGeneration = conditions.ComputeGenerationHash(r2.Spec)
	r2.Status.AvailableReplicas = 10
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	key := fmt.Sprintf("%s/%s", r2.Namespace, r2.Name)
	c, i, k8sI := f.newController(func() time.Duration { return 30 * time.Minute })
	f.runController(key, true, false, c, i, k8sI)
	//When the controller starts, it will enqueue the rollout so we expect the rollout to enqueue at least once.
	assert.Equal(t, 1, f.enqueuedObjects[key])

}

func TestSyncRolloutWaitIncrementStepIndex(t *testing.T) {

	f := newFixture(t)
	steps := []v1alpha1.CanaryStep{
		{
			SetWeight: int32Ptr(10),
		}, {
			Wait: int32Ptr(5),
		}, {
			Pause: &v1alpha1.RolloutPause{},
		},
	}
	r1 := newCanaryRollout("foo", 10, nil, steps, int32Ptr(1), intstr.FromInt(1), intstr.FromInt(0))
	r1.Status.CanaryStatus.StableRS = "895c6c4f9"

	r2 := bumpVersion(r1)
	r2.Status.CurrentPodHash = "5f79b78d7f"
	earlier := metav1.Now()
	earlier.Time = earlier.Add(-10 * time.Second)
	r2.Status.CanaryStatus.WaitStartTime = &earlier
	r2.Status.AvailableReplicas = 10
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 9, 9)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))

	patchBytes := filterInformerActions(f.client.Actions())[0].(core.PatchAction).GetPatch()
	expectedPatch := calculatePatch(r2, `{
	"status":{
		"canaryStatus":{
			"waitStartTime": null
		},
		"currentStepIndex":2
	}
}`)
	assert.Equal(t, expectedPatch, string(patchBytes))
}



func TestScaleCanary(t *testing.T) {
	newTimestamp := metav1.Date(2016, 5, 20, 3, 0, 0, 0, time.UTC)
	oldTimestamp := metav1.Date(2016, 5, 20, 2, 0, 0, 0, time.UTC)
	olderTimestamp := metav1.Date(2016, 5, 20, 1, 0, 0, 0, time.UTC)
	oldestTimestamp := metav1.Date(2016, 5, 20, 0, 0, 0, 0, time.UTC)

	setWeight50 := []v1alpha1.CanaryStep{{SetWeight:int32Ptr(50)}}

	zeroIntStr := intstr.FromInt(0)
	oneIntStr := intstr.FromInt(1)

	tests := []struct {
		name       string
		rollout    *v1alpha1.Rollout
		oldRollout *v1alpha1.Rollout

		newRS  *appsv1.ReplicaSet
		stableRS  *appsv1.ReplicaSet
		oldRSs []*appsv1.ReplicaSet

		expectedNew  *appsv1.ReplicaSet
		expectedStable  *appsv1.ReplicaSet
		expectedOld  []*appsv1.ReplicaSet
		wasntUpdated map[string]bool

		desiredReplicasAnnotations map[string]int32
	}{
		{
			name:       "normal scaling event: 10 -> 12",
			rollout:    newCanaryRollout("foo", 12, nil, nil, nil, oneIntStr, zeroIntStr),
			oldRollout:    newCanaryRollout("foo", 10, nil, nil, nil, oneIntStr, zeroIntStr),

			newRS:  rs("foo-v1", 10, nil, newTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{},

			expectedNew: rs("foo-v1", 12, nil, newTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{},
		},
		{
			name:       "normal scaling event: 10 -> 5",
			rollout:    newCanaryRollout("foo", 5, nil, nil, nil, oneIntStr, zeroIntStr),
			oldRollout:    newCanaryRollout("foo", 10, nil, nil, nil, oneIntStr, zeroIntStr),

			newRS:  rs("foo-v1", 10, nil, newTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{},

			expectedNew: rs("foo-v1", 5, nil, newTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{},
		},
		{
			name:       "Scale up non-active latest Replicaset",
			rollout:    newCanaryRollout("foo", 5, nil, nil, nil, oneIntStr, zeroIntStr),
			oldRollout:    newCanaryRollout("foo", 5, nil, nil, nil, oneIntStr, zeroIntStr),

			newRS:  rs("foo-v2", 0, nil, newTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{rs("foo-v1", 0, nil, oldTimestamp, nil)},

			expectedNew: rs("foo-v2", 5, nil, newTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{rs("foo-v1", 0, nil, oldTimestamp, nil)},
		},
		{
			name:       "No updates",
			rollout:    newCanaryRollout("foo", 10, nil, []v1alpha1.CanaryStep{{SetWeight: int32Ptr(50)}}, int32Ptr(0), oneIntStr, zeroIntStr),
			oldRollout: newCanaryRollout("foo", 10, nil, []v1alpha1.CanaryStep{{SetWeight: int32Ptr(50)}}, int32Ptr(0), oneIntStr, zeroIntStr),

			newRS: rs("foo-v3", 5, nil, newTimestamp, nil),
			stableRS: rs("foo-v2", 5, nil, oldTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{
				rs("foo-v1", 0, nil, olderTimestamp, nil),
				rs("foo-v0", 0, nil, oldestTimestamp, nil),
			},

			expectedNew: rs("foo-v3", 5, nil, newTimestamp, nil),
			expectedStable: rs("foo-v2", 5, nil, oldTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{
				rs("foo-v1", 0, nil, olderTimestamp, nil),
				rs("foo-v0", 0, nil, oldestTimestamp, nil),
			},
		},
		{
			name:       "New RS not created yet",
			rollout:    newCanaryRollout("foo", 10, nil, setWeight50, int32Ptr(0), oneIntStr, zeroIntStr),
			oldRollout: newCanaryRollout("foo", 12, nil, setWeight50, int32Ptr(0), oneIntStr, zeroIntStr),

			stableRS: rs("foo-v0", 12, nil, oldTimestamp, nil),

			expectedStable: rs("foo-v0", 10, nil, oldTimestamp, nil),
		},
		{
			name:       "Scale up active RS before new RS",
			rollout:    newCanaryRolloutWithStatus("foo", 10, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),
			oldRollout: newCanaryRolloutWithStatus("foo", 8, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),

			newRS: rs("foo-v2", 4, nil, newTimestamp, nil),
			stableRS: rs("foo-v1", 4, nil, oldTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{
				rs("foo-v0", 1, nil, oldestTimestamp, nil),
			},

			expectedNew: rs("foo-v2", 4, nil, newTimestamp, nil),
			expectedStable: rs("foo-v1", 5, nil, oldTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{
				rs("foo-v0", 1, nil, oldestTimestamp, nil),
			},
		},
		{
			name:       "Scale down oldRS, newRS and active RS",
			rollout:    newCanaryRolloutWithStatus("foo", 8, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),
			oldRollout: newCanaryRolloutWithStatus("foo", 10, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),

			newRS: rs("foo-v2", 5, nil, newTimestamp, nil),
			stableRS: rs("foo-v1", 5, nil, oldTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{
				rs("foo-v0", 2, nil, oldestTimestamp, nil),
			},

			expectedNew: rs("foo-v2", 4, nil, newTimestamp, nil),
			expectedStable: rs("foo-v1", 4, nil, oldTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{
				rs("foo-v0", 0, nil, oldestTimestamp, nil),
			},
		},
		{
			name:       "Scale down OldRS if stable is at desired count",
			rollout:    newCanaryRolloutWithStatus("foo", 10, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),
			oldRollout: newCanaryRolloutWithStatus("foo", 8, nil, setWeight50, int32Ptr(0), zeroIntStr, oneIntStr,"foo-v0"),

			newRS: rs("foo-v2", 4, nil, newTimestamp, nil),
			stableRS: rs("foo-v1", 5, nil, oldTimestamp, nil),
			oldRSs: []*appsv1.ReplicaSet{
				rs("foo-v0", 1, nil, oldestTimestamp, nil),
			},

			expectedNew: rs("foo-v2", 4, nil, newTimestamp, nil),
			expectedStable: rs("foo-v1", 5, nil, oldTimestamp, nil),
			expectedOld: []*appsv1.ReplicaSet{
				rs("foo-v0", 0, nil, oldestTimestamp, nil),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_ = olderTimestamp
			rolloutFake := fake.Clientset{}
			k8sFake := k8sfake.Clientset{}
			c := &Controller{
				rolloutsclientset: &rolloutFake,
				kubeclientset:     &k8sFake,
				recorder:          &record.FakeRecorder{},
			}

			if err := c.scaleCanary(test.oldRSs, test.newRS, test.stableRS, test.rollout); err != nil {
				t.Errorf("%s: unexpected error: %v", test.name, err)
				return
			}

			// Construct the nameToSize map that will hold all the sizes we got our of tests
			// Skip updating the map if the replica set wasn't updated since there will be
			// no update action for it.
			nameToSize := make(map[string]int32)
			if test.newRS != nil {
				nameToSize[test.newRS.Name] = *(test.newRS.Spec.Replicas)
			}
			if test.stableRS != nil {
				nameToSize[test.stableRS.Name] = *(test.stableRS.Spec.Replicas)
			}
			for i := range test.oldRSs {
				rs := test.oldRSs[i]
				nameToSize[rs.Name] = *(rs.Spec.Replicas)
			}
			// Get all the UPDATE actions and update nameToSize with all the updated sizes.
			for _, action := range k8sFake.Actions() {
				rs := action.(testclient.UpdateAction).GetObject().(*appsv1.ReplicaSet)
				if !test.wasntUpdated[rs.Name] {
					nameToSize[rs.Name] = *(rs.Spec.Replicas)
				}
			}

			if test.expectedNew != nil && test.newRS != nil && *(test.expectedNew.Spec.Replicas) != nameToSize[test.newRS.Name] {
				t.Errorf("%s: expected new replicas: %d, got: %d", test.name, *(test.expectedNew.Spec.Replicas), nameToSize[test.newRS.Name])
				return
			}
			if test.expectedStable != nil && test.stableRS != nil && *(test.expectedStable.Spec.Replicas) != nameToSize[test.stableRS.Name] {
				t.Errorf("%s: expected stable replicas: %d, got: %d", test.name, *(test.expectedStable.Spec.Replicas), nameToSize[test.expectedStable.Name])
				return
			}
			if len(test.expectedOld) != len(test.oldRSs) {
				t.Errorf("%s: expected %d old replica sets, got %d", test.name, len(test.expectedOld), len(test.oldRSs))
				return
			}
			for n := range test.oldRSs {
				rs := test.oldRSs[n]
				expected := test.expectedOld[n]
				if *(expected.Spec.Replicas) != nameToSize[rs.Name] {
					t.Errorf("%s: expected old (%s) replicas: %d, got: %d", test.name, rs.Name, *(expected.Spec.Replicas), nameToSize[rs.Name])
				}
			}
		})
	}
}


