// Package pxc provides a minimal client-compatible stub of the Percona XtraDB Cluster CRD types.
// These match the fields used by mysql-keeper from the pxc.percona.com/v1 API.
// If you import the real Percona operator library, replace this file with the actual import.
package pxc

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion for Percona XtraDB Cluster operator.
var (
	GroupVersion  = schema.GroupVersion{Group: "pxc.percona.com", Version: "v1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&PerconaXtraDBCluster{},
		&PerconaXtraDBClusterList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// AppState mirrors pxc operator's AppState type.
type AppState string

const (
	AppStateUnknown  AppState = ""
	AppStateInit     AppState = "initializing"
	AppStateReady    AppState = "ready"
	AppStateError    AppState = "error"
	AppStateStopping AppState = "stopping"
	AppStatePaused   AppState = "paused"
)

// PerconaXtraDBCluster is the CRD type for a Percona XtraDB Cluster.
// Only fields required by mysql-keeper are included.
// +kubebuilder:object:root=true
type PerconaXtraDBCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PerconaXtraDBClusterSpec   `json:"spec,omitempty"`
	Status PerconaXtraDBClusterStatus `json:"status,omitempty"`
}

// PerconaXtraDBClusterSpec — minimal fields used by mysql-keeper.
type PerconaXtraDBClusterSpec struct {
	PXC         *PXCSpec        `json:"pxc,omitempty"`
	Replication *PXCReplication `json:"replication,omitempty"`
}

// PXCSpec holds PXC pod spec fields used by mysql-keeper.
type PXCSpec struct {
	Size int32 `json:"size,omitempty"`
}

// PXCReplication configures cross-site async replication managed by the PXC operator.
// The operator uses isSource in each channel to determine which cluster is the writer.
// isSource=true  → this cluster is the source (writable, read_only=OFF).
// isSource=false → this cluster is the replica (read-only, read_only=ON).
type PXCReplication struct {
	Enabled  bool                 `json:"enabled,omitempty"`
	Channels []ReplicationChannel `json:"channels,omitempty"`
}

// ReplicationChannel represents one async replication channel between DC and DR.
type ReplicationChannel struct {
	// Name identifies this channel (must match the channel name on both sides).
	Name string `json:"name"`
	// IsSource controls whether this cluster publishes (true) or subscribes (false).
	// The PXC operator enforces read_only based on this field.
	IsSource    bool         `json:"isSource,omitempty"`
	SourcesList []SourceHost `json:"sourcesList,omitempty"`
}

// SourceHost is a remote MySQL host that acts as the replication source.
type SourceHost struct {
	Host   string `json:"host"`
	Port   int32  `json:"port,omitempty"`
	Weight int32  `json:"weight,omitempty"`
}

// PerconaXtraDBClusterStatus mirrors the operator's status structure.
type PerconaXtraDBClusterStatus struct {
	// State is the overall cluster state (ready, initializing, error, etc.).
	State AppState `json:"state,omitempty"`

	// PXC holds per-component status including ready replica counts.
	PXC AppStatus `json:"pxc,omitempty"`

	// ProxySQL holds ProxySQL component status.
	ProxySQL AppStatus `json:"proxysql,omitempty"`
}

// AppStatus holds ready/size info for a PXC component.
type AppStatus struct {
	Size  int32 `json:"size,omitempty"`
	Ready int32 `json:"ready,omitempty"`
}

// +kubebuilder:object:root=true

// PerconaXtraDBClusterList contains a list of PerconaXtraDBCluster.
type PerconaXtraDBClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PerconaXtraDBCluster `json:"items"`
}

func (in *PerconaXtraDBCluster) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *PerconaXtraDBCluster) DeepCopy() *PerconaXtraDBCluster {
	if in == nil {
		return nil
	}
	out := new(PerconaXtraDBCluster)
	in.DeepCopyInto(out)
	return out
}

func (in *PerconaXtraDBCluster) DeepCopyInto(out *PerconaXtraDBCluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}

func (in *PerconaXtraDBClusterSpec) DeepCopyInto(out *PerconaXtraDBClusterSpec) {
	*out = *in
	if in.PXC != nil {
		pxc := *in.PXC
		out.PXC = &pxc
	}
	if in.Replication != nil {
		r := *in.Replication
		if in.Replication.Channels != nil {
			r.Channels = make([]ReplicationChannel, len(in.Replication.Channels))
			for i, ch := range in.Replication.Channels {
				r.Channels[i] = ch
				if ch.SourcesList != nil {
					r.Channels[i].SourcesList = make([]SourceHost, len(ch.SourcesList))
					copy(r.Channels[i].SourcesList, ch.SourcesList)
				}
			}
		}
		out.Replication = &r
	}
}

func (in *PerconaXtraDBClusterList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *PerconaXtraDBClusterList) DeepCopy() *PerconaXtraDBClusterList {
	if in == nil {
		return nil
	}
	out := new(PerconaXtraDBClusterList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PerconaXtraDBCluster, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}
