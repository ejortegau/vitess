/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtgate

import (
	"context"
	"io"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/stats"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/discovery"
	"vitess.io/vitess/go/vt/log"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtgatepb "vitess.io/vitess/go/vt/proto/vtgate"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/srvtopo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	econtext "vitess.io/vitess/go/vt/vtgate/executorcontext"
	"vitess.io/vitess/go/vt/vttablet/queryservice"
)

// ScatterConn is used for executing queries across
// multiple shard level connections.
type ScatterConn struct {
	timings              *stats.MultiTimings
	tabletCallErrorCount *stats.CountersWithMultiLabels
	txConn               *TxConn
	gateway              *TabletGateway
}

// shardActionFunc defines the contract for a shard action
// outside of a transaction. Every such function executes the
// necessary action on a shard, sends the results to sResults, and
// return an error if any.  multiGo is capable of executing
// multiple shardActionFunc actions in parallel and
// consolidating the results and errors for the caller.
type shardActionFunc func(rs *srvtopo.ResolvedShard, i int) error

// shardActionTransactionFunc defines the contract for a shard action
// that may be in a transaction. Every such function executes the
// necessary action on a shard (with an optional Begin call), aggregates
// the results, and return an error if any.
// multiGoTransaction is capable of executing multiple
// shardActionTransactionFunc actions in parallel and consolidating
// the results and errors for the caller.
type shardActionTransactionFunc func(rs *srvtopo.ResolvedShard, i int, shardActionInfo *shardActionInfo) (*shardActionInfo, error)

type (
	nullResultsObserver struct{}
)

func (nullResultsObserver) Observe(*sqltypes.Result) {}

// NewScatterConn creates a new ScatterConn.
func NewScatterConn(statsName string, txConn *TxConn, gw *TabletGateway) *ScatterConn {
	// this only works with TabletGateway
	tabletCallErrorCountStatsName := ""
	if statsName != "" {
		tabletCallErrorCountStatsName = statsName + "ErrorCount"
	}
	return &ScatterConn{
		timings: stats.NewMultiTimings(
			statsName,
			"Scatter connection timings",
			[]string{"Operation", "Keyspace", "ShardName", "DbType"}),
		tabletCallErrorCount: stats.NewCountersWithMultiLabels(
			tabletCallErrorCountStatsName,
			"Error count from tablet calls in scatter conns",
			[]string{"Operation", "Keyspace", "ShardName", "DbType"}),
		txConn:  txConn,
		gateway: gw,
	}
}

func (stc *ScatterConn) startAction(name string, target *querypb.Target) (time.Time, []string) {
	statsKey := []string{name, target.Keyspace, target.Shard, topoproto.TabletTypeLString(target.TabletType)}
	startTime := time.Now()
	return startTime, statsKey
}

func (stc *ScatterConn) endAction(startTime time.Time, allErrors *concurrency.AllErrorRecorder, statsKey []string, err *error, session *econtext.SafeSession) {
	if *err != nil {
		allErrors.RecordError(*err)
		// Don't increment the error counter for duplicate
		// keys or bad queries, as those errors are caused by
		// client queries and are not VTGate's fault.
		ec := vterrors.Code(*err)
		if ec != vtrpcpb.Code_ALREADY_EXISTS && ec != vtrpcpb.Code_INVALID_ARGUMENT {
			stc.tabletCallErrorCount.Add(statsKey, 1)
		}
		if ec == vtrpcpb.Code_RESOURCE_EXHAUSTED || ec == vtrpcpb.Code_ABORTED {
			session.SetRollback()
		}
	}
	stc.timings.Record(statsKey, startTime)
}

func (stc *ScatterConn) endLockAction(startTime time.Time, allErrors *concurrency.AllErrorRecorder, statsKey []string, err *error) {
	if *err != nil {
		allErrors.RecordError(*err)
		stc.tabletCallErrorCount.Add(statsKey, 1)
	}
	stc.timings.Record(statsKey, startTime)
}

type reset int

const (
	none reset = iota
	shard
	newQS
)

// ExecuteMultiShard is like Execute,
// but each shard gets its own Sql Queries and BindVariables.
//
// It always returns a non-nil query result and an array of
// shard errors which may be nil so that callers can optionally
// process a partially-successful operation.
func (stc *ScatterConn) ExecuteMultiShard(
	ctx context.Context,
	primitive engine.Primitive,
	rss []*srvtopo.ResolvedShard,
	queries []*querypb.BoundQuery,
	session *econtext.SafeSession,
	autocommit bool,
	ignoreMaxMemoryRows bool,
	resultsObserver econtext.ResultsObserver,
	fetchLastInsertID bool,
) (qr *sqltypes.Result, errs []error) {

	if len(rss) != len(queries) {
		return nil, []error{vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] got mismatched number of queries and shards")}
	}

	// mu protects qr
	var mu sync.Mutex
	qr = new(sqltypes.Result)

	if session.InLockSession() && triggerLockHeartBeat(session) {
		go stc.runLockQuery(ctx, session)
	}

	if session.Options != nil {
		session.Options.FetchLastInsertId = fetchLastInsertID
	}

	allErrors := stc.multiGoTransaction(
		ctx,
		"Execute",
		rss,
		session,
		autocommit,
		func(rs *srvtopo.ResolvedShard, i int, info *shardActionInfo) (*shardActionInfo, error) {
			var (
				innerqr *sqltypes.Result
				err     error
				opts    *querypb.ExecuteOptions
				alias   *topodatapb.TabletAlias
				qs      queryservice.QueryService
			)
			transactionID := info.transactionID
			reservedID := info.reservedID

			if session != nil && session.Session != nil {
				opts = session.Session.Options
			}

			if opts == nil && fetchLastInsertID {
				opts = &querypb.ExecuteOptions{FetchLastInsertId: fetchLastInsertID}
			}

			if autocommit {
				// As this is auto-commit, the transactionID is supposed to be zero.
				if transactionID != int64(0) {
					return nil, vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "in autocommit mode, transactionID should be zero but was: %d", transactionID)
				}
			}

			qs, err = getQueryService(ctx, rs, info, session, false)
			if err != nil {
				return nil, err
			}

			retryRequest := func(exec func()) {
				retry := checkAndResetShardSession(info, err, session, rs.Target)
				switch retry {
				case newQS:
					// Current tablet is not available, try querying new tablet using gateway.
					qs = rs.Gateway
					fallthrough
				case shard:
					// if we need to reset a reserved connection, here is our chance to try executing again,
					// against a new connection
					exec()
				}
			}

			switch info.actionNeeded {
			case nothing:
				innerqr, err = qs.Execute(ctx, rs.Target, queries[i].Sql, queries[i].BindVariables, info.transactionID, info.reservedID, opts)
				if err != nil {
					retryRequest(func() {
						// we seem to have lost our connection. it was a reserved connection, let's try to recreate it
						info.actionNeeded = reserve
						info.ignoreOldSession = true
						var state queryservice.ReservedState
						state, innerqr, err = qs.ReserveExecute(ctx, rs.Target, session.SetPreQueries(), queries[i].Sql, queries[i].BindVariables, 0 /*transactionId*/, opts)
						reservedID = state.ReservedID
						alias = state.TabletAlias
					})
				}
			case begin:
				var state queryservice.TransactionState
				state, innerqr, err = qs.BeginExecute(ctx, rs.Target, session.SavePoints(), queries[i].Sql, queries[i].BindVariables, reservedID, opts)
				transactionID = state.TransactionID
				alias = state.TabletAlias
				if err != nil {
					retryRequest(func() {
						// we seem to have lost our connection. it was a reserved connection, let's try to recreate it
						info.actionNeeded = reserveBegin
						info.ignoreOldSession = true
						var state queryservice.ReservedTransactionState
						state, innerqr, err = qs.ReserveBeginExecute(ctx, rs.Target, session.SetPreQueries(), session.SavePoints(), queries[i].Sql, queries[i].BindVariables, opts)
						transactionID = state.TransactionID
						reservedID = state.ReservedID
						alias = state.TabletAlias
					})
				}
			case reserve:
				var state queryservice.ReservedState
				state, innerqr, err = qs.ReserveExecute(ctx, rs.Target, session.SetPreQueries(), queries[i].Sql, queries[i].BindVariables, transactionID, opts)
				reservedID = state.ReservedID
				alias = state.TabletAlias
			case reserveBegin:
				var state queryservice.ReservedTransactionState
				state, innerqr, err = qs.ReserveBeginExecute(ctx, rs.Target, session.SetPreQueries(), session.SavePoints(), queries[i].Sql, queries[i].BindVariables, opts)
				transactionID = state.TransactionID
				reservedID = state.ReservedID
				alias = state.TabletAlias
			default:
				return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] unexpected actionNeeded on query execution: %v", info.actionNeeded)
			}
			session.Log(primitive, rs.Target, rs.Gateway, queries[i].Sql, info.actionNeeded == begin || info.actionNeeded == reserveBegin, queries[i].BindVariables)

			// We need to new shard info irrespective of the error.
			newInfo := info.updateTransactionAndReservedID(transactionID, reservedID, alias, innerqr)
			if err != nil {
				return newInfo, err
			}
			mu.Lock()
			defer mu.Unlock()

			if innerqr != nil {
				resultsObserver.Observe(innerqr)
			}

			// Don't append more rows if row count is exceeded.
			if ignoreMaxMemoryRows || len(qr.Rows) <= maxMemoryRows {
				qr.AppendResult(innerqr)
			}
			return newInfo, nil
		},
	)

	if !ignoreMaxMemoryRows && len(qr.Rows) > maxMemoryRows {
		return nil, []error{vterrors.NewErrorf(vtrpcpb.Code_RESOURCE_EXHAUSTED, vterrors.NetPacketTooLarge, "in-memory row count exceeded allowed limit of %d", maxMemoryRows)}
	}

	return qr, allErrors.GetErrors()
}

func triggerLockHeartBeat(session *econtext.SafeSession) bool {
	now := time.Now().Unix()
	lastHeartbeat := session.GetLockHeartbeat()
	return now-lastHeartbeat >= int64(lockHeartbeatTime.Seconds())
}

func (stc *ScatterConn) runLockQuery(ctx context.Context, session *econtext.SafeSession) {
	rs := &srvtopo.ResolvedShard{Target: session.LockSession.Target, Gateway: stc.gateway}
	query := &querypb.BoundQuery{Sql: "select 1", BindVariables: nil}
	_, lockErr := stc.ExecuteLock(ctx, rs, query, session, sqlparser.IsUsedLock)
	if lockErr != nil {
		log.Warningf("Locking heartbeat failed, held locks might be released: %s", lockErr.Error())
	}
}

func checkAndResetShardSession(info *shardActionInfo, err error, session *econtext.SafeSession, target *querypb.Target) reset {
	retry := none
	if info.reservedID != 0 && info.transactionID == 0 {
		if wasConnectionClosed(err) {
			retry = shard
		}
		if requireNewQS(err, target) {
			retry = newQS
		}
	}
	if retry != none {
		_ = session.ResetShard(info.alias)
	}
	return retry
}

func getQueryService(ctx context.Context, rs *srvtopo.ResolvedShard, info *shardActionInfo, session *econtext.SafeSession, skipReset bool) (queryservice.QueryService, error) {
	if info.alias == nil {
		return rs.Gateway, nil
	}
	qs, err := rs.Gateway.QueryServiceByAlias(ctx, info.alias, rs.Target)
	if err != nil && info.transactionID != 0 {
		err = vterrors.VT15001(vterrors.Code(err), err.Error())
	}

	if err == nil || skipReset {
		return qs, err
	}
	// If the session info has only reserved connection and no transaction then we will route it through gateway
	// Otherwise, we will fail.
	if info.reservedID == 0 || info.transactionID != 0 {
		return nil, err
	}
	err = session.ResetShard(info.alias)
	if err != nil {
		return nil, err
	}
	// Returning rs.Gateway will make the gateway to choose new healthy tablet for the targeted tablet type.
	return rs.Gateway, nil
}

func (stc *ScatterConn) processOneStreamingResult(mu *sync.Mutex, fieldSent *bool, qr *sqltypes.Result, callback func(*sqltypes.Result) error) error {
	mu.Lock()
	defer mu.Unlock()
	if *fieldSent {
		if len(qr.Rows) == 0 {
			// It's another field info result. Don't send.
			return nil
		}
	} else {
		if len(qr.Fields) == 0 {
			// Unreachable: this can happen only if vttablet misbehaves.
			return vterrors.VT13001("received rows before fields")
		}
		*fieldSent = true
	}

	return callback(qr)
}

// StreamExecuteMulti is like StreamExecute,
// but each shard gets its own bindVars. If len(shards) is not equal to
// len(bindVars), the function panics.
// Note we guarantee the callback will not be called concurrently
// by multiple go routines, through processOneStreamingResult.
func (stc *ScatterConn) StreamExecuteMulti(
	ctx context.Context,
	primitive engine.Primitive,
	query string,
	rss []*srvtopo.ResolvedShard,
	bindVars []map[string]*querypb.BindVariable,
	session *econtext.SafeSession,
	autocommit bool,
	callback func(reply *sqltypes.Result) error,
	resultsObserver econtext.ResultsObserver,
	fetchLastInsertID bool,
) []error {
	if session.InLockSession() && triggerLockHeartBeat(session) {
		go stc.runLockQuery(ctx, session)
	}

	observedCallback := func(reply *sqltypes.Result) error {
		if reply != nil {
			resultsObserver.Observe(reply)
		}
		return callback(reply)
	}

	if session.Options != nil {
		session.Options.FetchLastInsertId = fetchLastInsertID
	}

	allErrors := stc.multiGoTransaction(
		ctx,
		"StreamExecute",
		rss,
		session,
		autocommit,
		func(rs *srvtopo.ResolvedShard, i int, info *shardActionInfo) (*shardActionInfo, error) {
			var (
				err   error
				opts  *querypb.ExecuteOptions
				alias *topodatapb.TabletAlias
				qs    queryservice.QueryService
			)
			transactionID := info.transactionID
			reservedID := info.reservedID

			if session != nil && session.Session != nil {
				opts = session.Session.Options
			}

			if opts == nil && fetchLastInsertID {
				opts = &querypb.ExecuteOptions{FetchLastInsertId: fetchLastInsertID}
			}

			if autocommit {
				// As this is auto-commit, the transactionID is supposed to be zero.
				if transactionID != int64(0) {
					return nil, vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "in autocommit mode, transactionID should be zero but was: %d", transactionID)
				}
			}

			qs, err = getQueryService(ctx, rs, info, session, false)
			if err != nil {
				return nil, err
			}

			retryRequest := func(exec func()) {
				retry := checkAndResetShardSession(info, err, session, rs.Target)
				switch retry {
				case newQS:
					// Current tablet is not available, try querying new tablet using gateway.
					qs = rs.Gateway
					fallthrough
				case shard:
					// if we need to reset a reserved connection, here is our chance to try executing again,
					// against a new connection
					exec()
				}
			}

			switch info.actionNeeded {
			case nothing:
				err = qs.StreamExecute(ctx, rs.Target, query, bindVars[i], transactionID, reservedID, opts, observedCallback)
				if err != nil {
					retryRequest(func() {
						// we seem to have lost our connection. it was a reserved connection, let's try to recreate it
						info.actionNeeded = reserve
						var state queryservice.ReservedState
						state, err = qs.ReserveStreamExecute(ctx, rs.Target, session.SetPreQueries(), query, bindVars[i], 0 /*transactionId*/, opts, observedCallback)
						reservedID = state.ReservedID
						alias = state.TabletAlias
					})
				}
			case begin:
				var state queryservice.TransactionState
				state, err = qs.BeginStreamExecute(ctx, rs.Target, session.SavePoints(), query, bindVars[i], reservedID, opts, observedCallback)
				transactionID = state.TransactionID
				alias = state.TabletAlias
				if err != nil {
					retryRequest(func() {
						// we seem to have lost our connection. it was a reserved connection, let's try to recreate it
						info.actionNeeded = reserveBegin
						var state queryservice.ReservedTransactionState
						state, err = qs.ReserveBeginStreamExecute(ctx, rs.Target, session.SetPreQueries(), session.SavePoints(), query, bindVars[i], opts, observedCallback)
						transactionID = state.TransactionID
						reservedID = state.ReservedID
						alias = state.TabletAlias
					})
				}
			case reserve:
				var state queryservice.ReservedState
				state, err = qs.ReserveStreamExecute(ctx, rs.Target, session.SetPreQueries(), query, bindVars[i], transactionID, opts, observedCallback)
				reservedID = state.ReservedID
				alias = state.TabletAlias
			case reserveBegin:
				var state queryservice.ReservedTransactionState
				state, err = qs.ReserveBeginStreamExecute(ctx, rs.Target, session.SetPreQueries(), session.SavePoints(), query, bindVars[i], opts, observedCallback)
				transactionID = state.TransactionID
				reservedID = state.ReservedID
				alias = state.TabletAlias
			default:
				return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] unexpected actionNeeded on query execution: %v", info.actionNeeded)
			}
			session.Log(primitive, rs.Target, rs.Gateway, query, info.actionNeeded == begin || info.actionNeeded == reserveBegin, bindVars[i])

			// We need the new shard info irrespective of the error.
			newInfo := info.updateTransactionAndReservedID(transactionID, reservedID, alias, nil)
			if err != nil {
				return newInfo, err
			}

			return newInfo, nil
		},
	)
	return allErrors.GetErrors()
}

// timeTracker is a convenience wrapper used by MessageStream
// to track how long a stream has been unavailable.
type timeTracker struct {
	mu         sync.Mutex
	timestamps map[*querypb.Target]time.Time
}

func newTimeTracker() *timeTracker {
	return &timeTracker{
		timestamps: make(map[*querypb.Target]time.Time),
	}
}

// Reset resets the timestamp set by Record.
func (tt *timeTracker) Reset(target *querypb.Target) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	delete(tt.timestamps, target)
}

// Record records the time to Now if there was no previous timestamp,
// and it keeps returning that value until the next Reset.
func (tt *timeTracker) Record(target *querypb.Target) time.Time {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	last, ok := tt.timestamps[target]
	if !ok {
		last = time.Now()
		tt.timestamps[target] = last
	}
	return last
}

// MessageStream streams messages from the specified shards.
// Note we guarantee the callback will not be called concurrently
// by multiple go routines, through processOneStreamingResult.
func (stc *ScatterConn) MessageStream(ctx context.Context, rss []*srvtopo.ResolvedShard, name string, callback func(*sqltypes.Result) error) error {
	// The cancelable context is used for handling errors
	// from individual streams.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// mu is used to merge multiple callback calls into one.
	var mu sync.Mutex
	fieldSent := false
	lastErrors := newTimeTracker()
	allErrors := stc.multiGo("MessageStream", rss, func(rs *srvtopo.ResolvedShard, i int) error {
		// This loop handles the case where a reparent happens, which can cause
		// an individual stream to end. If we don't succeed on the retries for
		// messageStreamGracePeriod, we abort and return an error.
		for {
			err := rs.Gateway.MessageStream(ctx, rs.Target, name, func(qr *sqltypes.Result) error {
				lastErrors.Reset(rs.Target)
				return stc.processOneStreamingResult(&mu, &fieldSent, qr, callback)
			})
			// nil and EOF are equivalent. UNAVAILABLE can be returned by vttablet if it's demoted
			// from primary to replica. For any of these conditions, we have to retry.
			if err != nil && err != io.EOF && vterrors.Code(err) != vtrpcpb.Code_UNAVAILABLE {
				cancel()
				return err
			}

			// There was no error. We have to see if we need to retry.
			// If context was canceled, likely due to client disconnect,
			// return normally without retrying.
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			firstErrorTimeStamp := lastErrors.Record(rs.Target)
			if time.Since(firstErrorTimeStamp) >= messageStreamGracePeriod {
				// Cancel all streams and return an error.
				cancel()
				return vterrors.Errorf(vtrpcpb.Code_DEADLINE_EXCEEDED, "message stream from %v has repeatedly failed for longer than %v", rs.Target, messageStreamGracePeriod)
			}

			// It's not been too long since our last good send. Wait and retry.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(messageStreamGracePeriod / 5):
			}
		}
	})
	return allErrors.AggrError(vterrors.Aggregate)
}

// Close closes the underlying Gateway.
func (stc *ScatterConn) Close() error {
	return stc.gateway.Close(context.Background())
}

// GetGatewayCacheStatus returns a displayable version of the Gateway cache.
func (stc *ScatterConn) GetGatewayCacheStatus() TabletCacheStatusList {
	return stc.gateway.CacheStatus()
}

// GetHealthCheckCacheStatus returns a displayable version of the HealthCheck cache.
func (stc *ScatterConn) GetHealthCheckCacheStatus() discovery.TabletsCacheStatusList {
	return stc.gateway.TabletsCacheStatus()
}

// multiGo performs the requested 'action' on the specified
// shards in parallel. This does not handle any transaction state.
// The action function must match the shardActionFunc2 signature.
func (stc *ScatterConn) multiGo(
	name string,
	rss []*srvtopo.ResolvedShard,
	action shardActionFunc,
) (allErrors *concurrency.AllErrorRecorder) {
	allErrors = new(concurrency.AllErrorRecorder)
	if len(rss) == 0 {
		return allErrors
	}

	oneShard := func(rs *srvtopo.ResolvedShard, i int) {
		var err error
		startTime, statsKey := stc.startAction(name, rs.Target)
		// Send a dummy session.
		// TODO(sougou): plumb a real session through this call.
		defer stc.endAction(startTime, allErrors, statsKey, &err, econtext.NewSafeSession(nil))
		err = action(rs, i)
	}

	if len(rss) == 1 {
		// only one shard, do it synchronously.
		oneShard(rss[0], 0)
		return allErrors
	}

	var wg sync.WaitGroup
	for i, rs := range rss {
		wg.Add(1)
		go func(rs *srvtopo.ResolvedShard, i int) {
			defer wg.Done()
			oneShard(rs, i)
		}(rs, i)
	}
	wg.Wait()
	return allErrors
}

// panicData is used to capture panics during parallel execution.
type panicData struct {
	p     any
	trace []byte
}

// multiGoTransaction performs the requested 'action' on the specified
// ResolvedShards in parallel. For each shard, if the requested
// session is in a transaction, it opens a new transactions on the connection,
// and updates the Session with the transaction id. If the session already
// contains a transaction id for the shard, it reuses it.
// The action function must match the shardActionTransactionFunc signature.
//
// It returns an error recorder in which each shard error is recorded positionally,
// i.e. if rss[2] had an error, then the error recorder will store that error
// in the second position.
func (stc *ScatterConn) multiGoTransaction(
	ctx context.Context,
	name string,
	rss []*srvtopo.ResolvedShard,
	session *econtext.SafeSession,
	autocommit bool,
	action shardActionTransactionFunc,
) (allErrors *concurrency.AllErrorRecorder) {

	numShards := len(rss)
	allErrors = new(concurrency.AllErrorRecorder)

	if numShards == 0 {
		return allErrors
	}
	oneShard := func(rs *srvtopo.ResolvedShard, i int) {
		var err error
		startTime, statsKey := stc.startAction(name, rs.Target)
		defer stc.endAction(startTime, allErrors, statsKey, &err, session)

		info, shardSession, err := actionInfo(ctx, rs.Target, session, autocommit, stc.txConn.txMode.TransactionMode())
		if err != nil {
			return
		}
		info, err = action(rs, i, info)
		if info == nil {
			return
		}
		if info.ignoreOldSession {
			shardSession = nil
		}
		if shardSession != nil && info.rowsAffected {
			// We might not always update or append in the session.
			// We need to track if rows were affected in the transaction.
			shardSession.RowsAffected = info.rowsAffected
		}
		if info.actionNeeded != nothing && (info.transactionID != 0 || info.reservedID != 0) {
			appendErr := session.AppendOrUpdate(rs.Target, info, shardSession, stc.txConn.txMode.TransactionMode())
			if appendErr != nil {
				err = appendErr
			}
		}
	}

	if numShards == 1 {
		// only one shard, do it synchronously.
		for i, rs := range rss {
			oneShard(rs, i)
		}
	} else {
		var panicRecord atomic.Value
		var wg sync.WaitGroup
		for i, rs := range rss {
			wg.Add(1)
			go func(rs *srvtopo.ResolvedShard, i int) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						panicRecord.Store(&panicData{
							p:     r,
							trace: debug.Stack(),
						})
					}
				}()
				oneShard(rs, i)
			}(rs, i)
		}
		wg.Wait()
		if pr, ok := panicRecord.Load().(*panicData); ok {
			log.Errorf("caught a panic during parallel execution:\n%s", string(pr.trace))
			panic(pr.p) // rethrow the captured panic in the main thread
		}
	}

	if session.MustRollback() {
		_ = stc.txConn.Rollback(ctx, session)
	}
	return allErrors
}

// ExecuteLock performs the requested 'action' on the specified
// ResolvedShard. If the lock session already has a reserved connection,
// it reuses it. Otherwise open a new reserved connection.
// The action function must match the shardActionTransactionFunc signature.
//
// It returns an error recorder in which each shard error is recorded positionally,
// i.e. if rss[2] had an error, then the error recorder will store that error
// in the second position.
func (stc *ScatterConn) ExecuteLock(ctx context.Context, rs *srvtopo.ResolvedShard, query *querypb.BoundQuery, session *econtext.SafeSession, lockFuncType sqlparser.LockingFuncType) (*sqltypes.Result, error) {

	var (
		qr    *sqltypes.Result
		err   error
		opts  *querypb.ExecuteOptions
		alias *topodatapb.TabletAlias
	)
	allErrors := new(concurrency.AllErrorRecorder)
	startTime, statsKey := stc.startAction("ExecuteLock", rs.Target)
	defer stc.endLockAction(startTime, allErrors, statsKey, &err)

	if session == nil || session.Session == nil {
		return nil, vterrors.VT13001("session cannot be nil")
	}

	opts = session.Session.Options
	info, err := lockInfo(rs.Target, session, lockFuncType)
	// Lock session is created on alphabetic sorted keyspace.
	// This error will occur if the existing session target does not match the current target.
	// This will happen either due to re-sharding or a new keyspace which comes before the existing order.
	// In which case, we will try to release old locks and return error.
	if err != nil {
		_ = stc.txConn.ReleaseLock(ctx, session)
		return nil, vterrors.Wrap(err, "Any previous held locks are released")
	}
	qs, err := getQueryService(ctx, rs, info, nil, true)
	if err != nil {
		return nil, err
	}
	reservedID := info.reservedID

	switch info.actionNeeded {
	case nothing:
		qr, err = qs.Execute(ctx, rs.Target, query.Sql, query.BindVariables, 0 /* transactionID */, reservedID, opts)
		if err != nil && wasConnectionClosed(err) {
			// TODO: try to acquire lock again.
			session.ResetLock()
			err = vterrors.Wrap(err, "held locks released")
		}
		if reservedID != 0 {
			session.UpdateLockHeartbeat()
		}
	case reserve:
		var state queryservice.ReservedState
		state, qr, err = qs.ReserveExecute(ctx, rs.Target, session.SetPreQueries(), query.Sql, query.BindVariables, 0 /* transactionID */, opts)
		reservedID = state.ReservedID
		alias = state.TabletAlias
		if err != nil && reservedID != 0 {
			_ = stc.txConn.ReleaseLock(ctx, session)
		}

		if reservedID != 0 {
			session.SetLockSession(&vtgatepb.Session_ShardSession{
				Target:      rs.Target,
				ReservedId:  reservedID,
				TabletAlias: alias,
			})
		}
	default:
		return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] unexpected actionNeeded on lock execution: %v", info.actionNeeded)
	}

	if err != nil {
		return nil, err
	}
	return qr, err
}

func wasConnectionClosed(err error) bool {
	sqlErr := sqlerror.NewSQLErrorFromError(err).(*sqlerror.SQLError)
	message := sqlErr.Error()

	switch sqlErr.Number() {
	case sqlerror.CRServerGone, sqlerror.CRServerLost:
		return true
	case sqlerror.ERQueryInterrupted:
		return vterrors.TxClosed.MatchString(message)
	default:
		return false
	}
}

// requireNewQS this checks if we need to fallback to new tablet.
func requireNewQS(err error, target *querypb.Target) bool {
	code := vterrors.Code(err)
	msg := err.Error()
	switch code {
	// when the tablet or mysql is unavailable for any reason.
	case vtrpcpb.Code_UNAVAILABLE:
		return true
	// when received wrong tablet error message.
	case vtrpcpb.Code_FAILED_PRECONDITION:
		return vterrors.RxWrongTablet.MatchString(msg)
	// when received cluster_event from tablet and tablet is not operational.
	// this will also help in buffering the query if needed.
	case vtrpcpb.Code_CLUSTER_EVENT:
		return (target != nil && target.TabletType == topodatapb.TabletType_PRIMARY) || vterrors.RxOp.MatchString(msg)
	}
	return false
}

// actionInfo looks at the current session, and returns information about what needs to be done for this tablet
func actionInfo(ctx context.Context, target *querypb.Target, session *econtext.SafeSession, autocommit bool, txMode vtgatepb.TransactionMode) (*shardActionInfo, *vtgatepb.Session_ShardSession, error) {
	if !(session.InTransaction() || session.InReservedConn()) {
		return &shardActionInfo{}, nil, nil
	}
	ignoreSession := ctx.Value(engine.IgnoreReserveTxn)
	if ignoreSession != nil {
		return &shardActionInfo{}, nil, nil
	}
	// No need to protect ourselves from the race condition between
	// Find and AppendOrUpdate. The higher level functions ensure that no
	// duplicate (target) tuples can execute
	// this at the same time.
	shardSession, err := session.FindAndChangeSessionIfInSingleTxMode(target.Keyspace, target.Shard, target.TabletType, txMode)
	if err != nil {
		return nil, nil, err
	}

	shouldReserve := session.InReservedConn() && (shardSession == nil || shardSession.ReservedId == 0)
	shouldBegin := session.InTransaction() && (shardSession == nil || shardSession.TransactionId == 0) && !autocommit

	var act = nothing
	switch {
	case shouldBegin && shouldReserve:
		act = reserveBegin
	case shouldReserve:
		act = reserve
	case shouldBegin:
		act = begin
	}

	info := &shardActionInfo{
		actionNeeded: act,
	}
	if shardSession != nil {
		info.transactionID = shardSession.TransactionId
		info.reservedID = shardSession.ReservedId
		info.alias = shardSession.TabletAlias
		info.rowsAffected = shardSession.RowsAffected
	}
	return info, shardSession, nil
}

// lockInfo looks at the current session, and returns information about what needs to be done for this tablet
func lockInfo(target *querypb.Target, session *econtext.SafeSession, lockFuncType sqlparser.LockingFuncType) (*shardActionInfo, error) {
	info := &shardActionInfo{actionNeeded: nothing}
	if session.LockSession != nil {
		if !proto.Equal(target, session.LockSession.Target) {
			return nil, vterrors.Errorf(vtrpcpb.Code_NOT_FOUND, "target does match the existing lock session target: (%v, %v)", target, session.LockSession.Target)
		}
		info.reservedID = session.LockSession.ReservedId
		info.alias = session.LockSession.TabletAlias
	}
	// Only GetLock needs to start a reserved connection.
	// Once in reserved connection, it will be used for other calls as well.
	// But, we don't want to start a reserved connection for other calls like IsFreeLock, IsUsedLock, etc.
	if lockFuncType != sqlparser.GetLock {
		return info, nil
	}
	if info.reservedID == 0 {
		info.actionNeeded = reserve
	}
	return info, nil
}

type shardActionInfo struct {
	actionNeeded              actionNeeded
	reservedID, transactionID int64
	alias                     *topodatapb.TabletAlias

	// ignoreOldSession is used when there is a retry on the same shard due to connection loss for a reserved connection.
	// The old reference should be ignored and new shard session should be added to the session.
	ignoreOldSession bool
	rowsAffected     bool
}

func (sai *shardActionInfo) TransactionID() int64 {
	return sai.transactionID
}

func (sai *shardActionInfo) ReservedID() int64 {
	return sai.reservedID
}

func (sai *shardActionInfo) RowsAffected() bool {
	return sai.rowsAffected
}

func (sai *shardActionInfo) Alias() *topodatapb.TabletAlias {
	return sai.alias
}

func (sai *shardActionInfo) updateTransactionAndReservedID(txID int64, rID int64, alias *topodatapb.TabletAlias, qr *sqltypes.Result) *shardActionInfo {
	firstTimeRowsAffected := false
	if txID != 0 && qr != nil && !sai.rowsAffected {
		firstTimeRowsAffected = qr.RowsAffected > 0
	}
	if txID == sai.transactionID && rID == sai.reservedID && !firstTimeRowsAffected {
		// As transaction id and reserved id have not changed, there is nothing to update in session shard sessions.
		return nil
	}
	newInfo := *sai
	newInfo.reservedID = rID
	newInfo.transactionID = txID
	newInfo.alias = alias
	newInfo.rowsAffected = firstTimeRowsAffected
	return &newInfo
}

type actionNeeded int

const (
	nothing actionNeeded = iota
	reserveBegin
	reserve
	begin
)
