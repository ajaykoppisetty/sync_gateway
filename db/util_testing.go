package db

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/couchbase/gocb"
	"github.com/couchbase/sync_gateway/base"
	"github.com/stretchr/testify/assert"
)

// ViewsAndGSIBucketReadier inherits from EmptyBucketReadier, but also initializes Views and GSI indexes. It is run asynchronously as soon as a test is finished with a bucket.
var ViewsAndGSIBucketReadier base.BucketWorkerFunc = func(ctx context.Context, b *base.CouchbaseBucketGoCB, tbp *base.GocbTestBucketPool) error {

	tbp.Logf(ctx, "checking if bucket is empty")
	if itemCount, err := b.QueryBucketItemCount(); err != nil {
		return err
	} else if itemCount == 0 {
		tbp.Logf(ctx, "Bucket already empty - skipping")
	} else {
		tbp.Logf(ctx, "Bucket not empty (%d items), emptying bucket via N1QL", itemCount)
		// use n1ql to empty bucket, with the hope that the query service is happier to deal with the rollback.
		res, err := b.Query(`DELETE FROM $_bucket`, nil, gocb.RequestPlus, false)
		if err != nil {
			return err
		}
		_ = res.Close()
	}

	tbp.Logf(ctx, "initializing bucket views")
	err := InitializeViews(b)
	if err != nil {
		return err
	}
	tbp.Logf(ctx, "bucket views initialized")

	tbp.Logf(ctx, "waiting for empty bucket indexes")
	// we can't init indexes concurrently, so we'll just wait for them to be empty after flushing.
	err = WaitForIndexEmpty(b, base.TestUseXattrs())
	if err != nil {
		tbp.Logf(ctx, "WaitForIndexEmpty returned an error: %v", err)
		return err
	}
	tbp.Logf(ctx, "bucket indexes empty")

	return nil
}

// ViewsAndGSIBucketInit is run once on a package's start. This is run synchronously per-bucket, so can be used to build GSI indexes safely.
var ViewsAndGSIBucketInit base.BucketWorkerFunc = func(ctx context.Context, b *base.CouchbaseBucketGoCB, tbp *base.GocbTestBucketPool) error {

	tbp.Logf(ctx, "dropping existing bucket indexes")
	if err := base.DropAllBucketIndexes(b); err != nil {
		tbp.Logf(ctx, "Failed to drop bucket indexes: %v", err)
		return err
	}

	// create a primary index so we can delete all items via n1ql instead of flushing
	tbp.Logf(ctx, "creating primary index on bucket")
	res, err := b.Query(`CREATE PRIMARY INDEX on $_bucket`, nil, gocb.RequestPlus, true)
	if err != nil {
		return err
	}
	_ = res.Close()

	tbp.Logf(ctx, "creating SG bucket indexes")
	if err := InitializeIndexes(b, base.TestUseXattrs(), 0); err != nil {
		return err
	}

	return nil
}

func isIndexEmpty(bucket *base.CouchbaseBucketGoCB, useXattrs bool) (bool, error) {
	var results gocb.QueryResults

	// Create the star channel query
	statement := fmt.Sprintf("%s LIMIT 1", QueryStarChannel.statement) // append LIMIT 1 since we only care if there are any results or not
	starChannelQueryStatement := replaceSyncTokensQuery(statement, useXattrs)
	starChannelQueryStatement = replaceIndexTokensQuery(starChannelQueryStatement, sgIndexes[IndexAllDocs], useXattrs)
	params := map[string]interface{}{}
	params[QueryParamStartSeq] = 0
	params[QueryParamEndSeq] = math.MaxInt64

	// Execute the query
	results, err := bucket.Query(starChannelQueryStatement, params, gocb.RequestPlus, true)

	// If there was an error, then retry.  Assume it's an "index rollback" error which happens as
	// the index processes the bucket flush operation
	if err != nil {
		return false, err
	}

	// If it's empty, we're done
	var queryRow AllDocsIndexQueryRow
	found := results.Next(&queryRow)
	resultsCloseErr := results.Close()
	if resultsCloseErr != nil {
		return false, err
	}

	return !found, nil
}

// Workaround SG #3570 by doing a polling loop until the star channel query returns 0 results.
// Uses the star channel index as a proxy to indicate that _all_ indexes are empty (which might not be true)
func WaitForIndexEmpty(bucket *base.CouchbaseBucketGoCB, useXattrs bool) error {

	retryWorker := func() (shouldRetry bool, err error, value interface{}) {
		empty, err := isIndexEmpty(bucket, useXattrs)
		if err != nil {
			return true, err, nil
		}
		return !empty, nil, empty
	}

	// Kick off the retry loop
	err, _ := base.RetryLoop(
		"Wait for index to be empty",
		retryWorker,
		base.CreateMaxDoublingSleeperFunc(30, 500, 4000),
	)
	return err

}

// Count how many rows are in gocb.QueryResults
func ResultsEmpty(results gocb.QueryResults) (resultsEmpty bool) {

	var queryRow AllDocsIndexQueryRow
	found := results.Next(&queryRow)
	return !found

}

func (db *DatabaseContext) CacheCompactActive() bool {
	channelCache := db.changeCache.getChannelCache()
	compactingCache, ok := channelCache.(*channelCacheImpl)
	if !ok {
		return false
	}
	return compactingCache.isCompactActive()
}

func (db *DatabaseContext) WaitForCaughtUp(targetCount int64) error {
	for i := 0; i < 100; i++ {
		caughtUpCount := base.ExpvarVar2Int(db.DbStats.StatsCblReplicationPull().Get(base.StatKeyPullReplicationsCaughtUp))
		if caughtUpCount >= targetCount {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("WaitForCaughtUp didn't catch up")
}

type StatWaiter struct {
	initCount   int64       // Document cached count when NewStatWaiter is called
	targetCount int64       // Target count used when Wait is called
	stat        *expvar.Int // Expvar to wait on
	tb          testing.TB  // Raises tb.Fatalf on wait timeout
}

func (db *DatabaseContext) NewStatWaiter(stat *expvar.Int, tb testing.TB) *StatWaiter {
	return &StatWaiter{
		initCount:   stat.Value(),
		targetCount: stat.Value(),
		stat:        stat,
		tb:          tb,
	}
}

func (db *DatabaseContext) NewDCPCachingCountWaiter(tb testing.TB) *StatWaiter {
	stat, ok := db.DbStats.StatsDatabase().Get(base.StatKeyDcpCachingCount).(*expvar.Int)
	if !ok {
		tb.Fatalf("Unable to retrieve StatKeyDcpCachingCount during StatWaiter initialization ")
	}
	return db.NewStatWaiter(stat, tb)
}

func (db *DatabaseContext) NewPullReplicationCaughtUpWaiter(tb testing.TB) *StatWaiter {
	stat, ok := db.DbStats.StatsCblReplicationPull().Get(base.StatKeyPullReplicationsCaughtUp).(*expvar.Int)
	if !ok {
		tb.Fatalf("Unable to retrieve StatKeyPullReplicationsCaughtUp during StatWaiter initialization ")
	}
	return db.NewStatWaiter(stat, tb)
}

func (db *DatabaseContext) NewCacheRevsActiveWaiter(tb testing.TB) *StatWaiter {
	stat, ok := db.DbStats.StatsCache().Get(base.StatKeyChannelCacheRevsActive).(*expvar.Int)
	if !ok {
		tb.Fatalf("Unable to retrieve StatKeyChannelCacheRevsActive during StatWaiter initialization ")
	}
	return db.NewStatWaiter(stat, tb)
}

func (sw *StatWaiter) Add(count int) {
	sw.targetCount += int64(count)
}

func (sw *StatWaiter) AddAndWait(count int) {
	sw.targetCount += int64(count)
	sw.Wait()
}

// Wait uses backoff retry for up to ~27s
func (sw *StatWaiter) Wait() {
	actualCount := sw.stat.Value()
	if actualCount >= sw.targetCount {
		return
	}

	waitTime := 1 * time.Millisecond
	for i := 0; i < 13; i++ {
		waitTime = waitTime * 2
		time.Sleep(waitTime)
		actualCount = sw.stat.Value()
		if actualCount >= sw.targetCount {
			return
		}
	}

	sw.tb.Fatalf("StatWaiter.Wait timed out waiting for stat to reach %d (actual: %d)", sw.targetCount, actualCount)
}

func AssertEqualBodies(t *testing.T, expected, actual Body) {
	expectedCanonical, err := base.JSONMarshalCanonical(expected)
	assert.NoError(t, err)
	actualCanonical, err := base.JSONMarshalCanonical(actual)
	assert.NoError(t, err)
	assert.Equal(t, string(expectedCanonical), string(actualCanonical))
}

func WaitForUserWaiterChange(userWaiter *ChangeWaiter) bool {
	var isChanged bool
	for i := 0; i < 100; i++ {
		isChanged = userWaiter.RefreshUserCount()
		if isChanged {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return isChanged
}
