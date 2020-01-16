package base

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// base tests require GSI
	TestBucketPool = NewTestBucketPool(EmptyBucketReadier, NoopBucketWorkerFunc)
	defer TestBucketPool.Close()

	os.Exit(m.Run())
}
