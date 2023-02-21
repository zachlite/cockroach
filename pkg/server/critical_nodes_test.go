package server

import (
	"context"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestCriticalNodes(t *testing.T) {

	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	//tc := serverutils.StartNewTestCluster(t, 3, base.TestClusterArgs{
	//	ReplicationMode: base.ReplicationManual, // saves time
	//})

	// create a table
	// give that table a zone configuration that is impossible to be met
	// like num_replicas = 5
	// ask for Critical nodes, and see the badness.

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	_ = sqlDB
	_ = kvDB
	defer s.Stopper().Stop(ctx)

	result, err := s.StatusServer().(serverpb.StatusServer).CriticalNodes(ctx, &serverpb.CriticalNodesRequest{})
	require.NoError(t, err)

	log.Infof(ctx, "result: %v", result)
	require.Equal(t, 1, 21, "%v", result)
}
