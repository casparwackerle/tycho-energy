package attribution

const (
	SystemNamespace = "__system__"
	SystemPod       = "__system__"
	SystemContainer = "__system__"
)

type WorkloadKey struct {
	Namespace string
	Pod       string
	Container string
}

func SystemWorkloadKey() WorkloadKey {
	return WorkloadKey{
		Namespace: SystemNamespace,
		Pod:       SystemPod,
		Container: SystemContainer,
	}
}

func (wk WorkloadKey) IsSystem() bool {
	return wk.Namespace == SystemNamespace &&
		wk.Pod == SystemPod &&
		wk.Container == SystemContainer
}

func (wk WorkloadKey) IsZero() bool {
	return wk.Namespace == "" && wk.Pod == "" && wk.Container == ""
}
