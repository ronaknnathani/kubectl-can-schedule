// Package input parses scheduling inputs into a normalized list of Workloads.
//
// Inputs are mutually exclusive:
//   - one or more manifest files (-f), each possibly multi-document, containing
//     Pods, Deployments, and/or StatefulSets; or
//   - a synthetic workload described entirely by resource flags.
package input

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// Workload is a single schedulable input object expanded to a replica count.
type Workload struct {
	Kind      string // Pod | Deployment | StatefulSet | (flags)
	Name      string
	Namespace string
	Replicas  int32
	Source    string // file path or "flags"

	// base is the pod template for a single replica (one container set, volumes,
	// affinity, tolerations, resource requests, ...). Per-replica pods are derived
	// from it by Replica.
	base *corev1.Pod

	// volumeClaimTemplates holds StatefulSet .spec.volumeClaimTemplates. For each
	// replica, a per-ordinal PVC is synthesized and a corresponding volume is
	// injected into the pod, mirroring the StatefulSet controller.
	volumeClaimTemplates []corev1.PersistentVolumeClaim
}

// Replica builds the pod (and any synthetic PVCs it references) for the given
// zero-based replica ordinal. The returned pod has a unique name, the workload
// namespace, an empty NodeName, and the default scheduler implied.
func (w *Workload) Replica(ordinal int) (*corev1.Pod, []*corev1.PersistentVolumeClaim) {
	pod := w.base.DeepCopy()
	pod.Namespace = w.Namespace
	pod.Name = fmt.Sprintf("%s-%d", w.Name, ordinal)
	pod.UID = types.UID(uuid.NewUUID())
	pod.Spec.NodeName = ""
	// We always evaluate against the default scheduler profile.
	pod.Spec.SchedulerName = ""
	pod.ResourceVersion = ""

	var pvcs []*corev1.PersistentVolumeClaim
	for i := range w.volumeClaimTemplates {
		vct := w.volumeClaimTemplates[i]
		pvcName := fmt.Sprintf("%s-%s-%d", vct.Name, w.Name, ordinal)
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: w.Namespace,
			},
			Spec: *vct.Spec.DeepCopy(),
		}
		pvcs = append(pvcs, pvc)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: vct.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}
	return pod, pvcs
}
