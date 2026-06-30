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
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

// ParseFiles reads the given manifest files and returns the workloads they
// contain. A filename of "-" reads from stdin. Files may contain multiple
// documents separated by "---". Supported kinds: Pod, Deployment, StatefulSet.
func ParseFiles(filenames []string, defaultNamespace string, stdin io.Reader) ([]*Workload, error) {
	var workloads []*Workload
	for _, fn := range filenames {
		var (
			data []byte
			err  error
			src  = fn
		)
		if fn == "-" {
			data, err = io.ReadAll(stdin)
			src = "stdin"
		} else {
			data, err = os.ReadFile(fn)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", src, err)
		}
		ws, err := decodeDocuments(data, src, defaultNamespace)
		if err != nil {
			return nil, err
		}
		workloads = append(workloads, ws...)
	}
	if len(workloads) == 0 {
		return nil, fmt.Errorf("no Pod, Deployment, or StatefulSet objects found in the provided manifests")
	}
	return workloads, nil
}

func decodeDocuments(data []byte, source, defaultNS string) ([]*Workload, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out []*Workload
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
		w, err := workloadFromObject(obj, source, defaultNS)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", source, gvk, err)
		}
		out = append(out, w)
	}
	return out, nil
}

func workloadFromObject(obj interface{}, source, defaultNS string) (*Workload, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		ns := namespaceOr(o.Namespace, defaultNS)
		base := o.DeepCopy()
		base.Namespace = ns
		return &Workload{
			Kind:      "Pod",
			Name:      nameOr(o.Name, o.GenerateName, "pod"),
			Namespace: ns,
			Replicas:  1,
			Source:    source,
			base:      base,
		}, nil
	case *appsv1.Deployment:
		ns := namespaceOr(o.Namespace, defaultNS)
		return &Workload{
			Kind:      "Deployment",
			Name:      o.Name,
			Namespace: ns,
			Replicas:  replicasOr(o.Spec.Replicas),
			Source:    source,
			base:      podFromTemplate(o.Spec.Template, ns),
		}, nil
	case *appsv1.StatefulSet:
		ns := namespaceOr(o.Namespace, defaultNS)
		return &Workload{
			Kind:                 "StatefulSet",
			Name:                 o.Name,
			Namespace:            ns,
			Replicas:             replicasOr(o.Spec.Replicas),
			Source:               source,
			base:                 podFromTemplate(o.Spec.Template, ns),
			volumeClaimTemplates: o.Spec.VolumeClaimTemplates,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %T (only Pod, Deployment, StatefulSet are supported)", obj)
	}
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

func replicasOr(r *int32) int32 {
	if r == nil {
		return 1
	}
	if *r < 0 {
		return 0
	}
	return *r
}
