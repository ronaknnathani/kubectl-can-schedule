package scheduling

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/ronaknnathani/kubectl-can-schedule/pkg/snapshot"
)

func TestBuildDefaultFrameworkAndFilter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("8Gi"),
				v1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}

	client := fakeclient.NewSimpleClientset(node)
	factory := informers.NewSharedInformerFactory(client, 0)

	lister := snapshot.NewSwappable()
	lister.Set([]*v1.Node{node}, nil)

	f, err := BuildDefaultFramework(ctx, client, lister, factory)
	if err != nil {
		t.Fatalf("BuildDefaultFramework: %v", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Name: "c",
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}},
		},
	}

	state := framework.NewCycleState()
	_, preStatus, _ := f.RunPreFilterPlugins(ctx, state, pod)
	if !preStatus.IsSuccess() {
		t.Fatalf("prefilter failed: %v", preStatus.AsError())
	}

	nodeInfo, err := lister.NodeInfos().Get("node1")
	if err != nil {
		t.Fatalf("get nodeInfo: %v", err)
	}
	status := f.RunFilterPlugins(ctx, state, pod, nodeInfo)
	if !status.IsSuccess() {
		t.Fatalf("expected node1 to fit, got: %v", status.AsError())
	}

	// A pod requesting more CPU than allocatable must not fit.
	bigPod := pod.DeepCopy()
	bigPod.Name = "big"
	bigPod.Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("100")
	state2 := framework.NewCycleState()
	_, bigPreStatus, _ := f.RunPreFilterPlugins(ctx, state2, bigPod)
	if !bigPreStatus.IsSuccess() {
		t.Fatalf("big pod prefilter failed: %v", bigPreStatus.AsError())
	}
	if f.RunFilterPlugins(ctx, state2, bigPod, nodeInfo).IsSuccess() {
		t.Fatalf("expected big pod (requests 100 cpu) NOT to fit on node1")
	}
}
