package deploy

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	appsv1 "github.com/openshift/api/apps/v1"
	appsfake "github.com/openshift/client-go/apps/clientset/versioned/fake"
	appsapi "github.com/openshift/origin/pkg/apps/apis/apps"
	appsutil "github.com/openshift/origin/pkg/apps/util"
	appstest "github.com/openshift/origin/pkg/apps/util/test"

	// install all APIs
	_ "github.com/openshift/origin/pkg/api/install"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
)

func deploymentFor(config *appsv1.DeploymentConfig, status appsutil.DeploymentStatus) *corev1.ReplicationController {
	d, err := appsutil.MakeDeployment(config)
	if err != nil {
		panic(err)
	}
	d.Annotations[appsapi.DeploymentStatusAnnotation] = string(status)
	return d
}

// TestCmdDeploy_latestOk ensures that attempts to start a new deployment
// succeeds given an existing deployment in a terminal state.
func TestCmdDeploy_latestOk(t *testing.T) {
	validStatusList := []appsutil.DeploymentStatus{
		appsutil.DeploymentStatusComplete,
		appsutil.DeploymentStatusFailed,
	}
	for _, status := range validStatusList {
		config := appstest.OkDeploymentConfig(1)
		updatedConfig := config

		osClient := &appsfake.Clientset{}
		osClient.PrependReactor("get", "deploymentconfigs", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, config, nil
		})
		osClient.PrependReactor("create", "deploymentconfigs", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			if action.GetSubresource() != "instantiate" {
				return false, nil, nil
			}
			updatedConfig.Status.LatestVersion++
			return true, updatedConfig, nil
		})

		kubeClient := kubefake.NewSimpleClientset()
		kubeClient.PrependReactor("get", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, deploymentFor(config, status), nil
		})

		o := &DeployOptions{appsClient: osClient.Apps(), kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}
		err := o.deploy(config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if exp, got := int64(2), updatedConfig.Status.LatestVersion; exp != got {
			t.Fatalf("expected deployment config version: %d, got: %d", exp, got)
		}
	}
}

// TestCmdDeploy_latestConcurrentRejection ensures that attempts to start a
// deployment concurrent with a running deployment are rejected.
func TestCmdDeploy_latestConcurrentRejection(t *testing.T) {
	invalidStatusList := []appsutil.DeploymentStatus{
		appsutil.DeploymentStatusNew,
		appsutil.DeploymentStatusPending,
		appsutil.DeploymentStatusRunning,
	}

	for _, status := range invalidStatusList {
		config := appstest.OkDeploymentConfig(1)
		existingDeployment := deploymentFor(config, status)
		kubeClient := kubefake.NewSimpleClientset(existingDeployment)
		o := &DeployOptions{kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}

		err := o.deploy(config)
		if err == nil {
			t.Errorf("expected an error starting deployment with existing status %s", status)
		}
	}
}

// TestCmdDeploy_latestLookupError ensures that an error is thrown when
// existing deployments can't be looked up due to some fatal server error.
func TestCmdDeploy_latestLookupError(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kubeClient.PrependReactor("get", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, kerrors.NewInternalError(fmt.Errorf("internal error"))
	})

	config := appstest.OkDeploymentConfig(1)
	o := &DeployOptions{kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}
	err := o.deploy(config)

	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestCmdDeploy_retryOk ensures that a failed deployment can be retried.
func TestCmdDeploy_retryOk(t *testing.T) {
	deletedPods := []string{}
	config := appstest.OkDeploymentConfig(1)

	var updatedDeployment *corev1.ReplicationController
	existingDeployment := deploymentFor(config, appsutil.DeploymentStatusFailed)
	existingDeployment.Annotations[appsapi.DeploymentCancelledAnnotation] = appsapi.DeploymentCancelledAnnotationValue
	existingDeployment.Annotations[appsapi.DeploymentStatusReasonAnnotation] = appsapi.DeploymentCancelledByUser

	mkpod := func(name string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					appsutil.DeployerPodForDeploymentLabel: existingDeployment.Name,
				},
			},
		}
	}
	existingDeployerPods := []corev1.Pod{
		mkpod("hook-pre"), mkpod("hook-post"), mkpod("deployerpod"),
	}

	kubeClient := kubefake.NewSimpleClientset()
	kubeClient.PrependReactor("get", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, existingDeployment, nil
	})
	kubeClient.PrependReactor("update", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		updatedDeployment = action.(clientgotesting.UpdateAction).GetObject().(*corev1.ReplicationController)
		return true, updatedDeployment, nil
	})
	kubeClient.PrependReactor("list", "pods", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, &corev1.PodList{Items: existingDeployerPods}, nil
	})
	kubeClient.PrependReactor("delete", "pods", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		deletedPods = append(deletedPods, action.(clientgotesting.DeleteAction).GetName())
		return true, nil, nil
	})

	o := &DeployOptions{kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}
	err := o.retry(config)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if updatedDeployment == nil {
		t.Fatalf("expected updated config")
	}

	if appsutil.IsDeploymentCancelled(updatedDeployment) {
		t.Fatalf("deployment should not have the cancelled flag set anymore")
	}

	if appsutil.DeploymentStatusReasonFor(updatedDeployment) != "" {
		t.Fatalf("deployment status reason should be empty")
	}

	sort.Strings(deletedPods)
	expectedDeletions := []string{"deployerpod", "hook-post", "hook-pre"}
	if e, a := expectedDeletions, deletedPods; !reflect.DeepEqual(e, a) {
		t.Fatalf("Not all deployer pods for the failed deployment were deleted.\nEXPECTED: %v\nACTUAL: %v", e, a)
	}

	if e, a := appsutil.DeploymentStatusNew, appsutil.DeploymentStatusFor(updatedDeployment); e != a {
		t.Fatalf("expected deployment status %s, got %s", e, a)
	}
}

// TestCmdDeploy_retryRejectNonFailed ensures that attempts to retry a non-
// failed deployment are rejected.
func TestCmdDeploy_retryRejectNonFailed(t *testing.T) {
	invalidStatusList := []appsutil.DeploymentStatus{
		appsutil.DeploymentStatusNew,
		appsutil.DeploymentStatusPending,
		appsutil.DeploymentStatusRunning,
		appsutil.DeploymentStatusComplete,
	}

	for _, status := range invalidStatusList {
		config := appstest.OkDeploymentConfig(1)
		existingDeployment := deploymentFor(config, status)
		kubeClient := kubefake.NewSimpleClientset(existingDeployment)
		o := &DeployOptions{kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}
		err := o.retry(config)
		if err == nil {
			t.Errorf("expected an error retrying deployment with status %s", status)
		}
	}
}

// TestCmdDeploy_cancelOk ensures that attempts to cancel deployments
// for a config result in cancelling all in-progress deployments
// and none of the completed/faild ones.
func TestCmdDeploy_cancelOk(t *testing.T) {
	type existing struct {
		version      int64
		status       appsapi.DeploymentStatus
		shouldCancel bool
	}
	type scenario struct {
		version  int64
		existing []existing
	}

	scenarios := []scenario{
		// No existing deployments
		{1, []existing{{1, appsapi.DeploymentStatusComplete, false}}},
		// A single existing failed deployment
		{1, []existing{{1, appsapi.DeploymentStatusFailed, false}}},
		// Multiple existing completed/failed deployments
		{2, []existing{{2, appsapi.DeploymentStatusFailed, false}, {1, appsapi.DeploymentStatusComplete, false}}},
		// A single existing new deployment
		{1, []existing{{1, appsapi.DeploymentStatusNew, true}}},
		// A single existing pending deployment
		{1, []existing{{1, appsapi.DeploymentStatusPending, true}}},
		// A single existing running deployment
		{1, []existing{{1, appsapi.DeploymentStatusRunning, true}}},
		// Multiple existing deployments with one in new/pending/running
		{3, []existing{{3, appsapi.DeploymentStatusRunning, true}, {2, appsapi.DeploymentStatusComplete, false}, {1, appsapi.DeploymentStatusFailed, false}}},
		// Multiple existing deployments with more than one in new/pending/running
		{3, []existing{{3, appsapi.DeploymentStatusNew, true}, {2, appsapi.DeploymentStatusRunning, true}, {1, appsapi.DeploymentStatusFailed, false}}},
	}

	for _, scenario := range scenarios {
		updatedDeployments := []corev1.ReplicationController{}
		config := appstest.OkDeploymentConfig(scenario.version)
		existingDeployments := &corev1.ReplicationControllerList{}
		for _, e := range scenario.existing {
			d, _ := appsutil.MakeDeployment(appstest.OkDeploymentConfig(e.version))
			d.Annotations[appsapi.DeploymentStatusAnnotation] = string(e.status)
			existingDeployments.Items = append(existingDeployments.Items, *d)
		}

		kubeClient := kubefake.NewSimpleClientset()
		kubeClient.PrependReactor("update", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			updated := action.(clientgotesting.UpdateAction).GetObject().(*corev1.ReplicationController)
			updatedDeployments = append(updatedDeployments, *updated)
			return true, updated, nil
		})
		kubeClient.PrependReactor("list", "replicationcontrollers", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, existingDeployments, nil
		})

		o := &DeployOptions{kubeClient: kubeClient, IOStreams: genericclioptions.NewTestIOStreamsDiscard()}

		err := o.cancel(config)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		expectedCancellations := []int64{}
		actualCancellations := []int64{}
		for _, e := range scenario.existing {
			if e.shouldCancel {
				expectedCancellations = append(expectedCancellations, e.version)
			}
		}
		for _, d := range updatedDeployments {
			actualCancellations = append(actualCancellations, appsutil.DeploymentVersionFor(&d))
		}

		sort.Sort(Int64Slice(actualCancellations))
		sort.Sort(Int64Slice(expectedCancellations))
		if !reflect.DeepEqual(actualCancellations, expectedCancellations) {
			t.Fatalf("expected cancellations: %v, actual: %v", expectedCancellations, actualCancellations)
		}
	}
}

type Int64Slice []int64

func (p Int64Slice) Len() int           { return len(p) }
func (p Int64Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p Int64Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func TestDeploy_reenableTriggers(t *testing.T) {
	mktrigger := func() appsv1.DeploymentTriggerPolicy {
		t := appstest.OkImageChangeTrigger()
		t.ImageChangeParams.Automatic = false
		return t
	}

	var updated *appsv1.DeploymentConfig

	osClient := &appsfake.Clientset{}
	osClient.AddReactor("update", "deploymentconfigs", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		updated = action.(clientgotesting.UpdateAction).GetObject().(*appsv1.DeploymentConfig)
		return true, updated, nil
	})

	config := appstest.OkDeploymentConfig(1)
	config.Spec.Triggers = []appsv1.DeploymentTriggerPolicy{}
	count := 3
	for i := 0; i < count; i++ {
		config.Spec.Triggers = append(config.Spec.Triggers, mktrigger())
	}

	o := &DeployOptions{appsClient: osClient.Apps(), IOStreams: genericclioptions.NewTestIOStreamsDiscard()}
	err := o.reenableTriggers(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if updated == nil {
		t.Fatalf("expected an updated config")
	}

	if e, a := count, len(config.Spec.Triggers); e != a {
		t.Fatalf("expected %d triggers, got %d", e, a)
	}
	for _, trigger := range config.Spec.Triggers {
		if !trigger.ImageChangeParams.Automatic {
			t.Errorf("expected trigger to be enabled: %#v", trigger.ImageChangeParams)
		}
	}
}
