// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package spanstatsconsumer

import (
	"context"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"math"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keyvisualizer/keyvisstorage"
	"github.com/cockroachdb/cockroach/pkg/keyvisualizer/spanstatskvaccessor"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

const maxBoundaries = 512

// SpanStatsConsumer interacts with the key visualizer subsystem in KV and persists
// collected statistics to the tenant's system tables.
type SpanStatsConsumer struct {
	kvAccessor *spanstatskvaccessor.SpanStatsKVAccessor
	ri         *kvcoord.RangeIterator
	settings   *cluster.Settings
	ie         *sql.InternalExecutor
}

// New constructs a new SpanStatsConsumer.
func New(
	accessor *spanstatskvaccessor.SpanStatsKVAccessor,
	iterator *kvcoord.RangeIterator,
	settings *cluster.Settings,
	executor *sql.InternalExecutor,
) *SpanStatsConsumer {
	return &SpanStatsConsumer{
		kvAccessor: accessor,
		ri:         iterator,
		settings:   settings,
		ie:         executor,
	}
}

// boundaries must be contiguous, and lexicographically sorted.
func maybeCombineBoundaries(boundaries []roachpb.Span, max int) []roachpb.Span {
	if len(boundaries) <= max {
		return boundaries
	}

	combineFactor := int(math.Ceil(float64(len(boundaries)) / float64(max)))
	combinedLength := int(math.Ceil(float64(len(boundaries)) / float64(combineFactor)))
	combined := make([]roachpb.Span, combinedLength)

	log.Infof(context.Background(), "boundary length: %d", len(boundaries))
	log.Infof(context.Background(), "combine factor: %d", combineFactor)
	log.Infof(context.Background(), "combined length: %d", combinedLength)

	// iterate through boundaries, incrementing by combineFactor
	for i := 0; i < combinedLength; i++ {
		startSpan := boundaries[i*combineFactor]

		endIndex := i*combineFactor + combineFactor - 1
		if endIndex >= len(boundaries) {
			combined[i] = startSpan
		} else {
			combined[i] = startSpan.Combine(boundaries[endIndex])
		}
	}

	return combined
}

// UpdateBoundaries is part of the keyvisualizer.SpanStatsConsumer interface.
func (s *SpanStatsConsumer) UpdateBoundaries(ctx context.Context) error {
	boundaries, err := s.decideBoundaries(ctx)
	if err != nil {
		return err
	}
	updateTime := timeutil.Now().Add(10 * time.Second) // Arbitrary, but long enough for the payload to propagate to all nodes.
	_, err = s.kvAccessor.UpdateBoundaries(ctx, maybeCombineBoundaries(boundaries, maxBoundaries), updateTime)
	return err
}

// GetSamples is part of the keyvisualizer.SpanStatsConsumer interface.
func (s *SpanStatsConsumer) GetSamples(ctx context.Context) error {
	mostRecentSampleTime, err := keyvisstorage.MostRecentSampleTime(ctx, s.ie)
	if err != nil {
		panic(errors.NewAssertionErrorWithWrappedErrf(
			err, "read most recent sample time failed"))
	}

	samplesRes, err := s.kvAccessor.GetSamples(ctx, mostRecentSampleTime)
	if err != nil {
		return err
	}

	//maxBuckets := keyvissettings.MaxBuckets.Get(&s.settings.SV)
	//for i, sample := range samplesRes.Samples {
	//	samplesRes.Samples[i].SpanStats = downsample(sample.SpanStats, int(maxBuckets))
	//}

	if err := keyvisstorage.WriteSamples(ctx, s.ie, samplesRes.Samples); err != nil {
		panic(errors.NewAssertionErrorWithWrappedErrf(
			err, "write samples failed"))
	}

	return nil
}

// decideBoundaries decides the key spans that we want statistics
// for. For now, it will tell KV to collect statistics for all
// ranges from [Min, Max).
func (s *SpanStatsConsumer) decideBoundaries(ctx context.Context) ([]roachpb.Span, error) {
	var boundaries []roachpb.Span

	tenantSpan := roachpb.RSpan{
		Key:    roachpb.RKeyMin,
		EndKey: roachpb.RKeyMax,
	}

	s.ri.Seek(ctx, tenantSpan.Key, kvcoord.Ascending)

	for {
		if !s.ri.Valid() {
			return nil, s.ri.Error()
		}

		boundaries = append(boundaries, roachpb.Span{
			Key:    roachpb.Key(s.ri.Desc().StartKey),
			EndKey: roachpb.Key(s.ri.Desc().EndKey),
		})

		if !s.ri.NeedAnother(tenantSpan) {
			break
		}

		s.ri.Next(ctx)
	}

	return boundaries, nil
}

// DeleteExpiredSamples deletes historical samples older than 2 weeks.
func (s *SpanStatsConsumer) DeleteExpiredSamples(ctx context.Context) error {
	twoWeeksAgo := timeutil.Now().AddDate(0, 0, -14)
	if err := keyvisstorage.DeleteSamplesBeforeTime(ctx, s.ie, twoWeeksAgo); err != nil {
		panic(errors.NewAssertionErrorWithWrappedErrf(
			err, "delete expired samples failed"))
	}
	return nil
}
