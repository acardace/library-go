package installer

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/events/eventstesting"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestNewNodeStateForInstallInProgress(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-secret"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 1)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 1)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 2)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 2)}},
	)

	var installerPod *corev1.Pod

	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		if installerPod != nil {
			return true, nil, errors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, installerPod.Name)
		}
		installerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		kubeClient.PrependReactor("get", "pods", getPodsReactor(installerPod))
		return true, installerPod, nil
	})

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		&operatorv1.StaticPodOperatorStatus{
			LatestAvailableRevision: 1,
			NodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 0,
					TargetRevision:  0,
				},
			},
		},
		nil,
		nil,
	)

	eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})
	podCommand := []string{"/bin/true", "--foo=test", "--bar"}
	c := NewInstallerController(
		"test", "test-pod",
		[]revision.RevisionResource{{Name: "test-config"}},
		[]revision.RevisionResource{{Name: "test-secret"}},
		podCommand,
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		eventRecorder,
	)
	c.ownerRefsFn = func(revision int32) ([]metav1.OwnerReference, error) {
		return []metav1.OwnerReference{}, nil
	}
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }

	t.Log("setting target revision")
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if installerPod != nil {
		t.Fatalf("not expected to create installer pod yet")
	}

	_, currStatus, _, _ := fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if currStatus.NodeStatuses[0].TargetRevision != 1 {
		t.Fatalf("expected target revision generation 1, got: %d", currStatus.NodeStatuses[0].TargetRevision)
	}

	t.Log("starting installer pod")

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}
	if installerPod == nil {
		t.Fatalf("expected to create installer pod")
	}

	t.Run("VerifyPodCommand", func(t *testing.T) {
		cmd := installerPod.Spec.Containers[0].Command
		if !reflect.DeepEqual(podCommand, cmd) {
			t.Errorf("expected pod command %#v to match resulting installer pod command: %#v", podCommand, cmd)
		}
	})

	t.Run("VerifyPodArguments", func(t *testing.T) {
		args := installerPod.Spec.Containers[0].Args
		if len(args) == 0 {
			t.Errorf("pod args should not be empty")
		}
		foundRevision := false
		for _, arg := range args {
			if arg == "--revision=1" {
				foundRevision = true
			}
		}
		if !foundRevision {
			t.Errorf("revision installer argument not found")
		}
	})

	t.Log("synching again, nothing happens")
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if currStatus.NodeStatuses[0].TargetRevision != 1 {
		t.Fatalf("expected target revision generation 1, got: %d", currStatus.NodeStatuses[0].TargetRevision)
	}
	if currStatus.NodeStatuses[0].CurrentRevision != 0 {
		t.Fatalf("expected current revision generation 0, got: %d", currStatus.NodeStatuses[0].CurrentRevision)
	}

	t.Log("installer succeeded")
	installerPod.Status.Phase = corev1.PodSucceeded

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if generation := currStatus.NodeStatuses[0].CurrentRevision; generation != 0 {
		t.Errorf("expected current revision generation for node to be 0, got %d", generation)
	}

	t.Log("static pod launched, but is not ready")
	staticPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-test-node-1",
			Namespace: "test",
			Labels:    map[string]string{"revision": "1"},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Status: corev1.ConditionFalse,
					Type:   corev1.PodReady,
				},
			},
			Phase: corev1.PodRunning,
		},
	}
	kubeClient.PrependReactor("get", "pods", getPodsReactor(staticPod))

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if generation := currStatus.NodeStatuses[0].CurrentRevision; generation != 0 {
		t.Errorf("expected current revision generation for node to be 0, got %d", generation)
	}

	t.Log("static pod is ready")
	staticPod.Status.Conditions[0].Status = corev1.ConditionTrue

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if generation := currStatus.NodeStatuses[0].CurrentRevision; generation != 1 {
		t.Errorf("expected current revision generation for node to be 1, got %d", generation)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	currStatus.LatestAvailableRevision = 2
	currStatus.NodeStatuses[0].TargetRevision = 2
	currStatus.NodeStatuses[0].CurrentRevision = 1
	fakeStaticPodOperatorClient.UpdateStaticPodOperatorStatus("1", currStatus)

	installerPod.Name = "installer-2-test-node-1"
	installerPod.Status.Phase = corev1.PodFailed
	installerPod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "installer",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Message: "fake death"},
			},
		},
	}
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.GetStaticPodOperatorState()
	if generation := currStatus.NodeStatuses[0].LastFailedRevision; generation != 0 {
		t.Errorf("expected last failed revision generation for node to be 0, got %d", generation)
	}

	// installer pod failures are suppressed
	if errors := currStatus.NodeStatuses[0].LastFailedRevisionErrors; len(errors) != 0 {
		t.Error(errors)
	}

	if v1helpers.FindOperatorCondition(currStatus.Conditions, condition.NodeInstallerProgressingConditionType) == nil {
		t.Error("missing Progressing")
	}
	if v1helpers.FindOperatorCondition(currStatus.Conditions, condition.StaticPodsAvailableConditionType) == nil {
		t.Error("missing Available")
	}
}

func getPodsReactor(pods ...*corev1.Pod) ktesting.ReactionFunc {
	return func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		podName := action.(ktesting.GetAction).GetName()
		for _, p := range pods {
			if p.Namespace == action.GetNamespace() && p.Name == podName {
				return true, p, nil
			}
		}
		return false, nil, nil
	}
}

func TestCreateInstallerPod(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "test-secret"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-secret", 1)}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: fmt.Sprintf("%s-%d", "test-config", 1)}},
	)

	var installerPod *corev1.Pod
	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		installerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		return false, nil, nil
	})
	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))

	fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		&operatorv1.StaticPodOperatorStatus{
			LatestAvailableRevision: 1,
			NodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 0,
					TargetRevision:  0,
				},
			},
		},
		nil,
		nil,
	)
	eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

	c := NewInstallerController(
		"test", "test-pod",
		[]revision.RevisionResource{{Name: "test-config"}},
		[]revision.RevisionResource{{Name: "test-secret"}},
		[]string{"/bin/true"},
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		kubeClient.CoreV1(),
		eventRecorder,
	)
	c.ownerRefsFn = func(revision int32) ([]metav1.OwnerReference, error) {
		return []metav1.OwnerReference{}, nil
	}
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }
	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if installerPod != nil {
		t.Fatalf("expected first sync not to create installer pod")
	}

	if err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder)); err != nil {
		t.Fatal(err)
	}

	if installerPod == nil {
		t.Fatalf("expected to create installer pod")
	}

	if installerPod.Spec.Containers[0].Image != "docker.io/foo/bar" {
		t.Fatalf("expected docker.io/foo/bar image, got %q", installerPod.Spec.Containers[0].Image)
	}

	if installerPod.Spec.Containers[0].Command[0] != "/bin/true" {
		t.Fatalf("expected /bin/true as a command, got %q", installerPod.Spec.Containers[0].Command[0])
	}

	if installerPod.Name != "installer-1-test-node-1" {
		t.Fatalf("expected name installer-1-test-node-1, got %q", installerPod.Name)
	}

	if installerPod.Namespace != "test" {
		t.Fatalf("expected test namespace, got %q", installerPod.Namespace)
	}

	expectedArgs := []string{
		"-v=2",
		"--revision=1",
		"--namespace=test",
		"--pod=test-config",
		"--resource-dir=/etc/kubernetes/static-pod-resources",
		"--pod-manifest-dir=/etc/kubernetes/manifests",
		"--configmaps=test-config",
		"--secrets=test-secret",
	}

	if len(expectedArgs) != len(installerPod.Spec.Containers[0].Args) {
		t.Fatalf("expected arguments does not match container arguments: %#v != %#v", expectedArgs, installerPod.Spec.Containers[0].Args)
	}

	for i, v := range installerPod.Spec.Containers[0].Args {
		if expectedArgs[i] != v {
			t.Errorf("arg[%d] expected %q, got %q", i, expectedArgs[i], v)
		}
	}
}

func TestEnsureInstallerPod(t *testing.T) {
	tests := []struct {
		name         string
		expectedArgs []string
		configs      []revision.RevisionResource
		secrets      []revision.RevisionResource
		expectedErr  string
	}{
		{
			name: "normal",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--secrets=test-secret",
			},
			configs: []revision.RevisionResource{{Name: "test-config"}},
			secrets: []revision.RevisionResource{{Name: "test-secret"}},
		},
		{
			name: "optional",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--configmaps=test-config-2",
				"--optional-configmaps=test-config-opt",
				"--secrets=test-secret",
				"--secrets=test-secret-2",
				"--optional-secrets=test-secret-opt",
			},
			configs: []revision.RevisionResource{
				{Name: "test-config"},
				{Name: "test-config-2"},
				{Name: "test-config-opt", Optional: true}},
			secrets: []revision.RevisionResource{
				{Name: "test-secret"},
				{Name: "test-secret-2"},
				{Name: "test-secret-opt", Optional: true}},
		},
		{
			name: "first-cm-not-optional",
			expectedArgs: []string{
				"-v=2",
				"--revision=1",
				"--namespace=test",
				"--pod=test-config",
				"--resource-dir=/etc/kubernetes/static-pod-resources",
				"--pod-manifest-dir=/etc/kubernetes/manifests",
				"--configmaps=test-config",
				"--secrets=test-secret",
			},
			configs:     []revision.RevisionResource{{Name: "test-config", Optional: true}},
			secrets:     []revision.RevisionResource{{Name: "test-secret"}},
			expectedErr: "pod configmap test-config is required, cannot be optional",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset()

			var installerPod *corev1.Pod
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				installerPod = action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				return false, nil, nil
			})
			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))

			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{
					LatestAvailableRevision: 1,
					NodeStatuses: []operatorv1.NodeStatus{
						{
							NodeName:        "test-node-1",
							CurrentRevision: 0,
							TargetRevision:  0,
						},
					},
				},
				nil,
				nil,
			)
			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			c := NewInstallerController(
				"test", "test-pod",
				tt.configs,
				tt.secrets,
				[]string{"/bin/true"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				eventRecorder,
			)
			c.ownerRefsFn = func(revision int32) ([]metav1.OwnerReference, error) {
				return []metav1.OwnerReference{}, nil
			}
			err := c.ensureInstallerPod("test-node-1", &operatorv1.StaticPodOperatorSpec{}, 1)
			if err != nil {
				if tt.expectedErr == "" {
					t.Errorf("InstallerController.ensureInstallerPod() expected no error, got = %v", err)
					return
				}
				if tt.expectedErr != err.Error() {
					t.Errorf("InstallerController.ensureInstallerPod() got error = %v, wanted %s", err, tt.expectedErr)
					return
				}
				return
			}
			if tt.expectedErr != "" {
				t.Errorf("InstallerController.ensureInstallerPod() passed but expected error %s", tt.expectedErr)
			}

			if len(tt.expectedArgs) != len(installerPod.Spec.Containers[0].Args) {
				t.Fatalf("expected arguments does not match container arguments: %#v != %#v", tt.expectedArgs, installerPod.Spec.Containers[0].Args)
			}

			for i, v := range installerPod.Spec.Containers[0].Args {
				if tt.expectedArgs[i] != v {
					t.Errorf("arg[%d] expected %q, got %q", i, tt.expectedArgs[i], v)
				}
			}
		})
	}
}

func TestCreateInstallerPodMultiNode(t *testing.T) {
	tests := []struct {
		name                    string
		nodeStatuses            []operatorv1.NodeStatus
		staticPods              []*corev1.Pod
		latestAvailableRevision int32
		expectedUpgradeOrder    []int
		expectedSyncError       []bool
		updateStatusErrors      []error
		numOfInstallersOOM      int
		ownerRefsFn             func(revision int32) ([]metav1.OwnerReference, error)
	}{
		{
			name:                    "three fresh nodes",
			latestAvailableRevision: 1,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName: "test-node-0",
				},
				{
					NodeName: "test-node-1",
				},
				{
					NodeName: "test-node-2",
				},
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "three nodes with current revision, all static pods ready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "one node already transitioning",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
					TargetRevision:  2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "one node already transitioning, although it is newer",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
					TargetRevision:  3,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "three nodes, 2 not updated, one with failure in last revision",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    1,
					LastFailedRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{},
		},
		{
			name:                    "three nodes, 2 not updated, one with failure in old revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 2,
				},
				{
					NodeName:           "test-node-1",
					CurrentRevision:    2,
					LastFailedRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 2, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name:                    "three nodes with outdated current revision, second static pods unready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "four nodes with outdated current revision, installer of 2nd was OOM killed, two more OOM happen, then success",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			// we call sync 2*3 times:
			// 1. notice update of node 1
			// 2. create installer for node 1, OOM, fall-through, notice update of node 1
			// 3. create installer for node 1, OOM, fall-through, notice update of node 1
			// 4. create installer for node 1, which succeeds, set CurrentRevision
			// 5. notice update of node 2
			// 6. create installer for node 2, which succeeds, set CurrentRevision
			expectedUpgradeOrder: []int{1, 1, 1, 2},
			numOfInstallersOOM:   2,
		},
		{
			name:                    "three nodes with outdated current revision, 2nd & 3rd static pods unready",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, false),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "updated node unready and newer version available, but updated again before older nodes are touched",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name:                    "two nodes on revision 1 and one node on revision 4",
			latestAvailableRevision: 5,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 4,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 4, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "two nodes 2 revisions behind and 1 node on latest available revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 3,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 3, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodSucceeded, true),
			},
			expectedUpgradeOrder: []int{1, 2},
		},
		{
			name:                    "two nodes at different revisions behind and 1 node on latest available revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 3,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 3, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodSucceeded, true),
			},
			expectedUpgradeOrder: []int{2, 1},
		},
		{
			name:                    "second node with old static pod than current revision",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-1",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 2,
				},
				{
					NodeName:        "test-node-3",
					CurrentRevision: 2,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 2, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 2, corev1.PodRunning, false),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
		{
			name:                    "first update status fails",
			latestAvailableRevision: 2,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName: "test-node-1",
				},
			},
			expectedUpgradeOrder: []int{0},
			updateStatusErrors:   []error{errors.NewInternalError(fmt.Errorf("unknown"))},
			expectedSyncError:    []bool{true},
		},
		{
			name:                    "three nodes, 2 not updated, one already transitioning to a revision which is no longer available",
			latestAvailableRevision: 3,
			nodeStatuses: []operatorv1.NodeStatus{
				{
					NodeName:        "test-node-0",
					CurrentRevision: 1,
				},
				{
					NodeName:        "test-node-1",
					CurrentRevision: 1,
					TargetRevision:  2,
				},
				{
					NodeName:        "test-node-2",
					CurrentRevision: 1,
				},
			},
			staticPods: []*corev1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-0"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
			ownerRefsFn: func(revision int32) (references []metav1.OwnerReference, err error) {
				if revision == 3 {
					return []metav1.OwnerReference{}, nil
				}
				return nil, fmt.Errorf("TEST")
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			createdInstallerPods := []*corev1.Pod{}
			installerPods := map[string]*corev1.Pod{}
			updatedStaticPods := map[string]*corev1.Pod{}

			namespace := fmt.Sprintf("test-%d", i)

			installerNodeAndID := func(installerName string) (string, int) {
				ss := strings.SplitN(strings.TrimPrefix(installerName, "installer-"), "-", 2)
				id, err := strconv.Atoi(ss[0])
				if err != nil {
					t.Fatalf("unexpected id derived from install pod name %q: %v", installerName, err)
				}
				return ss[1], id
			}

			kubeClient := fake.NewSimpleClientset(
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-secret"}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-config"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: fmt.Sprintf("%s-%d", "test-secret", test.latestAvailableRevision)}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: fmt.Sprintf("%s-%d", "test-config", test.latestAvailableRevision)}},
			)
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				createdPod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				createdInstallerPods = append(createdInstallerPods, createdPod)
				if _, found := installerPods[createdPod.Name]; found {
					return false, nil, errors.NewAlreadyExists(corev1.SchemeGroupVersion.WithResource("pods").GroupResource(), createdPod.Name)
				}
				installerPods[createdPod.Name] = createdPod
				if test.numOfInstallersOOM > 0 {
					test.numOfInstallersOOM--

					createdPod.Status.Phase = corev1.PodFailed
					createdPod.Status.ContainerStatuses = []corev1.ContainerStatus{
						{
							Name: "container",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 1,
									Reason:   "OOMKilled",
									Message:  "killed by OOM",
								},
							},
							Ready: false,
						},
					}
				} else {
					// Once the installer pod is created, set its status to succeeded.
					// Note that in reality, this will probably take couple sync cycles to happen, however it is useful to do this fast
					// to rule out timing bugs.
					createdPod.Status.Phase = corev1.PodSucceeded

					nodeName, id := installerNodeAndID(createdPod.Name)
					staticPodName := mirrorPodNameForNode("test-pod", nodeName)

					updatedStaticPods[staticPodName] = newStaticPod(staticPodName, id, corev1.PodRunning, true)
				}

				return true, nil, nil
			})

			// When newNodeStateForInstallInProgress ask for pod, give it a pod that already succeeded.
			kubeClient.PrependReactor("get", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				podName := action.(ktesting.GetAction).GetName()
				if pod, found := installerPods[podName]; found {
					return true, pod, nil
				}
				if pod, exists := updatedStaticPods[podName]; exists {
					if pod == nil {
						return false, nil, nil
					}
					return true, pod, nil
				}
				for _, pod := range test.staticPods {
					if pod.Name == podName {
						return true, pod, nil
					}
				}
				return false, nil, nil
			})
			kubeClient.PrependReactor("delete", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				podName := action.(ktesting.GetAction).GetName()
				if pod, found := installerPods[podName]; found {
					delete(installerPods, podName)
					return true, pod, nil
				}
				return false, nil, nil
			})

			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test-"+test.name))
			statusUpdateCount := 0
			statusUpdateErrorFunc := func(rv string, status *operatorv1.StaticPodOperatorStatus) error {
				var err error
				if statusUpdateCount < len(test.updateStatusErrors) {
					err = test.updateStatusErrors[statusUpdateCount]
				}
				statusUpdateCount++
				return err
			}
			fakeStaticPodOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				&operatorv1.StaticPodOperatorStatus{
					LatestAvailableRevision: test.latestAvailableRevision,
					NodeStatuses:            test.nodeStatuses,
				},
				statusUpdateErrorFunc,
				nil,
			)

			eventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{})

			c := NewInstallerController(
				namespace, "test-pod",
				[]revision.RevisionResource{{Name: "test-config"}},
				[]revision.RevisionResource{{Name: "test-secret"}},
				[]string{"/bin/true"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				kubeClient.CoreV1(),
				eventRecorder,
			)
			c.ownerRefsFn = func(revision int32) ([]metav1.OwnerReference, error) {
				return []metav1.OwnerReference{}, nil
			}
			if test.ownerRefsFn != nil {
				c.ownerRefsFn = test.ownerRefsFn
			}
			c.installerPodImageFn = func() string { return "docker.io/foo/bar" }

			// Each node needs at least 2 syncs to first create the pod and then acknowledge its existence.
			for i := 1; i <= len(test.nodeStatuses)*2+1; i++ {
				err := c.Sync(context.TODO(), factory.NewSyncContext("InstallerController", eventRecorder))
				expectedErr := false
				if i-1 < len(test.expectedSyncError) && test.expectedSyncError[i-1] {
					expectedErr = true
				}
				if err != nil && !expectedErr {
					t.Errorf("failed to execute %d sync: %v", i, err)
				} else if err == nil && expectedErr {
					t.Errorf("expected sync error in sync %d, but got nil", i)
				}
			}

			for i := range test.expectedUpgradeOrder {
				if i >= len(createdInstallerPods) {
					t.Fatalf("expected more (got only %d) installer pods in the node order %v", len(createdInstallerPods), test.expectedUpgradeOrder[i:])
				}

				nodeName, _ := installerNodeAndID(createdInstallerPods[i].Name)
				if expected, got := test.nodeStatuses[test.expectedUpgradeOrder[i]].NodeName, nodeName; expected != got {
					t.Errorf("expected installer pod number %d to be for node %q, but got %q", i, expected, got)
				}
			}
			if len(test.expectedUpgradeOrder) < len(createdInstallerPods) {
				t.Errorf("too many installer pods created, expected %d, got %d", len(test.expectedUpgradeOrder), len(createdInstallerPods))
			}
		})
	}

}

func TestInstallerController_manageInstallationPods(t *testing.T) {
	type fields struct {
		targetNamespace      string
		staticPodName        string
		configMaps           []revision.RevisionResource
		secrets              []revision.RevisionResource
		command              []string
		operatorConfigClient v1helpers.StaticPodOperatorClient
		kubeClient           kubernetes.Interface
		eventRecorder        events.Recorder
		installerPodImageFn  func() string
	}
	type args struct {
		operatorSpec           *operatorv1.StaticPodOperatorSpec
		originalOperatorStatus *operatorv1.StaticPodOperatorStatus
		resourceVersion        string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    bool
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &InstallerController{
				targetNamespace:     tt.fields.targetNamespace,
				staticPodName:       tt.fields.staticPodName,
				configMaps:          tt.fields.configMaps,
				secrets:             tt.fields.secrets,
				command:             tt.fields.command,
				operatorClient:      tt.fields.operatorConfigClient,
				configMapsGetter:    tt.fields.kubeClient.CoreV1(),
				podsGetter:          tt.fields.kubeClient.CoreV1(),
				eventRecorder:       tt.fields.eventRecorder,
				installerPodImageFn: tt.fields.installerPodImageFn,
			}
			got, err := c.manageInstallationPods(context.TODO(), tt.args.operatorSpec, tt.args.originalOperatorStatus, tt.args.resourceVersion)
			if (err != nil) != tt.wantErr {
				t.Errorf("InstallerController.manageInstallationPods() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("InstallerController.manageInstallationPods() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodeToStartRevisionWith(t *testing.T) {
	type StaticPod struct {
		name     string
		state    staticPodState
		revision int32
	}
	type Test struct {
		name        string
		nodes       []operatorv1.NodeStatus
		pods        []StaticPod
		expected    int
		expectedErr bool
	}

	newNode := func(name string, current, target int32) operatorv1.NodeStatus {
		return operatorv1.NodeStatus{NodeName: name, CurrentRevision: current, TargetRevision: target}
	}

	for _, test := range []Test{
		{
			name:        "empty",
			expectedErr: true,
		},
		{
			name: "no pods",
			pods: nil,
			nodes: []operatorv1.NodeStatus{
				newNode("a", 0, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 0,
		},
		{
			name: "all ready",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 1, 0),
			},
			expected: 0,
		},
		{
			name: "one failed",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateFailed, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 1, 0),
			},
			expected: 2,
		},
		{
			name: "one pending",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStatePending, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 1, 0),
				newNode("c", 0, 0),
			},
			expected: 2,
		},
		{
			name: "multiple pending",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStatePending, 1},
				{"c", staticPodStatePending, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "one updating",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
				{"b", staticPodStatePending, 0},
				{"c", staticPodStateReady, 0},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 1),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "pods missing",
			pods: []StaticPod{
				{"a", staticPodStateReady, 1},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 1, 0),
				newNode("b", 0, 0),
				newNode("c", 0, 0),
			},
			expected: 1,
		},
		{
			name: "one old",
			pods: []StaticPod{
				{"a", staticPodStateReady, 2},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 2},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 2, 0),
				newNode("b", 2, 0),
				newNode("c", 2, 0),
			},
			expected: 1,
		},
		{
			name: "one behind, but as stated",
			pods: []StaticPod{
				{"a", staticPodStateReady, 2},
				{"b", staticPodStateReady, 1},
				{"c", staticPodStateReady, 2},
			},
			nodes: []operatorv1.NodeStatus{
				newNode("a", 2, 0),
				newNode("b", 1, 0),
				newNode("c", 2, 0),
			},
			expected: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeGetStaticPodState := func(ctx context.Context, nodeName string) (state staticPodState, revision, reason string, errs []string, err error) {
				for _, p := range test.pods {
					if p.name == nodeName {
						return p.state, strconv.Itoa(int(p.revision)), "", nil, nil
					}
				}
				return staticPodStatePending, "", "", nil, errors.NewNotFound(schema.GroupResource{Resource: "pods"}, nodeName)
			}
			i, _, err := nodeToStartRevisionWith(context.TODO(), fakeGetStaticPodState, test.nodes)
			if err == nil && test.expectedErr {
				t.Fatalf("expected error, got none")
			}
			if err != nil && !test.expectedErr {
				t.Fatalf("unexpected error: %v", err)
			}
			if i != test.expected {
				t.Errorf("expected node ID %d, got %d", test.expected, i)
			}
		})
	}
}

func TestSetConditions(t *testing.T) {

	type TestCase struct {
		name                      string
		latestAvailableRevision   int32
		lastFailedRevision        int32
		currentRevisions          []int32
		expectedAvailableStatus   operatorv1.ConditionStatus
		expectedProgressingStatus operatorv1.ConditionStatus
		expectedFailingStatus     operatorv1.ConditionStatus
	}

	testCase := func(name string, available, progressing, failed bool, lastFailedRevision, latest int32, current ...int32) TestCase {
		availableStatus := operatorv1.ConditionFalse
		pendingStatus := operatorv1.ConditionFalse
		expectedFailingStatus := operatorv1.ConditionFalse
		if available {
			availableStatus = operatorv1.ConditionTrue
		}
		if progressing {
			pendingStatus = operatorv1.ConditionTrue
		}
		if failed {
			expectedFailingStatus = operatorv1.ConditionTrue
		}
		return TestCase{name, latest, lastFailedRevision, current, availableStatus, pendingStatus, expectedFailingStatus}
	}

	testCases := []TestCase{
		testCase("AvailableProgressingDegraded", true, true, true, 1, 2, 2, 1, 2, 1),
		testCase("AvailableProgressing", true, true, false, 0, 2, 2, 1, 2, 1),
		testCase("AvailableNotProgressing", true, false, false, 0, 2, 2, 2, 2),
		testCase("NotAvailableProgressing", false, true, false, 0, 2, 0, 0),
		testCase("NotAvailableAtOldLevelProgressing", true, true, false, 0, 2, 1, 1),
		testCase("NotAvailableNotProgressing", false, false, false, 0, 2),
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			status := &operatorv1.StaticPodOperatorStatus{
				LatestAvailableRevision: tc.latestAvailableRevision,
			}
			for _, current := range tc.currentRevisions {
				status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{CurrentRevision: current, LastFailedRevision: tc.lastFailedRevision})
			}
			setAvailableProgressingNodeInstallerFailingConditions(status)

			availableCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.StaticPodsAvailableConditionType)
			if availableCondition == nil {
				t.Error("Available condition: not found")
			} else if availableCondition.Status != tc.expectedAvailableStatus {
				t.Errorf("Available condition: expected status %v, actual status %v", tc.expectedAvailableStatus, availableCondition.Status)
			}

			pendingCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.NodeInstallerProgressingConditionType)
			if pendingCondition == nil {
				t.Error("Progressing condition: not found")
			} else if pendingCondition.Status != tc.expectedProgressingStatus {
				t.Errorf("Progressing condition: expected status %v, actual status %v", tc.expectedProgressingStatus, pendingCondition.Status)
			}

			failingCondition := v1helpers.FindOperatorCondition(status.Conditions, condition.NodeInstallerDegradedConditionType)
			if failingCondition == nil {
				t.Error("Failing condition: not found")
			} else if failingCondition.Status != tc.expectedFailingStatus {
				t.Errorf("Failing condition: expected status %v, actual status %v", tc.expectedFailingStatus, failingCondition.Status)
			}
		})
	}

}

func TestEnsureRequiredResources(t *testing.T) {
	tests := []struct {
		name           string
		certConfigMaps []revision.RevisionResource
		certSecrets    []revision.RevisionResource

		revisionNumber int32
		configMaps     []revision.RevisionResource
		secrets        []revision.RevisionResource

		startingResources []runtime.Object
		expectedErr       string
	}{
		{
			name: "none",
		},
		{
			name: "skip-optional",
			certConfigMaps: []revision.RevisionResource{
				{Name: "foo-cm", Optional: true},
			},
			certSecrets: []revision.RevisionResource{
				{Name: "foo-s", Optional: true},
			},
		},
		{
			name: "wait-required",
			configMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			secrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			expectedErr: "missing required resources: [configmaps: foo-cm-0, secrets: foo-s-0]",
		},
		{
			name: "found-required",
			configMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			secrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			startingResources: []runtime.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-cm-0"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-s-0"}},
			},
		},
		{
			name: "wait-required-certs",
			certConfigMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			certSecrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			expectedErr: "missing required resources: [configmaps: foo-cm, secrets: foo-s]",
		},
		{
			name: "found-required-certs",
			certConfigMaps: []revision.RevisionResource{
				{Name: "foo-cm"},
			},
			certSecrets: []revision.RevisionResource{
				{Name: "foo-s"},
			},
			startingResources: []runtime.Object{
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-cm"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foo-s"}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(test.startingResources...)
			c := &InstallerController{
				targetNamespace: "ns",
				certConfigMaps:  test.certConfigMaps,
				certSecrets:     test.certSecrets,
				configMaps:      test.configMaps,
				secrets:         test.secrets,
				eventRecorder:   eventstesting.NewTestingEventRecorder(t),

				configMapsGetter: client.CoreV1(),
				secretsGetter:    client.CoreV1(),
			}

			actual := c.ensureRequiredResourcesExist(context.TODO(), test.revisionNumber)
			switch {
			case len(test.expectedErr) == 0 && actual == nil:
			case len(test.expectedErr) == 0 && actual != nil:
				t.Fatal(actual)
			case len(test.expectedErr) != 0 && actual == nil:
				t.Fatal(actual)
			case len(test.expectedErr) != 0 && actual != nil && !strings.Contains(actual.Error(), test.expectedErr):
				t.Fatalf("actual error: %q does not match expected: %q", actual.Error(), test.expectedErr)
			}

		})
	}
}

func newStaticPod(name string, revision int, phase corev1.PodPhase, ready bool) *corev1.Pod {
	condStatus := corev1.ConditionTrue
	if !ready {
		condStatus = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test",
			Labels:    map[string]string{"revision": strconv.Itoa(revision)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Status: condStatus,
					Type:   corev1.PodReady,
				},
			},
			Phase: phase,
		},
	}
}

func newStaticPodWithReadyTime(name string, revision int, phase corev1.PodPhase, ready bool, readyTime time.Time) *corev1.Pod {
	ret := newStaticPod(name, revision, phase, ready)
	ret.Status.Conditions[0].LastTransitionTime = metav1.NewTime(readyTime)
	return ret
}

func TestTimeToWait(t *testing.T) {
	nodeStatuses := []operatorv1.NodeStatus{
		{
			NodeName: "test-node-1",
		},
		{
			NodeName: "test-node-2",
		},
		{
			NodeName: "test-node-3",
		},
	}
	fakeNow := time.Now()
	fakeClock := clock.NewFakeClock(fakeNow)

	tenSecondsAgo := fakeNow.Add(-10 * time.Second)
	thirtySecondsAgo := fakeNow.Add(-30 * time.Second)
	thirtyMinutesAgo := fakeNow.Add(-30 * time.Minute)

	tests := []struct {
		name            string
		minReadySeconds time.Duration
		staticPods      []runtime.Object
		expected        time.Duration
	}{
		{
			name:            "all long ready",
			minReadySeconds: 35 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
			},
			expected: 0,
		},
		{
			name:            "no pods",
			minReadySeconds: 35 * time.Second,
			expected:        0 * time.Second,
		},
		{
			name:            "exact match",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, thirtyMinutesAgo),
			},
			expected: 0,
		},
		{
			name:            "one short",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, tenSecondsAgo),
			},
			expected: 20 * time.Second,
		},
		{
			name:            "one not ready",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, true, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, true, tenSecondsAgo),
			},
			expected: 20 * time.Second,
		},
		{
			name:            "none ready",
			minReadySeconds: 30 * time.Second,
			staticPods: []runtime.Object{
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-1"), 1, corev1.PodRunning, false, thirtySecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-2"), 1, corev1.PodRunning, false, tenSecondsAgo),
				newStaticPodWithReadyTime(mirrorPodNameForNode("test-pod", "test-node-3"), 1, corev1.PodRunning, false, tenSecondsAgo),
			},
			expected: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kubeClient := fake.NewSimpleClientset(test.staticPods...)

			c := &InstallerController{
				targetNamespace:  "test",
				staticPodName:    "test-pod",
				minReadyDuration: test.minReadySeconds,
				podsGetter:       kubeClient.CoreV1(),
				clock:            fakeClock,
			}

			actual := c.timeToWaitBeforeInstallingNextPod(context.TODO(), nodeStatuses)
			if actual != test.expected {
				t.Fatal(actual)
			}
		})
	}

}
