// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package ttljob

import (
	"context"
	"math"
	"regexp"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/metric/aggmetric"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

var (
	defaultSelectBatchSize = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_select_batch_size",
		"default amount of rows to select in a single query during a TTL job",
		500,
		settings.PositiveInt,
	).WithPublic()
	defaultDeleteBatchSize = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_delete_batch_size",
		"default amount of rows to delete in a single query during a TTL job",
		100,
		settings.PositiveInt,
	).WithPublic()
	defaultRangeConcurrency = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_range_concurrency",
		"default amount of ranges to process at once during a TTL delete",
		1,
		settings.PositiveInt,
	).WithPublic()
	defaultDeleteRateLimit = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.default_delete_rate_limit",
		"default delete rate limit for all TTL jobs. Use 0 to signify no rate limit.",
		0,
		settings.NonNegativeInt,
	).WithPublic()

	rangeBatchSize = settings.RegisterIntSetting(
		settings.TenantWritable,
		"sql.ttl.range_batch_size",
		"amount of ranges to fetch at a time for a table during the TTL job",
		100,
		settings.PositiveInt,
	).WithPublic()
)

type rowLevelTTLResumer struct {
	job *jobs.Job
	st  *cluster.Settings
}

// RowLevelTTLAggMetrics are the row-level TTL job agg metrics.
type RowLevelTTLAggMetrics struct {
	RangeTotalDuration *aggmetric.AggHistogram
	SelectDuration     *aggmetric.AggHistogram
	DeleteDuration     *aggmetric.AggHistogram
	RowSelections      *aggmetric.AggCounter
	RowDeletions       *aggmetric.AggCounter
	NumActiveRanges    *aggmetric.AggGauge

	mu struct {
		syncutil.Mutex
		m map[string]rowLevelTTLMetrics
	}
}

type rowLevelTTLMetrics struct {
	RangeTotalDuration *aggmetric.Histogram
	SelectDuration     *aggmetric.Histogram
	DeleteDuration     *aggmetric.Histogram
	RowSelections      *aggmetric.Counter
	RowDeletions       *aggmetric.Counter
	NumActiveRanges    *aggmetric.Gauge
}

// MetricStruct implements the metric.Struct interface.
func (m *RowLevelTTLAggMetrics) MetricStruct() {}

var invalidPrometheusRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func (m *RowLevelTTLAggMetrics) loadMetrics(relation string) rowLevelTTLMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()

	relation = invalidPrometheusRe.ReplaceAllString(relation, "_")
	if ret, ok := m.mu.m[relation]; ok {
		return ret
	}
	ret := rowLevelTTLMetrics{
		RangeTotalDuration: m.RangeTotalDuration.AddChild(relation),
		SelectDuration:     m.SelectDuration.AddChild(relation),
		DeleteDuration:     m.DeleteDuration.AddChild(relation),
		RowSelections:      m.RowSelections.AddChild(relation),
		RowDeletions:       m.RowDeletions.AddChild(relation),
		NumActiveRanges:    m.NumActiveRanges.AddChild(relation),
	}
	m.mu.m[relation] = ret
	return ret
}

func makeRowLevelTTLAggMetrics(histogramWindowInterval time.Duration) metric.Struct {
	sigFigs := 2
	b := aggmetric.MakeBuilder("relation")
	ret := &RowLevelTTLAggMetrics{
		RangeTotalDuration: b.Histogram(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.range_total_duration",
				Help:        "Duration for processing a range during row level TTL.",
				Measurement: "nanoseconds",
				Unit:        metric.Unit_NANOSECONDS,
				MetricType:  io_prometheus_client.MetricType_HISTOGRAM,
			},
			histogramWindowInterval,
			time.Hour.Nanoseconds(),
			sigFigs,
		),
		SelectDuration: b.Histogram(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.select_duration",
				Help:        "Duration for select requests during row level TTL.",
				Measurement: "nanoseconds",
				Unit:        metric.Unit_NANOSECONDS,
				MetricType:  io_prometheus_client.MetricType_HISTOGRAM,
			},
			histogramWindowInterval,
			time.Minute.Nanoseconds(),
			sigFigs,
		),
		DeleteDuration: b.Histogram(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.delete_duration",
				Help:        "Duration for delete requests during row level TTL.",
				Measurement: "nanoseconds",
				Unit:        metric.Unit_NANOSECONDS,
				MetricType:  io_prometheus_client.MetricType_HISTOGRAM,
			},
			histogramWindowInterval,
			time.Minute.Nanoseconds(),
			sigFigs,
		),
		RowSelections: b.Counter(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.rows_selected",
				Help:        "Number of rows selected for deletion by the row level TTL job.",
				Measurement: "num_rows",
				Unit:        metric.Unit_COUNT,
				MetricType:  io_prometheus_client.MetricType_COUNTER,
			},
		),
		RowDeletions: b.Counter(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.rows_deleted",
				Help:        "Number of rows deleted by the row level TTL job.",
				Measurement: "num_rows",
				Unit:        metric.Unit_COUNT,
				MetricType:  io_prometheus_client.MetricType_COUNTER,
			},
		),
		NumActiveRanges: b.Gauge(
			metric.Metadata{
				Name:        "jobs.row_level_ttl.num_active_ranges",
				Help:        "Number of active workers attempting to delete for row level TTL.",
				Measurement: "num_active_workers",
				Unit:        metric.Unit_COUNT,
			},
		),
	}
	ret.mu.m = make(map[string]rowLevelTTLMetrics)
	return ret
}

var _ jobs.Resumer = (*rowLevelTTLResumer)(nil)

// Resume implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) Resume(ctx context.Context, execCtx interface{}) error {
	p := execCtx.(sql.JobExecContext)
	db := p.ExecCfg().DB
	descs := p.ExtendedEvalContext().Descs
	var knobs sql.TTLTestingKnobs
	if ttlKnobs := p.ExecCfg().TTLTestingKnobs; ttlKnobs != nil {
		knobs = *ttlKnobs
	}

	details := t.job.Details().(jobspb.RowLevelTTLDetails)

	aostDuration := -time.Second * 30
	if knobs.AOSTDuration != nil {
		aostDuration = *knobs.AOSTDuration
	}
	aost, err := tree.MakeDTimestampTZ(timeutil.Now().Add(aostDuration), time.Microsecond)
	if err != nil {
		return err
	}

	var initialVersion descpb.DescriptorVersion

	// TODO(#75428): feature flag check, ttl pause check.
	// TODO(#75428): only allow ascending order PKs for now schema side.
	var ttlSettings catpb.RowLevelTTL
	var pkColumns []string
	var pkTypes []*types.T
	var pkDirs []descpb.IndexDescriptor_Direction
	var name string
	var rangeSpan roachpb.Span
	if err := db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		desc, err := descs.GetImmutableTableByID(
			ctx,
			txn,
			details.TableID,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}
		initialVersion = desc.GetVersion()
		// If the AOST timestamp is before the latest descriptor timestamp, exit
		// early as the delete will not work.
		if desc.GetModificationTime().GoTime().After(aost.Time) {
			return errors.Newf(
				"found a recent schema change on the table at %s, aborting",
				desc.GetModificationTime().GoTime().Format(time.RFC3339),
			)
		}
		pkColumns = desc.GetPrimaryIndex().IndexDesc().KeyColumnNames
		for _, id := range desc.GetPrimaryIndex().IndexDesc().KeyColumnIDs {
			col, err := desc.FindColumnWithID(id)
			if err != nil {
				return err
			}
			pkTypes = append(pkTypes, col.GetType())
		}
		pkDirs = desc.GetPrimaryIndex().IndexDesc().KeyColumnDirections

		ttl := desc.GetRowLevelTTL()
		if ttl == nil {
			return errors.Newf("unable to find TTL on table %s", desc.GetName())
		}

		_, dbDesc, err := descs.GetImmutableDatabaseByID(
			ctx,
			txn,
			desc.GetParentID(),
			tree.CommonLookupFlags{
				Required: true,
			},
		)
		if err != nil {
			return err
		}
		schemaDesc, err := descs.GetImmutableSchemaByID(
			ctx,
			txn,
			desc.GetParentSchemaID(),
			tree.CommonLookupFlags{
				Required: true,
			},
		)
		if err != nil {
			return err
		}

		tn := tree.MakeTableNameWithSchema(
			tree.Name(dbDesc.GetName()),
			tree.Name(schemaDesc.GetName()),
			tree.Name(desc.GetName()),
		)
		name = tn.FQString()
		rangeSpan = desc.TableSpan(p.ExecCfg().Codec)
		ttlSettings = *ttl
		return nil
	}); err != nil {
		return err
	}

	metrics := p.ExecCfg().JobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(name)

	var rangeDesc roachpb.RangeDescriptor
	var alloc tree.DatumAlloc
	type rangeToProcess struct {
		startPK, endPK tree.Datums
	}

	g := ctxgroup.WithContext(ctx)

	rangeConcurrency := getRangeConcurrency(p.ExecCfg().SV(), ttlSettings)
	selectBatchSize := getSelectBatchSize(p.ExecCfg().SV(), ttlSettings)
	deleteBatchSize := getDeleteBatchSize(p.ExecCfg().SV(), ttlSettings)
	deleteRateLimit := getDeleteRateLimit(p.ExecCfg().SV(), ttlSettings)
	deleteRateLimiter := quotapool.NewRateLimiter(
		"ttl-delete",
		quotapool.Limit(deleteRateLimit),
		deleteRateLimit,
	)

	ch := make(chan rangeToProcess, rangeConcurrency)
	for i := 0; i < rangeConcurrency; i++ {
		g.GoCtx(func(ctx context.Context) error {
			for r := range ch {
				start := timeutil.Now()
				err := runTTLOnRange(
					ctx,
					p.ExecCfg(),
					details,
					p.ExtendedEvalContext().Descs,
					metrics,
					initialVersion,
					r.startPK,
					r.endPK,
					pkColumns,
					selectBatchSize,
					deleteBatchSize,
					deleteRateLimiter,
					*aost,
				)
				metrics.RangeTotalDuration.RecordValue(int64(timeutil.Since(start)))
				if err != nil {
					// Continue until channel is fully read.
					// Otherwise, the keys input will be blocked.
					for r = range ch {
					}
					return err
				}
			}
			return nil
		})
	}

	if err := func() (retErr error) {
		defer func() {
			close(ch)
			retErr = errors.CombineErrors(retErr, g.Wait())
		}()
		done := false

		batchSize := rangeBatchSize.Get(p.ExecCfg().SV())
		for !done {
			var ranges []kv.KeyValue

			// Scan ranges up to rangeBatchSize.
			if err := db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
				metaStart := keys.RangeMetaKey(keys.MustAddr(rangeSpan.Key).Next())
				metaEnd := keys.RangeMetaKey(keys.MustAddr(rangeSpan.EndKey))

				kvs, err := txn.Scan(ctx, metaStart, metaEnd, batchSize)
				if err != nil {
					return err
				}
				if len(kvs) < int(batchSize) {
					done = true
					if len(kvs) == 0 || !kvs[len(kvs)-1].Key.Equal(metaEnd.AsRawKey()) {
						// Normally we need to scan one more KV because the ranges are addressed by
						// the end key.
						extraKV, err := txn.Scan(ctx, metaEnd, keys.Meta2Prefix.PrefixEnd(), 1 /* one result */)
						if err != nil {
							return err
						}
						kvs = append(kvs, extraKV[0])
					}
				}
				ranges = kvs
				return nil
			}); err != nil {
				return err
			}

			// Send these to each goroutine worker.
			for _, r := range ranges {
				if err := r.ValueProto(&rangeDesc); err != nil {
					return err
				}
				rangeSpan.Key = rangeDesc.EndKey.AsRawKey()
				var nextRange rangeToProcess
				nextRange.startPK, err = keyToDatums(rangeDesc.StartKey, p.ExecCfg().Codec, pkTypes, pkDirs, &alloc)
				if err != nil {
					return err
				}
				nextRange.endPK, err = keyToDatums(rangeDesc.EndKey, p.ExecCfg().Codec, pkTypes, pkDirs, &alloc)
				if err != nil {
					return err
				}
				ch <- nextRange
			}
		}
		return nil
	}(); err != nil {
		return err
	}
	return nil
}

func getSelectBatchSize(sv *settings.Values, ttl catpb.RowLevelTTL) int {
	if bs := ttl.SelectBatchSize; bs != 0 {
		return int(bs)
	}
	return int(defaultSelectBatchSize.Get(sv))
}

func getDeleteBatchSize(sv *settings.Values, ttl catpb.RowLevelTTL) int {
	if bs := ttl.DeleteBatchSize; bs != 0 {
		return int(bs)
	}
	return int(defaultDeleteBatchSize.Get(sv))
}

func getRangeConcurrency(sv *settings.Values, ttl catpb.RowLevelTTL) int {
	if rc := ttl.RangeConcurrency; rc != 0 {
		return int(rc)
	}
	return int(defaultRangeConcurrency.Get(sv))
}

func getDeleteRateLimit(sv *settings.Values, ttl catpb.RowLevelTTL) int64 {
	val := func() int64 {
		if bs := ttl.DeleteRateLimit; bs != 0 {
			return bs
		}
		return defaultDeleteRateLimit.Get(sv)
	}()
	// Put the maximum tokens possible if there is no rate limit.
	if val == 0 {
		return math.MaxInt64
	}
	return val
}

func runTTLOnRange(
	ctx context.Context,
	execCfg *sql.ExecutorConfig,
	details jobspb.RowLevelTTLDetails,
	descriptors *descs.Collection,
	metrics rowLevelTTLMetrics,
	tableVersion descpb.DescriptorVersion,
	startPK tree.Datums,
	endPK tree.Datums,
	pkColumns []string,
	selectBatchSize, deleteBatchSize int,
	deleteRateLimiter *quotapool.RateLimiter,
	aost tree.DTimestampTZ,
) error {
	metrics.NumActiveRanges.Inc(1)
	defer metrics.NumActiveRanges.Dec(1)

	ie := execCfg.InternalExecutor
	db := execCfg.DB

	// TODO(#75428): look at using a dist sql flow job

	selectBuilder := makeSelectQueryBuilder(
		details.TableID,
		details.Cutoff,
		pkColumns,
		startPK,
		endPK,
		aost,
		selectBatchSize,
	)
	deleteBuilder := makeDeleteQueryBuilder(
		details.TableID,
		details.Cutoff,
		pkColumns,
		deleteBatchSize,
	)

	for {
		// Step 1. Fetch some rows we want to delete using a historical
		// SELECT query.
		var expiredRowsPKs []tree.Datums

		if err := db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			var err error
			start := timeutil.Now()
			expiredRowsPKs, err = selectBuilder.run(ctx, ie, txn)
			metrics.DeleteDuration.RecordValue(int64(timeutil.Since(start)))
			return err
		}); err != nil {
			return errors.Wrapf(err, "error selecting rows to delete")
		}
		metrics.RowSelections.Inc(int64(len(expiredRowsPKs)))

		// Step 2. Delete the rows which have expired.

		for startRowIdx := 0; startRowIdx < len(expiredRowsPKs); startRowIdx += deleteBatchSize {
			until := startRowIdx + deleteBatchSize
			if until > len(expiredRowsPKs) {
				until = len(expiredRowsPKs)
			}
			deleteBatch := expiredRowsPKs[startRowIdx:until]
			if err := db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
				// If we detected a schema change here, the delete will not succeed
				// (the SELECT still will because of the AOST). Early exit here.
				desc, err := descriptors.GetImmutableTableByID(
					ctx,
					txn,
					details.TableID,
					tree.ObjectLookupFlagsWithRequired(),
				)
				if err != nil {
					return err
				}
				version := desc.GetVersion()
				if mockVersion := execCfg.TTLTestingKnobs.MockDescriptorVersionDuringDelete; mockVersion != nil {
					version = *mockVersion
				}
				if version != tableVersion {
					return errors.Newf(
						"table has had a schema change since the job has started at %s, aborting",
						desc.GetModificationTime().GoTime().Format(time.RFC3339),
					)
				}

				tokens, err := deleteRateLimiter.Acquire(ctx, int64(len(deleteBatch)))
				if err != nil {
					return err
				}
				defer tokens.Consume()

				// TODO(#75428): configure admission priority
				start := timeutil.Now()
				err = deleteBuilder.run(ctx, ie, txn, deleteBatch)
				metrics.DeleteDuration.RecordValue(int64(timeutil.Since(start)))
				return err
			}); err != nil {
				return errors.Wrapf(err, "error during row deletion")
			}
			metrics.RowDeletions.Inc(int64(len(deleteBatch)))
		}

		// Step 3. Early exit if necessary.

		// If we selected less than the select batch size, we have selected every
		// row and so we end it here.
		if len(expiredRowsPKs) < selectBatchSize {
			break
		}
	}
	return nil
}

// keyToDatums translates a RKey on a range for a table to the appropriate datums.
func keyToDatums(
	key roachpb.RKey,
	codec keys.SQLCodec,
	pkTypes []*types.T,
	pkDirs []descpb.IndexDescriptor_Direction,
	alloc *tree.DatumAlloc,
) (tree.Datums, error) {
	// If any of these errors, that means we reached an "empty" key, which
	// symbolizes the start or end of a range.
	if _, _, err := codec.DecodeTablePrefix(key.AsRawKey()); err != nil {
		return nil, nil //nolint:returnerrcheck
	}
	if _, _, _, err := codec.DecodeIndexPrefix(key.AsRawKey()); err != nil {
		return nil, nil //nolint:returnerrcheck
	}
	encDatums := make([]rowenc.EncDatum, len(pkTypes))
	if _, foundNull, err := rowenc.DecodeIndexKey(
		codec,
		pkTypes,
		encDatums,
		pkDirs,
		key.AsRawKey(),
	); err != nil {
		return nil, err
	} else if foundNull {
		return nil, nil
	}
	datums := make(tree.Datums, len(pkTypes))
	for i, encDatum := range encDatums {
		if err := encDatum.EnsureDecoded(pkTypes[i], alloc); err != nil {
			return nil, err
		}
		datums[i] = encDatum.Datum
	}
	return datums, nil
}

// OnFailOrCancel implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) OnFailOrCancel(ctx context.Context, execCtx interface{}) error {
	return nil
}

func init() {
	jobs.RegisterConstructor(jobspb.TypeRowLevelTTL, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &rowLevelTTLResumer{
			job: job,
			st:  settings,
		}
	})
	jobs.MakeRowLevelTTLMetricsHook = makeRowLevelTTLAggMetrics
}
