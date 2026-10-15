package clusterstate

import (
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"

	"github.com/epheo/stornas/internal/model"
)

// replicationModel flattens an r-drbd Volume's replica status for the UI.
// Sorted by node so identical state encodes to identical frames.
func replicationModel(v *rdrbdv1alpha1.Volume) *model.Replication {
	out := &model.Replication{
		Replicas:   make([]model.Replica, 0, len(v.Status.Replicas)),
		SplitBrain: meta.IsStatusConditionTrue(v.Status.Conditions, rdrbdv1alpha1.ConditionSplitBrain),
	}
	for _, r := range v.Status.Replicas {
		rep := model.Replica{Node: r.Node, DiskState: r.DiskState, InUse: r.InUse}
		if rep.DiskState == "" {
			rep.DiskState = "Unknown"
		}
		if r.SyncPercent != nil {
			pct := int(*r.SyncPercent)
			rep.SyncPercent = &pct
		}
		for node, state := range r.Peers {
			rep.Peers = append(rep.Peers, model.Peer{Node: node, Connected: state == "Connected", Status: state})
		}
		sort.Slice(rep.Peers, func(i, j int) bool { return rep.Peers[i].Node < rep.Peers[j].Node })
		out.Replicas = append(out.Replicas, rep)
	}
	sort.Slice(out.Replicas, func(i, j int) bool { return out.Replicas[i].Node < out.Replicas[j].Node })
	return out
}
