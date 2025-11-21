package metadata

import (
	"context"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/pkg/kubelet"
)

// kubeletCollector is responsible for collecting pod and container metadata
// from the kubelet (or Kubernetes API as a fallback).
type kubeletCollector struct {
	cfg    Config
	store  *Store
	lister kubelet.KubeletPodLister
}

// newKubeletCollector constructs a kubeletCollector bound to the shared Store.
func newKubeletCollector(cfg Config, store *Store) *kubeletCollector {
	return &kubeletCollector{
		cfg:    cfg,
		store:  store,
		lister: kubelet.KubeletPodLister{},
	}
}

// regexReplaceContainerIDPrefix removes arbitrary runtime prefixes like
// "docker://", "containerd://", "cri-o://" from kubelet container IDs.
var regexReplaceContainerIDPrefix = regexp.MustCompile(`.*//`)

// normalizeContainerID standardizes container IDs as used in Tycho by
// stripping runtime-specific prefixes. Empty input results in empty output.
func normalizeContainerID(raw string) string {
	if raw == "" {
		return ""
	}
	return regexReplaceContainerIDPrefix.ReplaceAllString(raw, "")
}

// mapPodPhase converts a corev1.PodPhase into Tycho's PodPhase type.
func mapPodPhase(phase corev1.PodPhase) PodPhase {
	switch phase {
	case corev1.PodPending:
		return PodPhasePending
	case corev1.PodRunning:
		return PodPhaseRunning
	case corev1.PodSucceeded:
		return PodPhaseSucceeded
	case corev1.PodFailed:
		return PodPhaseFailed
	default:
		return PodPhaseUnknown
	}
}

// resolvePodOwner picks a primary owner from the pod's OwnerReferences, if any.
// It prefers the reference marked as Controller=true; otherwise it falls back
// to the first entry. If no owner is present, it returns empty strings.
func resolvePodOwner(pod *corev1.Pod) (kind, name string) {
	if pod == nil {
		return "", ""
	}
	if len(pod.OwnerReferences) == 0 {
		return "", ""
	}

	// Prefer the controller owner, if present.
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind, ref.Name
		}
	}

	// Fall back to the first owner reference.
	ref := &pod.OwnerReferences[0]
	return ref.Kind, ref.Name
}

// mapContainerState interprets kubelet's ContainerState and returns Tycho's
// ContainerState plus an optional exit code for terminated containers.
func mapContainerState(state corev1.ContainerState) (ContainerState, *int) {
	if state.Running != nil {
		return ContainerStateRunning, nil
	}
	if state.Terminated != nil {
		code := int(state.Terminated.ExitCode)
		return ContainerStateTerminated, &code
	}
	if state.Waiting != nil {
		return ContainerStateWaiting, nil
	}
	return ContainerStateUnknown, nil
}

// extractResourceRequestsLimits converts Kubernetes ResourceRequirements into
// simple numeric CPU and memory values.
//
// CPU:   millicores (q.MilliValue())
// Memory: bytes     (q.Value())
func extractResourceRequestsLimits(rr corev1.ResourceRequirements) (reqCPUMillis, reqMemBytes, limCPUMillis, limMemBytes int64) {
	if rr.Requests != nil {
		if q, ok := rr.Requests[corev1.ResourceCPU]; ok {
			reqCPUMillis = q.MilliValue()
		}
		if q, ok := rr.Requests[corev1.ResourceMemory]; ok {
			reqMemBytes = q.Value()
		}
	}
	if rr.Limits != nil {
		if q, ok := rr.Limits[corev1.ResourceCPU]; ok {
			limCPUMillis = q.MilliValue()
		}
		if q, ok := rr.Limits[corev1.ResourceMemory]; ok {
			limMemBytes = q.Value()
		}
	}
	return
}

// Collect refreshes pod and container metadata at the given time.
//
// Parameters:
//   - ctx:  request context, for cancellation.
//   - ts:   aligned wall-clock time from the engine.
//   - mono: monotonic index derived from ts (0 if MonoSource not configured).
//
// Implementation:
//   - lists pods via kubelet,
//   - updates PodMeta records in the store,
//   - updates ContainerMeta records for each container status,
//   - sets LastSeenMono/LastSeenWall appropriately.
func (kc *kubeletCollector) Collect(ctx context.Context, ts time.Time, mono uint64) {
	// Short-circuit if context is already cancelled.
	select {
	case <-ctx.Done():
		klog.V(4).Infof("[metadata/kubelet] Collect aborted due to context cancellation")
		return
	default:
	}

	pods, err := kc.lister.ListPods()
	if err != nil {
		klog.Errorf("[metadata/kubelet] ListPods failed: %v", err)
		return
	}

	if pods == nil || len(*pods) == 0 {
		klog.V(5).Infof("[metadata/kubelet] no pods reported at ts=%s mono=%d", ts.Format(time.RFC3339Nano), mono)
		return
	}

	for i := range *pods {
		pod := &(*pods)[i]
		kc.handlePod(pod, ts, mono)
	}
}

func (kc *kubeletCollector) handlePod(pod *corev1.Pod, ts time.Time, mono uint64) {
	phase := mapPodPhase(pod.Status.Phase)

	ownerKind, ownerName := resolvePodOwner(pod)

	// --- NEW: aggregate pod-level resources ---
	podReqCPU, podReqMem, podLimCPU, podLimMem := aggregatePodResources(pod)

	podMeta := &PodMeta{
		PodUID:    string(pod.UID),
		PodName:   pod.Name,
		Namespace: pod.Namespace,

		Phase:    phase,
		NodeName: pod.Spec.NodeName,
		QoSClass: string(pod.Status.QOSClass),

		OwnerKind: ownerKind,
		OwnerName: ownerName,

		RequestsCPUMillis: podReqCPU,
		RequestsMemBytes:  podReqMem,
		LimitsCPUMillis:   podLimCPU,
		LimitsMemBytes:    podLimMem,

		LastSeenMono: mono,
		LastSeenWall: ts,

		Labels: pod.Labels,
	}

	kc.store.UpsertPod(podMeta)

	appResources := make(map[string]corev1.ResourceRequirements, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		appResources[c.Name] = c.Resources
	}

	initResources := make(map[string]corev1.ResourceRequirements, len(pod.Spec.InitContainers))
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		initResources[c.Name] = c.Resources
	}

	ephemeralResources := make(map[string]corev1.ResourceRequirements, len(pod.Spec.EphemeralContainers))
	for i := range pod.Spec.EphemeralContainers {
		c := &pod.Spec.EphemeralContainers[i]
		ephemeralResources[c.Name] = c.Resources
	}

	// Normal containers
	kc.handleContainerStatuses(
		pod,
		phase,
		pod.Status.ContainerStatuses,
		appResources,
		ts,
		mono,
	)

	// Init containers
	kc.handleContainerStatuses(
		pod,
		phase,
		pod.Status.InitContainerStatuses,
		initResources,
		ts,
		mono,
	)

	// Ephemeral containers
	kc.handleContainerStatuses(
		pod,
		phase,
		pod.Status.EphemeralContainerStatuses,
		ephemeralResources,
		ts,
		mono,
	)
}

// handleContainerStatuses converts a list of corev1.ContainerStatus entries
// into ContainerMeta records and upserts them into the store.
func (kc *kubeletCollector) handleContainerStatuses(
	pod *corev1.Pod,
	phase PodPhase,
	statuses []corev1.ContainerStatus,
	resByName map[string]corev1.ResourceRequirements,
	ts time.Time,
	mono uint64,
) {
	if len(statuses) == 0 {
		return
	}

	for i := range statuses {
		st := &statuses[i]

		normalizedID := normalizeContainerID(st.ContainerID)
		if normalizedID == "" {
			// Container not started yet / no runtime ID assigned.
			continue
		}

		state, exitCode := mapContainerState(st.State)

		// Look up resource requests/limits by container name, if present.
		var reqCPU, reqMem, limCPU, limMem int64
		if resByName != nil {
			if rr, ok := resByName[st.Name]; ok {
				reqCPU, reqMem, limCPU, limMem = extractResourceRequestsLimits(rr)
			}
		}

		meta := &ContainerMeta{
			ContainerID:   normalizedID,
			ContainerName: st.Name,

			PodUID:    string(pod.UID),
			PodName:   pod.Name,
			Namespace: pod.Namespace,

			Phase:    phase,
			State:    state,
			ExitCode: exitCode,

			LastSeenMono: mono,
			LastSeenWall: ts,

			// Attach pod-level QoS; labels/annotations can be added later if needed.
			QoSClass: string(pod.Status.QOSClass),

			RequestsCPUMillis: reqCPU,
			RequestsMemBytes:  reqMem,
			LimitsCPUMillis:   limCPU,
			LimitsMemBytes:    limMem,
		}

		kc.store.UpsertContainer(meta)
	}
}

// aggregatePodResources computes pod-level CPU/memory requests and limits
// following Kubernetes scheduling semantics:
//
// - For normal containers: sum of requests/limits.
// - For init containers:   max of requests/limits across init containers.
// Pod-level value per resource is max(sum(normal), max(init)).
//
// Ephemeral containers are ignored for scheduling and are therefore not
// included in the pod-level aggregate.
func aggregatePodResources(pod *corev1.Pod) (reqCPUMillis, reqMemBytes, limCPUMillis, limMemBytes int64) {
	if pod == nil {
		return 0, 0, 0, 0
	}

	// Sum over regular containers.
	var sumReqCPU, sumReqMem, sumLimCPU, sumLimMem int64
	for i := range pod.Spec.Containers {
		rr := pod.Spec.Containers[i].Resources
		rCPU, rMem, lCPU, lMem := extractResourceRequestsLimits(rr)
		sumReqCPU += rCPU
		sumReqMem += rMem
		sumLimCPU += lCPU
		sumLimMem += lMem
	}

	// Max over init containers.
	var maxInitReqCPU, maxInitReqMem, maxInitLimCPU, maxInitLimMem int64
	for i := range pod.Spec.InitContainers {
		rr := pod.Spec.InitContainers[i].Resources
		rCPU, rMem, lCPU, lMem := extractResourceRequestsLimits(rr)
		if rCPU > maxInitReqCPU {
			maxInitReqCPU = rCPU
		}
		if rMem > maxInitReqMem {
			maxInitReqMem = rMem
		}
		if lCPU > maxInitLimCPU {
			maxInitLimCPU = lCPU
		}
		if lMem > maxInitLimMem {
			maxInitLimMem = lMem
		}
	}

	// Effective pod values: max(sum(normal), max(init)).
	reqCPUMillis = sumReqCPU
	if maxInitReqCPU > reqCPUMillis {
		reqCPUMillis = maxInitReqCPU
	}

	reqMemBytes = sumReqMem
	if maxInitReqMem > reqMemBytes {
		reqMemBytes = maxInitReqMem
	}

	limCPUMillis = sumLimCPU
	if maxInitLimCPU > limCPUMillis {
		limCPUMillis = maxInitLimCPU
	}

	limMemBytes = sumLimMem
	if maxInitLimMem > limMemBytes {
		limMemBytes = maxInitLimMem
	}

	return
}
