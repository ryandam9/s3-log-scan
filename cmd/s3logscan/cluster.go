package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	emrtypes "github.com/aws/aws-sdk-go-v2/service/emr/types"
)

// emrLister is the slice of the EMR client used for cluster-name
// resolution; the fake in tests implements the same interface.
type emrLister interface {
	ListClusters(ctx context.Context, in *emr.ListClustersInput, optFns ...func(*emr.Options)) (*emr.ListClustersOutput, error)
}

// clusterMatch is one EMR cluster whose name matched -cluster-name.
type clusterMatch struct {
	ID      string
	State   string
	Created time.Time
}

func (m clusterMatch) String() string {
	created := "unknown creation time"
	if !m.Created.IsZero() {
		created = "created " + m.Created.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s (%s, %s)", m.ID, m.State, created)
}

// resolveClusterID finds the EMR cluster ID for a cluster name via
// ListClusters, considering only clusters that are currently RUNNING
// or WAITING (filtered server-side). Logs of terminated clusters can
// still be scanned by passing their ID with -cluster-id. When several
// active clusters share the name, the most recently created one is
// chosen and the others are returned so the caller can say so.
func resolveClusterID(ctx context.Context, client emrLister, name string) (chosen clusterMatch, others []clusterMatch, err error) {
	var matches []clusterMatch
	input := &emr.ListClustersInput{
		ClusterStates: []emrtypes.ClusterState{
			emrtypes.ClusterStateRunning,
			emrtypes.ClusterStateWaiting,
		},
	}
	for {
		out, err := client.ListClusters(ctx, input)
		if err != nil {
			return clusterMatch{}, nil, fmt.Errorf("listing EMR clusters: %w", err)
		}
		for _, c := range out.Clusters {
			if aws.ToString(c.Name) != name {
				continue
			}
			m := clusterMatch{ID: aws.ToString(c.Id)}
			if c.Status != nil {
				m.State = string(c.Status.State)
				if c.Status.Timeline != nil {
					m.Created = aws.ToTime(c.Status.Timeline.CreationDateTime)
				}
			}
			matches = append(matches, m)
		}
		if out.Marker == nil || aws.ToString(out.Marker) == "" {
			break
		}
		input.Marker = out.Marker
	}

	switch len(matches) {
	case 0:
		return clusterMatch{}, nil, fmt.Errorf("no running or waiting EMR cluster named %q found (terminated clusters can be targeted with -cluster-id)", name)
	case 1:
		return matches[0], nil, nil
	default:
		sort.Slice(matches, func(i, j int) bool { return matches[i].Created.After(matches[j].Created) })
		return matches[0], matches[1:], nil
	}
}

// joinClusterPrefix appends an EMR cluster ID to the key prefix so
// every LIST and GET is scoped to bucket/prefix/<cluster-id>/.
func joinClusterPrefix(prefix, clusterID string) string {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + clusterID + "/"
}
