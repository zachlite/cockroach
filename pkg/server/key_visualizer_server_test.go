package server_test

import (
	"context"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/keyvisualizer/keyvispb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestKeyVisualizerServer(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	const numNodes = 3
	tc := testcluster.StartTestCluster(t, numNodes, base.TestClusterArgs{})
	defer tc.Stopper().Stop(ctx)

	keyVisServer := tc.Server(0).KeyVisServer()

	// Invalid! Key > EndKey.
	invalidBoundary := roachpb.Span{Key: roachpb.Key("b"), EndKey: roachpb.Key("a")}
	req := &keyvispb.UpdateBoundariesRequest{
		Boundaries: roachpb.Spans{invalidBoundary},
		Time:       timeutil.Now(),
	}

	_, err := keyVisServer.UpdateBoundaries(ctx, req)
	require.ErrorContains(t, err, "can not set boundary to invalid span")

	// Set cluster setting
	_, err = tc.ServerConn(0).Exec(`SET CLUSTER SETTING keyvisualizer.max_buckets = 1`)
	require.NoError(t, err)

	// Assert that the key visualizer server does not
	// accept a payload with invalid boundaries:

	// Too many!
	spans := roachpb.Spans{
		roachpb.Span{
			Key:    roachpb.Key("a"),
			EndKey: roachpb.Key("b"),
		},
		roachpb.Span{
			Key:    roachpb.Key("b"),
			EndKey: roachpb.Key("c"),
		},
	}

	req = &keyvispb.UpdateBoundariesRequest{
		Boundaries: spans,
		Time:       timeutil.Now(),
	}

	_, err = keyVisServer.UpdateBoundaries(ctx, req)
	require.ErrorContains(t, err, "expected less than or equal to 1 boundaries, received 2")

	// All good
	req = &keyvispb.UpdateBoundariesRequest{
		Boundaries: roachpb.Spans{keys.EverythingSpan},
		Time:       timeutil.Now(),
	}

	_, err = keyVisServer.UpdateBoundaries(ctx, req)
	require.NoError(t, err)

}
