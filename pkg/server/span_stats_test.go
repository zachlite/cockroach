// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package server_test

import (
	"bytes"
	"context"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestSpanStatsMetaScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	testCluster := serverutils.StartNewTestCluster(t, 3, base.TestClusterArgs{})
	defer testCluster.Stopper().Stop(context.Background())
	s := testCluster.Server(0)

	testSpans := []roachpb.Span{
		{
			Key:    keys.Meta1Prefix,
			EndKey: keys.Meta1KeyMax,
		},
		{
			Key:    keys.LocalMax,
			EndKey: keys.Meta2KeyMax,
		},
		{
			Key:    keys.Meta2Prefix,
			EndKey: keys.Meta2KeyMax,
		},
	}

	// SpanStats should have no problem finding descriptors for
	// spans up to and including Meta2KeyMax.
	for _, span := range testSpans {
		res, err := s.StatusServer().(serverpb.StatusServer).SpanStats(ctx,
			&roachpb.SpanStatsRequest{
				NodeID: "0",
				Spans: []roachpb.Span{
					span,
				},
			},
		)
		require.NoError(t, err)
		require.Equal(t, int32(1), res.SpanToStats[span.String()].RangeCount)
	}
}

func TestLocalSpanStats(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.UnderStress(t)
	ctx := context.Background()
	numNodes := 3
	tc := testcluster.StartTestCluster(t, numNodes, base.TestClusterArgs{})
	defer tc.Stopper().Stop(ctx)

	s := tc.Server(0).(*server.TestServer)

	store, err := s.Stores().GetStore(s.GetFirstStoreID())
	require.NoError(t, err)
	// Create a number of ranges using splits.
	splitKeys := []string{"a", "c", "e", "g", "i"}
	for _, k := range splitKeys {
		_, _, err := s.SplitRange(roachpb.Key(k))
		require.NoError(t, err)
	}

	// Wait for splits to finish.
	testutils.SucceedsSoon(t, func() error {
		repl := store.LookupReplica(roachpb.RKey("z"))
		if actualRSpan := repl.Desc().RSpan(); !actualRSpan.Key.Equal(roachpb.RKey("i")) {
			return errors.Errorf("expected range %s to begin at key 'i'", repl)
		}
		return nil
	})

	// Create some keys across the ranges.
	incKeys := []string{"b", "bb", "bbb", "d", "dd", "h"}
	for _, k := range incKeys {
		if _, err := store.DB().Inc(context.Background(), []byte(k), 5); err != nil {
			t.Fatal(err)
		}
	}

	// Create spans encompassing different ranges.
	spans := []roachpb.Span{
		{
			Key:    roachpb.Key("a"),
			EndKey: roachpb.Key("i"),
		},
		{
			Key:    roachpb.Key("a"),
			EndKey: roachpb.Key("c"),
		},
		{
			Key:    roachpb.Key("b"),
			EndKey: roachpb.Key("e"),
		},
		{
			Key:    roachpb.Key("e"),
			EndKey: roachpb.Key("i"),
		},
		{
			Key:    roachpb.Key("b"),
			EndKey: roachpb.Key("d"),
		},
		{
			Key:    roachpb.Key("b"),
			EndKey: roachpb.Key("bbb"),
		},
	}

	type testCase struct {
		span           roachpb.Span
		expectedRanges int32
		expectedKeys   int64
	}

	testCases := []testCase{
		{spans[0], 4, int64(numNodes * 6)},
		{spans[1], 1, int64(numNodes * 3)},
		{spans[2], 2, int64(numNodes * 5)},
		{spans[3], 2, int64(numNodes * 1)},
		{spans[4], 2, int64(numNodes * 3)},
		{spans[5], 1, int64(numNodes * 2)},
	}
	// Multi-span request
	multiResult, err := s.StatusServer().(serverpb.StatusServer).SpanStats(ctx,
		&roachpb.SpanStatsRequest{
			NodeID: "0",
			Spans:  spans,
		},
	)
	require.NoError(t, err)

	// Verify stats across different spans.
	for _, tcase := range testCases {
		rSpan, err := keys.SpanAddr(tcase.span)
		require.NoError(t, err)

		// Assert expected values from multi-span request
		spanStats := multiResult.SpanToStats[tcase.span.String()]
		require.Equal(
			t,
			tcase.expectedRanges,
			spanStats.RangeCount,
			fmt.Sprintf(
				"Multi-span: expected %d ranges in span [%s - %s], found %d",
				tcase.expectedRanges,
				rSpan.Key.String(),
				rSpan.EndKey.String(),
				spanStats.RangeCount,
			),
		)
		require.Equal(
			t,
			tcase.expectedKeys,
			spanStats.TotalStats.LiveCount,
			fmt.Sprintf(
				"Multi-span: expected %d keys in span [%s - %s], found %d",
				tcase.expectedKeys,
				rSpan.Key.String(),
				rSpan.EndKey.String(),
				spanStats.TotalStats.LiveCount,
			),
		)
	}
}

// What am I testing?
// The returned response has keys for each of the original span.
// This is separate from benchmarking
func TestFoo(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	numNodes := 3
	args := base.TestClusterArgs{}
	args.ServerArgs.Knobs.SpanConfig = &spanconfig.TestingKnobs{
		// Need multiple (uncoalesced) tenant ranges.
		StoreDisableCoalesceAdjacent: true,
	}

	tc := testcluster.StartTestCluster(t, numNodes, args)
	defer tc.Stopper().Stop(ctx)

	s := tc.Server(0).(*server.TestServer)
	_ = s
	sqlDB := sqlutils.MakeSQLRunner(tc.Conns[0])
	sqlDB.Exec(t, "SET CLUSTER SETTING sql.stats.automatic_collection.enabled=false")
	sqlDB.Exec(t, `SET CLUSTER SETTING spanconfig.storage_coalesce_adjacent.enabled = false`)
	sqlDB.Exec(t, "SELECT crdb_internal.generate_test_objects('test', 10)")

	// Wait until the span config reconciler runs.
	time.Sleep(time.Second * 10)
	res := sqlDB.QueryStr(t, "SHOW RANGES WITH TABLES, KEYS")

	for _, row := range res {
		log.Infof(ctx, "row: %v", row)
	}

	log.Infof(ctx, "Tenant id: %v", sqlDB.QueryStr(t, "SHOW TENANTS"))

	// create lots of ranges
	// get the ranges, and pass them to span stats
	// 1) does it work?
	// 2) start benchmarking with different configs.

	//
	//var spans []roachpb.Span
	//for i := 0; i < 100; i++ {
	//	startKey := make(roachpb.Key, 4)
	//	endKey := make(roachpb.Key, 4)
	//	binary.LittleEndian.PutUint32(startKey, uint32(i))
	//	binary.LittleEndian.PutUint32(endKey, uint32(i+1))
	//	sp := roachpb.Span{
	//		Key:    startKey,
	//		EndKey: endKey,
	//	}
	//	spans = append(spans, sp)
	//}
	//
	//stats, err := s.StatusServer().(serverpb.StatusServer).SpanStats(ctx,
	//	&roachpb.SpanStatsRequest{
	//		NodeID: "0",
	//		Spans:  spans,
	//	},
	//)
	//require.NoError(t, err)
	//_ = stats

}

// BenchmarkSpanStats measures the cost of collecting span statistics.
func BenchmarkSpanStats(b *testing.B) {
	skip.UnderShort(b)
	defer log.Scope(b).Close(b)

	createCluster := func(numNodes int) serverutils.TestClusterInterface {
		return serverutils.StartNewTestCluster(b, numNodes,
			base.TestClusterArgs{
				ReplicationMode: base.ReplicationAuto,
			})
	}

	clusterSpecs := []struct {
		name     string
		numNodes int
	}{
		{
			name:     "3node",
			numNodes: 3,
		},
	}

	type testSpec struct {
		numSpans  int
		numRanges int
	}

	var testSpecs []testSpec
	numSpanCases := []int{10, 100, 200}
	numRangeCases := []int{25, 50, 100}
	for _, numSpans := range numSpanCases {
		for _, numRanges := range numRangeCases {
			testSpecs = append(testSpecs, testSpec{numSpans: numSpans, numRanges: numRanges})
		}
	}

	for _, cluster := range clusterSpecs {
		b.Run(cluster.name, func(b *testing.B) {
			tc := createCluster(cluster.numNodes)
			ctx := context.Background()
			defer tc.Stopper().Stop(ctx)
			for _, ts := range testSpecs {
				b.Run(fmt.Sprintf("BenchmarkSpanStats - span stats for %d node cluster, collecting %d spans with %d ranges each",
					cluster.numNodes,
					ts.numSpans,
					ts.numRanges,
				), func(b *testing.B) {

					tenant := tc.Server(0).TenantOrServer()
					tenantCodec := keys.MakeSQLCodec(serverutils.TestTenantID())
					tenantPrefix := tenantCodec.TenantPrefix()

					makeKey := func(keys ...[]byte) roachpb.Key {
						return bytes.Join(keys, nil)
					}

					var spans []roachpb.Span

					// Create a table spans
					for i := 0; i < ts.numSpans; i++ {
						spanStartKey := makeKey(
							tenantPrefix,
							[]byte{byte(i)},
						)
						spanEndKey := makeKey(
							tenantPrefix,
							[]byte{byte(i + 1)},
						)
						// Create ranges.
						for j := 0; j < ts.numRanges; j++ {
							splitKey := makeKey(spanStartKey, []byte{byte(j)})
							_, _, err := tc.Server(0).SplitRange(splitKey)
							require.NoError(b, err)
						}
						spans = append(spans, roachpb.Span{
							Key:    spanStartKey,
							EndKey: spanEndKey,
						})
					}

					_, err := tc.ServerConn(0).Exec(`SET CLUSTER SETTING server.span_stats.span_batch_limit = $1`, ts.numSpans)
					require.NoError(b, err)
					_, err = tc.ServerConn(0).Exec(`SET CLUSTER SETTING server.span_stats.range_batch_limit = $1`, ts.numRanges)
					require.NoError(b, err)

					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						_, err := tenant.TenantStatusServer().(serverpb.TenantStatusServer).SpanStats(ctx,
							&roachpb.SpanStatsRequest{
								NodeID: "0", // 0 indicates we want stats from all nodes.
								Spans:  spans,
							})
						require.NoError(b, err)
					}
					b.StopTimer()
				})
			}
		})
	}
}
