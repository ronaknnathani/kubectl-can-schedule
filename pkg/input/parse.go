package input

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

// ParseFiles reads the given manifest files and returns the workloads they
// contain. A filename of "-" reads from stdin. Files may contain multiple
// documents separated by "---". Supported kinds: Pod, Deployment, StatefulSet.
func ParseFiles(filenames []string, defaultNamespace string, stdin io.Reader) ([]*Workload, error) {
	var workloads []*Workload
	for _, filename := range filenames {
		var (
			data   []byte
			err    error
			source = filename
		)
		if filename == "-" {
			data, err = io.ReadAll(stdin)
			source = "stdin"
		} else {
			data, err = os.ReadFile(filename)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", source, err)
		}
		fileWorkloads, err := decodeDocuments(data, source, defaultNamespace)
		if err != nil {
			return nil, err
		}
		workloads = append(workloads, fileWorkloads...)
	}
	if len(workloads) == 0 {
		return nil, fmt.Errorf("no Pod, Deployment, or StatefulSet objects found in the provided manifests")
	}
	return workloads, nil
}

func decodeDocuments(data []byte, source, defaultNS string) ([]*Workload, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var workloads []*Workload
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: reading document: %w", source, err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj, gvk, err := scheme.Codecs.UniversalDeserializer().Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: decoding object: %w", source, err)
		}
		w, err := workloadFromObject(obj, defaultNS)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", source, gvk, err)
		}
		workloads = append(workloads, w)
	}
	return workloads, nil
}

func workloadFromObject(obj runtime.Object, defaultNS string) (*Workload, error) {
	var w *Workload
	switch o := obj.(type) {
	case *corev1.Pod:
		ns := namespaceOr(o.Namespace, defaultNS)
		base := o.DeepCopy()
		base.Namespace = ns
		w = &Workload{
			Kind:      "Pod",
			Name:      nameOr(o.Name, o.GenerateName, "pod"),
			Namespace: ns,
			Replicas:  1,
			base:      base,
		}
	case *appsv1.Deployment:
		ns := namespaceOr(o.Namespace, defaultNS)
		name := nameOr(o.Name, o.GenerateName, "deployment")
		replicas, err := replicasOf(o.Spec.Replicas)
		if err != nil {
			return nil, fmt.Errorf("Deployment %s: %w", name, err)
		}
		w = &Workload{
			Kind:      "Deployment",
			Name:      name,
			Namespace: ns,
			Replicas:  replicas,
			base:      podFromTemplate(o.Spec.Template, ns),
		}
	case *appsv1.StatefulSet:
		ns := namespaceOr(o.Namespace, defaultNS)
		name := nameOr(o.Name, o.GenerateName, "statefulset")
		replicas, err := replicasOf(o.Spec.Replicas)
		if err != nil {
			return nil, fmt.Errorf("StatefulSet %s: %w", name, err)
		}
		w = &Workload{
			Kind:                 "StatefulSet",
			Name:                 name,
			Namespace:            ns,
			Replicas:             replicas,
			base:                 podFromTemplate(o.Spec.Template, ns),
			volumeClaimTemplates: o.Spec.VolumeClaimTemplates,
		}
	default:
		return nil, fmt.Errorf("unsupported kind %T (only Pod, Deployment, StatefulSet are supported)", obj)
	}

	if err := validatePodResourceNames(w.base); err != nil {
		return nil, fmt.Errorf("%s %s: %w", w.Kind, w.Name, err)
	}
	return w, nil
}

func podFromTemplate(tmpl corev1.PodTemplateSpec, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Labels:      tmpl.Labels,
			Annotations: tmpl.Annotations,
		},
		Spec: *tmpl.Spec.DeepCopy(),
	}
}

func namespaceOr(ns, fallback string) string {
	if ns != "" {
		return ns
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

func nameOr(name, generateName, fallback string) string {
	if name != "" {
		return name
	}
	if generateName != "" {
		return generateName + "x"
	}
	return fallback
}

func replicasOf(r *int32) (int32, error) {
	if r == nil {
		return 1, nil
	}
	if *r < 0 {
		return 0, fmt.Errorf("spec.replicas must not be negative, got %d", *r)
	}
	return *r, nil
}
