package base

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/couchbase/gocb"
	"github.com/couchbaselabs/walrus"
	"github.com/pkg/errors"
)

const (
	testBucketQuotaMB    = 250
	testBucketNamePrefix = "sg_int_"
	testClusterUsername  = "Administrator"
	testClusterPassword  = "password"

	// Creates this many buckets in the backing store to be pooled for testing.
	defaultBucketPoolSize = 10
	testEnvPoolSize       = "SG_TEST_BUCKET_POOL_SIZE"

	// Prevents reuse and cleanup of buckets used in failed tests for later inspection.
	// When all pooled buckets are in a preserved state, any remaining tests are skipped.
	testEnvPreserve = "SG_TEST_BUCKET_POOL_PRESERVE"

	// Prints detailed logs from the test pooling framework.
	testEnvVerbose = "SG_TEST_BUCKET_POOL_DEBUG"
)

type bucketName string

type GocbTestBucketPool struct {
	readyBucketPool    chan *CouchbaseBucketGoCB
	bucketReadierQueue chan bucketName
	cluster            *gocb.Cluster
	clusterMgr         *gocb.ClusterManager
	ctxCancelFunc      context.CancelFunc
	defaultBucketSpec  BucketSpec

	useGSI bool

	// preserveBuckets can be set to true to prevent removal of a bucket used in a failing test.
	preserveBuckets bool
	// keep track of number of preserved buckets to prevent bucket exhaustion deadlock
	preservedBucketCount uint32

	// Enables test pool logging
	verbose bool
}

// numBuckets returns the configured number of buckets to use in the pool.
func numBuckets() int {
	numBuckets := defaultBucketPoolSize
	if envPoolSize := os.Getenv(testEnvPoolSize); envPoolSize != "" {
		var err error
		numBuckets, err = strconv.Atoi(envPoolSize)
		if err != nil {
			log.Fatalf("Couldn't parse %s: %v", testEnvPoolSize, err)
		}
	}
	return numBuckets
}

func testCluster(server string) *gocb.Cluster {
	cluster, err := gocb.Connect(server)
	if err != nil {
		log.Fatalf("Couldn't connect to %q: %v", server, err)
	}

	err = cluster.Authenticate(gocb.PasswordAuthenticator{
		Username: testClusterUsername,
		Password: testClusterPassword,
	})
	if err != nil {
		log.Fatalf("Couldn't authenticate with %q: %v", server, err)
	}
	return cluster
}

type BucketWorkerFunc func(ctx context.Context, b *CouchbaseBucketGoCB, tbp *GocbTestBucketPool) error

// BucketFlushReadier ensures the bucket is empty.
var BucketFlushReadier BucketWorkerFunc = func(ctx context.Context, b *CouchbaseBucketGoCB, tbp *GocbTestBucketPool) error {
	// Empty bucket
	if itemCount, err := b.QueryBucketItemCount(); err != nil {
		return err
	} else if itemCount == 0 {
		tbp.Logf(ctx, "Bucket already empty - skipping flush")
	} else {
		tbp.Logf(ctx, "Bucket not empty (%d items), flushing bucket", itemCount)
		err := b.Flush()
		if err != nil {
			return err
		}
	}
	return nil
}

// NoopBucketWorkerFunc does nothing
var NoopBucketWorkerFunc BucketWorkerFunc = func(ctx context.Context, b *CouchbaseBucketGoCB, tbp *GocbTestBucketPool) error {
	return nil
}

var defaultBucketSpec = BucketSpec{
	Server:          UnitTestUrl(),
	CouchbaseDriver: GoCBCustomSGTranscoder,
	Auth: TestAuthenticator{
		Username: testClusterUsername,
		Password: testClusterPassword,
	},
	UseXattrs: TestUseXattrs(),
}

// NewTestBucketPool initializes a new GocbTestBucketPool. To be called from TestMain for packages requiring test buckets.
// Set useGSI to false to skip index creation for packages that don't require GSI, to speed up bucket readiness.
func NewTestBucketPool(bucketReadierFunc, bucketInitFunc BucketWorkerFunc) *GocbTestBucketPool {
	// We can safely skip setup when we want Walrus buckets to be used. They'll be created on-demand via GetTestBucketAndSpec.
	if !TestUseCouchbaseServer() {
		return nil
	}

	numBuckets := numBuckets()
	// TODO: What about pooling servers too??
	// That way, we can have unlimited buckets available in a single test pool... True horizontal scalability in tests!
	cluster := testCluster(UnitTestUrl())

	// Used to manage cancellation of worker goroutines
	ctx, ctxCancelFunc := context.WithCancel(context.Background())

	preserveBuckets, _ := strconv.ParseBool(os.Getenv(testEnvPreserve))
	verbose, _ := strconv.ParseBool(os.Getenv(testEnvVerbose))

	tbp := GocbTestBucketPool{
		readyBucketPool:    make(chan *CouchbaseBucketGoCB, numBuckets),
		bucketReadierQueue: make(chan bucketName, numBuckets),
		cluster:            cluster,
		clusterMgr:         cluster.Manager(testClusterUsername, testClusterPassword),
		ctxCancelFunc:      ctxCancelFunc,
		defaultBucketSpec:  defaultBucketSpec,
		preserveBuckets:    preserveBuckets,
		verbose:            verbose,
	}

	go tbp.bucketReadierWorker(ctx, bucketReadierFunc)

	if err := tbp.createTestBuckets(numBuckets, bucketInitFunc); err != nil {
		log.Fatalf("Couldn't create test buckets: %v", err)
	}

	return &tbp
}

func (tbp *GocbTestBucketPool) Logf(ctx context.Context, format string, args ...interface{}) {
	if tbp == nil || !tbp.verbose {
		return
	}

	format = addPrefixes(format, ctx, LevelNone, KeySGTest)
	if colorEnabled() {
		// Green
		format = "\033[0;32m" + format + "\033[0m"
	}

	_, _ = fmt.Fprintf(consoleFOutput, format+"\n", args...)
}

// GetTestBucket returns a bucket to be used during a test.
// The returned teardownFn must be called once the test is done,
// which closes the bucket, readies it for a new test, and releases back into the pool.
func (tbp *GocbTestBucketPool) GetTestBucketAndSpec(t testing.TB) (b Bucket, s BucketSpec, teardownFn func()) {

	ctx := testCtx(t)

	// Return a new Walrus bucket when tbp has not been initialized
	if tbp == nil {
		if !UnitTestUrlIsWalrus() {
			tbp.Logf(ctx, "nil TestBucketPool, but not using a Walrus test URL")
			os.Exit(1)
		}

		walrusBucket := walrus.NewBucket(testBucketNamePrefix + GenerateRandomID())
		ctx := bucketCtx(ctx, walrusBucket)
		tbp.Logf(ctx, "Creating new walrus test bucket")

		return walrusBucket, getBucketSpec(bucketName(walrusBucket.GetName())), func() {
			tbp.Logf(ctx, "Teardown called - Closing walrus test bucket")
			walrusBucket.Close()
		}
	}

	if atomic.LoadUint32(&tbp.preservedBucketCount) >= uint32(cap(tbp.readyBucketPool)) {
		tbp.Logf(ctx,
			"No more buckets available for testing. All pooled buckets have been preserved by failing tests.")
		t.Skipf("No more buckets available for testing. All pooled buckets have been preserved for failing tests.")
	}

	tbp.Logf(ctx, "Attempting to get test bucket from pool")
	gocbBucket := <-tbp.readyBucketPool
	ctx = bucketCtx(ctx, gocbBucket)
	tbp.Logf(ctx, "Got test bucket from pool")

	return gocbBucket, getBucketSpec(bucketName(gocbBucket.GetName())), func() {
		if tbp.preserveBuckets && t.Failed() {
			tbp.Logf(ctx, "Test using bucket failed. Preserving bucket for later inspection")
			atomic.AddUint32(&tbp.preservedBucketCount, 1)
			return
		}

		tbp.Logf(ctx, "Teardown called - closing bucket")
		gocbBucket.Close()
		tbp.Logf(ctx, "Teardown called - Pushing into bucketReadier queue")
		tbp.bucketReadierQueue <- bucketName(gocbBucket.GetName())
	}
}

// bucketCtx extends the parent context with a bucket name.
func bucketCtx(parent context.Context, b Bucket) context.Context {
	return bucketNameCtx(parent, b.GetName())
}

// bucketNameCtx extends the parent context with a bucket name.
func bucketNameCtx(parent context.Context, bucketName string) context.Context {
	parentLogCtx, _ := parent.Value(LogContextKey{}).(LogContext)
	newCtx := LogContext{
		TestName:       parentLogCtx.TestName,
		TestBucketName: bucketName,
	}
	return context.WithValue(parent, LogContextKey{}, newCtx)
}

// testCtx creates a log context for the given test.
func testCtx(t testing.TB) context.Context {
	return context.WithValue(context.Background(), LogContextKey{}, LogContext{TestName: t.Name()})
}

// Close cleans up any buckets, and closes the pool.
func (tbp *GocbTestBucketPool) Close() {
	if tbp == nil {
		// noop
		return
	}

	// Cancel async workers
	tbp.ctxCancelFunc()

	if err := tbp.cluster.Close(); err != nil {
		tbp.Logf(context.Background(), "Couldn't close cluster connection: %v", err)
	}
}

// removes any integration test buckets
func (tbp *GocbTestBucketPool) removeOldTestBuckets() error {
	buckets, err := tbp.clusterMgr.GetBuckets()
	if err != nil {
		return errors.Wrap(err, "couldn't retrieve buckets from cluster manager")
	}

	wg := sync.WaitGroup{}

	for _, b := range buckets {
		if strings.HasPrefix(b.Name, testBucketNamePrefix) {
			ctx := bucketNameCtx(context.Background(), b.Name)
			tbp.Logf(ctx, "Removing old test bucket")
			wg.Add(1)

			// Run the RemoveBucket requests concurrently, as it takes a while per bucket.
			go func(b *gocb.BucketSettings) {
				err := tbp.clusterMgr.RemoveBucket(b.Name)
				if err != nil {
					tbp.Logf(ctx, "Error removing old test bucket: %v", err)
				} else {
					tbp.Logf(ctx, "Removed old test bucket")
				}

				wg.Done()
			}(b)
		}
	}

	wg.Wait()

	return nil
}

// creates a new set of integration test buckets and pushes them into the readier queue.
func (tbp *GocbTestBucketPool) createTestBuckets(numBuckets int, bucketInitFunc BucketWorkerFunc) error {

	wg := sync.WaitGroup{}

	existingBuckets, err := tbp.clusterMgr.GetBuckets()
	if err != nil {
		return err
	}

	openBuckets := make([]*CouchbaseBucketGoCB, numBuckets)

	// create required buckets
	for i := 0; i < numBuckets; i++ {
		testBucketName := testBucketNamePrefix + strconv.Itoa(i)
		ctx := bucketNameCtx(context.Background(), testBucketName)

		var bucketExists bool
		for _, b := range existingBuckets {
			if testBucketName == b.Name {
				tbp.Logf(ctx, "Skipping InsertBucket... Bucket already exists")
				bucketExists = true
			}
		}

		wg.Add(1)

		// Bucket creation takes a few seconds for each bucket,
		// so create and wait for readiness concurrently.
		go func(i int, bucketExists bool) {
			if !bucketExists {
				tbp.Logf(ctx, "Creating new test bucket")
				err := tbp.clusterMgr.InsertBucket(&gocb.BucketSettings{
					Name:          testBucketName,
					Quota:         testBucketQuotaMB,
					Type:          gocb.Couchbase,
					FlushEnabled:  true,
					IndexReplicas: false,
					Replicas:      0,
				})
				if err != nil {
					tbp.Logf(ctx, "Couldn't create test bucket: %v", err)
					os.Exit(1)
				}

				// Have an initial wait for bucket creation before the OpenBucket retry starts
				// time.Sleep(time.Second * 2 * time.Duration(numBuckets))
			}

			b, err := tbp.openTestBucket(bucketName(testBucketName), CreateSleeperFunc(5*numBuckets, 1000))
			if err != nil {
				tbp.Logf(ctx, "Timed out trying to open new bucket: %v", err)
				os.Exit(1)
			}
			openBuckets[i] = b

			wg.Done()
		}(i, bucketExists)
	}

	wg.Wait()

	// All the buckets are ready, so now we can perform some synchronous setup (e.g. Creating GSI indexes)
	for i := 0; i < numBuckets; i++ {
		testBucketName := testBucketNamePrefix + strconv.Itoa(i)
		ctx := bucketNameCtx(context.Background(), testBucketName)

		tbp.Logf(ctx, "running bucketInitFunc")
		b := openBuckets[i]
		if err := bucketInitFunc(ctx, b, tbp); err != nil {
			tbp.Logf(ctx, "Error from bucketInitFunc: %v", err)
			os.Exit(1)
		}

		b.Close()

		tbp.Logf(ctx, "Putting gocbBucket onto bucketReadierQueue")
		tbp.bucketReadierQueue <- bucketName(testBucketName)
	}

	return nil
}

// bucketReadierWorker reads a channel of "dirty" buckets (bucketReadierQueue), does something to get them ready, and then puts them back into the pool.
// The mechanism for getting the bucket ready can vary by package being tested (for instance, a package not requiring views or GSI can use the BucketFlushReadier function)
// A package requiring views or GSI, will need to pass in the db.ViewsAndGSIBucketReadier function.
func (tbp *GocbTestBucketPool) bucketReadierWorker(ctx context.Context, bucketReadierFunc BucketWorkerFunc) {
	tbp.Logf(context.Background(), "Starting bucketReadier")

loop:
	for {
		select {
		case <-ctx.Done():
			tbp.Logf(context.Background(), "bucketReadier got ctx cancelled")
			break loop

		case testBucketName := <-tbp.bucketReadierQueue:
			ctx := bucketNameCtx(ctx, string(testBucketName))
			tbp.Logf(ctx, "bucketReadier got bucket")

			go func(testBucketName bucketName) {
				b, err := tbp.openTestBucket(testBucketName, CreateSleeperFunc(5, 1000))
				if err != nil {
					panic(err)
				}

				tbp.Logf(ctx, "Running bucket through readier function")
				err = bucketReadierFunc(ctx, b, tbp)
				if err != nil {
					panic(err)
				}

				tbp.Logf(ctx, "Bucket ready, putting back into ready pool")
				tbp.readyBucketPool <- b
			}(testBucketName)
		}
	}

	tbp.Logf(context.Background(), "Stopping bucketReadier")
}

func (tbp *GocbTestBucketPool) openTestBucket(testBucketName bucketName, sleeper RetrySleeper) (*CouchbaseBucketGoCB, error) {

	ctx := bucketNameCtx(context.Background(), string(testBucketName))

	bucketSpec := tbp.defaultBucketSpec
	bucketSpec.BucketName = string(testBucketName)

	waitForNewBucketWorker := func() (shouldRetry bool, err error, value interface{}) {
		gocbBucket, err := GetCouchbaseBucketGoCB(bucketSpec)
		if err != nil {
			tbp.Logf(ctx, "Retrying OpenBucket")
			return true, err, nil
		}
		return false, nil, gocbBucket
	}

	tbp.Logf(ctx, "Opening bucket")
	err, val := RetryLoop("waitForNewBucket", waitForNewBucketWorker,
		// The more buckets we're creating simultaneously on the cluster,
		// the longer this seems to take, so scale the wait time.
		sleeper)

	gocbBucket := val.(*CouchbaseBucketGoCB)

	return gocbBucket, err
}

func getBucketSpec(testBucketName bucketName) BucketSpec {
	bucketSpec := defaultBucketSpec
	bucketSpec.BucketName = string(testBucketName)
	return bucketSpec
}
