// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cli

import (
	"context"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"math/rand"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// runInitialSQL concerns itself with running "initial SQL" code when
// a cluster is started for the first time.
//
// The "startSingleNode" argument is true for `start-single-node`,
// and `cockroach demo` with 2 nodes or fewer.
// If adminUser is non-empty, an admin user with that name is
// created upon initialization. Its password is then also returned.
func runInitialSQL(
	ctx context.Context, s *server.Server, startSingleNode bool, adminUser, adminPassword string,
) error {
	newCluster := s.InitialStart() && s.NodeID() == kvserver.FirstNodeID
	if !newCluster {
		// The initial SQL code only runs the first time the cluster is initialized.
		return nil
	}

	if startSingleNode {
		// For start-single-node, set the default replication factor to
		// 1 so as to avoid warning messages and unnecessary rebalance
		// churn.
		if err := cliDisableReplication(ctx, s); err != nil {
			log.Ops.Errorf(ctx, "could not disable replication: %v", err)
			return err
		}
		log.Ops.Infof(ctx, "Replication was disabled for this cluster.\n"+
			"When/if adding nodes in the future, update zone configurations to increase the replication factor.")

		err := seedHHR(ctx, s)
		if err != nil {
			return err
		}
	}

	if adminUser != "" && !s.Insecure() {
		if err := createAdminUser(ctx, s, adminUser, adminPassword); err != nil {
			return err
		}
	}

	return nil
}

func seedHHR(ctx context.Context, s *server.Server) error {

	fakeKey := func() string {
		keyLength := 10
		alphabet := "ab" // limit key entropy to 2 ^ 16 = 65536
		var strBuilder strings.Builder

		for i := 0; i < keyLength; i++ {
			char := alphabet[rand.Intn(len(alphabet))]
			strBuilder.WriteString(string(char))
		}

		return strBuilder.String()
	}

	return s.RunLocalSQL(ctx,
		func(ctx context.Context, ie *sql.InternalExecutor) error {
			_, err := ie.Exec(ctx, "clear system.hot_ranges", nil, fmt.Sprintf("DELETE FROM system.hot_ranges"))
			if err != nil {
				return err
			}

			nSamples := 4 * 24 * 14 // 6 hours
			nRanges := 1000

			now := time.Now()
			startTime := now.UnixNano()
			intervalTimeNanos := int64(15 * 60 * 1000000000)

			for i := 0; i < nSamples; i++ {

				keys := make([]string, nRanges)
				values := make([]float32, nRanges)

				for r := 0; r < nRanges; r++ {
					keys[r] = fakeKey()
					values[r] = rand.Float32()
				}

				sample := serverpb.HHRResponse_HHRSample{
					Timestamp: &hlc.Timestamp{
						WallTime:  startTime + (int64(i) * intervalTimeNanos),
						Logical:   0,
						Synthetic: false,
					},
					StartKey: keys,
					Qps:      values,
				}

				serialized, serializationError := sample.Marshal()
				if serializationError != nil {
					return err
				}

				// insert into db
				_, err := ie.Exec(ctx, "seed system.hot_ranges", nil, "INSERT INTO system.hot_ranges (tenant_id, info) VALUES (1, $1);", serialized)

				if err != nil {
					return err
				}
			}

			return nil
		})
}

// createAdminUser creates an admin user with the given name.
func createAdminUser(ctx context.Context, s *server.Server, adminUser, adminPassword string) error {
	return s.RunLocalSQL(ctx,
		func(ctx context.Context, ie *sql.InternalExecutor) error {
			_, err := ie.Exec(
				ctx, "admin-user", nil,
				fmt.Sprintf("CREATE USER %s WITH PASSWORD $1", adminUser),
				adminPassword,
			)
			if err != nil {
				return err
			}
			// TODO(knz): Demote the admin user to an operator privilege with fewer options.
			_, err = ie.Exec(ctx, "admin-user", nil, fmt.Sprintf("GRANT admin TO %s", tree.Name(adminUser)))
			return err
		})
}

// cliDisableReplication changes the replication factor on
// all defined zones to become 1. This is used by start-single-node
// and demo to define single-node clusters, so as to avoid
// churn in the log files.
//
// The change is effected using the internal SQL interface of the
// given server object.
func cliDisableReplication(ctx context.Context, s *server.Server) error {
	return s.RunLocalSQL(ctx,
		func(ctx context.Context, ie *sql.InternalExecutor) (retErr error) {
			it, err := ie.QueryIterator(ctx, "get-zones", nil,
				"SELECT target FROM crdb_internal.zones")
			if err != nil {
				return err
			}
			// We have to make sure to close the iterator since we might return
			// from the for loop early (before Next() returns false).
			defer func() { retErr = errors.CombineErrors(retErr, it.Close()) }()

			var ok bool
			for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
				zone := string(*it.Cur()[0].(*tree.DString))
				if _, err := ie.Exec(ctx, "set-zone", nil,
					fmt.Sprintf("ALTER %s CONFIGURE ZONE USING num_replicas = 1", zone)); err != nil {
					return err
				}
			}
			return err
		})
}
