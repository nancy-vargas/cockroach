// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package streamingest

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
)

func distStreamIngestionPlanSpecs(
	streamAddress streamingccl.StreamAddress,
	topology streamingccl.Topology,
	nodes []roachpb.NodeID,
	jobID int64,
) ([]*execinfrapb.StreamIngestionDataSpec, *execinfrapb.StreamIngestionFrontierSpec, error) {

	// For each stream partition in the topology, assign it to a node.
	streamIngestionSpecs := make([]*execinfrapb.StreamIngestionDataSpec, 0, len(nodes))

	trackedSpans := make([]roachpb.Span, 0)
	for i, partition := range topology.Partitions {
		// Round robin assign the stream partitions to nodes. Partitions 0 through
		// len(nodes) - 1 creates the spec. Future partitions just add themselves to
		// the partition addresses.
		if i < len(nodes) {
			spec := &execinfrapb.StreamIngestionDataSpec{
				StreamAddress:      streamAddress,
				PartitionAddresses: make([]streamingccl.PartitionAddress, 0),
			}
			streamIngestionSpecs = append(streamIngestionSpecs, spec)
		}
		n := i % len(nodes)
		streamIngestionSpecs[n].PartitionAddresses = append(streamIngestionSpecs[n].PartitionAddresses,
			partition)
		partitionKey := roachpb.Key(partition)
		// We create "fake" spans to uniquely identify the partition. This is used
		// to keep track of the resolved ts received for a particular partition in
		// the frontier processor.
		trackedSpans = append(trackedSpans, roachpb.Span{
			Key:    partitionKey,
			EndKey: partitionKey.Next(),
		})
	}

	// Create a spec for the StreamIngestionFrontier processor on the coordinator
	// node.
	// TODO: set HighWaterAtStart once the job progress logic has been hooked up.
	streamIngestionFrontierSpec := &execinfrapb.StreamIngestionFrontierSpec{TrackedSpans: trackedSpans}

	return streamIngestionSpecs, streamIngestionFrontierSpec, nil
}

func distStreamIngest(
	ctx context.Context,
	execCtx sql.JobExecContext,
	nodes []roachpb.NodeID,
	jobID int64,
	planCtx *sql.PlanningCtx,
	dsp *sql.DistSQLPlanner,
	streamIngestionSpecs []*execinfrapb.StreamIngestionDataSpec,
	streamIngestionFrontierSpec *execinfrapb.StreamIngestionFrontierSpec,
) error {
	ctx = logtags.AddTag(ctx, "stream-ingest-distsql", nil)
	evalCtx := execCtx.ExtendedEvalContext()
	var noTxn *kv.Txn

	if len(streamIngestionSpecs) == 0 {
		return nil
	}

	// Setup a one-stage plan with one proc per input spec.
	corePlacement := make([]physicalplan.ProcessorCorePlacement, len(streamIngestionSpecs))
	for i := range streamIngestionSpecs {
		corePlacement[i].NodeID = nodes[i]
		corePlacement[i].Core.StreamIngestionData = streamIngestionSpecs[i]
	}

	p := planCtx.NewPhysicalPlan()
	p.AddNoInputStage(
		corePlacement,
		execinfrapb.PostProcessSpec{},
		streamIngestionResultTypes,
		execinfrapb.Ordering{},
	)

	execCfg := execCtx.ExecCfg()
	gatewayNodeID, err := execCfg.NodeID.OptionalNodeIDErr(48274)
	if err != nil {
		return err
	}

	// The ResultRouters from the previous stage will feed in to the
	// StreamIngestionFrontier processor.
	p.AddSingleGroupStage(gatewayNodeID,
		execinfrapb.ProcessorCoreUnion{StreamIngestionFrontier: streamIngestionFrontierSpec},
		execinfrapb.PostProcessSpec{}, streamIngestionResultTypes)

	p.PlanToStreamColMap = []int{0}
	dsp.FinalizePlan(planCtx, p)

	rw := makeStreamIngestionResultWriter(ctx, jobID, execCfg.JobRegistry)

	recv := sql.MakeDistSQLReceiver(
		ctx,
		rw,
		tree.Rows,
		nil, /* rangeCache */
		noTxn,
		nil, /* clockUpdater */
		evalCtx.Tracing,
		execCfg.ContentionRegistry,
	)
	defer recv.Release()

	// Copy the evalCtx, as dsp.Run() might change it.
	evalCtxCopy := *evalCtx
	dsp.Run(planCtx, noTxn, p, recv, &evalCtxCopy, nil /* finishedSetupFn */)
	return rw.Err()
}

type streamIngestionResultWriter struct {
	ctx          context.Context
	registry     *jobs.Registry
	jobID        int64
	rowsAffected int
	err          error
}

func makeStreamIngestionResultWriter(
	ctx context.Context, jobID int64, registry *jobs.Registry,
) *streamIngestionResultWriter {
	return &streamIngestionResultWriter{
		ctx:      ctx,
		registry: registry,
		jobID:    jobID,
	}
}

// AddRow implements the sql.rowResultWriter interface.
func (s *streamIngestionResultWriter) AddRow(ctx context.Context, row tree.Datums) error {
	if len(row) == 0 {
		return errors.New("streamIngestionResultWriter received an empty row")
	}
	if row[0] == nil {
		return errors.New("streamIngestionResultWriter expects non-nil row entry")
	}

	job, err := s.registry.LoadJob(ctx, s.jobID)
	if err != nil {
		return err
	}
	return job.HighWaterProgressed(s.ctx, func(ctx context.Context, txn *kv.Txn,
		details jobspb.ProgressDetails) (hlc.Timestamp, error) {
		// Decode the row and write the ts.
		var ingestedHighWatermark hlc.Timestamp
		if err := protoutil.Unmarshal([]byte(*row[0].(*tree.DBytes)),
			&ingestedHighWatermark); err != nil {
			return ingestedHighWatermark, errors.NewAssertionErrorWithWrappedErrf(err,
				`unmarshalling resolved timestamp`)
		}
		return ingestedHighWatermark, nil
	})
}

// IncrementRowsAffected implements the sql.rowResultWriter interface.
func (s *streamIngestionResultWriter) IncrementRowsAffected(n int) {
	s.rowsAffected += n
}

// SetError implements the sql.rowResultWriter interface.
func (s *streamIngestionResultWriter) SetError(err error) {
	s.err = err
}

// Err implements the sql.rowResultWriter interface.
func (s *streamIngestionResultWriter) Err() error {
	return s.err
}